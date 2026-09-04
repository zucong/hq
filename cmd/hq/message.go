package main

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const maxMessageTextBytes = 2 * 1024

func stableMessageID(commandID string) string {
	digest := strings.ToUpper(digestText(commandID))
	return "MSG-" + digest[:24]
}

func validateMessageText(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("缺少 --text")
	}
	if !utf8.ValidString(value) {
		return "", fmt.Errorf("--text 必须是合法 UTF-8")
	}
	if strings.ContainsAny(value, "\r\x00") {
		return "", fmt.Errorf("--text 不得包含 CR 或 NUL")
	}
	if len([]byte(value)) > maxMessageTextBytes {
		return "", fmt.Errorf("--text 为 %d bytes，超过 2 KiB 硬上限；请把长内容写入文件并使用 --ref-file", len([]byte(value)))
	}
	if containsSensitive(value) {
		return "", fmt.Errorf("--text 疑似包含密钥或金额，拒绝写入事件账本")
	}
	return value, nil
}

func cleanStringList(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

func messageByID(ledger *ledgerState, id string) (Event, bool) {
	for _, event := range ledger.events {
		if event.Type == "message_prepared" && event.MessageID == id {
			return event, true
		}
	}
	return Event{}, false
}

func (a *App) cmdMessage(args []string) error {
	if len(args) > 0 && args[0] == "ack" {
		return a.cmdMessageAck(args[1:])
	}
	fs := newLeafParser("message")
	fs.SetOutput(a.Err)
	target := fs.String("to", "", "recipient")
	kind := fs.String("kind", "", "info|question|request|handoff|directive")
	urgency := fs.String("urgency", messageUrgencyNormal, "normal|urgent")
	caseID := fs.String("case", "", "optional case_id")
	body := fs.String("text", "", "消息正文")
	refFiles := fs.StringSlice("ref-file", nil, "引用文件")
	refCases := fs.StringSlice("ref-case", nil, "引用 case")
	refMessages := fs.StringSlice("ref-message", nil, "引用 message")
	refEvents := fs.StringSlice("ref-event", nil, "引用 event")
	thread := fs.String("thread", "", "thread id")
	replyTo := fs.String("reply-to", "", "被回复 message_id")
	policy := a.Config.effectiveDeliveryPolicy()
	delivery := fs.String("delivery", policy.DefaultMode, "auto|wakeup|quiet|inject")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	targetRule, ok := a.Config.exactRule(strings.TrimSpace(*target))
	if !ok {
		return fmt.Errorf("message recipient 未登记或已停用")
	}
	if targetRule.Name == actor.Name {
		return fmt.Errorf("message recipient 不能是自己")
	}
	cleanKind := strings.TrimSpace(*kind)
	if !validMessageKind(cleanKind) {
		return fmt.Errorf("--kind 只能是 info|question|request|handoff|directive")
	}
	requestedDelivery := strings.TrimSpace(*delivery)
	if !validDeliveryRequestMode(requestedDelivery) {
		return fmt.Errorf("--delivery 只能是 auto|wakeup|quiet|inject")
	}
	cleanUrgency := strings.TrimSpace(*urgency)
	if !validMessageUrgency(cleanUrgency) {
		return fmt.Errorf("--urgency 只能是 normal|urgent")
	}
	if cleanUrgency == messageUrgencyUrgent && cleanKind != "directive" {
		return fmt.Errorf("--urgency urgent 只允许与 --kind directive 一起使用，以绑定 active assignment 和冻结 authority；普通消息请删除 --urgency urgent")
	}
	if cleanUrgency == messageUrgencyUrgent {
		if fs.Changed("delivery") && requestedDelivery != deliveryModeAuto && requestedDelivery != deliveryModeWakeup {
			return fmt.Errorf("urgent 消息必须在下一安全回合主动唤醒；请删除 --delivery，或改为 --delivery wakeup")
		}
		requestedDelivery = deliveryModeWakeup
	}
	runtimeState := deliveryRuntimeIdle
	if requestedDelivery == deliveryModeAuto {
		runtimeState, err = a.inspectDeliveryTarget(targetRule.Name)
		if err != nil {
			return err
		}
	}
	cleanCase := strings.TrimSpace(*caseID)
	if cleanCase != "" {
		if err := validateCaseID(cleanCase); err != nil {
			return err
		}
	}
	cleanBody, err := validateMessageText(*body)
	if err != nil {
		return err
	}
	files := cleanStringList(*refFiles)
	for i, value := range files {
		files[i], err = normalizeRef(value, a.HQRoot, true)
		if err != nil {
			return fmt.Errorf("ref-file：%w", err)
		}
	}
	cases, messages, events := cleanStringList(*refCases), cleanStringList(*refMessages), cleanStringList(*refEvents)
	for _, value := range cases {
		if err := validateCaseID(value); err != nil {
			return fmt.Errorf("ref-case：%w", err)
		}
	}
	for _, value := range messages {
		if err := validateLedgerID("ref-message", value); err != nil {
			return err
		}
	}
	for _, value := range events {
		if err := validateLedgerID("ref-event", value); err != nil {
			return err
		}
	}
	cleanReply, cleanThread := strings.TrimSpace(*replyTo), strings.TrimSpace(*thread)
	if cleanReply != "" {
		if err := validateLedgerID("reply-to", cleanReply); err != nil {
			return err
		}
	}
	commandID := stableCommandID("message", actor.Name, targetRule.Name, cleanCase, cleanKind, cleanUrgency, cleanThread, cleanReply, cleanBody,
		strings.Join(files, "\x1f"), strings.Join(cases, "\x1f"), strings.Join(messages, "\x1f"), strings.Join(events, "\x1f"), requestedDelivery)
	messageID := stableMessageID(commandID)
	digest := requestDigest("message", commandID, messageID)
	releaseOriginFence := func() {}
	if !a.DryRun && (requestedDelivery == deliveryModeWakeup || requestedDelivery == deliveryModeAuto) {
		releaseOriginFence, err = a.lockRuntimeSeatOriginFence(targetRule.Name)
		if err != nil {
			return err
		}
		if requestedDelivery == deliveryModeAuto {
			runtimeState, err = a.inspectDeliveryTarget(targetRule.Name)
			if err != nil {
				releaseOriginFence()
				return err
			}
		}
	}
	originFenceHeld := true
	defer func() {
		if originFenceHeld {
			releaseOriginFence()
		}
	}()
	result, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		if cleanCase != "" {
			if _, err := ledger.currentCase(cleanCase); err != nil {
				return Event{}, err
			}
		}
		for _, value := range cases {
			if _, err := ledger.currentCase(value); err != nil {
				return Event{}, fmt.Errorf("ref-case：%w", err)
			}
		}
		for _, value := range messages {
			if _, ok := messageByID(ledger, value); !ok {
				return Event{}, fmt.Errorf("ref-message 不存在：%s", value)
			}
		}
		for _, value := range events {
			if _, ok := ledger.events[value]; !ok {
				return Event{}, fmt.Errorf("ref-event 不存在：%s", value)
			}
		}
		if cleanReply != "" {
			original, ok := messageByID(ledger, cleanReply)
			if !ok {
				return Event{}, fmt.Errorf("reply-to 不是已知 message_id")
			}
			if cleanThread == "" {
				cleanThread = original.ThreadID
			}
			if original.ThreadID != cleanThread {
				return Event{}, fmt.Errorf("reply-to 与 thread 不匹配")
			}
		}
		if cleanThread == "" {
			cleanThread = stableMessageID(stableCommandID("thread", actor.Name, targetRule.Name, cleanCase))
		}
		if err := validateLedgerID("thread", cleanThread); err != nil {
			return Event{}, err
		}
		event, err := a.newEvent(actor, "message_prepared", cleanCase)
		if err != nil {
			return Event{}, err
		}
		event.Recipient, event.RecipientLabel = targetRule.Name, targetRule.Label
		event.MessageID, event.MessageKind, event.Urgency, event.Message = messageID, cleanKind, cleanUrgency, cleanBody
		event.RefFiles, event.RefCases, event.RefMessages, event.RefEvents = files, cases, messages, events
		event.ThreadID, event.ReplyTo = cleanThread, cleanReply
		if cleanKind == "directive" {
			if cleanCase == "" {
				return Event{}, fmt.Errorf("directive 必须使用 --case 绑定当前 active assignment；普通补充说明请使用 --kind request")
			}
			matches := make([]*caseAssignment, 0, 1)
			for _, assignment := range ledger.activeAssignments(cleanCase) {
				if assignment.Recipient == targetRule.Name &&
					(actor.Name == assignment.Issuer || actor.Name == assignment.Reviewer || actor.Name == assignment.Acceptor) {
					matches = append(matches, assignment)
				}
			}
			if len(matches) != 1 {
				return Event{}, conflictf("directive 必须唯一绑定 recipient=%s 在 case=%s 上的 active assignment，当前匹配=%d；运行 `hq assignment list --case %s` 核验。若目标、验收或约束发生变化，不要用 message，改由冻结 issuer 运行 `hq case revise --id %s --version N --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P0 --source PATH --supersede-active --next TEXT`", targetRule.Name, cleanCase, len(matches), cleanCase, cleanCase)
			}
			assignment := matches[0]
			event.CaseVersion, event.CaseDigest = assignment.CaseVersion, assignment.CaseDigest
			copyAssignmentStateBinding(&event, assignment)
		}
		mode, reason, err := selectMessageDelivery(requestedDelivery, cleanKind, cleanUrgency, runtimeState, ledger.deliveryBudgetSpent(targetRule.Name), policy)
		if err != nil {
			return Event{}, err
		}
		if cleanUrgency != messageUrgencyUrgent && !targetRule.CanManageStaff && targetRule.ApprovalRef != "" && !ledger.hasEverReceivedCase(targetRule.Name) {
			mode, reason = deliveryModeInject, "recipient-awaiting-first-manager-case"
		}
		targetPrimitive, _, _ := deliveryModePrimitives(mode)
		event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = mode, targetPrimitive, reason
		event.Delivery, event.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, targetRule.Name)
		payload, err := a.deliveryPayload(event)
		if err != nil {
			return Event{}, err
		}
		event.PayloadDigest = digestText(payload)
		return event, nil
	})
	if err != nil {
		return err
	}
	releaseOriginFence()
	originFenceHeld = false
	prepared := result.Event
	if a.DryRun {
		return a.output(prepared, fmt.Sprintf("DRY-RUN：message=%s kind=%s urgency=%s → %s delivery=%s", prepared.MessageID, prepared.MessageKind, effectiveMessageUrgency(prepared.Urgency), prepared.Recipient, prepared.DeliveryID))
	}
	outcome, deliveryErr := a.processDelivery(prepared, "")
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(outcome, "")
		}
		return deliveryErr
	}
	if outcome.DeliveryStatus == deliveryQueued {
		detail := "等待接收方自然唤醒"
		if prepared.DeliveryReason == "recipient-awaiting-first-manager-case" {
			detail = "接收方尚未从直属经理收到首个 durable case；消息不会唤醒或要求其处理，待首个 case 建立上下文后再消费"
		}
		return a.output(outcome, fmt.Sprintf("message=%s 已记账并排队；recipient=%s mode=%s wakeup=false reason=%s；%s；delivery=%s", prepared.MessageID, targetRule.Name, outcome.DeliveryMode, prepared.DeliveryReason, detail, prepared.DeliveryID))
	}
	return a.output(outcome, fmt.Sprintf("message=%s 已送达；recipient=%s kind=%s urgency=%s mode=%s wakeup=%t delivery=%s", prepared.MessageID, targetRule.Name, cleanKind, cleanUrgency, outcome.DeliveryMode, outcome.Wakeup, prepared.DeliveryID))
}

func (a *App) cmdMessageAck(args []string) error {
	fs := newLeafParser("message ack")
	fs.SetOutput(a.Err)
	messageID := fs.String("message", "", "message_id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	cleanID := strings.TrimSpace(*messageID)
	if err := validateLedgerID("message", cleanID); err != nil {
		return err
	}
	commandID, digest := stableCommandID("message-ack", actor.Name, cleanID), requestDigest("message-ack", actor.Name, cleanID)
	result, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		origin, ok := messageByID(ledger, cleanID)
		if !ok {
			return Event{}, fmt.Errorf("message_id 不存在：%s", cleanID)
		}
		record := ledger.deliveries[origin.DeliveryID]
		if record == nil || record.Status != deliverySent || record.Terminal.ID == "" {
			return Event{}, fmt.Errorf("message=%s 尚未送达，不能 ack", cleanID)
		}
		semantic, ok := semanticDeliveredEvent(record.Terminal, ledger.events)
		if !ok || semantic.Type != "message_sent" {
			return Event{}, fmt.Errorf("message=%s 没有可确认的送达事件", cleanID)
		}
		if semantic.Recipient != actor.Name {
			return Event{}, permissionf("只有 message recipient 可 ack")
		}
		event, err := a.newEvent(actor, "message_acked", semantic.CaseID)
		if err != nil {
			return Event{}, err
		}
		event.MessageID, event.RelatedEventID = cleanID, record.Terminal.ID
		event.DeliveryID, event.PayloadDigest = semantic.DeliveryID, semantic.PayloadDigest
		event.MessageKind, event.ThreadID = semantic.MessageKind, semantic.ThreadID
		return event, nil
	})
	if err != nil {
		return err
	}
	return a.output(result.Event, fmt.Sprintf("message=%s acked；event=%s delivery=%s", result.Event.MessageID, result.Event.ID, result.Event.DeliveryID))
}
