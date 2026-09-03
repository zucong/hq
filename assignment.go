package main

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

const assignmentContractVersion = 2

func normalizeAssignmentDue(value string) (string, error) {
	clean := strings.TrimSpace(value)
	if clean == "" {
		return "", nil
	}
	due, err := time.Parse(time.RFC3339, clean)
	if err != nil {
		return "", fmt.Errorf("due 必须是 RFC3339：%w", err)
	}
	return due.UTC().Format(time.RFC3339), nil
}

type AssignmentView struct {
	AssignmentID         string `json:"assignment_id"`
	AssignmentEventID    string `json:"assignment_event_id"`
	StatusEventID        string `json:"status_event_id,omitempty"`
	StatusUpdatedAt      string `json:"status_updated_at,omitempty"`
	AssignmentDigest     string `json:"assignment_digest,omitempty"`
	ContractVersion      int    `json:"contract_version"`
	AssigneeSeatVersion  int    `json:"assignee_seat_version,omitempty"`
	AssigneeSeatDigest   string `json:"assignee_seat_digest,omitempty"`
	RoleCardID           string `json:"role_card_id,omitempty"`
	RoleCardVersion      int    `json:"role_card_version,omitempty"`
	RoleCardDigest       string `json:"role_card_digest,omitempty"`
	RoleCardManualPath   string `json:"role_card_manual_path,omitempty"`
	CaseID               string `json:"case_id"`
	CaseVersion          int    `json:"case_version"`
	CaseDigest           string `json:"case_digest,omitempty"`
	Project              string `json:"project,omitempty"`
	Issuer               string `json:"issuer"`
	Assignee             string `json:"assignee"`
	Reviewer             string `json:"reviewer"`
	Acceptor             string `json:"acceptor"`
	DueAt                string `json:"due_at,omitempty"`
	Status               string `json:"status"`
	DeliveryID           string `json:"delivery_id,omitempty"`
	ActivationStatus     string `json:"activation_status,omitempty"`
	ActivationAttempts   int    `json:"activation_attempts,omitempty"`
	ActivationNextAction string `json:"activation_next_action,omitempty"`
}

// frozenSeatContract is the immutable employee identity captured by an
// assignment. A stable agent slug is only an address; it must never allow a
// replacement seat incarnation to inherit work that was issued to an older
// role/manual/permission contract.
type frozenSeatContract struct {
	Assignee        string
	SeatVersion     int
	SeatDigest      string
	RoleCardID      string
	RoleCardVersion int
	RoleCardDigest  string
	ManualPath      string
}

func frozenSeatFromEvent(event Event) frozenSeatContract {
	return frozenSeatContract{
		Assignee: event.Recipient, SeatVersion: event.AssigneeSeatVersion, SeatDigest: event.AssigneeSeatDigest,
		RoleCardID: event.RoleCardID, RoleCardVersion: event.RoleCardVersion,
		RoleCardDigest: event.RoleCardDigest, ManualPath: event.RoleCardManualPath,
	}
}

func frozenSeatFromAssignment(assignment *caseAssignment) frozenSeatContract {
	if assignment == nil {
		return frozenSeatContract{}
	}
	return frozenSeatContract{
		Assignee: assignment.Recipient, SeatVersion: assignment.AssigneeSeatVersion, SeatDigest: assignment.AssigneeSeatDigest,
		RoleCardID: assignment.RoleCardID, RoleCardVersion: assignment.RoleCardVersion,
		RoleCardDigest: assignment.RoleCardDigest, ManualPath: assignment.RoleCardManualPath,
	}
}

// frozenSeatForDeliveredBusinessEvent resolves the assignee contract behind a
// delivered issue or assignment-backed report. Report recipients are
// reviewers, so deriving the assignee from Event.Recipient would validate the
// wrong seat.
func frozenSeatForDeliveredBusinessEvent(ledger *ledgerState, event Event) (frozenSeatContract, bool, error) {
	switch event.Type {
	case "issue_sent":
		return frozenSeatFromEvent(event), true, nil
	case "report_sent":
		if event.AssignmentEventID == "" {
			return frozenSeatContract{}, false, nil
		}
		assignment := ledger.assignments[event.AssignmentEventID]
		if assignment == nil {
			return frozenSeatContract{}, false, fmt.Errorf("report 引用的 assignment 不存在：%s", event.AssignmentEventID)
		}
		if !eventMatchesAssignmentState(event, assignment) {
			return frozenSeatContract{}, false, fmt.Errorf("report 与冻结 assignment 合同不一致：%s", assignment.AssignmentID)
		}
		return frozenSeatFromAssignment(assignment), true, nil
	default:
		return frozenSeatContract{}, false, nil
	}
}

func currentSeatForFrozenContract(cfg Config, contract frozenSeatContract) (AgentRule, error) {
	rule, ok := cfg.exactRule(contract.Assignee)
	if !ok {
		return AgentRule{}, conflictf("assignment 冻结的员工席位已停用或不存在：%s", contract.Assignee)
	}
	if rule.SeatVersion != contract.SeatVersion || rule.SeatDigest != contract.SeatDigest {
		return AgentRule{}, conflictf("员工 %s 当前 seat=%d/%s 与 assignment 冻结 seat=%d/%s 不一致；必须先完成当前任务，或显式创建新任务合同",
			contract.Assignee, rule.SeatVersion, rule.SeatDigest, contract.SeatVersion, contract.SeatDigest)
	}
	if rule.RoleCardID != contract.RoleCardID || rule.RoleCardVersion != contract.RoleCardVersion ||
		rule.RoleCardDigest != contract.RoleCardDigest || rule.ManualPath != contract.ManualPath {
		return AgentRule{}, conflictf("员工 %s 当前角色卡与 assignment 冻结角色卡 %s 不一致", contract.Assignee,
			roleCardKey(contract.RoleCardID, contract.RoleCardVersion))
	}
	card, err := cfg.roleCardForAgent(rule)
	if err != nil {
		return AgentRule{}, conflictf("员工 %s 当前角色卡绑定无效：%v", contract.Assignee, err)
	}
	if card.ID != contract.RoleCardID || card.Version != contract.RoleCardVersion ||
		card.Digest != contract.RoleCardDigest || card.ManualPath != contract.ManualPath {
		return AgentRule{}, conflictf("员工 %s 当前 registry role card 与 assignment 冻结角色卡不一致", contract.Assignee)
	}
	return rule, nil
}

func (a *App) verifyCurrentFrozenSeat(contract frozenSeatContract) error {
	rule, err := currentSeatForFrozenContract(a.Config, contract)
	if err != nil {
		return err
	}
	if _, err := verifyAgentRoleCardArtifact(a.Config, a.HQRoot, rule); err != nil {
		return conflictf("员工 %s 的冻结角色手册核验失败：%v", contract.Assignee, err)
	}
	return nil
}

func (a *App) preflightActionFrozenSeat(actionID, actor string) error {
	ledger, err := a.ledgerState()
	if err != nil {
		return err
	}
	actionable, ok := ledger.actionableEvent(actionID)
	original, semanticOK := semanticDeliveredEvent(actionable, ledger.events)
	if !ok || !semanticOK || original.Recipient != actor {
		// The authoritative transaction returns the normal not-found/permission
		// error. Avoid changing error ordering during this artifact preflight.
		return nil
	}
	contract, required, err := frozenSeatForDeliveredBusinessEvent(ledger, original)
	if err != nil {
		return err
	}
	if !required {
		return nil
	}
	return a.verifyCurrentFrozenSeat(contract)
}

// validateCandidateSeatContinuity prevents a registry replacement from
// silently rebinding durable work to another incarnation of the same slug.
// Pending issue delivery and every unconsumed assignment both reserve the
// exact seat contract until they converge to a terminal business outcome.
func (s *ledgerState) validateCandidateSeatContinuity(cfg Config) error {
	type binding struct {
		key      string
		contract frozenSeatContract
	}
	bindings := make([]binding, 0)
	wipByAssignee := map[string]int{}
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment == nil || assignment.Consumed {
			continue
		}
		key := "assignment:" + assignment.AssignmentID
		bindings = append(bindings, binding{key: key, contract: frozenSeatFromAssignment(assignment)})
		wipByAssignee[assignment.Recipient]++
	}
	for _, record := range s.deliveries {
		if record == nil || record.Origin.Type != "issue_prepared" || record.Status == deliverySent {
			continue
		}
		bindings = append(bindings, binding{key: "pending-delivery:" + record.Origin.DeliveryID, contract: frozenSeatFromEvent(record.Origin)})
		wipByAssignee[record.Origin.Recipient]++
	}
	sort.Slice(bindings, func(i, j int) bool { return bindings[i].key < bindings[j].key })
	for _, binding := range bindings {
		if _, err := currentSeatForFrozenContract(cfg, binding.contract); err != nil {
			return fmt.Errorf("%s 仍占用冻结员工席位：%w", binding.key, err)
		}
	}
	wipAssignees := make([]string, 0, len(wipByAssignee))
	for assignee := range wipByAssignee {
		wipAssignees = append(wipAssignees, assignee)
	}
	sort.Strings(wipAssignees)
	for _, assignee := range wipAssignees {
		rule, ok := cfg.exactRule(assignee)
		if !ok {
			return conflictf("员工 %s 有 %d 个 active/pending assignment，但当前 seat 已停用或不存在", assignee, wipByAssignee[assignee])
		}
		if wipByAssignee[assignee] > rule.MaxWIP {
			return conflictf("员工 %s 当前 active/pending assignment=%d，超过 registry max_wip=%d；必须先收敛现有委派，不能通过直接改 config 制造超额 WIP", assignee, wipByAssignee[assignee], rule.MaxWIP)
		}
	}

	witness, witnessErr := cfg.approvalWitness()
	if witnessErr != nil && len(s.approvals) != 0 {
		return witnessErr
	}
	approvalIDs := make([]string, 0, len(s.approvals))
	for id := range s.approvals {
		approvalIDs = append(approvalIDs, id)
	}
	sort.Strings(approvalIDs)
	for _, id := range approvalIDs {
		record := s.approvals[id]
		if record == nil || (record.Status != "requested" && record.Status != "granted") {
			continue
		}
		target, ok := cfg.exactRule(record.Request.Recipient)
		if !ok || !target.CanReceiveOrder || !target.CanAccept || !cfg.isManager(target) || target.ReportsTo != witness.Name {
			return fmt.Errorf("approval:%s status=%s 仍占用冻结 target seat，但当前 target 已停用、无接令/验收权限、不是部门经理或不再直属总裁秘书", id, record.Status)
		}
		if target.SeatVersion != record.Request.AssigneeSeatVersion || target.SeatDigest != record.Request.AssigneeSeatDigest {
			return fmt.Errorf("approval:%s status=%s 仍占用冻结 target seat：当前=%d/%s frozen=%d/%s；请先 revoke/expire approval，或消费后收敛对应 assignment，再更新 seat", id, record.Status, target.SeatVersion, target.SeatDigest, record.Request.AssigneeSeatVersion, record.Request.AssigneeSeatDigest)
		}
	}
	return nil
}

func assignmentContractDigest(event Event) string {
	return requestDigest("assignment-contract-v2", event.AssignmentID, event.CaseID,
		strconv.Itoa(event.CaseVersion), event.CaseDigest, event.Project,
		event.AssignmentIssuer, event.Recipient, event.Reviewer, event.Acceptor, event.DueAt,
		strconv.Itoa(event.AssigneeSeatVersion), event.AssigneeSeatDigest,
		event.RoleCardID, strconv.Itoa(event.RoleCardVersion), event.RoleCardDigest, event.RoleCardManualPath)
}

func acceptanceReceiptDigest(deliveredEventID string, original, receipt Event) string {
	return requestDigest("acceptance-receipt-v1", deliveredEventID, original.CaseID,
		original.CaseDigest, original.ArtifactRef, original.SourceRef, original.Verification,
		original.AssignmentID, original.AssignmentDigest, receipt.Actor, receipt.Note, receipt.NextAction)
}

func copyAssignmentBinding(dst *Event, source Event) {
	dst.AssignmentEventID = source.AssignmentEventID
	dst.AssignmentID, dst.AssignmentDigest = source.AssignmentID, source.AssignmentDigest
	dst.AssignmentIssuer = source.AssignmentIssuer
	dst.AssigneeSeatVersion, dst.AssigneeSeatDigest = source.AssigneeSeatVersion, source.AssigneeSeatDigest
	dst.RoleCardID, dst.RoleCardVersion = source.RoleCardID, source.RoleCardVersion
	dst.RoleCardDigest, dst.RoleCardManualPath = source.RoleCardDigest, source.RoleCardManualPath
	dst.Reviewer, dst.ReviewerLabel = source.Reviewer, source.ReviewerLabel
	dst.Acceptor, dst.AcceptorLabel, dst.DueAt = source.Acceptor, source.AcceptorLabel, source.DueAt
}

func copyAssignmentStateBinding(dst *Event, assignment *caseAssignment) {
	dst.AssignmentEventID = assignment.EventID
	dst.AssignmentID, dst.AssignmentDigest = assignment.AssignmentID, assignment.AssignmentDigest
	dst.AssignmentIssuer = assignment.Issuer
	dst.AssigneeSeatVersion, dst.AssigneeSeatDigest = assignment.AssigneeSeatVersion, assignment.AssigneeSeatDigest
	dst.RoleCardID, dst.RoleCardVersion = assignment.RoleCardID, assignment.RoleCardVersion
	dst.RoleCardDigest, dst.RoleCardManualPath = assignment.RoleCardDigest, assignment.RoleCardManualPath
	dst.Reviewer, dst.ReviewerLabel = assignment.Reviewer, assignment.ReviewerLabel
	dst.Acceptor, dst.AcceptorLabel, dst.DueAt = assignment.Acceptor, assignment.AcceptorLabel, assignment.DueAt
}

func sameAssignmentBinding(a, b Event) bool {
	return a.AssignmentEventID == b.AssignmentEventID &&
		a.AssignmentID == b.AssignmentID && a.AssignmentDigest == b.AssignmentDigest &&
		a.AssignmentIssuer == b.AssignmentIssuer &&
		a.AssigneeSeatVersion == b.AssigneeSeatVersion && a.AssigneeSeatDigest == b.AssigneeSeatDigest &&
		a.RoleCardID == b.RoleCardID && a.RoleCardVersion == b.RoleCardVersion &&
		a.RoleCardDigest == b.RoleCardDigest && a.RoleCardManualPath == b.RoleCardManualPath &&
		a.Reviewer == b.Reviewer && a.ReviewerLabel == b.ReviewerLabel &&
		a.Acceptor == b.Acceptor && a.AcceptorLabel == b.AcceptorLabel && a.DueAt == b.DueAt
}

func eventMatchesAssignmentState(event Event, assignment *caseAssignment) bool {
	return event.AssignmentEventID == assignment.EventID &&
		event.AssignmentID == assignment.AssignmentID && event.AssignmentDigest == assignment.AssignmentDigest &&
		event.AssignmentIssuer == assignment.Issuer &&
		event.AssigneeSeatVersion == assignment.AssigneeSeatVersion && event.AssigneeSeatDigest == assignment.AssigneeSeatDigest &&
		event.RoleCardID == assignment.RoleCardID && event.RoleCardVersion == assignment.RoleCardVersion &&
		event.RoleCardDigest == assignment.RoleCardDigest && event.RoleCardManualPath == assignment.RoleCardManualPath &&
		event.Reviewer == assignment.Reviewer && event.ReviewerLabel == assignment.ReviewerLabel &&
		event.Acceptor == assignment.Acceptor && event.AcceptorLabel == assignment.AcceptorLabel && event.DueAt == assignment.DueAt
}

func validateAssignmentContractEvent(event Event, cfg Config) error {
	if event.AssignmentID == "" {
		return fmt.Errorf("event_version=%d 的 issue 必须携带 assignment_id", eventSchemaVersion)
	}
	if err := validateLedgerID("assignment_id", event.AssignmentID); err != nil {
		return err
	}
	if err := validateDigest("assignment_digest", event.AssignmentDigest); err != nil {
		return err
	}
	if event.AssignmentIssuer != event.Actor {
		return fmt.Errorf("assignment issuer 必须等于 issue actor")
	}
	if event.Reviewer == "" || event.Acceptor == "" || event.ReviewerLabel == "" || event.AcceptorLabel == "" {
		return fmt.Errorf("assignment contract 缺少 reviewer/acceptor 及历史 label")
	}
	for role, name := range map[string]string{"reviewer": event.Reviewer, "acceptor": event.Acceptor} {
		rule, ok := historicalRule(cfg, name)
		if !ok || !rule.CanAccept {
			return fmt.Errorf("assignment %s=%s 未登记或无 can_accept", role, name)
		}
	}
	if event.Reviewer != event.AssignmentIssuer || event.Acceptor != event.AssignmentIssuer ||
		event.ReviewerLabel != event.ActorLabel || event.AcceptorLabel != event.ActorLabel {
		return fmt.Errorf("assignment reviewer/acceptor 必须等于已授权 issuer 并保留相同历史 label")
	}
	if event.AssigneeSeatVersion < 1 {
		return fmt.Errorf("assignment 缺少 assignee seat version")
	}
	if err := validateDigest("assignee_seat_digest", event.AssigneeSeatDigest); err != nil {
		return err
	}
	if !roleCardIDPattern.MatchString(event.RoleCardID) || event.RoleCardVersion < 1 {
		return fmt.Errorf("assignment 缺少合法 role card id/version")
	}
	if err := validateDigest("role_card_digest", event.RoleCardDigest); err != nil {
		return err
	}
	card, ok := cfg.roleCard(event.RoleCardID, event.RoleCardVersion)
	if !ok || card.Digest != event.RoleCardDigest || card.ManualPath != event.RoleCardManualPath {
		return fmt.Errorf("assignment role card 未匹配 registry 中保留的冻结版本")
	}
	if card.Department == "" {
		return fmt.Errorf("assignment role card department 为空")
	}
	if event.AssignmentDigest != assignmentContractDigest(event) {
		return fmt.Errorf("assignment_digest 与冻结合同字段不匹配")
	}
	if event.DueAt != "" {
		due, err := time.Parse(time.RFC3339, event.DueAt)
		if err != nil {
			return fmt.Errorf("assignment due_at 不是 RFC3339：%w", err)
		}
		if due.UTC().Format(time.RFC3339) != event.DueAt {
			return fmt.Errorf("assignment due_at 必须是规范化 UTC RFC3339")
		}
		if event.Type == "issue_prepared" && !due.After(mustEventTime(event)) {
			return fmt.Errorf("assignment due_at 必须晚于签发事件时间")
		}
	}
	return nil
}

func assignmentFromIssue(event Event, cfg Config) (*caseAssignment, error) {
	if err := validateAssignmentContractEvent(event, cfg); err != nil {
		return nil, err
	}
	return &caseAssignment{
		EventID: event.ID, StatusEventID: event.ID, AssignmentID: event.AssignmentID, AssignmentDigest: event.AssignmentDigest,
		AssigneeSeatVersion: event.AssigneeSeatVersion, AssigneeSeatDigest: event.AssigneeSeatDigest,
		RoleCardID: event.RoleCardID, RoleCardVersion: event.RoleCardVersion,
		RoleCardDigest: event.RoleCardDigest, RoleCardManualPath: event.RoleCardManualPath,
		CaseID: event.CaseID, CaseVersion: event.CaseVersion, CaseDigest: event.CaseDigest, Project: event.Project,
		Issuer: event.AssignmentIssuer, Recipient: event.Recipient, Reviewer: event.Reviewer, ReviewerLabel: event.ReviewerLabel,
		Acceptor: event.Acceptor, AcceptorLabel: event.AcceptorLabel,
		DueAt: event.DueAt, Status: "issued", ContractVersion: assignmentContractVersion,
		SubmissionGeneration: event.ID,
	}, nil
}

func assignmentFromDeliveredIssue(event, origin Event, cfg Config) (*caseAssignment, error) {
	if origin.Type != "issue_prepared" {
		return nil, fmt.Errorf("assignment origin 必须是 issue_prepared")
	}
	if event.CaseID != origin.CaseID || event.Project != origin.Project || event.Recipient != origin.Recipient ||
		event.CaseVersion != origin.CaseVersion || event.CaseDigest != origin.CaseDigest ||
		event.AssignmentID != origin.AssignmentID || event.AssignmentDigest != origin.AssignmentDigest ||
		event.AssignmentIssuer != origin.AssignmentIssuer || event.Reviewer != origin.Reviewer ||
		event.ReviewerLabel != origin.ReviewerLabel || event.Acceptor != origin.Acceptor ||
		event.AcceptorLabel != origin.AcceptorLabel || event.DueAt != origin.DueAt ||
		event.AssigneeSeatVersion != origin.AssigneeSeatVersion || event.AssigneeSeatDigest != origin.AssigneeSeatDigest ||
		event.RoleCardID != origin.RoleCardID || event.RoleCardVersion != origin.RoleCardVersion ||
		event.RoleCardDigest != origin.RoleCardDigest || event.RoleCardManualPath != origin.RoleCardManualPath {
		return nil, fmt.Errorf("issue terminal 未冻结匹配的 assignment contract")
	}
	assignment, err := assignmentFromIssue(origin, cfg)
	if err != nil {
		return nil, err
	}
	assignment.EventID, assignment.StatusEventID, assignment.SubmissionGeneration = event.ID, event.ID, event.ID
	return assignment, nil
}

func (a *caseAssignment) view() AssignmentView {
	return AssignmentView{
		AssignmentID: a.AssignmentID, AssignmentEventID: a.EventID, StatusEventID: a.StatusEventID, AssignmentDigest: a.AssignmentDigest,
		ContractVersion:     a.ContractVersion,
		AssigneeSeatVersion: a.AssigneeSeatVersion, AssigneeSeatDigest: a.AssigneeSeatDigest,
		RoleCardID: a.RoleCardID, RoleCardVersion: a.RoleCardVersion,
		RoleCardDigest: a.RoleCardDigest, RoleCardManualPath: a.RoleCardManualPath,
		CaseID: a.CaseID, CaseVersion: a.CaseVersion, CaseDigest: a.CaseDigest, Project: a.Project,
		Issuer: a.Issuer, Assignee: a.Recipient, Reviewer: a.Reviewer, Acceptor: a.Acceptor,
		DueAt: a.DueAt, Status: a.Status,
	}
}

func (s *ledgerState) assignmentViews() []AssignmentView {
	views := make([]AssignmentView, 0, len(s.assignmentList))
	for _, eventID := range s.assignmentList {
		if assignment := s.assignments[eventID]; assignment != nil {
			view := assignment.view()
			if statusEvent, ok := s.events[assignment.StatusEventID]; ok {
				view.StatusUpdatedAt = statusEvent.At
			}
			for _, record := range s.deliveries {
				if record.Terminal.ID != assignment.EventID {
					continue
				}
				view.DeliveryID = record.Origin.DeliveryID
				view.ActivationStatus = record.ActivationStatus
				view.ActivationAttempts = record.ActivationAttemptCount
				if assignment.Status == "issued" {
					_, view.ActivationNextAction = describeDelivery(record)
				}
				break
			}
			views = append(views, view)
		}
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].AssignmentID == views[j].AssignmentID {
			return views[i].AssignmentEventID < views[j].AssignmentEventID
		}
		return views[i].AssignmentID < views[j].AssignmentID
	})
	return views
}

func (s *ledgerState) activeAssignmentCountForAssignee(name string) int {
	count := 0
	for _, eventID := range s.assignmentList {
		assignment := s.assignments[eventID]
		if assignment != nil && assignment.Recipient == name && !assignment.Consumed {
			count++
		}
	}
	return count
}

func (a *App) cmdAssignment(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq assignment list|show ...")
	}
	switch args[0] {
	case "list":
		return a.cmdAssignmentList(args[1:])
	case "show":
		return a.cmdAssignmentShow(args[1:])
	default:
		return fmt.Errorf("未知 assignment 子命令 %q", args[0])
	}
}

func (a *App) cmdAssignmentList(args []string) error {
	fs := newLeafParser("assignment list")
	fs.SetOutput(a.Err)
	caseID := fs.String("case", "", "case_id")
	assignee := fs.String("assignee", "", "assignee agent")
	status := fs.String("status", "", "issued|accepted|submitted|rework|completed|reported|returned")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *caseID != "" {
		if err := validateCaseID(strings.TrimSpace(*caseID)); err != nil {
			return err
		}
	}
	cleanStatus := strings.TrimSpace(*status)
	if cleanStatus != "" && cleanStatus != "issued" && cleanStatus != "accepted" && cleanStatus != "submitted" &&
		cleanStatus != "rework" && cleanStatus != "completed" && cleanStatus != "reported" && cleanStatus != "returned" {
		return fmt.Errorf("--status 只能是 issued/accepted/submitted/rework/completed/reported/returned")
	}
	ledger, err := a.strictLedgerStateReadOnly()
	if err != nil {
		return err
	}
	views := ledger.assignmentViews()
	filtered := make([]AssignmentView, 0, len(views))
	for _, view := range views {
		if *caseID != "" && view.CaseID != strings.TrimSpace(*caseID) {
			continue
		}
		if *assignee != "" && view.Assignee != strings.TrimSpace(*assignee) {
			continue
		}
		if cleanStatus != "" && view.Status != cleanStatus {
			continue
		}
		filtered = append(filtered, view)
	}
	if a.JSON {
		return a.output(filtered, "")
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "HQ assignments：%d", len(filtered))
	for _, view := range filtered {
		fmt.Fprintf(&builder, "\n%s case=%s@v%d assignee=%s reviewer=%s acceptor=%s status=%s activation=%s due=%s",
			view.AssignmentID, view.CaseID, view.CaseVersion, view.Assignee, view.Reviewer, view.Acceptor, view.Status, view.ActivationStatus, view.DueAt)
	}
	return a.output(filtered, builder.String())
}

func (a *App) cmdAssignmentShow(args []string) error {
	fs := newLeafParser("assignment show")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "assignment_id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cleanID := strings.TrimSpace(*id)
	if err := validateLedgerID("assignment_id", cleanID); err != nil {
		return err
	}
	ledger, err := a.strictLedgerStateReadOnly()
	if err != nil {
		return err
	}
	for _, view := range ledger.assignmentViews() {
		if view.AssignmentID == cleanID || view.AssignmentEventID == cleanID {
			return a.output(view, fmt.Sprintf("assignment=%s case=%s@v%d assignee=%s role=%s@%d reviewer=%s acceptor=%s status=%s activation=%s activation_attempts=%d next=%s digest=%s due=%s",
				view.AssignmentID, view.CaseID, view.CaseVersion, view.Assignee,
				view.RoleCardID, view.RoleCardVersion, view.Reviewer, view.Acceptor,
				view.Status, view.ActivationStatus, view.ActivationAttempts, view.ActivationNextAction, view.AssignmentDigest, view.DueAt))
		}
	}
	return fmt.Errorf("assignment 不存在：%s", cleanID)
}

func (a *App) strictLedgerStateReadOnly() (*ledgerState, error) {
	if store, ok := a.Store.(interface {
		LedgerStateReadOnly(Config) (*ledgerState, error)
	}); ok {
		return store.LedgerStateReadOnly(a.Config)
	}
	return a.ledgerState()
}
