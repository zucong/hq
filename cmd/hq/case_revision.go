package main

import (
	"fmt"
	"strconv"
	"strings"
)

func assignmentRevisionAuthorizationDigest(old *caseAssignment, replacementID string, caseVersion int, caseDigest string) string {
	if old == nil {
		return ""
	}
	return requestDigest("assignment-revision-v1", old.EventID, old.AssignmentID, old.AssignmentDigest,
		replacementID, strconv.Itoa(caseVersion), caseDigest, old.Issuer, old.Recipient)
}

func expectedCaseStateForSupersede(status string) (string, error) {
	switch status {
	case "issued":
		return string(statusDispatched), nil
	case "accepted":
		return string(statusInProgress), nil
	case "rework":
		return string(statusReturned), nil
	case "submitted":
		return "", conflictf("assignment 已提交并等待 review，不能直接 supersede；冻结 acceptor 应先对当前 report 运行 `hq return --event REPORT_EVENT --reason 'requirements changed' --next '等待 replacement assignment'`，再重试原 `hq case revise --supersede-active`")
	default:
		return "", conflictf("assignment status=%s 不处于可安全替换的 issued|accepted|rework；先运行 `hq assignment show --id ASSIGNMENT_ID` 核验", status)
	}
}

func (s *ledgerState) validatePendingAssignmentRevisionSequence(event Event) error {
	if len(s.pendingAssignmentRevisions) == 0 {
		return nil
	}
	if len(s.pendingAssignmentRevisions) != 1 {
		return fmt.Errorf("账本同时存在多个未配对 assignment revision")
	}
	var caseID string
	var pending *pendingAssignmentRevision
	for id, value := range s.pendingAssignmentRevisions {
		caseID, pending = id, value
	}
	if pending == nil || pending.Superseded.ID == "" {
		return fmt.Errorf("assignment revision pending 记录损坏")
	}
	base := strings.TrimSuffix(pending.Superseded.CommandID, ":part:1")
	if pending.Revised.ID == "" {
		if event.Type != "case_revised" || event.CaseID != caseID || event.CommandID != base+":part:2" ||
			event.CommandDigest != pending.Superseded.CommandDigest || event.Sequence != pending.Superseded.Sequence+1 ||
			event.BasisEventID != pending.Superseded.ID {
			return fmt.Errorf("assignment_superseded 必须与紧邻、同原子 command/digest 的 case_revised 配对")
		}
		return nil
	}
	if event.Type != "issue_prepared" || event.CaseID != caseID || event.CommandID != base ||
		event.CommandDigest != pending.Superseded.CommandDigest || event.Sequence != pending.Revised.Sequence+1 ||
		event.BasisEventID != pending.Revised.ID || event.AuthorizationType != "revision" {
		return fmt.Errorf("case_revised assignment revision 必须与紧邻、同原子 command/digest 的 replacement issue_prepared 配对")
	}
	return nil
}

func (s *ledgerState) applyAssignmentSupersededEvent(event Event, cfg Config) error {
	if len(s.pendingAssignmentRevisions) != 0 {
		return fmt.Errorf("已有未配对 assignment revision，拒绝开始另一轮")
	}
	state, err := s.currentCase(event.CaseID)
	if err != nil {
		return err
	}
	active := s.activeAssignments(event.CaseID)
	if len(active) != 1 {
		return fmt.Errorf("assignment_superseded 要求 case 恰有一个 active assignment，实际=%d", len(active))
	}
	assignment := active[0]
	expectedState, err := expectedCaseStateForSupersede(assignment.Status)
	if err != nil {
		return err
	}
	if assignment.Issuer != event.Actor || assignment.Reviewer != event.Actor || assignment.Acceptor != event.Actor {
		return fmt.Errorf("只有冻结 issuer/reviewer/acceptor 可 supersede active assignment")
	}
	if assignment.Recipient != event.Recipient || state.Owner != assignment.Recipient ||
		state.Version != assignment.CaseVersion || state.Digest != assignment.CaseDigest || state.Status != expectedState {
		return fmt.Errorf("被 supersede assignment 未匹配当前 case owner/version/digest/status")
	}
	if event.RelatedEventID != assignment.EventID || event.AssignmentEventID != assignment.EventID ||
		event.SupersedesAssignmentEventID != assignment.EventID || event.AssignmentID != assignment.AssignmentID ||
		event.SupersedesAssignmentID != assignment.AssignmentID || event.CaseVersion != assignment.CaseVersion ||
		event.CaseDigest != assignment.CaseDigest || !eventMatchesAssignmentState(event, assignment) {
		return fmt.Errorf("assignment_superseded 未冻结旧 assignment 的完整合同")
	}
	if err := validateLedgerID("replacement_assignment_id", event.ReplacementAssignmentID); err != nil {
		return err
	}
	for _, existing := range s.assignments {
		if existing != nil && existing.AssignmentID == event.ReplacementAssignmentID {
			return fmt.Errorf("replacement_assignment_id 已存在：%s", event.ReplacementAssignmentID)
		}
	}
	if event.FromState != state.Status || event.ToState != string(statusRevisionPending) || event.Owner != event.Actor {
		return fmt.Errorf("assignment_superseded 必须把 case 原子转为 revision_pending 并交回冻结 issuer")
	}
	if err := validateStateTransition(actionSupersedeAssignment, event.FromState, event.ToState); err != nil {
		return err
	}
	if event.BasisEventID != state.SpecEventID {
		return fmt.Errorf("assignment_superseded 必须冻结当前 case spec event basis")
	}
	assignment.Consumed = true
	assignment.Status = "superseded"
	assignment.StatusEventID = event.ID
	assignment.SupersededByAssignmentID = event.ReplacementAssignmentID
	s.pendingAssignmentRevisions[event.CaseID] = &pendingAssignmentRevision{Superseded: event}
	_ = cfg
	return nil
}

func (s *ledgerState) validateAssignmentRevisionFinalInvariant() error {
	if len(s.pendingAssignmentRevisions) == 0 {
		return nil
	}
	ids := make([]string, 0, len(s.pendingAssignmentRevisions))
	for id := range s.pendingAssignmentRevisions {
		ids = append(ids, id)
	}
	return fmt.Errorf("assignment revision 缺少同一原子 supersede → case_revised → replacement issue_prepared：%s", strings.Join(ids, ","))
}

func (a *App) cmdCaseReviseActive(spec CaseSpec, revision caseRevisionOptions) error {
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if !actor.Rule.CanAccept {
		return permissionf("只有冻结 assignment issuer/reviewer/acceptor 可执行在途修订；actor=%s 缺少 can_accept", actor.Name)
	}
	commandID := stableCommandID("case-revise-active", actor.Name, spec.CaseID, strconv.Itoa(spec.Version))
	preflight, err := a.ledgerState()
	if err != nil {
		return err
	}
	var old *caseAssignment
	var targetRule AgentRule
	var roleCard RoleCard
	var stateBasis CaseState
	replacementID := stableCommandID("assignment", commandID)
	cleanDue := revision.Due
	if committed, ok := preflight.commands[commandID]; ok {
		if committed.Type != "issue_prepared" || committed.AuthorizationType != "revision" || committed.CaseID != spec.CaseID || committed.CaseVersion != spec.Version {
			return conflictf("command_id 已由非匹配 assignment revision 使用：%s", commandID)
		}
		spec.ParentCaseID, spec.RootCaseID, spec.Project = committed.ParentCaseID, committed.RootCaseID, committed.Project
		spec.Digest = caseSpecDigest(spec)
		if !revision.DueExplicit {
			cleanDue = committed.DueAt
		}
		old = preflight.assignments[committed.SupersedesAssignmentEventID]
		if old == nil {
			return fmt.Errorf("已提交 assignment revision 缺少旧 assignment=%s", committed.SupersedesAssignmentEventID)
		}
		targetRule, _ = a.Config.exactRule(committed.Recipient)
		roleCard, err = a.Config.roleCardForAgent(targetRule)
		if err != nil {
			return err
		}
		replacementID = committed.AssignmentID
	} else {
		state, stateErr := preflight.currentCase(spec.CaseID)
		if stateErr != nil {
			return stateErr
		}
		active := preflight.activeAssignments(spec.CaseID)
		if len(active) != 1 {
			return conflictf("--supersede-active 要求 case=%s 恰有一个 active assignment，当前=%d；运行 `hq assignment list --case %s` 核验。没有 active assignment 时删除 --supersede-active；多个时先收敛冲突", spec.CaseID, len(active), spec.CaseID)
		}
		old = active[0]
		if _, stateErr := expectedCaseStateForSupersede(old.Status); stateErr != nil {
			return stateErr
		}
		if actor.Name != old.Issuer || actor.Name != old.Reviewer || actor.Name != old.Acceptor {
			return permissionf("只有冻结 issuer/reviewer/acceptor=%s 可替换 assignment=%s；当前 actor=%s。请把加急变更交给该直属负责人，不要越级 message 员工", old.Issuer, old.AssignmentID, actor.Name)
		}
		targetRule, _ = a.Config.exactRule(old.Recipient)
		if targetRule.Name == "" || targetRule.ReportsTo != actor.Name || !targetRule.CanReceiveOrder || !targetRule.CanAccept {
			return conflictf("原 assignee=%s 已停用、失去接令资格或不再直属 issuer=%s；先按正式 staff/role 变更流程收敛，不得把在途修订改投他人", old.Recipient, actor.Name)
		}
		roleCard, err = a.Config.roleCardForAgent(targetRule)
		if err != nil {
			return err
		}
		if _, err := verifyAgentRoleCardArtifact(a.Config, a.HQRoot, targetRule); err != nil {
			return fmt.Errorf("replacement assignee 角色卡核验失败：%w", err)
		}
		if state.Owner != old.Recipient || state.Version != old.CaseVersion || state.Digest != old.CaseDigest {
			return conflictf("active assignment 已不匹配 case 当前 owner/version/digest；运行 `hq case show --id %s` 与 `hq assignment show --id %s` 后处理真实状态", spec.CaseID, old.AssignmentID)
		}
		if spec.Version != state.Version+1 {
			return fmt.Errorf("case 新版本必须是 %d，实际=%d", state.Version+1, spec.Version)
		}
		spec.ParentCaseID, spec.RootCaseID, spec.Project = state.ParentCaseID, state.RootCaseID, state.Project
		spec.Digest = caseSpecDigest(spec)
		stateBasis = *state
		if !revision.DueExplicit {
			cleanDue = old.DueAt
		}
	}
	if cleanDue != "" && !mustParseTime(cleanDue).After(a.Store.NowTime()) {
		return fmt.Errorf("replacement assignment due=%s 已到期；请显式提供未来 `--due RFC3339`，或使用 `--due=` 清除截止时间", cleanDue)
	}
	digest := requestDigest("case-revise-active", actor.Name, spec.CaseID, strconv.Itoa(spec.Version), spec.Digest,
		old.EventID, old.AssignmentID, old.AssignmentDigest, replacementID, roleCard.Digest, targetRule.SeatDigest,
		revision.Next, revision.Note, cleanDue)
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
			return nil, fmt.Errorf("replacement assignee 角色卡在 revise admission 期间漂移：%w", err)
		}
		state, err := ledger.currentCase(spec.CaseID)
		if err != nil {
			return nil, err
		}
		if stateBasis.ID == "" || state.Version != stateBasis.Version || state.Digest != stateBasis.Digest ||
			state.Status != stateBasis.Status || state.Owner != stateBasis.Owner || state.LastEventID != stateBasis.LastEventID {
			return nil, conflictf("case %s 在 active revise admission 期间已变化；运行 `hq case show --id %s` 后基于最新状态重试", spec.CaseID, spec.CaseID)
		}
		active := ledger.activeAssignments(spec.CaseID)
		if len(active) != 1 || active[0].EventID != old.EventID || active[0].Status != old.Status || active[0].StatusEventID != old.StatusEventID {
			return nil, conflictf("assignment 在 active revise admission 期间已变化；运行 `hq assignment list --case %s` 后重试，禁止制造第二份任务", spec.CaseID)
		}
		currentOld := active[0]
		if used := ledger.assignmentCapacityUsed(targetRule.Name); used > targetRule.MaxWIP {
			return nil, conflictf("员工 %s 当前 assignment capacity=%d 已超过 max_wip=%d；先收敛账本异常", targetRule.Name, used, targetRule.MaxWIP)
		}
		basis := state.SpecEventID
		if basis == "" {
			basis = state.LastEventID
		}
		superseded, err := a.newEvent(actor, "assignment_superseded", spec.CaseID)
		if err != nil {
			return nil, err
		}
		superseded.RelatedEventID = currentOld.EventID
		copyAssignmentStateBinding(&superseded, currentOld)
		superseded.SupersedesAssignmentEventID, superseded.SupersedesAssignmentID = currentOld.EventID, currentOld.AssignmentID
		superseded.ReplacementAssignmentID = replacementID
		superseded.CaseVersion, superseded.CaseDigest = currentOld.CaseVersion, currentOld.CaseDigest
		superseded.Recipient, superseded.RecipientLabel = targetRule.Name, targetRule.Label
		superseded.FromState, superseded.ToState, superseded.Owner = state.Status, string(statusRevisionPending), actor.Name
		superseded.BasisEventID, superseded.NextAction, superseded.Note = basis, revision.Next, revision.Note

		revised, err := a.newEvent(actor, "case_revised", spec.CaseID)
		if err != nil {
			return nil, err
		}
		revised.RelatedEventID, revised.PreviousCaseDigest, revised.BasisEventID = basis, state.Digest, superseded.ID
		revised.ParentCaseID, revised.RootCaseID = spec.ParentCaseID, spec.RootCaseID
		revised.Title, revised.Project, revised.SourceRef = spec.Title, spec.Project, spec.SourceRef
		revised.Objective, revised.Acceptance, revised.Constraints = spec.Objective, spec.Acceptance, spec.Constraints
		revised.Priority, revised.SpecRef = spec.Priority, spec.SpecRef
		revised.CaseVersion, revised.CaseDigest = spec.Version, spec.Digest

		prepared, err := a.newEvent(actor, "issue_prepared", spec.CaseID)
		if err != nil {
			return nil, err
		}
		prepared.FromState, prepared.Project = string(statusRevisionPending), spec.Project
		prepared.Recipient, prepared.RecipientLabel = targetRule.Name, targetRule.Label
		prepared.CaseVersion, prepared.CaseDigest = spec.Version, spec.Digest
		prepared.AssignmentID, prepared.AssignmentIssuer = replacementID, actor.Name
		prepared.SupersedesAssignmentEventID, prepared.SupersedesAssignmentID = currentOld.EventID, currentOld.AssignmentID
		prepared.BasisEventID = revised.ID
		prepared.AssigneeSeatVersion, prepared.AssigneeSeatDigest = targetRule.SeatVersion, targetRule.SeatDigest
		prepared.RoleCardID, prepared.RoleCardVersion = roleCard.ID, roleCard.Version
		prepared.RoleCardDigest, prepared.RoleCardManualPath = roleCard.Digest, roleCard.ManualPath
		prepared.Reviewer, prepared.ReviewerLabel = actor.Name, actor.Label
		prepared.Acceptor, prepared.AcceptorLabel = actor.Name, actor.Label
		prepared.DueAt, prepared.NextAction, prepared.Note = cleanDue, revision.Next, revision.Note
		prepared.AuthorizationType = "revision"
		prepared.AuthorizationDigest = assignmentRevisionAuthorizationDigest(currentOld, replacementID, spec.Version, spec.Digest)
		prepared.AssignmentDigest = assignmentContractDigest(prepared)
		prepared.Urgency = messageUrgencyUrgent
		prepared.DeliveryMode, prepared.DeliveryTarget, prepared.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "assignment-revision-urgent"
		prepared.Delivery, prepared.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, targetRule.Name)
		payload, err := a.deliveryPayload(prepared)
		if err != nil {
			return nil, err
		}
		prepared.PayloadDigest = digestText(payload)
		return []Event{superseded, revised, prepared}, nil
	})
	if err != nil {
		return err
	}
	releaseOriginFence()
	originFenceHeld = false
	prepared := result.Events[len(result.Events)-1]
	if a.DryRun {
		return a.output(prepared, fmt.Sprintf("DRY-RUN：case=%s 将从 assignment=%s 原子修订为 v%d replacement=%s；delivery=%s", spec.CaseID, old.AssignmentID, spec.Version, prepared.AssignmentID, prepared.DeliveryID))
	}
	outcome, deliveryErr := a.processDelivery(prepared, "")
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(outcome, "")
		}
		return deliveryErr
	}
	summary := fmt.Sprintf("case=%s 已加急修订到 v%d；旧 assignment=%s status=superseded；replacement=%s 已推送给 %s；delivery=%s",
		spec.CaseID, spec.Version, old.AssignmentID, prepared.AssignmentID, targetRule.Name, prepared.DeliveryID)
	if a.Config.isManager(actor.Rule) {
		ledger, ledgerErr := a.ledgerState()
		if ledgerErr == nil {
			directive, directiveErr := managerPostIssueDirective(ledger, a.Config, actor.Name, spec.CaseID, prepared.AssignmentID)
			if directiveErr == nil {
				outcome.ActorDirective = &directive
				summary += "；" + directive.text()
			}
		}
	}
	return a.output(outcome, summary)
}
