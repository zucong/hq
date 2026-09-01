package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const estopStateVersion = 1

type EstopItem struct {
	Agent               string `json:"agent"`
	Department          string `json:"department"`
	ReportsTo           string `json:"reports_to"`
	WorkspaceID         string `json:"workspace_id"`
	TabID               string `json:"tab_id"`
	PaneID              string `json:"pane_id"`
	CWD                 string `json:"cwd"`
	Kind                string `json:"kind"`
	PlanEventID         string `json:"plan_event_id,omitempty"`
	FreezeOutcome       string `json:"freeze_outcome"`
	FreezeError         string `json:"freeze_error,omitempty"`
	FreezeEventID       string `json:"freeze_event_id,omitempty"`
	FreezeRetryEventID  string `json:"freeze_retry_event_id,omitempty"`
	FreezeRetryable     bool   `json:"freeze_retryable,omitempty"`
	RestoreOutcome      string `json:"restore_outcome,omitempty"`
	RestoreError        string `json:"restore_error,omitempty"`
	RestoreEventID      string `json:"restore_event_id,omitempty"`
	RestoreRetryEventID string `json:"restore_retry_event_id,omitempty"`
	RestoreRetryable    bool   `json:"restore_retryable,omitempty"`
	RestoreFailureClass string `json:"restore_failure_class,omitempty"`
	RestoredWorkspace   string `json:"restored_workspace_id,omitempty"`
	RestoredTab         string `json:"restored_tab_id,omitempty"`
	RestoredPane        string `json:"restored_pane_id,omitempty"`
}

type EstopState struct {
	Version           int         `json:"version"`
	EstopID           string      `json:"estop_id"`
	Actor             string      `json:"actor"`
	ActivatedAt       string      `json:"activated_at"`
	UpdatedAt         string      `json:"updated_at"`
	State             string      `json:"state"`
	Reason            string      `json:"reason"`
	ActivationEventID string      `json:"activation_event_id,omitempty"`
	ReleaseEventID    string      `json:"release_event_id,omitempty"`
	Items             []EstopItem `json:"items"`
}

type estopLedgerRecord struct {
	Activation    Event
	ExpectedCount int
	PlanDigest    string
	Agents        map[string]*estopAgentLedgerState
	Release       Event
}

type estopAgentLedgerState struct {
	Plan         Event
	Freeze       Event
	FreezeRetry  Event
	Restore      Event
	RestoreRetry Event
}

type FileEstopStore struct {
	Root      string
	Failpoint func(string) error
}

type lockedEstopStore struct {
	store *FileEstopStore
	state EstopState
}

func isEstopEvent(eventType string) bool {
	switch eventType {
	case "estop_activated", "estop_agent_planned", "estop_agent_frozen", "estop_agent_freeze_ambiguous", "estop_agent_freeze_failed",
		"estop_agent_freeze_retry", "estop_agent_restored", "estop_agent_restore_ambiguous", "estop_agent_restore_failed",
		"estop_agent_restore_retry", "estop_released":
		return true
	default:
		return false
	}
}

func validateEstopEventFields(event Event) error {
	require := func(fields ...struct{ name, value string }) error { return requireEventFields(event, fields...) }
	switch event.Type {
	case "estop_activated":
		return require(eventField("estop_id", event.EstopID), eventField("note", event.Note),
			eventField("result", event.Result), eventField("payload_digest", event.PayloadDigest))
	case "estop_agent_planned", "estop_agent_frozen", "estop_agent_freeze_ambiguous", "estop_agent_freeze_failed",
		"estop_agent_freeze_retry", "estop_agent_restored", "estop_agent_restore_ambiguous", "estop_agent_restore_failed",
		"estop_agent_restore_retry":
		fields := []struct{ name, value string }{eventField("estop_id", event.EstopID), eventField("related_event_id", event.RelatedEventID),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("workspace_id", event.WorkspaceID), eventField("tab_id", event.TabID), eventField("pane_id", event.PaneID),
			eventField("cwd", event.CWD), eventField("agent_kind", event.AgentKind), eventField("result", event.Result)}
		if event.Type == "estop_agent_freeze_retry" || event.Type == "estop_agent_restore_retry" {
			fields = append(fields, eventField("basis_event_id", event.BasisEventID))
		}
		return require(fields...)
	case "estop_released":
		return require(eventField("estop_id", event.EstopID), eventField("related_event_id", event.RelatedEventID), eventField("note", event.Note))
	default:
		return fmt.Errorf("未知 ESTOP 事件类型 %q", event.Type)
	}
}

func validEstopEventResult(event Event) bool {
	switch event.Type {
	case "estop_agent_planned":
		return event.Result == "planned"
	case "estop_agent_frozen", "estop_agent_restored":
		return event.Result == "confirmed"
	case "estop_agent_freeze_ambiguous", "estop_agent_restore_ambiguous":
		return event.Result == "ambiguous"
	case "estop_agent_freeze_failed":
		return event.Result == "definitely-not-run"
	case "estop_agent_restore_failed":
		return event.Result == "definitely-not-run" || event.Result == "confirmed-rollback"
	case "estop_agent_freeze_retry", "estop_agent_restore_retry":
		return event.Result == "retry-authorized"
	case "estop_activated":
		count, err := strconv.Atoi(event.Result)
		return err == nil && count >= 0 && validateDigest("payload_digest", event.PayloadDigest) == nil
	case "estop_released":
		return event.Result == ""
	default:
		return false
	}
}

func estopPlanIdentity(recipient, label, workspaceID, tabID, paneID, cwd, kind string) string {
	return strings.Join([]string{recipient, label, workspaceID, tabID, paneID, filepath.Clean(cwd), kind}, "\x00")
}

func estopPlanDigestForItems(cfg Config, items []EstopItem) string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		rule, _ := historicalRule(cfg, item.Agent)
		lines = append(lines, estopPlanIdentity(item.Agent, rule.Label, item.WorkspaceID, item.TabID, item.PaneID, item.CWD, item.Kind))
	}
	sort.Strings(lines)
	return digestText(strings.Join(lines, "\n"))
}

func (r *estopLedgerRecord) planComplete() bool {
	if r == nil || len(r.Agents) != r.ExpectedCount {
		return false
	}
	lines := make([]string, 0, len(r.Agents))
	for _, agent := range r.Agents {
		if agent == nil || agent.Plan.ID == "" {
			return false
		}
		plan := agent.Plan
		lines = append(lines, estopPlanIdentity(plan.Recipient, plan.RecipientLabel, plan.WorkspaceID, plan.TabID, plan.PaneID, plan.CWD, plan.AgentKind))
	}
	sort.Strings(lines)
	return digestText(strings.Join(lines, "\n")) == r.PlanDigest
}

func sameEstopAgentIdentity(left, right Event) bool {
	return estopPlanIdentity(left.Recipient, left.RecipientLabel, left.WorkspaceID, left.TabID, left.PaneID, left.CWD, left.AgentKind) ==
		estopPlanIdentity(right.Recipient, right.RecipientLabel, right.WorkspaceID, right.TabID, right.PaneID, right.CWD, right.AgentKind)
}

func (s *ledgerState) applyEstopEvent(event Event, cfg Config) error {
	if err := requireNoStateFields(event); err != nil {
		return err
	}
	if err := validateLedgerID("estop_id", event.EstopID); err != nil {
		return err
	}
	if !validEstopEventResult(event) {
		return fmt.Errorf("%s result=%q 与事件类型不匹配", event.Type, event.Result)
	}
	actor, ok := configRuleIncludingDisabled(cfg, event.Actor)
	if !ok || !actor.CanManageStaff {
		return fmt.Errorf("estop actor %s 必须精确登记且具备 can_manage_staff", event.Actor)
	}
	switch event.Type {
	case "estop_activated":
		if existing := s.estops[event.EstopID]; existing != nil {
			return fmt.Errorf("estop_id 重复：%s", event.EstopID)
		}
		expected, _ := strconv.Atoi(event.Result)
		s.estops[event.EstopID] = &estopLedgerRecord{Activation: event, ExpectedCount: expected, PlanDigest: event.PayloadDigest, Agents: map[string]*estopAgentLedgerState{}}
	case "estop_agent_planned", "estop_agent_frozen", "estop_agent_freeze_ambiguous", "estop_agent_freeze_failed",
		"estop_agent_freeze_retry", "estop_agent_restored", "estop_agent_restore_ambiguous", "estop_agent_restore_failed",
		"estop_agent_restore_retry":
		record := s.estops[event.EstopID]
		if record == nil || record.Release.ID != "" || event.RelatedEventID != record.Activation.ID {
			return fmt.Errorf("estop agent 事件未关联 active activation")
		}
		if _, ok := historicalRule(cfg, event.Recipient); !ok {
			return fmt.Errorf("estop recipient 未登记：%s", event.Recipient)
		}
		if err := validateLedgerID("workspace_id", event.WorkspaceID); err != nil {
			return err
		}
		if err := validateLedgerID("tab_id", event.TabID); err != nil {
			return err
		}
		if err := validateLedgerID("pane_id", event.PaneID); err != nil {
			return err
		}
		agent := record.Agents[event.Recipient]
		if event.Type == "estop_agent_planned" {
			if agent != nil || len(record.Agents) >= record.ExpectedCount {
				return fmt.Errorf("estop agent plan 重复或超过 activation expected count")
			}
			record.Agents[event.Recipient] = &estopAgentLedgerState{Plan: event}
			return nil
		}
		if agent == nil || agent.Plan.ID == "" {
			return fmt.Errorf("estop agent %s outcome/retry 缺少 planned 事实", event.Recipient)
		}
		if !record.planComplete() {
			return fmt.Errorf("estop expected frozen set 尚未由主账本完整证明")
		}
		if !sameEstopAgentIdentity(agent.Plan, event) {
			return fmt.Errorf("estop agent %s outcome 身份字段偏离 planned 事实", event.Recipient)
		}
		switch event.Type {
		case "estop_agent_freeze_retry":
			if agent.Freeze.Type != "estop_agent_freeze_failed" || agent.FreezeRetry.ID != "" || event.BasisEventID != agent.Freeze.ID {
				return fmt.Errorf("estop freeze retry 未关联当前 definitely-not-run 事实")
			}
			agent.FreezeRetry = event
		case "estop_agent_frozen", "estop_agent_freeze_ambiguous", "estop_agent_freeze_failed":
			if agent.Restore.ID != "" || (agent.Freeze.ID != "" && agent.FreezeRetry.ID == "") {
				return fmt.Errorf("estop agent %s freeze outcome 冲突或重复", event.Recipient)
			}
			agent.Freeze, agent.FreezeRetry = event, Event{}
		case "estop_agent_restore_retry":
			if agent.Freeze.Type != "estop_agent_frozen" || agent.Restore.Type != "estop_agent_restore_failed" ||
				agent.RestoreRetry.ID != "" || event.BasisEventID != agent.Restore.ID {
				return fmt.Errorf("estop restore retry 未关联当前可安全重试失败事实")
			}
			agent.RestoreRetry = event
		case "estop_agent_restored", "estop_agent_restore_ambiguous", "estop_agent_restore_failed":
			if agent.Freeze.Type != "estop_agent_frozen" || (agent.Restore.ID != "" && agent.RestoreRetry.ID == "") {
				return fmt.Errorf("estop agent %s restore-before-freeze 或 outcome 冲突", event.Recipient)
			}
			agent.Restore, agent.RestoreRetry = event, Event{}
		}
	case "estop_released":
		record := s.estops[event.EstopID]
		if record == nil || record.Release.ID != "" || event.RelatedEventID != record.Activation.ID {
			return fmt.Errorf("estop release 未关联唯一 active activation")
		}
		if !record.planComplete() {
			return fmt.Errorf("estop release 前 expected frozen set 未完整落账")
		}
		for recipient, agent := range record.Agents {
			if agent.Freeze.Type != "estop_agent_frozen" || agent.Restore.Type != "estop_agent_restored" ||
				agent.FreezeRetry.ID != "" || agent.RestoreRetry.ID != "" {
				return fmt.Errorf("estop release 前 agent %s 未完成 confirmed freeze -> confirmed restore", recipient)
			}
		}
		record.Release = event
	}
	return nil
}

func validateEstopState(state EstopState) error {
	if state.Version != estopStateVersion {
		return fmt.Errorf("不支持 estop state version %d", state.Version)
	}
	if err := validateLedgerID("estop_id", state.EstopID); err != nil {
		return err
	}
	if state.Actor == "" || state.Reason == "" || (state.State != "active" && state.State != "released") {
		return fmt.Errorf("estop state 缺少 actor/reason 或状态非法")
	}
	activated, err := time.Parse(time.RFC3339, state.ActivatedAt)
	if err != nil {
		return fmt.Errorf("activated_at 非法：%w", err)
	}
	updated, err := time.Parse(time.RFC3339, state.UpdatedAt)
	if err != nil || updated.Before(activated) {
		return fmt.Errorf("updated_at 非法或早于 activated_at")
	}
	seen := map[string]bool{}
	for _, item := range state.Items {
		if item.Agent == "" || item.WorkspaceID == "" || item.TabID == "" || item.PaneID == "" || item.CWD == "" || item.Kind == "" || item.Department == "" {
			return fmt.Errorf("estop item 缺少稳定 agent/workspace/tab/pane/cwd/kind/department")
		}
		if seen[item.Agent] {
			return fmt.Errorf("estop item agent 重复：%s", item.Agent)
		}
		seen[item.Agent] = true
		switch item.FreezeOutcome {
		case "pending", "attempting", "confirmed", "ambiguous", "failed":
		default:
			return fmt.Errorf("estop item %s freeze_outcome 非法：%s", item.Agent, item.FreezeOutcome)
		}
		if item.FreezeRetryable && item.FreezeOutcome != "failed" {
			return fmt.Errorf("estop item %s 仅 failed freeze 可标记 retryable", item.Agent)
		}
		if item.RestoreOutcome != "" {
			switch item.RestoreOutcome {
			case "attempting", "confirmed", "ambiguous", "failed":
			default:
				return fmt.Errorf("estop item %s restore_outcome 非法：%s", item.Agent, item.RestoreOutcome)
			}
		}
		if item.RestoreRetryable && item.RestoreOutcome != "failed" {
			return fmt.Errorf("estop item %s 仅 failed restore 可标记 retryable", item.Agent)
		}
		if item.RestoreFailureClass != "" && (item.RestoreOutcome != "failed" ||
			(item.RestoreFailureClass != "definitely-not-run" && item.RestoreFailureClass != "confirmed-rollback")) {
			return fmt.Errorf("estop item %s restore_failure_class 非法", item.Agent)
		}
	}
	if state.State == "released" {
		if state.ReleaseEventID == "" {
			return fmt.Errorf("released estop 缺少 release_event_id")
		}
		for _, item := range state.Items {
			if item.FreezeOutcome != "confirmed" || item.RestoreOutcome != "confirmed" {
				return fmt.Errorf("released estop 含未确认恢复 item：%s", item.Agent)
			}
		}
	}
	return nil
}

func decodeStrictEstop(raw []byte, state *EstopState) error {
	if err := decodeStrictJSON(raw, state); err != nil {
		return err
	}
	return validateEstopState(*state)
}

func validateOwnedMode(path string, mode os.FileMode, directory bool) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || (directory && !info.IsDir()) || (!directory && !info.Mode().IsRegular()) {
		return fmt.Errorf("路径必须是非 symlink %s：%s", map[bool]string{true: "目录", false: "普通文件"}[directory], path)
	}
	if info.Mode().Perm() != mode {
		return fmt.Errorf("%s mode=%04o，要求 %04o", path, info.Mode().Perm(), mode)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Getuid() {
		return fmt.Errorf("%s owner 不是当前 uid", path)
	}
	return nil
}

func (s *FileEstopStore) statePath() string { return filepath.Join(s.Root, "state.json") }
func (s *FileEstopStore) tempPath() string  { return filepath.Join(s.Root, ".state.tmp") }

func (s *FileEstopStore) Read() (EstopState, bool, error) {
	if s == nil || s.Root == "" {
		return EstopState{}, false, fmt.Errorf("estop store 未注入")
	}
	if _, err := os.Lstat(s.tempPath()); err == nil {
		return EstopState{}, false, fmt.Errorf("estop 存在未恢复 temp，读路径 fail-closed：%s", s.tempPath())
	} else if !os.IsNotExist(err) {
		return EstopState{}, false, err
	}
	path := s.statePath()
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return EstopState{}, false, nil
	} else if err != nil {
		return EstopState{}, false, err
	}
	if err := validateOwnedMode(s.Root, 0o700, true); err != nil {
		return EstopState{}, false, err
	}
	if err := validateOwnedMode(path, 0o600, false); err != nil {
		return EstopState{}, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return EstopState{}, false, err
	}
	var state EstopState
	if err := decodeStrictEstop(raw, &state); err != nil {
		return EstopState{}, false, fmt.Errorf("estop state 损坏 %s：%w", path, err)
	}
	return state, true, nil
}

func (s *FileEstopStore) hit(name string) error {
	if s.Failpoint == nil {
		return nil
	}
	if err := s.Failpoint(name); err != nil {
		return fmt.Errorf("estop failpoint %s: %w", name, err)
	}
	return nil
}

func (s *FileEstopStore) WithLock(fn func(*lockedEstopStore) error) error {
	if s == nil || s.Root == "" {
		return fmt.Errorf("estop store 未注入")
	}
	if err := os.MkdirAll(s.Root, 0o700); err != nil {
		return err
	}
	if err := validateOwnedMode(s.Root, 0o700, true); err != nil {
		return err
	}
	lockPath := filepath.Join(s.Root, ".lock")
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := validateOwnedMode(lockPath, 0o600, false); err != nil {
		return err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := s.recoverTempLocked(); err != nil {
		return err
	}
	state, exists, err := s.readLocked()
	if err != nil {
		return err
	}
	locked := &lockedEstopStore{store: s}
	if exists {
		locked.state = state
	}
	return fn(locked)
}

func (s *FileEstopStore) readLocked() (EstopState, bool, error) {
	path := s.statePath()
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return EstopState{}, false, nil
	} else if err != nil {
		return EstopState{}, false, err
	}
	if err := validateOwnedMode(path, 0o600, false); err != nil {
		return EstopState{}, false, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return EstopState{}, false, err
	}
	var state EstopState
	if err := decodeStrictEstop(raw, &state); err != nil {
		return EstopState{}, false, fmt.Errorf("estop state 损坏 %s：%w", path, err)
	}
	return state, true, nil
}

func validEstopRecovery(old, next EstopState) bool {
	oldTime, oldErr := time.Parse(time.RFC3339, old.UpdatedAt)
	nextTime, nextErr := time.Parse(time.RFC3339, next.UpdatedAt)
	if oldErr != nil || nextErr != nil || nextTime.Before(oldTime) {
		return false
	}
	return old.EstopID == next.EstopID || (old.State == "released" && next.State == "active")
}

func (s *FileEstopStore) recoverTempLocked() error {
	tmp := s.tempPath()
	if _, err := os.Lstat(tmp); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if err := validateOwnedMode(tmp, 0o600, false); err != nil {
		return err
	}
	raw, err := os.ReadFile(tmp)
	if err != nil {
		return err
	}
	var next EstopState
	if err := decodeStrictEstop(raw, &next); err != nil {
		return fmt.Errorf("estop temp 损坏：%w", err)
	}
	old, exists, err := s.readLocked()
	if err != nil {
		return err
	}
	if exists {
		oldRaw, _ := json.Marshal(old)
		nextRaw, _ := json.Marshal(next)
		if bytes.Equal(oldRaw, nextRaw) {
			return os.Remove(tmp)
		}
		if !validEstopRecovery(old, next) {
			return fmt.Errorf("estop temp 与 state 无可证明前后关系，拒绝恢复")
		}
	}
	if err := os.Rename(tmp, s.statePath()); err != nil {
		return err
	}
	return syncDir(s.Root)
}

func (l *lockedEstopStore) Write(state EstopState) error {
	if err := validateEstopState(state); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := l.store.tempPath()
	if _, err := os.Lstat(tmp); err == nil {
		return fmt.Errorf("estop temp 已存在，拒绝覆盖：%s", tmp)
	} else if !os.IsNotExist(err) {
		return err
	}
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(raw, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := l.store.hit("after_temp_write"); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := l.store.hit("after_temp_fsync"); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.store.statePath()); err != nil {
		return err
	}
	if err := l.store.hit("after_rename"); err != nil {
		return err
	}
	if err := syncDir(l.store.Root); err != nil {
		return err
	}
	if err := l.store.hit("after_parent_fsync"); err != nil {
		return err
	}
	l.state = state
	return nil
}

func (a *App) authorizeEstopLive() (Actor, error) {
	if a.Identity == nil || a.ConfigPath == "" {
		return Actor{}, fmt.Errorf("ESTOP 必须注入 IdentityProvider 与实时 config path")
	}
	live, err := loadConfig(a.ConfigPath)
	if err != nil {
		return Actor{}, fmt.Errorf("ESTOP 写前重读实时 config：%w", err)
	}
	pane := a.MaintenancePane
	if pane == "" {
		pane = a.CallerPane
	}
	if pane == "" {
		pane = os.Getenv("HERDR_PANE_ID")
	}
	if pane == "" {
		return Actor{}, fmt.Errorf("ESTOP 缺少调用者 pane_id")
	}
	actor, err := a.resolveIdentity(live, pane)
	if err != nil {
		return Actor{}, err
	}
	rule, ok := live.exactRule(actor.Name)
	if !ok || !rule.CanManageStaff {
		return Actor{}, permissionf("actor %s 已失去实时 can_manage_staff 权限", actor.Name)
	}
	actor.Rule, actor.Label = rule, rule.Label
	a.Config, a.MaintenanceActor = live, actor.Name
	return actor, nil
}

func planEstopItems(snapshot HerdrSnapshot, cfg Config, hqRoot string) ([]EstopItem, error) {
	var workspaceIDs []string
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == cfg.WorkspaceLabel {
			workspaceIDs = append(workspaceIDs, workspace.ID)
		}
	}
	if len(workspaceIDs) != 1 {
		return nil, fmt.Errorf("ESTOP 要求 workspace label 精确匹配一个稳定 ID，实际=%d", len(workspaceIDs))
	}
	workspaceID := workspaceIDs[0]
	tabs, panes := map[string]HerdrTab{}, map[string]HerdrPane{}
	for _, tab := range snapshot.Tabs {
		tabs[tab.ID] = tab
	}
	for _, pane := range snapshot.Panes {
		panes[pane.ID] = pane
	}
	seenNames := map[string]int{}
	for _, agent := range snapshot.Agents {
		seenNames[agent.Name]++
	}
	var items []EstopItem
	for _, agent := range snapshot.Agents {
		if agent.WorkspaceID != workspaceID {
			continue
		}
		if seenNames[agent.Name] != 1 {
			return nil, fmt.Errorf("ESTOP agent name 跨 workspace/现场歧义：%s", agent.Name)
		}
		exactRule, exact := cfg.exactRule(agent.Name)
		if exact && (cfg.isManager(exactRule) || exactRule.hasResponsibility(roleAccountCloser)) {
			continue
		}
		rule, ok := cfg.ruleFor(agent.Name)
		if !ok || rule.Disabled {
			return nil, fmt.Errorf("ESTOP 无法把 live agent 精确归属到 active config/pattern：%s", agent.Name)
		}
		if !acceptableLiveStatus(agent.Status) {
			return nil, fmt.Errorf("ESTOP agent %s status=%s 关系不明", agent.Name, agent.Status)
		}
		tab, tabOK := tabs[agent.TabID]
		pane, paneOK := panes[agent.PaneID]
		expectedCWD, workstationErr := resolveAgentWorkstation(hqRoot, rule)
		if workstationErr != nil {
			return nil, fmt.Errorf("ESTOP agent %s 登记工位不可用：%w", agent.Name, workstationErr)
		}
		if !tabOK || !paneOK || tab.WorkspaceID != workspaceID || pane.WorkspaceID != workspaceID || pane.TabID != tab.ID ||
			tab.ID != agent.TabID || filepath.Clean(agent.CWD) != expectedCWD || filepath.Clean(tab.CWD) != expectedCWD || filepath.Clean(pane.CWD) != expectedCWD ||
			(rule.Kind != "" && rule.Kind != agent.Kind) {
			return nil, fmt.Errorf("ESTOP agent/tab/pane/cwd/kind 关系歧义：%s", agent.Name)
		}
		items = append(items, EstopItem{Agent: agent.Name, Department: rule.Department, ReportsTo: rule.ReportsTo,
			WorkspaceID: workspaceID, TabID: agent.TabID, PaneID: agent.PaneID, CWD: agent.CWD, Kind: agent.Kind, FreezeOutcome: "pending"})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Agent < items[j].Agent })
	return items, nil
}

func (a *App) cmdEstop(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq estop activate|release|status")
	}
	switch args[0] {
	case "activate":
		return a.cmdEstopActivate(args[1:])
	case "release":
		return a.cmdEstopRelease(args[1:])
	case "status":
		return a.cmdEstopStatus(args[1:])
	default:
		return fmt.Errorf("未知 estop 子命令 %q", args[0])
	}
}

func (a *App) hitEstopFailpoint(name string) error {
	if a.EstopFailpoint == nil {
		return nil
	}
	if err := a.EstopFailpoint(name); err != nil {
		return fmt.Errorf("estop failpoint %s: %w", name, err)
	}
	return nil
}

func (a *App) appendEstopEvent(actor Actor, state *EstopState, item *EstopItem, eventType, result, note string) (Event, error) {
	planDigest := ""
	if eventType == "estop_activated" {
		result = strconv.Itoa(len(state.Items))
		planDigest = estopPlanDigestForItems(a.Config, state.Items)
	}
	parts := []string{state.EstopID, eventType}
	if item != nil {
		parts = append(parts, item.Agent)
		switch eventType {
		case "estop_agent_freeze_retry":
			parts = append(parts, item.FreezeEventID)
		case "estop_agent_restore_retry":
			parts = append(parts, item.RestoreEventID)
		case "estop_agent_frozen", "estop_agent_freeze_ambiguous", "estop_agent_freeze_failed":
			parts = append(parts, item.FreezeRetryEventID)
		case "estop_agent_restored", "estop_agent_restore_ambiguous", "estop_agent_restore_failed":
			parts = append(parts, item.RestoreRetryEventID)
		}
	}
	commandID := stableCommandID("estop-event", parts...)
	digest := requestDigest("estop-event", strings.Join(parts, "\x00"), result, note, planDigest)
	txn, err := a.Store.Transact(a.Config, commandID, digest, false, func(ledger *ledgerState) (Event, error) {
		event, err := a.newOperationsEvent(actor, eventType, estopSystemCaseID)
		if err != nil {
			return Event{}, err
		}
		event.EstopID, event.Note = state.EstopID, note
		if eventType == "estop_activated" {
			event.Result, event.PayloadDigest = result, planDigest
		}
		if eventType != "estop_activated" {
			record := ledger.estops[state.EstopID]
			if record == nil {
				return Event{}, fmt.Errorf("estop activation 尚未落账")
			}
			event.RelatedEventID = record.Activation.ID
		}
		if item != nil {
			rule, _ := historicalRule(a.Config, item.Agent)
			event.Recipient, event.RecipientLabel = item.Agent, rule.Label
			event.WorkspaceID, event.TabID, event.PaneID = item.WorkspaceID, item.TabID, item.PaneID
			event.CWD, event.AgentKind, event.Result = item.CWD, item.Kind, result
			switch eventType {
			case "estop_agent_freeze_retry":
				event.BasisEventID = item.FreezeEventID
			case "estop_agent_restore_retry":
				event.BasisEventID = item.RestoreEventID
			}
		}
		return event, nil
	})
	return txn.Event, err
}

func replayEstopRecord(store EventStore, cfg Config, estopID string) (*estopLedgerRecord, error) {
	if store == nil {
		return nil, fmt.Errorf("ESTOP 主事件账本未注入")
	}
	events, err := store.ReadAll(cfg)
	if err != nil {
		return nil, fmt.Errorf("严格重放 ESTOP 主事件账本：%w", err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		return nil, fmt.Errorf("严格重放 ESTOP 主事件账本：%w", err)
	}
	record := ledger.estops[estopID]
	if record == nil {
		return nil, fmt.Errorf("ESTOP %s 缺少 activation 事实", estopID)
	}
	return record, nil
}

func estopFreezeType(outcome string) string {
	return map[string]string{
		"confirmed": "estop_agent_frozen", "ambiguous": "estop_agent_freeze_ambiguous", "failed": "estop_agent_freeze_failed",
	}[outcome]
}

func estopRestoreType(outcome string) string {
	return map[string]string{
		"confirmed": "estop_agent_restored", "ambiguous": "estop_agent_restore_ambiguous", "failed": "estop_agent_restore_failed",
	}[outcome]
}

func estopRestoreOutcome(event Event) string {
	switch event.Type {
	case "estop_agent_restored":
		return "confirmed"
	case "estop_agent_restore_ambiguous":
		return "ambiguous"
	case "estop_agent_restore_failed":
		return "failed"
	default:
		return ""
	}
}

func validateEstopItemAgainstLedger(item EstopItem, record *estopLedgerRecord) error {
	agent := record.Agents[item.Agent]
	if agent == nil {
		if item.PlanEventID != "" || item.FreezeEventID != "" || item.FreezeRetryEventID != "" || item.RestoreEventID != "" || item.RestoreRetryEventID != "" {
			return fmt.Errorf("agent %s sentinel 引用主账本不存在的事件", item.Agent)
		}
		return nil
	}
	if !record.planComplete() || item.PlanEventID == "" || item.PlanEventID != agent.Plan.ID {
		return fmt.Errorf("agent %s planned set/event sentinel/账本冲突", item.Agent)
	}
	if item.FreezeRetryEventID != agent.FreezeRetry.ID {
		return fmt.Errorf("agent %s freeze retry event sentinel/账本冲突", item.Agent)
	}
	if item.FreezeRetryEventID != "" && (item.FreezeOutcome == "pending" || item.FreezeOutcome == "attempting") {
		if item.FreezeEventID != agent.Freeze.ID || agent.Freeze.Type != "estop_agent_freeze_failed" {
			return fmt.Errorf("agent %s freeze retry basis sentinel/账本冲突", item.Agent)
		}
		return nil
	}
	if item.FreezeEventID == "" || item.FreezeEventID != agent.Freeze.ID || estopFreezeType(item.FreezeOutcome) != agent.Freeze.Type {
		return fmt.Errorf("agent %s freeze outcome/event sentinel/账本冲突", item.Agent)
	}
	if item.FreezeOutcome == "failed" && (!item.FreezeRetryable || agent.Freeze.Result != "definitely-not-run") {
		return fmt.Errorf("agent %s freeze failed 重试分类 sentinel/账本冲突", item.Agent)
	}
	if item.RestoreRetryEventID != agent.RestoreRetry.ID {
		return fmt.Errorf("agent %s restore retry event sentinel/账本冲突", item.Agent)
	}
	if item.RestoreOutcome == "" {
		if item.RestoreRetryEventID != "" && agent.Restore.Type == "estop_agent_restore_failed" {
			return nil
		}
		if agent.Restore.ID != "" {
			return fmt.Errorf("agent %s sentinel 遗漏账本 restore 事实", item.Agent)
		}
		return nil
	}
	if item.RestoreEventID == "" || item.RestoreEventID != agent.Restore.ID || estopRestoreType(item.RestoreOutcome) != agent.Restore.Type {
		return fmt.Errorf("agent %s restore outcome/event sentinel/账本冲突", item.Agent)
	}
	if item.RestoreOutcome == "failed" && (!item.RestoreRetryable || item.RestoreFailureClass != agent.Restore.Result) {
		return fmt.Errorf("agent %s restore failed 重试分类 sentinel/账本冲突", item.Agent)
	}
	return nil
}

func validateEstopPlanAgainstState(state EstopState, record *estopLedgerRecord, cfg Config) error {
	if !record.planComplete() || len(state.Items) != record.ExpectedCount || estopPlanDigestForItems(cfg, state.Items) != record.PlanDigest {
		return fmt.Errorf("ESTOP expected frozen set sentinel/主账本计数或摘要冲突")
	}
	return nil
}

func (a *App) reconcileFreezeFactsBeforeRelease(actor Actor, locked *lockedEstopStore, state *EstopState, snapshot HerdrSnapshot) error {
	record, err := replayEstopRecord(a.Store, a.Config, state.EstopID)
	if err != nil {
		return err
	}
	if record.Release.ID != "" || record.Activation.ID == "" {
		return fmt.Errorf("ESTOP 主账本 activation 已 release 或缺失")
	}
	if state.ActivationEventID == "" {
		state.ActivationEventID = record.Activation.ID
		state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
		if err := locked.Write(*state); err != nil {
			return err
		}
	} else if state.ActivationEventID != record.Activation.ID {
		return fmt.Errorf("ESTOP activation sentinel/账本冲突")
	}
	if err := validateEstopPlanAgainstState(*state, record, a.Config); err != nil {
		return err
	}
	for i := range state.Items {
		item := &state.Items[i]
		agent := record.Agents[item.Agent]
		if item.FreezeOutcome == "confirmed" && (agent == nil || agent.Freeze.ID == "") {
			if item.FreezeEventID != "" || snapshotHasTab(snapshot, item.TabID) {
				return fmt.Errorf("agent %s confirmed freeze 无法从 sentinel crash window 证明", item.Agent)
			}
			event, err := a.appendEstopEvent(actor, state, item, "estop_agent_frozen", "confirmed", "sentinel confirmed close; pre-release snapshot proves original tab absent")
			if err != nil {
				return err
			}
			item.FreezeEventID = event.ID
			state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(*state); err != nil {
				return err
			}
			record, err = replayEstopRecord(a.Store, a.Config, state.EstopID)
			if err != nil {
				return err
			}
		}
		agent = record.Agents[item.Agent]
		if agent != nil && agent.RestoreRetry.ID == "" && agent.Restore.ID != "" && item.RestoreEventID != agent.Restore.ID {
			ledgerOutcome := estopRestoreOutcome(agent.Restore)
			recoverableLead := item.RestoreOutcome == "" || item.RestoreOutcome == "attempting" ||
				(item.RestoreOutcome == ledgerOutcome && item.RestoreEventID == "") || item.RestoreRetryEventID != ""
			if !recoverableLead || ledgerOutcome == "" {
				return fmt.Errorf("agent %s restore outcome 账本领先窗口与 sentinel 冲突", item.Agent)
			}
			if ledgerOutcome == "confirmed" {
				rule, ok := a.Config.ruleFor(item.Agent)
				if !ok {
					return fmt.Errorf("agent %s confirmed restore 账本领先但 current rule 缺失", item.Agent)
				}
				live, exists := findExactLiveAgent(snapshot, item.WorkspaceID, item.Agent)
				if matched, _ := exactLiveMatch(snapshot, item.WorkspaceID, rule, a.HQRoot); !exists || !matched {
					return fmt.Errorf("agent %s confirmed restore 账本领先但无精确 live 证据", item.Agent)
				}
				item.RestoredWorkspace, item.RestoredTab, item.RestoredPane = live.WorkspaceID, live.TabID, live.PaneID
			}
			item.RestoreOutcome, item.RestoreEventID, item.RestoreRetryEventID = ledgerOutcome, agent.Restore.ID, ""
			item.RestoreError = agent.Restore.Note
			item.RestoreFailureClass = ""
			item.RestoreRetryable = false
			if ledgerOutcome == "failed" {
				item.RestoreFailureClass, item.RestoreRetryable = agent.Restore.Result, true
			}
			state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(*state); err != nil {
				return err
			}
		}
		agent = record.Agents[item.Agent]
		if agent != nil && item.RestoreOutcome == "failed" && item.RestoreRetryable && item.RestoreRetryEventID == "" &&
			agent.RestoreRetry.ID != "" && agent.RestoreRetry.BasisEventID == item.RestoreEventID {
			item.RestoreRetryEventID = agent.RestoreRetry.ID
			item.RestoreOutcome, item.RestoreError, item.RestoreFailureClass, item.RestoreRetryable = "", "", "", false
			state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(*state); err != nil {
				return err
			}
		}
		if agent != nil && item.RestoreRetryEventID != "" && agent.RestoreRetry.ID == "" && item.RestoreOutcome != "" &&
			agent.Restore.ID != "" && estopRestoreType(item.RestoreOutcome) == agent.Restore.Type {
			item.RestoreEventID, item.RestoreRetryEventID = agent.Restore.ID, ""
			state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(*state); err != nil {
				return err
			}
		}
		agent = record.Agents[item.Agent]
		if item.RestoreOutcome != "" && (agent == nil || agent.Restore.ID == "") {
			if item.RestoreEventID != "" {
				return fmt.Errorf("agent %s restore sentinel 引用不存在的主账本事件", item.Agent)
			}
			result := map[string]string{"confirmed": "confirmed", "ambiguous": "ambiguous", "failed": item.RestoreFailureClass}[item.RestoreOutcome]
			if item.RestoreOutcome == "confirmed" {
				rule, ok := a.Config.ruleFor(item.Agent)
				if !ok {
					return fmt.Errorf("agent %s restore crash window 无 current rule", item.Agent)
				}
				if matched, _ := exactLiveMatch(snapshot, item.WorkspaceID, rule, a.HQRoot); !matched {
					return fmt.Errorf("agent %s confirmed restore 无法由精确 live snapshot 证明", item.Agent)
				}
			} else if item.RestoreOutcome == "failed" {
				if !item.RestoreRetryable || (result != "definitely-not-run" && result != "confirmed-rollback") {
					return fmt.Errorf("agent %s failed restore 缺少可审计安全重试分类", item.Agent)
				}
				if _, exists := findExactLiveAgent(snapshot, item.WorkspaceID, item.Agent); exists {
					return fmt.Errorf("agent %s failed restore 但 live child 存在", item.Agent)
				}
			}
			event, err := a.appendEstopEvent(actor, state, item, estopRestoreType(item.RestoreOutcome), result, item.RestoreError)
			if err != nil {
				return err
			}
			item.RestoreEventID = event.ID
			state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(*state); err != nil {
				return err
			}
			record, err = replayEstopRecord(a.Store, a.Config, state.EstopID)
			if err != nil {
				return err
			}
		}
		if err := validateEstopItemAgainstLedger(*item, record); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) cmdEstopActivate(args []string) error {
	fs := newLeafParser("estop activate")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "稳定 estop id")
	reason := fs.String("reason", "", "单行短原因")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq estop activate --id ID --reason TEXT")
	}
	if err := validateLedgerID("estop_id", strings.TrimSpace(*id)); err != nil {
		return err
	}
	cleanReason, err := validateShortText("reason", *reason, true)
	if err != nil {
		return err
	}
	if a.Herdr == nil || a.Estop == nil {
		return fmt.Errorf("ESTOP 必须显式注入 Herdr control 与 state store")
	}
	actor, err := a.authorizeEstopLive()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if a.DryRun {
		snapshot, err := a.herdrSnapshot(ctx)
		if err != nil {
			return err
		}
		items, err := planEstopItems(snapshot, a.Config, a.HQRoot)
		if err != nil {
			return err
		}
		return a.output(EstopState{Version: estopStateVersion, EstopID: *id, Actor: actor.Name, State: "active", Reason: cleanReason, Items: items},
			fmt.Sprintf("DRY-RUN：将冻结 %d 个子角色；未写 sentinel/账本，未调用 close", len(items)))
	}
	var final EstopState
	err = a.Estop.WithLock(func(locked *lockedEstopStore) error {
		state := locked.state
		if state.EstopID != "" {
			if state.State == "active" && state.EstopID != *id {
				return fmt.Errorf("已有 active estop=%s", state.EstopID)
			}
			if state.State == "released" && state.EstopID == *id {
				final = state
				return nil
			}
			if state.State == "released" && state.EstopID != *id {
				state = EstopState{}
			}
		}
		if state.EstopID == "" {
			snapshot, err := a.herdrSnapshot(ctx)
			if err != nil {
				return err
			}
			items, err := planEstopItems(snapshot, a.Config, a.HQRoot)
			if err != nil {
				return err
			}
			now := a.operationsNow().Format(time.RFC3339)
			state = EstopState{Version: estopStateVersion, EstopID: *id, Actor: actor.Name, ActivatedAt: now, UpdatedAt: now,
				State: "active", Reason: cleanReason, Items: items}
			if err := locked.Write(state); err != nil {
				return err
			}
		}
		if state.ActivationEventID == "" {
			event, err := a.appendEstopEvent(actor, &state, nil, "estop_activated", "active", state.Reason)
			if err != nil {
				return err
			}
			state.ActivationEventID, state.UpdatedAt = event.ID, a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(state); err != nil {
				return err
			}
		}
		record, err := replayEstopRecord(a.Store, a.Config, state.EstopID)
		if err != nil {
			return err
		}
		for i := range state.Items {
			item := &state.Items[i]
			if planned := record.Agents[item.Agent]; planned != nil && planned.Plan.ID != "" {
				probe := planned.Plan
				rule, _ := historicalRule(a.Config, item.Agent)
				probe.Recipient, probe.RecipientLabel = item.Agent, rule.Label
				probe.WorkspaceID, probe.TabID, probe.PaneID = item.WorkspaceID, item.TabID, item.PaneID
				probe.CWD, probe.AgentKind = item.CWD, item.Kind
				if !sameEstopAgentIdentity(planned.Plan, probe) {
					return fmt.Errorf("agent %s sentinel plan 与主账本冲突", item.Agent)
				}
				if item.PlanEventID == "" {
					item.PlanEventID = planned.Plan.ID
					state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
					if err := locked.Write(state); err != nil {
						return err
					}
				} else if item.PlanEventID != planned.Plan.ID {
					return fmt.Errorf("agent %s plan event id sentinel/账本冲突", item.Agent)
				}
				continue
			}
			event, err := a.appendEstopEvent(actor, &state, item, "estop_agent_planned", "planned", "expected frozen set member")
			if err != nil {
				return err
			}
			item.PlanEventID, state.UpdatedAt = event.ID, a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(state); err != nil {
				return err
			}
			record, err = replayEstopRecord(a.Store, a.Config, state.EstopID)
			if err != nil {
				return err
			}
		}
		if !record.planComplete() {
			return fmt.Errorf("ESTOP expected frozen set 未完整落入主账本")
		}
		for i := range state.Items {
			item := &state.Items[i]
			retryFailed := item.FreezeOutcome == "failed" && item.FreezeEventID != "" && item.FreezeRetryable
			if _, err := a.authorizeEstopLive(); err != nil {
				return err
			}
			if (item.FreezeOutcome == "confirmed" || item.FreezeOutcome == "failed") && item.FreezeEventID == "" {
				eventType := estopFreezeType(item.FreezeOutcome)
				result := map[string]string{"confirmed": "confirmed", "failed": "definitely-not-run"}[item.FreezeOutcome]
				event, err := a.appendEstopEvent(actor, &state, item, eventType, result, item.FreezeError)
				if err != nil {
					return err
				}
				item.FreezeEventID, state.UpdatedAt = event.ID, a.operationsNow().Format(time.RFC3339)
				if err := locked.Write(state); err != nil {
					return err
				}
			}
			if item.FreezeOutcome == "confirmed" {
				continue
			}
			if item.FreezeOutcome == "failed" {
				if !retryFailed {
					continue
				}
				event, err := a.appendEstopEvent(actor, &state, item, "estop_agent_freeze_retry", "retry-authorized", "explicit activate retry after definitely-not-run")
				if err != nil {
					return err
				}
				item.FreezeRetryEventID = event.ID
				item.FreezeOutcome, item.FreezeError, item.FreezeRetryable = "pending", "", false
				state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
				if err := locked.Write(state); err != nil {
					return err
				}
			}
			if item.FreezeOutcome == "attempting" || item.FreezeOutcome == "ambiguous" {
				priorOutcome := item.FreezeOutcome
				snapshot, snapErr := a.herdrSnapshot(ctx)
				if snapErr != nil {
					item.FreezeOutcome, item.FreezeError = "ambiguous", truncateError(snapErr)
				} else if !snapshotHasTab(snapshot, item.TabID) {
					item.FreezeOutcome, item.FreezeError = "confirmed", ""
				} else {
					item.FreezeOutcome = "ambiguous"
				}
				if item.FreezeOutcome != priorOutcome {
					item.FreezeEventID = ""
				}
			} else {
				item.FreezeOutcome, item.FreezeError = "attempting", ""
				state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
				if err := locked.Write(state); err != nil {
					return err
				}
				mutation := a.Herdr.CloseTab(ctx, item.TabID)
				snapshot, snapErr := a.herdrSnapshot(ctx)
				absent := snapErr == nil && !snapshotHasTab(snapshot, item.TabID)
				switch mutation.Outcome {
				case herdrConfirmed:
					if absent {
						item.FreezeOutcome = "confirmed"
					} else {
						item.FreezeOutcome, item.FreezeError = "ambiguous", "confirmed close 后 tab 仍存在或 snapshot 失败"
					}
				case herdrAmbiguous:
					if absent {
						item.FreezeOutcome = "confirmed"
					} else {
						item.FreezeOutcome, item.FreezeError = "ambiguous", truncateError(mutation.Err)
					}
				case herdrDefinitelyNotRun:
					item.FreezeOutcome, item.FreezeError = "failed", truncateError(mutation.Err)
					item.FreezeRetryable = true
				default:
					item.FreezeOutcome, item.FreezeError = "ambiguous", "unknown Herdr outcome"
				}
			}
			state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(state); err != nil {
				return err
			}
			if err := a.hitEstopFailpoint("after_freeze_state_before_event"); err != nil {
				return err
			}
			if item.FreezeOutcome != "attempting" && (item.FreezeEventID == "" || item.FreezeRetryEventID != "") {
				eventType := estopFreezeType(item.FreezeOutcome)
				result := map[string]string{"confirmed": "confirmed", "ambiguous": "ambiguous", "failed": "definitely-not-run"}[item.FreezeOutcome]
				event, err := a.appendEstopEvent(actor, &state, item, eventType, result, item.FreezeError)
				if err != nil {
					return err
				}
				item.FreezeEventID, item.FreezeRetryEventID = event.ID, ""
				state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
				if err := locked.Write(state); err != nil {
					return err
				}
			}
		}
		final = state
		return nil
	})
	if err != nil {
		return err
	}
	for _, item := range final.Items {
		if item.FreezeOutcome != "confirmed" {
			return fmt.Errorf("ESTOP active 但未全停：agent=%s outcome=%s；保留 recovery state", item.Agent, item.FreezeOutcome)
		}
	}
	return a.output(final, fmt.Sprintf("ESTOP %s active：子角色冻结=%d；注册表经理与销账职责位豁免；未自动关闭/拍板", final.EstopID, len(final.Items)))
}

func snapshotHasTab(snapshot HerdrSnapshot, tabID string) bool {
	for _, tab := range snapshot.Tabs {
		if tab.ID == tabID {
			return true
		}
	}
	return false
}

func findExactLiveAgent(snapshot HerdrSnapshot, workspaceID, name string) (HerdrAgent, bool) {
	var found []HerdrAgent
	for _, agent := range snapshot.Agents {
		if agent.WorkspaceID == workspaceID && agent.Name == name {
			found = append(found, agent)
		}
	}
	if len(found) != 1 {
		return HerdrAgent{}, false
	}
	return found[0], true
}

type estopRestoreAttempt struct {
	Outcome      string
	FailureClass string
	Error        string
	Live         HerdrAgent
}

func (a *App) stopRolledBackSessions(before []SessionEvent, agent, actor string) error {
	seen := map[string]bool{}
	for _, event := range before {
		seen[event.EventID] = true
	}
	after, err := a.Sessions.List(SessionFilter{Agent: agent})
	if err != nil {
		return err
	}
	latest := map[string]SessionEvent{}
	for _, event := range after {
		latest[event.SessionID] = event
	}
	for _, event := range after {
		if seen[event.EventID] || event.Type != "started" || latest[event.SessionID].Type == "stopped" {
			continue
		}
		stopped := event
		stopped.EventID = stableCommandID("estop-session-stop", event.SessionID)
		stopped.Type = "stopped"
		stopped.At = a.operationsNow().UTC().Format(time.RFC3339)
		stopped.Actor = actor
		stopped.Reason = "ESTOP restore attempt rolled back before safe retry"
		if err := a.Sessions.Append(stopped); err != nil {
			return err
		}
	}
	return nil
}

func hasNewEstopChildArtifacts(before, after HerdrSnapshot, workspaceID string, rule AgentRule, hqRoot string) bool {
	knownTabs := map[string]bool{}
	for _, tab := range before.Tabs {
		knownTabs[tab.ID] = true
	}
	wantCWD, err := resolveAgentWorkstation(hqRoot, rule)
	if err != nil {
		// A malformed current registry cannot prove that no child artifact was
		// created. Keep restore reconciliation in the ambiguous state.
		return true
	}
	for _, agent := range after.Agents {
		if agent.WorkspaceID == workspaceID && agent.Name == rule.Name {
			return true
		}
	}
	for _, tab := range after.Tabs {
		if !knownTabs[tab.ID] && tab.WorkspaceID == workspaceID && tab.Label == rosterTabLabel(rule) && filepath.Clean(tab.CWD) == wantCWD {
			return true
		}
	}
	return false
}

func (a *App) attemptEstopRestore(ctx context.Context, workspaceID string, rule AgentRule, actor Actor, before HerdrSnapshot) estopRestoreAttempt {
	beforeSessions, err := a.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil {
		return estopRestoreAttempt{Outcome: "ambiguous", Error: truncateError(err)}
	}
	child := *a
	child.Config, child.MaintenanceActor = a.Config, actor.Name
	// Release already owns the ESTOP store's exclusive lock. Calling the public
	// gated start path here would try to take a shared admission lease and
	// deadlock itself. This admitted variant is deliberately confined to the
	// exact restore path protected by that exclusive lease.
	startErr := child.startHQAgentAdmitted(ctx, workspaceID, rule)
	if startErr == nil {
		after, snapErr := a.herdrSnapshot(ctx)
		matched, _ := exactLiveMatch(after, workspaceID, rule, a.HQRoot)
		live, exists := findExactLiveAgent(after, workspaceID, rule.Name)
		if snapErr == nil && matched && exists {
			return estopRestoreAttempt{Outcome: "confirmed", Live: live}
		}
		return estopRestoreAttempt{Outcome: "ambiguous", Error: "start 后无法精确 snapshot reconcile"}
	}
	var partial *PartialStartError
	if errors.As(startErr, &partial) {
		if cleanupErr := a.closeOwnedTab(ctx, partial.TabID); cleanupErr != nil {
			return estopRestoreAttempt{Outcome: "ambiguous", Error: truncateError(combineLifecycleError(startErr, cleanupErr, partial.TabID))}
		}
	}
	after, snapErr := a.herdrSnapshot(ctx)
	if snapErr != nil || hasNewEstopChildArtifacts(before, after, workspaceID, rule, a.HQRoot) {
		return estopRestoreAttempt{Outcome: "ambiguous", Error: truncateError(startErr)}
	}
	if err := a.stopRolledBackSessions(beforeSessions, rule.Name, actor.Name); err != nil {
		return estopRestoreAttempt{Outcome: "ambiguous", Error: truncateError(fmt.Errorf("rollback session 记账未确认：%w", err))}
	}
	var notRun *DefinitelyNotRunStartError
	class := "confirmed-rollback"
	if errors.As(startErr, &notRun) {
		class = "definitely-not-run"
	}
	return estopRestoreAttempt{Outcome: "failed", FailureClass: class, Error: truncateError(startErr)}
}

func (a *App) verifyConfirmedRestoresLive(state EstopState, snapshot HerdrSnapshot) error {
	for _, item := range state.Items {
		if item.RestoreOutcome != "confirmed" {
			continue
		}
		rule, ok := a.Config.ruleFor(item.Agent)
		if !ok || rule.Disabled || a.Config.isManager(rule) || rule.hasResponsibility(roleAccountCloser) ||
			rule.Department != item.Department || rule.ReportsTo != item.ReportsTo ||
			(rule.Kind != "" && rule.Kind != item.Kind) {
			return fmt.Errorf("agent %s confirmed restore 不再符合 current config/plan，ESTOP 保持 active", item.Agent)
		}
		if matched, mismatch := exactLiveMatch(snapshot, item.WorkspaceID, rule, a.HQRoot); !matched {
			if mismatch == "" {
				mismatch = "missing"
			}
			return fmt.Errorf("agent %s confirmed restore 当前无精确 live：%s；ESTOP 保持 active", item.Agent, mismatch)
		}
	}
	return nil
}

func (a *App) cmdEstopRelease(args []string) error {
	fs := newLeafParser("estop release")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "稳定 estop id")
	reason := fs.String("reason", "", "显式解除原因")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq estop release --id ID --reason TEXT")
	}
	cleanReason, err := validateShortText("reason", *reason, true)
	if err != nil {
		return err
	}
	if a.Herdr == nil || a.Estop == nil || a.Sessions == nil {
		return fmt.Errorf("ESTOP release 必须注入 Herdr/state/session")
	}
	actor, err := a.authorizeEstopLive()
	if err != nil {
		return err
	}
	ctx := context.Background()
	if a.DryRun {
		state, exists, err := a.Estop.Read()
		if err != nil {
			return err
		}
		if !exists || state.EstopID != *id || state.State != "active" {
			return fmt.Errorf("无匹配 active estop 可 dry-run release")
		}
		return a.output(state, "DRY-RUN：已核验显式解除范围；未 create/start、未写 sentinel/账本")
	}
	var final EstopState
	err = a.Estop.WithLock(func(locked *lockedEstopStore) error {
		state := locked.state
		if state.EstopID == "" {
			return fmt.Errorf("无 estop state")
		}
		if state.EstopID != *id {
			return fmt.Errorf("estop id 不匹配：active=%s", state.EstopID)
		}
		if state.State == "released" {
			final = state
			return nil
		}
		preflightSnapshot, err := a.herdrSnapshot(ctx)
		if err != nil {
			return fmt.Errorf("ESTOP release 事实核对前 snapshot：%w", err)
		}
		if err := a.reconcileFreezeFactsBeforeRelease(actor, locked, &state, preflightSnapshot); err != nil {
			return err
		}
		// Historical restore outcomes are not current live evidence.  Refuse any
		// new restore mutation when a previously restored child has since drifted
		// or disappeared.
		if err := a.verifyConfirmedRestoresLive(state, preflightSnapshot); err != nil {
			return err
		}
		for i := range state.Items {
			item := &state.Items[i]
			if item.FreezeOutcome != "confirmed" {
				return fmt.Errorf("agent %s freeze=%s，不能解除", item.Agent, item.FreezeOutcome)
			}
			if item.RestoreOutcome == "confirmed" {
				continue
			}
			retryFailed := item.RestoreOutcome == "failed" && item.RestoreEventID != "" && item.RestoreRetryable
			if item.RestoreOutcome == "failed" {
				if !retryFailed {
					return fmt.Errorf("agent %s 上次恢复不可安全重试；ESTOP 保持 active", item.Agent)
				}
				event, err := a.appendEstopEvent(actor, &state, item, "estop_agent_restore_retry", "retry-authorized", "explicit release retry after proven no-live outcome")
				if err != nil {
					return err
				}
				if err := a.hitEstopFailpoint("after_restore_retry_event"); err != nil {
					return err
				}
				item.RestoreRetryEventID = event.ID
				item.RestoreOutcome, item.RestoreError, item.RestoreFailureClass, item.RestoreRetryable = "", "", "", false
				state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
				if err := locked.Write(state); err != nil {
					return err
				}
				if err := a.hitEstopFailpoint("after_restore_retry_state"); err != nil {
					return err
				}
			}
			liveActor, authErr := a.authorizeEstopLive()
			if authErr != nil {
				return authErr
			}
			actor = liveActor
			rule, ok := a.Config.ruleFor(item.Agent)
			if !ok || rule.Disabled || a.Config.isManager(rule) || rule.hasResponsibility(roleAccountCloser) || rule.Department != item.Department || (rule.Kind != "" && rule.Kind != item.Kind) {
				return fmt.Errorf("agent %s 不再符合 config/pattern，保留 active", item.Agent)
			}
			if _, err := validateWorkstation(a.HQRoot, rule); err != nil {
				return err
			}
			if item.RestoreOutcome == "ambiguous" && item.RestoreEventID != "" {
				return fmt.Errorf("agent %s restore ambiguous；禁止借 snapshot 盲目重启", item.Agent)
			}
			snapshot, err := a.herdrSnapshot(ctx)
			if err != nil {
				return err
			}
			if live, exists := findExactLiveAgent(snapshot, item.WorkspaceID, item.Agent); exists {
				priorOutcome := item.RestoreOutcome
				if matched, mismatch := exactLiveMatch(snapshot, item.WorkspaceID, rule, a.HQRoot); !matched {
					return fmt.Errorf("agent %s 已出现但不精确：%s", item.Agent, mismatch)
				}
				item.RestoreOutcome, item.RestoredWorkspace, item.RestoredTab, item.RestoredPane = "confirmed", live.WorkspaceID, live.TabID, live.PaneID
				if item.RestoreOutcome != priorOutcome {
					item.RestoreEventID = ""
				}
			} else if item.RestoreOutcome == "attempting" || item.RestoreOutcome == "ambiguous" {
				item.RestoreOutcome, item.RestoreError = "ambiguous", "既有恢复尝试无精确 live 结果；禁止盲目再次 create/start"
			} else {
				item.RestoreOutcome, item.RestoreError = "attempting", ""
				state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
				if err := locked.Write(state); err != nil {
					return err
				}
				attempt := a.attemptEstopRestore(ctx, item.WorkspaceID, rule, actor, snapshot)
				item.RestoreOutcome, item.RestoreError, item.RestoreFailureClass = attempt.Outcome, attempt.Error, attempt.FailureClass
				item.RestoreRetryable = attempt.Outcome == "failed"
				if attempt.Outcome == "confirmed" {
					item.RestoredWorkspace, item.RestoredTab, item.RestoredPane = attempt.Live.WorkspaceID, attempt.Live.TabID, attempt.Live.PaneID
				}
			}
			if item.RestoreEventID == "" || item.RestoreRetryEventID != "" {
				eventType := estopRestoreType(item.RestoreOutcome)
				result := map[string]string{"confirmed": "confirmed", "ambiguous": "ambiguous", "failed": item.RestoreFailureClass}[item.RestoreOutcome]
				event, err := a.appendEstopEvent(actor, &state, item, eventType, result, item.RestoreError)
				if err != nil {
					return err
				}
				item.RestoreEventID, item.RestoreRetryEventID = event.ID, ""
				if err := a.hitEstopFailpoint("after_restore_outcome_event"); err != nil {
					return err
				}
			}
			state.UpdatedAt = a.operationsNow().Format(time.RFC3339)
			if err := locked.Write(state); err != nil {
				return err
			}
			if item.RestoreOutcome != "confirmed" {
				return fmt.Errorf("agent %s 恢复=%s；ESTOP 保持 active", item.Agent, item.RestoreOutcome)
			}
		}
		// Take a fresh snapshot after the last restore and re-prove every
		// confirmed child before committing the aggregate release event.
		finalSnapshot, err := a.herdrSnapshot(ctx)
		if err != nil {
			return fmt.Errorf("ESTOP release 最终精确复核 snapshot：%w", err)
		}
		if err := a.verifyConfirmedRestoresLive(state, finalSnapshot); err != nil {
			return err
		}
		if state.ReleaseEventID == "" {
			event, err := a.appendEstopEvent(actor, &state, nil, "estop_released", "released", cleanReason)
			if err != nil {
				return err
			}
			state.ReleaseEventID = event.ID
		}
		state.State, state.UpdatedAt = "released", a.operationsNow().Format(time.RFC3339)
		if err := locked.Write(state); err != nil {
			return err
		}
		final = state
		return nil
	})
	if err != nil {
		return err
	}
	return a.output(final, fmt.Sprintf("ESTOP %s 已显式解除；确认恢复=%d", final.EstopID, len(final.Items)))
}

func (a *App) cmdEstopStatus(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("用法：hq estop status")
	}
	if a.Estop == nil {
		return fmt.Errorf("estop store 未注入")
	}
	state, exists, err := a.Estop.Read()
	if err != nil {
		return err
	}
	if !exists {
		return a.output(map[string]any{"version": estopStateVersion, "state": "inactive"}, "ESTOP inactive")
	}
	return a.output(state, fmt.Sprintf("ESTOP %s state=%s items=%d", state.EstopID, state.State, len(state.Items)))
}

func (s EstopState) confirmedFrozen() map[string]EstopItem {
	result := map[string]EstopItem{}
	if s.State != "active" {
		return result
	}
	for _, item := range s.Items {
		if item.FreezeOutcome == "confirmed" && item.RestoreOutcome != "confirmed" {
			result[item.Agent] = item
		}
	}
	return result
}
