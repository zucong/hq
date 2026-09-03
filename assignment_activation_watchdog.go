package main

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func sameLiveIncarnation(left, right LiveBinding) bool {
	return left.WorkspaceID == right.WorkspaceID && left.TabID == right.TabID && left.PaneID == right.PaneID &&
		left.TerminalID == right.TerminalID && left.Revision == right.Revision && left.Seat == right.Seat &&
		left.Kind == right.Kind && left.CWD == right.CWD
}

func assignmentActivationReadyStatus(status string) bool {
	return status == "idle" || status == "done"
}

func terminalReadyForAssignmentActivation(kind string, raw []byte) bool {
	if kind != "codex" {
		return true
	}
	if terminalShowsContentSafeguard(raw) {
		return false
	}
	text := string(raw)
	prompt := strings.LastIndex(text, "Ask Codex to do anything")
	if prompt < 0 {
		return false
	}
	tail := strings.ToLower(text[prompt:])
	return !strings.Contains(tail, "do you trust") && !strings.Contains(tail, "hook trust") &&
		!strings.Contains(tail, "this content can't be shown")
}

func activationLastAt(record *deliveryRecord) Event {
	if record.ActivationTerminal.ID != "" {
		return record.ActivationTerminal
	}
	if record.ActivationAttempt.ID != "" {
		return record.ActivationAttempt
	}
	return record.Terminal
}

func (a *App) appendAssignmentActivationTerminal(deliveryID, attemptID, eventType, state string, actor Actor, cause error) error {
	commandID := stableCommandID("assignment-activation-terminal", deliveryID, attemptID, eventType)
	digest := requestDigest("assignment-activation-terminal", deliveryID, attemptID, eventType, state)
	_, err := a.Store.Transact(a.Config, commandID, digest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.deliveries[deliveryID]
		if record == nil || record.ActivationAttempt.ID != attemptID {
			return Event{}, conflictf("delivery %s activation attempt 已变化；读取=%s 当前=%s", deliveryID, attemptID, func() string {
				if record == nil {
					return "missing"
				}
				return record.ActivationAttempt.ID
			}())
		}
		assignment := ledger.assignments[record.Terminal.ID]
		if assignment == nil {
			return Event{}, fmt.Errorf("delivery %s 缺少 assignment", deliveryID)
		}
		event, err := a.newOperationsEvent(actor, eventType, record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		populateAssignmentActivationEvent(&event, record, assignment)
		event.AttemptEventID, event.Delivery = attemptID, state
		if cause != nil {
			event.Note = truncateError(cause)
		}
		return event, nil
	})
	return err
}

func (a *App) markStaleAssignmentActivationUnknown(deliveryID, attemptID string) error {
	ledger, err := a.ledgerState()
	if err != nil {
		return err
	}
	record := ledger.deliveries[deliveryID]
	if record == nil {
		return fmt.Errorf("delivery 不存在：%s", deliveryID)
	}
	actor := Actor{Name: record.ActivationAttempt.Actor, Label: record.ActivationAttempt.ActorLabel, PaneID: record.ActivationAttempt.ActorPaneID}
	return a.appendAssignmentActivationTerminal(deliveryID, attemptID, "assignment_activation_unknown", activationUnknown, actor,
		fmt.Errorf("gateway 发现 activation attempted 无终态；无法证明 Prompt 是否发生"))
}

func (a *App) markAssignmentActivationExhausted(deliveryID string, max int) error {
	commandID := stableCommandID("assignment-activation-exhausted", deliveryID)
	digest := requestDigest("assignment-activation-exhausted", deliveryID, fmt.Sprint(max))
	_, err := a.Store.Transact(a.Config, commandID, digest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.deliveries[deliveryID]
		if record == nil {
			return Event{}, fmt.Errorf("delivery 不存在：%s", deliveryID)
		}
		assignment := ledger.assignments[record.Terminal.ID]
		if assignment == nil {
			return Event{}, fmt.Errorf("delivery %s 缺少 assignment", deliveryID)
		}
		actor := Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}
		event, err := a.newOperationsEvent(actor, "assignment_activation_exhausted", record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		populateAssignmentActivationEvent(&event, record, assignment)
		event.Note = fmt.Sprintf("员工在 %d 次同 assignment 激活重投后仍未 accept；停止自动重投", max)
		return event, nil
	})
	return err
}

func (a *App) activateIssuedAssignmentOnce(ctx context.Context, deliveryID string, force bool, requestedActor Actor) error {
	releaseDelivery, err := a.lockOperation(operationScopeDelivery, deliveryID)
	if err != nil {
		return err
	}
	defer releaseDelivery()

	ledger, err := a.ledgerState()
	if err != nil {
		return err
	}
	record := ledger.deliveries[deliveryID]
	if record == nil || record.Origin.Type != "issue_prepared" || record.Status != deliverySent {
		return nil
	}
	assignment := ledger.assignments[record.Terminal.ID]
	if assignment == nil || assignment.Status != "issued" || assignment.Consumed {
		return nil
	}
	if record.ActivationStatus == activationAttempted {
		return a.markStaleAssignmentActivationUnknown(deliveryID, record.ActivationAttempt.ID)
	}
	if record.ActivationStatus == activationUnknown || (record.ActivationStatus == activationExhausted && !force) {
		return nil
	}
	last := activationLastAt(record)
	if !force && a.operationsNow().Before(mustEventTime(last).Add(a.Config.assignmentAcceptTimeout())) {
		return nil
	}
	max := a.Config.effectiveDeliveryPolicy().MaxActivationRedeliveries
	if !force && record.ActivationAttemptCount >= max {
		if err := a.markAssignmentActivationExhausted(deliveryID, max); err != nil {
			return err
		}
		fmt.Fprintf(a.Err, "[HQ assignment activation] delivery=%s assignment=%s 自动激活额度已耗尽；经理核验后运行 hq delivery retry --id %s，不要新建 assignment\n", deliveryID, assignment.AssignmentID, deliveryID)
		return nil
	}

	releaseSeat, err := a.lockRuntimeSeat(assignment.Recipient)
	if err != nil {
		return err
	}
	defer releaseSeat()

	var runErr error
	decision, admissionErr := a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: assignment.Recipient}, func() error {
		if err := a.verifyCurrentFrozenSeat(frozenSeatFromAssignment(assignment)); err != nil {
			return err
		}
		snapshot, err := a.herdrSnapshot(ctx)
		if err != nil {
			return err
		}
		binding, err := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: assignment.Recipient, RequireInteractiveReady: true})
		if err != nil || !assignmentActivationReadyStatus(binding.Status) {
			return nil
		}
		sessionEvents, err := a.Sessions.List(SessionFilter{Agent: assignment.Recipient})
		if err != nil {
			return err
		}
		if _, err := activeSessionForBinding(sessionEvents, binding); err != nil {
			return nil
		}
		if reader, ok := a.Herdr.(HerdrAgentReader); ok {
			raw, readErr := reader.ReadAgent(ctx, assignment.Recipient)
			if readErr != nil || !terminalReadyForAssignmentActivation(binding.Kind, raw) {
				return nil
			}
		} else if binding.Kind == "codex" {
			return nil
		}

		payload, err := a.deliveryPayload(record.Origin)
		if err != nil {
			return err
		}
		if digestText(payload) != record.Origin.PayloadDigest {
			return fmt.Errorf("delivery %s 原始 payload digest 已漂移", deliveryID)
		}
		attemptNo := record.ActivationAttemptCount + 1
		activationActor := requestedActor
		if activationActor.Name == "" {
			activationActor = Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}
		}
		commandID := stableCommandID("assignment-activation-attempt", deliveryID, fmt.Sprint(attemptNo))
		digest := requestDigest("assignment-activation-attempt", activationActor.Name, deliveryID, assignment.EventID, fmt.Sprint(attemptNo), record.Origin.PayloadDigest)
		result, err := a.Store.Transact(a.Config, commandID, digest, false, func(current *ledgerState) (Event, error) {
			fresh := current.deliveries[deliveryID]
			if fresh == nil || fresh.ActivationAttemptCount != attemptNo-1 || fresh.ActivationStatus == activationUnknown ||
				(fresh.ActivationStatus == activationExhausted && !force) {
				return Event{}, conflictf("delivery %s activation 状态已变化，请重新读取", deliveryID)
			}
			freshAssignment := current.assignments[fresh.Terminal.ID]
			if freshAssignment == nil || freshAssignment.Status != "issued" {
				return Event{}, conflictf("assignment 已被 accept 或不存在，无需重投")
			}
			event, err := a.newOperationsEvent(activationActor, "assignment_activation_attempted", fresh.Origin.CaseID)
			if err != nil {
				return Event{}, err
			}
			populateAssignmentActivationEvent(&event, fresh, freshAssignment)
			event.Delivery = deliveryAttempted
			return event, nil
		})
		if err != nil {
			return err
		}
		if result.AlreadyCommitted {
			return nil
		}
		attemptID := result.Event.ID
		if a.DeliveryFailpoint != nil {
			if err := a.DeliveryFailpoint("after_assignment_activation_attempt_recorded"); err != nil {
				return err
			}
		}

		freshSnapshot, err := a.herdrSnapshot(ctx)
		if err != nil {
			return a.appendAssignmentActivationTerminal(deliveryID, attemptID, "assignment_activation_failed_pre_send", activationFailedPreSend, activationActor, err)
		}
		freshBinding, err := ResolveLiveBinding(freshSnapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: assignment.Recipient, RequireInteractiveReady: true})
		if err != nil || !assignmentActivationReadyStatus(freshBinding.Status) || !sameLiveIncarnation(binding, freshBinding) {
			if err == nil {
				err = fmt.Errorf("assignment activation 前 runtime 不再是同一 idle/done incarnation")
			}
			return a.appendAssignmentActivationTerminal(deliveryID, attemptID, "assignment_activation_failed_pre_send", activationFailedPreSend, activationActor, err)
		}
		mutation := a.Herdr.Prompt(ctx, assignment.Recipient, payload)
		switch mutation.Outcome {
		case herdrConfirmed:
			if mutation.Err != nil {
				return a.appendAssignmentActivationTerminal(deliveryID, attemptID, "assignment_activation_unknown", activationUnknown, activationActor, mutation.Err)
			}
			return a.appendAssignmentActivationTerminal(deliveryID, attemptID, "assignment_activation_sent", activationSent, activationActor, nil)
		case herdrDefinitelyNotRun:
			return a.appendAssignmentActivationTerminal(deliveryID, attemptID, "assignment_activation_failed_pre_send", activationFailedPreSend, activationActor, mutation.Err)
		default:
			return a.appendAssignmentActivationTerminal(deliveryID, attemptID, "assignment_activation_unknown", activationUnknown, activationActor, mutation.Err)
		}
	})
	if !decision.Allowed {
		return nil
	}
	if admissionErr != nil {
		runErr = admissionErr
	}
	return runErr
}

func (a *App) recoverIssuedAssignmentActivationsOnce(ctx context.Context) error {
	if a.Herdr == nil || a.Sessions == nil {
		return nil
	}
	ledger, err := a.ledgerState()
	if err != nil {
		return err
	}
	var ids []string
	for id, record := range ledger.deliveries {
		if record.Origin.Type != "issue_prepared" || record.Status != deliverySent || record.Terminal.ID == "" {
			continue
		}
		assignment := ledger.assignments[record.Terminal.ID]
		if assignment != nil && assignment.Status == "issued" && !assignment.Consumed {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	var failures []string
	for _, id := range ids {
		if err := a.activateIssuedAssignmentOnce(ctx, id, false, Actor{}); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", id, err))
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("assignment activation watchdog：%s", strings.Join(failures, "; "))
	}
	return nil
}
