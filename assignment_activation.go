package main

import "fmt"

const (
	activationAttempted     = "attempted"
	activationSent          = "sent"
	activationFailedPreSend = "failed_pre_send"
	activationUnknown       = "unknown"
	activationExhausted     = "exhausted"
)

func isAssignmentActivationEvent(eventType string) bool {
	switch eventType {
	case "assignment_activation_attempted", "assignment_activation_sent",
		"assignment_activation_failed_pre_send", "assignment_activation_unknown",
		"assignment_activation_resolved_sent", "assignment_activation_resolved_not_sent",
		"assignment_activation_exhausted":
		return true
	default:
		return false
	}
}

func validateAssignmentActivationRequiredFields(event Event) error {
	require := func(fields ...struct{ name, value string }) error { return requireEventFields(event, fields...) }
	common := []struct{ name, value string }{
		eventField("related_event_id", event.RelatedEventID),
		eventField("assignment_event_id", event.AssignmentEventID),
		eventField("assignment_id", event.AssignmentID),
		eventField("recipient", event.Recipient),
		eventField("recipient_label", event.RecipientLabel),
		eventField("delivery_id", event.DeliveryID),
		eventField("payload_digest", event.PayloadDigest),
	}
	switch event.Type {
	case "assignment_activation_attempted":
		return require(append(common, eventField("delivery", event.Delivery))...)
	case "assignment_activation_sent", "assignment_activation_failed_pre_send", "assignment_activation_unknown":
		return require(append(common, eventField("attempt_event_id", event.AttemptEventID), eventField("delivery", event.Delivery))...)
	case "assignment_activation_resolved_sent", "assignment_activation_resolved_not_sent":
		return require(append(common, eventField("attempt_event_id", event.AttemptEventID),
			eventField("delivery", event.Delivery), eventField("note", event.Note), eventField("resolution_ref", event.ResolutionRef))...)
	case "assignment_activation_exhausted":
		return require(append(common, eventField("note", event.Note))...)
	default:
		return fmt.Errorf("未知 assignment activation 事件 %q", event.Type)
	}
}

func (s *ledgerState) activationRecord(event Event, actorRule AgentRule) (*deliveryRecord, *caseAssignment, error) {
	if err := validateLedgerID("delivery_id", event.DeliveryID); err != nil {
		return nil, nil, err
	}
	if err := validateDigest("payload_digest", event.PayloadDigest); err != nil {
		return nil, nil, err
	}
	record := s.deliveries[event.DeliveryID]
	if record == nil || record.Origin.Type != "issue_prepared" || record.Status != deliverySent || record.Terminal.ID == "" {
		return nil, nil, fmt.Errorf("delivery %s 不是已送达的 issue", event.DeliveryID)
	}
	assignment := s.assignments[record.Terminal.ID]
	if assignment == nil {
		return nil, nil, fmt.Errorf("delivery %s 缺少对应 assignment", event.DeliveryID)
	}
	if event.RelatedEventID != assignment.EventID || !eventMatchesAssignmentState(event, assignment) {
		return nil, nil, fmt.Errorf("delivery %s 的 activation 与冻结 assignment 合同不一致", event.DeliveryID)
	}
	if event.CaseID != assignment.CaseID || event.Recipient != record.Origin.Recipient ||
		event.RecipientLabel != record.Origin.RecipientLabel || event.PayloadDigest != record.Origin.PayloadDigest {
		return nil, nil, fmt.Errorf("delivery %s 的 activation target/payload/case 已漂移", event.DeliveryID)
	}
	if event.Actor != record.Origin.Actor && !actorRule.CanManageStaff {
		return nil, nil, fmt.Errorf("actor %s 既不是 assignment issuer，也无 can_manage_staff", event.Actor)
	}
	if err := requireNoStateFields(event); err != nil {
		return nil, nil, err
	}
	return record, assignment, nil
}

func (s *ledgerState) applyAssignmentActivationEvent(event Event, actorRule AgentRule) error {
	record, assignment, err := s.activationRecord(event, actorRule)
	if err != nil {
		return err
	}
	switch event.Type {
	case "assignment_activation_attempted":
		if event.Delivery != deliveryAttempted {
			return fmt.Errorf("assignment activation attempted 的 delivery 必须为 attempted")
		}
		if assignment.Status != "issued" {
			return fmt.Errorf("assignment %s 当前为 %s，不需要重新激活", assignment.AssignmentID, assignment.Status)
		}
		if record.ActivationStatus == activationAttempted || record.ActivationStatus == activationUnknown {
			return fmt.Errorf("delivery %s activation 当前为 %s，不可自动重投", event.DeliveryID, record.ActivationStatus)
		}
		record.ActivationStatus, record.ActivationAttempt, record.ActivationTerminal = activationAttempted, event, Event{}
		record.ActivationAttemptCount++
		return nil
	case "assignment_activation_sent", "assignment_activation_failed_pre_send", "assignment_activation_unknown":
		if record.ActivationStatus != activationAttempted || record.ActivationAttempt.ID == "" || event.AttemptEventID != record.ActivationAttempt.ID {
			return fmt.Errorf("delivery %s 没有匹配的 assignment activation attempted", event.DeliveryID)
		}
		expected := map[string]string{
			"assignment_activation_sent":            activationSent,
			"assignment_activation_failed_pre_send": activationFailedPreSend,
			"assignment_activation_unknown":         activationUnknown,
		}[event.Type]
		if event.Delivery != expected {
			return fmt.Errorf("事件 %s 的 delivery 必须为 %s", event.Type, expected)
		}
		if event.Actor != record.ActivationAttempt.Actor {
			return fmt.Errorf("delivery %s activation terminal actor 与 attempted actor 不一致", event.DeliveryID)
		}
		record.ActivationStatus, record.ActivationTerminal = expected, event
		return nil
	case "assignment_activation_resolved_sent", "assignment_activation_resolved_not_sent":
		if !actorRule.CanManageStaff {
			return fmt.Errorf("仅 can_manage_staff 可解除 unknown assignment activation")
		}
		if record.ActivationStatus != activationUnknown || event.AttemptEventID != record.ActivationAttempt.ID {
			return fmt.Errorf("delivery %s activation 当前为 %s，仅 unknown 可 resolve", event.DeliveryID, record.ActivationStatus)
		}
		expected := activationSent
		if event.Type == "assignment_activation_resolved_not_sent" {
			expected = activationFailedPreSend
		}
		if event.Delivery != expected {
			return fmt.Errorf("事件 %s 的 delivery 必须为 %s", event.Type, expected)
		}
		record.ActivationStatus, record.ActivationTerminal = expected, event
		return nil
	case "assignment_activation_exhausted":
		if assignment.Status != "issued" || record.ActivationAttemptCount == 0 ||
			(record.ActivationStatus != activationSent && record.ActivationStatus != activationFailedPreSend) {
			return fmt.Errorf("delivery %s 当前 activation=%s attempts=%d，不可标记 exhausted", event.DeliveryID, record.ActivationStatus, record.ActivationAttemptCount)
		}
		record.ActivationStatus, record.ActivationTerminal = activationExhausted, event
		return nil
	default:
		return fmt.Errorf("未知 assignment activation 事件 %q", event.Type)
	}
}

func populateAssignmentActivationEvent(event *Event, record *deliveryRecord, assignment *caseAssignment) {
	event.RelatedEventID = assignment.EventID
	copyDeliveryEnvelope(event, record.Origin)
	copyAssignmentStateBinding(event, assignment)
}
