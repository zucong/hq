package main

import (
	"fmt"
	"strings"
)

type DeliveryOutcome struct {
	CommandCommitted bool                  `json:"command_committed"`
	CaseStateApplied bool                  `json:"case_state_applied"`
	BusinessOutcome  string                `json:"business_outcome"`
	BusinessEventID  string                `json:"business_event_id"`
	DeliveryID       string                `json:"delivery_id"`
	DeliveryStatus   string                `json:"delivery_status"`
	RecoveryAction   string                `json:"recovery_action,omitempty"`
	DeliveryMode     string                `json:"delivery_mode"`
	DeliveryTarget   string                `json:"delivery_target"`
	Wakeup           bool                  `json:"wakeup"`
	HQMessages       []DeliveryContextItem `json:"hq_messages,omitempty"`
	ActorDirective   *ActorDirective       `json:"actor_directive,omitempty"`
}

type DeliveryOutcomeError struct {
	Outcome DeliveryOutcome
	Cause   error
}

func (e *DeliveryOutcomeError) Error() string {
	prefix := "投递请求已记账，case 业务状态尚未推进"
	if e.Outcome.CaseStateApplied {
		prefix = "case 业务状态已提交"
	}
	return fmt.Sprintf("%s event=%s；delivery=%s 状态=%s；恢复动作=%s；原因=%v", prefix,
		e.Outcome.BusinessEventID, e.Outcome.DeliveryID, e.Outcome.DeliveryStatus,
		e.Outcome.RecoveryAction, e.Cause)
}

func (e *DeliveryOutcomeError) Unwrap() error { return e.Cause }

func requestDigest(kind string, parts ...string) string {
	return digestText(strings.Join(append([]string{kind}, parts...), "\x00"))
}

func newDeliveryOutcome(origin Event, status string) DeliveryOutcome {
	caseStateApplied := origin.Type == "event_accepted" || origin.Type == "event_returned"
	business := "delivery_prepared_committed"
	if caseStateApplied {
		business = "case_state_committed"
	}
	return DeliveryOutcome{
		CommandCommitted: true, CaseStateApplied: caseStateApplied, BusinessOutcome: business,
		BusinessEventID: origin.ID, DeliveryID: origin.DeliveryID, DeliveryStatus: status,
		DeliveryMode: effectiveEventDeliveryMode(origin), DeliveryTarget: effectiveEventDeliveryTarget(origin),
		Wakeup: eventWakesTarget(origin),
	}
}

func deliveryAppliesCaseState(origin Event) bool {
	return origin.Type != "message_prepared"
}

// deliveryMayActivateSeat answers whether this durable business delivery is
// itself authority to start an offline seat. on_assignment is a contract
// policy, not an issue-only policy: the original issue starts the contract,
// and a return or manager action message may resume the same still-active
// contract after its runtime has hibernated.
func (a *App) deliveryMayActivateSeat(origin Event, mode string) (bool, error) {
	if mode != deliveryModeWakeup {
		return false, nil
	}
	rule, ok := a.Config.exactRule(origin.Recipient)
	if !ok {
		return false, nil
	}
	switch rule.ActivationPolicy {
	case activationAlways:
		return true, nil
	case activationOnAssignment:
		if origin.Type == "issue_prepared" {
			return true, nil
		}
		ledger, err := a.ledgerState()
		if err != nil {
			return false, fmt.Errorf("核验 on_assignment 唤醒合同：%w", err)
		}
		assignment, ok, err := assignmentAuthorizingResume(ledger, origin)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		if err := a.verifyCurrentFrozenSeat(frozenSeatFromAssignment(assignment)); err != nil {
			return false, fmt.Errorf("on_assignment 唤醒前冻结席位核验失败：%w", err)
		}
		return true, nil
	default:
		return false, nil
	}
}

// assignmentAuthorizingResume deliberately recognizes only two continuations
// of an existing contract. A report return carries the exact frozen assignment
// binding. An actionable manager message must name the same case and assignee,
// and its sender must be one of the authority seats frozen by that contract.
// Informational or unrelated messages never acquire runtime-start authority.
func assignmentAuthorizingResume(ledger *ledgerState, origin Event) (*caseAssignment, bool, error) {
	if ledger == nil || origin.CaseID == "" || origin.Recipient == "" {
		return nil, false, nil
	}
	switch origin.Type {
	case "event_returned":
		if origin.AssignmentEventID == "" || origin.Recipient == origin.AssignmentIssuer {
			return nil, false, nil
		}
		assignment := ledger.assignments[origin.AssignmentEventID]
		if assignment == nil || assignment.Consumed || assignment.Status != "rework" ||
			assignment.CaseID != origin.CaseID || assignment.Recipient != origin.Recipient {
			return nil, false, nil
		}
		if !eventMatchesAssignmentState(origin, assignment) {
			return nil, false, fmt.Errorf("返工 delivery=%s 与 active assignment=%s 的冻结合同不一致",
				origin.DeliveryID, assignment.AssignmentID)
		}
		if origin.Actor != assignment.Reviewer && origin.Actor != assignment.Acceptor {
			return nil, false, fmt.Errorf("返工 delivery=%s 的 actor=%s 不是 assignment=%s 的 reviewer/acceptor",
				origin.DeliveryID, origin.Actor, assignment.AssignmentID)
		}
		return assignment, true, nil

	case "message_prepared":
		if !messageNeedsAction(origin.MessageKind) {
			return nil, false, nil
		}
		matches := make([]*caseAssignment, 0, 1)
		for _, assignment := range ledger.activeAssignments(origin.CaseID) {
			if assignment.Recipient != origin.Recipient {
				continue
			}
			if origin.Actor != assignment.Issuer && origin.Actor != assignment.Reviewer && origin.Actor != assignment.Acceptor {
				continue
			}
			matches = append(matches, assignment)
		}
		if len(matches) > 1 {
			return nil, false, fmt.Errorf("行动消息 delivery=%s 对 case=%s、员工=%s 匹配到 %d 个 active assignment；先运行 hq assignment list --case %s 收敛冲突",
				origin.DeliveryID, origin.CaseID, origin.Recipient, len(matches), origin.CaseID)
		}
		if len(matches) == 1 {
			return matches[0], true, nil
		}
	}
	return nil, false, nil
}

func (a *App) transact(commandID, digest string, build TransactionBuilder) (TransactionResult, error) {
	if a.Store == nil {
		return TransactionResult{}, fmt.Errorf("Store 未注入")
	}
	return a.Store.Transact(a.Config, commandID, digest, a.DryRun, build)
}

func (a *App) deliveryPayload(event Event) (string, error) {
	ref := a.Store.EventRef(event)
	switch event.Type {
	case "case_escalation_prepared":
		return formatCaseEscalationEnvelope(event, ref), nil
	case "issue_prepared":
		return formatIssueEnvelope(event, ref)
	case "message_prepared":
		return formatMessageEnvelope(event, ref)
	case "report_prepared":
		return formatReportMessage(Actor{Name: event.Actor, Label: event.ActorLabel}, event, ref), nil
	case "event_accepted":
		return fmt.Sprintf("[HQ notification][%s] CASE=%s EVENT=%s DELIVERY=%s：已核验接收；下一步：%s；账本：%s",
			event.ActorLabel, event.CaseID, event.ID, event.DeliveryID, event.NextAction, ref), nil
	case "event_returned":
		return fmt.Sprintf("[HQ notification][%s] CASE=%s EVENT=%s DELIVERY=%s：交接退回；原因：%s；复交条件：%s；账本：%s",
			event.ActorLabel, event.CaseID, event.ID, event.DeliveryID, event.Note, event.NextAction, ref), nil
	default:
		return "", fmt.Errorf("事件类型 %s 不是 outbox origin", event.Type)
	}
}

func (a *App) processDelivery(origin Event, retryCommand string) (DeliveryOutcome, error) {
	releaseOperation, err := a.lockOperation(operationScopeDelivery, origin.DeliveryID)
	if err != nil {
		return DeliveryOutcome{}, err
	}
	defer releaseOperation()
	return a.processDeliveryLocked(origin, retryCommand)
}

func (a *App) processDeliveryLocked(origin Event, retryCommand string) (DeliveryOutcome, error) {
	view, ok, err := a.Store.Delivery(a.Config, origin.DeliveryID)
	if err != nil {
		return DeliveryOutcome{}, err
	}
	if !ok {
		return DeliveryOutcome{}, fmt.Errorf("delivery 不存在：%s", origin.DeliveryID)
	}
	outcome := newDeliveryOutcome(origin, view.Status)
	silent := false
	switch view.Status {
	case deliverySent:
		outcome.CaseStateApplied = deliveryAppliesCaseState(origin)
		if outcome.CaseStateApplied {
			outcome.BusinessOutcome = "case_state_committed"
		} else {
			outcome.BusinessOutcome = "message_delivered"
		}
		return outcome, nil
	case deliveryUnknown:
		outcome.RecoveryAction = "运维白名单核对接收方后显式 resolve；禁止自动重投"
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("delivery 结果不确定")}
	case deliveryAttempted:
		if !eventWakesTarget(origin) {
			terminal, terminalErr := a.appendDeliveryTerminal(origin, Event{ID: view.AttemptEventID}, deliverySent, nil)
			if terminalErr != nil {
				return outcome, terminalErr
			}
			outcome.DeliveryStatus, outcome.BusinessOutcome = terminal.Delivery, "quiet_delivered"
			return outcome, nil
		}
		return a.markDeliveryUnknown(origin, view.AttemptEventID, fmt.Errorf("发现 attempted 无终态；保守冻结"))
	case deliveryQueued:
		outcome.BusinessOutcome = "accepted_queued"
		return outcome, nil
	case deliveryFailedPreSend:
		if retryCommand == "" {
			outcome.RecoveryAction = "使用 delivery retry 并复用同一 delivery_id"
			return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("transport 已确证未投递")}
		}
	case deliveryPrepared:
		if retryCommand == "" {
			retryCommand = "delivery-attempt:" + origin.DeliveryID
		}
	default:
		return outcome, fmt.Errorf("未知 delivery 状态：%s", view.Status)
	}

	mode := effectiveEventDeliveryMode(origin)
	if mode == deliveryModeInject && view.Status == deliveryPrepared {
		queued, err := a.appendDeliveryQueued(origin)
		if err != nil {
			return outcome, err
		}
		outcome.DeliveryStatus, outcome.BusinessOutcome = queued.Delivery, "accepted_queued"
		if a.DeliveryFailpoint != nil {
			if err := a.DeliveryFailpoint("after_silent_queued"); err != nil {
				outcome.RecoveryAction = "重试相同 message；稳定 delivery_id 与目标 pending 去重"
				return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: err}
			}
		}
		return outcome, nil
	}
	if mode == deliveryModeQuiet && view.Status == deliveryPrepared {
		state, err := a.inspectDeliveryTarget(origin.Recipient)
		if err != nil {
			return outcome, err
		}
		if state == deliveryRuntimeOffline {
			queued, err := a.appendDeliveryQueued(origin)
			if err != nil {
				return outcome, err
			}
			outcome.DeliveryStatus, outcome.BusinessOutcome = queued.Delivery, "accepted_queued"
			if a.DeliveryFailpoint != nil {
				if err := a.DeliveryFailpoint("after_silent_queued"); err != nil {
					outcome.RecoveryAction = "重试相同 message；稳定 delivery_id 与目标 pending 去重"
					return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: err}
				}
			}
			return outcome, nil
		}
		silent = true
	}
	// Actionable non-wakeup messages participate in the same seat lifecycle
	// serialization even though they do not Prompt. Once their durable origin
	// exists, a reaper that wins next must observe it as a blocker; if the
	// reaper won first, delivery observes the now-offline runtime and queues.
	serializeRuntimeSeat := eventWakesTarget(origin) ||
		(origin.Type == "message_prepared" && messageNeedsAction(origin.MessageKind))
	if serializeRuntimeSeat {
		releaseRuntimeSeat, lockErr := a.lockRuntimeSeat(origin.Recipient)
		if lockErr != nil {
			return outcome, lockErr
		}
		defer releaseRuntimeSeat()
	}
	if eventWakesTarget(origin) {
		var admittedOutcome DeliveryOutcome
		decision, executionErr := a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: origin.Recipient}, func() error {
			var err error
			admittedOutcome, err = a.processDeliveryAfterRuntimeAdmission(origin, retryCommand, outcome, silent, mode)
			return err
		})
		if !decision.Allowed {
			return a.recordDeliveryAdmissionDenied(origin, retryCommand, executionErr)
		}
		return admittedOutcome, executionErr
	}
	return a.processDeliveryAfterRuntimeAdmission(origin, retryCommand, outcome, silent, mode)
}

func (a *App) processDeliveryAfterRuntimeAdmission(origin Event, retryCommand string, outcome DeliveryOutcome, silent bool, mode string) (DeliveryOutcome, error) {
	if eventWakesTarget(origin) {
		rule, ok := a.Config.exactRule(origin.Recipient)
		if !ok {
			return outcome, fmt.Errorf("投递目标员工席位已停用或不存在：%s", origin.Recipient)
		}
		if _, err := verifyAgentRoleCardArtifact(a.Config, a.HQRoot, rule); err != nil {
			return outcome, fmt.Errorf("投递前角色卡/手册核验失败：%w", err)
		}
		if err := a.ensureRuntimeSeatOriginSafeLocked(origin.Recipient); err != nil {
			return outcome, err
		}
	}
	if origin.Type == "issue_prepared" {
		if err := a.verifyCurrentFrozenSeat(frozenSeatFromEvent(origin)); err != nil {
			return outcome, fmt.Errorf("issue 投递前冻结席位核验失败：%w", err)
		}
	}
	// A report return targets the original assignee and reopens the same frozen
	// assignment. An issue return instead targets its issuer, which is why the
	// assignment_issuer distinction is required here.
	if origin.Type == "event_returned" && origin.AssignmentEventID != "" && origin.Recipient != origin.AssignmentIssuer {
		if err := a.verifyCurrentFrozenSeat(frozenSeatFromEvent(origin)); err != nil {
			return outcome, fmt.Errorf("返工通知投递前冻结席位核验失败：%w", err)
		}
	}
	basePayload, err := a.deliveryPayload(origin)
	if err != nil {
		return outcome, err
	}
	if digestText(basePayload) != origin.PayloadDigest {
		return outcome, fmt.Errorf("delivery %s payload digest 与 origin 不一致", origin.DeliveryID)
	}
	initialRuntimeState := deliveryRuntimeState("")
	if eventWakesTarget(origin) {
		// Runtime admission answers whether this class of mutation is allowed;
		// exact live binding answers whether the registered seat is the process
		// that would actually receive it. Check before recording an attempt so a
		// wrong-kind/CWD/incarnation/not-ready occupant gets zero transport and a
		// repair can reuse the same prepared delivery.
		initialRuntimeState, err = a.inspectDeliveryTarget(origin.Recipient)
		if err != nil {
			return outcome, fmt.Errorf("投递前 live binding 核验失败：%w", err)
		}
		coldResumable, activationErr := a.deliveryMayActivateSeat(origin, mode)
		if activationErr != nil {
			return outcome, fmt.Errorf("投递前 runtime 激活授权核验失败：%w", activationErr)
		}
		if initialRuntimeState == deliveryRuntimeOffline && !coldResumable {
			return outcome, fmt.Errorf("投递前 live binding 核验失败：目标 %s 离线，且当前 delivery 没有启动该席位的合同授权。先运行 hq assignment list --assignee %s 核验未完成合同；若存在，由冻结 issuer/reviewer/acceptor 针对该 assignment 的 case 运行 hq message --to %s --case CASE_ID --kind request --text TEXT --delivery wakeup；若不存在，由 case owner 先运行 hq issue 建立合法 assignment。目标在线后运行 hq reconcile，复用已记账 delivery=%s；不要重发原业务，也不要裸用 herdr prompt",
				origin.Recipient, origin.Recipient, origin.Recipient, origin.DeliveryID)
		}
	}
	promptPayload := basePayload
	attemptDigest := requestDigest("delivery-attempt", origin.DeliveryID, origin.Recipient, origin.PayloadDigest, retryCommand)
	attemptResult, err := a.Store.Transact(a.Config, retryCommand, attemptDigest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.deliveries[origin.DeliveryID]
		if record == nil {
			return Event{}, fmt.Errorf("delivery 不存在：%s", origin.DeliveryID)
		}
		currentPayload, err := a.deliveryPayload(record.Origin)
		if err != nil {
			return Event{}, err
		}
		if record.Origin.Recipient != origin.Recipient || record.Origin.PayloadDigest != origin.PayloadDigest || digestText(currentPayload) != record.Origin.PayloadDigest {
			return Event{}, fmt.Errorf("同 delivery_id 的 target/payload 已变化")
		}
		currentPrompt := currentPayload
		currentBundle := deliveryContextBatch{}
		if eventWakesTarget(record.Origin) {
			currentBundle, err = a.prepareTurnBundle(ledger, record.Origin.Recipient, a.Config.effectiveDeliveryPolicy())
			if err != nil {
				return Event{}, fmt.Errorf("准备有界 turn bundle：%w", err)
			}
			currentPrompt = mergeDeliveryPrompt(currentPayload, currentBundle.Envelopes)
		}
		event, err := a.newEvent(Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}, "delivery_attempted", record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		event.RelatedEventID = record.Origin.ID
		copyDeliveryEnvelope(&event, record.Origin)
		event.Delivery = deliveryAttempted
		if eventWakesTarget(record.Origin) {
			populateTurnBundleManifest(&event, record.Origin, currentBundle, currentPayload, currentPrompt)
		}
		promptPayload = currentPrompt
		return event, nil
	})
	if err != nil {
		return outcome, err
	}
	if attemptResult.AlreadyCommitted {
		fresh, ok, freshErr := a.Store.Delivery(a.Config, origin.DeliveryID)
		if freshErr != nil {
			return outcome, freshErr
		}
		if ok && fresh.AttemptEventID == attemptResult.Event.ID {
			switch fresh.Status {
			case deliverySent:
				outcome.DeliveryStatus = deliverySent
				outcome.CaseStateApplied = deliveryAppliesCaseState(origin)
				if outcome.CaseStateApplied {
					outcome.BusinessOutcome = "case_state_committed"
				} else {
					outcome.BusinessOutcome = "message_delivered"
				}
				return outcome, nil
			case deliveryFailedPreSend:
				outcome.DeliveryStatus = deliveryFailedPreSend
				outcome.RecoveryAction = "该 retry command 已确证未送达；使用新的稳定 retry command 再试"
				return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("retry command 已提交且确证未投递")}
			case deliveryUnknown:
				outcome.DeliveryStatus = deliveryUnknown
				outcome.RecoveryAction = "运维白名单人工核对并 resolve；禁止自动重投"
				return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("retry command 已冻结为 unknown")}
			}
		}
		return a.markDeliveryUnknown(origin, attemptResult.Event.ID, fmt.Errorf("attempt command 已提交但无可证明终态"))
	}
	if a.DeliveryFailpoint != nil {
		if err := a.DeliveryFailpoint("after_attempt_recorded"); err != nil {
			outcome.DeliveryStatus = deliveryAttempted
			outcome.RecoveryAction = "重启 reconcile 将转 unknown，禁止自动重投"
			return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: err}
		}
	}
	if silent {
		terminal, err := a.appendDeliveryTerminal(origin, attemptResult.Event, deliverySent, nil)
		if err != nil {
			outcome.DeliveryStatus = deliveryAttempted
			outcome.RecoveryAction = "静默 history 终态落账失败；重启转 unknown 后人工核对"
			return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: err}
		}
		outcome.DeliveryStatus, outcome.BusinessOutcome = terminal.Delivery, "quiet_delivered"
		outcome.CaseStateApplied = deliveryAppliesCaseState(origin)
		return outcome, nil
	}
	failedBeforePrompt := func(cause error, recovery string) (DeliveryOutcome, error) {
		_, terminalErr := a.appendDeliveryTerminal(origin, attemptResult.Event, deliveryFailedPreSend, cause)
		outcome.DeliveryStatus = deliveryFailedPreSend
		outcome.RecoveryAction = recovery
		if terminalErr != nil {
			return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("pre-prompt=%v；终态落账=%w", cause, terminalErr)}
		}
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: cause}
	}
	// Seat activation is a registry policy: always-on seats may be restored by a
	// wakeup; on-assignment seats require either the durable issue or a verified
	// continuation of the same active assignment contract. Manual seats and
	// every unbound business event require an existing runtime binding.
	// Start success alone is never proof that the same incarnation remains bound.
	if eventWakesTarget(origin) {
		if initialRuntimeState == deliveryRuntimeOffline {
			if resumeErr := a.coldResumeDeliveryTargetAdmitted(origin.Recipient); resumeErr != nil {
				return failedBeforePrompt(resumeErr, "目标 cold-resume 未完成；显式 delivery retry")
			}
		}
		state, inspectErr := a.inspectDeliveryTarget(origin.Recipient)
		if inspectErr != nil {
			return failedBeforePrompt(fmt.Errorf("最终 live binding 核验失败：%w", inspectErr), "修复目标 binding 后显式 delivery retry")
		}
		if state == deliveryRuntimeOffline {
			return failedBeforePrompt(fmt.Errorf("最终 live binding 核验失败：目标 %s 离线", origin.Recipient), "恢复目标席位后显式 delivery retry")
		}
	}
	attempt := a.deliverAttempt(origin.Recipient, promptPayload)
	switch attempt.Outcome {
	case transportSent:
		if a.DeliveryFailpoint != nil {
			if err := a.DeliveryFailpoint("after_transport_sent"); err != nil {
				outcome.DeliveryStatus = deliveryAttempted
				outcome.RecoveryAction = "重启 reconcile 将转 unknown；人工核对，不得重投"
				return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: err}
			}
		}
		terminal, err := a.appendDeliveryTerminal(origin, attemptResult.Event, deliverySent, nil)
		if err != nil {
			outcome.DeliveryStatus = deliveryAttempted
			outcome.RecoveryAction = "终态落账失败；重启转 unknown 后人工核对"
			return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: err}
		}
		outcome.DeliveryStatus = terminal.Delivery
		outcome.CaseStateApplied = deliveryAppliesCaseState(origin)
		if outcome.CaseStateApplied {
			outcome.BusinessOutcome = "case_state_committed"
		} else {
			outcome.BusinessOutcome = "message_delivered"
		}
		return outcome, nil
	case transportDefinitelyNotSent:
		_, terminalErr := a.appendDeliveryTerminal(origin, attemptResult.Event, deliveryFailedPreSend, attempt.Err)
		outcome.DeliveryStatus = deliveryFailedPreSend
		outcome.RecoveryAction = "显式 delivery retry；复用同一 delivery_id"
		if terminalErr != nil {
			return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("transport=%v；终态落账=%w", attempt.Err, terminalErr)}
		}
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: attempt.Err}
	case transportAmbiguous:
		return a.markDeliveryUnknown(origin, attemptResult.Event.ID, attempt.Err)
	default:
		return a.markDeliveryUnknown(origin, attemptResult.Event.ID, fmt.Errorf("transport 返回未知 outcome %q：%v", attempt.Outcome, attempt.Err))
	}
}

func (a *App) recordDeliveryAdmissionDenied(origin Event, retryCommand string, cause error) (DeliveryOutcome, error) {
	outcome := newDeliveryOutcome(origin, deliveryFailedPreSend)
	outcome.RecoveryAction = "ESTOP 显式 release 后使用 delivery retry；复用同一 delivery_id"
	if retryCommand == "" {
		retryCommand = "delivery-attempt:" + origin.DeliveryID
	}
	digest := requestDigest("delivery-attempt", origin.DeliveryID, origin.Recipient, origin.PayloadDigest, retryCommand)
	result, err := a.transactBatch(retryCommand, digest, func(ledger *ledgerState) ([]Event, error) {
		record := ledger.deliveries[origin.DeliveryID]
		if record == nil {
			return nil, fmt.Errorf("delivery 不存在：%s", origin.DeliveryID)
		}
		currentPayload, err := a.deliveryPayload(record.Origin)
		if err != nil {
			return nil, err
		}
		if record.Origin.Recipient != origin.Recipient || record.Origin.PayloadDigest != origin.PayloadDigest || digestText(currentPayload) != record.Origin.PayloadDigest {
			return nil, fmt.Errorf("同 delivery_id 的 target/payload 已变化")
		}
		currentPrompt := currentPayload
		currentBundle := deliveryContextBatch{}
		if eventWakesTarget(record.Origin) {
			currentBundle, err = a.prepareTurnBundle(ledger, record.Origin.Recipient, a.Config.effectiveDeliveryPolicy())
			if err != nil {
				return nil, fmt.Errorf("准备被 admission 拒绝的 turn bundle manifest：%w", err)
			}
			currentPrompt = mergeDeliveryPrompt(currentPayload, currentBundle.Envelopes)
		}
		actor := Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}
		attempt, err := a.newEvent(actor, "delivery_attempted", record.Origin.CaseID)
		if err != nil {
			return nil, err
		}
		attempt.RelatedEventID = record.Origin.ID
		copyDeliveryEnvelope(&attempt, record.Origin)
		attempt.Delivery = deliveryAttempted
		if eventWakesTarget(record.Origin) {
			populateTurnBundleManifest(&attempt, record.Origin, currentBundle, currentPayload, currentPrompt)
		}
		terminal, err := a.newEvent(actor, "delivery_failed_pre_send", record.Origin.CaseID)
		if err != nil {
			return nil, err
		}
		terminal.RelatedEventID = record.Origin.ID
		terminal.AttemptEventID = attempt.ID
		copyDeliveryEnvelope(&terminal, record.Origin)
		terminal.Delivery = deliveryFailedPreSend
		terminal.Note = truncateError(cause)
		return []Event{attempt, terminal}, nil
	})
	if err != nil {
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("runtime admission=%v；failed_pre_send 原子落账=%w", cause, err)}
	}
	if result.DryRun {
		outcome.DeliveryStatus = deliveryPrepared
		outcome.RecoveryAction = "DRY-RUN：ESTOP runtime admission 拒绝，未写终态且未调用 Herdr"
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: cause}
	}
	view, ok, viewErr := a.Store.Delivery(a.Config, origin.DeliveryID)
	if viewErr != nil {
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("runtime admission=%v；读取终态=%w", cause, viewErr)}
	}
	if !ok || view.Status != deliveryFailedPreSend {
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("runtime admission=%v；delivery 未收敛到 failed_pre_send：status=%s", cause, view.Status)}
	}
	outcome.DeliveryStatus = view.Status
	return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: cause}
}

func (a *App) appendDeliveryTerminal(origin, attempt Event, state string, cause error) (Event, error) {
	store := a.durableRecoveryStore()
	commandID := "delivery-terminal:" + attempt.ID + ":" + state
	digest := requestDigest("delivery-terminal", origin.DeliveryID, attempt.ID, state)
	// Every successful wakeup is a proven turn boundary, even when there was no
	// pending quiet/inject context to bundle.  Converge that boundary through a
	// batch so delivery_budget_reset is recorded together with the terminal;
	// otherwise standalone wakeups accumulate forever and eventually strand an
	// actionable message in the silent queue.
	if state == deliverySent && hasTurnBundleManifest(attempt) {
		batchStore, ok := store.(batchEventStore)
		if !ok {
			return Event{}, fmt.Errorf("Store 不支持原子多事件恢复事务，fail-closed")
		}
		result, err := batchStore.TransactBatch(a.Config, commandID, digest, false, func(ledger *ledgerState) ([]Event, error) {
			record := ledger.deliveries[origin.DeliveryID]
			if record == nil || record.Attempt.ID != attempt.ID {
				return nil, fmt.Errorf("delivery %s 缺少匹配的 bundled attempt", origin.DeliveryID)
			}
			children, err := a.buildTurnBundleConvergenceEvents(ledger, record)
			if err != nil {
				return nil, err
			}
			terminal, err := a.newDeliveryTerminalEvent(ledger, record, attempt.ID, state, cause)
			if err != nil {
				return nil, err
			}
			return append(children, terminal), nil
		})
		if err != nil {
			return Event{}, err
		}
		return result.Events[len(result.Events)-1], nil
	}
	result, err := store.Transact(a.Config, commandID, digest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.deliveries[origin.DeliveryID]
		if record == nil {
			return Event{}, fmt.Errorf("delivery 不存在：%s", origin.DeliveryID)
		}
		return a.newDeliveryTerminalEvent(ledger, record, attempt.ID, state, cause)
	})
	return result.Event, err
}

func (a *App) newDeliveryTerminalEvent(ledger *ledgerState, record *deliveryRecord, attemptID, state string, cause error) (Event, error) {
	eventType := "delivery_failed_pre_send"
	if state == deliveryUnknown {
		eventType = "delivery_unknown"
	} else if state == deliverySent {
		switch record.Origin.Type {
		case "issue_prepared":
			eventType = "issue_sent"
		case "case_escalation_prepared":
			eventType = "case_escalation_sent"
		case "message_prepared":
			eventType = "message_sent"
		case "report_prepared":
			eventType = "report_sent"
		case "event_accepted":
			eventType = "accept_notice_sent"
		case "event_returned":
			eventType = "return_notice_sent"
		default:
			return Event{}, fmt.Errorf("未知 outbox origin type %s", record.Origin.Type)
		}
	}
	event, err := a.newEvent(Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}, eventType, record.Origin.CaseID)
	if err != nil {
		return Event{}, err
	}
	event.RelatedEventID, event.AttemptEventID = record.Origin.ID, attemptID
	copyDeliveryEnvelope(&event, record.Origin)
	event.Delivery, event.Note = state, truncateError(cause)
	if state == deliverySent {
		a.fillSentState(&event, record.Origin, ledger)
	}
	return event, nil
}

func (a *App) appendDeliveryQueued(origin Event) (Event, error) {
	commandID := "delivery-queue:" + origin.DeliveryID
	digest := requestDigest("delivery-queue", origin.DeliveryID, origin.Recipient, origin.PayloadDigest,
		effectiveEventDeliveryMode(origin), effectiveEventDeliveryTarget(origin))
	result, err := a.Store.Transact(a.Config, commandID, digest, false, func(ledger *ledgerState) (Event, error) {
		record := ledger.deliveries[origin.DeliveryID]
		if record == nil {
			return Event{}, fmt.Errorf("delivery 不存在：%s", origin.DeliveryID)
		}
		event, err := a.newEvent(Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}, "delivery_queued", record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		copyDeliveryEnvelope(&event, record.Origin)
		event.RelatedEventID, event.Delivery = record.Origin.ID, deliveryQueued
		return event, nil
	})
	return result.Event, err
}

func (a *App) fillSentState(event *Event, origin Event, ledger *ledgerState) {
	state := ledger.snapshot.Cases[origin.CaseID]
	switch origin.Type {
	case "case_escalation_prepared":
		event.FromState, event.ToState, event.Owner = state.Status, string(statusEscalated), origin.Recipient
		event.ParentCaseID, event.RootCaseID = origin.ParentCaseID, origin.RootCaseID
		event.Title, event.Project = origin.Title, origin.Project
		event.SourceRef, event.NextAction, event.Note = origin.SourceRef, origin.NextAction, origin.Note
		event.CaseVersion, event.CaseDigest = origin.CaseVersion, origin.CaseDigest
	case "report_prepared":
		target, _, _ := reportTargetState(origin.Result)
		event.FromState, event.ToState, event.Owner = state.Status, target, origin.Recipient
		event.Title, event.Project, event.Result, event.Severity = origin.Title, origin.Project, origin.Result, origin.Severity
		event.SourceRef, event.ArtifactRef = origin.SourceRef, origin.ArtifactRef
		event.Location, event.Verification, event.NextAction = origin.Location, origin.Verification, origin.NextAction
		copyAssignmentBinding(event, origin)
		event.CaseVersion, event.CaseDigest = origin.CaseVersion, origin.CaseDigest
	case "issue_prepared":
		event.FromState, event.ToState, event.Owner = state.Status, string(statusDispatched), origin.Recipient
		event.CaseVersion, event.CaseDigest = origin.CaseVersion, origin.CaseDigest
		event.AuthorizationType, event.AuthorizationDigest = origin.AuthorizationType, origin.AuthorizationDigest
		event.ApprovalID, event.DecisionRef = origin.ApprovalID, origin.DecisionRef
		event.Issuer, event.CapturedBy = origin.Issuer, origin.CapturedBy
		event.NextAction, event.Note = origin.NextAction, origin.Note
		event.Project = origin.Project
		copyAssignmentBinding(event, origin)
		event.SupersedesAssignmentEventID = origin.SupersedesAssignmentEventID
		event.SupersedesAssignmentID = origin.SupersedesAssignmentID
		event.BasisEventID = origin.BasisEventID
		event.Urgency = origin.Urgency
	case "message_prepared":
		event.MessageKind, event.Urgency, event.Message = origin.MessageKind, origin.Urgency, origin.Message
		event.MessageID, event.SourceRef, event.ThreadID, event.ReplyTo = origin.MessageID, origin.SourceRef, origin.ThreadID, origin.ReplyTo
		event.RefFiles, event.RefCases = append([]string(nil), origin.RefFiles...), append([]string(nil), origin.RefCases...)
		event.RefMessages, event.RefEvents = append([]string(nil), origin.RefMessages...), append([]string(nil), origin.RefEvents...)
		if origin.MessageKind == "directive" {
			event.CaseVersion, event.CaseDigest = origin.CaseVersion, origin.CaseDigest
			copyAssignmentBinding(event, origin)
		}
	}
}

func (a *App) markDeliveryUnknown(origin Event, attemptEventID string, cause error) (DeliveryOutcome, error) {
	view, ok, viewErr := a.durableRecoveryStore().Delivery(a.Config, origin.DeliveryID)
	if viewErr != nil {
		return DeliveryOutcome{}, viewErr
	}
	if ok && view.Status == deliveryUnknown {
		outcome := newDeliveryOutcome(origin, deliveryUnknown)
		outcome.RecoveryAction = "运维白名单人工核对并 resolve"
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: cause}
	}
	if attemptEventID == "" && ok {
		attemptEventID = view.AttemptEventID
	}
	_, terminalErr := a.appendDeliveryTerminal(origin, Event{ID: attemptEventID}, deliveryUnknown, cause)
	outcome := newDeliveryOutcome(origin, deliveryUnknown)
	outcome.RecoveryAction = "运维白名单人工核对接收方后 resolve；禁止自动重投"
	if terminalErr != nil {
		return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: fmt.Errorf("原始错误=%v；unknown 落账失败=%w", cause, terminalErr)}
	}
	return outcome, &DeliveryOutcomeError{Outcome: outcome, Cause: cause}
}

func (a *App) reconcileDeliveries() error {
	views, err := a.Store.Deliveries(a.Config)
	if err != nil {
		return err
	}
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return err
	}
	byID := make(map[string]Event, len(events))
	for _, event := range events {
		byID[event.ID] = event
	}
	var failures []string
	for _, view := range views {
		// Terminal deliveries are already converged. Re-reading and strictly
		// replaying the complete ledger once per historical delivery makes
		// gateway startup quadratic and can exceed hq up's handshake deadline
		// for a healthy, long-lived company. Only states with actual recovery
		// work enter the per-delivery lock and freshness read.
		if view.Status != deliveryPrepared && view.Status != deliveryAttempted &&
			!(view.Status == deliveryQueued && view.DeliveryMode == deliveryModeQuiet) {
			continue
		}
		if reconcileErr := func() error {
			releaseOperation, lockErr := a.lockOperation(operationScopeDelivery, view.DeliveryID)
			if lockErr != nil {
				return lockErr
			}
			defer releaseOperation()

			fresh, ok, readErr := a.Store.Delivery(a.Config, view.DeliveryID)
			if readErr != nil {
				return readErr
			}
			if !ok {
				return fmt.Errorf("reconcile delivery 不存在：%s", view.DeliveryID)
			}
			origin, ok := byID[fresh.OriginEventID]
			if !ok {
				return fmt.Errorf("reconcile delivery origin 不存在：%s", fresh.OriginEventID)
			}
			switch fresh.Status {
			case deliveryPrepared:
				_, deliveryErr := a.processDeliveryLocked(origin, "")
				return deliveryErr
			case deliveryAttempted:
				_, deliveryErr := a.markDeliveryUnknown(origin, fresh.AttemptEventID, fmt.Errorf("reconcile 发现 attempted 无终态"))
				return deliveryErr
			case deliveryQueued:
				if effectiveEventDeliveryMode(origin) != deliveryModeQuiet {
					return nil
				}
				state, stateErr := a.inspectDeliveryTarget(origin.Recipient)
				if stateErr != nil {
					return stateErr
				}
				if state != deliveryRuntimeOffline {
					return a.deliverQueuedAtNaturalWakeLocked(origin)
				}
			}
			return nil
		}(); reconcileErr != nil {
			failures = append(failures, reconcileErr.Error())
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("delivery reconcile 有 %d 项需处理：%s", len(failures), strings.Join(failures, " | "))
	}
	return nil
}
