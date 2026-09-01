package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// LiveBinding is a point-in-time proof that a stable HQ seat is occupied by
// one exact Herdr runtime incarnation. It is diagnostic/execution state, not a
// durable replacement for the registry identity.
type LiveBinding struct {
	WorkspaceID      string             `json:"workspace_id"`
	WorkspaceLabel   string             `json:"workspace_label"`
	TerminalID       string             `json:"terminal_id,omitempty"`
	TabID            string             `json:"tab_id"`
	PaneID           string             `json:"pane_id"`
	Seat             string             `json:"seat"`
	Kind             string             `json:"kind"`
	CWD              string             `json:"cwd"`
	Status           string             `json:"status"`
	InteractiveReady bool               `json:"interactive_ready"`
	Revision         uint64             `json:"revision,omitempty"`
	AgentSession     *HerdrAgentSession `json:"agent_session,omitempty"`
	Rule             AgentRule          `json:"-"`
}

type LiveBindingRequest struct {
	Seat                    string
	PaneID                  string
	RequireInteractiveReady bool
}

func pathWithin(path, root string) bool {
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absPath)
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func ResolveLiveBinding(snapshot HerdrSnapshot, cfg Config, hqRoot string, request LiveBindingRequest) (LiveBinding, error) {
	if strings.TrimSpace(request.Seat) == "" && strings.TrimSpace(request.PaneID) == "" {
		return LiveBinding{}, fmt.Errorf("live binding 必须指定 seat 或 pane_id")
	}
	var workspaceIDs []string
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == cfg.WorkspaceLabel {
			workspaceIDs = append(workspaceIDs, workspace.ID)
		}
	}
	if len(workspaceIDs) != 1 {
		return LiveBinding{}, fmt.Errorf("目标 workspace label=%s 必须精确匹配一个稳定 ID，实际=%d", cfg.WorkspaceLabel, len(workspaceIDs))
	}
	workspaceID := workspaceIDs[0]
	var candidates []HerdrAgent
	for _, agent := range snapshot.Agents {
		if request.Seat != "" && agent.Name != request.Seat {
			continue
		}
		if request.PaneID != "" && agent.PaneID != request.PaneID {
			continue
		}
		candidates = append(candidates, agent)
	}
	if len(candidates) == 0 {
		return LiveBinding{}, fmt.Errorf("herdr 快照中找不到 seat=%q pane=%q 对应的 live agent", request.Seat, request.PaneID)
	}
	if len(candidates) != 1 {
		return LiveBinding{}, fmt.Errorf("live binding 候选不唯一：seat=%q pane=%q count=%d", request.Seat, request.PaneID, len(candidates))
	}
	live := candidates[0]
	rule, ok := cfg.exactRule(live.Name)
	if !ok {
		return LiveBinding{}, fmt.Errorf("当前 agent %q 未登记或已停用", live.Name)
	}
	if live.WorkspaceID != workspaceID {
		return LiveBinding{}, fmt.Errorf("agent %s 位于其他 workspace=%s，要求=%s", live.Name, live.WorkspaceID, workspaceID)
	}
	if !acceptableLiveStatus(live.Status) {
		return LiveBinding{}, fmt.Errorf("agent %s 的 status=%s 不满足在岗合同", live.Name, live.Status)
	}
	if request.RequireInteractiveReady && !live.InteractiveReady {
		return LiveBinding{}, fmt.Errorf("agent %s interactive_ready=false", live.Name)
	}
	if rule.Kind != "" && live.Kind != rule.Kind {
		return LiveBinding{}, fmt.Errorf("agent %s kind=%s 与 registry=%s 不匹配", live.Name, live.Kind, rule.Kind)
	}
	expectedCWD, err := resolveAgentWorkstation(hqRoot, rule)
	if err != nil {
		return LiveBinding{}, fmt.Errorf("agent %s 的登记工位不可用：%w", live.Name, err)
	}
	if !pathWithin(live.CWD, expectedCWD) {
		return LiveBinding{}, fmt.Errorf("agent %s 的 cwd=%s，不在登记工位 %s 内", live.Name, live.CWD, expectedCWD)
	}
	var tab *HerdrTab
	for index := range snapshot.Tabs {
		if snapshot.Tabs[index].ID == live.TabID {
			tab = &snapshot.Tabs[index]
			break
		}
	}
	var pane *HerdrPane
	for index := range snapshot.Panes {
		if snapshot.Panes[index].ID == live.PaneID {
			pane = &snapshot.Panes[index]
			break
		}
	}
	if tab == nil || pane == nil || tab.WorkspaceID != workspaceID || pane.WorkspaceID != workspaceID || pane.TabID != tab.ID {
		return LiveBinding{}, fmt.Errorf("agent %s 的 workspace/tab/pane 关系不完整", live.Name)
	}
	if tab.Label != rosterTabLabel(rule) {
		return LiveBinding{}, fmt.Errorf("agent %s 的 tab label=%q 与 registry=%q 不匹配", live.Name, tab.Label, rosterTabLabel(rule))
	}
	if filepath.Clean(tab.CWD) != expectedCWD || filepath.Clean(pane.CWD) != expectedCWD || filepath.Clean(live.CWD) != expectedCWD {
		return LiveBinding{}, fmt.Errorf("agent %s 的 agent/tab/pane cwd 未精确绑定登记工位", live.Name)
	}
	if live.TerminalID != "" && pane.TerminalID != "" && live.TerminalID != pane.TerminalID {
		return LiveBinding{}, fmt.Errorf("agent %s 的 terminal incarnation 与 pane 不匹配", live.Name)
	}
	terminalID := live.TerminalID
	if terminalID == "" {
		terminalID = pane.TerminalID
	}
	revision := live.Revision
	if pane.Revision > revision {
		revision = pane.Revision
	}
	return LiveBinding{
		WorkspaceID: workspaceID, WorkspaceLabel: cfg.WorkspaceLabel,
		TerminalID: terminalID, TabID: live.TabID, PaneID: live.PaneID,
		Seat: live.Name, Kind: live.Kind, CWD: live.CWD, Status: live.Status,
		InteractiveReady: live.InteractiveReady, Revision: revision,
		AgentSession: live.AgentSession, Rule: rule,
	}, nil
}
