package main

import (
	"fmt"
	"strings"
)

func (a *App) cmdReconcile(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("用法：hq reconcile")
	}
	if err := a.reconcileDeliveries(); err != nil {
		return err
	}
	return a.output(map[string]string{"delivery_reconcile": "complete"}, "delivery reconcile 完成；未自动重投 attempted/unknown")
}

func (a *App) cmdDelivery(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq delivery status|retry|resolve|budget|consume")
	}
	switch args[0] {
	case "status":
		return a.cmdDeliveryStatus(args[1:])
	case "retry":
		return a.cmdDeliveryRetry(args[1:])
	case "resolve":
		return a.cmdDeliveryResolve(args[1:])
	case "budget":
		return a.cmdDeliveryBudget(args[1:])
	case "consume":
		return a.cmdDeliveryConsume(args[1:])
	default:
		return fmt.Errorf("未知 delivery 子命令 %q", args[0])
	}
}

func (a *App) cmdDeliveryStatus(args []string) error {
	fs := newLeafParser("delivery status")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "delivery_id；留空列出全部")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*id) == "" {
		views, err := a.Store.Deliveries(a.Config)
		if err != nil {
			return err
		}
		return a.output(views, fmt.Sprintf("%d 个 delivery", len(views)))
	}
	view, ok, err := a.Store.Delivery(a.Config, strings.TrimSpace(*id))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("delivery 不存在：%s", *id)
	}
	return a.output(view, fmt.Sprintf("delivery=%s status=%s：%s；下一步：%s（internal=%s attempts=%d）", view.DeliveryID, view.ProjectionStatus, view.StatusDescription, view.NextAction, view.Status, view.AttemptCount))
}

func (a *App) cmdDeliveryRetry(args []string) error {
	fs := newLeafParser("delivery retry")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "delivery_id")
	command := fs.String("command", "", "稳定 retry command_id（可选）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	deliveryID := strings.TrimSpace(*id)
	if err := validateLedgerID("delivery_id", deliveryID); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	view, ok, err := a.Store.Delivery(a.Config, deliveryID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("delivery 不存在：%s", deliveryID)
	}
	if view.Status != deliveryFailedPreSend {
		return fmt.Errorf("delivery %s 当前为 %s，仅 failed_pre_send 可显式 retry", deliveryID, view.Status)
	}
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return err
	}
	origin, ok := findEvent(events, view.OriginEventID)
	if !ok {
		return fmt.Errorf("delivery origin 不存在：%s", view.OriginEventID)
	}
	if actor.Name != origin.Actor && !actor.Rule.CanManageStaff {
		return permissionf("仅 origin actor 或运维白名单可 retry delivery")
	}
	retryCommand := strings.TrimSpace(*command)
	if retryCommand == "" {
		retryCommand = fmt.Sprintf("delivery-retry:%s:%d", deliveryID, view.AttemptCount+1)
	}
	if err := validateLedgerID("retry command_id", retryCommand); err != nil {
		return err
	}
	outcome, deliveryErr := a.processDelivery(origin, retryCommand)
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(outcome, "")
		}
		return deliveryErr
	}
	return a.output(outcome, fmt.Sprintf("delivery=%s retry 完成，状态=%s", deliveryID, outcome.DeliveryStatus))
}

func (a *App) cmdDeliveryResolve(args []string) error {
	fs := newLeafParser("delivery resolve")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "delivery_id")
	outcome := fs.String("outcome", "", "delivered|not-delivered")
	reason := fs.String("reason", "", "人工核对理由")
	evidence := fs.String("evidence", "", "核对依据路径[#定位]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if !actor.Rule.CanManageStaff {
		return permissionf("仅运维白名单可解除 unknown delivery")
	}
	deliveryID := strings.TrimSpace(*id)
	if err := validateLedgerID("delivery_id", deliveryID); err != nil {
		return err
	}
	cleanReason, err := validateShortText("reason", *reason, true)
	if err != nil {
		return err
	}
	cleanEvidence, err := normalizeRef(*evidence, a.HQRoot, true)
	if err != nil {
		return err
	}
	if *outcome != "delivered" && *outcome != "not-delivered" {
		return fmt.Errorf("outcome 只能是 delivered|not-delivered")
	}
	releaseOperation, err := a.lockOperation(operationScopeDelivery, deliveryID)
	if err != nil {
		return err
	}
	defer releaseOperation()
	view, ok, err := a.Store.Delivery(a.Config, deliveryID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("delivery 不存在：%s", deliveryID)
	}
	if view.AttemptEventID == "" {
		return fmt.Errorf("delivery %s 没有可核对的 attempted 事件", deliveryID)
	}
	if *outcome == "delivered" && view.OriginType == "issue_prepared" {
		ledger, err := a.ledgerState()
		if err != nil {
			return err
		}
		record := ledger.deliveries[deliveryID]
		if record == nil || record.Origin.ID != view.OriginEventID {
			return conflictf("delivery %s 在 resolve 手册预检期间已变化", deliveryID)
		}
		if err := a.verifyCurrentFrozenSeat(frozenSeatFromEvent(record.Origin)); err != nil {
			return fmt.Errorf("resolve delivered 前冻结席位/手册核验失败：%w", err)
		}
	}
	attemptEventID := view.AttemptEventID
	commandID := stableCommandID("delivery-resolve", deliveryID, attemptEventID)
	digest := requestDigest("delivery-resolve", actor.Name, deliveryID, attemptEventID, *outcome, cleanReason, cleanEvidence)
	result, err := a.transactBatch(commandID, digest, func(ledger *ledgerState) ([]Event, error) {
		record := ledger.deliveries[deliveryID]
		if record == nil {
			return nil, fmt.Errorf("delivery 不存在：%s", deliveryID)
		}
		if record.Status != deliveryUnknown {
			return nil, fmt.Errorf("delivery %s 当前为 %s，仅 unknown 可 resolve", deliveryID, record.Status)
		}
		if record.Attempt.ID != attemptEventID {
			return nil, conflictf("delivery %s 在 resolve admission 期间 attempt 已变化：读取=%s 当前=%s；请重新核对最新 attempt",
				deliveryID, attemptEventID, record.Attempt.ID)
		}
		var bundled []Event
		eventType, state := "delivery_resolved_not_sent", deliveryFailedPreSend
		if *outcome == "delivered" {
			if record.Origin.Type == "issue_prepared" {
				if _, err := currentSeatForFrozenContract(a.Config, frozenSeatFromEvent(record.Origin)); err != nil {
					return nil, fmt.Errorf("resolve delivered 前冻结席位核验失败：%w", err)
				}
			}
			eventType, state = "delivery_resolved_sent", deliverySent
			var err error
			bundled, err = a.buildTurnBundleConvergenceEvents(ledger, record)
			if err != nil {
				return nil, err
			}
		}
		event, err := a.newEvent(actor, eventType, record.Origin.CaseID)
		if err != nil {
			return nil, err
		}
		event.RelatedEventID = record.Origin.ID
		event.AttemptEventID = record.Attempt.ID
		copyDeliveryEnvelope(&event, record.Origin)
		event.Delivery = state
		event.ResolutionRef = cleanEvidence
		if state == deliverySent {
			if !isBusinessDeliveryOrigin(record.Origin.Type) || ledger.businessDeliveryOriginMatchesCurrent(record) {
				a.fillSentState(&event, record.Origin, ledger)
			}
		}
		// fillSentState restores business fields from the origin and may populate
		// its historical note. The resolution reason is the terminal event's
		// required operational evidence and therefore wins afterward.
		event.Note = cleanReason
		return append(bundled, event), nil
	})
	if err != nil {
		return err
	}
	resolved := result.Events[len(result.Events)-1]
	return a.output(resolved, fmt.Sprintf("delivery=%s 已人工 resolve 为 %s；event=%s", deliveryID, resolved.Delivery, resolved.ID))
}
