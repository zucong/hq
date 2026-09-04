package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	deliveryModeAuto   = "auto"
	deliveryModeWakeup = "wakeup"
	deliveryModeQuiet  = "quiet"
	deliveryModeInject = "inject"

	deliveryTargetNextTurn = "next-turn"
	deliveryTargetNextStep = "next-step"

	messageUrgencyNormal = "normal"
	messageUrgencyUrgent = "urgent"

	defaultDeliveryBundleItems       = 8
	defaultDeliveryBundleBytes       = 16 * 1024
	defaultAssignmentAcceptTimeout   = 2 * time.Minute
	defaultMaxActivationRedeliveries = 2
	defaultManagerQueueStallTimeout  = 2 * time.Minute
	defaultManagerQueueEscalateAfter = 10 * time.Minute
	defaultMaxManagerQueueNudges     = 2
	maxDeliveryBundleItems           = 100
	maxDeliveryBundleBytes           = 1024 * 1024
	maxTurnBundleBaseBytes           = 64 * 1024
	maxTurnBundleEnvelopeBytes       = 8 * 1024
	turnBundleVersion                = 1
)

type deliveryRuntimeState string

const (
	deliveryRuntimeIdle    deliveryRuntimeState = "idle"
	deliveryRuntimeBusy    deliveryRuntimeState = "busy"
	deliveryRuntimeOffline deliveryRuntimeState = "offline"
)

type DeliveryBudgetView struct {
	Target               string `json:"target"`
	Spent                int    `json:"spent"`
	Limit                int    `json:"limit"`
	Remaining            int    `json:"remaining"`
	DefaultMode          string `json:"default_mode"`
	WouldDowngradeWakeup bool   `json:"would_downgrade_wakeup"`
}

type DeliveryContextItem struct {
	MessageID   string   `json:"message_id"`
	From        string   `json:"from"`
	Kind        string   `json:"kind"`
	Urgency     string   `json:"urgency"`
	Text        string   `json:"text"`
	CaseID      string   `json:"case_id,omitempty"`
	ThreadID    string   `json:"thread_id,omitempty"`
	ReplyTo     string   `json:"reply_to,omitempty"`
	RefFiles    []string `json:"ref_files,omitempty"`
	RefCases    []string `json:"ref_cases,omitempty"`
	RefMessages []string `json:"ref_messages,omitempty"`
	RefEvents   []string `json:"ref_events,omitempty"`
}

type deliveryContextRecord struct {
	Origin Event
	Status string
}

type deliveryContextBatch struct {
	Records        []deliveryContextRecord
	Items          []DeliveryContextItem
	Envelopes      []string
	PayloadDigests []string
	ItemBytes      []int
	Bytes          int
	Overflow       int
	MaxItems       int
	MaxBytes       int
	NextItemBytes  int
	NextDeliveryID string
	NextDigest     string
	NextEnvelope   string
}

func validDeliveryRequestMode(value string) bool {
	switch value {
	case deliveryModeAuto, deliveryModeWakeup, deliveryModeQuiet, deliveryModeInject:
		return true
	default:
		return false
	}
}

func validResolvedDeliveryMode(value string) bool {
	return value == deliveryModeWakeup || value == deliveryModeQuiet || value == deliveryModeInject
}

func deliveryModePrimitives(mode string) (target string, wakeup bool, err error) {
	switch mode {
	case deliveryModeWakeup:
		return deliveryTargetNextTurn, true, nil
	case deliveryModeQuiet:
		return deliveryTargetNextTurn, false, nil
	case deliveryModeInject:
		return deliveryTargetNextStep, false, nil
	default:
		return "", false, fmt.Errorf("未知投递档位 %q", mode)
	}
}

func effectiveEventDeliveryMode(event Event) string {
	if event.DeliveryMode == "" {
		return deliveryModeWakeup
	}
	return event.DeliveryMode
}

func effectiveEventDeliveryTarget(event Event) string {
	if event.DeliveryTarget != "" {
		return event.DeliveryTarget
	}
	target, _, _ := deliveryModePrimitives(effectiveEventDeliveryMode(event))
	return target
}

func eventWakesTarget(event Event) bool {
	_, wakeup, _ := deliveryModePrimitives(effectiveEventDeliveryMode(event))
	return wakeup
}

func validateDeliveryPrimitives(event Event) error {
	mode := effectiveEventDeliveryMode(event)
	if !validResolvedDeliveryMode(mode) {
		return fmt.Errorf("事件 %s 的 delivery_mode 非法：%q", event.Type, event.DeliveryMode)
	}
	target, _, _ := deliveryModePrimitives(mode)
	if event.DeliveryTarget != "" && event.DeliveryTarget != target {
		return fmt.Errorf("事件 %s 的 delivery_mode=%s 与 delivery_target=%s 不正交匹配", event.Type, mode, event.DeliveryTarget)
	}
	return nil
}

func (c Config) effectiveDeliveryPolicy() DeliveryPolicy {
	if c.DeliveryPolicy == nil {
		return DeliveryPolicy{DefaultMode: deliveryModeWakeup, MaxConsecutiveWakes: 3,
			MaxBundleItems: defaultDeliveryBundleItems, MaxBundleBytes: defaultDeliveryBundleBytes,
			AssignmentAcceptTimeout:   defaultAssignmentAcceptTimeout.String(),
			MaxActivationRedeliveries: defaultMaxActivationRedeliveries,
			ManagerQueueStallTimeout:  defaultManagerQueueStallTimeout.String(),
			ManagerQueueEscalateAfter: defaultManagerQueueEscalateAfter.String(),
			MaxManagerQueueNudges:     defaultMaxManagerQueueNudges}
	}
	policy := *c.DeliveryPolicy
	if policy.MaxBundleItems == 0 {
		policy.MaxBundleItems = defaultDeliveryBundleItems
	}
	if policy.MaxBundleBytes == 0 {
		policy.MaxBundleBytes = defaultDeliveryBundleBytes
	}
	if strings.TrimSpace(policy.AssignmentAcceptTimeout) == "" {
		policy.AssignmentAcceptTimeout = defaultAssignmentAcceptTimeout.String()
	}
	if policy.MaxActivationRedeliveries == 0 {
		policy.MaxActivationRedeliveries = defaultMaxActivationRedeliveries
	}
	if strings.TrimSpace(policy.ManagerQueueStallTimeout) == "" {
		policy.ManagerQueueStallTimeout = defaultManagerQueueStallTimeout.String()
	}
	if strings.TrimSpace(policy.ManagerQueueEscalateAfter) == "" {
		policy.ManagerQueueEscalateAfter = defaultManagerQueueEscalateAfter.String()
	}
	if policy.MaxManagerQueueNudges == 0 {
		policy.MaxManagerQueueNudges = defaultMaxManagerQueueNudges
	}
	return policy
}

func (c Config) managerQueueWatchdogPolicy() (time.Duration, time.Duration, int) {
	policy := c.effectiveDeliveryPolicy()
	stall, err := time.ParseDuration(policy.ManagerQueueStallTimeout)
	if err != nil {
		stall = defaultManagerQueueStallTimeout
	}
	escalate, err := time.ParseDuration(policy.ManagerQueueEscalateAfter)
	if err != nil {
		escalate = defaultManagerQueueEscalateAfter
	}
	return stall, escalate, policy.MaxManagerQueueNudges
}

func (c Config) assignmentAcceptTimeout() time.Duration {
	duration, err := time.ParseDuration(c.effectiveDeliveryPolicy().AssignmentAcceptTimeout)
	if err != nil {
		return defaultAssignmentAcceptTimeout
	}
	return duration
}

func messageNeedsAction(kind string) bool {
	return kind == "question" || kind == "request" || kind == "handoff" || kind == "directive"
}

func effectiveMessageUrgency(value string) string {
	if value == "" {
		return messageUrgencyNormal
	}
	return value
}

func validMessageUrgency(value string) bool {
	return value == messageUrgencyNormal || value == messageUrgencyUrgent
}

func selectMessageDelivery(requested, kind, urgency string, state deliveryRuntimeState, spent int, policy DeliveryPolicy) (string, string, error) {
	if !validDeliveryRequestMode(requested) {
		return "", "", fmt.Errorf("--delivery 只能是 auto|wakeup|quiet|inject")
	}
	urgency = effectiveMessageUrgency(urgency)
	if !validMessageUrgency(urgency) {
		return "", "", fmt.Errorf("--urgency 只能是 normal|urgent")
	}
	if urgency == messageUrgencyUrgent && kind != "directive" {
		return "", "", fmt.Errorf("--urgency urgent 只允许用于绑定 active assignment 的 directive")
	}
	if urgency == messageUrgencyUrgent {
		if requested == deliveryModeQuiet || requested == deliveryModeInject {
			return "", "", fmt.Errorf("urgent 消息必须在下一安全回合主动唤醒；--delivery 只能省略、auto 或 wakeup")
		}
		return deliveryModeWakeup, "urgent-next-turn", nil
	}
	mode, reason := requested, "caller-requested"
	if mode == deliveryModeAuto {
		switch {
		case state == deliveryRuntimeBusy:
			mode, reason = deliveryModeInject, "adaptive-busy"
		case messageNeedsAction(kind):
			mode, reason = deliveryModeWakeup, "adaptive-idle-action"
		default:
			mode, reason = deliveryModeQuiet, "adaptive-idle-info"
		}
	}
	if mode == deliveryModeWakeup && spent >= policy.MaxConsecutiveWakes {
		mode, reason = deliveryModeInject, "wake-budget-exhausted"
	}
	if _, _, err := deliveryModePrimitives(mode); err != nil {
		return "", "", err
	}
	return mode, reason, nil
}

func (a *App) inspectDeliveryTarget(target string) (deliveryRuntimeState, error) {
	if a.DeliveryTargetState != nil {
		return a.DeliveryTargetState(target)
	}
	if a.Herdr == nil {
		// Explicit fake transports used by synthetic test fixtures predate the
		// runtime-presence seam. Treat them as an online idle target.
		return deliveryRuntimeIdle, nil
	}
	snapshot, err := a.Herdr.Snapshot(a.requestContext(), HerdrSnapshotScope{WorkspaceLabel: a.Config.WorkspaceLabel})
	if err != nil {
		return "", fmt.Errorf("读取投递目标运行态：%w", err)
	}
	workspaceID := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			workspaceID = workspace.ID
			break
		}
	}
	if workspaceID == "" {
		return deliveryRuntimeOffline, nil
	}
	rule, ok := a.Config.exactRule(target)
	if !ok {
		return "", fmt.Errorf("目标未登记或已停用：%s", target)
	}
	matched, mismatch := exactLiveMatch(snapshot, workspaceID, rule, a.HQRoot)
	if !matched {
		if mismatch == "" {
			return deliveryRuntimeOffline, nil
		}
		return "", fmt.Errorf("目标 %s 不满足精确在岗合同：%s", target, mismatch)
	}
	for _, live := range snapshot.Agents {
		if live.Name == target && live.WorkspaceID == workspaceID {
			if live.Status == "working" || live.Status == "blocked" {
				return deliveryRuntimeBusy, nil
			}
			return deliveryRuntimeIdle, nil
		}
	}
	return "", fmt.Errorf("目标 %s 精确匹配后消失", target)
}

func (a *App) coldResumeDeliveryTarget(target string) error {
	releaseRuntimeSeat, err := a.lockRuntimeSeat(target)
	if err != nil {
		return err
	}
	defer releaseRuntimeSeat()
	if err := a.ensureRuntimeSeatOriginSafeLocked(target); err != nil {
		return err
	}
	_, err = a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentResume, Target: target}, func() error {
		return a.coldResumeDeliveryTargetAdmitted(target)
	})
	return err
}

func (a *App) coldResumeDeliveryTargetAdmitted(target string) error {
	return a.coldResumeDeliveryTargetAdmittedWithOptions(target, runtimeStartOptions{})
}

func (a *App) coldResumeDeliveryTargetAdmittedWithOptions(target string, options runtimeStartOptions) error {
	rule, ok := a.Config.exactRule(target)
	if !ok {
		return fmt.Errorf("cold-resume 目标未登记或已停用：%s", target)
	}
	if a.DeliveryColdResume != nil {
		if _, err := verifyAgentRoleCardArtifact(a.Config, a.HQRoot, rule); err != nil {
			return fmt.Errorf("cold-resume 目标 %s role card 复核失败：%w", target, err)
		}
		return a.DeliveryColdResume(target)
	}
	if a.Herdr == nil || a.Sessions == nil {
		return fmt.Errorf("目标 %s 离线且未注入 cold-resume 依赖", target)
	}
	if _, err := validateWorkstation(a.HQRoot, rule); err != nil {
		return err
	}
	lock, err := a.lockUpContext(a.requestContext())
	if err != nil {
		return err
	}
	defer unlock(lock)
	ctx := a.requestContext()
	// Delivery recovery is an agent-resume action, not authority to create the
	// company control plane. Requiring the registered workspace to exist keeps
	// an ESTOP-exempt manager resume from smuggling a CreateWorkspace mutation
	// through the narrower agent_resume admission.
	workspaceID, err := a.requireExistingHQWorkspace(ctx)
	if err != nil {
		return err
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	if matched, mismatch := exactLiveMatch(snapshot, workspaceID, rule, a.HQRoot); matched {
		return nil
	} else if mismatch != "" {
		return fmt.Errorf("cold-resume 目标存在但不满足精确在岗合同：%s", mismatch)
	}
	return a.startHQAgentAdmittedWithOptions(ctx, workspaceID, rule, options)
}

func (s *ledgerState) deliveryBudgetSpent(target string) int {
	return s.deliveryWakeSpends[target]
}

func (s *ledgerState) deliveryContextRecords(target string) []*deliveryRecord {
	var records []*deliveryRecord
	for _, record := range s.deliveries {
		// Only explicit messages are model context; business transitions never
		// become queued message envelopes.
		if record.Origin.Type != "message_prepared" || record.Origin.Recipient != target ||
			record.ContextState == deliveryContextClaimed || record.Ack.ID != "" {
			continue
		}
		if s.turnBundleReservations[record.Origin.DeliveryID] != "" {
			continue
		}
		if record.Status == deliveryQueued || (record.Status == deliverySent && record.ContextState == deliveryContextHistory) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Origin.Sequence < records[j].Origin.Sequence })
	return records
}

func (a *App) deliveryBudgetView(target string) (DeliveryBudgetView, error) {
	ledger, err := a.ledgerState()
	if err != nil {
		return DeliveryBudgetView{}, err
	}
	policy := a.Config.effectiveDeliveryPolicy()
	spent := ledger.deliveryBudgetSpent(target)
	remaining := policy.MaxConsecutiveWakes - spent
	if remaining < 0 {
		remaining = 0
	}
	return DeliveryBudgetView{Target: target, Spent: spent, Limit: policy.MaxConsecutiveWakes,
		Remaining: remaining, DefaultMode: policy.DefaultMode, WouldDowngradeWakeup: spent >= policy.MaxConsecutiveWakes}, nil
}

func (a *App) cmdDeliveryBudget(args []string) error {
	if len(args) == 0 || args[0] != "status" {
		return fmt.Errorf("用法：hq delivery budget status [--target AGENT]")
	}
	fs := newLeafParser("delivery budget status")
	fs.SetOutput(a.Err)
	target := fs.String("target", "", "目标 agent；默认当前角色")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	cleanTarget := strings.TrimSpace(*target)
	if cleanTarget == "" {
		cleanTarget = actor.Name
	}
	if _, ok := a.Config.exactRule(cleanTarget); !ok {
		return fmt.Errorf("预算目标未登记或已停用：%s", cleanTarget)
	}
	view, err := a.deliveryBudgetView(cleanTarget)
	if err != nil {
		return err
	}
	return a.output(view, fmt.Sprintf("target=%s wakes=%d/%d remaining=%d default=%s downgrade=%t",
		view.Target, view.Spent, view.Limit, view.Remaining, view.DefaultMode, view.WouldDowngradeWakeup))
}

func (a *App) cmdDeliveryConsume(args []string) error {
	fs := newLeafParser("delivery consume")
	fs.SetOutput(a.Err)
	limitText := fs.String("limit", "100", "本次人工恢复最多读取条数（1..100）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	limit, parseErr := strconv.Atoi(strings.TrimSpace(*limitText))
	if fs.NArg() != 0 || parseErr != nil || limit < 1 || limit > 100 {
		return usagef("用法：hq delivery consume [--limit 1..100]")
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if a.JSON {
		_, err = a.consumeDeliveryContextAtomically(actor, limit, "manual-json", func(batch deliveryContextBatch) error {
			return a.writeDeliveryContextJSON(batch.Items)
		})
		if err != nil {
			return err
		}
	} else {
		consumed := 0
		for consumed < limit {
			batch, consumeErr := a.consumeDeliveryContextAtomically(actor, 1, "manual-text", func(batch deliveryContextBatch) error {
				if len(batch.Envelopes) == 0 {
					return nil
				}
				return writeDeliveryContext(a.Out, []byte(batch.Envelopes[0]+"\n\n"))
			})
			if consumeErr != nil {
				return consumeErr
			}
			if len(batch.Items) == 0 {
				break
			}
			consumed++
		}
		if consumed == 0 {
			if err := writeDeliveryContext(a.Out, []byte("无静默消息\n")); err != nil {
				return err
			}
		}
	}
	if !a.DryRun {
		if err := a.resetDeliveryBudget(actor); err != nil {
			return err
		}
	}
	return nil
}

var errNoDeliveryContextToConsume = errors.New("no delivery context to consume")

// consumeDeliveryContextAtomically serializes context selection, output, and
// the durable promote/claim projection under one ledger lock. Output happens
// before the batch is appended, preserving retry-on-write/commit-failure
// behavior, while another process cannot reserve the same records for a turn
// bundle between output and claim.
func (a *App) consumeDeliveryContextAtomically(actor Actor, limit int, purpose string, render func(deliveryContextBatch) error) (deliveryContextBatch, error) {
	if render == nil {
		return deliveryContextBatch{}, fmt.Errorf("delivery context renderer 不能为空")
	}
	if a.DryRun {
		batch, err := a.prepareDeliveryContext(actor.Name, limit, false)
		if err != nil {
			return deliveryContextBatch{}, err
		}
		return batch, render(batch)
	}
	seed, err := a.ledgerState()
	if err != nil {
		return deliveryContextBatch{}, err
	}
	seedRecords := seed.deliveryContextRecords(actor.Name)
	if limit > 0 && len(seedRecords) > limit {
		seedRecords = seedRecords[:limit]
	}
	if len(seedRecords) == 0 {
		empty := newDeliveryContextBatch(0)
		return empty, render(empty)
	}
	seedHash := seed.snapshot.LastEventHash
	if seedHash == "" {
		seedHash = genesisEventHash
	}
	commandID := stableCommandID("delivery-context-consume", actor.Name, purpose, strconv.Itoa(limit), strconv.FormatBool(a.JSON), seedHash)
	digest := requestDigest("delivery-context-consume", actor.Name, purpose, strconv.Itoa(limit), strconv.FormatBool(a.JSON), seedHash)
	var rendered deliveryContextBatch
	result, err := a.transactBatch(commandID, digest, func(ledger *ledgerState) ([]Event, error) {
		records := ledger.deliveryContextRecords(actor.Name)
		if limit > 0 && len(records) > limit {
			records = records[:limit]
		}
		if len(records) == 0 {
			return nil, errNoDeliveryContextToConsume
		}
		batch := newDeliveryContextBatch(len(records))
		for _, record := range records {
			if err := a.appendDeliveryContextRecord(&batch, record); err != nil {
				return nil, err
			}
		}
		events, err := a.buildDeliveryContextConsumeEvents(ledger, actor, records)
		if err != nil {
			return nil, err
		}
		if err := render(batch); err != nil {
			return nil, err
		}
		rendered = batch
		return events, nil
	})
	if errors.Is(err, errNoDeliveryContextToConsume) || (err == nil && result.AlreadyCommitted) {
		empty := newDeliveryContextBatch(0)
		return empty, render(empty)
	}
	if err != nil {
		return deliveryContextBatch{}, err
	}
	return rendered, nil
}

func (a *App) buildDeliveryContextConsumeEvents(ledger *ledgerState, actor Actor, records []*deliveryRecord) ([]Event, error) {
	result := make([]Event, 0, len(records)*3)
	for _, candidate := range records {
		record := ledger.deliveries[candidate.Origin.DeliveryID]
		if record == nil || record.Origin.Recipient != actor.Name || record.Ack.ID != "" {
			return nil, fmt.Errorf("delivery context %s 已变化", candidate.Origin.DeliveryID)
		}
		if owner := ledger.turnBundleReservations[record.Origin.DeliveryID]; owner != "" {
			return nil, fmt.Errorf("delivery context %s 已由 turn bundle attempt=%s 保留", record.Origin.DeliveryID, owner)
		}
		switch record.Status {
		case deliveryQueued:
			attempt, err := a.newEvent(Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}, "delivery_attempted", record.Origin.CaseID)
			if err != nil {
				return nil, err
			}
			attempt.RelatedEventID, attempt.Delivery = record.Origin.ID, deliveryAttempted
			copyDeliveryEnvelope(&attempt, record.Origin)

			sent, err := a.newEvent(Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}, "message_sent", record.Origin.CaseID)
			if err != nil {
				return nil, err
			}
			sent.RelatedEventID, sent.AttemptEventID, sent.Delivery = record.Origin.ID, attempt.ID, deliverySent
			copyDeliveryEnvelope(&sent, record.Origin)
			a.fillSentState(&sent, record.Origin, ledger)
			result = append(result, attempt, sent)
		case deliverySent:
			if record.ContextState != deliveryContextHistory {
				return nil, fmt.Errorf("delivery context %s state=%s 不可消费", record.Origin.DeliveryID, record.ContextState)
			}
		default:
			return nil, fmt.Errorf("delivery context %s status=%s 不可消费", record.Origin.DeliveryID, record.Status)
		}

		claim, err := a.newEvent(actor, "delivery_context_claimed", record.Origin.CaseID)
		if err != nil {
			return nil, err
		}
		claim.RelatedEventID, claim.Delivery = record.Origin.ID, deliverySent
		copyDeliveryEnvelope(&claim, record.Origin)
		result = append(result, claim)
	}
	return result, nil
}

func (a *App) prepareDeliveryContext(target string, limit int, promoteQueued bool) (deliveryContextBatch, error) {
	ledger, err := a.ledgerState()
	if err != nil {
		return deliveryContextBatch{}, err
	}
	records := ledger.deliveryContextRecords(target)
	if limit > 0 && len(records) > limit {
		records = records[:limit]
	}
	batch := newDeliveryContextBatch(len(records))
	for _, record := range records {
		if promoteQueued && record.Status == deliveryQueued {
			if err := a.deliverQueuedAtNaturalWake(record.Origin); err != nil {
				return deliveryContextBatch{}, err
			}
		}
		if err := a.appendDeliveryContextRecord(&batch, record); err != nil {
			return deliveryContextBatch{}, err
		}
	}
	return batch, nil
}

func newDeliveryContextBatch(capacity int) deliveryContextBatch {
	return deliveryContextBatch{
		Records:        make([]deliveryContextRecord, 0, capacity),
		Items:          make([]DeliveryContextItem, 0, capacity),
		Envelopes:      make([]string, 0, capacity),
		PayloadDigests: make([]string, 0, capacity),
		ItemBytes:      make([]int, 0, capacity),
	}
}

func (a *App) appendDeliveryContextRecord(batch *deliveryContextBatch, record *deliveryRecord) error {
	origin := record.Origin
	if origin.Type != "message_prepared" {
		return fmt.Errorf("静默上下文仅接受 message origin，实际=%s", origin.Type)
	}
	messageID := origin.MessageID
	if messageID == "" {
		messageID = origin.ID
	}
	item := DeliveryContextItem{
		MessageID: messageID, From: origin.ActorLabel, Kind: origin.MessageKind, Urgency: effectiveMessageUrgency(origin.Urgency), Text: origin.Message,
		CaseID: origin.CaseID, ThreadID: origin.ThreadID, ReplyTo: origin.ReplyTo,
		RefFiles: append([]string(nil), origin.RefFiles...), RefCases: append([]string(nil), origin.RefCases...),
		RefMessages: append([]string(nil), origin.RefMessages...), RefEvents: append([]string(nil), origin.RefEvents...),
	}
	envelope, err := a.deliveryPayload(origin)
	if err != nil {
		return err
	}
	if digestText(envelope) != origin.PayloadDigest {
		return fmt.Errorf("静默 delivery %s payload digest 与 origin 不一致", origin.DeliveryID)
	}
	if len(batch.Envelopes) > 0 {
		batch.Bytes += len("\n\n")
	}
	batch.Bytes += len([]byte(envelope))
	batch.Records = append(batch.Records, deliveryContextRecord{Origin: origin, Status: record.Status})
	batch.Items = append(batch.Items, item)
	batch.Envelopes = append(batch.Envelopes, envelope)
	batch.PayloadDigests = append(batch.PayloadDigests, origin.PayloadDigest)
	batch.ItemBytes = append(batch.ItemBytes, len([]byte(envelope)))
	return nil
}

// prepareTurnBundle selects one exact FIFO prefix while the delivery attempt
// transaction holds the ledger lock. It never skips an oversized head item:
// doing so would allow later context to overtake earlier context.
func (a *App) prepareTurnBundle(ledger *ledgerState, target string, policy DeliveryPolicy) (deliveryContextBatch, error) {
	records := ledger.deliveryContextRecords(target)
	capacity := len(records)
	if capacity > policy.MaxBundleItems {
		capacity = policy.MaxBundleItems
	}
	batch := newDeliveryContextBatch(capacity)
	batch.MaxItems, batch.MaxBytes = policy.MaxBundleItems, policy.MaxBundleBytes
	for _, record := range records {
		if len(batch.Records) >= policy.MaxBundleItems {
			break
		}
		candidate := newDeliveryContextBatch(1)
		if err := a.appendDeliveryContextRecord(&candidate, record); err != nil {
			return deliveryContextBatch{}, err
		}
		additionalBytes := candidate.Bytes
		if len(batch.Records) > 0 {
			additionalBytes += len("\n\n")
		}
		if batch.Bytes+additionalBytes > policy.MaxBundleBytes {
			batch.NextItemBytes = candidate.ItemBytes[0]
			batch.NextDeliveryID = candidate.Records[0].Origin.DeliveryID
			batch.NextDigest = candidate.PayloadDigests[0]
			batch.NextEnvelope = candidate.Envelopes[0]
			break
		}
		if err := a.appendDeliveryContextRecord(&batch, record); err != nil {
			return deliveryContextBatch{}, err
		}
	}
	batch.Overflow = len(records) - len(batch.Records)
	return batch, nil
}

func mergeDeliveryPrompt(base string, envelopes []string) string {
	if len(envelopes) == 0 {
		return base
	}
	return base + "\n\n" + strings.Join(envelopes, "\n\n")
}

func populateTurnBundleManifest(event *Event, origin Event, batch deliveryContextBatch, base, prompt string) {
	event.TurnBundleVersion = turnBundleVersion
	event.TurnPromptDigest = digestText(prompt)
	event.TurnBundleBasePayload = base
	event.TurnBundleDeliveryIDs = make([]string, len(batch.Records))
	for index, record := range batch.Records {
		event.TurnBundleDeliveryIDs[index] = record.Origin.DeliveryID
	}
	event.TurnBundlePayloadDigests = append([]string(nil), batch.PayloadDigests...)
	event.TurnBundleItemBytes = append([]int(nil), batch.ItemBytes...)
	event.TurnBundleEnvelopes = append([]string(nil), batch.Envelopes...)
	event.TurnBundleBytes = batch.Bytes
	event.TurnBundleOverflow = batch.Overflow
	event.TurnBundleMaxItems = batch.MaxItems
	event.TurnBundleMaxBytes = batch.MaxBytes
	event.TurnBundleNextItemBytes = batch.NextItemBytes
	event.TurnBundleNextDeliveryID = batch.NextDeliveryID
	event.TurnBundleNextDigest = batch.NextDigest
	event.TurnBundleNextEnvelope = batch.NextEnvelope
	event.TurnBundleDigest = turnBundleManifestDigest(*event, origin)
}

func turnBundleManifestDigest(event Event, origin Event) string {
	parts := []string{
		strconv.Itoa(event.TurnBundleVersion), origin.DeliveryID, origin.Recipient, origin.PayloadDigest,
		event.TurnPromptDigest, strconv.Itoa(event.TurnBundleBytes), strconv.Itoa(event.TurnBundleOverflow),
		strconv.Itoa(event.TurnBundleMaxItems), strconv.Itoa(event.TurnBundleMaxBytes), strconv.Itoa(event.TurnBundleNextItemBytes),
		event.TurnBundleNextDeliveryID, event.TurnBundleNextDigest,
	}
	for index := range event.TurnBundleDeliveryIDs {
		parts = append(parts, event.TurnBundleDeliveryIDs[index], event.TurnBundlePayloadDigests[index], strconv.Itoa(event.TurnBundleItemBytes[index]))
	}
	return requestDigest("turn-bundle-manifest", parts...)
}

func hasTurnBundleManifest(event Event) bool {
	return event.TurnBundleVersion != 0 || event.TurnBundleDigest != "" || event.TurnPromptDigest != "" ||
		len(event.TurnBundleDeliveryIDs) != 0 || len(event.TurnBundlePayloadDigests) != 0 || len(event.TurnBundleItemBytes) != 0 ||
		event.TurnBundleBytes != 0 || event.TurnBundleOverflow != 0 || event.TurnBundleMaxItems != 0 ||
		event.TurnBundleMaxBytes != 0 || event.TurnBundleNextItemBytes != 0 || event.TurnBundleBasePayload != "" ||
		len(event.TurnBundleEnvelopes) != 0 || event.TurnBundleNextDeliveryID != "" || event.TurnBundleNextDigest != "" ||
		event.TurnBundleNextEnvelope != ""
}

func (s *ledgerState) validateTurnBundleAttempt(event Event, record *deliveryRecord) error {
	if !hasTurnBundleManifest(event) {
		if eventWakesTarget(record.Origin) {
			return fmt.Errorf("event_version=%d 的 wakeup delivery_attempted 必须携带 turn bundle manifest", eventSchemaVersion)
		}
		return nil
	}
	if !eventWakesTarget(record.Origin) {
		return fmt.Errorf("非 wakeup delivery 不允许 turn bundle manifest")
	}
	if event.TurnBundleVersion != turnBundleVersion {
		return fmt.Errorf("turn_bundle_version 必须是 %d", turnBundleVersion)
	}
	if err := validateDigest("turn_bundle_digest", event.TurnBundleDigest); err != nil {
		return err
	}
	if err := validateDigest("turn_prompt_digest", event.TurnPromptDigest); err != nil {
		return err
	}
	selected := len(event.TurnBundleDeliveryIDs)
	if len(event.TurnBundlePayloadDigests) != selected || len(event.TurnBundleItemBytes) != selected || len(event.TurnBundleEnvelopes) != selected {
		return fmt.Errorf("turn bundle manifest 的 delivery/payload/bytes/envelope 数组长度不一致")
	}
	if !utf8.ValidString(event.TurnBundleBasePayload) || strings.ContainsRune(event.TurnBundleBasePayload, '\x00') ||
		len([]byte(event.TurnBundleBasePayload)) > maxTurnBundleBaseBytes || digestText(event.TurnBundleBasePayload) != record.Origin.PayloadDigest {
		return fmt.Errorf("turn bundle base payload 与 origin payload 不一致或越界")
	}
	if event.TurnBundleMaxItems < 1 || event.TurnBundleMaxItems > maxDeliveryBundleItems ||
		event.TurnBundleMaxBytes < 1 || event.TurnBundleMaxBytes > maxDeliveryBundleBytes {
		return fmt.Errorf("turn bundle manifest 缺少合法的冻结 policy limits")
	}
	if selected > event.TurnBundleMaxItems {
		return fmt.Errorf("turn bundle items=%d 超过冻结 policy=%d", selected, event.TurnBundleMaxItems)
	}
	if event.TurnBundleBytes < 0 || event.TurnBundleBytes > event.TurnBundleMaxBytes || event.TurnBundleOverflow < 0 || event.TurnBundleNextItemBytes < 0 {
		return fmt.Errorf("turn bundle bytes/overflow 越界")
	}
	computedBytes := 0
	seen := map[string]bool{}
	for index, deliveryID := range event.TurnBundleDeliveryIDs {
		if err := validateLedgerID("turn bundle delivery_id", deliveryID); err != nil {
			return err
		}
		if seen[deliveryID] {
			return fmt.Errorf("turn bundle delivery_id 重复：%s", deliveryID)
		}
		seen[deliveryID] = true
		if err := validateDigest("turn bundle payload_digest", event.TurnBundlePayloadDigests[index]); err != nil {
			return err
		}
		if event.TurnBundleItemBytes[index] < 1 {
			return fmt.Errorf("turn bundle item_bytes 必须为正数")
		}
		envelope := event.TurnBundleEnvelopes[index]
		if !utf8.ValidString(envelope) || strings.ContainsRune(envelope, '\x00') || len([]byte(envelope)) > maxTurnBundleEnvelopeBytes {
			return fmt.Errorf("turn bundle envelope[%d] 不是合法的有界 UTF-8 payload", index)
		}
		if len([]byte(envelope)) != event.TurnBundleItemBytes[index] {
			return fmt.Errorf("turn bundle item_bytes[%d] 与真实 envelope bytes 不一致", index)
		}
		if digestText(envelope) != event.TurnBundlePayloadDigests[index] {
			return fmt.Errorf("turn bundle envelope[%d] digest 与 payload manifest 不一致", index)
		}
		if index > 0 {
			computedBytes += len("\n\n")
		}
		computedBytes += event.TurnBundleItemBytes[index]
	}
	if computedBytes != event.TurnBundleBytes {
		return fmt.Errorf("turn_bundle_bytes=%d 与 item manifest 计算值=%d 不一致", event.TurnBundleBytes, computedBytes)
	}
	if digestText(mergeDeliveryPrompt(event.TurnBundleBasePayload, event.TurnBundleEnvelopes)) != event.TurnPromptDigest {
		return fmt.Errorf("turn_prompt_digest 与真实 base+envelopes 不一致")
	}
	candidates := s.deliveryContextRecords(record.Origin.Recipient)
	if selected > len(candidates) || event.TurnBundleOverflow != len(candidates)-selected {
		return fmt.Errorf("turn bundle selected/overflow 与当前 FIFO pending context 不一致")
	}
	if event.TurnBundleOverflow == 0 {
		if event.TurnBundleNextItemBytes != 0 || event.TurnBundleNextDeliveryID != "" || event.TurnBundleNextDigest != "" || event.TurnBundleNextEnvelope != "" {
			return fmt.Errorf("无 overflow 的 turn bundle 不允许 next item manifest")
		}
	} else if selected < event.TurnBundleMaxItems {
		separator := 0
		if selected > 0 {
			separator = len("\n\n")
		}
		if !utf8.ValidString(event.TurnBundleNextEnvelope) || strings.ContainsRune(event.TurnBundleNextEnvelope, '\x00') ||
			len([]byte(event.TurnBundleNextEnvelope)) > maxTurnBundleEnvelopeBytes {
			return fmt.Errorf("turn bundle next envelope 不是合法的有界 UTF-8 payload")
		}
		if event.TurnBundleNextItemBytes < 1 || event.TurnBundleBytes+separator+event.TurnBundleNextItemBytes <= event.TurnBundleMaxBytes {
			return fmt.Errorf("turn bundle overflow 未被 item/byte 上限解释")
		}
		next := candidates[selected].Origin
		if event.TurnBundleNextDeliveryID != next.DeliveryID || event.TurnBundleNextDigest != next.PayloadDigest ||
			len([]byte(event.TurnBundleNextEnvelope)) != event.TurnBundleNextItemBytes ||
			digestText(event.TurnBundleNextEnvelope) != next.PayloadDigest {
			return fmt.Errorf("turn bundle next item manifest 与真实 FIFO 队头不一致")
		}
	} else if event.TurnBundleNextItemBytes != 0 || event.TurnBundleNextDeliveryID != "" || event.TurnBundleNextDigest != "" || event.TurnBundleNextEnvelope != "" {
		return fmt.Errorf("item 上限触发的 turn bundle 不允许 next item manifest")
	}
	for index := 0; index < selected; index++ {
		origin := candidates[index].Origin
		if event.TurnBundleDeliveryIDs[index] != origin.DeliveryID || event.TurnBundlePayloadDigests[index] != origin.PayloadDigest {
			return fmt.Errorf("turn bundle manifest 不是当前 pending context 的精确 FIFO 前缀")
		}
	}
	if turnBundleManifestDigest(event, record.Origin) != event.TurnBundleDigest {
		return fmt.Errorf("turn_bundle_digest 与 manifest 不一致")
	}
	return nil
}

func turnBundleManifestIndex(attempt Event, deliveryID, payloadDigest string) (int, bool) {
	for index, candidate := range attempt.TurnBundleDeliveryIDs {
		if candidate == deliveryID && attempt.TurnBundlePayloadDigests[index] == payloadDigest {
			return index, true
		}
	}
	return -1, false
}

func (s *ledgerState) reserveTurnBundleContext(attempt Event) error {
	if !hasTurnBundleManifest(attempt) {
		return nil
	}
	for _, deliveryID := range attempt.TurnBundleDeliveryIDs {
		if owner := s.turnBundleReservations[deliveryID]; owner != "" {
			return fmt.Errorf("turn bundle context %s 已由 attempt=%s 保留", deliveryID, owner)
		}
	}
	for _, deliveryID := range attempt.TurnBundleDeliveryIDs {
		s.turnBundleReservations[deliveryID] = attempt.ID
	}
	return nil
}

func (s *ledgerState) releaseTurnBundleContext(attempt Event) error {
	if !hasTurnBundleManifest(attempt) {
		return nil
	}
	for _, deliveryID := range attempt.TurnBundleDeliveryIDs {
		if owner := s.turnBundleReservations[deliveryID]; owner != attempt.ID {
			return fmt.Errorf("turn bundle context %s reservation=%s，期望=%s", deliveryID, owner, attempt.ID)
		}
	}
	for _, deliveryID := range attempt.TurnBundleDeliveryIDs {
		delete(s.turnBundleReservations, deliveryID)
	}
	return nil
}

func (s *ledgerState) validateTurnBundleChild(event Event, record *deliveryRecord) error {
	parentID := event.TurnBundleParentAttempt
	owner := s.turnBundleReservations[record.Origin.DeliveryID]
	if parentID == "" {
		if owner != "" {
			return fmt.Errorf("delivery context 已由 turn bundle attempt=%s 保留", owner)
		}
		return nil
	}
	if err := validateLedgerID("turn_bundle_parent_attempt_id", parentID); err != nil {
		return err
	}
	parent, ok := s.events[parentID]
	if !ok || parent.Type != "delivery_attempted" || !hasTurnBundleManifest(parent) {
		return fmt.Errorf("turn bundle child 缺少匹配的 parent attempted event")
	}
	parentRecord := s.deliveries[parent.DeliveryID]
	if parentRecord == nil || parentRecord.Attempt.ID != parentID ||
		(parentRecord.Status != deliveryAttempted && parentRecord.Status != deliveryUnknown) {
		return fmt.Errorf("turn bundle parent attempt 当前不可收敛")
	}
	if _, ok := turnBundleManifestIndex(parent, record.Origin.DeliveryID, record.Origin.PayloadDigest); !ok {
		return fmt.Errorf("turn bundle child 不在 parent manifest 中")
	}
	if owner != parentID {
		return fmt.Errorf("turn bundle child reservation=%s，期望=%s", owner, parentID)
	}
	return nil
}

func (s *ledgerState) validateTurnBundleConverged(attempt Event) error {
	if !hasTurnBundleManifest(attempt) {
		return nil
	}
	for _, deliveryID := range attempt.TurnBundleDeliveryIDs {
		record := s.deliveries[deliveryID]
		if record == nil || record.Status != deliverySent || record.ContextState != deliveryContextClaimed ||
			record.BundledByAttemptID != attempt.ID {
			return fmt.Errorf("turn bundle context %s 未与 parent terminal 原子收敛", deliveryID)
		}
	}
	return nil
}

func turnBundleContextReferencesAttempt(record *deliveryRecord, attemptID string) bool {
	if record == nil || attemptID == "" {
		return false
	}
	return record.Attempt.TurnBundleParentAttempt == attemptID ||
		record.Terminal.TurnBundleParentAttempt == attemptID ||
		record.BundledByAttemptID == attemptID
}

func (s *ledgerState) validateTurnBundleUnconverged(attempt Event) error {
	if !hasTurnBundleManifest(attempt) {
		return nil
	}
	for _, deliveryID := range attempt.TurnBundleDeliveryIDs {
		if turnBundleContextReferencesAttempt(s.deliveries[deliveryID], attempt.ID) {
			return fmt.Errorf("turn bundle context %s 已开始收敛，parent 不可记录未投递终态", deliveryID)
		}
	}
	return nil
}

// validateTurnBundleFinalInvariants is intentionally a ledger-tail check. A
// sent terminal transaction applies child attempt/sent/claim events before its
// parent terminal, so individual apply steps temporarily expose convergence.
// No durable ledger tail may stop in that transient state.
func (s *ledgerState) validateTurnBundleFinalInvariants() error {
	parents := map[string]bool{}
	for _, record := range s.deliveries {
		for _, parentID := range []string{
			record.Attempt.TurnBundleParentAttempt,
			record.Terminal.TurnBundleParentAttempt,
			record.BundledByAttemptID,
		} {
			if parentID != "" {
				parents[parentID] = true
			}
		}
	}
	for parentID := range parents {
		parent, ok := s.events[parentID]
		if !ok || parent.Type != "delivery_attempted" || !hasTurnBundleManifest(parent) {
			return fmt.Errorf("turn bundle convergence 引用了无效 parent attempt=%s", parentID)
		}
		parentRecord := s.deliveries[parent.DeliveryID]
		if parentRecord == nil || parentRecord.Attempt.ID != parentID || parentRecord.Status != deliverySent {
			return fmt.Errorf("turn bundle parent attempt=%s 未到 delivered terminal，但 child 已开始收敛", parentID)
		}
		if err := s.validateTurnBundleConverged(parent); err != nil {
			return err
		}
	}
	return nil
}

// buildTurnBundleConvergenceEvents materializes the selected context deliveries
// before the parent sent/resolved-sent terminal. The enclosing transaction
// appends these events and the parent terminal as one durable batch, so replay
// can never observe a sent parent with pending manifest context.
func (a *App) buildTurnBundleConvergenceEvents(ledger *ledgerState, parent *deliveryRecord) ([]Event, error) {
	attempt := parent.Attempt
	if !hasTurnBundleManifest(attempt) || len(attempt.TurnBundleDeliveryIDs) == 0 {
		return nil, nil
	}
	targetRule, ok := historicalRule(a.Config, parent.Origin.Recipient)
	if !ok {
		return nil, fmt.Errorf("turn bundle target 未登记：%s", parent.Origin.Recipient)
	}
	claimActor := Actor{Name: targetRule.Name, Label: targetRule.Label, Department: targetRule.Department, Rule: targetRule}
	result := make([]Event, 0, len(attempt.TurnBundleDeliveryIDs)*3)
	for index, deliveryID := range attempt.TurnBundleDeliveryIDs {
		record := ledger.deliveries[deliveryID]
		if record == nil || record.Origin.Type != "message_prepared" || record.Origin.Recipient != parent.Origin.Recipient ||
			record.Origin.PayloadDigest != attempt.TurnBundlePayloadDigests[index] {
			return nil, fmt.Errorf("turn bundle context %s 与 parent manifest 不一致", deliveryID)
		}
		if owner := ledger.turnBundleReservations[deliveryID]; owner != attempt.ID {
			return nil, fmt.Errorf("turn bundle context %s reservation=%s，期望=%s", deliveryID, owner, attempt.ID)
		}
		switch record.Status {
		case deliveryQueued:
			childAttempt, err := a.newEvent(Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}, "delivery_attempted", record.Origin.CaseID)
			if err != nil {
				return nil, err
			}
			childAttempt.RelatedEventID = record.Origin.ID
			childAttempt.TurnBundleParentAttempt = attempt.ID
			copyDeliveryEnvelope(&childAttempt, record.Origin)
			childAttempt.Delivery = deliveryAttempted

			childSent, err := a.newEvent(Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}, "message_sent", record.Origin.CaseID)
			if err != nil {
				return nil, err
			}
			childSent.RelatedEventID, childSent.AttemptEventID = record.Origin.ID, childAttempt.ID
			childSent.TurnBundleParentAttempt = attempt.ID
			copyDeliveryEnvelope(&childSent, record.Origin)
			childSent.Delivery = deliverySent
			a.fillSentState(&childSent, record.Origin, ledger)
			result = append(result, childAttempt, childSent)
		case deliverySent:
			if record.ContextState == deliveryContextClaimed {
				if record.BundledByAttemptID != attempt.ID {
					return nil, fmt.Errorf("turn bundle context %s 已被其他边界消费", deliveryID)
				}
				continue
			}
			if record.ContextState != deliveryContextHistory {
				return nil, fmt.Errorf("turn bundle context %s sent state=%s 不可消费", deliveryID, record.ContextState)
			}
		default:
			return nil, fmt.Errorf("turn bundle context %s delivery status=%s 不可原子收敛", deliveryID, record.Status)
		}

		claim, err := a.newEvent(claimActor, "delivery_context_claimed", record.Origin.CaseID)
		if err != nil {
			return nil, err
		}
		claim.RelatedEventID = record.Origin.ID
		claim.TurnBundleParentAttempt = attempt.ID
		copyDeliveryEnvelope(&claim, record.Origin)
		claim.Delivery = deliverySent
		result = append(result, claim)
	}
	if ledger.deliveryBudgetSpent(parent.Origin.Recipient) > 0 {
		reset, err := a.newEvent(claimActor, "delivery_budget_reset", "")
		if err != nil {
			return nil, err
		}
		reset.Recipient, reset.RecipientLabel = claimActor.Name, claimActor.Label
		reset.RelatedEventID = ledger.deliveryLastWake[claimActor.Name]
		reset.DeliveryReason = "natural-wake"
		result = append(result, reset)
	}
	return result, nil
}

func writeDeliveryContext(writer io.Writer, payload []byte) error {
	written, err := writer.Write(payload)
	if err != nil {
		return err
	}
	if written != len(payload) {
		return io.ErrShortWrite
	}
	return nil
}

func (a *App) writeDeliveryContextJSON(items []DeliveryContextItem) error {
	value, err := makeJSONSafeForJavaScript(items)
	if err != nil {
		return err
	}
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	return writeDeliveryContext(a.Out, payload)
}

func (a *App) deliverQueuedAtNaturalWake(origin Event) error {
	releaseOperation, err := a.lockOperation(operationScopeDelivery, origin.DeliveryID)
	if err != nil {
		return err
	}
	defer releaseOperation()
	return a.deliverQueuedAtNaturalWakeLocked(origin)
}

func (a *App) deliverQueuedAtNaturalWakeLocked(origin Event) error {
	commandID := "delivery-natural-attempt:" + origin.DeliveryID
	digest := requestDigest("delivery-natural-attempt", origin.DeliveryID, origin.Recipient, origin.PayloadDigest)
	result, err := a.Store.Transact(a.Config, commandID, digest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.deliveries[origin.DeliveryID]
		if record == nil || record.Status != deliveryQueued {
			return Event{}, fmt.Errorf("delivery %s 不在 queued", origin.DeliveryID)
		}
		event, err := a.newEvent(Actor{Name: origin.Actor, Label: origin.ActorLabel, PaneID: origin.ActorPaneID}, "delivery_attempted", origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		copyDeliveryEnvelope(&event, origin)
		event.RelatedEventID, event.Delivery = origin.ID, deliveryAttempted
		return event, nil
	})
	if err != nil {
		view, ok, viewErr := a.Store.Delivery(a.Config, origin.DeliveryID)
		if viewErr == nil && ok && view.Status == deliverySent {
			return nil
		}
		return err
	}
	_, err = a.appendDeliveryTerminal(origin, result.Event, deliverySent, nil)
	return err
}

func (a *App) claimDeliveryContext(actor Actor, origin Event) error {
	commandID := "delivery-context-claim:" + origin.DeliveryID
	digest := requestDigest("delivery-context-claim", actor.Name, origin.DeliveryID, origin.PayloadDigest)
	_, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		record := ledger.deliveries[origin.DeliveryID]
		if record == nil || record.Status != deliverySent || record.ContextState != deliveryContextHistory {
			return Event{}, fmt.Errorf("delivery %s 没有可并入的 target history", origin.DeliveryID)
		}
		if record.Origin.Recipient != actor.Name {
			return Event{}, permissionf("只有目标 agent 可消费静默上下文")
		}
		event, err := a.newEvent(actor, "delivery_context_claimed", origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		copyDeliveryEnvelope(&event, origin)
		event.RelatedEventID, event.Delivery = origin.ID, deliverySent
		return event, nil
	})
	return err
}

func (a *App) resetDeliveryBudget(actor Actor) error {
	ledger, err := a.ledgerState()
	if err != nil || ledger.deliveryBudgetSpent(actor.Name) == 0 {
		return err
	}
	last := ledger.deliveryLastWake[actor.Name]
	commandID := stableCommandID("delivery-budget-reset", actor.Name, last)
	digest := requestDigest("delivery-budget-reset", actor.Name, last)
	_, err = a.transact(commandID, digest, func(current *ledgerState) (Event, error) {
		if current.deliveryBudgetSpent(actor.Name) == 0 {
			return Event{}, fmt.Errorf("目标 %s 当前唤醒预算未消耗", actor.Name)
		}
		event, err := a.newEvent(actor, "delivery_budget_reset", "")
		if err != nil {
			return Event{}, err
		}
		event.Recipient = actor.Name
		event.RecipientLabel = actor.Label
		event.RelatedEventID = current.deliveryLastWake[actor.Name]
		event.DeliveryReason = "natural-wake"
		return event, nil
	})
	return err
}

func copyDeliveryEnvelope(dst *Event, origin Event) {
	dst.Recipient, dst.RecipientLabel = origin.Recipient, origin.RecipientLabel
	dst.DeliveryID, dst.PayloadDigest = origin.DeliveryID, origin.PayloadDigest
	dst.DeliveryMode, dst.DeliveryTarget, dst.DeliveryReason = origin.DeliveryMode, origin.DeliveryTarget, origin.DeliveryReason
	dst.Urgency = origin.Urgency
}
