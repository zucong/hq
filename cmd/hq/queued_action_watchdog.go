package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// QueuedActionBacklog is the oldest actionable message for a target that was
// downgraded only because the consecutive-wake budget was exhausted. Explicit
// quiet/inject messages are deliberately excluded: their caller asked HQ not
// to wake the target.
type QueuedActionBacklog struct {
	Target       string
	DeliveryID   string
	BasisEventID string
	SelectedAt   string
	CaseID       string
}

func (s *ledgerState) queuedWakeBudgetActions(cfg Config) []QueuedActionBacklog {
	byTarget := map[string]QueuedActionBacklog{}
	sequences := map[string]int64{}
	for _, record := range s.deliveries {
		if record == nil || record.Status != deliveryQueued || record.ContextState != deliveryContextPending ||
			record.Origin.Type != "message_prepared" || record.Origin.DeliveryReason != "wake-budget-exhausted" ||
			!messageNeedsAction(record.Origin.MessageKind) {
			continue
		}
		rule, ok := cfg.exactRule(record.Origin.Recipient)
		if !ok || !isNudgeRecipient(cfg, rule) {
			continue
		}
		if previous, ok := sequences[record.Origin.Recipient]; ok && previous <= record.Origin.Sequence {
			continue
		}
		sequences[record.Origin.Recipient] = record.Origin.Sequence
		byTarget[record.Origin.Recipient] = QueuedActionBacklog{
			Target: record.Origin.Recipient, DeliveryID: record.Origin.DeliveryID,
			BasisEventID: record.Origin.ID, SelectedAt: record.Origin.At, CaseID: record.Origin.CaseID,
		}
	}
	targets := make([]string, 0, len(byTarget))
	for target := range byTarget {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	result := make([]QueuedActionBacklog, 0, len(targets))
	for _, target := range targets {
		result = append(result, byTarget[target])
	}
	return result
}

func queuedActionDedupe(backlog QueuedActionBacklog, kind string, stage int) string {
	return strings.Join([]string{"queued-action", backlog.Target, backlog.BasisEventID, kind, strconv.Itoa(stage)}, ":")
}

func queuedActionReminderMessage(backlog QueuedActionBacklog, stage, max int) string {
	message := fmt.Sprintf("HQ延迟投递守卫%d/%d：action delivery=%s 因连续唤醒预算进入静默队列。先运行 hq delivery consume，按FIFO读取并执行；然后继续原case=%s合同并用HQ收敛。",
		stage, max, backlog.DeliveryID, backlog.CaseID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ延迟投递守卫%d/%d：有action delivery=%s待读取。先运行 hq delivery consume，再按原合同执行并用HQ收敛。", stage, max, backlog.DeliveryID)
}

func queuedActionEscalationMessage(backlog QueuedActionBacklog, nudges int) string {
	message := fmt.Sprintf("HQ延迟投递升级：%s经%d次durable唤醒仍未消费action delivery=%s（case=%s）。请要求其运行 hq delivery consume 并按原合同收敛；HQ未代执行业务。",
		backlog.Target, nudges, backlog.DeliveryID, backlog.CaseID)
	if _, err := validateShortText("message", message, true); err == nil {
		return message
	}
	return fmt.Sprintf("HQ延迟投递升级：%s仍未消费delivery=%s。请要求其运行 hq delivery consume 后收敛原合同。", backlog.Target, backlog.DeliveryID)
}

func (a *App) runQueuedActionWatchdogOnce(ctx context.Context) error {
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
	backlogs := ledger.queuedWakeBudgetActions(a.Config)
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
		status, statusErr := liveQueueTargetStatus(snapshot, a.Config, a.HQRoot, backlog.Target)
		if statusErr != nil || (status != "idle" && status != "done") {
			continue
		}
		selectedAt, parseErr := parseOperationsTime("queued action selected_at", backlog.SelectedAt)
		if parseErr != nil {
			return parseErr
		}
		if now.Sub(selectedAt) < stallAfter {
			continue
		}

		lastAt := selectedAt
		completedNudges := 0
		active := false
		var uncertain *nudgeLedgerRecord
		for stage := 1; stage <= maxNudges; stage++ {
			dedupe := queuedActionDedupe(backlog, "nudge", stage)
			record := ledgerNudgeByDedupe(ledger, dedupe)
			if record == nil {
				break
			}
			completedNudges = stage
			lastAt, _ = parseOperationsTime("queued action nudge.at", record.Origin.At)
			if nudgeStateActive(record.State) {
				if record.State == "attempted" || record.State == "unknown" {
					uncertain = record
				} else {
					active = true
					if err := a.driveQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false, true); err != nil {
						failures = append(failures, fmt.Sprintf("%s: %v", backlog.Target, err))
					}
				}
				break
			}
		}
		if active {
			continue
		}
		if uncertain == nil && completedNudges < maxNudges {
			if completedNudges > 0 && now.Sub(lastAt) < stallAfter {
				continue
			}
			stage := completedNudges + 1
			dedupe := queuedActionDedupe(backlog, "nudge", stage)
			id := stableCommandID("queued-action-nudge", dedupe)
			if err := a.driveQueueNudge(ctx, id, dedupe, backlog.Target, queuedActionReminderMessage(backlog, stage, maxNudges), true, true); err != nil {
				failures = append(failures, fmt.Sprintf("%s: %v", backlog.Target, err))
			}
			continue
		}
		if now.Sub(lastAt) < escalateAfter {
			continue
		}
		rule, ok := a.Config.exactRule(backlog.Target)
		if !ok || rule.ReportsTo == "" {
			continue
		}
		dedupe := queuedActionDedupe(backlog, "escalate", 1)
		record := ledgerNudgeByDedupe(ledger, dedupe)
		if record != nil {
			if record.State == "queued" || record.State == "claimed" {
				if err := a.driveQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false, true); err != nil {
					failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Target, rule.ReportsTo, err))
				}
			}
			continue
		}
		id := stableCommandID("queued-action-escalation", dedupe)
		message := queuedActionEscalationMessage(backlog, maxNudges)
		if uncertain != nil {
			message = fmt.Sprintf("HQ延迟投递升级：%s的恢复nudge=%s结果不确定；请核对后reconcile，并要求其运行 hq delivery consume。", backlog.Target, uncertain.Origin.NudgeID)
		}
		if err := a.driveQueueNudge(ctx, id, dedupe, rule.ReportsTo, message, true, true); err != nil {
			failures = append(failures, fmt.Sprintf("%s->%s: %v", backlog.Target, rule.ReportsTo, err))
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("queued action watchdog：%s", strings.Join(failures, "; "))
	}
	return nil
}
