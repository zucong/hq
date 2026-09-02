package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type HerdrMutationOutcome string

const (
	herdrDefinitelyNotRun HerdrMutationOutcome = "definitely-not-run"
	herdrAmbiguous        HerdrMutationOutcome = "ambiguous-timeout"
	herdrConfirmed        HerdrMutationOutcome = "confirmed"
)

type HerdrMutationResult struct {
	Outcome   HerdrMutationOutcome
	Raw       []byte
	ErrorCode string
	Err       error
}

// HerdrAPIError preserves the machine-readable error contract emitted by the
// Herdr CLI. Callers must use Code for recovery decisions; Message is only for
// operators and may be localized or reworded by Herdr.
type HerdrAPIError struct {
	Code    string
	Message string
}

func (e *HerdrAPIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("herdr API error %s", e.Code)
	}
	return fmt.Sprintf("herdr API error %s: %s", e.Code, e.Message)
}

type herdrErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func decodeHerdrAPIError(raw []byte) (*HerdrAPIError, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, false
	}
	var envelope herdrErrorEnvelope
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return nil, false
	}
	code := strings.TrimSpace(envelope.Error.Code)
	if code == "" {
		return nil, false
	}
	return &HerdrAPIError{Code: code, Message: strings.TrimSpace(envelope.Error.Message)}, true
}

// These errors are emitted before the requested mutation reaches a terminal
// input/process side effect. The list is deliberately closed: an unknown code
// remains ambiguous for a mutation until the adapter is updated with evidence
// from the Herdr protocol.
func herdrErrorDefinitelyNoEffect(code string) bool {
	switch code {
	case "agent_blocked", "agent_not_found", "agent_not_ready", "agent_not_running",
		"workspace_not_found", "tab_not_found", "pane_not_found", "not_found",
		"invalid_params", "invalid_request", "invalid_target", "invalid_key",
		"confirmation_required", "server_not_running", "server_unavailable",
		"protocol_mismatch", "unsupported_protocol":
		return true
	default:
		return false
	}
}

type HerdrWorkspace struct {
	ID    string `json:"workspace_id"`
	Label string `json:"label"`
}

type HerdrTab struct {
	ID          string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	CWD         string `json:"cwd"`
	Number      int    `json:"number,omitempty"`
}

type HerdrPane struct {
	ID          string `json:"pane_id"`
	TerminalID  string `json:"terminal_id,omitempty"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	CWD         string `json:"cwd"`
	Revision    uint64 `json:"revision,omitempty"`
}

type HerdrAgentSession struct {
	Source string `json:"source"`
	Agent  string `json:"agent"`
	Kind   string `json:"kind"`
	Value  string `json:"value"`
}

type HerdrAgent struct {
	Name             string             `json:"name"`
	Kind             string             `json:"kind"`
	Status           string             `json:"status"`
	CWD              string             `json:"cwd"`
	TerminalID       string             `json:"terminal_id,omitempty"`
	WorkspaceID      string             `json:"workspace_id"`
	TabID            string             `json:"tab_id"`
	PaneID           string             `json:"pane_id"`
	InteractiveReady bool               `json:"interactive_ready"`
	AgentSession     *HerdrAgentSession `json:"agent_session,omitempty"`
	Revision         uint64             `json:"revision,omitempty"`
}

type HerdrSnapshot struct {
	Workspaces []HerdrWorkspace `json:"workspaces"`
	Tabs       []HerdrTab       `json:"tabs"`
	Panes      []HerdrPane      `json:"panes"`
	Agents     []HerdrAgent     `json:"agents"`
}

// HerdrSnapshotScope binds strict snapshot validation to one HQ instance.
// Objects outside the target workspace still need stable identity and a
// provable relationship to a known workspace, but their CWD is not an input to
// this HQ instance and may be absent.
type HerdrSnapshotScope struct {
	WorkspaceLabel string
}

type HerdrTabSpec struct {
	WorkspaceID string
	CWD         string
	Label       string
	Env         map[string]string
}

type HerdrTabCreated struct {
	Tab  HerdrTab
	Pane HerdrPane
}

type HerdrControl interface {
	Snapshot(context.Context, HerdrSnapshotScope) (HerdrSnapshot, error)
	CreateWorkspace(context.Context, string) (HerdrWorkspace, HerdrMutationResult)
	CreateTab(context.Context, HerdrTabSpec) (HerdrTabCreated, HerdrMutationResult)
	StartAgent(context.Context, string, string, string, []string) HerdrMutationResult
	CloseTab(context.Context, string) HerdrMutationResult
	RunPane(context.Context, string, string) HerdrMutationResult
	Prompt(context.Context, string, string) HerdrMutationResult
}

// HerdrAgentReader is optional so synthetic controls that only exercise the
// mutation contract do not need to emulate terminal scrollback. Production
// execHerdrControl implements it for runtime safeguard detection.
type HerdrAgentReader interface {
	ReadAgent(context.Context, string) ([]byte, error)
}

type unavailableHerdrControl struct{ Err error }

func (u unavailableHerdrControl) failure() HerdrMutationResult {
	return HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: u.Err}
}
func (u unavailableHerdrControl) Snapshot(context.Context, HerdrSnapshotScope) (HerdrSnapshot, error) {
	return HerdrSnapshot{}, u.Err
}
func (u unavailableHerdrControl) CreateWorkspace(context.Context, string) (HerdrWorkspace, HerdrMutationResult) {
	return HerdrWorkspace{}, u.failure()
}
func (u unavailableHerdrControl) CreateTab(context.Context, HerdrTabSpec) (HerdrTabCreated, HerdrMutationResult) {
	return HerdrTabCreated{}, u.failure()
}
func (u unavailableHerdrControl) StartAgent(context.Context, string, string, string, []string) HerdrMutationResult {
	return u.failure()
}
func (u unavailableHerdrControl) CloseTab(context.Context, string) HerdrMutationResult {
	return u.failure()
}
func (u unavailableHerdrControl) RunPane(context.Context, string, string) HerdrMutationResult {
	return u.failure()
}
func (u unavailableHerdrControl) Prompt(context.Context, string, string) HerdrMutationResult {
	return u.failure()
}

type execHerdrControl struct {
	Bin             string
	SnapshotTimeout time.Duration
	MutationTimeout time.Duration
	StartTimeout    time.Duration
	PromptTimeout   time.Duration
}

const (
	defaultHerdrSnapshotTimeout = 5 * time.Second
	defaultHerdrMutationTimeout = 15 * time.Second
	defaultHerdrStartTimeout    = 70 * time.Second
	defaultHerdrPromptTimeout   = 15 * time.Second
)

func newExecHerdrControl(bin string) (*execHerdrControl, error) {
	abs, err := filepath.Abs(bin)
	if err != nil || !filepath.IsAbs(abs) {
		return nil, fmt.Errorf("herdr 二进制必须是绝对路径")
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, fmt.Errorf("canonicalize herdr 二进制：%w", err)
	}
	info, err := os.Lstat(canonical)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("herdr 二进制必须是 canonical 非 symlink 普通文件：%s", canonical)
	}
	return &execHerdrControl{
		Bin: canonical, SnapshotTimeout: defaultHerdrSnapshotTimeout, MutationTimeout: defaultHerdrMutationTimeout,
		StartTimeout: defaultHerdrStartTimeout, PromptTimeout: defaultHerdrPromptTimeout,
	}, nil
}

func resolveHerdrExecutable(value string) (string, error) {
	if value == "" {
		value = "herdr"
	}
	if !filepath.IsAbs(value) {
		resolved, err := exec.LookPath(value)
		if err != nil {
			return "", fmt.Errorf("找不到 herdr 二进制：%w", err)
		}
		value = resolved
	}
	return filepath.Abs(value)
}

func (c *execHerdrControl) run(ctx context.Context, mutating bool, args ...string) HerdrMutationResult {
	if c == nil || !filepath.IsAbs(c.Bin) {
		return HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: fmt.Errorf("herdr control 未配置绝对二进制")}
	}
	cmd := exec.CommandContext(ctx, c.Bin, args...)
	cmd.Env = os.Environ()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Start(); err != nil {
		return HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: fmt.Errorf("herdr %s 未启动：%w", strings.Join(args, " "), err)}
	}
	err := cmd.Wait()
	if err != nil {
		outcome := herdrDefinitelyNotRun
		if mutating {
			outcome = herdrAmbiguous
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return HerdrMutationResult{Outcome: outcome, Raw: stdout.Bytes(), Err: fmt.Errorf("herdr %s deadline：%w", strings.Join(args, " "), ctx.Err())}
		}
		if apiErr, ok := decodeHerdrAPIError(stderr.Bytes()); ok {
			if !mutating || herdrErrorDefinitelyNoEffect(apiErr.Code) {
				outcome = herdrDefinitelyNotRun
			}
			return HerdrMutationResult{Outcome: outcome, Raw: stdout.Bytes(), ErrorCode: apiErr.Code, Err: apiErr}
		}
		// A few wrappers forward the server envelope to stdout even on a
		// non-zero exit. Accept that representation without weakening the
		// closed classification for malformed or unknown errors.
		if apiErr, ok := decodeHerdrAPIError(stdout.Bytes()); ok {
			if !mutating || herdrErrorDefinitelyNoEffect(apiErr.Code) {
				outcome = herdrDefinitelyNotRun
			}
			return HerdrMutationResult{Outcome: outcome, Raw: stdout.Bytes(), ErrorCode: apiErr.Code, Err: apiErr}
		}
		return HerdrMutationResult{Outcome: outcome, Raw: stdout.Bytes(), Err: fmt.Errorf("herdr %s：%w：%s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))}
	}
	return HerdrMutationResult{Outcome: herdrConfirmed, Raw: stdout.Bytes()}
}

func (c *execHerdrControl) Snapshot(parent context.Context, scope HerdrSnapshotScope) (HerdrSnapshot, error) {
	ctx, cancel := context.WithTimeout(parent, c.SnapshotTimeout)
	defer cancel()
	result := c.run(ctx, false, "api", "snapshot")
	if result.Err != nil {
		return HerdrSnapshot{}, result.Err
	}
	return decodeHerdrSnapshot(result.Raw, scope)
}

func decodeHerdrSnapshot(raw []byte, scope HerdrSnapshotScope) (HerdrSnapshot, error) {
	type rawAgent struct {
		TerminalID       string             `json:"terminal_id"`
		Name             string             `json:"name"`
		Agent            string             `json:"agent"`
		Kind             string             `json:"kind"`
		Status           string             `json:"status"`
		AgentStatus      string             `json:"agent_status"`
		CWD              string             `json:"cwd"`
		WorkspaceID      string             `json:"workspace_id"`
		TabID            string             `json:"tab_id"`
		PaneID           string             `json:"pane_id"`
		InteractiveReady bool               `json:"interactive_ready"`
		AgentSession     *HerdrAgentSession `json:"agent_session"`
		Revision         uint64             `json:"revision"`
	}
	var envelope struct {
		Result struct {
			Snapshot struct {
				Workspaces []HerdrWorkspace `json:"workspaces"`
				Tabs       []HerdrTab       `json:"tabs"`
				Panes      []HerdrPane      `json:"panes"`
				Agents     []rawAgent       `json:"agents"`
			} `json:"snapshot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return HerdrSnapshot{}, fmt.Errorf("解析 herdr snapshot：%w", err)
	}
	snapshot := HerdrSnapshot{Workspaces: envelope.Result.Snapshot.Workspaces, Tabs: envelope.Result.Snapshot.Tabs, Panes: envelope.Result.Snapshot.Panes}
	for _, item := range envelope.Result.Snapshot.Agents {
		kind, status := item.Kind, item.Status
		if kind == "" {
			kind = item.Agent
		}
		if status == "" {
			status = item.AgentStatus
		}
		snapshot.Agents = append(snapshot.Agents, HerdrAgent{
			Name: item.Name, Kind: kind, Status: status, CWD: item.CWD,
			TerminalID: item.TerminalID, WorkspaceID: item.WorkspaceID, TabID: item.TabID, PaneID: item.PaneID,
			InteractiveReady: item.InteractiveReady, AgentSession: item.AgentSession, Revision: item.Revision,
		})
	}
	deriveTabCWDs(&snapshot)
	if err := validateHerdrSnapshot(snapshot, scope); err != nil {
		return HerdrSnapshot{}, err
	}
	return snapshot, nil
}

// Herdr protocol 20 exposes cwd on panes and agents, but not on the tab
// objects returned by api snapshot. Preserve the existing tab-level contract
// only when every pane in that tab provides the same non-empty cwd; otherwise
// leave it empty so target-workspace validation fails closed.
func deriveTabCWDs(snapshot *HerdrSnapshot) {
	type evidence struct {
		cwd      string
		seen     bool
		complete bool
	}
	byTab := map[string]evidence{}
	for _, pane := range snapshot.Panes {
		item := byTab[pane.TabID]
		clean := ""
		if pane.CWD != "" {
			clean = filepath.Clean(pane.CWD)
		}
		if !item.seen {
			item = evidence{cwd: clean, seen: true, complete: clean != ""}
		} else if clean == "" || clean != item.cwd {
			item.complete = false
		}
		byTab[pane.TabID] = item
	}
	for index := range snapshot.Tabs {
		if snapshot.Tabs[index].CWD != "" {
			continue
		}
		item := byTab[snapshot.Tabs[index].ID]
		if item.seen && item.complete {
			snapshot.Tabs[index].CWD = item.cwd
		}
	}
}

func validateHerdrSnapshot(snapshot HerdrSnapshot, scope HerdrSnapshotScope) error {
	if strings.TrimSpace(scope.WorkspaceLabel) == "" {
		return fmt.Errorf("herdr snapshot 缺少目标 workspace label")
	}
	seen := map[string]string{}
	check := func(kind, id, fingerprint string) error {
		if id == "" {
			return fmt.Errorf("herdr snapshot 缺少 %s 稳定 ID", kind)
		}
		key := kind + ":" + id
		if prior, ok := seen[key]; ok && prior != fingerprint {
			return fmt.Errorf("herdr snapshot 同一稳定 ID 冲突：%s", key)
		}
		if _, ok := seen[key]; ok {
			return fmt.Errorf("herdr snapshot 重复稳定 ID：%s", key)
		}
		seen[key] = fingerprint
		return nil
	}
	knownWorkspaces := map[string]bool{}
	targetWorkspaceIDs := map[string]bool{}
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == "" {
			return fmt.Errorf("herdr snapshot workspace %q 缺少 label", workspace.ID)
		}
		if err := check("workspace", workspace.ID, workspace.Label); err != nil {
			return err
		}
		knownWorkspaces[workspace.ID] = true
		if workspace.Label == scope.WorkspaceLabel {
			targetWorkspaceIDs[workspace.ID] = true
		}
	}
	if len(targetWorkspaceIDs) > 1 {
		return fmt.Errorf("herdr snapshot workspace label %q 匹配多个稳定 ID", scope.WorkspaceLabel)
	}
	tabs := map[string]HerdrTab{}
	for _, tab := range snapshot.Tabs {
		if tab.WorkspaceID == "" || tab.Label == "" || (targetWorkspaceIDs[tab.WorkspaceID] && tab.CWD == "") {
			return fmt.Errorf("herdr snapshot tab %q 缺少 workspace/label/cwd", tab.ID)
		}
		if !knownWorkspaces[tab.WorkspaceID] {
			return fmt.Errorf("herdr snapshot tab %q workspace_id=%q 归属不明", tab.ID, tab.WorkspaceID)
		}
		if err := check("tab", tab.ID, fmt.Sprintf("%s\x00%s\x00%s\x00%d", tab.WorkspaceID, tab.Label, tab.CWD, tab.Number)); err != nil {
			return err
		}
		tabs[tab.ID] = tab
	}
	panes := map[string]HerdrPane{}
	for _, pane := range snapshot.Panes {
		if pane.WorkspaceID == "" || pane.TabID == "" || (targetWorkspaceIDs[pane.WorkspaceID] && pane.CWD == "") {
			return fmt.Errorf("herdr snapshot pane %q 缺少 workspace/tab/cwd", pane.ID)
		}
		if !knownWorkspaces[pane.WorkspaceID] {
			return fmt.Errorf("herdr snapshot pane %q workspace_id=%q 归属不明", pane.ID, pane.WorkspaceID)
		}
		if err := check("pane", pane.ID, pane.WorkspaceID+"\x00"+pane.TabID+"\x00"+pane.CWD); err != nil {
			return err
		}
		tab, ok := tabs[pane.TabID]
		if !ok || tab.WorkspaceID != pane.WorkspaceID {
			return fmt.Errorf("herdr snapshot pane %q 的 tab/workspace 关系无法证明", pane.ID)
		}
		if targetWorkspaceIDs[pane.WorkspaceID] && filepath.Clean(tab.CWD) != filepath.Clean(pane.CWD) {
			return fmt.Errorf("herdr snapshot 目标 pane %q 的 tab/pane cwd 冲突", pane.ID)
		}
		panes[pane.ID] = pane
	}
	agentWorkspaces := map[string]string{}
	for _, agent := range snapshot.Agents {
		if agent.WorkspaceID == "" || agent.TabID == "" || agent.PaneID == "" {
			return fmt.Errorf("herdr snapshot agent %q 缺少 name/kind/status/cwd/workspace/tab/pane", agent.Name)
		}
		if !knownWorkspaces[agent.WorkspaceID] {
			return fmt.Errorf("herdr snapshot agent %q workspace_id=%q 归属不明", agent.Name, agent.WorkspaceID)
		}
		target := targetWorkspaceIDs[agent.WorkspaceID]
		if target && (agent.Name == "" || agent.Kind == "" || agent.Status == "" || agent.CWD == "") {
			return fmt.Errorf("herdr snapshot 目标 agent %q 缺少 name/kind/status/cwd/workspace/tab/pane", agent.Name)
		}
		stableAgentID := agent.WorkspaceID + "/" + agent.Name
		if agent.Name == "" {
			stableAgentID = agent.WorkspaceID + "/@pane:" + agent.PaneID
		}
		if err := check("agent", stableAgentID, agent.Kind+"\x00"+agent.Status+"\x00"+agent.CWD+"\x00"+agent.TabID+"\x00"+agent.PaneID); err != nil {
			return err
		}
		tab, tabOK := tabs[agent.TabID]
		pane, paneOK := panes[agent.PaneID]
		if !tabOK || !paneOK || tab.WorkspaceID != agent.WorkspaceID || pane.WorkspaceID != agent.WorkspaceID || pane.TabID != agent.TabID {
			return fmt.Errorf("herdr snapshot agent %q 的 workspace/tab/pane 关系无法证明", agent.Name)
		}
		if target && (filepath.Clean(agent.CWD) != filepath.Clean(tab.CWD) || filepath.Clean(agent.CWD) != filepath.Clean(pane.CWD)) {
			return fmt.Errorf("herdr snapshot 目标 agent %q 的 agent/tab/pane cwd 冲突", agent.Name)
		}
		if target && agent.TerminalID != "" && pane.TerminalID != "" && agent.TerminalID != pane.TerminalID {
			return fmt.Errorf("herdr snapshot 目标 agent %q 的 terminal incarnation 与 pane 冲突", agent.Name)
		}
		if agent.Name != "" {
			if prior, ok := agentWorkspaces[agent.Name]; ok && prior != agent.WorkspaceID && (targetWorkspaceIDs[prior] || target) {
				return fmt.Errorf("herdr snapshot 目标 workspace agent %q 跨 workspace 同名", agent.Name)
			}
			agentWorkspaces[agent.Name] = agent.WorkspaceID
		}
	}
	return nil
}

func (c *execHerdrControl) CreateWorkspace(parent context.Context, label string) (HerdrWorkspace, HerdrMutationResult) {
	ctx, cancel := context.WithTimeout(parent, c.MutationTimeout)
	defer cancel()
	result := c.run(ctx, true, "workspace", "create", "--label", label, "--no-focus")
	if result.Err != nil {
		return HerdrWorkspace{}, result
	}
	var envelope struct {
		Result struct {
			Workspace HerdrWorkspace `json:"workspace"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result.Raw, &envelope); err != nil || envelope.Result.Workspace.ID == "" {
		result.Outcome, result.Err = herdrAmbiguous, fmt.Errorf("解析 herdr workspace create：%w", err)
		return HerdrWorkspace{}, result
	}
	return envelope.Result.Workspace, result
}

func (c *execHerdrControl) CreateTab(parent context.Context, spec HerdrTabSpec) (HerdrTabCreated, HerdrMutationResult) {
	ctx, cancel := context.WithTimeout(parent, c.MutationTimeout)
	defer cancel()
	args := []string{"tab", "create", "--workspace", spec.WorkspaceID, "--cwd", spec.CWD, "--label", spec.Label, "--no-focus"}
	keys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		args = append(args, "--env", key+"="+spec.Env[key])
	}
	result := c.run(ctx, true, args...)
	if result.Err != nil {
		return HerdrTabCreated{}, result
	}
	var envelope struct {
		Result struct {
			Tab      HerdrTab  `json:"tab"`
			RootPane HerdrPane `json:"root_pane"`
		} `json:"result"`
	}
	if err := json.Unmarshal(result.Raw, &envelope); err != nil || envelope.Result.Tab.ID == "" || envelope.Result.RootPane.ID == "" {
		result.Outcome, result.Err = herdrAmbiguous, fmt.Errorf("解析 herdr tab create：%w", err)
		return HerdrTabCreated{}, result
	}
	return HerdrTabCreated{Tab: envelope.Result.Tab, Pane: envelope.Result.RootPane}, result
}

func (c *execHerdrControl) StartAgent(parent context.Context, name, kind, paneID string, native []string) HerdrMutationResult {
	ctx, cancel := context.WithTimeout(parent, c.StartTimeout)
	defer cancel()
	args := []string{"agent", "start", name, "--kind", kind, "--pane", paneID, "--timeout", "60000"}
	if len(native) != 0 {
		args = append(args, "--")
		args = append(args, native...)
	}
	result := c.run(ctx, true, args...)
	if result.Err != nil && result.ErrorCode == "agent_pane_busy" {
		// Herdr proves that it did not start an agent when the newly-created
		// pane's shell has not reached its interactive prompt yet.
		result.Outcome = herdrDefinitelyNotRun
	}
	return result
}

func (c *execHerdrControl) CloseTab(parent context.Context, tabID string) HerdrMutationResult {
	ctx, cancel := context.WithTimeout(parent, c.MutationTimeout)
	defer cancel()
	return c.run(ctx, true, "tab", "close", tabID)
}

func (c *execHerdrControl) RunPane(parent context.Context, paneID, command string) HerdrMutationResult {
	ctx, cancel := context.WithTimeout(parent, c.MutationTimeout)
	defer cancel()
	return c.run(ctx, true, "pane", "run", paneID, command)
}

func (c *execHerdrControl) Prompt(parent context.Context, target, message string) HerdrMutationResult {
	ctx, cancel := context.WithTimeout(parent, c.PromptTimeout)
	defer cancel()
	return c.run(ctx, true, "agent", "prompt", target, message)
}

func (c *execHerdrControl) ReadAgent(parent context.Context, target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, c.SnapshotTimeout)
	defer cancel()
	result := c.run(ctx, false, "agent", "read", target, "--source", "detection", "--lines", "160", "--format", "text")
	if result.Err != nil {
		return nil, result.Err
	}
	return append([]byte(nil), result.Raw...), nil
}
