package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type AssignmentProgressItem struct {
	Agent           string
	AssignmentID    string
	AssignmentEvent string
	CaseID          string
	Status          string
	StatusEventID   string
	StatusUpdatedAt string
}

type AssignmentProgressBacklog struct {
	Agent        string
	BasisEventID string
	SelectedAt   string
	Items        []AssignmentProgressItem
}

func (s *ledgerState) assignmentProgressBacklogs(cfg Config) ([]AssignmentProgressBacklog, error) {
	byAgent := map[string][]AssignmentProgressItem{}
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment == nil || assignment.Consumed || (assignment.Status != "accepted" && assignment.Status != "rework") {
			continue
		}
		rule, ok := cfg.exactRule(assignment.Recipient)
		if !ok || cfg.isManager(rule) || !isNudgeRecipient(cfg, rule) {
			continue
		}
		statusEvent, ok := s.events[assignment.StatusEventID]
		if !ok {
			return nil, fmt.Errorf("assignment %s status=%s 缺少 status event=%s", assignment.AssignmentID, assignment.Status, assignment.StatusEventID)
		}
		if _, err := parseOperationsTime("assignment progress status event.at", statusEvent.At); err != nil {
			return nil, err
		}
		byAgent[assignment.Recipient] = append(byAgent[assignment.Recipient], AssignmentProgressItem{
			Agent: assignment.Recipient, AssignmentID: assignment.AssignmentID, AssignmentEvent: assignment.EventID,
			CaseID: assignment.CaseID, Status: assignment.Status, StatusEventID: statusEvent.ID, StatusUpdatedAt: statusEvent.At,
		})
	}
	agents := make([]string, 0, len(byAgent))
	for agent := range byAgent {
		agents = append(agents, agent)
	}
	sort.Strings(agents)
	backlogs := make([]AssignmentProgressBacklog, 0, len(agents))
	for _, agent := range agents {
		items := byAgent[agent]
		sort.Slice(items, func(i, j int) bool {
			if items[i].StatusUpdatedAt == items[j].StatusUpdatedAt {
				return items[i].AssignmentID < items[j].AssignmentID
			}
			return items[i].StatusUpdatedAt < items[j].StatusUpdatedAt
		})
		backlogs = append(backlogs, AssignmentProgressBacklog{
			Agent: agent, BasisEventID: items[0].StatusEventID, SelectedAt: items[0].StatusUpdatedAt, Items: items,
		})
	}
	return backlogs, nil
}

func assignmentProgressDedupe(agent, basis, kind string, stage int) string {
	return strings.Join([]string{"assignment-progress", agent, basis, kind, strconv.Itoa(stage)}, ":")
}

func assignmentProgressReminderMessage(backlog AssignmentProgressBacklog, stage, max int) string {
	item := backlog.Items[0]
	message := fmt.Sprintf("HQ执行守卫%d/%d：原assignment仍为%s，case=%s。先运行 hq assignment show --id %s 与 hq history --case %s；完成后重试 hq report，受阻须 report --result blocked。",
		stage, max, item.Status, item.CaseID, item.AssignmentID, item.CaseID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ执行守卫%d/%d：case=%s 的原assignment仍未report。核对原合同后继续；完成须 hq report，受阻须 report blocked。", stage, max, item.CaseID)
}

func assignmentProgressEscalationMessage(backlog AssignmentProgressBacklog, nudges int) string {
	item := backlog.Items[0]
	message := fmt.Sprintf("HQ执行升级：员工%s经%d次durable催办仍未收敛原assignment，case=%s status=%s。请要求其按原合同report completed或blocked；HQ未代报或改状态。",
		backlog.Agent, nudges, item.CaseID, item.Status)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ执行升级：员工%s经%d次催办仍未report case=%s。请核对原assignment并要求收敛；HQ未代报。", backlog.Agent, nudges, item.CaseID)
}

func assignmentProgressUncertainEscalationMessage(backlog AssignmentProgressBacklog, nudgeID string) string {
	message := fmt.Sprintf("HQ执行升级：员工%s的催办nudge=%s结果不确定，已禁止自动重投。请运行 hq nudge status --id %s，核对后reconcile并要求其收敛case=%s；HQ未代报。",
		backlog.Agent, nudgeID, nudgeID, backlog.Items[0].CaseID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ执行升级：员工%s的nudge=%s结果不确定。请核对并reconcile后要求其收敛原assignment。", backlog.Agent, nudgeID)
}

func assignmentProgressRuntimeRecoveryEnvelope(rule AgentRule, work runtimeRecoveryWork) string {
	lines := []string{
		fmt.Sprintf("[HQ runtime recovery] trigger=assignment_progress seat=%s。你仍是同一员工；新 runtime 不改变角色、权限边界、汇报线或原任务合同。", rule.Name),
		"本恢复信封替代上方‘等待首个 case’指令。隐藏聊天记录不会复制；只以当前 AGENTS.md、同一工位文件和 HQ durable ledger 为事实源。",
	}
	for _, assignment := range work.assignments {
		lines = append(lines, fmt.Sprintf("ACTIVE_ASSIGNMENT id=%s event=%s case=%s status=%s；运行 `hq assignment show --id %s` 与 `hq history --case %s`，从 durable 状态继续；不要重复 accept 或建立替代任务。",
			assignment.AssignmentID, assignment.AssignmentEventID, assignment.CaseID, assignment.Status, assignment.AssignmentID, assignment.CaseID))
	}
	lines = append(lines, "继续未完成工作；完成后必须在原 case 运行 `hq report`，受阻必须 `hq report --result blocked`，不得仅结束 Agent 回合。")
	return strings.Join(lines, "\n")
}

func (a *App) resumeAssignmentProgressTarget(ctx context.Context, backlog AssignmentProgressBacklog) (bool, error) {
	releaseSeat, err := a.lockRuntimeSeat(backlog.Agent)
	if err != nil {
		return false, err
	}
	defer releaseSeat()
	if err := a.ensureRuntimeSeatOriginSafeLocked(backlog.Agent); err != nil {
		return false, err
	}
	state, err := a.inspectDeliveryTarget(backlog.Agent)
	if err != nil || state != deliveryRuntimeOffline {
		return false, err
	}
	rule, ok := a.Config.exactRule(backlog.Agent)
	if !ok || (rule.ActivationPolicy != activationOnAssignment && rule.ActivationPolicy != activationAlways) {
		return false, nil
	}
	work, err := a.runtimeRecoveryWorkFor(backlog.Agent)
	if err != nil {
		return false, err
	}
	decision, err := a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentResume, Target: backlog.Agent}, func() error {
		return a.coldResumeDeliveryTargetAdmittedWithOptions(backlog.Agent, runtimeStartOptions{
			Actor: "hq-assignment-progress-watchdog", Reason: "active assignment runtime recovery",
			PromptSuffix: assignmentProgressRuntimeRecoveryEnvelope(rule, work),
		})
	})
	if err != nil || !decision.Allowed {
		return false, err
	}
	return true, nil
}

func (a *App) runAssignmentProgressWatchdogOnce(ctx context.Context) error {
	if a.Herdr == nil || a.Store == nil || a.Sessions == nil || a.MaintenancePane == "" {
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
	backlogs, err := ledger.assignmentProgressBacklogs(a.Config)
	if err != nil || len(backlogs) == 0 {
		return err
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	now := a.operationsNow()
	stallAfter, escalateAfter, maxNudges := a.Config.managerQueueWatchdogPolicy()
	failures := make([]string, 0)
	for _, backlog := range backlogs {
		selectedAt, parseErr := parseOperationsTime("assignment progress selected_at", backlog.SelectedAt)
		if parseErr != nil {
			return parseErr
		}
		if now.Sub(selectedAt) < stallAfter {
			continue
		}
		status, statusErr := liveQueueTargetStatus(snapshot, a.Config, a.HQRoot, backlog.Agent)
		if statusErr != nil {
			resumed, resumeErr := a.resumeAssignmentProgressTarget(ctx, backlog)
			if resumeErr != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", backlog.Agent, resumeErr))
			}
			if resumed || resumeErr != nil {
				continue
			}
			continue
		}
		if status != "idle" && status != "done" {
			continue
		}

		lastAt := selectedAt
		completedNudges := 0
		active := false
		var uncertainNudge *nudgeLedgerRecord
		for stage := 1; stage <= maxNudges; stage++ {
			dedupe := assignmentProgressDedupe(backlog.Agent, backlog.BasisEventID, "nudge", stage)
			record := ledgerNudgeByDedupe(ledger, dedupe)
			if record == nil {
				break
			}
			completedNudges = stage
			lastAt, _ = parseOperationsTime("assignment progress nudge.at", record.Origin.At)
			if nudgeStateActive(record.State) {
				if record.State == "attempted" || record.State == "unknown" {
					uncertainNudge = record
				} else {
					active = true
					if err := a.driveQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false, true); err != nil {
						failures = append(failures, fmt.Sprintf("%s: %v", backlog.Agent, err))
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
			dedupe := assignmentProgressDedupe(backlog.Agent, backlog.BasisEventID, "nudge", stage)
			id := stableCommandID("assignment-progress-nudge", dedupe)
			if err := a.driveQueueNudge(ctx, id, dedupe, backlog.Agent, assignmentProgressReminderMessage(backlog, stage, maxNudges), true, true); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", backlog.Agent, err))
			}
			continue
		}
		if now.Sub(lastAt) < escalateAfter {
			continue
		}
		rule, ok := a.Config.exactRule(backlog.Agent)
		if !ok || rule.ReportsTo == "" {
			continue
		}
		dedupe := assignmentProgressDedupe(backlog.Agent, backlog.BasisEventID, "escalate", 1)
		record := ledgerNudgeByDedupe(ledger, dedupe)
		if record != nil {
			if record.State == "queued" || record.State == "claimed" {
				if err := a.driveQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false, true); err != nil {
					failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Agent, rule.ReportsTo, err))
				}
			}
			continue
		}
		id := stableCommandID("assignment-progress-escalation", dedupe)
		message := assignmentProgressEscalationMessage(backlog, maxNudges)
		if uncertainNudge != nil {
			message = assignmentProgressUncertainEscalationMessage(backlog, uncertainNudge.Origin.NudgeID)
		}
		if err := a.driveQueueNudge(ctx, id, dedupe, rule.ReportsTo, message, true, true); err != nil {
			failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Agent, rule.ReportsTo, err))
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("assignment progress watchdog：%s", strings.Join(failures, "; "))
	}
	return nil
}
