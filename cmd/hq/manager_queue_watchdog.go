package main

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
)

const managerQueueNudgeTTL = 15 * time.Minute

type ManagerQueueItem struct {
	Kind            string `json:"kind"`
	Manager         string `json:"manager"`
	AssignmentID    string `json:"assignment_id,omitempty"`
	CaseID          string `json:"case_id"`
	Status          string `json:"status"`
	ActionEventID   string `json:"action_event_id,omitempty"`
	StatusEventID   string `json:"status_event_id"`
	StatusUpdatedAt string `json:"status_updated_at"`
	BasisEventID    string `json:"basis_event_id"`
	BasisUpdatedAt  string `json:"basis_updated_at"`
}

type ManagerQueueBacklog struct {
	Manager      string             `json:"manager"`
	BasisEventID string             `json:"basis_event_id"`
	SelectedAt   string             `json:"selected_at"`
	Items        []ManagerQueueItem `json:"items"`
}

type ManagerSupervisionItem struct {
	AssignmentID  string `json:"assignment_id"`
	CaseID        string `json:"case_id"`
	Assignee      string `json:"assignee"`
	Status        string `json:"status"`
	StatusEventID string `json:"status_event_id"`
}

type ManagerParkingState struct {
	Manager      string                   `json:"manager"`
	BasisEventID string                   `json:"basis_event_id"`
	SelectedAt   string                   `json:"selected_at"`
	Items        []ManagerSupervisionItem `json:"items"`
}

func managerQueueItemLess(left, right ManagerQueueItem) bool {
	leftPriority := managerQueueKindPriority(left.Kind)
	rightPriority := managerQueueKindPriority(right.Kind)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}
	leftSelectedAt := managerQueueItemSelectedAt(left)
	rightSelectedAt := managerQueueItemSelectedAt(right)
	if leftSelectedAt == rightSelectedAt {
		if left.CaseID == right.CaseID {
			return left.AssignmentID < right.AssignmentID
		}
		return left.CaseID < right.CaseID
	}
	return leftSelectedAt < rightSelectedAt
}

func managerQueueItemBasis(item ManagerQueueItem) (string, string) {
	if item.BasisEventID != "" && item.BasisUpdatedAt != "" {
		return item.BasisEventID, item.BasisUpdatedAt
	}
	return item.StatusEventID, item.StatusUpdatedAt
}

func managerQueueItemSelectedAt(item ManagerQueueItem) string {
	_, selectedAt := managerQueueItemBasis(item)
	return selectedAt
}

func managerQueueKindPriority(kind string) int {
	switch kind {
	case "review":
		return 0
	case "work":
		return 1
	case "owned_case":
		return 2
	default:
		return 3
	}
}

// isStrictCaseDescendant keeps queue supervision scoped to the manager's
// current assignment tree. A sibling or another root can never refresh this
// assignment's stall basis.
func (s *ledgerState) isStrictCaseDescendant(caseID, ancestorID string) bool {
	if caseID == "" || ancestorID == "" || caseID == ancestorID {
		return false
	}
	seen := map[string]bool{}
	current := s.snapshot.Cases[caseID]
	for current != nil && current.ParentCaseID != "" && !seen[current.ID] {
		seen[current.ID] = true
		if current.ParentCaseID == ancestorID {
			return true
		}
		current = s.snapshot.Cases[current.ParentCaseID]
	}
	return false
}

type managerDelegationProgress struct {
	Covered bool
	Latest  Event
}

// managerDelegationProgressFor separates a manager's own execution queue from
// work they have durably delegated. An unconsumed descendant assignment issued
// by this manager is driven by the activation/progress watchdog and later
// becomes this manager's review item; nudging the parent assignment in parallel
// would demand a report before the delegated evidence exists. An open
// descendant still owned by the manager is covered by the owned_case item.
//
// Once those descendants converge, their latest business transition becomes
// the parent's stable queue basis. This gives the manager a fresh stall window
// to synthesize the result without allowing messages or infrastructure events
// to manufacture progress.
func (s *ledgerState) managerDelegationProgressFor(manager, parentCaseID string) managerDelegationProgress {
	relevantCases := map[string]bool{}
	progress := managerDelegationProgress{}
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment == nil || assignment.Issuer != manager ||
			!s.isStrictCaseDescendant(assignment.CaseID, parentCaseID) {
			continue
		}
		relevantCases[assignment.CaseID] = true
		if !assignment.Consumed {
			progress.Covered = true
		}
	}
	for caseID, state := range s.snapshot.Cases {
		if state == nil || state.Owner != manager || state.Status != string(statusOpen) ||
			!s.isStrictCaseDescendant(caseID, parentCaseID) {
			continue
		}
		relevantCases[caseID] = true
		progress.Covered = true
	}
	for _, event := range s.events {
		if !relevantCases[event.CaseID] ||
			(event.Type != "case_created" && event.Type != "case_revised" && event.ToState == "") {
			continue
		}
		if progress.Latest.ID == "" || event.Sequence > progress.Latest.Sequence {
			progress.Latest = event
		}
	}
	return progress
}

func (s *ledgerState) managerQueueBacklogs(cfg Config) ([]ManagerQueueBacklog, error) {
	byManager := map[string][]ManagerQueueItem{}
	activeCases := map[string]bool{}
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment == nil || assignment.Consumed {
			continue
		}
		activeCases[assignment.CaseID] = true
		manager := ""
		kind := ""
		actionEvent := ""
		switch assignment.Status {
		case "submitted":
			manager, kind, actionEvent = assignment.Acceptor, "review", s.assignmentReviewEventID(assignment)
		case "issued", "accepted", "rework":
			if rule, ok := cfg.exactRule(assignment.Recipient); ok && cfg.isManager(rule) {
				manager, kind = assignment.Recipient, "work"
				if assignment.Status == "issued" {
					actionEvent = assignment.EventID
				}
			}
		}
		if manager == "" {
			continue
		}
		rule, ok := cfg.exactRule(manager)
		if !ok || !isNudgeRecipient(cfg, rule) {
			continue
		}
		statusEvent, ok := s.events[assignment.StatusEventID]
		if !ok {
			return nil, fmt.Errorf("assignment %s status=%s 缺少 status event=%s", assignment.AssignmentID, assignment.Status, assignment.StatusEventID)
		}
		if _, err := parseOperationsTime("assignment status event.at", statusEvent.At); err != nil {
			return nil, err
		}
		basisEvent := statusEvent
		if kind == "work" && assignment.Status != "issued" {
			delegation := s.managerDelegationProgressFor(manager, assignment.CaseID)
			if delegation.Covered {
				continue
			}
			if delegation.Latest.ID != "" && delegation.Latest.Sequence > statusEvent.Sequence {
				basisEvent = delegation.Latest
			}
		}
		if _, err := parseOperationsTime("manager queue basis event.at", basisEvent.At); err != nil {
			return nil, err
		}
		byManager[manager] = append(byManager[manager], ManagerQueueItem{
			Kind: kind, Manager: manager, AssignmentID: assignment.AssignmentID, CaseID: assignment.CaseID,
			Status: assignment.Status, ActionEventID: actionEvent, StatusEventID: statusEvent.ID, StatusUpdatedAt: statusEvent.At,
			BasisEventID: basisEvent.ID, BasisUpdatedAt: basisEvent.At,
		})
	}
	for caseID, state := range s.snapshot.Cases {
		// Only a newly created, unassigned case is actionable merely because a
		// manager owns it. Accepted, blocked, escalated, and other non-terminal
		// historical states may require a different actor or a fresh lifecycle;
		// treating every non-closed case as manager work creates false wakeups.
		if state == nil || state.Status != string(statusOpen) || activeCases[caseID] {
			continue
		}
		manager, ok := cfg.exactRule(state.Owner)
		if !ok || !cfg.isManager(manager) || !isNudgeRecipient(cfg, manager) {
			continue
		}
		statusEvent, ok := s.events[state.LastEventID]
		if !ok {
			return nil, fmt.Errorf("case %s 缺少 last event=%s", caseID, state.LastEventID)
		}
		if _, err := parseOperationsTime("case status event.at", statusEvent.At); err != nil {
			return nil, err
		}
		byManager[manager.Name] = append(byManager[manager.Name], ManagerQueueItem{
			Kind: "owned_case", Manager: manager.Name, CaseID: caseID, Status: state.Status,
			StatusEventID: statusEvent.ID, StatusUpdatedAt: statusEvent.At,
			BasisEventID: statusEvent.ID, BasisUpdatedAt: statusEvent.At,
		})
	}
	managers := make([]string, 0, len(byManager))
	for manager := range byManager {
		managers = append(managers, manager)
	}
	sort.Strings(managers)
	backlogs := make([]ManagerQueueBacklog, 0, len(managers))
	for _, manager := range managers {
		items := byManager[manager]
		sort.Slice(items, func(i, j int) bool { return managerQueueItemLess(items[i], items[j]) })
		basisEventID, selectedAt := managerQueueItemBasis(items[0])
		backlogs = append(backlogs, ManagerQueueBacklog{
			Manager: manager, BasisEventID: basisEventID, SelectedAt: selectedAt, Items: items,
		})
	}
	return backlogs, nil
}

// managerParkingStates returns managers whose only current responsibility is
// supervision of already-issued direct-report work. There is deliberately no
// business event for "parked": this is a deterministic projection of the
// assignment ledger, so a child submission immediately turns into a review
// queue item without another transition or polling loop.
func (s *ledgerState) managerParkingStates(cfg Config) ([]ManagerParkingState, error) {
	backlogs, err := s.managerQueueBacklogs(cfg)
	if err != nil {
		return nil, err
	}
	actionable := map[string]bool{}
	for _, backlog := range backlogs {
		actionable[backlog.Manager] = len(backlog.Items) != 0
	}
	type parkingAccumulator struct {
		state    ManagerParkingState
		sequence int64
	}
	byManager := map[string]*parkingAccumulator{}
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment == nil || assignment.Consumed || assignment.Issuer == "" || assignment.Status == "submitted" {
			continue
		}
		rule, ok := cfg.exactRule(assignment.Issuer)
		if !ok || !cfg.isManager(rule) || actionable[assignment.Issuer] {
			continue
		}
		statusEvent, ok := s.events[assignment.StatusEventID]
		if !ok {
			return nil, fmt.Errorf("supervised assignment %s status=%s 缺少 status event=%s", assignment.AssignmentID, assignment.Status, assignment.StatusEventID)
		}
		if _, err := parseOperationsTime("manager parking status event.at", statusEvent.At); err != nil {
			return nil, err
		}
		entry := byManager[assignment.Issuer]
		if entry == nil {
			entry = &parkingAccumulator{state: ManagerParkingState{Manager: assignment.Issuer}}
			byManager[assignment.Issuer] = entry
		}
		entry.state.Items = append(entry.state.Items, ManagerSupervisionItem{
			AssignmentID: assignment.AssignmentID, CaseID: assignment.CaseID, Assignee: assignment.Recipient,
			Status: assignment.Status, StatusEventID: assignment.StatusEventID,
		})
		if entry.state.BasisEventID == "" || statusEvent.Sequence > entry.sequence {
			entry.sequence = statusEvent.Sequence
			entry.state.BasisEventID, entry.state.SelectedAt = statusEvent.ID, statusEvent.At
		}
	}
	managers := make([]string, 0, len(byManager))
	for manager := range byManager {
		managers = append(managers, manager)
	}
	sort.Strings(managers)
	states := make([]ManagerParkingState, 0, len(managers))
	for _, manager := range managers {
		state := byManager[manager].state
		sort.Slice(state.Items, func(i, j int) bool {
			if state.Items[i].CaseID == state.Items[j].CaseID {
				return state.Items[i].AssignmentID < state.Items[j].AssignmentID
			}
			return state.Items[i].CaseID < state.Items[j].CaseID
		})
		states = append(states, state)
	}
	return states, nil
}

func managerPostIssueDirective(ledger *ledgerState, cfg Config, manager, issuedCase, issuedAssignment string) (ActorDirective, error) {
	directive := ActorDirective{ProtocolVersion: agentRuntimeProtocolVersion, CaseID: issuedCase, AssignmentID: issuedAssignment}
	backlogs, err := ledger.managerQueueBacklogs(cfg)
	if err != nil {
		return ActorDirective{}, err
	}
	for _, backlog := range backlogs {
		if backlog.Manager != manager || len(backlog.Items) == 0 {
			continue
		}
		item := backlog.Items[0]
		directive.Action = "continue_queue"
		directive.Reason = fmt.Sprintf("委派已完成，但仍有 %d 项 durable 经理待办，当前 case=%s status=%s", len(backlog.Items), item.CaseID, item.Status)
		directive.NextAction = managerQueueAction(item)
		return directive, nil
	}
	parking, err := ledger.managerParkingStates(cfg)
	if err != nil {
		return ActorDirective{}, err
	}
	for _, state := range parking {
		if state.Manager != manager {
			continue
		}
		directive.Action = "end_turn"
		directive.Reason = fmt.Sprintf("当前仅有 %d 项直属下属执行责任，没有待审 submission 或其他已登记的经理动作", len(state.Items))
		directive.WakeOn = []string{"submission", "blocked", "needs_decision", "delivery_failure", "progress_escalation"}
		directive.Prohibited = []string{"sleep_polling", "process_polling", "herdr_status_polling", "artifact_polling"}
		return directive, nil
	}
	return ActorDirective{}, fmt.Errorf("经理 %s 完成 issue 后既无 actionable queue，也无可核验的 active supervised assignment；拒绝猜测下一动作", manager)
}

func managerQueueAction(item ManagerQueueItem) string {
	switch item.Kind {
	case "review":
		return fmt.Sprintf("核验证据后运行 hq accept --event %s --next TEXT，或 hq return --event %s --reason TEXT --next TEXT", item.ActionEventID, item.ActionEventID)
	case "work":
		if item.Status == "issued" {
			return fmt.Sprintf("运行 hq accept --event %s --next TEXT 后执行原合同", item.ActionEventID)
		}
		return fmt.Sprintf("继续原合同并运行 hq report --case %s ...；受阻必须 report --result blocked", item.CaseID)
	default:
		return fmt.Sprintf("运行 hq case show --id %s，并在本回合委派、推进或明确收敛", item.CaseID)
	}
}

func managerQueueReminderMessage(backlog ManagerQueueBacklog, stage, max int) string {
	item := backlog.Items[0]
	var action string
	switch item.Kind {
	case "review":
		action = fmt.Sprintf("有报告待审。通过 hq accept --event %s --next TEXT；退回 hq return --event %s --reason TEXT --next TEXT", item.ActionEventID, item.ActionEventID)
	case "work":
		if item.Status == "issued" {
			action = fmt.Sprintf("运行 hq accept --event %s --next TEXT 后执行原合同", item.ActionEventID)
		} else {
			action = fmt.Sprintf("继续原合同并运行 hq report --case %s；受阻须 report --result blocked", item.CaseID)
		}
	default:
		action = fmt.Sprintf("运行 hq case show --id %s 后委派、推进或明确收敛", item.CaseID)
	}
	message := fmt.Sprintf("HQ守卫%d/%d：待办%d，case=%s。%s。", stage, max, len(backlog.Items), item.CaseID, action)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	if item.Kind == "review" {
		return fmt.Sprintf("HQ守卫%d/%d：有报告待审。通过 hq accept --event %s --next TEXT；退回 hq return --event %s --reason TEXT --next TEXT。", stage, max, item.ActionEventID, item.ActionEventID)
	}
	return fmt.Sprintf("HQ守卫%d/%d：仍有%d项待办；运行 hq inbox 与 hq assignment list 后收敛；受阻须在原assignment report blocked。", stage, max, len(backlog.Items))
}

func managerQueueEscalationMessage(backlog ManagerQueueBacklog, nudges int) string {
	item := backlog.Items[0]
	message := fmt.Sprintf("HQ队列升级：直属经理%s经%d次durable催办后队列未变化，仍有%d项；首项case=%s status=%s。请要求其收敛或在原assignment报告blocked；HQ未自动验收或改状态。", backlog.Manager, nudges, len(backlog.Items), item.CaseID, item.Status)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ队列升级：直属经理%s经%d次催办仍有%d项未收敛。请核对其 inbox/assignment，并要求收敛或报告blocked；HQ未自动验收。", backlog.Manager, nudges, len(backlog.Items))
}

func managerQueueUncertainEscalationMessage(backlog ManagerQueueBacklog, nudgeID string) string {
	message := fmt.Sprintf("HQ队列升级：经理%s的催办nudge=%s结果不确定，已禁止自动重投。请运行 hq nudge status --id %s，核对后reconcile，并要求其收敛case=%s；HQ未自动验收。", backlog.Manager, nudgeID, nudgeID, backlog.Items[0].CaseID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ队列升级：经理%s的催办nudge=%s结果不确定，已禁止重投。请核对后reconcile并要求其收敛；HQ未自动验收。", backlog.Manager, nudgeID)
}

func managerQueueDedupe(manager, basis, kind string, stage int) string {
	return strings.Join([]string{"manager-queue", manager, basis, kind, strconv.Itoa(stage)}, ":")
}

func ledgerNudgeByDedupe(ledger *ledgerState, dedupe string) *nudgeLedgerRecord {
	for _, record := range ledger.nudges {
		if record != nil && record.Origin.DedupeKey == dedupe {
			return record
		}
	}
	return nil
}

func liveQueueTargetStatus(snapshot HerdrSnapshot, cfg Config, hqRoot, target string) (string, error) {
	rule, ok := cfg.exactRule(target)
	if !ok || !isNudgeRecipient(cfg, rule) {
		return "", fmt.Errorf("queue target %s 不再是精确登记且可接收任务的在职 HQ seat", target)
	}
	workspaceID := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == cfg.WorkspaceLabel {
			if workspaceID != "" {
				return "", fmt.Errorf("workspace label %s 匹配多个 workspace", cfg.WorkspaceLabel)
			}
			workspaceID = workspace.ID
		}
	}
	if workspaceID == "" {
		return "", fmt.Errorf("workspace label %s 不存在", cfg.WorkspaceLabel)
	}
	binding, err := ResolveLiveBinding(snapshot, cfg, hqRoot, LiveBindingRequest{Seat: target, RequireInteractiveReady: true})
	if err != nil {
		return "", err
	}
	if binding.WorkspaceID != workspaceID {
		return "", fmt.Errorf("queue target %s 位于错误 workspace", target)
	}
	return binding.Status, nil
}

func (a *App) driveQueueNudge(ctx context.Context, id, dedupe, target, message string, create, allowAssignmentWorker bool) error {
	child := *a
	child.RequestContext = nonNilContext(ctx)
	child.CallerPane = child.MaintenancePane
	child.Out, child.Err = io.Discard, io.Discard
	if store, ok := a.Store.(*Store); ok {
		child.Store = store.withRequestContext(ctx)
	}
	if create {
		if err := child.cmdNudgeEnqueueWithScope([]string{"--id", id, "--dedupe", dedupe, "--to", target, "--message", message, "--ttl", managerQueueNudgeTTL.String()}, allowAssignmentWorker); err != nil {
			return err
		}
	}
	view, err := child.readNudge(id)
	if err != nil {
		return err
	}
	claimID := stableCommandID("manager-queue-claim", id)
	claimExpired := false
	if view.State == "claimed" {
		claimExpiry, parseErr := parseOperationsTime("queue nudge claim_expires_at", view.ClaimExpiresAt)
		if parseErr != nil {
			return parseErr
		}
		claimExpired = !child.operationsNow().Before(claimExpiry)
		if claimExpired {
			claimID = stableCommandID("manager-queue-reclaim", id, view.ClaimID, view.ClaimExpiresAt)
		}
	}
	if view.State == "queued" || claimExpired {
		if err := child.cmdNudgeClaim([]string{"--id", id, "--claim", claimID, "--lease", "1m"}); err != nil {
			return err
		}
		view, err = child.readNudge(id)
		if err != nil {
			return err
		}
	}
	if view.State == "claimed" && view.ClaimID == claimID {
		return child.cmdNudgeDeliver([]string{"--id", id, "--claim", claimID})
	}
	if view.State == "attempted" || view.State == "unknown" {
		return fmt.Errorf("queue nudge=%s state=%s；结果不确定，禁止自动重投，须人工 reconcile", id, view.State)
	}
	return nil
}

func (a *App) driveManagerQueueNudge(ctx context.Context, id, dedupe, target, message string, create bool) error {
	return a.driveQueueNudge(ctx, id, dedupe, target, message, create, false)
}

func (a *App) runManagerQueueWatchdogOnce(ctx context.Context) error {
	if a.Herdr == nil || a.Store == nil || a.MaintenancePane == "" {
		return nil
	}
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return err
	}
	ledger, err := validateLedger(events, a.Config)
	if err != nil {
		return err
	}
	backlogs, err := ledger.managerQueueBacklogs(a.Config)
	if err != nil {
		return err
	}
	if len(backlogs) == 0 {
		return nil
	}
	queuedActionTargets := map[string]bool{}
	for _, pending := range ledger.queuedWakeBudgetActions(a.Config) {
		queuedActionTargets[pending.Target] = true
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	now := a.operationsNow()
	stallAfter, escalateAfter, maxNudges := a.Config.managerQueueWatchdogPolicy()
	failures := make([]string, 0)
	for _, backlog := range backlogs {
		// Preserve FIFO instruction semantics: a manager with an actionable
		// budget-downgraded message must consume that context before a generic
		// queue reminder asks it to advance older state.
		if queuedActionTargets[backlog.Manager] {
			continue
		}
		status, statusErr := liveQueueTargetStatus(snapshot, a.Config, a.HQRoot, backlog.Manager)
		if statusErr != nil || (status != "idle" && status != "done") {
			continue
		}
		selectedAt, parseErr := parseOperationsTime("manager queue selected_at", backlog.SelectedAt)
		if parseErr != nil {
			return parseErr
		}
		if now.Sub(selectedAt) < stallAfter {
			continue
		}
		lastAt := selectedAt
		completedNudges := 0
		active := false
		var uncertainNudge *nudgeLedgerRecord
		for stage := 1; stage <= maxNudges; stage++ {
			dedupe := managerQueueDedupe(backlog.Manager, backlog.BasisEventID, "nudge", stage)
			record := ledgerNudgeByDedupe(ledger, dedupe)
			if record == nil {
				break
			}
			completedNudges = stage
			lastAt, _ = parseOperationsTime("manager queue nudge.at", record.Origin.At)
			if nudgeStateActive(record.State) {
				if record.State == "attempted" || record.State == "unknown" {
					uncertainNudge = record
				} else {
					active = true
					if err := a.driveManagerQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false); err != nil {
						failures = append(failures, fmt.Sprintf("%s: %v", backlog.Manager, err))
					}
				}
				break
			}
		}
		if active {
			continue
		}
		if uncertainNudge == nil && completedNudges < maxNudges {
			if completedNudges > 0 && now.Sub(lastAt) < stallAfter {
				continue
			}
			stage := completedNudges + 1
			dedupe := managerQueueDedupe(backlog.Manager, backlog.BasisEventID, "nudge", stage)
			id := stableCommandID("manager-queue-nudge", dedupe)
			if err := a.driveManagerQueueNudge(ctx, id, dedupe, backlog.Manager, managerQueueReminderMessage(backlog, stage, maxNudges), true); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", backlog.Manager, err))
			}
			continue
		}
		if now.Sub(lastAt) < escalateAfter {
			continue
		}
		rule, ok := a.Config.exactRule(backlog.Manager)
		if !ok || rule.ReportsTo == "" {
			continue
		}
		dedupe := managerQueueDedupe(backlog.Manager, backlog.BasisEventID, "escalate", 1)
		record := ledgerNudgeByDedupe(ledger, dedupe)
		if record != nil {
			if record.State == "queued" || record.State == "claimed" {
				if err := a.driveManagerQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false); err != nil {
					failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Manager, rule.ReportsTo, err))
				}
			}
			continue
		}
		id := stableCommandID("manager-queue-escalation", dedupe)
		message := managerQueueEscalationMessage(backlog, maxNudges)
		if uncertainNudge != nil {
			message = managerQueueUncertainEscalationMessage(backlog, uncertainNudge.Origin.NudgeID)
		}
		if err := a.driveManagerQueueNudge(ctx, id, dedupe, rule.ReportsTo, message, true); err != nil {
			failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Manager, rule.ReportsTo, err))
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("manager queue watchdog：%s", strings.Join(failures, "; "))
	}
	return nil
}
