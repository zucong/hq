package main

import (
	"fmt"
	"sort"
	"strings"
)

const (
	deliveryPrepared      = "prepared"
	deliveryQueued        = "queued"
	deliveryAttempted     = "attempted"
	deliverySent          = "sent"
	deliveryFailedPreSend = "failed_pre_send"
	deliveryUnknown       = "unknown"
)

const (
	deliveryContextPending = "pending"
	deliveryContextHistory = "history"
	deliveryContextClaimed = "claimed"
)

type deliveryRecord struct {
	Origin                 Event
	Status                 string
	Attempt                Event
	Terminal               Event
	Ack                    Event
	AttemptCount           int
	ContextState           string
	BundledByAttemptID     string
	BundledByDeliveryID    string
	ActivationStatus       string
	ActivationAttempt      Event
	ActivationTerminal     Event
	ActivationAttemptCount int
}

type DeliveryView struct {
	DeliveryID                string   `json:"delivery_id"`
	CaseID                    string   `json:"case_id"`
	OriginEventID             string   `json:"origin_event_id"`
	OriginType                string   `json:"origin_type"`
	Target                    string   `json:"target"`
	PayloadDigest             string   `json:"payload_digest"`
	Status                    string   `json:"internal_status"`
	ProjectionStatus          string   `json:"status"`
	AttemptEventID            string   `json:"attempt_event_id,omitempty"`
	TerminalEventID           string   `json:"terminal_event_id,omitempty"`
	AckedBy                   string   `json:"acked_by,omitempty"`
	AckEventID                string   `json:"ack_event_id,omitempty"`
	AttemptCount              int      `json:"attempt_count"`
	DeliveryMode              string   `json:"delivery_mode"`
	DeliveryTarget            string   `json:"delivery_target"`
	Wakeup                    bool     `json:"wakeup"`
	Urgency                   string   `json:"urgency"`
	DecisionReason            string   `json:"decision_reason,omitempty"`
	ContextState              string   `json:"context_state"`
	TurnBundleVersion         int      `json:"turn_bundle_version,omitempty"`
	TurnBundleDigest          string   `json:"turn_bundle_digest,omitempty"`
	TurnPromptDigest          string   `json:"turn_prompt_digest,omitempty"`
	TurnBundleDeliveryIDs     []string `json:"turn_bundle_delivery_ids,omitempty"`
	TurnBundlePayloadDigests  []string `json:"turn_bundle_payload_digests,omitempty"`
	TurnBundleItemBytes       []int    `json:"turn_bundle_item_bytes,omitempty"`
	TurnBundleBytes           int      `json:"turn_bundle_bytes,omitempty"`
	TurnBundleOverflow        int      `json:"turn_bundle_overflow,omitempty"`
	TurnBundleMaxItems        int      `json:"turn_bundle_max_items,omitempty"`
	TurnBundleMaxBytes        int      `json:"turn_bundle_max_bytes,omitempty"`
	TurnBundleNextItemBytes   int      `json:"turn_bundle_next_item_bytes,omitempty"`
	BundledByAttemptID        string   `json:"bundled_by_attempt_id,omitempty"`
	BundledByDeliveryID       string   `json:"bundled_by_delivery_id,omitempty"`
	StatusDescription         string   `json:"status_description"`
	NextAction                string   `json:"next_action,omitempty"`
	ActivationStatus          string   `json:"activation_status,omitempty"`
	ActivationAttemptEventID  string   `json:"activation_attempt_event_id,omitempty"`
	ActivationTerminalEventID string   `json:"activation_terminal_event_id,omitempty"`
	ActivationAttemptCount    int      `json:"activation_attempt_count,omitempty"`
}

func describeDelivery(record *deliveryRecord) (string, string) {
	if record.Ack.ID != "" {
		return "接收方已明确确认收到", "无需操作"
	}
	switch record.Status {
	case deliveryPrepared:
		return "消息已记入 HQ，但尚未开始外部投递", "HQ 将开始投递"
	case deliveryQueued:
		if record.Origin.Type == "message_prepared" && record.Origin.DeliveryReason == "wake-budget-exhausted" && messageNeedsAction(record.Origin.MessageKind) {
			return "行动消息因连续唤醒预算耗尽进入静默队列；gateway 会在超时且目标 idle|done 后用 durable nudge 延迟唤醒", "接收方被唤醒后运行 hq delivery consume，按 FIFO 执行；不要重发原消息"
		}
		return "消息正在静默队列中，不会主动唤醒接收方", "HQ 会在下一次唤醒 prompt 或 accept 输出中自动合并；delivery consume 仅用于人工恢复"
	case deliveryAttempted:
		return "已尝试外部投递，但尚无可证明的终态", "等待 reconcile；不要盲目重发"
	case deliveryFailedPreSend:
		return "已确认没有投递给接收方", "运行 hq delivery retry"
	case deliverySent:
		if record.Origin.Type == "issue_prepared" && record.Ack.ID == "" {
			switch record.ActivationStatus {
			case activationAttempted:
				return "issue 已注入，但 assignment 激活重投尚无可证明终态", "等待下一轮看门狗转为 unknown；禁止盲目重投"
			case activationUnknown:
				return "issue 已注入，但无法证明最近一次 assignment 激活重投是否送达", fmt.Sprintf("由 can_manage_staff 核对后运行 hq delivery resolve --id %s --outcome delivered|not-delivered --reason TEXT --evidence PATH", record.Origin.DeliveryID)
			case activationFailedPreSend:
				return "issue 首次注入成功；最近一次 assignment 激活重投已确证未送达", "HQ 会在员工席位恢复为安全 idle 后继续有界重试"
			case activationExhausted:
				return "issue 首次注入成功，但员工始终未 accept，自动激活重投额度已耗尽", fmt.Sprintf("经理核验终端后运行 hq delivery retry --id %s；不要新建 assignment", record.Origin.DeliveryID)
			case activationSent:
				return "issue 与最近一次激活重投均已确认注入，但员工仍未 accept", "HQ 会在超时后继续有界激活；不要重复派单"
			default:
				return "issue 已确认注入，但这不等于员工已 accept 激活", "HQ 将在 accept 超时且席位可安全重投时复用同一 assignment 自动激活"
			}
		}
		return "HQ 已确认投递完成，但接收方未必已阅读或 ack", "如业务需要，等待接收方 ack"
	case deliveryUnknown:
		return "无法证明投递成功或失败", "由总裁办运维核对后运行 hq delivery resolve"
	default:
		return "未知投递状态", "查看 verbose/debug 事件"
	}
}

func deliveryProjectionStatus(record *deliveryRecord) string {
	if record.Ack.ID != "" {
		return "acked"
	}
	switch record.Status {
	case deliveryPrepared, deliveryQueued, deliveryAttempted:
		return "pending"
	case deliveryFailedPreSend:
		return "failed"
	case deliverySent:
		return "sent"
	default:
		// unknown is intentionally not collapsed into pending or failed: HQ
		// cannot prove whether the external side effect happened.
		return deliveryUnknown
	}
}

func (s *ledgerState) acknowledgeDelivery(delivered Event, ack Event) error {
	record := s.deliveries[delivered.DeliveryID]
	if record == nil || record.Status != deliverySent {
		return fmt.Errorf("ack 引用的 delivery 未处于 sent：%s", delivered.DeliveryID)
	}
	if record.Origin.Recipient != ack.Actor || delivered.Recipient != ack.Actor {
		return fmt.Errorf("acked_by 必须等于 origin.recipient 与 accept.actor")
	}
	if record.Ack.ID != "" {
		return fmt.Errorf("delivery 已被业务核验：%s", delivered.DeliveryID)
	}
	record.Ack = ack
	return nil
}

func isDeliveryOriginType(eventType string) bool {
	switch eventType {
	case "case_escalation_prepared", "report_prepared", "issue_prepared", "message_prepared", "event_accepted", "event_returned":
		return true
	default:
		return false
	}
}

func isDeliverySentType(eventType string) bool {
	switch eventType {
	case "case_escalation_sent", "report_sent", "issue_sent", "message_sent", "accept_notice_sent", "return_notice_sent", "delivery_resolved_sent":
		return true
	default:
		return false
	}
}

func validateDeliveryFields(event Event, expectedState string) error {
	if err := validateLedgerID("delivery_id", event.DeliveryID); err != nil {
		return err
	}
	if err := validateDigest("payload_digest", event.PayloadDigest); err != nil {
		return err
	}
	if event.Delivery != expectedState {
		return fmt.Errorf("delivery 状态必须是 %s，实际为 %q", expectedState, event.Delivery)
	}
	if event.Recipient == "" {
		return fmt.Errorf("delivery 缺少 recipient")
	}
	return nil
}

func (s *ledgerState) registerDeliveryOrigin(event Event) error {
	if err := validateDeliveryFields(event, deliveryPrepared); err != nil {
		return err
	}
	if existing := s.deliveries[event.DeliveryID]; existing != nil {
		return fmt.Errorf("delivery_id 重复：%s（已有 origin=%s）", event.DeliveryID, existing.Origin.ID)
	}
	s.deliveries[event.DeliveryID] = &deliveryRecord{Origin: event, Status: deliveryPrepared}
	// Reserve wake budget at durable queue admission, not after the external
	// prompt. Concurrent senders therefore serialize against the same ledger
	// projection and cannot all observe stale budget.
	if event.DeliveryMode != "" && eventWakesTarget(event) {
		s.deliveryWakeSpends[event.Recipient]++
		s.deliveryLastWake[event.Recipient] = event.ID
	}
	return nil
}

func (s *ledgerState) deliveryForEvent(event Event) (*deliveryRecord, error) {
	record := s.deliveries[event.DeliveryID]
	if record == nil {
		return nil, fmt.Errorf("delivery_id 未登记：%s", event.DeliveryID)
	}
	if event.RelatedEventID != record.Origin.ID || event.CaseID != record.Origin.CaseID ||
		event.Recipient != record.Origin.Recipient || event.PayloadDigest != record.Origin.PayloadDigest {
		return nil, fmt.Errorf("delivery %s 的 origin/case/target/payload digest 与 prepared 不一致", event.DeliveryID)
	}
	if effectiveEventDeliveryMode(event) != effectiveEventDeliveryMode(record.Origin) ||
		effectiveEventDeliveryTarget(event) != effectiveEventDeliveryTarget(record.Origin) {
		return nil, fmt.Errorf("delivery %s 的 mode/target 与 prepared 不一致", event.DeliveryID)
	}
	return record, nil
}

func (s *ledgerState) applyDeliveryQueued(event Event) error {
	if err := validateDeliveryFields(event, deliveryQueued); err != nil {
		return err
	}
	record, err := s.deliveryForEvent(event)
	if err != nil {
		return err
	}
	mode := effectiveEventDeliveryMode(record.Origin)
	if mode != deliveryModeQuiet && mode != deliveryModeInject {
		return fmt.Errorf("delivery %s 只有 quiet/inject 可静默排队", event.DeliveryID)
	}
	if record.Status != deliveryPrepared {
		return fmt.Errorf("delivery %s 当前为 %s，不可重复排队", event.DeliveryID, record.Status)
	}
	if err := requireNoStateFields(event); err != nil {
		return err
	}
	record.Status, record.Terminal, record.ContextState = deliveryQueued, event, deliveryContextPending
	return nil
}

func (s *ledgerState) applyDeliveryAttempt(event Event) error {
	if err := validateDeliveryFields(event, deliveryAttempted); err != nil {
		return err
	}
	record, err := s.deliveryForEvent(event)
	if err != nil {
		return err
	}
	if event.Actor != record.Origin.Actor || event.ActorLabel != record.Origin.ActorLabel || event.ActorPaneID != record.Origin.ActorPaneID {
		return fmt.Errorf("delivery %s attempted actor 与 origin 不一致", event.DeliveryID)
	}
	if record.Status != deliveryPrepared && record.Status != deliveryQueued && record.Status != deliveryFailedPreSend {
		return fmt.Errorf("delivery %s 当前为 %s，不可开始真实尝试", event.DeliveryID, record.Status)
	}
	if err := s.validateTurnBundleChild(event, record); err != nil {
		return fmt.Errorf("delivery %s turn bundle child 无效：%w", event.DeliveryID, err)
	}
	if err := s.validateTurnBundleAttempt(event, record); err != nil {
		return fmt.Errorf("delivery %s turn bundle manifest 无效：%w", event.DeliveryID, err)
	}
	if err := requireNoStateFields(event); err != nil {
		return err
	}
	if err := s.reserveTurnBundleContext(event); err != nil {
		return err
	}
	record.Status = deliveryAttempted
	record.Attempt = event
	record.Terminal = Event{}
	record.AttemptCount++
	return nil
}

func (s *ledgerState) applyDeliveryTerminal(event Event, state string) error {
	if err := validateDeliveryFields(event, state); err != nil {
		return err
	}
	record, err := s.deliveryForEvent(event)
	if err != nil {
		return err
	}
	if event.Actor != record.Origin.Actor || event.ActorLabel != record.Origin.ActorLabel || event.ActorPaneID != record.Origin.ActorPaneID {
		return fmt.Errorf("delivery %s terminal actor 与 origin 不一致", event.DeliveryID)
	}
	if record.Status != deliveryAttempted || record.Attempt.ID == "" || event.AttemptEventID != record.Attempt.ID {
		return fmt.Errorf("delivery %s 没有匹配的 attempted 事件", event.DeliveryID)
	}
	if event.TurnBundleParentAttempt != record.Attempt.TurnBundleParentAttempt {
		return fmt.Errorf("delivery %s terminal 的 turn bundle parent 与 attempted 不一致", event.DeliveryID)
	}
	if err := s.validateTurnBundleChild(event, record); err != nil {
		return fmt.Errorf("delivery %s terminal child 无效：%w", event.DeliveryID, err)
	}
	if state != deliverySent {
		if err := requireNoStateFields(event); err != nil {
			return err
		}
	}
	if state == deliverySent {
		if err := s.validateTurnBundleConverged(record.Attempt); err != nil {
			return err
		}
		if err := s.releaseTurnBundleContext(record.Attempt); err != nil {
			return err
		}
	} else {
		if err := s.validateTurnBundleUnconverged(record.Attempt); err != nil {
			return err
		}
		if state == deliveryFailedPreSend {
			if err := s.releaseTurnBundleContext(record.Attempt); err != nil {
				return err
			}
		}
	}
	record.Status, record.Terminal = state, event
	if state == deliverySent {
		if eventWakesTarget(record.Origin) {
			record.ContextState = deliveryContextClaimed
		} else {
			record.ContextState = deliveryContextHistory
		}
	}
	return nil
}

func (s *ledgerState) applyDeliveryContextClaim(event Event) error {
	if err := validateDeliveryFields(event, deliverySent); err != nil {
		return err
	}
	record, err := s.deliveryForEvent(event)
	if err != nil {
		return err
	}
	if event.Actor != record.Origin.Recipient || record.Status != deliverySent || record.ContextState != deliveryContextHistory {
		return fmt.Errorf("delivery %s 没有可由目标消费的 history", event.DeliveryID)
	}
	if err := s.validateTurnBundleChild(event, record); err != nil {
		return fmt.Errorf("delivery %s claim child 无效：%w", event.DeliveryID, err)
	}
	if event.TurnBundleParentAttempt != "" && record.Terminal.TurnBundleParentAttempt != "" &&
		record.Terminal.TurnBundleParentAttempt != event.TurnBundleParentAttempt {
		return fmt.Errorf("delivery %s claim 与 bundled message_sent parent 不一致", event.DeliveryID)
	}
	if err := requireNoStateFields(event); err != nil {
		return err
	}
	record.ContextState = deliveryContextClaimed
	if event.TurnBundleParentAttempt != "" {
		parent := s.events[event.TurnBundleParentAttempt]
		record.BundledByAttemptID = event.TurnBundleParentAttempt
		record.BundledByDeliveryID = parent.DeliveryID
	}
	return nil
}

func (s *ledgerState) applyDeliveryResolution(event Event, delivered bool, actorRule AgentRule) (*deliveryRecord, error) {
	expected := deliveryFailedPreSend
	if delivered {
		expected = deliverySent
	}
	if err := validateDeliveryFields(event, expected); err != nil {
		return nil, err
	}
	record, err := s.deliveryForEvent(event)
	if err != nil {
		return nil, err
	}
	if record.Status != deliveryUnknown {
		return nil, fmt.Errorf("delivery %s 当前为 %s，仅 unknown 可人工解除", event.DeliveryID, record.Status)
	}
	if event.AttemptEventID != record.Attempt.ID {
		return nil, fmt.Errorf("delivery %s resolution 未引用最后 attempted 事件", event.DeliveryID)
	}
	if !actorRule.CanManageStaff {
		return nil, fmt.Errorf("actor %s 不在运维白名单，不能解除 unknown delivery", event.Actor)
	}
	if event.Note == "" || event.ResolutionRef == "" {
		return nil, fmt.Errorf("解除 unknown 必须记录理由与依据引用")
	}
	if delivered {
		if err := s.validateTurnBundleConverged(record.Attempt); err != nil {
			return nil, err
		}
	} else if err := s.validateTurnBundleUnconverged(record.Attempt); err != nil {
		return nil, err
	}
	if err := s.releaseTurnBundleContext(record.Attempt); err != nil {
		return nil, err
	}
	record.Status, record.Terminal = expected, event
	if delivered {
		if eventWakesTarget(record.Origin) {
			record.ContextState = deliveryContextClaimed
		} else {
			record.ContextState = deliveryContextHistory
		}
	}
	return record, nil
}

func semanticDeliveredEvent(event Event, events map[string]Event) (Event, bool) {
	if event.Type == "case_escalation_sent" || event.Type == "report_sent" || event.Type == "issue_sent" || event.Type == "message_sent" {
		return event, true
	}
	if event.Type != "delivery_resolved_sent" {
		return Event{}, false
	}
	origin, ok := events[event.RelatedEventID]
	if !ok || (origin.Type != "case_escalation_prepared" && origin.Type != "report_prepared" && origin.Type != "issue_prepared" && origin.Type != "message_prepared") {
		return Event{}, false
	}
	semantic := event
	semantic.Type = strings.TrimSuffix(origin.Type, "_prepared") + "_sent"
	semantic.Actor = origin.Actor
	semantic.ActorLabel = origin.ActorLabel
	semantic.ActorPaneID = origin.ActorPaneID
	semantic.Result = origin.Result
	semantic.Title = origin.Title
	semantic.Project = origin.Project
	semantic.ParentCaseID = origin.ParentCaseID
	semantic.RootCaseID = origin.RootCaseID
	semantic.ApprovalRef = origin.ApprovalRef
	semantic.SourceRef = origin.SourceRef
	semantic.ArtifactRef = origin.ArtifactRef
	semantic.Location = origin.Location
	semantic.Verification = origin.Verification
	semantic.NextAction = origin.NextAction
	semantic.Note = origin.Note
	semantic.CaseVersion = origin.CaseVersion
	semantic.CaseDigest = origin.CaseDigest
	copyAssignmentBinding(&semantic, origin)
	semantic.AuthorizationType = origin.AuthorizationType
	semantic.AuthorizationDigest = origin.AuthorizationDigest
	semantic.ApprovalID = origin.ApprovalID
	semantic.DecisionRef = origin.DecisionRef
	semantic.Issuer = origin.Issuer
	semantic.CapturedBy = origin.CapturedBy
	semantic.MessageKind = origin.MessageKind
	semantic.Urgency = origin.Urgency
	semantic.MessageID = origin.MessageID
	semantic.Message = origin.Message
	semantic.ThreadID = origin.ThreadID
	semantic.ReplyTo = origin.ReplyTo
	semantic.RefFiles, semantic.RefCases = append([]string(nil), origin.RefFiles...), append([]string(nil), origin.RefCases...)
	semantic.RefMessages, semantic.RefEvents = append([]string(nil), origin.RefMessages...), append([]string(nil), origin.RefEvents...)
	semantic.SupersedesAssignmentEventID = origin.SupersedesAssignmentEventID
	semantic.SupersedesAssignmentID = origin.SupersedesAssignmentID
	semantic.BasisEventID = origin.BasisEventID
	return semantic, true
}

func (s *ledgerState) validateResolvedDeliveryState(event Event, origin Event, cfg Config) error {
	switch origin.Type {
	case "case_escalation_prepared":
		state, err := s.currentCase(origin.CaseID)
		if err != nil {
			return err
		}
		if !sameCaseEscalationContract(origin, event) {
			return fmt.Errorf("resolved case escalation 与 prepared 合同不一致")
		}
		if event.FromState != state.Status || event.ToState != string(statusEscalated) || event.Owner != origin.Recipient {
			return fmt.Errorf("resolved case escalation 的前态、后态或 owner 不匹配")
		}
		if err := validateStateTransition(actionEscalate, event.FromState, event.ToState); err != nil {
			return err
		}
	case "report_prepared":
		state, err := s.currentCase(origin.CaseID)
		if err != nil {
			return err
		}
		target, action, err := reportTargetState(origin.Result)
		if err != nil {
			return err
		}
		if event.FromState != state.Status || event.ToState != target || event.Owner != origin.Recipient {
			return fmt.Errorf("resolved report 的前态、后态或 owner 不匹配")
		}
		if err := validateStateTransition(action, event.FromState, event.ToState); err != nil {
			return err
		}
		if origin.AssignmentEventID != "" {
			if err := s.validateAssignment(origin.AssignmentEventID, origin.CaseID, origin.Actor); err != nil {
				return err
			}
			assignment := s.assignments[origin.AssignmentEventID]
			if assignment.SubmissionEventID != origin.ID {
				return fmt.Errorf("assignment %s resolved report 未匹配本轮冻结 submission", assignment.AssignmentID)
			}
			assignment.Status = "submitted"
		}
	case "issue_prepared":
		state, err := s.currentCase(origin.CaseID)
		if err != nil {
			return err
		}
		if event.FromState != state.Status || event.ToState != string(statusDispatched) || event.Owner != origin.Recipient {
			return fmt.Errorf("resolved issue 的前态、后态或 owner 不匹配")
		}
		if err := validateStateTransition(actionIssue, event.FromState, event.ToState); err != nil {
			return err
		}
		assignment, err := assignmentFromDeliveredIssue(event, origin, cfg)
		if err != nil {
			return err
		}
		for _, existing := range s.assignments {
			if existing.AssignmentID == assignment.AssignmentID {
				return fmt.Errorf("assignment_id 重复：%s", assignment.AssignmentID)
			}
		}
		s.assignments[event.ID] = assignment
		s.assignmentList = append(s.assignmentList, event.ID)
	case "message_prepared":
		if err := requireNoStateFields(event); err != nil {
			return err
		}
	case "event_accepted", "event_returned":
		if err := requireNoStateFields(event); err != nil {
			return err
		}
	default:
		return fmt.Errorf("delivery origin %s 不可标记 delivered", origin.Type)
	}
	_ = cfg
	return nil
}

func (s *ledgerState) deliveryViews() []DeliveryView {
	views := make([]DeliveryView, 0, len(s.deliveries))
	for _, record := range s.deliveries {
		view := DeliveryView{
			DeliveryID: record.Origin.DeliveryID, CaseID: record.Origin.CaseID,
			OriginEventID: record.Origin.ID, OriginType: record.Origin.Type,
			Target: record.Origin.Recipient, PayloadDigest: record.Origin.PayloadDigest,
			Status: record.Status, ProjectionStatus: deliveryProjectionStatus(record), AttemptCount: record.AttemptCount,
			DeliveryMode: effectiveEventDeliveryMode(record.Origin), DeliveryTarget: effectiveEventDeliveryTarget(record.Origin),
			Wakeup: eventWakesTarget(record.Origin), DecisionReason: record.Origin.DeliveryReason, ContextState: record.ContextState,
			Urgency: effectiveMessageUrgency(record.Origin.Urgency),
		}
		view.StatusDescription, view.NextAction = describeDelivery(record)
		view.AttemptEventID = record.Attempt.ID
		view.TurnBundleVersion = record.Attempt.TurnBundleVersion
		view.TurnBundleDigest = record.Attempt.TurnBundleDigest
		view.TurnPromptDigest = record.Attempt.TurnPromptDigest
		view.TurnBundleDeliveryIDs = append([]string(nil), record.Attempt.TurnBundleDeliveryIDs...)
		view.TurnBundlePayloadDigests = append([]string(nil), record.Attempt.TurnBundlePayloadDigests...)
		view.TurnBundleItemBytes = append([]int(nil), record.Attempt.TurnBundleItemBytes...)
		view.TurnBundleBytes = record.Attempt.TurnBundleBytes
		view.TurnBundleOverflow = record.Attempt.TurnBundleOverflow
		view.TurnBundleMaxItems = record.Attempt.TurnBundleMaxItems
		view.TurnBundleMaxBytes = record.Attempt.TurnBundleMaxBytes
		view.TurnBundleNextItemBytes = record.Attempt.TurnBundleNextItemBytes
		view.BundledByAttemptID = record.BundledByAttemptID
		view.BundledByDeliveryID = record.BundledByDeliveryID
		view.TerminalEventID = record.Terminal.ID
		view.AckedBy = record.Ack.Actor
		view.AckEventID = record.Ack.ID
		view.ActivationStatus = record.ActivationStatus
		view.ActivationAttemptEventID = record.ActivationAttempt.ID
		view.ActivationTerminalEventID = record.ActivationTerminal.ID
		view.ActivationAttemptCount = record.ActivationAttemptCount
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool { return views[i].DeliveryID < views[j].DeliveryID })
	return views
}

func (s *ledgerState) deliveryView(deliveryID string) (DeliveryView, bool) {
	for _, view := range s.deliveryViews() {
		if view.DeliveryID == deliveryID {
			return view, true
		}
	}
	return DeliveryView{}, false
}

func (s *ledgerState) actionableEvent(id string) (Event, bool) {
	event, ok := s.events[id]
	if !ok {
		return Event{}, false
	}
	if _, semanticOK := semanticDeliveredEvent(event, s.events); semanticOK {
		return event, true
	}
	if event.Type != "case_escalation_prepared" && event.Type != "report_prepared" && event.Type != "issue_prepared" && event.Type != "message_prepared" {
		return Event{}, false
	}
	record := s.deliveries[event.DeliveryID]
	if record == nil || record.Status != deliverySent || record.Terminal.ID == "" {
		return Event{}, false
	}
	if _, semanticOK := semanticDeliveredEvent(record.Terminal, s.events); semanticOK {
		return record.Terminal, true
	}
	return Event{}, false
}
