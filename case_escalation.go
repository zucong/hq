package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

const escalationDeliveryReason = "manager-escalation"

type caseEscalationRequest struct {
	Spec       CaseSpec
	Reason     string
	NextAction string
}

func (a *App) parseCaseEscalation(args []string) (caseEscalationRequest, error) {
	fs := newLeafParser("case escalate")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "新 escalation 子 case 的稳定 case_id")
	parent := fs.String("parent", "", "当前经理持有的父 case_id")
	title := fs.String("title", "", "标题")
	project := fs.String("project", "", "禁止显式提供；escalation child 自动继承父 case")
	objective := fs.String("objective", "", "返工目标")
	acceptance := fs.String("acceptance", "", "返工验收条件")
	constraints := fs.String("constraints", "", "返工约束")
	priority := fs.String("priority", "", "P0|P1|P2")
	specRef := fs.String("spec-ref", "", "复杂规格 Markdown 引用")
	source := fs.String("source", "", "升级依据路径[#定位]")
	reason := fs.String("reason", "", "必须升级的新增事实")
	next := fs.String("next", "", "直属上级接手后的下一步")
	if err := fs.Parse(args); err != nil {
		return caseEscalationRequest{}, err
	}
	if fs.Changed("project") {
		return caseEscalationRequest{}, fmt.Errorf("escalation child 的 project 由唯一 root/parent 自动继承；删除 --project 后重试")
	}

	cleanID := strings.TrimSpace(*id)
	if err := validateCaseID(cleanID); err != nil {
		return caseEscalationRequest{}, err
	}
	cleanParent := strings.TrimSpace(*parent)
	if err := validateCaseID(cleanParent); err != nil {
		return caseEscalationRequest{}, fmt.Errorf("parent：%w", err)
	}
	if cleanParent == cleanID {
		return caseEscalationRequest{}, fmt.Errorf("escalation 子 case 不能把自己作为 parent")
	}
	cleanTitle, err := validateShortText("title", *title, true)
	if err != nil {
		return caseEscalationRequest{}, err
	}
	cleanProject, err := validateShortText("project", *project, false)
	if err != nil {
		return caseEscalationRequest{}, err
	}
	cleanObjective, err := validateCaseBody("objective", *objective, true)
	if err != nil {
		return caseEscalationRequest{}, err
	}
	cleanAcceptance, err := validateCaseBody("acceptance", *acceptance, true)
	if err != nil {
		return caseEscalationRequest{}, err
	}
	cleanConstraints, err := validateCaseBody("constraints", *constraints, true)
	if err != nil {
		return caseEscalationRequest{}, err
	}
	cleanPriority := strings.ToUpper(strings.TrimSpace(*priority))
	if cleanPriority != "P0" && cleanPriority != "P1" && cleanPriority != "P2" {
		return caseEscalationRequest{}, fmt.Errorf("--priority 只能是 P0/P1/P2")
	}
	cleanSpec, err := normalizeRef(*specRef, a.HQRoot, false)
	if err != nil {
		return caseEscalationRequest{}, fmt.Errorf("spec-ref：%w", err)
	}
	if cleanSpec != "" && strings.ToLower(filepath.Ext(strings.Split(cleanSpec, "#")[0])) != ".md" {
		return caseEscalationRequest{}, fmt.Errorf("spec-ref 必须引用 Markdown")
	}
	cleanSource, err := normalizeRef(*source, a.HQRoot, true)
	if err != nil {
		return caseEscalationRequest{}, fmt.Errorf("source：%w", err)
	}
	cleanReason, err := validateBusinessText("reason", *reason, true)
	if err != nil {
		return caseEscalationRequest{}, err
	}
	cleanNext, err := validateBusinessText("next", *next, true)
	if err != nil {
		return caseEscalationRequest{}, err
	}
	spec := CaseSpec{
		CaseID: cleanID, ParentCaseID: cleanParent, Title: cleanTitle, Project: cleanProject,
		Objective: cleanObjective, Acceptance: cleanAcceptance, Constraints: cleanConstraints,
		Priority: cleanPriority, SpecRef: cleanSpec, SourceRef: cleanSource, Version: 1,
	}
	return caseEscalationRequest{Spec: spec, Reason: cleanReason, NextAction: cleanNext}, nil
}

// cmdCaseEscalate creates a new child rather than rewinding the parent or an
// accepted historical submission. The child creation and durable upward
// delivery intent are one atomic ledger transaction. The only possible target
// is the manager's current functional reports_to.
func (a *App) cmdCaseEscalate(args []string) error {
	request, err := a.parseCaseEscalation(args)
	if err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if !a.Config.isManager(actor.Rule) || !actor.Rule.CanCreate {
		return permissionf("只有具备 manager:<department> 职责位且 can_create 的部门经理可创建上行 escalation；普通员工请先 report 给直属经理")
	}
	if actor.Rule.ReportsTo == "" {
		return permissionf("经理 %s 没有登记直属上级，无法建立可审计的上行 escalation；请先修复 registry reports_to", actor.Name)
	}
	superior, ok := a.Config.exactRule(actor.Rule.ReportsTo)
	if !ok || !superior.CanAccept || (!a.Config.isManager(superior) && !superior.CanIssue) {
		return permissionf("直属上级 %s 未登记、已停用，或缺少接收并继续路由 durable case 的 can_accept/can_issue（或 manager）能力；请先修复 registry", actor.Rule.ReportsTo)
	}

	preflight, err := a.currentCase(request.Spec.ParentCaseID)
	if err != nil {
		return fmt.Errorf("父 case：%w", err)
	}
	if preflight.Status == string(statusClosed) {
		return conflictf("父 case %s 已关闭，不能追加 escalation；本 HQ space 的项目树已归档；%s", preflight.ID, newHQSpaceGuidance())
	}
	if preflight.Status == string(statusEscalated) {
		return conflictf("父 case %s 正在等待上级核验 escalation；先 accept/return 当前 escalation，不得继续新增 child", preflight.ID)
	}
	if preflight.Owner != actor.Name {
		return permissionf("只有父 case 当前负责人可升级；parent=%s 当前负责人=%s。不要退回已 accepted 的旧 report；请由当前负责人运行 hq case escalate", preflight.ID, preflight.Owner)
	}
	request.Spec.Project = preflight.Project
	request.Spec.RootCaseID = preflight.RootCaseID
	if request.Spec.RootCaseID == "" {
		request.Spec.RootCaseID = preflight.ID
	}
	request.Spec.Digest = caseSpecDigest(request.Spec)

	commandID := stableCommandID("case-escalate", actor.Name, request.Spec.ParentCaseID, request.Spec.CaseID, request.Spec.Digest, superior.Name, request.NextAction)
	digest := requestDigest("case-escalate", actor.Name, request.Spec.ParentCaseID, request.Spec.CaseID,
		request.Spec.Digest, superior.Name, request.Reason, request.NextAction)
	result, err := a.transactBatch(commandID, digest, func(ledger *ledgerState) ([]Event, error) {
		if _, exists := ledger.snapshot.Cases[request.Spec.CaseID]; exists {
			return nil, fmt.Errorf("case 已存在：%s", request.Spec.CaseID)
		}
		parent, err := ledger.currentCase(request.Spec.ParentCaseID)
		if err != nil {
			return nil, fmt.Errorf("父 case：%w", err)
		}
		if parent.Status == string(statusClosed) {
			return nil, conflictf("父 case %s 已关闭，不能追加 escalation；本 HQ space 的项目树已归档；%s", parent.ID, newHQSpaceGuidance())
		}
		if parent.Status == string(statusEscalated) {
			return nil, conflictf("父 case %s 正在等待上级核验 escalation，不能追加 child", parent.ID)
		}
		if parent.Owner != actor.Name {
			return nil, permissionf("case 在 escalation admission 期间换手；parent=%s 当前负责人=%s，请重新读取后由当前负责人升级", parent.ID, parent.Owner)
		}
		if request.Spec.Project != parent.Project {
			return nil, conflictf("escalation child project 与 parent %s 不一致", parent.ID)
		}
		if err := ledger.rejectActiveAssignment(parent.ID, "发起上行 escalation"); err != nil {
			return nil, err
		}
		if parent.Version != preflight.Version || parent.Digest != preflight.Digest || parent.LastEventID != preflight.LastEventID {
			return nil, conflictf("父 case 在 escalation admission 期间已变化；请重新读取 hq case show --id %s 后重试", parent.ID)
		}

		created, err := a.newEvent(actor, "case_created", request.Spec.CaseID)
		if err != nil {
			return nil, err
		}
		created.ParentCaseID, created.RootCaseID = request.Spec.ParentCaseID, request.Spec.RootCaseID
		created.Title, created.Project, created.SourceRef = request.Spec.Title, request.Spec.Project, request.Spec.SourceRef
		created.Objective, created.Acceptance = request.Spec.Objective, request.Spec.Acceptance
		created.Constraints, created.Priority, created.SpecRef = request.Spec.Constraints, request.Spec.Priority, request.Spec.SpecRef
		created.CaseVersion, created.CaseDigest = request.Spec.Version, request.Spec.Digest
		created.ToState, created.Owner, created.NextAction = string(statusOpen), actor.Name, "原子提交后向直属上级升级"

		prepared, err := a.newEvent(actor, "case_escalation_prepared", request.Spec.CaseID)
		if err != nil {
			return nil, err
		}
		prepared.ParentCaseID, prepared.RootCaseID = request.Spec.ParentCaseID, request.Spec.RootCaseID
		prepared.FromState, prepared.Title, prepared.Project = string(statusOpen), request.Spec.Title, request.Spec.Project
		prepared.Recipient, prepared.RecipientLabel = superior.Name, superior.Label
		prepared.CaseVersion, prepared.CaseDigest = request.Spec.Version, request.Spec.Digest
		prepared.SourceRef, prepared.NextAction, prepared.Note = request.Spec.SourceRef, request.NextAction, request.Reason
		prepared.DeliveryMode, prepared.DeliveryTarget, prepared.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, escalationDeliveryReason
		prepared.Delivery, prepared.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, superior.Name)
		payload, err := a.deliveryPayload(prepared)
		if err != nil {
			return nil, err
		}
		prepared.PayloadDigest = digestText(payload)
		return []Event{created, prepared}, nil
	})
	if err != nil {
		return err
	}
	prepared := result.Events[len(result.Events)-1]
	if a.DryRun {
		return a.output(prepared, fmt.Sprintf("DRY-RUN：将创建 escalation 子 case %s 并固定上交直属上级 %s；parent=%s delivery=%s", prepared.CaseID, prepared.Recipient, prepared.ParentCaseID, prepared.DeliveryID))
	}
	outcome, deliveryErr := a.processDelivery(prepared, "")
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(outcome, "")
		}
		return deliveryErr
	}
	return a.output(outcome, fmt.Sprintf("escalation 已送达直属上级 %s；case=%s parent=%s event=%s delivery=%s；上级应先 accept，再审批并 issue 给其直属经理", prepared.Recipient, prepared.CaseID, prepared.ParentCaseID, prepared.ID, prepared.DeliveryID))
}

func sameCaseEscalationContract(prepared, sent Event) bool {
	return prepared.CaseID == sent.CaseID && prepared.ParentCaseID == sent.ParentCaseID &&
		prepared.RootCaseID == sent.RootCaseID &&
		prepared.Recipient == sent.Recipient && prepared.RecipientLabel == sent.RecipientLabel &&
		prepared.Title == sent.Title && prepared.Project == sent.Project &&
		prepared.SourceRef == sent.SourceRef && prepared.NextAction == sent.NextAction &&
		prepared.CaseVersion == sent.CaseVersion && prepared.CaseDigest == sent.CaseDigest &&
		prepared.DeliveryID == sent.DeliveryID && prepared.PayloadDigest == sent.PayloadDigest &&
		prepared.DeliveryReason == sent.DeliveryReason &&
		effectiveEventDeliveryMode(prepared) == effectiveEventDeliveryMode(sent) &&
		effectiveEventDeliveryTarget(prepared) == effectiveEventDeliveryTarget(sent)
}

func formatCaseEscalationEnvelope(event Event, ref string) string {
	return fmt.Sprintf("[HQ escalation][%s] CASE=%s PARENT=%s VERSION=%d DIGEST=%s EVENT=%s DELIVERY=%s：直属经理已创建并上交 durable 返工 case；原因：%s；依据：%s；请先运行 `hq accept --event %s --next TEXT`，再按权限审批并 issue 给你的直属经理；下一步：%s；账本：%s",
		event.ActorLabel, event.CaseID, event.ParentCaseID, event.CaseVersion, event.CaseDigest,
		event.ID, event.DeliveryID, event.Note, event.SourceRef, event.ID, event.NextAction, ref)
}
