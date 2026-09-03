package main

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const closureQueuePromptItemLimit = 8

type ClosureQueueItem struct {
	Closer          string `json:"closer"`
	CaseID          string `json:"case_id"`
	Status          string `json:"status"`
	StatusEventID   string `json:"status_event_id"`
	StatusUpdatedAt string `json:"status_updated_at"`
}

type ClosureQueueBacklog struct {
	Closer       string             `json:"closer"`
	BasisEventID string             `json:"basis_event_id"`
	SelectedAt   string             `json:"selected_at"`
	Items        []ClosureQueueItem `json:"items"`
}

func closureQueueReadyStatus(status string) bool {
	return status == string(statusAccepted) || status == string(statusFindingAccepted)
}

// closureQueueBacklog derives only cases that the account closer can inspect
// and close immediately in post-order. It never treats open, blocked, or
// needs-decision work as implicitly approved for closure.
func (s *ledgerState) closureQueueBacklog(cfg Config) (*ClosureQueueBacklog, error) {
	closer, err := cfg.accountCloser()
	if err != nil {
		return nil, err
	}
	children := make(map[string][]*CaseState)
	for _, state := range s.snapshot.Cases {
		if state != nil && state.ParentCaseID != "" {
			children[state.ParentCaseID] = append(children[state.ParentCaseID], state)
		}
	}
	items := make([]ClosureQueueItem, 0)
	for _, state := range s.snapshot.Cases {
		if state == nil || !closureQueueReadyStatus(state.Status) {
			continue
		}
		postOrderReady := true
		for _, child := range children[state.ID] {
			if child.Status != string(statusClosed) {
				postOrderReady = false
				break
			}
		}
		if !postOrderReady || len(s.activeAssignments(state.ID)) != 0 || len(s.unsettledClosureDeliveries(state.ID, false)) != 0 {
			continue
		}
		statusEvent, ok := s.events[state.LastEventID]
		if !ok {
			return nil, fmt.Errorf("closure queue case %s 缺少 last event=%s", state.ID, state.LastEventID)
		}
		if _, err := parseOperationsTime("closure queue status event.at", statusEvent.At); err != nil {
			return nil, err
		}
		items = append(items, ClosureQueueItem{
			Closer: closer.Name, CaseID: state.ID, Status: state.Status,
			StatusEventID: statusEvent.ID, StatusUpdatedAt: statusEvent.At,
		})
	}
	if len(items) == 0 {
		return nil, nil
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].StatusUpdatedAt == items[j].StatusUpdatedAt {
			return items[i].CaseID < items[j].CaseID
		}
		return items[i].StatusUpdatedAt < items[j].StatusUpdatedAt
	})
	return &ClosureQueueBacklog{
		Closer: closer.Name, BasisEventID: items[0].StatusEventID,
		SelectedAt: items[0].StatusUpdatedAt, Items: items,
	}, nil
}

func boundedClosureQueueCases(items []ClosureQueueItem) (string, int) {
	limit := len(items)
	if limit > closureQueuePromptItemLimit {
		limit = closureQueuePromptItemLimit
	}
	values := make([]string, 0, limit)
	for _, item := range items[:limit] {
		values = append(values, item.CaseID+"("+item.Status+")")
	}
	return strings.Join(values, ","), len(items) - limit
}

func closureQueueReminderMessage(backlog ClosureQueueBacklog, stage, max int) string {
	candidates, omitted := boundedClosureQueueCases(backlog.Items)
	omittedText := ""
	if omitted > 0 {
		omittedText = fmt.Sprintf("，另有%d项将在后续后序批次处理", omitted)
	}
	message := fmt.Sprintf("HQ销账守卫%d/%d：发现%d个已验收且当前满足后序关闭前置的case。本轮按列出顺序逐项核验至多%d项：候选=%s%s。对每个CASE分别运行 hq case show --id CASE 与 hq history --case CASE；仅在该项关闭依据成立时运行 hq close --case CASE --reason TEXT --source PATH，然后继续下一项。处理完本批或遇到不能关闭的项即停止；不得关闭open/blocked/needs_decision，也不得把本提醒当作关闭批准。",
		stage, max, len(backlog.Items), min(len(backlog.Items), closureQueuePromptItemLimit), candidates, omittedText)
	if _, err := validateBusinessText("message", message, true); err == nil {
		return message
	}
	item := backlog.Items[0]
	return fmt.Sprintf("HQ销账守卫%d/%d：有%d个已验收case满足后序关闭前置。先运行 hq case show --id %s 与 hq history --case %s；依据成立时运行 hq close --case %s --reason TEXT --source PATH。不得自动关闭或代替业务判断。",
		stage, max, len(backlog.Items), item.CaseID, item.CaseID, item.CaseID)
}

func closureQueueDedupe(closer, basis string, stage int) string {
	return strings.Join([]string{"closure-queue", closer, basis, "nudge", strconv.Itoa(stage)}, ":")
}

func (a *App) runClosureQueueWatchdogOnce(ctx context.Context) error {
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
	backlog, err := ledger.closureQueueBacklog(a.Config)
	if err != nil || backlog == nil {
		return err
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	status, statusErr := liveQueueTargetStatus(snapshot, a.Config, a.HQRoot, backlog.Closer)
	if statusErr != nil || (status != "idle" && status != "done") {
		return nil
	}
	selectedAt, err := parseOperationsTime("closure queue selected_at", backlog.SelectedAt)
	if err != nil {
		return err
	}
	now := a.operationsNow()
	stallAfter, _, maxNudges := a.Config.managerQueueWatchdogPolicy()
	if now.Sub(selectedAt) < stallAfter {
		return nil
	}
	lastAt := selectedAt
	completedNudges := 0
	for stage := 1; stage <= maxNudges; stage++ {
		dedupe := closureQueueDedupe(backlog.Closer, backlog.BasisEventID, stage)
		record := ledgerNudgeByDedupe(ledger, dedupe)
		if record == nil {
			break
		}
		completedNudges = stage
		lastAt, _ = parseOperationsTime("closure queue nudge.at", record.Origin.At)
		if record.State == "queued" || record.State == "claimed" {
			return a.driveQueueNudge(ctx, record.Origin.NudgeID, dedupe, record.Origin.Recipient, record.Origin.Message, false, false)
		}
		if record.State == "attempted" || record.State == "unknown" {
			return nil
		}
	}
	if completedNudges >= maxNudges || (completedNudges > 0 && now.Sub(lastAt) < stallAfter) {
		return nil
	}
	stage := completedNudges + 1
	dedupe := closureQueueDedupe(backlog.Closer, backlog.BasisEventID, stage)
	id := stableCommandID("closure-queue-nudge", dedupe)
	return a.driveQueueNudge(ctx, id, dedupe, backlog.Closer, closureQueueReminderMessage(*backlog, stage, maxNudges), true, false)
}
