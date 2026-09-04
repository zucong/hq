package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type UrgentDirectiveBacklog struct {
	MessageID    string `json:"message_id"`
	CaseID       string `json:"case_id"`
	AssignmentID string `json:"assignment_id"`
	Sender       string `json:"sender"`
	Recipient    string `json:"recipient"`
	BasisEventID string `json:"basis_event_id"`
	SelectedAt   string `json:"selected_at"`
}

func (s *ledgerState) urgentDirectiveBacklogs(cfg Config) ([]UrgentDirectiveBacklog, error) {
	backlogs := make([]UrgentDirectiveBacklog, 0)
	for _, record := range s.deliveries {
		if record == nil || record.Origin.Type != "message_prepared" ||
			record.Origin.MessageKind != "directive" || effectiveMessageUrgency(record.Origin.Urgency) != messageUrgencyUrgent ||
			record.Status != deliverySent || record.Ack.ID != "" || record.Terminal.ID == "" {
			continue
		}
		rule, ok := cfg.exactRule(record.Origin.Recipient)
		if !ok || !isNudgeRecipient(cfg, rule) {
			continue
		}
		selectedAt := record.Terminal.At
		if _, err := parseOperationsTime("urgent directive sent.at", selectedAt); err != nil {
			return nil, err
		}
		messageID := record.Origin.MessageID
		if messageID == "" {
			messageID = record.Origin.ID
		}
		backlogs = append(backlogs, UrgentDirectiveBacklog{
			MessageID: messageID, CaseID: record.Origin.CaseID, AssignmentID: record.Origin.AssignmentID,
			Sender: record.Origin.Actor, Recipient: record.Origin.Recipient,
			BasisEventID: record.Terminal.ID, SelectedAt: selectedAt,
		})
	}
	sort.Slice(backlogs, func(i, j int) bool {
		if backlogs[i].SelectedAt == backlogs[j].SelectedAt {
			return backlogs[i].MessageID < backlogs[j].MessageID
		}
		return backlogs[i].SelectedAt < backlogs[j].SelectedAt
	})
	return backlogs, nil
}

func urgentDirectiveDedupe(backlog UrgentDirectiveBacklog, kind string, stage int) string {
	return strings.Join([]string{"urgent-directive", backlog.Recipient, backlog.MessageID, backlog.BasisEventID, kind, strconv.Itoa(stage)}, ":")
}

func urgentDirectiveReminderMessage(backlog UrgentDirectiveBacklog, stage, max int) string {
	message := fmt.Sprintf("HQ加急指令守卫%d/%d：message=%s case=%s 尚未确认已读。立即读取原信封与当前assignment=%s；读懂后运行 hq message ack --message %s。注意：directive不修改任务合同；若原要求已失效，以HQ当前assignment为准。",
		stage, max, backlog.MessageID, backlog.CaseID, backlog.AssignmentID, backlog.MessageID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ加急指令守卫%d/%d：message=%s尚未确认。立即读原信封，读懂后运行 hq message ack --message %s；directive不修改任务合同。", stage, max, backlog.MessageID, backlog.MessageID)
}

func urgentDirectiveEscalationMessage(backlog UrgentDirectiveBacklog, nudges int) string {
	message := fmt.Sprintf("HQ加急指令升级：%s经%d次durable提醒仍未ack message=%s case=%s。请直属负责人核验其是否读懂，并要求运行 hq message ack --message %s；HQ未代替其确认，也未修改任务合同。",
		backlog.Recipient, nudges, backlog.MessageID, backlog.CaseID, backlog.MessageID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ加急指令升级：%s仍未ack message=%s。请直属负责人核验并要求其确认；HQ未代ack。", backlog.Recipient, backlog.MessageID)
}

func urgentDirectiveUncertainEscalationMessage(backlog UrgentDirectiveBacklog, nudgeID string) string {
	message := fmt.Sprintf("HQ加急指令升级：%s的ack提醒nudge=%s结果不确定，已禁止自动重投。请运行 hq nudge status --id %s，核对并reconcile，再要求其确认message=%s；HQ未代ack。",
		backlog.Recipient, nudgeID, nudgeID, backlog.MessageID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ加急指令升级：%s的nudge=%s结果不确定。请核验reconcile，并要求其确认message=%s。", backlog.Recipient, nudgeID, backlog.MessageID)
}

func (a *App) runUrgentDirectiveWatchdogOnce(ctx context.Context) error {
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
	backlogs, err := ledger.urgentDirectiveBacklogs(a.Config)
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
		selectedAt, parseErr := parseOperationsTime("urgent directive selected_at", backlog.SelectedAt)
		if parseErr != nil {
			return parseErr
		}
		if now.Sub(selectedAt) < stallAfter {
			continue
		}
		status, statusErr := liveQueueTargetStatus(snapshot, a.Config, a.HQRoot, backlog.Recipient)
		if statusErr != nil || (status != "idle" && status != "done") {
			continue
		}

		lastAt := selectedAt
		completedNudges := 0
		active := false
		var uncertainNudge *nudgeLedgerRecord
		for stage := 1; stage <= maxNudges; stage++ {
			dedupe := urgentDirectiveDedupe(backlog, "nudge", stage)
			record := ledgerNudgeByDedupe(ledger, dedupe)
			if record == nil {
				break
			}
			completedNudges = stage
			lastAt, _ = parseOperationsTime("urgent directive nudge.at", record.Origin.At)
			if nudgeStateActive(record.State) {
				if record.State == "attempted" || record.State == "unknown" {
					uncertainNudge = record
				} else {
					active = true
					if err := a.driveQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false, true); err != nil {
						failures = append(failures, fmt.Sprintf("%s: %v", backlog.Recipient, err))
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
			dedupe := urgentDirectiveDedupe(backlog, "nudge", stage)
			id := stableCommandID("urgent-directive-nudge", dedupe)
			if err := a.driveQueueNudge(ctx, id, dedupe, backlog.Recipient, urgentDirectiveReminderMessage(backlog, stage, maxNudges), true, true); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", backlog.Recipient, err))
			}
			continue
		}
		if now.Sub(lastAt) < escalateAfter {
			continue
		}
		rule, ok := a.Config.exactRule(backlog.Recipient)
		if !ok || rule.ReportsTo == "" {
			continue
		}
		dedupe := urgentDirectiveDedupe(backlog, "escalate", 1)
		record := ledgerNudgeByDedupe(ledger, dedupe)
		if record != nil {
			if record.State == "queued" || record.State == "claimed" {
				if err := a.driveQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false, true); err != nil {
					failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Recipient, rule.ReportsTo, err))
				}
			}
			continue
		}
		id := stableCommandID("urgent-directive-escalation", dedupe)
		message := urgentDirectiveEscalationMessage(backlog, maxNudges)
		if uncertainNudge != nil {
			message = urgentDirectiveUncertainEscalationMessage(backlog, uncertainNudge.Origin.NudgeID)
		}
		if err := a.driveQueueNudge(ctx, id, dedupe, rule.ReportsTo, message, true, true); err != nil {
			failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Recipient, rule.ReportsTo, err))
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("urgent directive watchdog：%s", strings.Join(failures, "; "))
	}
	return nil
}
