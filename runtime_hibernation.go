package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const runtimeHibernationViewVersion = 1

type RuntimeSeatView struct {
	Version          int          `json:"version"`
	Agent            string       `json:"agent"`
	ActivationPolicy string       `json:"activation_policy"`
	KeepWarm         string       `json:"keep_warm"`
	RuntimeState     string       `json:"runtime_state"`
	Eligible         bool         `json:"eligible"`
	Blockers         []string     `json:"blockers,omitempty"`
	SessionID        string       `json:"session_id,omitempty"`
	WorkspaceID      string       `json:"workspace_id,omitempty"`
	TabID            string       `json:"tab_id,omitempty"`
	PaneID           string       `json:"pane_id,omitempty"`
	TerminalAt       string       `json:"terminal_at,omitempty"`
	LastOutcome      string       `json:"last_outcome,omitempty"`
	NextAction       string       `json:"next_action,omitempty"`
	StoppedSessions  []string     `json:"stopped_sessions,omitempty"`
	Binding          *LiveBinding `json:"binding,omitempty"`
}

type RuntimeReapReport struct {
	Version int               `json:"version"`
	At      string            `json:"at"`
	Seats   []RuntimeSeatView `json:"seats"`
}

type runtimeSeatAssessment struct {
	view    RuntimeSeatView
	rule    AgentRule
	started SessionEvent
	binding LiveBinding
}

type currentRegistryLeaseStore interface {
	lockCurrentRegistry(Config) (func(), error)
}

func (a *App) lockRuntimeCurrentRegistry() (func(), error) {
	store, ok := a.Store.(currentRegistryLeaseStore)
	if !ok {
		return nil, fmt.Errorf("Store 不支持 runtime close 前 current-registry lease")
	}
	return store.lockCurrentRegistry(a.Config)
}

func runtimeTabIDs(snapshot HerdrSnapshot) map[string]bool {
	result := make(map[string]bool, len(snapshot.Tabs))
	for _, tab := range snapshot.Tabs {
		result[tab.ID] = true
	}
	return result
}

func snapshotHasSessionRuntime(snapshot HerdrSnapshot, started SessionEvent) bool {
	for _, live := range snapshot.Agents {
		if live.Name == started.Agent && live.WorkspaceID == started.WorkspaceID && live.TabID == started.TabID && live.PaneID == started.PaneID {
			return true
		}
	}
	return false
}

func runtimeOrphanTabIDs(events []SessionEvent, snapshot HerdrSnapshot) []string {
	tabs := runtimeTabIDs(snapshot)
	seen := map[string]bool{}
	var result []string
	for _, started := range events {
		if started.Type != sessionStarted {
			continue
		}
		if tabs[started.TabID] && !snapshotHasSessionRuntime(snapshot, started) && !seen[started.TabID] {
			seen[started.TabID] = true
			result = append(result, started.TabID)
		}
	}
	sort.Strings(result)
	return result
}

func sessionMatchesBinding(started SessionEvent, binding LiveBinding) bool {
	if started.SessionID != binding.TabID || started.TabID != binding.TabID || started.PaneID != binding.PaneID ||
		started.WorkspaceID != binding.WorkspaceID || started.Agent != binding.Seat ||
		filepath.Clean(started.CWD) != filepath.Clean(binding.CWD) {
		return false
	}
	if started.TerminalID == "" && started.AgentSessionValue == "" {
		return false
	}
	if started.RuntimeKind != "" && started.RuntimeKind != binding.Kind {
		return false
	}
	if started.TerminalID != "" && binding.TerminalID != started.TerminalID {
		return false
	}
	if started.AgentSessionValue != "" {
		if binding.AgentSession == nil || binding.AgentSession.Source != started.AgentSessionSource ||
			binding.AgentSession.Agent != started.AgentSessionAgent || binding.AgentSession.Kind != started.AgentSessionKind ||
			binding.AgentSession.Value != started.AgentSessionValue {
			return false
		}
	}
	return binding.Revision >= started.Revision
}

func (a *App) appendDerivedSession(started SessionEvent, eventType, actor, reason string) (SessionEvent, error) {
	reason = strings.ReplaceAll(strings.ReplaceAll(strings.ReplaceAll(reason, "\r", " "), "\n", " "), "\x00", " ")
	runes := []rune(strings.TrimSpace(reason))
	if len(runes) > 180 {
		reason = string(runes[:180])
	} else {
		reason = string(runes)
	}
	if reason == "" {
		reason = "runtime lifecycle transition"
	}
	event, err := newDerivedSessionEvent(a.operationsNow(), eventType, started, actor, reason)
	if err != nil {
		return SessionEvent{}, err
	}
	if err := a.Sessions.Append(event); err != nil {
		return SessionEvent{}, err
	}
	return event, nil
}

func (a *App) appendHibernateDiagnostic(result *RuntimeSeatView, started SessionEvent, eventType, reason, successNext string) error {
	result.Eligible = false
	if _, err := a.appendDerivedSession(started, eventType, "hq-runtime-reaper", reason); err != nil {
		// The only durable diagnostic is still attempting. Never instruct an
		// operator to use --retry-failed when strict replay will (correctly)
		// require the more conservative unknown/incarnation verification path.
		result.LastOutcome = sessionHibernateAttempting
		result.NextAction = "session 诊断未落账，durable latest 仍为 hibernate_attempting；修复 session store，核验同一 incarnation/tab 后运行 hq --direct runtime reap --agent " + started.Agent + " --retry-unknown"
		return fmt.Errorf("%s 诊断落账失败；durable latest=hibernate_attempting：%w", eventType, err)
	}
	result.LastOutcome = eventType
	result.NextAction = successNext
	return nil
}

func assignmentTerminalTime(ledger *ledgerState, assignment *caseAssignment) (time.Time, bool) {
	if ledger == nil || assignment == nil || !assignment.Consumed {
		return time.Time{}, false
	}
	var terminal time.Time
	for _, event := range ledger.events {
		matchesAssignment := event.AssignmentEventID == assignment.EventID ||
			(assignment.AssignmentID != "" && event.AssignmentID == assignment.AssignmentID)
		if !matchesAssignment || (event.Type != "event_accepted" && event.Type != "event_returned") {
			continue
		}
		at, err := time.Parse(time.RFC3339, event.At)
		if err == nil && at.After(terminal) {
			terminal = at
		}
	}
	return terminal, !terminal.IsZero()
}

func (a *App) reconcileAbsentRuntimeSessions(events []SessionEvent, snapshot HerdrSnapshot, actor string) ([]string, error) {
	tabs := runtimeTabIDs(snapshot)
	var stopped []string
	for _, started := range activeSessionStarts(events) {
		if tabs[started.TabID] && snapshotHasSessionRuntime(snapshot, started) {
			continue
		}
		if _, err := a.appendDerivedSession(started, sessionStopped, actor, "runtime snapshot proves the recorded agent incarnation absent"); err != nil {
			return stopped, fmt.Errorf("收敛已消失 session=%s：%w", started.SessionID, err)
		}
		stopped = append(stopped, started.SessionID)
	}
	return stopped, nil
}

func appendRuntimeBlocker(view *RuntimeSeatView, format string, args ...any) {
	view.Blockers = append(view.Blockers, fmt.Sprintf(format, args...))
}

func deliveryMayRemainQueuedWhileSeatSleeps(record *deliveryRecord) bool {
	if record == nil || record.Origin.Type != "message_prepared" || messageNeedsAction(record.Origin.MessageKind) {
		return false
	}
	mode := effectiveEventDeliveryMode(record.Origin)
	if mode != deliveryModeQuiet && mode != deliveryModeInject {
		return false
	}
	return record.Status == deliveryQueued ||
		(record.Status == deliverySent && (record.ContextState == deliveryContextPending || record.ContextState == deliveryContextHistory))
}

func (a *App) assessRuntimeSeatLocked(rule AgentRule, reconcile bool, retryFailed, retryUnknown bool) (runtimeSeatAssessment, error) {
	keepWarm, keepWarmErr := effectiveSeatKeepWarm(rule)
	view := RuntimeSeatView{
		Version: runtimeHibernationViewVersion, Agent: rule.Name, ActivationPolicy: rule.ActivationPolicy,
		KeepWarm: keepWarm.String(), RuntimeState: "unknown",
	}
	assessment := runtimeSeatAssessment{view: view, rule: rule}
	if keepWarmErr != nil {
		return assessment, keepWarmErr
	}
	if rule.Disabled {
		assessment.view.RuntimeState = "disabled"
		appendRuntimeBlocker(&assessment.view, "seat_disabled")
		return assessment, nil
	}
	switch rule.ActivationPolicy {
	case activationAlways:
		appendRuntimeBlocker(&assessment.view, "policy_always_never_auto_stops")
	case activationManual:
		appendRuntimeBlocker(&assessment.view, "policy_manual_not_reapable")
	case activationOnAssignment:
	default:
		appendRuntimeBlocker(&assessment.view, "invalid_activation_policy:%s", rule.ActivationPolicy)
	}
	if a.Herdr == nil || a.Sessions == nil || a.Store == nil {
		return assessment, fmt.Errorf("runtime hibernation 必须注入 Herdr、session 与 ledger store")
	}
	snapshot, err := a.herdrSnapshot(a.requestContext())
	if err != nil {
		return assessment, fmt.Errorf("runtime reap snapshot：%w", err)
	}
	sessionEvents, err := a.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil {
		return assessment, fmt.Errorf("读取 agent session：%w", err)
	}
	orphanTabs := runtimeOrphanTabIDs(sessionEvents, snapshot)
	if reconcile {
		stopped, stopErr := a.reconcileAbsentRuntimeSessions(sessionEvents, snapshot, "hq-runtime-reaper")
		assessment.view.StoppedSessions = stopped
		if stopErr != nil {
			assessment.view.LastOutcome = "stopped_record_failed"
			assessment.view.NextAction = "tab 已消失；修复 session store 后重跑 hq --direct runtime reap --agent " + rule.Name
			return assessment, stopErr
		}
		if len(stopped) != 0 {
			sessionEvents, err = a.Sessions.List(SessionFilter{Agent: rule.Name})
			if err != nil {
				return assessment, err
			}
		}
	}

	var liveForSeat []HerdrAgent
	for _, live := range snapshot.Agents {
		if live.Name == rule.Name {
			liveForSeat = append(liveForSeat, live)
		}
	}
	if len(liveForSeat) == 0 {
		assessment.view.RuntimeState = "offline"
		assessment.view.LastOutcome = "already_stopped"
		if len(orphanTabs) != 0 {
			for _, tabID := range orphanTabs {
				appendRuntimeBlocker(&assessment.view, "orphan_tab_without_agent:%s", tabID)
			}
			assessment.view.NextAction = "agent runtime 已消失但空 tab 被保留；先运行 hq patrol 核验，再由运维者人工清理 orphan tab；下一条直属经理 durable hq issue 仍会复用 seat 并 cold-resume"
		} else {
			assessment.view.NextAction = "下一条直属经理 durable hq issue 会复用该 seat 并 cold-resume"
		}
		return assessment, nil
	}
	binding, err := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		assessment.view.RuntimeState = "drift"
		appendRuntimeBlocker(&assessment.view, "live_binding_drift:%s", err)
		assessment.view.NextAction = "修复精确 workspace/tab/pane/kind/cwd binding 后重跑 runtime status"
		return assessment, nil
	}
	assessment.binding = binding
	assessment.view.Binding = &assessment.binding
	assessment.view.RuntimeState = binding.Status
	assessment.view.WorkspaceID, assessment.view.TabID, assessment.view.PaneID = binding.WorkspaceID, binding.TabID, binding.PaneID
	if binding.Status == "working" || binding.Status == "blocked" {
		appendRuntimeBlocker(&assessment.view, "runtime_%s", binding.Status)
	}

	active := activeSessionStarts(sessionEvents)
	for _, started := range active {
		if sessionMatchesBinding(started, binding) {
			if assessment.started.SessionID != "" {
				appendRuntimeBlocker(&assessment.view, "multiple_sessions_match_live_incarnation")
				continue
			}
			assessment.started = started
			assessment.view.SessionID = started.SessionID
			continue
		}
		// An active historical incarnation whose tab still exists is not safe to
		// rewrite or close. It may be an ESTOP/operator orphan requiring review.
		if snapshotHasSessionRuntime(snapshot, started) {
			appendRuntimeBlocker(&assessment.view, "session_live_incarnation_mismatch:%s", started.SessionID)
		}
	}
	if assessment.started.SessionID == "" {
		appendRuntimeBlocker(&assessment.view, "missing_session_for_live_incarnation")
		assessment.view.NextAction = "不得按旧 session 关闭当前实例；先核验并修复 session/live binding"
		return assessment, nil
	}
	diagnostic := latestSessionDiagnostic(sessionEvents, assessment.started.SessionID)
	if diagnostic.Type != "" {
		assessment.view.LastOutcome = diagnostic.Type
		switch diagnostic.Type {
		case sessionHibernateFailed:
			if !retryFailed {
				appendRuntimeBlocker(&assessment.view, "hibernate_failed_requires_explicit_retry")
				assessment.view.NextAction = "确认同一 tab 仍在后运行 hq --direct runtime reap --agent " + rule.Name + " --retry-failed"
			}
		case sessionHibernateAttempting, sessionHibernateUnknown:
			if !retryUnknown {
				appendRuntimeBlocker(&assessment.view, "hibernate_unknown_requires_operator_verification")
				assessment.view.NextAction = "先用 runtime status 核验同一 session/tab；确认仍在后显式加 --retry-unknown，禁止自动重试"
			}
		case sessionFallbackAttempting, sessionFallbackUnknown:
			appendRuntimeBlocker(&assessment.view, "fallback_unknown_requires_operator_verification")
			assessment.view.NextAction = "先核验当前仍是同一 Codex session/tab；确认后由 can_manage_staff 角色运行 hq --direct runtime fallback --agent " + rule.Name + " --retry-unknown"
		case sessionFallbackFailed:
			appendRuntimeBlocker(&assessment.view, "fallback_failed_safe_to_retry")
			assessment.view.NextAction = "HQ 将在下一轮重新核验 terminal evidence；也可由 can_manage_staff 角色运行 hq --direct runtime fallback --agent " + rule.Name
		}
	}

	ledger, err := a.ledgerState()
	if err != nil {
		return assessment, fmt.Errorf("runtime reap 严格 replay ledger：%w", err)
	}
	var terminalAt time.Time
	var actionAckCommands, actionAckWaits, activationActions []string
	for _, assignment := range ledger.assignments {
		if assignment == nil || assignment.Recipient != rule.Name {
			continue
		}
		state := ledger.snapshot.Cases[assignment.CaseID]
		closed := state != nil && state.Status == string(statusClosed)
		if !assignment.Consumed && !closed {
			appendRuntimeBlocker(&assessment.view, "active_assignment:%s", assignment.AssignmentID)
			if assignment.Status == "issued" {
				for deliveryID, record := range ledger.deliveries {
					if record.Terminal.ID != assignment.EventID {
						continue
					}
					status := record.ActivationStatus
					if status == "" {
						status = "awaiting_accept"
					}
					appendRuntimeBlocker(&assessment.view, "assignment_activation_%s:%s", status, deliveryID)
					switch status {
					case activationUnknown:
						activationActions = append(activationActions, fmt.Sprintf("delivery=%s unknown：核对后运行 hq delivery resolve --id %s --outcome delivered|not-delivered --reason TEXT --evidence PATH", deliveryID, deliveryID))
					case activationExhausted:
						activationActions = append(activationActions, fmt.Sprintf("delivery=%s exhausted：核验终端后运行 hq delivery retry --id %s", deliveryID, deliveryID))
					default:
						activationActions = append(activationActions, fmt.Sprintf("delivery=%s：HQ 正在等待 accept，并仅会在安全 idle 终端上有界重投同一 assignment", deliveryID))
					}
					break
				}
			}
			continue
		}
		if assignment.Consumed {
			value, ok := assignmentTerminalTime(ledger, assignment)
			if !ok {
				appendRuntimeBlocker(&assessment.view, "missing_assignment_terminal_event:%s", assignment.AssignmentID)
			} else if value.After(terminalAt) {
				terminalAt = value
			}
		}
	}
	for deliveryID, record := range ledger.deliveries {
		if record == nil || (record.Origin.Actor != rule.Name && record.Origin.Recipient != rule.Name) {
			continue
		}
		if deliveryMayRemainQueuedWhileSeatSleeps(record) {
			continue
		}
		if record.Status != deliverySent {
			appendRuntimeBlocker(&assessment.view, "unresolved_delivery:%s:%s", deliveryID, record.Status)
			continue
		}
		// Transport sent/claimed is not proof that an actionable message was
		// understood. Keep both the sender (who may be awaiting a response) and
		// the recipient warm until the recipient records the durable message ack.
		if record.Origin.Type == "message_prepared" && messageNeedsAction(record.Origin.MessageKind) && record.Ack.ID == "" {
			appendRuntimeBlocker(&assessment.view, "unacked_action_message:%s:%s", deliveryID, record.Origin.MessageKind)
			messageID := record.Origin.MessageID
			if messageID == "" {
				messageID = record.Origin.ID
			}
			if record.Origin.Recipient == rule.Name {
				actionAckCommands = append(actionAckCommands, "hq message ack --message "+messageID)
			} else {
				actionAckWaits = append(actionAckWaits, fmt.Sprintf("message=%s recipient=%s", messageID, record.Origin.Recipient))
			}
			continue
		}
		if record.Ack.ID != "" {
			continue
		}
		if record.Origin.Recipient == rule.Name && (record.ContextState == deliveryContextPending || record.ContextState == deliveryContextHistory) {
			appendRuntimeBlocker(&assessment.view, "unread_delivery_context:%s:%s", deliveryID, record.ContextState)
		}
	}
	for nudgeID, record := range ledger.nudges {
		if record != nil && record.Origin.Recipient == rule.Name && nudgeStateActive(record.State) {
			appendRuntimeBlocker(&assessment.view, "unresolved_nudge:%s:%s", nudgeID, record.State)
		}
	}
	for reminderID, reminder := range ledger.reminders {
		if reminder != nil && reminder.Created.Recipient == rule.Name && reminder.Resolved.ID == "" {
			appendRuntimeBlocker(&assessment.view, "unresolved_reminder:%s", reminderID)
		}
	}
	for caseID, state := range ledger.snapshot.Cases {
		if state != nil && state.Owner == rule.Name && state.Status != string(statusClosed) {
			appendRuntimeBlocker(&assessment.view, "owned_open_case:%s:%s", caseID, state.Status)
		}
	}
	if assessment.view.NextAction == "" && len(activationActions) != 0 {
		sort.Strings(activationActions)
		assessment.view.NextAction = "assignment 尚未激活：" + strings.Join(activationActions, "；") + "；不要新建 assignment"
	} else if assessment.view.NextAction == "" && len(actionAckCommands) != 0 {
		sort.Strings(actionAckCommands)
		assessment.view.NextAction = "当前 seat 是行动型 message 接收方；逐条确认已读后执行：" + strings.Join(actionAckCommands, "；")
	} else if assessment.view.NextAction == "" && len(actionAckWaits) != 0 {
		sort.Strings(actionAckWaits)
		assessment.view.NextAction = "当前 seat 是行动型 message 发送方；等待接收方写入 durable ack：" + strings.Join(actionAckWaits, "；")
	}
	if terminalAt.IsZero() {
		appendRuntimeBlocker(&assessment.view, "no_terminal_assignment")
	} else {
		assessment.view.TerminalAt = terminalAt.UTC().Format(time.RFC3339)
		readyAt := terminalAt.Add(keepWarm)
		if a.operationsNow().Before(readyAt) {
			appendRuntimeBlocker(&assessment.view, "keep_warm_until:%s", readyAt.UTC().Format(time.RFC3339))
		}
	}
	sort.Strings(assessment.view.Blockers)
	assessment.view.Eligible = len(assessment.view.Blockers) == 0
	if assessment.view.Eligible {
		assessment.view.NextAction = "runtime 可安全休眠"
	}
	return assessment, nil
}

func (a *App) reapRuntimeSeat(agent string, retryFailed, retryUnknown bool) (RuntimeSeatView, error) {
	rule, ok := a.Config.exactRule(agent)
	if !ok {
		return RuntimeSeatView{}, fmt.Errorf("runtime seat 未登记或已停用：%s", agent)
	}
	releaseSeat, err := a.lockRuntimeSeat(rule.Name)
	if err != nil {
		return RuntimeSeatView{}, err
	}
	defer releaseSeat()
	var result RuntimeSeatView
	_, err = a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentHibernate, Target: rule.Name}, func() error {
		upLock, lockErr := a.lockUpContext(a.requestContext())
		if lockErr != nil {
			return lockErr
		}
		defer unlock(upLock)
		assessment, assessErr := a.assessRuntimeSeatLocked(rule, true, retryFailed, retryUnknown)
		result = assessment.view
		if assessErr != nil || !assessment.view.Eligible {
			return assessErr
		}
		result.Eligible = false
		attempt, appendErr := a.appendDerivedSession(assessment.started, sessionHibernateAttempting, "hq-runtime-reaper", "eligible runtime close attempt")
		if appendErr != nil {
			result.LastOutcome = "attempt_record_failed"
			result.NextAction = "未调用 CloseTab；修复 session store 后重跑 runtime reap"
			return fmt.Errorf("hibernate attempting 记账失败，尚未调用 CloseTab：%w", appendErr)
		}
		result.LastOutcome = attempt.Type
		// Re-run the complete eligibility projection after the durable attempting
		// fact. This catches fresh delivery origins, assignment/case changes and an
		// idle -> working/blocked transition before CloseTab.
		fresh, freshErr := a.assessRuntimeSeatLocked(rule, false, retryFailed, true)
		if freshErr != nil {
			if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateFailed,
				"definitely-not-run; pre-close assessment failed: "+truncateError(freshErr),
				"未调用 CloseTab；修复 assessment 依赖后运行 hq --direct runtime reap --agent "+rule.Name+" --retry-failed"); diagnosticErr != nil {
				return fmt.Errorf("hibernate close 前重新核验失败，且 %w", diagnosticErr)
			}
			return fmt.Errorf("hibernate close 前重新核验：%w", freshErr)
		}
		if !fresh.view.Eligible {
			result = fresh.view
			if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateDeferred,
				"pre-close eligibility changed: "+strings.Join(fresh.view.Blockers, ","),
				"未调用 CloseTab；资格已变化，等待下一轮重新评估"); diagnosticErr != nil {
				return fmt.Errorf("CloseTab 前资格已变化且 %w", diagnosticErr)
			}
			return nil
		}
		freshBinding := fresh.binding
		if mismatch := liveBindingIncarnationMismatch(assessment.binding, freshBinding); mismatch != "" || !sessionMatchesBinding(assessment.started, freshBinding) {
			reason := "pre-close incarnation changed"
			if mismatch != "" {
				reason += ": " + mismatch
			}
			if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateFailed, reason,
				"未调用 CloseTab；重新运行 runtime status 并修复 incarnation 后显式 --retry-failed"); diagnosticErr != nil {
				return fmt.Errorf("hibernate close 前 runtime incarnation 已变化，且 %w", diagnosticErr)
			}
			return fmt.Errorf("hibernate close 前 runtime incarnation 已变化；未调用 CloseTab")
		}
		// Hold the cross-process config-directory SH lease through CloseTab. A
		// concurrent staff update (for example on_assignment -> always or a new
		// seat/role incarnation) must either win before this point and make the
		// stale config comparison fail, or wait until the old incarnation is no
		// longer mutable.
		releaseRegistry, registryErr := a.lockRuntimeCurrentRegistry()
		if registryErr != nil {
			if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateDeferred,
				"registry changed before close: "+truncateError(registryErr),
				"未调用 CloseTab；重载 registry 后重新评估 activation/seat incarnation"); diagnosticErr != nil {
				return fmt.Errorf("registry 已变化且 %w", diagnosticErr)
			}
			return nil
		}
		defer releaseRegistry()
		lastSnapshot, lastSnapshotErr := a.herdrSnapshot(a.requestContext())
		if lastSnapshotErr != nil {
			if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateFailed,
				"definitely-not-run; final snapshot failed: "+truncateError(lastSnapshotErr),
				"未调用 CloseTab；修复 Herdr snapshot 后显式 --retry-failed"); diagnosticErr != nil {
				return fmt.Errorf("hibernate close 前最终 snapshot 失败，且 %w", diagnosticErr)
			}
			return fmt.Errorf("hibernate close 前最终 snapshot：%w", lastSnapshotErr)
		}
		lastBinding, lastBindingErr := ResolveLiveBinding(lastSnapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
		if lastBindingErr != nil || lastBinding.Status == "working" || lastBinding.Status == "blocked" || liveBindingIncarnationMismatch(freshBinding, lastBinding) != "" || !sessionMatchesBinding(assessment.started, lastBinding) {
			reason := "final live eligibility changed"
			if lastBindingErr != nil {
				reason += ": " + truncateError(lastBindingErr)
			} else {
				reason += ": status=" + lastBinding.Status
			}
			if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateDeferred, reason,
				"未调用 CloseTab；运行实例仍有工作或 incarnation 已变化，等待下一轮重新评估"); diagnosticErr != nil {
				return fmt.Errorf("最终资格已变化且 %w", diagnosticErr)
			}
			return nil
		}

		mutation := a.Herdr.CloseTab(a.requestContext(), assessment.started.TabID)
		after, afterErr := a.herdrSnapshot(a.requestContext())
		absent := afterErr == nil && !snapshotHasTab(after, assessment.started.TabID)
		if absent {
			if _, appendErr := a.appendDerivedSession(assessment.started, sessionStopped, "hq-runtime-reaper", "on_assignment runtime hibernated; tab absence confirmed"); appendErr != nil {
				result.LastOutcome = "stopped_record_failed"
				result.NextAction = "CloseTab 已生效；修复 session store 后重跑 runtime reap，只会补 stopped，不会重复关闭"
				return fmt.Errorf("runtime 已关闭但 stopped 记账失败：%w", appendErr)
			}
			result.RuntimeState, result.LastOutcome, result.Eligible = "offline", sessionStopped, false
			result.NextAction = "下一条直属经理 durable hq issue 会复用该 seat 并 cold-resume"
			return nil
		}
		reason := truncateError(mutation.Err)
		if afterErr != nil {
			reason = strings.TrimSpace(reason + "; snapshot=" + truncateError(afterErr))
		}
		if mutation.Outcome == herdrDefinitelyNotRun {
			if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateFailed, reason,
				"确认同一 tab 仍在后运行 hq --direct runtime reap --agent "+rule.Name+" --retry-failed"); diagnosticErr != nil {
				return fmt.Errorf("CloseTab definitely-not-run，且 %w", diagnosticErr)
			}
			return fmt.Errorf("runtime hibernate definitely-not-run；业务终态未改变")
		}
		if reason == "" {
			reason = "CloseTab outcome=" + string(mutation.Outcome) + " but tab absence was not proven"
		}
		if diagnosticErr := a.appendHibernateDiagnostic(&result, assessment.started, sessionHibernateUnknown, reason,
			"禁止自动重试；先核验同一 session/tab，确认仍在后显式使用 --retry-unknown"); diagnosticErr != nil {
			return fmt.Errorf("CloseTab 结果不确定，且 %w", diagnosticErr)
		}
		return fmt.Errorf("runtime hibernate 结果不确定；业务终态未改变，禁止自动重试")
	})
	return result, err
}

func (a *App) runtimeSeatStatus(agent string) (RuntimeSeatView, error) {
	rule, ok := configRuleIncludingDisabled(a.Config, agent)
	if !ok {
		return RuntimeSeatView{}, fmt.Errorf("runtime seat 未登记：%s", agent)
	}
	release, err := a.lockRuntimeSeat(rule.Name)
	if err != nil {
		return RuntimeSeatView{}, err
	}
	defer release()
	assessment, err := a.assessRuntimeSeatLocked(rule, false, false, false)
	return assessment.view, err
}

// ensureRuntimeSeatOriginSafeLocked is called while the caller owns the
// runtime-seat lease and before a new wakeup origin is written. It prevents an
// ambiguous, still-in-flight old CloseTab from being followed by a successful
// new Prompt that the delayed close could then kill.
func (a *App) ensureRuntimeSeatOriginSafeLocked(agent string) error {
	rule, ok := a.Config.exactRule(agent)
	if !ok || rule.ActivationPolicy != activationOnAssignment {
		return nil
	}
	if a.Herdr == nil || a.Sessions == nil {
		if a.ProductionRuntime {
			return fmt.Errorf("on_assignment origin admission 缺少 Herdr/session 依赖，拒绝 fail-open")
		}
		// Synthetic unit fixtures can explicitly omit the runtime plane. Real
		// production Apps always inject both dependencies above.
		return nil
	}
	snapshot, err := a.herdrSnapshot(a.requestContext())
	if err != nil {
		return fmt.Errorf("新工作写入前核验 runtime close fence：%w", err)
	}
	events, err := a.Sessions.List(SessionFilter{Agent: agent})
	if err != nil {
		return fmt.Errorf("新工作写入前读取 session close fence：%w", err)
	}
	if _, err := a.reconcileAbsentRuntimeSessions(events, snapshot, "hq-delivery-admission"); err != nil {
		return fmt.Errorf("新工作写入前收敛已消失 session：%w", err)
	}
	events, err = a.Sessions.List(SessionFilter{Agent: agent})
	if err != nil {
		return fmt.Errorf("新工作写入前重放 session close fence：%w", err)
	}
	for _, started := range activeSessionStarts(events) {
		diagnostic := latestSessionDiagnostic(events, started.SessionID)
		switch diagnostic.Type {
		case sessionHibernateAttempting, sessionHibernateUnknown:
			return conflictf("seat %s 的旧 runtime close 尚未收敛（session=%s latest=%s）；为避免延迟 CloseTab 杀掉新 Prompt，HQ 尚未写入新业务 origin/WIP。请由 can_manage_staff 运维角色先运行 hq --direct runtime status --agent %s；核验同一 incarnation/tab 仍在后运行 hq --direct runtime reap --agent %s --retry-unknown，再重试原命令", agent, started.SessionID, diagnostic.Type, agent, agent)
		case sessionFallbackAttempting, sessionFallbackUnknown:
			return conflictf("seat %s 的模型 fallback close 尚未收敛（session=%s latest=%s）；HQ 尚未写入新业务 origin/WIP。请由 can_manage_staff 运维角色先运行 hq --direct runtime status --agent %s 核验同一 Codex incarnation/tab；确认仍在后运行 hq --direct runtime fallback --agent %s --retry-unknown，再重试原命令", agent, started.SessionID, diagnostic.Type, agent, agent)
		case sessionHibernateFailed:
			// definitely-not-run is safe to supersede once real work arrives. A
			// deferred diagnostic clears the stale retry-failed requirement so the
			// next terminal cycle can be reaped normally.
			if _, err := a.appendDerivedSession(started, sessionHibernateDeferred, "hq-delivery-admission", "new durable work supersedes a definitely-not-run close attempt"); err != nil {
				return fmt.Errorf("新工作写入前清除 failed close fence：%w", err)
			}
		}
	}
	return nil
}

func (a *App) lockRuntimeSeatOriginFence(agent string) (func(), error) {
	release, err := a.lockRuntimeSeat(agent)
	if err != nil {
		return nil, err
	}
	if err := a.ensureRuntimeSeatOriginSafeLocked(agent); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

func (a *App) runtimeSeatNames(agent string) ([]string, error) {
	clean := strings.TrimSpace(agent)
	if clean != "" {
		if _, ok := configRuleIncludingDisabled(a.Config, clean); !ok {
			return nil, fmt.Errorf("runtime seat 未登记：%s", clean)
		}
		return []string{clean}, nil
	}
	var names []string
	for _, rule := range a.Config.Agents {
		if !rule.Disabled && rule.ActivationPolicy == activationOnAssignment {
			names = append(names, rule.Name)
		}
	}
	sort.Strings(names)
	return names, nil
}

func (a *App) reapRuntimeSeats(agent string, retryFailed, retryUnknown bool) (RuntimeReapReport, error) {
	report := RuntimeReapReport{Version: runtimeHibernationViewVersion, At: a.operationsNow().Format(time.RFC3339)}
	names, err := a.runtimeSeatNames(agent)
	if err != nil {
		report.Seats = append(report.Seats, RuntimeSeatView{Version: runtimeHibernationViewVersion, Agent: agent, RuntimeState: "error", NextAction: err.Error()})
		return report, err
	}
	var failures []string
	for _, name := range names {
		view, reapErr := a.reapRuntimeSeat(name, retryFailed, retryUnknown)
		if reapErr != nil {
			if view.Agent == "" {
				view.Agent = name
				view.Version = runtimeHibernationViewVersion
			}
			view.Blockers = append(view.Blockers, "reap_error:"+truncateError(reapErr))
			failures = append(failures, name+": "+reapErr.Error())
		}
		report.Seats = append(report.Seats, view)
	}
	if len(failures) != 0 {
		return report, fmt.Errorf("runtime reap 有 %d 个 seat 失败：%s", len(failures), strings.Join(failures, " | "))
	}
	return report, nil
}

func (a *App) cmdRuntime(args []string) error {
	if len(args) == 0 || (args[0] != "status" && args[0] != "reap" && args[0] != "fallback") {
		return fmt.Errorf("用法：hq --direct runtime status|reap|fallback [--agent NAME]")
	}
	if a.MaintenanceActor == "" {
		return permissionf("runtime lifecycle 仅允许已通过 can_manage_staff 运维白名单的 --direct 调用")
	}
	maintenanceRule, ok := a.Config.exactRule(a.MaintenanceActor)
	if !ok || !maintenanceRule.CanManageStaff {
		return permissionf("runtime lifecycle 调用者 %s 已不在 can_manage_staff 运维白名单；重载 registry 后重试", a.MaintenanceActor)
	}
	fs := newLeafParser("runtime " + args[0])
	fs.SetOutput(a.Err)
	agent := fs.String("agent", "", "精确 seat slug；默认全部 on_assignment seat")
	retryFailed := fs.Bool("retry-failed", false, "仅显式重试已证明 definitely-not-run 的关闭")
	retryUnknown := fs.Bool("retry-unknown", false, "人工核验同一 incarnation 仍在后显式重试 unknown")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq --direct runtime %s [--agent NAME] [--retry-failed] [--retry-unknown]", args[0])
	}
	if args[0] == "status" && (*retryFailed || *retryUnknown) {
		return fmt.Errorf("runtime status 不接受 retry flag")
	}
	if args[0] == "reap" && (*retryFailed || *retryUnknown) && strings.TrimSpace(*agent) == "" {
		return fmt.Errorf("--retry-failed/--retry-unknown 必须与单一 --agent NAME 一起使用，禁止批量重试 runtime close")
	}
	if args[0] == "fallback" {
		if strings.TrimSpace(*agent) == "" {
			return fmt.Errorf("runtime fallback 必须显式指定单一 --agent NAME，禁止批量替换模型载体")
		}
		if *retryFailed {
			return fmt.Errorf("runtime fallback 不接受 --retry-failed；首次或 definitely-not-run 重试直接移除该 flag，只有 fallback_attempting|fallback_unknown 需要 --retry-unknown")
		}
		if err := a.retryContentSafeguardFallback(a.requestContext(), *agent, *retryUnknown); err != nil {
			return err
		}
		_, err := fmt.Fprintf(a.Out, "runtime fallback 已收敛：agent=%s；运行 hq session list --agent %s 核验 old stopped、new started 与 fallback_recovery_sent\n", *agent, *agent)
		return err
	}
	if args[0] == "status" {
		names, err := a.runtimeSeatNames(*agent)
		if err != nil {
			return err
		}
		views := make([]RuntimeSeatView, 0, len(names))
		for _, name := range names {
			view, statusErr := a.runtimeSeatStatus(name)
			if statusErr != nil {
				return statusErr
			}
			views = append(views, view)
		}
		if a.JSON {
			encoder := json.NewEncoder(a.Out)
			encoder.SetIndent("", "  ")
			return encoder.Encode(views)
		}
		for _, view := range views {
			fmt.Fprintf(a.Out, "agent=%s policy=%s state=%s eligible=%t session=%s blockers=%s next=%s\n", view.Agent, view.ActivationPolicy, view.RuntimeState, view.Eligible, view.SessionID, strings.Join(view.Blockers, ","), view.NextAction)
		}
		return nil
	}
	report, reapErr := a.reapRuntimeSeats(*agent, *retryFailed, *retryUnknown)
	if a.JSON {
		encoder := json.NewEncoder(a.Out)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			return err
		}
		return reapErr
	}
	for _, view := range report.Seats {
		fmt.Fprintf(a.Out, "agent=%s state=%s outcome=%s eligible=%t blockers=%s next=%s\n", view.Agent, view.RuntimeState, view.LastOutcome, view.Eligible, strings.Join(view.Blockers, ","), view.NextAction)
	}
	return reapErr
}

func (a *App) runRuntimeReaperOnce(ctx context.Context) {
	if a.ConfigAccess != nil {
		a.ConfigAccess.RLock()
		defer a.ConfigAccess.RUnlock()
	}
	child := *a
	child.RequestContext = nonNilContext(ctx)
	if store, ok := a.Store.(*Store); ok {
		child.Store = store.withRequestContext(ctx)
	}
	if sessions, ok := a.Sessions.(*FileSessionStore); ok {
		child.Sessions = sessions.withRequestContext(ctx)
	}
	if a.ConfigPath != "" {
		cfg, err := loadConfig(a.ConfigPath)
		if err != nil {
			fmt.Fprintf(a.Err, "[HQ runtime reaper] reload config 失败：%v\n", err)
			return
		}
		child.Config = cfg
	}
	if fallbackErr := child.recoverContentSafeguardsOnce(ctx); fallbackErr != nil {
		fmt.Fprintf(a.Err, "[HQ runtime fallback] %v\n", fallbackErr)
	}
	if activationErr := child.recoverIssuedAssignmentActivationsOnce(ctx); activationErr != nil {
		fmt.Fprintf(a.Err, "[HQ assignment activation] %v\n", activationErr)
	}
	report, reapErr := child.reapRuntimeSeats("", false, false)
	for _, view := range report.Seats {
		for _, blocker := range view.Blockers {
			if strings.HasPrefix(blocker, "reap_error:") {
				fmt.Fprintf(a.Err, "[HQ runtime reaper] agent=%s %s next=%s\n", view.Agent, blocker, view.NextAction)
			}
		}
	}
	if reapErr != nil {
		fmt.Fprintf(a.Err, "[HQ runtime reaper] %v\n", reapErr)
	}
}
