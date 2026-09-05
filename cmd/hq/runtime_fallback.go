package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

const (
	contentSafeguardMarker  = "This content can't be shown"
	fallbackPendingReason   = "content_safeguard fallback_pending"
	maxRuntimeRecoveryItems = 8
)

type runtimeRecoveryWork struct {
	assignments           []AssignmentView
	supervisedAssignments []AssignmentView
	cases                 []*CaseState
	omitted               int
}

func terminalShowsContentSafeguard(raw []byte) bool {
	text := string(raw)
	index := strings.LastIndex(text, contentSafeguardMarker)
	if index < 0 {
		return false
	}
	tail := text[index:]
	return strings.Contains(tail, "We take extra caution with cybersecurity requests.") &&
		strings.Contains(tail, "Trusted Access:") && strings.Contains(tail, "Ask Codex to do anything")
}

func (a *App) runtimeRecoveryWorkFor(agent string) (runtimeRecoveryWork, error) {
	ledger, err := a.ledgerState()
	if err != nil {
		return runtimeRecoveryWork{}, err
	}
	work := runtimeRecoveryWork{}
	caseSeen := map[string]bool{}
	for _, view := range ledger.assignmentViews() {
		assignment := ledger.assignments[view.AssignmentEventID]
		if assignment == nil || assignment.Consumed {
			continue
		}
		if assignment.Recipient == agent &&
			(assignment.Status == "issued" || assignment.Status == "accepted" || assignment.Status == "rework") {
			work.assignments = append(work.assignments, view)
			caseSeen[view.CaseID] = true
			continue
		}
		// A manager may have no assignment of their own while a subordinate is
		// executing work that the manager must later review. That reviewer duty is
		// still durable work for runtime recovery: otherwise a provider safeguard
		// can strand the manager until (and even after) the subordinate reports.
		if (assignment.Reviewer == agent || assignment.Acceptor == agent) &&
			(assignment.Status == "issued" || assignment.Status == "accepted" || assignment.Status == "rework" || assignment.Status == "submitted") {
			work.supervisedAssignments = append(work.supervisedAssignments, view)
			caseSeen[view.CaseID] = true
		}
	}
	for _, state := range ledger.snapshot.Cases {
		// Ownership alone does not make historical or externally blocked work
		// actionable. Match the manager queue contract: only an unassigned open
		// case may enter a runtime recovery manifest.
		if state == nil || state.Owner != agent || state.Status != string(statusOpen) || caseSeen[state.ID] {
			continue
		}
		work.cases = append(work.cases, state)
	}
	sort.Slice(work.assignments, func(i, j int) bool { return work.assignments[i].AssignmentID < work.assignments[j].AssignmentID })
	sort.Slice(work.supervisedAssignments, func(i, j int) bool {
		leftSubmitted := work.supervisedAssignments[i].Status == "submitted"
		rightSubmitted := work.supervisedAssignments[j].Status == "submitted"
		if leftSubmitted != rightSubmitted {
			return leftSubmitted
		}
		return work.supervisedAssignments[i].AssignmentID < work.supervisedAssignments[j].AssignmentID
	})
	sort.Slice(work.cases, func(i, j int) bool { return work.cases[i].ID < work.cases[j].ID })
	remaining := maxRuntimeRecoveryItems
	if len(work.assignments) >= remaining {
		work.omitted = len(work.assignments) - remaining + len(work.supervisedAssignments) + len(work.cases)
		work.assignments = work.assignments[:maxRuntimeRecoveryItems]
		work.supervisedAssignments = nil
		work.cases = nil
		return work, nil
	}
	remaining -= len(work.assignments)
	if len(work.supervisedAssignments) >= remaining {
		work.omitted = len(work.supervisedAssignments) - remaining + len(work.cases)
		work.supervisedAssignments = work.supervisedAssignments[:remaining]
		work.cases = nil
		return work, nil
	}
	remaining -= len(work.supervisedAssignments)
	if len(work.cases) > remaining {
		work.omitted = len(work.cases) - remaining
		work.cases = work.cases[:remaining]
	}
	return work, nil
}

func (work runtimeRecoveryWork) empty() bool {
	return len(work.assignments) == 0 && len(work.supervisedAssignments) == 0 && len(work.cases) == 0
}

func supervisedAssignmentRecoveryLine(assignment AssignmentView) string {
	action := "下属仍持有执行权；不要接管或重复委派。核验 durable 状态后立即结束本回合，禁止 sleep、进程/Herdr 状态或产物轮询；等待 HQ 在正式 report 或异常升级时重新唤醒，再按冻结验收合同 review。"
	if assignment.Status == "submitted" {
		action = "已有正式 submission 待审；核验 durable 状态与产物后，按冻结验收合同显式 accept 或 return。"
	}
	return fmt.Sprintf("SUPERVISED_ASSIGNMENT id=%s event=%s case=%s status=%s assignee=%s reviewer=%s；先运行 `hq assignment show --id %s` 与 `hq history --case %s`。%s",
		assignment.AssignmentID, assignment.AssignmentEventID, assignment.CaseID, assignment.Status, assignment.Assignee, assignment.Reviewer,
		assignment.AssignmentID, assignment.CaseID, action)
}

func runtimeRecoveryEnvelope(rule AgentRule, policy RuntimeFallbackPolicy, work runtimeRecoveryWork) string {
	var lines []string
	lines = append(lines,
		runtimeProtocolLine(),
		fmt.Sprintf("[HQ runtime recovery] trigger=content_safeguard previous_kind=%s current_kind=%s seat=%s。你仍是同一员工；模型载体变化不改变角色、权限边界、汇报线或任务合同。", policy.FromKind, policy.ToKind, rule.Name),
		"本恢复信封替代上方‘等待首个 case’指令。隐藏聊天记录不会跨模型复制；只以当前 AGENTS.md、同一工位文件和 HQ durable ledger 为事实源，不得凭空补写旧会话结论。",
	)
	for _, assignment := range work.assignments {
		lines = append(lines, fmt.Sprintf("ACTIVE_ASSIGNMENT id=%s event=%s case=%s status=%s issuer=%s reviewer=%s；先运行 `hq assignment show --id %s` 与 `hq history --case %s`；若 status=issued，再运行 `hq accept --event %s`，否则从 durable 状态继续。",
			assignment.AssignmentID, assignment.AssignmentEventID, assignment.CaseID, assignment.Status, assignment.Issuer, assignment.Reviewer,
			assignment.AssignmentID, assignment.CaseID, assignment.AssignmentEventID))
	}
	for _, assignment := range work.supervisedAssignments {
		lines = append(lines, supervisedAssignmentRecoveryLine(assignment))
	}
	if isManagerResponsibilities(rule.Responsibilities) {
		lines = append(lines, managerEventDrivenWaitRule)
	}
	for _, state := range work.cases {
		lines = append(lines, fmt.Sprintf("ACTIVE_OWNED_CASE id=%s version=%d status=%s title=%q；先运行 `hq case show --id %s` 与 `hq history --case %s`，再从 durable 状态继续。", state.ID, state.Version, state.Status, state.Title, state.ID, state.ID))
	}
	if work.omitted > 0 {
		lines = append(lines, fmt.Sprintf("另有 %d 项 actionable durable work 未在本信封展开；运行 `hq inbox` 与 `hq assignment list` 读取完整队列，按 durable 状态处理，不要猜测。", work.omitted))
	}
	lines = append(lines, "先完成上述核验，再继续未完成工作；不要创建替代 case 或重复 assignment。")
	return strings.Join(lines, "\n")
}

func fallbackPending(events []SessionEvent, policy RuntimeFallbackPolicy) bool {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type == sessionStarted {
			return false
		}
		if event.Type == sessionStopped {
			return strings.HasPrefix(event.Reason, fallbackPendingReason+" to="+policy.ToKind)
		}
	}
	return false
}

func unresolvedFallbackSession(events []SessionEvent, policy RuntimeFallbackPolicy) (SessionEvent, bool) {
	active := activeSessionStarts(events)
	for index := len(active) - 1; index >= 0; index-- {
		started := active[index]
		if started.RuntimeKind != "" && started.RuntimeKind != policy.FromKind {
			continue
		}
		diagnostic := latestSessionDiagnostic(events, started.SessionID)
		if diagnostic.Type == sessionFallbackAttempting || diagnostic.Type == sessionFallbackUnknown {
			return started, true
		}
	}
	return SessionEvent{}, false
}

func activeSessionForBinding(events []SessionEvent, binding LiveBinding) (SessionEvent, error) {
	var matched SessionEvent
	for _, candidate := range activeSessionStarts(events) {
		if !sessionMatchesBinding(candidate, binding) {
			continue
		}
		if matched.SessionID != "" {
			return SessionEvent{}, fmt.Errorf("多个 active session 匹配同一 runtime：%s", binding.Seat)
		}
		matched = candidate
	}
	if matched.SessionID == "" {
		return SessionEvent{}, fmt.Errorf("live runtime 缺少精确 session 记账：%s", binding.Seat)
	}
	return matched, nil
}

func (a *App) markFallbackRecoverySent(ctx context.Context, rule AgentRule) error {
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	binding, err := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		return err
	}
	events, err := a.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil {
		return err
	}
	started, err := activeSessionForBinding(events, binding)
	if err != nil {
		return err
	}
	if _, err := a.appendDerivedSession(started, sessionFallbackRecoverySent, "hq-runtime-fallback", "durable recovery envelope delivery confirmed"); err != nil {
		return fmt.Errorf("fallback 已启动且恢复信封已投递，但 recovery_sent 记账失败；HQ 将安全地重复恢复信封：%w", err)
	}
	return nil
}

func (a *App) ensureFallbackRecovery(ctx context.Context, rule AgentRule, policy RuntimeFallbackPolicy, work runtimeRecoveryWork, binding LiveBinding, events []SessionEvent) error {
	started, err := activeSessionForBinding(events, binding)
	if err != nil {
		return err
	}
	if latestSessionDiagnostic(events, started.SessionID).Type == sessionFallbackRecoverySent {
		return nil
	}
	prompt := runtimeRecoveryEnvelope(rule, policy, work)
	result := a.Herdr.Prompt(ctx, rule.Name, prompt)
	if result.Err != nil {
		return fmt.Errorf("Grok 已在同一 seat 到岗，但恢复信封尚未确认投递；下一轮会安全重试：%w", result.Err)
	}
	if _, err := a.appendDerivedSession(started, sessionFallbackRecoverySent, "hq-runtime-fallback", "durable recovery envelope delivery confirmed on retry"); err != nil {
		return fmt.Errorf("恢复信封已确认投递但 recovery_sent 记账失败；下一轮会幂等重发：%w", err)
	}
	return nil
}

func (a *App) startFallbackRuntime(ctx context.Context, workspaceID string, rule AgentRule, policy RuntimeFallbackPolicy, work runtimeRecoveryWork) error {
	if err := a.startHQAgentAdmittedWithOptions(ctx, workspaceID, rule, runtimeStartOptions{
		Kind: policy.ToKind, PermissionMode: policy.PermissionMode, AgentArgs: policy.AgentArgs,
		Actor: "hq-runtime-fallback", Reason: "content_safeguard fallback resumed durable work",
		PromptSuffix: runtimeRecoveryEnvelope(rule, policy, work),
	}); err != nil {
		return err
	}
	return a.markFallbackRecoverySent(ctx, rule)
}

func (a *App) recoverRuntimeSeatFromSafeguard(ctx context.Context, workspaceID string, rule AgentRule, policy RuntimeFallbackPolicy, reader HerdrAgentReader, retryUnknown, requireAction bool) error {
	// A healthy primary runtime needs no recovery manifest. Avoid replaying
	// the entire company ledger for each healthy/never-started employee.
	if !requireAction {
		snapshot, err := a.herdrSnapshot(ctx)
		if err != nil {
			return err
		}
		var matches []HerdrAgent
		for _, live := range snapshot.Agents {
			if live.Name == rule.Name {
				matches = append(matches, live)
			}
		}
		if len(matches) == 1 && matches[0].Kind == policy.FromKind {
			raw, err := reader.ReadAgent(ctx, rule.Name)
			if err != nil {
				return err
			}
			if !terminalShowsContentSafeguard(raw) {
				return nil
			}
		}
		if len(matches) == 0 {
			events, err := a.Sessions.List(SessionFilter{Agent: rule.Name})
			if err != nil {
				return err
			}
			_, unresolved := unresolvedFallbackSession(events, policy)
			if !unresolved && !fallbackPending(events, policy) {
				return nil
			}
		}
	}
	releaseSeat, err := a.lockRuntimeSeat(rule.Name)
	if err != nil {
		return err
	}
	defer releaseSeat()
	_, err = a.withRuntimeAdmissions([]RuntimeAdmissionRequest{
		{Action: runtimeAdmissionAgentHibernate, Target: rule.Name},
		{Action: runtimeAdmissionAgentStart, Target: rule.Name},
	}, func() error {
		upLock, lockErr := a.lockUpContext(ctx)
		if lockErr != nil {
			return lockErr
		}
		defer unlock(upLock)

		work, workErr := a.runtimeRecoveryWorkFor(rule.Name)
		if workErr != nil {
			return fmt.Errorf("读取 durable recovery work：%w", workErr)
		}
		if work.empty() {
			if requireAction {
				return fmt.Errorf("seat %s 没有未完成 assignment 或其拥有的未关闭 case；无需切换模型载体", rule.Name)
			}
			return nil
		}
		events, eventsErr := a.Sessions.List(SessionFilter{Agent: rule.Name})
		if eventsErr != nil {
			return eventsErr
		}
		snapshot, snapshotErr := a.herdrSnapshot(ctx)
		if snapshotErr != nil {
			return snapshotErr
		}
		var live *HerdrAgent
		for index := range snapshot.Agents {
			if snapshot.Agents[index].Name == rule.Name {
				if live != nil {
					return fmt.Errorf("同名 live agent 不唯一，拒绝 fallback：%s", rule.Name)
				}
				live = &snapshot.Agents[index]
			}
		}
		if live == nil {
			if unresolved, ok := unresolvedFallbackSession(events, policy); ok {
				if _, appendErr := a.appendDerivedSession(unresolved, sessionStopped, "hq-runtime-fallback", fallbackPendingReason+" to="+policy.ToKind+"; later snapshot confirmed old tab absent"); appendErr != nil {
					return fmt.Errorf("snapshot 已证明旧 tab 消失，但 fallback pending 未落账：%w", appendErr)
				}
				return a.startFallbackRuntime(ctx, workspaceID, rule, policy, work)
			}
			if fallbackPending(events, policy) {
				return a.startFallbackRuntime(ctx, workspaceID, rule, policy, work)
			}
			if requireAction {
				return fmt.Errorf("seat %s 当前没有 live runtime，也没有已确认的 fallback_pending/未决 fallback session；拒绝凭空启动备用模型", rule.Name)
			}
			return nil
		}
		if live.Kind == policy.ToKind {
			binding, bindingErr := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
			if bindingErr != nil {
				return bindingErr
			}
			return a.ensureFallbackRecovery(ctx, rule, policy, work, binding, events)
		}
		if live.Kind != policy.FromKind || (live.Status != "idle" && live.Status != "blocked") {
			if requireAction {
				return fmt.Errorf("seat %s 当前 runtime kind=%s status=%s；只有 %s 的 idle|blocked session 才能依据 content_safeguard 切换", rule.Name, live.Kind, live.Status, policy.FromKind)
			}
			return nil
		}
		binding, bindingErr := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
		if bindingErr != nil {
			return bindingErr
		}
		started, startedErr := activeSessionForBinding(events, binding)
		if startedErr != nil {
			return fmt.Errorf("拒绝 fallback：%w", startedErr)
		}
		diagnostic := latestSessionDiagnostic(events, started.SessionID)
		if (diagnostic.Type == sessionFallbackAttempting || diagnostic.Type == sessionFallbackUnknown) && !retryUnknown {
			return fmt.Errorf("fallback close 尚未收敛：agent=%s session=%s latest=%s；禁止自动重试", rule.Name, started.SessionID, diagnostic.Type)
		}
		raw, readErr := reader.ReadAgent(ctx, rule.Name)
		if readErr != nil {
			return readErr
		}
		if !terminalShowsContentSafeguard(raw) {
			if requireAction {
				return fmt.Errorf("seat %s 的当前终端没有完整 content_safeguard 证据；未关闭 Codex，也未启动 Grok", rule.Name)
			}
			return nil
		}
		if _, appendErr := a.appendDerivedSession(started, sessionFallbackAttempting, "hq-runtime-fallback", "content_safeguard terminal evidence confirmed"); appendErr != nil {
			return fmt.Errorf("fallback attempting 未落账，尚未关闭旧 tab：%w", appendErr)
		}

		fresh, freshErr := a.herdrSnapshot(ctx)
		if freshErr != nil {
			_, _ = a.appendDerivedSession(started, sessionFallbackFailed, "hq-runtime-fallback", "definitely-not-run; final snapshot failed")
			return freshErr
		}
		freshBinding, freshBindingErr := ResolveLiveBinding(fresh, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
		if freshBindingErr != nil || liveBindingIncarnationMismatch(binding, freshBinding) != "" || !sessionMatchesBinding(started, freshBinding) {
			_, _ = a.appendDerivedSession(started, sessionFallbackFailed, "hq-runtime-fallback", "definitely-not-run; final binding changed")
			return fmt.Errorf("fallback close 前 runtime incarnation 已变化；未调用 CloseTab")
		}
		freshRaw, freshReadErr := reader.ReadAgent(ctx, rule.Name)
		if freshReadErr != nil {
			_, _ = a.appendDerivedSession(started, sessionFallbackFailed, "hq-runtime-fallback", "definitely-not-run; safeguard evidence changed")
			return freshReadErr
		}
		if !terminalShowsContentSafeguard(freshRaw) {
			_, _ = a.appendDerivedSession(started, sessionFallbackFailed, "hq-runtime-fallback", "definitely-not-run; safeguard evidence changed")
			return fmt.Errorf("fallback 关闭前 content_safeguard 证据已变化；未调用 CloseTab，请重新读取该 seat 后再决定是否重试")
		}

		releaseRegistry, registryErr := a.lockRuntimeCurrentRegistry()
		if registryErr != nil {
			_, _ = a.appendDerivedSession(started, sessionFallbackFailed, "hq-runtime-fallback", "definitely-not-run; registry changed")
			return registryErr
		}
		defer releaseRegistry()
		mutation := a.Herdr.CloseTab(ctx, started.TabID)
		after, afterErr := a.herdrSnapshot(ctx)
		if afterErr == nil && !snapshotHasTab(after, started.TabID) {
			if _, appendErr := a.appendDerivedSession(started, sessionStopped, "hq-runtime-fallback", fallbackPendingReason+" to="+policy.ToKind+"; old tab absence confirmed"); appendErr != nil {
				return fmt.Errorf("旧 tab 已关闭但 fallback pending 未落账：%w", appendErr)
			}
			return a.startFallbackRuntime(ctx, workspaceID, rule, policy, work)
		}
		reason := truncateError(mutation.Err)
		if mutation.Outcome == herdrDefinitelyNotRun {
			_, _ = a.appendDerivedSession(started, sessionFallbackFailed, "hq-runtime-fallback", "definitely-not-run; "+reason)
			return fmt.Errorf("fallback CloseTab definitely-not-run；旧 Codex session 保持在岗")
		}
		_, _ = a.appendDerivedSession(started, sessionFallbackUnknown, "hq-runtime-fallback", "CloseTab outcome unknown; "+reason)
		return fmt.Errorf("fallback CloseTab 结果不确定；禁止自动启动 Grok 以免双占 seat")
	})
	return err
}

func (a *App) retryContentSafeguardFallback(ctx context.Context, agent string, retryUnknown bool) error {
	policy := a.Config.RuntimeFallback
	if policy == nil || policy.Trigger != "content_safeguard" {
		return fmt.Errorf("runtime_fallback 未配置 content_safeguard；不能猜测备用模型或权限")
	}
	rule, ok := a.Config.exactRule(strings.TrimSpace(agent))
	if !ok {
		return fmt.Errorf("fallback seat 未登记或已停用：%s；运行 `hq staff list --all` 查看精确 seat slug", agent)
	}
	if rule.Kind != policy.FromKind {
		return fmt.Errorf("seat %s primary kind=%s，不匹配 runtime_fallback.from_kind=%s；未执行任何关闭或启动", rule.Name, rule.Kind, policy.FromKind)
	}
	reader, ok := a.Herdr.(HerdrAgentReader)
	if !ok {
		return fmt.Errorf("Herdr control 不支持 agent terminal read；无法核验 content_safeguard，未执行任何关闭或启动")
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	workspaceID := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			if workspaceID != "" {
				return fmt.Errorf("runtime fallback workspace label 不唯一：%s", a.Config.WorkspaceLabel)
			}
			workspaceID = workspace.ID
		}
	}
	if workspaceID == "" {
		return fmt.Errorf("runtime fallback 找不到 workspace：%s", a.Config.WorkspaceLabel)
	}
	return a.recoverRuntimeSeatFromSafeguard(ctx, workspaceID, rule, *policy, reader, retryUnknown, true)
}

func (a *App) recoverContentSafeguardsOnce(ctx context.Context) error {
	policy := a.Config.RuntimeFallback
	if policy == nil || !policy.Auto || policy.Trigger != "content_safeguard" {
		return nil
	}
	reader, ok := a.Herdr.(HerdrAgentReader)
	if !ok {
		return fmt.Errorf("runtime_fallback.auto=true，但 Herdr control 不支持 agent terminal read；未切换任何 session")
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	workspaceID := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			if workspaceID != "" {
				return fmt.Errorf("runtime fallback workspace label 不唯一：%s", a.Config.WorkspaceLabel)
			}
			workspaceID = workspace.ID
		}
	}
	if workspaceID == "" {
		return fmt.Errorf("runtime fallback 找不到 workspace：%s", a.Config.WorkspaceLabel)
	}
	var failures []string
	for _, rule := range a.Config.Agents {
		if rule.Disabled || rule.Kind != policy.FromKind {
			continue
		}
		if err := a.recoverRuntimeSeatFromSafeguard(ctx, workspaceID, rule, *policy, reader, false, false); err != nil {
			failures = append(failures, rule.Name+": "+err.Error())
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("runtime fallback 有 %d 个 seat 未收敛：%s", len(failures), strings.Join(failures, " | "))
	}
	return nil
}
