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
}

type ManagerQueueBacklog struct {
	Manager      string             `json:"manager"`
	BasisEventID string             `json:"basis_event_id"`
	SelectedAt   string             `json:"selected_at"`
	Items        []ManagerQueueItem `json:"items"`
}

func managerQueueItemLess(left, right ManagerQueueItem) bool {
	leftPriority := managerQueueKindPriority(left.Kind)
	rightPriority := managerQueueKindPriority(right.Kind)
	if leftPriority != rightPriority {
		return leftPriority < rightPriority
	}
	if left.StatusUpdatedAt == right.StatusUpdatedAt {
		if left.CaseID == right.CaseID {
			return left.AssignmentID < right.AssignmentID
		}
		return left.CaseID < right.CaseID
	}
	return left.StatusUpdatedAt < right.StatusUpdatedAt
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
		byManager[manager] = append(byManager[manager], ManagerQueueItem{
			Kind: kind, Manager: manager, AssignmentID: assignment.AssignmentID, CaseID: assignment.CaseID,
			Status: assignment.Status, ActionEventID: actionEvent, StatusEventID: statusEvent.ID, StatusUpdatedAt: statusEvent.At,
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
		backlogs = append(backlogs, ManagerQueueBacklog{
			Manager: manager, BasisEventID: items[0].StatusEventID, SelectedAt: items[0].StatusUpdatedAt, Items: items,
		})
	}
	return backlogs, nil
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
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	now := a.operationsNow()
	stallAfter, escalateAfter, maxNudges := a.Config.managerQueueWatchdogPolicy()
	failures := make([]string, 0)
	for _, backlog := range backlogs {
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
