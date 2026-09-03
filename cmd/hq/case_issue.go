package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// assignmentCapacityUsed reserves WIP as soon as an issue intent is durable.
// A definitely-not-sent or unknown delivery can still be retried, so it must
// occupy the seat until it either converges to a delivered assignment or the
// assignment completes. Counting only delivered assignments would allow two
// failed deliveries from different cases to overbook the same dormant seat.
func (s *ledgerState) assignmentCapacityUsed(assignee string) int {
	used := 0
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment != nil && assignment.Recipient == assignee && !assignment.Consumed {
			used++
		}
	}
	for _, record := range s.deliveries {
		if record != nil && record.Origin.Type == "issue_prepared" &&
			record.Origin.Recipient == assignee && record.Status != deliverySent {
			used++
		}
	}
	return used
}

func (s *ledgerState) assignmentCapacityGuidance(assignee, actor string) string {
	items := make([]string, 0)
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment == nil || assignment.Recipient != assignee || assignment.Consumed {
			continue
		}
		switch assignment.Status {
		case "submitted":
			reviewEvent := s.assignmentReviewEventID(assignment)
			if assignment.Acceptor == actor {
				items = append(items, fmt.Sprintf("%s case=%s status=submitted：你是 acceptor；核验证据后运行 `hq accept --event %s --next TEXT`，不合格则运行 `hq return --event %s --reason TEXT --next TEXT`", assignment.AssignmentID, assignment.CaseID, reviewEvent, reviewEvent))
			} else {
				items = append(items, fmt.Sprintf("%s case=%s status=submitted：等待 acceptor=%s 对 report event=%s 执行 accept/return", assignment.AssignmentID, assignment.CaseID, assignment.Acceptor, reviewEvent))
			}
		case "issued":
			deliveryID := "DELIVERY_ID"
			for _, record := range s.deliveries {
				if record != nil && record.Terminal.ID == assignment.EventID {
					deliveryID = record.Origin.DeliveryID
					break
				}
			}
			items = append(items, fmt.Sprintf("%s case=%s status=issued：assignee=%s 尚未接单；运行 `hq delivery status --id %s`，不得新建重复 assignment", assignment.AssignmentID, assignment.CaseID, assignment.Recipient, deliveryID))
		case "accepted", "rework":
			items = append(items, fmt.Sprintf("%s case=%s status=%s：assignee=%s 必须在原合同下继续并运行 `hq report --case %s ...`；经理可用 `hq message --to %s --kind request --case %s --text TEXT` 催办", assignment.AssignmentID, assignment.CaseID, assignment.Status, assignment.Recipient, assignment.CaseID, assignment.Recipient, assignment.CaseID))
		default:
			items = append(items, fmt.Sprintf("%s case=%s status=%s：先运行 `hq assignment show --id %s` 与 `hq history --case %s`", assignment.AssignmentID, assignment.CaseID, assignment.Status, assignment.AssignmentID, assignment.CaseID))
		}
	}
	for _, record := range s.deliveries {
		if record == nil || record.Origin.Type != "issue_prepared" || record.Origin.Recipient != assignee || record.Status == deliverySent {
			continue
		}
		items = append(items, fmt.Sprintf("pending delivery=%s case=%s status=%s：运行 `hq delivery status --id %s`，严格按 next_action 恢复", record.Origin.DeliveryID, record.Origin.CaseID, record.Status, record.Origin.DeliveryID))
	}
	sort.Strings(items)
	return strings.Join(items, "；")
}

// issueHierarchyViolation explains how to recover without weakening the
// one-edge reporting invariant.  The approval witness is resolved from the
// registry responsibility, never from a conventional agent name.
func (a *App) issueHierarchyViolation(actor Actor, target AgentRule, caseID string) error {
	base := fmt.Sprintf("issue 只允许精确纵向一层 target.ReportsTo==actor.Name；actor=%s target=%s target_reports_to=%s", actor.Name, target.Name, target.ReportsTo)
	witness, witnessErr := a.Config.approvalWitness()
	if witnessErr == nil && actor.Name == witness.Name && target.ReportsTo != "" {
		return permissionf("%s。总裁秘书（职责位 %s，agent=%s）只能给直属部门经理派 root case，不能越级直发 specialist；请先为直属经理 %s 准备匹配该 case+target 的授权，再运行 hq issue --case %s --to %s --approval APR --next TEXT（或使用 --decision FILE），由 %s 接收后拆分子 case 并直接 issue 给 %s", base, roleApprovalWitness, witness.Name, target.ReportsTo, caseID, target.ReportsTo, target.ReportsTo, target.Name)
	}
	if actor.Rule.ReportsTo != "" && target.ReportsTo != "" {
		if a.Config.isManager(actor.Rule) {
			return permissionf("%s。经理不得向上或跨部门 issue，也不要用 message 代替 durable ownership；若你是 case %s 当前负责人，请运行 hq case escalate --id NEW-REWORK-ID --parent %s --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 --source PATH --reason TEXT --next TEXT。HQ 会把新子 case 固定上交直属上级 %s，再由上级审批并把工作路由给对应经理 %s", base, caseID, caseID, actor.Rule.ReportsTo, target.ReportsTo)
		}
		return permissionf("%s。请先 report 给直属经理 %s；由经理持有父 case 后使用 hq case escalate 建立 durable 上行交接，再由管理链路由给对应经理 %s", base, actor.Rule.ReportsTo, target.ReportsTo)
	}
	if target.ReportsTo != "" {
		return permissionf("%s。请通过对应经理 %s 安排该员工，再由 %s 对 %s 直接 issue", base, target.ReportsTo, target.ReportsTo, target.Name)
	}
	return permissionf("%s。target 没有登记直属上级，不能通过 issue 向上或跨级委派；请向你的管理链上报", base)
}

// cmdIssue exposes the only delegation protocol: a versioned case.
func (a *App) cmdIssue(args []string) error {
	fs := newLeafParser("issue")
	fs.SetOutput(a.Err)
	caseID := fs.String("case", "", "case_id")
	target := fs.String("to", "", "target agent")
	approvalID := fs.String("approval", "", "一次性 approval_id")
	decision := fs.String("decision", "", "standing authorization decision")
	next := fs.String("next", "", "下一步")
	note := fs.String("note", "", "补充说明")
	due := fs.String("due", "", "可选 RFC3339 截止时间")
	delivery := fs.String("delivery", deliveryModeWakeup, "固定为 wakeup")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if strings.TrimSpace(*delivery) != deliveryModeWakeup {
		return fmt.Errorf("case 委派是正式派活，--delivery 固定为 wakeup；拒绝降档为 %q", strings.TrimSpace(*delivery))
	}
	cleanCase := strings.TrimSpace(*caseID)
	if err := validateCaseID(cleanCase); err != nil {
		return err
	}
	targetRule, ok := a.Config.exactRule(strings.TrimSpace(*target))
	if !ok || !targetRule.CanReceiveOrder || !targetRule.CanAccept {
		return permissionf("接收方未登记、已停用或缺少 can_receive_order/can_accept")
	}
	if targetRule.ReportsTo != actor.Name {
		return a.issueHierarchyViolation(actor, targetRule, cleanCase)
	}
	roleCard, err := a.Config.roleCardForAgent(targetRule)
	if err != nil {
		return err
	}
	if _, err := verifyAgentRoleCardArtifact(a.Config, a.HQRoot, targetRule); err != nil {
		return fmt.Errorf("接收方角色卡核验失败：%w", err)
	}
	cleanNext, err := validateBusinessText("next", *next, true)
	if err != nil {
		return err
	}
	cleanNote, err := validateBusinessText("note", *note, false)
	if err != nil {
		return err
	}
	cleanDue, err := normalizeAssignmentDue(*due)
	if err != nil {
		return err
	}
	if !actor.Rule.CanAccept {
		return permissionf("assignment issuer %s 无 can_accept，不能担任冻结的 reviewer/acceptor", actor.Name)
	}
	if a.Config.isManager(actor.Rule) {
		if actor.Rule.hasResponsibility(roleApprovalWitness) {
			return permissionf("配置非法：approval_witness 不得兼任 manager；拒绝使用 manager 内建授权绕过公司所有者 approval")
		}
		if strings.TrimSpace(*approvalID) != "" || strings.TrimSpace(*decision) != "" {
			return permissionf("部门经理委派直属下属使用内建 manager 授权，不使用 approval/decision；请删除 --approval/--decision 后重试：hq issue --case %s --to %s --next TEXT", cleanCase, targetRule.Name)
		}
	} else {
		if !actor.Rule.CanIssue {
			return permissionf("当前 actor 无 can_issue，且不是接收方的直属经理")
		}
		if (strings.TrimSpace(*approvalID) == "") == (strings.TrimSpace(*decision) == "") {
			return fmt.Errorf("非经理委派 case 必须二选一引用 --approval 或 --decision")
		}
	}
	preflightState, err := a.currentCase(cleanCase)
	if err != nil {
		return err
	}
	if preflightState.Status == string(statusEscalated) {
		return conflictf("case %s 是刚上交的 escalation，必须由当前直属上级先核验：hq accept --event %s --next TEXT；accept 后再针对当前 generation 申请 owner approval 并 issue", cleanCase, preflightState.LastEventID)
	}
	caseGeneration := strconv.Itoa(preflightState.Version) + ":" + preflightState.Digest
	commandID := stableCommandID("issue-case", actor.Name, cleanCase, caseGeneration, targetRule.Name,
		roleCardKey(roleCard.ID, roleCard.Version), targetRule.SeatDigest,
		strings.TrimSpace(*approvalID), strings.TrimSpace(*decision), cleanNext, cleanDue)
	digest := requestDigest("issue-case", actor.Name, cleanCase, caseGeneration, targetRule.Name,
		roleCard.Digest, targetRule.SeatDigest,
		strings.TrimSpace(*approvalID), strings.TrimSpace(*decision), cleanNext, cleanNote, cleanDue)
	releaseOriginFence := func() {}
	if !a.DryRun {
		releaseOriginFence, err = a.lockRuntimeSeatOriginFence(targetRule.Name)
		if err != nil {
			return err
		}
	}
	originFenceHeld := true
	defer func() {
		if originFenceHeld {
			releaseOriginFence()
		}
	}()
	result, err := a.transactBatch(commandID, digest, func(ledger *ledgerState) ([]Event, error) {
		if _, err := verifyAgentRoleCardArtifact(a.Config, a.HQRoot, targetRule); err != nil {
			return nil, fmt.Errorf("接收方角色卡在 issue admission 期间漂移：%w", err)
		}
		state, err := ledger.currentCase(cleanCase)
		if err != nil {
			return nil, err
		}
		if err := ledger.rejectActiveAssignment(cleanCase, "创建第二份 issue/assignment"); err != nil {
			return nil, err
		}
		if used := ledger.assignmentCapacityUsed(targetRule.Name); used >= targetRule.MaxWIP {
			return nil, conflictf("员工 %s 当前 assignment capacity=%d，已达到 max_wip=%d；待送达 issue 也会保留容量。先运行 `hq assignment list --assignee %s`；阻塞项：%s。完成上述 accept/return/report/delivery 收敛后，只重试当前 `hq issue`；不要创建重复 case/assignment，也不要改用裸 herdr prompt", targetRule.Name, used, targetRule.MaxWIP, targetRule.Name, ledger.assignmentCapacityGuidance(targetRule.Name, actor.Name))
		}
		if err := validateStateTransition(actionIssue, state.Status, string(statusDispatched)); err != nil {
			return nil, err
		}
		if state.Owner != actor.Name {
			return nil, permissionf("只有 case 当前负责人可以委派；case=%s 当前负责人=%s。经理如需分工，应先创建多个子 case", cleanCase, state.Owner)
		}
		if state.Version != preflightState.Version || state.Digest != preflightState.Digest {
			return nil, fmt.Errorf("case 在 issue admission 期间已变化；请读取最新版本后重试")
		}
		if state.Version == 0 || state.Digest == "" {
			return nil, fmt.Errorf("case 缺少合法规格版本或 digest，拒绝委派")
		}
		if cleanDue != "" && !mustParseTime(cleanDue).After(a.Store.NowTime()) {
			return nil, fmt.Errorf("due 必须晚于当前时间")
		}
		prepared, err := a.newEvent(actor, "issue_prepared", cleanCase)
		if err != nil {
			return nil, err
		}
		prepared.FromState, prepared.Project = state.Status, state.Project
		prepared.Recipient, prepared.RecipientLabel = targetRule.Name, targetRule.Label
		prepared.CaseVersion, prepared.CaseDigest = state.Version, state.Digest
		prepared.AssignmentID = stableCommandID("assignment", commandID)
		prepared.AssignmentIssuer = actor.Name
		prepared.AssigneeSeatVersion, prepared.AssigneeSeatDigest = targetRule.SeatVersion, targetRule.SeatDigest
		prepared.RoleCardID, prepared.RoleCardVersion = roleCard.ID, roleCard.Version
		prepared.RoleCardDigest, prepared.RoleCardManualPath = roleCard.Digest, roleCard.ManualPath
		prepared.Reviewer, prepared.ReviewerLabel = actor.Name, actor.Label
		prepared.Acceptor, prepared.AcceptorLabel = actor.Name, actor.Label
		prepared.DueAt = cleanDue
		prepared.AssignmentDigest = assignmentContractDigest(prepared)
		prepared.NextAction, prepared.Note = cleanNext, cleanNote
		prepared.DeliveryMode, prepared.DeliveryTarget, prepared.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "issue-fixed"
		batch := []Event{}
		if a.Config.isManager(actor.Rule) {
			prepared.AuthorizationType = "manager"
			prepared.AuthorizationDigest = requestDigest("manager", actor.Name, targetRule.Name)
		} else if strings.TrimSpace(*approvalID) != "" {
			record := ledger.approvals[strings.TrimSpace(*approvalID)]
			if record == nil || record.Grant.ID == "" {
				return nil, fmt.Errorf("approval 不存在或未 granted")
			}
			if record.Request.CaseID != cleanCase || record.Request.Recipient != targetRule.Name || record.Request.CaseVersion != state.Version || record.Request.CaseDigest != state.Digest {
				return nil, fmt.Errorf("approval scope 不匹配 case/target/case version+digest")
			}
			if record.Status != "granted" {
				return nil, fmt.Errorf("approval 当前状态=%s，不可委派", record.Status)
			}
			if !a.Store.NowTime().Before(mustParseTime(record.Request.ExpiresAt)) {
				return nil, fmt.Errorf("approval 已过期")
			}
			witness, witnessErr := a.Config.approvalWitness()
			if witnessErr != nil {
				return nil, witnessErr
			}
			if actor.Name != witness.Name || record.Grant.CapturedBy != witness.Name || record.Grant.Issuer != a.Config.ownerPrincipal() {
				return nil, fmt.Errorf("approval 的公司所有者或 %s 与当前配置不匹配", roleApprovalWitness)
			}
			if err := validateApprovalSnapshotFreshness(ledger, a.Config, record.Request, witness); err != nil {
				return nil, err
			}
			if targetRule.SeatVersion != record.Request.AssigneeSeatVersion || targetRule.SeatDigest != record.Request.AssigneeSeatDigest {
				return nil, staleApprovalError(record.Request, witness, fmt.Sprintf("当前 target seat=%d/%s 未匹配 approval 冻结 seat=%d/%s", targetRule.SeatVersion, targetRule.SeatDigest, record.Request.AssigneeSeatVersion, record.Request.AssigneeSeatDigest))
			}
			prepared.AuthorizationType, prepared.ApprovalID = "approval", record.Request.ApprovalID
			prepared.Issuer, prepared.CapturedBy = record.Grant.Issuer, actor.Name
			prepared.AuthorizationDigest = approvalIssueAuthorizationDigest(record.Request)
			if record.Request.ApprovalMode != "one_time" {
				return nil, fmt.Errorf("approval mode 必须是 one_time")
			}
			consume, err := a.newEvent(actor, "approval_consumed", cleanCase)
			if err != nil {
				return nil, err
			}
			copyApprovalScope(&consume, record.Request)
			consume.RelatedEventID, consume.ApprovalStatus = prepared.ID, "consumed"
			batch = append(batch, consume)
		} else {
			cleanDecision, authorizationDigest, err := readStandingAuthorization(strings.TrimSpace(*decision), a.Office, a.Config.ownerPrincipal(), standingScope{Action: "issue", Actor: actor.Name, Target: targetRule.Name, CasePrefix: cleanCase})
			if err != nil {
				return nil, fmt.Errorf("decision：%w", err)
			}
			prepared.AuthorizationType, prepared.DecisionRef, prepared.AuthorizationDigest = "standing_decision", cleanDecision, authorizationDigest
		}
		prepared.Delivery, prepared.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, targetRule.Name)
		payload, err := a.deliveryPayload(prepared)
		if err != nil {
			return nil, err
		}
		prepared.PayloadDigest = digestText(payload)
		batch = append(batch, prepared)
		return batch, nil
	})
	if err != nil {
		return err
	}
	releaseOriginFence()
	originFenceHeld = false
	prepared := result.Events[len(result.Events)-1]
	if a.DryRun {
		return a.output(prepared, fmt.Sprintf("DRY-RUN：case %s@v%d → %s；delivery=%s", prepared.CaseID, prepared.CaseVersion, prepared.Recipient, prepared.DeliveryID))
	}
	outcome, deliveryErr := a.processDelivery(prepared, "")
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(outcome, "")
		}
		return deliveryErr
	}
	return a.output(outcome, fmt.Sprintf("case 已委派给 %s；case=%s@v%d event=%s delivery=%s", targetRule.Name, cleanCase, prepared.CaseVersion, prepared.ID, prepared.DeliveryID))
}
