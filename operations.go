package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	nudgeSystemCaseID = "HQ-NUDGE"
	estopSystemCaseID = "HQ-ESTOP"
	minimumNudgeTTL   = 30 * time.Second
	maximumNudgeTTL   = 24 * time.Hour
	minimumClaimLease = 5 * time.Second
	maximumClaimLease = 5 * time.Minute
)

type nudgeLedgerRecord struct {
	Origin   Event
	Claim    Event
	Attempt  Event
	Terminal Event
	State    string
}

type reminderLedgerRecord struct {
	Created  Event
	Resolved Event
}

type NudgeView struct {
	Version        int    `json:"version"`
	NudgeID        string `json:"nudge_id"`
	DedupeKey      string `json:"dedupe_key"`
	Actor          string `json:"actor"`
	Recipient      string `json:"recipient"`
	Message        string `json:"message"`
	CreatedAt      string `json:"created_at"`
	ExpiresAt      string `json:"expires_at"`
	State          string `json:"state"`
	ClaimID        string `json:"claim_id,omitempty"`
	ClaimExpiresAt string `json:"claim_expires_at,omitempty"`
	DeliveryID     string `json:"delivery_id"`
	AttemptEventID string `json:"attempt_event_id,omitempty"`
	TerminalEvent  string `json:"terminal_event_id,omitempty"`
	ReminderID     string `json:"reminder_id,omitempty"`
	BasisEventID   string `json:"basis_last_event_id,omitempty"`
}

type ReminderScanResult struct {
	Version  int      `json:"version"`
	Created  []string `json:"created"`
	Resolved []string `json:"resolved"`
	Skipped  int      `json:"skipped"`
}

func isNudgeEvent(eventType string) bool {
	switch eventType {
	case "nudge_enqueued", "nudge_claimed", "nudge_claim_released", "nudge_delivery_attempted",
		"nudge_delivered", "nudge_failed", "nudge_unknown", "nudge_expired",
		"nudge_reconciled_delivered", "nudge_reconciled_not_run", "nudge_cancelled",
		"reminder_created", "reminder_resolved":
		return true
	default:
		return false
	}
}

func isInfrastructureEvent(eventType string) bool {
	return isNudgeEvent(eventType) || isEstopEvent(eventType) || isAssignmentActivationEvent(eventType)
}

func validateInfrastructureEventFields(event Event) error {
	if isAssignmentActivationEvent(event.Type) {
		return validateAssignmentActivationRequiredFields(event)
	}
	require := func(fields ...struct{ name, value string }) error { return requireEventFields(event, fields...) }
	switch event.Type {
	case "nudge_enqueued":
		return require(eventField("nudge_id", event.NudgeID), eventField("dedupe_key", event.DedupeKey),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("message", event.Message), eventField("expires_at", event.ExpiresAt), eventField("delivery_id", event.DeliveryID))
	case "reminder_created":
		return require(eventField("nudge_id", event.NudgeID), eventField("dedupe_key", event.DedupeKey),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("message", event.Message), eventField("expires_at", event.ExpiresAt), eventField("delivery_id", event.DeliveryID),
			eventField("reminder_id", event.ReminderID), eventField("basis_event_id", event.BasisEventID))
	case "nudge_claimed":
		return require(eventField("nudge_id", event.NudgeID), eventField("related_event_id", event.RelatedEventID),
			eventField("claim_id", event.ClaimID), eventField("claim_expires_at", event.ClaimExpiresAt), eventField("delivery_id", event.DeliveryID))
	case "nudge_claim_released", "nudge_delivery_attempted", "nudge_delivered", "nudge_failed", "nudge_unknown":
		return require(eventField("nudge_id", event.NudgeID), eventField("related_event_id", event.RelatedEventID),
			eventField("claim_id", event.ClaimID), eventField("delivery_id", event.DeliveryID))
	case "nudge_expired", "nudge_cancelled":
		return require(eventField("nudge_id", event.NudgeID), eventField("related_event_id", event.RelatedEventID), eventField("delivery_id", event.DeliveryID))
	case "nudge_reconciled_delivered", "nudge_reconciled_not_run":
		return require(eventField("nudge_id", event.NudgeID), eventField("related_event_id", event.RelatedEventID),
			eventField("delivery_id", event.DeliveryID), eventField("resolution_ref", event.ResolutionRef), eventField("note", event.Note))
	case "reminder_resolved":
		return require(eventField("nudge_id", event.NudgeID), eventField("reminder_id", event.ReminderID),
			eventField("basis_event_id", event.BasisEventID), eventField("related_event_id", event.RelatedEventID), eventField("delivery_id", event.DeliveryID))
	default:
		return validateEstopEventFields(event)
	}
}

func parseOperationsTime(label, value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s 不是 RFC3339：%w", label, err)
	}
	return parsed, nil
}

func validateNudgeIdentity(event Event) error {
	for label, value := range map[string]string{
		"nudge_id": event.NudgeID, "dedupe_key": event.DedupeKey, "delivery_id": event.DeliveryID,
	} {
		if value != "" {
			if err := validateLedgerID(label, value); err != nil {
				return err
			}
		}
	}
	if event.ClaimID != "" {
		if err := validateLedgerID("claim_id", event.ClaimID); err != nil {
			return err
		}
	}
	if event.ReminderID != "" {
		if err := validateLedgerID("reminder_id", event.ReminderID); err != nil {
			return err
		}
	}
	return nil
}

func nudgeStateActive(state string) bool {
	switch state {
	case "queued", "claimed", "attempted", "unknown":
		return true
	default:
		return false
	}
}

func (s *ledgerState) applyNudgeEvent(event Event, cfg Config) error {
	if err := requireNoStateFields(event); err != nil {
		return err
	}
	if err := validateNudgeIdentity(event); err != nil {
		return err
	}
	actor, actorExact := configRuleIncludingDisabled(cfg, event.Actor)
	if !actorExact || !actor.CanIssue {
		return fmt.Errorf("nudge/reminder actor %s 必须是精确登记且具备 can_issue", event.Actor)
	}
	now, err := parseOperationsTime("event.at", event.At)
	if err != nil {
		return err
	}
	switch event.Type {
	case "nudge_enqueued", "reminder_created":
		if _, exists := s.nudges[event.NudgeID]; exists {
			return fmt.Errorf("nudge_id 重复：%s", event.NudgeID)
		}
		if event.RelatedEventID != "" || event.ClaimID != "" || event.ClaimExpiresAt != "" || event.ResolutionRef != "" {
			return fmt.Errorf("%s 含非法关联/claim/resolution 字段", event.Type)
		}
		recipient, ok := configRuleIncludingDisabled(cfg, event.Recipient)
		if !ok || !cfg.isManager(recipient) {
			return fmt.Errorf("nudge recipient %s 必须是精确登记常驻经理", event.Recipient)
		}
		expires, err := parseOperationsTime("expires_at", event.ExpiresAt)
		if err != nil || !expires.After(now) {
			return fmt.Errorf("nudge expires_at 必须晚于 created time")
		}
		for _, record := range s.nudges {
			if record.Origin.DedupeKey == event.DedupeKey && nudgeStateActive(record.State) {
				return fmt.Errorf("dedupe_key 已有 active nudge：%s", record.Origin.NudgeID)
			}
		}
		if event.Type == "reminder_created" {
			state, err := s.currentCase(event.CaseID)
			if err != nil {
				return err
			}
			if state.Status == string(statusClosed) || state.Owner == "" || state.LastEventID != event.BasisEventID || state.Owner != event.Recipient {
				return fmt.Errorf("reminder basis/owner 已变化或事项已关闭")
			}
			if existing := s.reminderCases[event.CaseID]; existing != "" {
				return fmt.Errorf("case %s 生命周期已生成 reminder %s，拒绝循环催办", event.CaseID, existing)
			}
			s.reminders[event.ReminderID] = &reminderLedgerRecord{Created: event}
			s.reminderCases[event.CaseID] = event.ReminderID
		}
		s.nudges[event.NudgeID] = &nudgeLedgerRecord{Origin: event, State: "queued"}

	case "nudge_claimed":
		record, err := s.matchNudgeEvent(event)
		if err != nil {
			return err
		}
		if event.RelatedEventID != record.Origin.ID {
			return fmt.Errorf("nudge claim 未引用 enqueue event")
		}
		expires, _ := parseOperationsTime("expires_at", record.Origin.ExpiresAt)
		claimExpires, err := parseOperationsTime("claim_expires_at", event.ClaimExpiresAt)
		if err != nil || !claimExpires.After(now) || claimExpires.After(expires) || !now.Before(expires) {
			return fmt.Errorf("claim lease 必须位于 nudge TTL 内")
		}
		if record.State == "claimed" {
			priorExpiry, parseErr := parseOperationsTime("prior claim_expires_at", record.Claim.ClaimExpiresAt)
			if parseErr != nil || now.Before(priorExpiry) {
				return fmt.Errorf("nudge %s 已由 claim=%s 持有至 %s", event.NudgeID, record.Claim.ClaimID, record.Claim.ClaimExpiresAt)
			}
		} else if record.State != "queued" {
			return fmt.Errorf("nudge %s 当前 state=%s，不可 claim", event.NudgeID, record.State)
		}
		record.Claim, record.Attempt, record.State = event, Event{}, "claimed"

	case "nudge_claim_released":
		record, err := s.matchClaimEvent(event)
		if err != nil {
			return err
		}
		if record.State != "claimed" {
			return fmt.Errorf("nudge %s 当前 state=%s，不可 release claim", event.NudgeID, record.State)
		}
		if event.RelatedEventID != record.Claim.ID {
			return fmt.Errorf("claim release 未引用当前 claim event")
		}
		record.Claim, record.State = Event{}, "queued"

	case "nudge_expired":
		record, err := s.matchNudgeEvent(event)
		if err != nil {
			return err
		}
		expires, _ := parseOperationsTime("expires_at", record.Origin.ExpiresAt)
		if now.Before(expires) || (record.State != "queued" && record.State != "claimed") {
			return fmt.Errorf("nudge %s 尚未可安全过期，state=%s", event.NudgeID, record.State)
		}
		if event.RelatedEventID != record.Origin.ID {
			return fmt.Errorf("nudge expired 未引用 enqueue event")
		}
		record.Terminal, record.State = event, "expired"

	case "nudge_delivery_attempted":
		record, err := s.matchClaimEvent(event)
		if err != nil {
			return err
		}
		claimExpires, _ := parseOperationsTime("claim_expires_at", record.Claim.ClaimExpiresAt)
		expires, _ := parseOperationsTime("expires_at", record.Origin.ExpiresAt)
		if record.State != "claimed" || !now.Before(claimExpires) || !now.Before(expires) {
			return fmt.Errorf("nudge %s claim/TTL 已失效，禁止投递", event.NudgeID)
		}
		record.Attempt, record.State = event, "attempted"

	case "nudge_delivered", "nudge_failed", "nudge_unknown":
		record, err := s.matchClaimEvent(event)
		if err != nil {
			return err
		}
		if record.State != "attempted" || record.Attempt.ID != event.RelatedEventID {
			return fmt.Errorf("nudge %s 没有匹配的 attempted 事实", event.NudgeID)
		}
		state := map[string]string{"nudge_delivered": "delivered", "nudge_failed": "failed", "nudge_unknown": "unknown"}[event.Type]
		record.Terminal, record.State = event, state

	case "nudge_reconciled_delivered", "nudge_reconciled_not_run":
		record, err := s.matchNudgeEvent(event)
		if err != nil {
			return err
		}
		if record.State != "unknown" && record.State != "attempted" {
			return fmt.Errorf("nudge %s 当前 state=%s，不需人工 reconcile", event.NudgeID, record.State)
		}
		if event.RelatedEventID != record.Attempt.ID {
			return fmt.Errorf("nudge reconcile 未引用最后 attempted event")
		}
		state := "reconciled_delivered"
		if event.Type == "nudge_reconciled_not_run" {
			state = "reconciled_not_run"
		}
		record.Terminal, record.State = event, state

	case "nudge_cancelled":
		record, err := s.matchNudgeEvent(event)
		if err != nil {
			return err
		}
		if record.State != "queued" && record.State != "claimed" {
			return fmt.Errorf("nudge %s state=%s 不可自动取消", event.NudgeID, record.State)
		}
		record.Terminal, record.State = event, "cancelled"

	case "reminder_resolved":
		reminder := s.reminders[event.ReminderID]
		if reminder == nil || reminder.Created.NudgeID != event.NudgeID || reminder.Created.BasisEventID != event.BasisEventID || reminder.Created.ID != event.RelatedEventID {
			return fmt.Errorf("reminder_resolved 未匹配 created 事实")
		}
		if reminder.Resolved.ID != "" {
			return fmt.Errorf("reminder 已 resolved：%s", event.ReminderID)
		}
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if state.Status != string(statusClosed) && state.LastEventID == event.BasisEventID {
			return fmt.Errorf("case %s 尚无后续事件，不能撤销 reminder", event.CaseID)
		}
		reminder.Resolved = event
		if nudge := s.nudges[event.NudgeID]; nudge != nil && (nudge.State == "queued" || nudge.State == "claimed") {
			nudge.Terminal, nudge.State = event, "cancelled"
		}
	}
	return nil
}

func (s *ledgerState) matchNudgeEvent(event Event) (*nudgeLedgerRecord, error) {
	record := s.nudges[event.NudgeID]
	if record == nil {
		return nil, fmt.Errorf("nudge 未登记：%s", event.NudgeID)
	}
	if event.DeliveryID != record.Origin.DeliveryID || event.CaseID != record.Origin.CaseID {
		return nil, fmt.Errorf("nudge %s 的 case/delivery 关联冲突", event.NudgeID)
	}
	return record, nil
}

func (s *ledgerState) matchClaimEvent(event Event) (*nudgeLedgerRecord, error) {
	record, err := s.matchNudgeEvent(event)
	if err != nil {
		return nil, err
	}
	if record.Claim.ID == "" || event.ClaimID != record.Claim.ClaimID {
		return nil, fmt.Errorf("nudge %s claim 不匹配", event.NudgeID)
	}
	return record, nil
}

func (r *nudgeLedgerRecord) view() NudgeView {
	view := NudgeView{
		Version: eventSchemaVersion, NudgeID: r.Origin.NudgeID, DedupeKey: r.Origin.DedupeKey,
		Actor: r.Origin.Actor, Recipient: r.Origin.Recipient, Message: r.Origin.Message,
		CreatedAt: r.Origin.At, ExpiresAt: r.Origin.ExpiresAt, State: r.State,
		DeliveryID: r.Origin.DeliveryID, ReminderID: r.Origin.ReminderID, BasisEventID: r.Origin.BasisEventID,
	}
	view.ClaimID, view.ClaimExpiresAt = r.Claim.ClaimID, r.Claim.ClaimExpiresAt
	view.AttemptEventID, view.TerminalEvent = r.Attempt.ID, r.Terminal.ID
	return view
}

func (a *App) operationsNow() time.Time {
	if a.Clock != nil {
		return a.Clock().UTC()
	}
	if a.Store != nil {
		return a.Store.NowTime().UTC()
	}
	return time.Now().UTC()
}

func (a *App) newOperationsEvent(actor Actor, eventType, caseID string) (Event, error) {
	event, err := a.newEvent(actor, eventType, caseID)
	if err == nil {
		event.At = a.operationsNow().Format(time.RFC3339)
	}
	return event, err
}

func (a *App) nudgeActor() (Actor, error) {
	actor, err := a.actor()
	if err != nil {
		return Actor{}, err
	}
	rule, ok := a.Config.exactRule(actor.Name)
	if !ok || !rule.CanIssue {
		return Actor{}, permissionf("当前 actor %s 必须精确登记且具备 can_issue", actor.Name)
	}
	actor.Rule = rule
	return actor, nil
}

func (a *App) cmdNudge(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq nudge enqueue|claim|deliver|reconcile|status")
	}
	switch args[0] {
	case "enqueue":
		return a.cmdNudgeEnqueue(args[1:])
	case "claim":
		return a.cmdNudgeClaim(args[1:])
	case "deliver":
		return a.cmdNudgeDeliver(args[1:])
	case "reconcile":
		return a.cmdNudgeReconcile(args[1:])
	case "status":
		return a.cmdNudgeStatus(args[1:])
	default:
		return fmt.Errorf("未知 nudge 子命令 %q", args[0])
	}
}

func (a *App) cmdNudgeEnqueue(args []string) error {
	fs := newLeafParser("nudge enqueue")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "稳定 nudge id")
	dedupe := fs.String("dedupe", "", "未终结期间唯一 dedupe key")
	target := fs.String("to", "", "精确登记常驻经理")
	message := fs.String("message", "", "单行短提醒（≤200 rune）")
	ttl := fs.Duration("ttl", 15*time.Minute, "TTL（30s..24h）")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq nudge enqueue --id ID --dedupe KEY --to MANAGER --message TEXT [--ttl 15m]")
	}
	actor, err := a.nudgeActor()
	if err != nil {
		return err
	}
	cleanMessage, err := validateShortText("message", *message, true)
	if err != nil {
		return err
	}
	cleanID, cleanDedupe := strings.TrimSpace(*id), strings.TrimSpace(*dedupe)
	if err := validateLedgerID("nudge_id", cleanID); err != nil {
		return err
	}
	if err := validateLedgerID("dedupe_key", cleanDedupe); err != nil {
		return err
	}
	if *ttl < minimumNudgeTTL || *ttl > maximumNudgeTTL {
		return fmt.Errorf("--ttl 必须在 %s..%s", minimumNudgeTTL, maximumNudgeTTL)
	}
	rule, ok := a.Config.exactRule(strings.TrimSpace(*target))
	if !ok || !a.Config.isManager(rule) {
		return fmt.Errorf("--to 必须是精确登记、在职常驻经理")
	}
	now := a.operationsNow()
	commandID := stableCommandID("nudge-enqueue", cleanID)
	digest := requestDigest("nudge-enqueue", cleanID, cleanDedupe, actor.Name, rule.Name, cleanMessage, ttl.String())
	result, err := a.Store.Transact(a.Config, commandID, digest, a.DryRun, func(*ledgerState) (Event, error) {
		event, err := a.newOperationsEvent(actor, "nudge_enqueued", nudgeSystemCaseID)
		if err != nil {
			return Event{}, err
		}
		event.NudgeID, event.DedupeKey, event.Message = cleanID, cleanDedupe, cleanMessage
		event.Recipient, event.RecipientLabel = rule.Name, rule.Label
		event.ExpiresAt = now.Add(*ttl).Format(time.RFC3339)
		event.DeliveryID = stableDeliveryID(commandID, rule.Name)
		return event, nil
	})
	if err != nil {
		return err
	}
	return a.output(NudgeView{Version: eventSchemaVersion, NudgeID: result.Event.NudgeID, DedupeKey: result.Event.DedupeKey,
		Actor: result.Event.Actor, Recipient: result.Event.Recipient, Message: result.Event.Message, CreatedAt: result.Event.At,
		ExpiresAt: result.Event.ExpiresAt, State: "queued", DeliveryID: result.Event.DeliveryID}, "nudge 已排队；仅在 claim 后通过 Herdr Prompt 于回合边界投递")
}

func (a *App) cmdNudgeClaim(args []string) error {
	fs := newLeafParser("nudge claim")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "nudge id")
	claim := fs.String("claim", "", "稳定 claim id")
	lease := fs.Duration("lease", 30*time.Second, "claim lease（5s..5m）")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq nudge claim --id ID --claim CLAIM [--lease 30s]")
	}
	actor, err := a.nudgeActor()
	if err != nil {
		return err
	}
	cleanID, cleanClaim := strings.TrimSpace(*id), strings.TrimSpace(*claim)
	if err := validateLedgerID("nudge_id", cleanID); err != nil {
		return err
	}
	if err := validateLedgerID("claim_id", cleanClaim); err != nil {
		return err
	}
	if *lease < minimumClaimLease || *lease > maximumClaimLease {
		return fmt.Errorf("--lease 必须在 %s..%s", minimumClaimLease, maximumClaimLease)
	}
	now := a.operationsNow()
	commandID := stableCommandID("nudge-claim", cleanID, cleanClaim)
	digest := requestDigest("nudge-claim", cleanID, cleanClaim, lease.String())
	result, err := a.Store.Transact(a.Config, commandID, digest, a.DryRun, func(ledger *ledgerState) (Event, error) {
		record := ledger.nudges[cleanID]
		if record == nil {
			return Event{}, fmt.Errorf("nudge 未登记：%s", cleanID)
		}
		expires, err := parseOperationsTime("expires_at", record.Origin.ExpiresAt)
		if err != nil {
			return Event{}, err
		}
		eventType := "nudge_claimed"
		if !now.Before(expires) {
			eventType = "nudge_expired"
		}
		event, err := a.newOperationsEvent(actor, eventType, record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		event.NudgeID, event.RelatedEventID, event.DeliveryID = record.Origin.NudgeID, record.Origin.ID, record.Origin.DeliveryID
		if eventType == "nudge_claimed" {
			event.ClaimID = cleanClaim
			claimUntil := now.Add(*lease)
			if claimUntil.After(expires) {
				claimUntil = expires
			}
			event.ClaimExpiresAt = claimUntil.Format(time.RFC3339)
		}
		return event, nil
	})
	if err != nil {
		return err
	}
	if result.DryRun {
		return a.output(result.Event, "DRY-RUN：nudge claim/expire 仅校验，未写账本")
	}
	return a.cmdNudgeStatus([]string{"--id", result.Event.NudgeID})
}

func (a *App) readNudge(id string) (NudgeView, error) {
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return NudgeView{}, err
	}
	ledger, err := validateLedger(events, a.Config)
	if err != nil {
		return NudgeView{}, err
	}
	record := ledger.nudges[id]
	if record == nil {
		return NudgeView{}, fmt.Errorf("nudge 未登记：%s", id)
	}
	return record.view(), nil
}

func (a *App) cmdNudgeStatus(args []string) error {
	fs := newLeafParser("nudge status")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "nudge id")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq nudge status --id ID")
	}
	view, err := a.readNudge(strings.TrimSpace(*id))
	if err != nil {
		return err
	}
	return a.output(view, fmt.Sprintf("nudge=%s state=%s recipient=%s", view.NudgeID, view.State, view.Recipient))
}

func exactWorkingManager(snapshot HerdrSnapshot, cfg Config, hqRoot, recipient string) error {
	rule, ok := cfg.exactRule(recipient)
	if !ok || !cfg.isManager(rule) {
		return fmt.Errorf("recipient %s 不再是精确登记在职经理", recipient)
	}
	var workspaceIDs []string
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == cfg.WorkspaceLabel {
			workspaceIDs = append(workspaceIDs, workspace.ID)
		}
	}
	if len(workspaceIDs) != 1 {
		return fmt.Errorf("HQ workspace 精确匹配数=%d", len(workspaceIDs))
	}
	if matched, mismatch := exactLiveMatch(snapshot, workspaceIDs[0], rule, hqRoot); !matched {
		return fmt.Errorf("recipient %s 不满足精确在岗合同：%s", recipient, mismatch)
	}
	for _, agent := range snapshot.Agents {
		if agent.Name == recipient && agent.WorkspaceID == workspaceIDs[0] {
			if agent.Status != "working" {
				return fmt.Errorf("recipient %s 当前 status=%s；仅 working 经理使用回合边界 nudge", recipient, agent.Status)
			}
			return nil
		}
	}
	return fmt.Errorf("recipient %s 未在 HQ workspace", recipient)
}

func nudgeEnvelope(view NudgeView) string {
	return fmt.Sprintf("[HQ notification] HQ_NUDGE_V1 id=%s claim=%s expires=%s\n%s", view.NudgeID, view.ClaimID, view.ExpiresAt, view.Message)
}

func (a *App) cmdNudgeDeliver(args []string) error {
	fs := newLeafParser("nudge deliver")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "nudge id")
	claim := fs.String("claim", "", "当前 claim id")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq nudge deliver --id ID --claim CLAIM")
	}
	actor, err := a.nudgeActor()
	if err != nil {
		return err
	}
	if a.Herdr == nil {
		return fmt.Errorf("Herdr control 未注入，拒绝 PATH 回落")
	}
	cleanID, cleanClaim := strings.TrimSpace(*id), strings.TrimSpace(*claim)
	if err := validateLedgerID("nudge_id", cleanID); err != nil {
		return err
	}
	if err := validateLedgerID("claim_id", cleanClaim); err != nil {
		return err
	}
	releaseOperation, err := a.lockOperation(operationScopeNudge, cleanID)
	if err != nil {
		return err
	}
	defer releaseOperation()
	view, err := a.readNudge(cleanID)
	if err != nil {
		return err
	}
	if view.ClaimID == cleanClaim && (view.State == "delivered" || view.State == "failed" || view.State == "unknown" || strings.HasPrefix(view.State, "reconciled_")) {
		return a.output(view, "nudge 已有终态；重复命令幂等返回，未调用 Prompt")
	}
	if view.ClaimID == cleanClaim && view.State == "attempted" {
		return fmt.Errorf("nudge attempt 已落账但无可证明终态；禁止自动再投，须人工 reconcile")
	}
	if view.State != "claimed" || view.ClaimID != cleanClaim {
		return fmt.Errorf("nudge=%s state=%s claim=%s，不可投递", view.NudgeID, view.State, view.ClaimID)
	}
	now := a.operationsNow()
	expires, _ := parseOperationsTime("expires_at", view.ExpiresAt)
	claimExpires, _ := parseOperationsTime("claim_expires_at", view.ClaimExpiresAt)
	if !now.Before(expires) || !now.Before(claimExpires) {
		return fmt.Errorf("nudge TTL 或 claim lease 已过期；先重新 claim/expire")
	}
	releaseRuntimeSeat, err := a.lockRuntimeSeat(view.Recipient)
	if err != nil {
		return err
	}
	defer releaseRuntimeSeat()
	_, err = a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: view.Recipient}, func() error {
		return a.deliverClaimedNudgeAdmitted(actor, view)
	})
	return err
}

func (a *App) deliverClaimedNudgeAdmitted(actor Actor, view NudgeView) error {
	initial, err := a.herdrSnapshot(a.requestContext())
	if err != nil {
		return fmt.Errorf("投递前 Herdr snapshot：%w", err)
	}
	if err := exactWorkingManager(initial, a.Config, a.HQRoot, view.Recipient); err != nil {
		return fmt.Errorf("投递前 live binding 核验失败：%w", err)
	}
	commandID := stableCommandID("nudge-attempt", view.NudgeID, view.ClaimID)
	digest := requestDigest("nudge-attempt", view.NudgeID, view.ClaimID, view.DeliveryID)
	attempt, err := a.Store.Transact(a.Config, commandID, digest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.nudges[view.NudgeID]
		if record == nil {
			return Event{}, fmt.Errorf("nudge 未登记")
		}
		event, err := a.newOperationsEvent(actor, "nudge_delivery_attempted", record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		event.NudgeID, event.ClaimID, event.DeliveryID = view.NudgeID, view.ClaimID, view.DeliveryID
		event.RelatedEventID = record.Claim.ID
		return event, nil
	})
	if err != nil {
		return err
	}
	if attempt.AlreadyCommitted {
		fresh, readErr := a.readNudge(view.NudgeID)
		if readErr != nil {
			return readErr
		}
		if fresh.State == "delivered" || fresh.State == "failed" || fresh.State == "unknown" || strings.HasPrefix(fresh.State, "reconciled_") {
			return a.output(fresh, "nudge attempt 已有终态；未重复 Prompt")
		}
		return fmt.Errorf("nudge attempt 已落账但无可证明终态；禁止自动再投，须人工 reconcile")
	}
	if a.NudgeFailpoint != nil {
		if err := a.NudgeFailpoint("after_attempt_recorded"); err != nil {
			return fmt.Errorf("nudge attempt 已落账；禁止重投，须人工 reconcile：%w", err)
		}
	}
	final, bindingErr := a.herdrSnapshot(a.requestContext())
	if bindingErr == nil {
		bindingErr = exactWorkingManager(final, a.Config, a.HQRoot, view.Recipient)
	}
	if bindingErr != nil {
		fresh, terminalErr := a.appendNudgeTerminal(actor, view, attempt.Event, "nudge_failed", truncateError(bindingErr))
		if terminalErr != nil {
			return fmt.Errorf("最终 live binding 核验失败=%v，且 failed 终态落账失败；禁止重投，须人工 reconcile：%w", bindingErr, terminalErr)
		}
		return fmt.Errorf("最终 live binding 核验失败，未调用 Prompt；nudge=%s state=%s：%w", fresh.NudgeID, fresh.State, bindingErr)
	}
	mutation := a.Herdr.Prompt(a.requestContext(), view.Recipient, nudgeEnvelope(view))
	if a.NudgeFailpoint != nil {
		if err := a.NudgeFailpoint("after_prompt_before_result"); err != nil {
			return fmt.Errorf("Prompt 结果落账前崩溃窗口；禁止重投，须人工 reconcile：%w", err)
		}
	}
	eventType := "nudge_unknown"
	switch mutation.Outcome {
	case herdrConfirmed:
		eventType = "nudge_delivered"
	case herdrDefinitelyNotRun:
		eventType = "nudge_failed"
	case herdrAmbiguous:
		eventType = "nudge_unknown"
	default:
		eventType = "nudge_unknown"
	}
	fresh, terminalErr := a.appendNudgeTerminal(actor, view, attempt.Event, eventType, truncateError(mutation.Err))
	if terminalErr != nil {
		return fmt.Errorf("Herdr outcome=%s，但终态落账失败；禁止重投，须人工 reconcile：%w", mutation.Outcome, terminalErr)
	}
	if fresh.State == "unknown" {
		return fmt.Errorf("nudge 投递结果 ambiguous；state=unknown，禁止自动重投")
	}
	return a.output(fresh, fmt.Sprintf("nudge=%s state=%s", fresh.NudgeID, fresh.State))
}

func (a *App) appendNudgeTerminal(actor Actor, view NudgeView, attempt Event, eventType, note string) (NudgeView, error) {
	terminalCommand := stableCommandID("nudge-terminal", attempt.ID, eventType)
	terminalDigest := requestDigest("nudge-terminal", view.NudgeID, attempt.ID, eventType)
	_, terminalErr := a.Store.Transact(a.Config, terminalCommand, terminalDigest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.nudges[view.NudgeID]
		event, err := a.newOperationsEvent(actor, eventType, record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		event.NudgeID, event.ClaimID, event.DeliveryID = view.NudgeID, view.ClaimID, view.DeliveryID
		event.RelatedEventID = attempt.ID
		event.Note = note
		return event, nil
	})
	if terminalErr != nil {
		return NudgeView{}, terminalErr
	}
	fresh, err := a.readNudge(view.NudgeID)
	if err != nil {
		return NudgeView{}, err
	}
	return fresh, nil
}

func (a *App) cmdNudgeReconcile(args []string) error {
	fs := newLeafParser("nudge reconcile")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "nudge id")
	resolution := fs.String("resolution", "", "delivered|not-run")
	ref := fs.String("ref", "", "人工核对证据原文")
	note := fs.String("note", "", "短核对说明")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq nudge reconcile --id ID --resolution delivered|not-run --ref PATH --note TEXT")
	}
	actor, err := a.nudgeActor()
	if err != nil {
		return err
	}
	cleanID := strings.TrimSpace(*id)
	if err := validateLedgerID("nudge_id", cleanID); err != nil {
		return err
	}
	cleanNote, err := validateShortText("note", *note, true)
	if err != nil {
		return err
	}
	cleanRef, err := normalizeRef(*ref, a.HQRoot, true)
	if err != nil {
		return err
	}
	eventType := "nudge_reconciled_delivered"
	if *resolution == "not-run" {
		eventType = "nudge_reconciled_not_run"
	} else if *resolution != "delivered" {
		return fmt.Errorf("--resolution 只能是 delivered|not-run")
	}
	releaseOperation, err := a.lockOperation(operationScopeNudge, cleanID)
	if err != nil {
		return err
	}
	defer releaseOperation()
	commandID := stableCommandID("nudge-reconcile", cleanID, *resolution)
	digest := requestDigest("nudge-reconcile", cleanID, *resolution, cleanRef, cleanNote)
	_, err = a.Store.Transact(a.Config, commandID, digest, a.DryRun, func(ledger *ledgerState) (Event, error) {
		record := ledger.nudges[cleanID]
		if record == nil || record.Attempt.ID == "" {
			return Event{}, fmt.Errorf("nudge 没有可 reconcile 的 attempt")
		}
		event, err := a.newOperationsEvent(actor, eventType, record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		event.NudgeID, event.DeliveryID, event.RelatedEventID = record.Origin.NudgeID, record.Origin.DeliveryID, record.Attempt.ID
		event.ResolutionRef, event.Note = cleanRef, cleanNote
		return event, nil
	})
	if err != nil {
		return err
	}
	return a.cmdNudgeStatus([]string{"--id", cleanID})
}

func reminderStableID(caseID, basis string) string {
	return stableCommandID("reminder", caseID, basis)
}

func isReminderOpenState(state *CaseState) bool {
	return state != nil && state.Status != string(statusClosed) && state.Owner != ""
}

func (a *App) cmdReminder(args []string) error {
	if len(args) == 0 || args[0] != "scan" {
		return fmt.Errorf("用法：hq reminder scan --after DURATION")
	}
	return a.cmdReminderScan(args[1:])
}

func (a *App) cmdReminderScan(args []string) error {
	fs := newLeafParser("reminder scan")
	fs.SetOutput(a.Err)
	after := fs.Duration("after", 24*time.Hour, "开口项超期阈值")
	ttl := fs.Duration("ttl", 15*time.Minute, "提醒 nudge TTL")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return fmt.Errorf("用法：hq reminder scan [--after 24h] [--ttl 15m]")
	}
	if *after <= 0 || *ttl < minimumNudgeTTL || *ttl > maximumNudgeTTL {
		return fmt.Errorf("--after 必须为正；--ttl 必须在 %s..%s", minimumNudgeTTL, maximumNudgeTTL)
	}
	actor, err := a.nudgeActor()
	if err != nil {
		return err
	}
	// One strict full replay occurs before the first write. Any physical or
	// semantic read error therefore produces zero reminders, never a partial scan.
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return fmt.Errorf("reminder scan 严格重放失败，零写：%w", err)
	}
	ledger, err := validateLedger(events, a.Config)
	if err != nil {
		return fmt.Errorf("reminder scan 严格重放失败，零写：%w", err)
	}
	now := a.operationsNow()
	type resolveAction struct{ reminderID, eventID, caseID, nudgeID, basis, deliveryID string }
	var resolves []resolveAction
	for id, reminder := range ledger.reminders {
		if reminder.Resolved.ID != "" {
			continue
		}
		state := ledger.snapshot.Cases[reminder.Created.CaseID]
		if state == nil {
			return fmt.Errorf("reminder %s 引用不存在 case，零新增", id)
		}
		if state.Status == string(statusClosed) || state.LastEventID != reminder.Created.BasisEventID {
			resolves = append(resolves, resolveAction{id, reminder.Created.ID, reminder.Created.CaseID, reminder.Created.NudgeID, reminder.Created.BasisEventID, reminder.Created.DeliveryID})
		}
	}
	type createAction struct{ caseID, owner, basis, message, reminderID, nudgeID string }
	var creates []createAction
	caseIDs := make([]string, 0, len(ledger.snapshot.Cases))
	for caseID := range ledger.snapshot.Cases {
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)
	skipped := 0
	for _, caseID := range caseIDs {
		state := ledger.snapshot.Cases[caseID]
		if !isReminderOpenState(state) || ledger.reminderCases[caseID] != "" {
			skipped++
			continue
		}
		owner, ok := a.Config.exactRule(state.Owner)
		if !ok || !a.Config.isManager(owner) {
			skipped++
			continue
		}
		updated, err := parseOperationsTime("case.updated_at", state.UpdatedAt)
		if err != nil {
			return fmt.Errorf("case %s updated_at 非法，零新增：%w", caseID, err)
		}
		if now.Sub(updated) < *after {
			skipped++
			continue
		}
		reminderID := reminderStableID(caseID, state.LastEventID)
		nudgeID := stableCommandID("reminder-nudge", caseID, state.LastEventID)
		message := fmt.Sprintf("CASE=%s 超期未处理；依据事件=%s；仅提醒一次，不自动关闭或拍板。", caseID, state.LastEventID)
		if _, err := validateShortText("message", message, true); err != nil {
			return err
		}
		creates = append(creates, createAction{caseID, owner.Name, state.LastEventID, message, reminderID, nudgeID})
	}
	result := ReminderScanResult{Version: 1, Created: []string{}, Resolved: []string{}, Skipped: skipped}
	for _, action := range resolves {
		commandID := stableCommandID("reminder-resolve", action.reminderID, ledger.snapshot.Cases[action.caseID].LastEventID)
		digest := requestDigest("reminder-resolve", action.reminderID, action.basis, ledger.snapshot.Cases[action.caseID].LastEventID)
		_, err := a.Store.Transact(a.Config, commandID, digest, a.DryRun, func(current *ledgerState) (Event, error) {
			reminder := current.reminders[action.reminderID]
			if reminder == nil {
				return Event{}, fmt.Errorf("reminder 消失：%s", action.reminderID)
			}
			event, err := a.newOperationsEvent(actor, "reminder_resolved", action.caseID)
			if err != nil {
				return Event{}, err
			}
			event.ReminderID, event.NudgeID, event.BasisEventID = action.reminderID, action.nudgeID, action.basis
			event.RelatedEventID, event.DeliveryID = action.eventID, action.deliveryID
			return event, nil
		})
		if err != nil {
			return err
		}
		result.Resolved = append(result.Resolved, action.reminderID)
	}
	for _, action := range creates {
		rule, _ := a.Config.exactRule(action.owner)
		commandID := stableCommandID("reminder-create", action.caseID, action.basis)
		digest := requestDigest("reminder-create", action.caseID, action.basis, action.owner, action.message, ttl.String())
		_, err := a.Store.Transact(a.Config, commandID, digest, a.DryRun, func(current *ledgerState) (Event, error) {
			state := current.snapshot.Cases[action.caseID]
			if !isReminderOpenState(state) || state.LastEventID != action.basis || state.Owner != action.owner {
				return Event{}, fmt.Errorf("case %s 在并发扫描中已变化，拒绝陈旧 reminder", action.caseID)
			}
			event, err := a.newOperationsEvent(actor, "reminder_created", action.caseID)
			if err != nil {
				return Event{}, err
			}
			event.ReminderID, event.NudgeID, event.BasisEventID = action.reminderID, action.nudgeID, action.basis
			event.DedupeKey = "reminder:" + action.caseID + ":" + action.basis
			event.Message, event.Recipient, event.RecipientLabel = action.message, rule.Name, rule.Label
			event.ExpiresAt = now.Add(*ttl).Format(time.RFC3339)
			event.DeliveryID = stableDeliveryID(commandID, rule.Name)
			return event, nil
		})
		if err != nil {
			return err
		}
		result.Created = append(result.Created, action.reminderID)
	}
	return a.output(result, fmt.Sprintf("reminder scan：created=%d resolved=%d skipped=%d；未关闭事项、未改变 owner/status、未生成批准", len(result.Created), len(result.Resolved), result.Skipped))
}
