package main

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type CaseStatus string

const (
	statusOpen            CaseStatus = "open"
	statusDispatched      CaseStatus = "dispatched"
	statusInProgress      CaseStatus = "in_progress"
	statusReported        CaseStatus = "reported"
	statusBlocked         CaseStatus = "blocked"
	statusNeedsDecision   CaseStatus = "needs_decision"
	statusFindingReported CaseStatus = "finding_reported"
	statusFindingAccepted CaseStatus = "finding_accepted"
	statusEscalated       CaseStatus = "escalated"
	statusAccepted        CaseStatus = "accepted"
	statusReturned        CaseStatus = "returned"
	statusClosed          CaseStatus = "closed"
)

var knownCaseStatuses = map[CaseStatus]bool{
	statusOpen: true, statusDispatched: true, statusInProgress: true,
	statusReported: true, statusBlocked: true, statusNeedsDecision: true,
	statusFindingReported: true, statusFindingAccepted: true,
	statusEscalated: true, statusAccepted: true, statusReturned: true, statusClosed: true,
}

type transitionAction string

const (
	actionCreate               transitionAction = "create"
	actionIssue                transitionAction = "issue"
	actionAcceptOrder          transitionAction = "accept_order"
	actionReportCompleted      transitionAction = "report_completed"
	actionReportBlocked        transitionAction = "report_blocked"
	actionReportNeedsDecision  transitionAction = "report_needs_decision"
	actionReportFinding        transitionAction = "report_finding"
	actionReportReturned       transitionAction = "report_returned"
	actionAcceptReport         transitionAction = "accept_report"
	actionAcceptFinding        transitionAction = "accept_finding"
	actionAcceptBlocked        transitionAction = "accept_blocked"
	actionAcceptNeedsDecision  transitionAction = "accept_needs_decision"
	actionAcceptReturnedReport transitionAction = "accept_returned_report"
	actionEscalate             transitionAction = "escalate"
	actionAcceptEscalation     transitionAction = "accept_escalation"
	actionReturnEscalation     transitionAction = "return_escalation"
	actionReturnOrder          transitionAction = "return_order"
	actionReturnReport         transitionAction = "return_report"
	actionClose                transitionAction = "close"
)

// transitionTable is the only state-transition contract used by both command
// writes and ledger replay. A missing edge is a hard failure.
var transitionTable = map[transitionAction]map[CaseStatus]CaseStatus{
	actionCreate: {
		"": statusOpen,
	},
	actionIssue: {
		statusOpen: statusDispatched, statusReturned: statusDispatched,
		statusAccepted: statusDispatched, statusBlocked: statusDispatched,
		statusNeedsDecision: statusDispatched, statusFindingAccepted: statusDispatched,
	},
	actionAcceptOrder: {statusDispatched: statusInProgress},
	actionReportCompleted: {
		statusOpen: statusReported, statusDispatched: statusReported, statusInProgress: statusReported,
		statusReturned: statusReported, statusAccepted: statusReported, statusBlocked: statusReported,
		statusNeedsDecision: statusReported, statusFindingAccepted: statusReported,
	},
	actionReportBlocked: {
		statusOpen: statusBlocked, statusDispatched: statusBlocked, statusInProgress: statusBlocked,
		statusReturned: statusBlocked, statusAccepted: statusBlocked, statusBlocked: statusBlocked,
		statusNeedsDecision: statusBlocked, statusFindingAccepted: statusBlocked,
	},
	actionReportNeedsDecision: {
		statusOpen: statusNeedsDecision, statusDispatched: statusNeedsDecision, statusInProgress: statusNeedsDecision,
		statusReturned: statusNeedsDecision, statusAccepted: statusNeedsDecision, statusBlocked: statusNeedsDecision,
		statusNeedsDecision: statusNeedsDecision, statusFindingAccepted: statusNeedsDecision,
	},
	actionReportFinding: {
		statusOpen: statusFindingReported, statusDispatched: statusFindingReported, statusInProgress: statusFindingReported,
		statusReturned: statusFindingReported, statusAccepted: statusFindingReported, statusBlocked: statusFindingReported,
		statusNeedsDecision: statusFindingReported, statusFindingAccepted: statusFindingReported,
	},
	actionReportReturned: {
		statusOpen: statusReturned, statusDispatched: statusReturned, statusInProgress: statusReturned,
		statusReturned: statusReturned, statusAccepted: statusReturned, statusBlocked: statusReturned,
		statusNeedsDecision: statusReturned, statusFindingAccepted: statusReturned,
	},
	actionAcceptReport:         {statusReported: statusAccepted},
	actionAcceptFinding:        {statusFindingReported: statusFindingAccepted},
	actionAcceptBlocked:        {statusBlocked: statusBlocked},
	actionAcceptNeedsDecision:  {statusNeedsDecision: statusNeedsDecision},
	actionAcceptReturnedReport: {statusReturned: statusAccepted},
	actionEscalate:             {statusOpen: statusEscalated},
	actionAcceptEscalation:     {statusEscalated: statusAccepted},
	actionReturnEscalation:     {statusEscalated: statusReturned},
	actionReturnOrder:          {statusDispatched: statusReturned},
	actionReturnReport: {
		statusReported: statusReturned, statusFindingReported: statusReturned,
		statusBlocked: statusReturned, statusNeedsDecision: statusReturned,
		statusReturned: statusReturned,
	},
	actionClose: {
		statusOpen: statusClosed, statusDispatched: statusClosed, statusInProgress: statusClosed,
		statusReported: statusClosed, statusBlocked: statusClosed, statusNeedsDecision: statusClosed,
		statusFindingReported: statusClosed, statusFindingAccepted: statusClosed,
		statusAccepted: statusClosed, statusReturned: statusClosed,
	},
}

func validateStateTransition(action transitionAction, from, to string) error {
	fromStatus, toStatus := CaseStatus(from), CaseStatus(to)
	if from != "" && !knownCaseStatuses[fromStatus] {
		return fmt.Errorf("未知前态 %q", from)
	}
	if !knownCaseStatuses[toStatus] {
		return fmt.Errorf("未知后态 %q", to)
	}
	edges, ok := transitionTable[action]
	if !ok {
		return fmt.Errorf("未知 transition action %q", action)
	}
	expected, ok := edges[fromStatus]
	if !ok || expected != toStatus {
		return fmt.Errorf("非法状态转移 %s: %q -> %q", action, from, to)
	}
	return nil
}

func reportTargetState(result string) (string, transitionAction, error) {
	switch result {
	case "completed":
		return string(statusReported), actionReportCompleted, nil
	case "blocked":
		return string(statusBlocked), actionReportBlocked, nil
	case "needs-decision":
		return string(statusNeedsDecision), actionReportNeedsDecision, nil
	case "finding":
		return string(statusFindingReported), actionReportFinding, nil
	case "returned":
		return string(statusReturned), actionReportReturned, nil
	default:
		return "", "", fmt.Errorf("result 只能是 completed/blocked/needs-decision/finding/returned")
	}
}

func acceptTargetState(original Event) (string, error) {
	if original.Type == "issue_sent" {
		return string(statusInProgress), nil
	}
	if original.Type == "case_escalation_sent" {
		return string(statusAccepted), nil
	}
	if original.Type != "report_sent" {
		return "", fmt.Errorf("事件类型 %s 不可 accept", original.Type)
	}
	switch original.Result {
	case "completed", "returned":
		return string(statusAccepted), nil
	case "blocked":
		return string(statusBlocked), nil
	case "needs-decision":
		return string(statusNeedsDecision), nil
	case "finding":
		return string(statusFindingAccepted), nil
	default:
		return "", fmt.Errorf("report_sent result 非法：%s", original.Result)
	}
}

type caseAssignment struct {
	EventID             string
	StatusEventID       string
	AssignmentID        string
	AssignmentDigest    string
	ContractVersion     int
	AssigneeSeatVersion int
	AssigneeSeatDigest  string
	RoleCardID          string
	RoleCardVersion     int
	RoleCardDigest      string
	RoleCardManualPath  string
	CaseID              string
	CaseVersion         int
	CaseDigest          string
	Project             string
	Issuer              string
	Recipient           string
	Reviewer            string
	ReviewerLabel       string
	Acceptor            string
	AcceptorLabel       string
	DueAt               string
	Status              string
	Accepted            bool
	Consumed            bool
	// SubmissionGeneration is stable throughout one report attempt/review
	// round and advances only when the reviewer returns that report for rework.
	// SubmissionEventID reserves the one durable report_prepared allowed in the
	// round, so changing report arguments cannot bypass a failed/unknown outbox.
	SubmissionGeneration string
	SubmissionEventID    string
}

type approvalLedgerRecord struct {
	Request  Event
	Grant    Event
	Terminal Event
	Status   string
}

type ledgerState struct {
	snapshot               Snapshot
	events                 map[string]Event
	commands               map[string]Event
	resolved               map[string]bool
	assignments            map[string]*caseAssignment
	assignmentList         []string
	caseGenerations        map[string]string
	ownerSubmissions       map[string]string
	deliveries             map[string]*deliveryRecord
	businessDeliveryRounds map[string]string
	nudges                 map[string]*nudgeLedgerRecord
	reminders              map[string]*reminderLedgerRecord
	reminderCases          map[string]string
	estops                 map[string]*estopLedgerRecord
	approvals              map[string]*approvalLedgerRecord
	deliveryWakeSpends     map[string]int
	deliveryLastWake       map[string]string
	turnBundleReservations map[string]string
}

func newLedgerState() *ledgerState {
	return &ledgerState{
		snapshot:               newSnapshot(),
		events:                 map[string]Event{},
		commands:               map[string]Event{},
		resolved:               map[string]bool{},
		assignments:            map[string]*caseAssignment{},
		caseGenerations:        map[string]string{},
		ownerSubmissions:       map[string]string{},
		deliveries:             map[string]*deliveryRecord{},
		businessDeliveryRounds: map[string]string{},
		nudges:                 map[string]*nudgeLedgerRecord{},
		reminders:              map[string]*reminderLedgerRecord{},
		reminderCases:          map[string]string{},
		estops:                 map[string]*estopLedgerRecord{},
		approvals:              map[string]*approvalLedgerRecord{},
		deliveryWakeSpends:     map[string]int{},
		deliveryLastWake:       map[string]string{},
		turnBundleReservations: map[string]string{},
	}
}

func ownerSubmissionKey(caseID, actor, generation string) string {
	return strings.Join([]string{caseID, actor, generation}, "\x00")
}

// caseGeneration identifies the latest durable business-state round for a
// case. Outbox-only events (prepared/attempted/failed/unknown) deliberately do
// not advance it, so retrying a report resolves to the original command and
// delivery instead of manufacturing a second submission.
func (s *ledgerState) caseGeneration(caseID string) string {
	return s.caseGenerations[caseID]
}

func validateLedger(events []Event, cfg Config) (*ledgerState, error) {
	state := newLedgerState()
	for _, event := range events {
		if err := state.validateAndApply(event, cfg); err != nil {
			return nil, err
		}
	}
	if err := state.validateLedgerFinalInvariants(cfg); err != nil {
		return nil, err
	}
	return state, nil
}

func historicalRule(cfg Config, name string) (AgentRule, bool) {
	if rule, ok := configRuleIncludingDisabled(cfg, name); ok {
		return rule, true
	}
	return cfg.ruleFor(name)
}

func (s *ledgerState) currentCase(caseID string) (*CaseState, error) {
	state, ok := s.snapshot.Cases[caseID]
	if !ok {
		return nil, fmt.Errorf("case 不存在：%s", caseID)
	}
	return state, nil
}

func (s *ledgerState) assignmentFor(caseID, actor string) (string, bool) {
	assignment, ok := s.assignmentRecordFor(caseID, actor)
	if ok {
		return assignment.EventID, true
	}
	if state := s.snapshot.Cases[caseID]; state != nil && state.Owner == actor {
		return "", true
	}
	return "", false
}

func (s *ledgerState) assignmentRecordFor(caseID, actor string) (*caseAssignment, bool) {
	for i := len(s.assignmentList) - 1; i >= 0; i-- {
		assignment := s.assignments[s.assignmentList[i]]
		if !assignment.Consumed && assignment.CaseID == caseID && assignment.Recipient == actor {
			return assignment, true
		}
	}
	return nil, false
}

func (s *ledgerState) hasEverReceivedCase(actor string) bool {
	for _, assignment := range s.assignments {
		if assignment.Recipient == actor {
			return true
		}
	}
	return false
}

func (s *ledgerState) hasEverReceivedCaseAssignment(caseID, actor string) bool {
	for _, assignment := range s.assignments {
		if assignment.CaseID == caseID && assignment.Recipient == actor {
			return true
		}
	}
	return false
}

func (s *ledgerState) unclosedDescendantsPostOrder(parentID string) []*CaseState {
	children := make(map[string][]string)
	for id, candidate := range s.snapshot.Cases {
		if candidate != nil && id != parentID {
			children[candidate.ParentCaseID] = append(children[candidate.ParentCaseID], id)
		}
	}
	for parent := range children {
		sort.Strings(children[parent])
	}
	result := make([]*CaseState, 0)
	visited := map[string]bool{parentID: true}
	var visit func(string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		for _, childID := range children[id] {
			visit(childID)
		}
		candidate := s.snapshot.Cases[id]
		if candidate != nil && candidate.Status != string(statusClosed) {
			result = append(result, candidate)
		}
	}
	for _, childID := range children[parentID] {
		visit(childID)
	}
	return result
}

func renderDescendantStates(descendants []*CaseState) string {
	values := make([]string, 0, len(descendants))
	for _, descendant := range descendants {
		values = append(values, fmt.Sprintf("%s(status=%s)", descendant.ID, descendant.Status))
	}
	return strings.Join(values, ", ")
}

func (s *ledgerState) assignmentReviewEventID(assignment *caseAssignment) string {
	if assignment == nil || assignment.SubmissionEventID == "" {
		return "REPORT_EVENT"
	}
	origin := s.events[assignment.SubmissionEventID]
	if record := s.deliveries[origin.DeliveryID]; record != nil && record.Terminal.ID != "" {
		return record.Terminal.ID
	}
	return assignment.SubmissionEventID
}

func (s *ledgerState) escalationReviewEventID(caseID, fallback string) string {
	latest := Event{}
	for _, event := range s.events {
		if event.CaseID == caseID && event.Type == "case_escalation_sent" && !s.resolved[event.ID] && event.Sequence > latest.Sequence {
			latest = event
		}
	}
	if latest.ID != "" {
		return latest.ID
	}
	return fallback
}

func (s *ledgerState) closureStep(state *CaseState, accountCloser string) string {
	active := s.activeAssignments(state.ID)
	prerequisites := make([]string, 0, len(active)+1)
	for _, assignment := range active {
		switch assignment.Status {
		case "issued":
			prerequisites = append(prerequisites, fmt.Sprintf("actor=%s 先运行 hq accept --event %s --next TEXT（或 hq return --event %s --reason TEXT --next TEXT）", assignment.Recipient, assignment.EventID, assignment.EventID))
		case "accepted", "rework":
			prerequisites = append(prerequisites, fmt.Sprintf("actor=%s 先完成本轮并运行 hq report --case %s --result completed --artifact PATH --verify TEXT --next TEXT；再由 actor=%s 对新 report event 运行 hq accept --event REPORT_EVENT --next TEXT（或 hq return --event REPORT_EVENT --reason TEXT --next TEXT）", assignment.Recipient, state.ID, assignment.Acceptor))
		case "submitted":
			eventID := s.assignmentReviewEventID(assignment)
			prerequisites = append(prerequisites, fmt.Sprintf("actor=%s 先运行 hq accept --event %s --next TEXT（或 hq return --event %s --reason TEXT --next TEXT）", assignment.Acceptor, eventID, eventID))
		default:
			prerequisites = append(prerequisites, fmt.Sprintf("assignment=%s status=%s：先运行 hq assignment show --id %s 与 hq history --case %s，再由 assignee=%s、reviewer=%s 按账本状态收敛", assignment.AssignmentID, assignment.Status, assignment.AssignmentID, state.ID, assignment.Recipient, assignment.Acceptor))
		}
	}
	if state.Status == string(statusEscalated) {
		eventID := s.escalationReviewEventID(state.ID, state.LastEventID)
		prerequisites = append(prerequisites, fmt.Sprintf("actor=%s 先运行 hq accept --event %s --next TEXT（或 hq return --event %s --reason TEXT --next TEXT）", state.Owner, eventID, eventID))
	}
	closeCommand := fmt.Sprintf("actor=%s 运行 hq close --case %s --reason TEXT --source PATH", accountCloser, state.ID)
	if len(prerequisites) == 0 {
		return closeCommand
	}
	return fmt.Sprintf("case=%s(status=%s)：%s；上述合同收敛后再由 %s", state.ID, state.Status, strings.Join(prerequisites, "；"), closeCommand)
}

func (s *ledgerState) renderPostOrderClosureGuidance(descendants []*CaseState, parentID, accountCloser string) string {
	steps := make([]string, 0, len(descendants)+1)
	for _, descendant := range descendants {
		steps = append(steps, s.closureStep(descendant, accountCloser))
	}
	if parent := s.snapshot.Cases[parentID]; parent != nil {
		steps = append(steps, s.closureStep(parent, accountCloser))
	}
	return strings.Join(steps, "；")
}

func isClosureWorkflowDelivery(eventType string) bool {
	switch eventType {
	case "issue_prepared", "report_prepared", "case_escalation_prepared", "event_accepted", "event_returned":
		return true
	default:
		return false
	}
}

func deliveryBlocksCaseClosure(status string) bool {
	switch status {
	case deliveryPrepared, deliveryQueued, deliveryAttempted, deliveryFailedPreSend, deliveryUnknown:
		return true
	default:
		return false
	}
}

func (s *ledgerState) unsettledClosureDeliveries(caseID string, includeDescendants bool) []*deliveryRecord {
	scope := map[string]bool{caseID: true}
	if includeDescendants {
		children := make(map[string][]string)
		for id, candidate := range s.snapshot.Cases {
			if candidate != nil {
				children[candidate.ParentCaseID] = append(children[candidate.ParentCaseID], id)
			}
		}
		queue := []string{caseID}
		for len(queue) != 0 {
			parent := queue[0]
			queue = queue[1:]
			for _, childID := range children[parent] {
				if !scope[childID] {
					scope[childID] = true
					queue = append(queue, childID)
				}
			}
		}
	}
	blockers := make([]*deliveryRecord, 0)
	for _, record := range s.deliveries {
		if record != nil && scope[record.Origin.CaseID] && isClosureWorkflowDelivery(record.Origin.Type) && deliveryBlocksCaseClosure(record.Status) {
			blockers = append(blockers, record)
		}
	}
	sort.Slice(blockers, func(i, j int) bool {
		if blockers[i].Origin.Sequence != blockers[j].Origin.Sequence {
			return blockers[i].Origin.Sequence < blockers[j].Origin.Sequence
		}
		return blockers[i].Origin.DeliveryID < blockers[j].Origin.DeliveryID
	})
	return blockers
}

func renderClosureDeliveryBlockers(blockers []*deliveryRecord) string {
	values := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		values = append(values, fmt.Sprintf("%s(case=%s,status=%s)", blocker.Origin.DeliveryID, blocker.Origin.CaseID, blocker.Status))
	}
	return strings.Join(values, ", ")
}

func renderClosureDeliveryRecoveryCommands(blockers []*deliveryRecord) string {
	commands := make([]string, 0, len(blockers)*2)
	for _, blocker := range blockers {
		id := blocker.Origin.DeliveryID
		switch blocker.Status {
		case deliveryPrepared:
			commands = append(commands, "hq reconcile", fmt.Sprintf("hq delivery status --id %s", id))
		case deliveryQueued:
			commands = append(commands, "hq reconcile", fmt.Sprintf("hq delivery status --id %s", id))
		case deliveryAttempted:
			commands = append(commands, "hq reconcile", fmt.Sprintf("hq delivery status --id %s（禁止盲目重投）", id))
		case deliveryUnknown:
			commands = append(commands, fmt.Sprintf("hq delivery status --id %s", id), fmt.Sprintf("由总裁办运维核对后运行 hq delivery resolve --id %s --outcome delivered|not-delivered --reason TEXT --evidence PATH；若核定 not-delivered，再运行 hq delivery retry --id %s", id, id))
		case deliveryFailedPreSend:
			commands = append(commands, fmt.Sprintf("hq delivery retry --id %s", id))
		}
	}
	return strings.Join(commands, "；")
}

func (s *ledgerState) validateCaseProjectLineage(caseID, parentID, project string) error {
	if parentID != "" {
		parent := s.snapshot.Cases[parentID]
		if parent == nil {
			return fmt.Errorf("parent case 不存在：%s", parentID)
		}
		if project != parent.Project {
			return fmt.Errorf("child %s project=%q 与 parent %s project=%q 不一致", caseID, project, parentID, parent.Project)
		}
	}
	childIDs := make([]string, 0)
	for childID, child := range s.snapshot.Cases {
		if child != nil && child.ParentCaseID == caseID {
			childIDs = append(childIDs, childID)
		}
	}
	sort.Strings(childIDs)
	for _, childID := range childIDs {
		child := s.snapshot.Cases[childID]
		if child.Project != project {
			return fmt.Errorf("parent %s project=%q 与现有 child %s project=%q 不一致；已有 child 时不可单独变更 parent project", caseID, project, childID, child.Project)
		}
	}
	return nil
}

// soleRootCase returns the sole root of a ledger that has already passed the
// strict single-space invariant. It deliberately returns nil for empty or
// malformed state so callers can still emit a fail-closed admission error.
func (s *ledgerState) soleRootCase() *CaseState {
	var root *CaseState
	for _, state := range s.snapshot.Cases {
		if state == nil || state.ParentCaseID != "" {
			continue
		}
		if root != nil {
			return nil
		}
		root = state
	}
	return root
}

func (s *ledgerState) caseProjectLineageBasis(caseID string) string {
	state := s.snapshot.Cases[caseID]
	if state == nil {
		return ""
	}
	parts := []string{"case", caseID, "parent", state.ParentCaseID}
	if parent := s.snapshot.Cases[state.ParentCaseID]; parent != nil {
		parts = append(parts, "parent-project", parent.Project)
	}
	childIDs := make([]string, 0)
	for childID, child := range s.snapshot.Cases {
		if child != nil && child.ParentCaseID == caseID {
			childIDs = append(childIDs, childID)
		}
	}
	sort.Strings(childIDs)
	for _, childID := range childIDs {
		parts = append(parts, "child", childID, s.snapshot.Cases[childID].Project)
	}
	return requestDigest("case-project-lineage", parts...)
}

func (s *ledgerState) validateCaseTreeFinalInvariants() error {
	caseIDs := make([]string, 0, len(s.snapshot.Cases))
	for caseID := range s.snapshot.Cases {
		caseIDs = append(caseIDs, caseID)
	}
	sort.Strings(caseIDs)
	if len(caseIDs) == 0 {
		return nil
	}
	roots := make([]string, 0, 1)
	for _, caseID := range caseIDs {
		state := s.snapshot.Cases[caseID]
		if state != nil && state.ParentCaseID == "" {
			roots = append(roots, caseID)
		}
	}
	if len(roots) != 1 {
		return fmt.Errorf("HQ single-project invariant：必须恰有一个 root case，实际=%d roots=%s", len(roots), strings.Join(roots, ","))
	}
	root := s.snapshot.Cases[roots[0]]
	if strings.TrimSpace(root.Project) == "" {
		return fmt.Errorf("HQ single-project invariant：root case %s 必须冻结非空 project", root.ID)
	}
	if root.RootCaseID != "" {
		return fmt.Errorf("HQ single-project invariant：root case %s 不得携带 root_case_id", root.ID)
	}
	for _, caseID := range caseIDs {
		state := s.snapshot.Cases[caseID]
		if state == nil {
			return fmt.Errorf("case tree 包含 nil state：%s", caseID)
		}
		if err := s.validateCaseProjectLineage(caseID, state.ParentCaseID, state.Project); err != nil {
			return fmt.Errorf("case tree project invariant：%w", err)
		}
		if state.Project != root.Project {
			return fmt.Errorf("HQ single-project invariant：case %s project=%q 与 root %s project=%q 不一致", caseID, state.Project, root.ID, root.Project)
		}
		if caseID != root.ID {
			if state.ParentCaseID == "" || state.RootCaseID != root.ID {
				return fmt.Errorf("HQ single-project invariant：non-root case %s 必须有 parent 并冻结 root_case_id=%s", caseID, root.ID)
			}
			seen := map[string]bool{caseID: true}
			ancestorID := state.ParentCaseID
			for ancestorID != root.ID {
				if seen[ancestorID] {
					return fmt.Errorf("HQ single-project invariant：case %s 存在 parent cycle", caseID)
				}
				seen[ancestorID] = true
				ancestor := s.snapshot.Cases[ancestorID]
				if ancestor == nil || ancestor.ParentCaseID == "" {
					return fmt.Errorf("HQ single-project invariant：case %s 的 parent chain 未连接唯一 root %s", caseID, root.ID)
				}
				ancestorID = ancestor.ParentCaseID
			}
		}
		if state.Status == string(statusClosed) {
			if descendants := s.unclosedDescendantsPostOrder(caseID); len(descendants) != 0 {
				return fmt.Errorf("closed case %s 仍有未关闭 descendants（post-order=%s）", caseID, renderDescendantStates(descendants))
			}
			if blockers := s.unsettledClosureDeliveries(caseID, false); len(blockers) != 0 {
				return fmt.Errorf("closed case %s 仍有未收敛 workflow delivery：%s", caseID, renderClosureDeliveryBlockers(blockers))
			}
		}
	}
	return nil
}

func (s *ledgerState) validateAssignment(id, caseID, actor string) error {
	assignment, ok := s.assignments[id]
	if !ok || assignment.Consumed || assignment.CaseID != caseID || assignment.Recipient != actor {
		return fmt.Errorf("assignment %q 对 case=%s actor=%s 无效或已消费", id, caseID, actor)
	}
	return nil
}

func requireNoStateFields(event Event) error {
	if event.ToState != "" || event.Owner != "" {
		return fmt.Errorf("事件 %s 不得改变状态或 owner", event.Type)
	}
	return nil
}

func requireEventFields(event Event, fields ...struct{ name, value string }) error {
	var missing []string
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("事件 %s 缺少类型必填字段：%s", event.Type, strings.Join(missing, ", "))
	}
	return nil
}

func eventField(name, value string) struct{ name, value string } {
	return struct{ name, value string }{name: name, value: value}
}

func validateEventRequiredFields(event Event) error {
	if handled, err := validateApprovalMessageRequiredFields(event); handled {
		return err
	}
	if isInfrastructureEvent(event.Type) {
		return validateInfrastructureEventFields(event)
	}
	require := func(fields ...struct{ name, value string }) error { return requireEventFields(event, fields...) }
	switch event.Type {
	case "case_created":
		if err := require(eventField("title", event.Title), eventField("source_ref", event.SourceRef),
			eventField("objective", event.Objective), eventField("acceptance", event.Acceptance),
			eventField("constraints", event.Constraints), eventField("priority", event.Priority),
			eventField("case_digest", event.CaseDigest),
			eventField("to_state", event.ToState), eventField("owner", event.Owner), eventField("next_action", event.NextAction)); err != nil {
			return err
		}
		if event.CaseVersion != 1 {
			return fmt.Errorf("case_created version 必须为 1")
		}
		return nil
	case "case_revised":
		if err := require(eventField("related_event_id", event.RelatedEventID), eventField("title", event.Title),
			eventField("source_ref", event.SourceRef), eventField("objective", event.Objective),
			eventField("acceptance", event.Acceptance), eventField("constraints", event.Constraints),
			eventField("priority", event.Priority), eventField("case_digest", event.CaseDigest)); err != nil {
			return err
		}
		if event.CaseVersion < 1 {
			return fmt.Errorf("case_revised 缺少合法 case_version")
		}
		if event.CaseVersion > 1 && event.PreviousCaseDigest == "" {
			return fmt.Errorf("case_revised 缺少 previous_case_digest")
		}
		return nil
	case "case_escalation_prepared":
		if event.CaseVersion != 1 {
			return fmt.Errorf("case_escalation_prepared case_version 必须为 1")
		}
		return require(eventField("parent_case_id", event.ParentCaseID), eventField("from_state", event.FromState),
			eventField("title", event.Title), eventField("recipient", event.Recipient),
			eventField("recipient_label", event.RecipientLabel), eventField("source_ref", event.SourceRef),
			eventField("next_action", event.NextAction), eventField("note", event.Note),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest), eventField("case_digest", event.CaseDigest))
	case "case_escalation_sent":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("attempt_event_id", event.AttemptEventID),
			eventField("parent_case_id", event.ParentCaseID), eventField("from_state", event.FromState),
			eventField("to_state", event.ToState), eventField("owner", event.Owner),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("source_ref", event.SourceRef), eventField("next_action", event.NextAction),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest), eventField("case_digest", event.CaseDigest))
	case "report_sent":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("attempt_event_id", event.AttemptEventID),
			eventField("from_state", event.FromState), eventField("to_state", event.ToState), eventField("owner", event.Owner),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest))
	case "report_prepared":
		if err := require(eventField("from_state", event.FromState), eventField("title", event.Title),
			eventField("result", event.Result), eventField("recipient", event.Recipient),
			eventField("recipient_label", event.RecipientLabel), eventField("next_action", event.NextAction),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest)); err != nil {
			return err
		}
		switch event.Result {
		case "completed":
			if event.SourceRef == "" && event.ArtifactRef == "" {
				return fmt.Errorf("事件 report_prepared completed 缺少 source_ref/artifact_ref")
			}
		case "finding":
			return require(eventField("severity", event.Severity), eventField("source_ref", event.SourceRef),
				eventField("location", event.Location), eventField("verification", event.Verification))
		case "blocked", "needs-decision", "returned":
			return require(eventField("source_ref", event.SourceRef), eventField("note", event.Note))
		}
	case "event_accepted":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("from_state", event.FromState),
			eventField("to_state", event.ToState), eventField("owner", event.Owner), eventField("next_action", event.NextAction),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest))
	case "event_returned":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("from_state", event.FromState),
			eventField("to_state", event.ToState), eventField("owner", event.Owner), eventField("note", event.Note),
			eventField("next_action", event.NextAction), eventField("recipient", event.Recipient),
			eventField("recipient_label", event.RecipientLabel), eventField("delivery", event.Delivery),
			eventField("delivery_id", event.DeliveryID), eventField("payload_digest", event.PayloadDigest))
	case "case_closed":
		return require(eventField("from_state", event.FromState), eventField("to_state", event.ToState),
			eventField("source_ref", event.SourceRef), eventField("note", event.Note), eventField("next_action", event.NextAction))
	case "delivery_attempted":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("recipient", event.Recipient),
			eventField("recipient_label", event.RecipientLabel), eventField("delivery", event.Delivery),
			eventField("delivery_id", event.DeliveryID), eventField("payload_digest", event.PayloadDigest))
	case "delivery_failed_pre_send", "delivery_unknown", "accept_notice_sent", "return_notice_sent":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("attempt_event_id", event.AttemptEventID),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest))
	case "delivery_resolved_not_sent", "delivery_resolved_sent":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("attempt_event_id", event.AttemptEventID),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest), eventField("note", event.Note),
			eventField("resolution_ref", event.ResolutionRef))
	case "delivery_queued", "delivery_context_claimed":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("recipient", event.Recipient),
			eventField("recipient_label", event.RecipientLabel), eventField("delivery", event.Delivery),
			eventField("delivery_id", event.DeliveryID), eventField("payload_digest", event.PayloadDigest),
			eventField("delivery_mode", event.DeliveryMode), eventField("delivery_target", event.DeliveryTarget))
	case "delivery_budget_reset":
		return require(eventField("related_event_id", event.RelatedEventID), eventField("recipient", event.Recipient),
			eventField("recipient_label", event.RecipientLabel), eventField("delivery_reason", event.DeliveryReason))
	}
	return nil
}

func (s *ledgerState) validateAndApply(event Event, cfg Config) error {
	if event.Version != eventSchemaVersion {
		return fmt.Errorf("不支持事件版本 %d；当前唯一版本=%d", event.Version, eventSchemaVersion)
	}
	if event.TurnBundleParentAttempt != "" {
		switch event.Type {
		case "delivery_attempted", "message_sent", "delivery_context_claimed":
		default:
			return fmt.Errorf("事件 %s 不允许 turn_bundle_parent_attempt_id", event.Type)
		}
	}
	if hasTurnBundleManifest(event) && event.Type != "delivery_attempted" {
		return fmt.Errorf("事件 %s 不允许 turn bundle manifest 字段", event.Type)
	}
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.CommandID) == "" || strings.TrimSpace(event.CommandDigest) == "" ||
		strings.TrimSpace(event.Type) == "" || strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.ActorLabel) == "" {
		return fmt.Errorf("事件缺少 event_id/command_id/command_digest/event_type/actor/actor_label")
	}
	if strings.TrimSpace(event.CaseID) == "" && (event.FromState != "" || event.ToState != "" || event.Owner != "") {
		return fmt.Errorf("无 case_id 的事件不得携带 case state/owner mutation")
	}
	if err := validateLedgerID("event_id", event.ID); err != nil {
		return err
	}
	if err := validateLedgerID("command_id", event.CommandID); err != nil {
		return err
	}
	if err := validateDigest("command_digest", event.CommandDigest); err != nil {
		return err
	}
	if err := validateDigest("previous_event_hash", event.PreviousEventHash); err != nil {
		return err
	}
	if err := validateDigest("event_hash", event.EventHash); err != nil {
		return err
	}
	if _, exists := s.events[event.ID]; exists {
		return fmt.Errorf("event_id 重复：%s", event.ID)
	}
	if previous, exists := s.commands[event.CommandID]; exists {
		return fmt.Errorf("command outcome 重复：%s（已有 event=%s）", event.CommandID, previous.ID)
	}
	if event.Sequence <= 0 {
		return fmt.Errorf("sequence 必须是正数 int64：%d", event.Sequence)
	}
	if s.snapshot.EventCount == 0 && event.Sequence != 1 {
		return fmt.Errorf("账本首条 sequence 必须从 1 开始：%d", event.Sequence)
	}
	if event.Sequence <= s.snapshot.LastSequence {
		return fmt.Errorf("sequence 必须全局严格递增：事件=%d 上一实际事件=%d", event.Sequence, s.snapshot.LastSequence)
	}
	expectedPrevious := s.snapshot.LastEventHash
	if expectedPrevious == "" {
		expectedPrevious = genesisEventHash
	}
	if event.PreviousEventHash != expectedPrevious {
		return fmt.Errorf("previous_event_hash 不匹配：事件=%s 期望=%s", event.PreviousEventHash, expectedPrevious)
	}
	expectedHash, err := hashEvent(event)
	if err != nil {
		return fmt.Errorf("计算 event_hash：%w", err)
	}
	if event.EventHash != expectedHash {
		return fmt.Errorf("event_hash 不匹配：事件=%s 计算=%s", event.EventHash, expectedHash)
	}
	if event.CaseID == "" {
		if !strings.HasPrefix(event.Type, "message_") && event.Type != "delivery_attempted" &&
			event.Type != "delivery_failed_pre_send" && event.Type != "delivery_unknown" &&
			event.Type != "delivery_resolved_not_sent" && event.Type != "delivery_resolved_sent" &&
			event.Type != "delivery_queued" && event.Type != "delivery_context_claimed" &&
			event.Type != "delivery_budget_reset" {
			return fmt.Errorf("case_id：缺少 case_id")
		}
	} else if err := validateCaseID(event.CaseID); err != nil {
		return fmt.Errorf("case_id：%w", err)
	}
	if _, err := time.Parse(time.RFC3339, event.At); err != nil {
		return fmt.Errorf("事件时间格式错误：%w", err)
	}
	actorRule, actorKnown := historicalRule(cfg, event.Actor)
	if !actorKnown {
		return fmt.Errorf("actor 未登记：%s", event.Actor)
	}
	if event.Recipient != "" {
		if _, ok := historicalRule(cfg, event.Recipient); !ok {
			return fmt.Errorf("recipient 未登记：%s", event.Recipient)
		}
		if event.RecipientLabel == "" {
			return fmt.Errorf("recipient=%s 缺少历史 recipient_label", event.Recipient)
		}
	}
	if event.FromState != "" && !knownCaseStatuses[CaseStatus(event.FromState)] {
		return fmt.Errorf("未知前态 %q", event.FromState)
	}
	if event.ToState != "" && !knownCaseStatuses[CaseStatus(event.ToState)] {
		return fmt.Errorf("未知后态 %q", event.ToState)
	}
	if err := validateEventRequiredFields(event); err != nil {
		return err
	}
	if err := validateEventShortFields(event); err != nil {
		return err
	}
	if err := s.validateBusinessDeliveryFence(event); err != nil {
		return err
	}

	switch event.Type {
	case "approval_requested", "approval_granted", "approval_consumed",
		"approval_revoked", "approval_expired", "issue_prepared", "issue_sent",
		"message_prepared", "message_sent", "message_acked":
		if err := s.applyApprovalMessageEvent(event, cfg, actorRule); err != nil {
			return err
		}

	case "case_created":
		if !actorRule.CanCreate {
			return fmt.Errorf("actor %s 无 can_create 权限", event.Actor)
		}
		if _, exists := s.snapshot.Cases[event.CaseID]; exists {
			return fmt.Errorf("case 重复创建：%s", event.CaseID)
		}
		if event.Owner == "" {
			return fmt.Errorf("case_created 缺少 owner")
		}
		if _, ok := historicalRule(cfg, event.Owner); !ok {
			return fmt.Errorf("owner 未登记：%s", event.Owner)
		}
		if err := validateStateTransition(actionCreate, event.FromState, event.ToState); err != nil {
			return err
		}
		if event.ParentCaseID != "" {
			parent, err := s.currentCase(event.ParentCaseID)
			if err != nil {
				return fmt.Errorf("父 case：%w", err)
			}
			if parent.Status == string(statusClosed) {
				return fmt.Errorf("已关闭父 case 不得新增 child")
			}
			if parent.Status == string(statusEscalated) {
				return fmt.Errorf("父 case 正在等待上级核验 escalation；必须先 accept/return 当前 escalation，再拆分子 case")
			}
			if parent.Owner != event.Actor || event.Owner != event.Actor {
				return fmt.Errorf("子 case 必须由父 case 当前负责人创建并暂由其持有")
			}
			if event.Project != parent.Project {
				return fmt.Errorf("child case project 必须与 parent 一致：child=%s project=%q parent=%s project=%q", event.CaseID, event.Project, parent.ID, parent.Project)
			}
			expectedRoot := parent.RootCaseID
			if expectedRoot == "" {
				expectedRoot = parent.ID
			}
			if event.RootCaseID != expectedRoot {
				return fmt.Errorf("子 case root_case_id 不匹配")
			}
		} else {
			if event.RootCaseID != "" {
				return fmt.Errorf("root case 不得携带 root_case_id；只有 child 从 parent 继承 lineage")
			}
			if strings.TrimSpace(event.Project) == "" {
				return fmt.Errorf("HQ single-project invariant：root case 必须冻结非空 project")
			}
			if len(s.snapshot.Cases) != 0 {
				return fmt.Errorf("HQ single-project invariant：不允许第二个 root/project")
			}
		}
		if err := validateDigest("case_digest", event.CaseDigest); err != nil {
			return err
		}
		if expected := eventCaseSpecDigest(event); event.CaseDigest != expected {
			return fmt.Errorf("case_created case_digest 与事件最终规格不匹配：event=%s computed=%s", event.CaseDigest, expected)
		}

	case "case_revised":
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if state.Owner != event.Actor {
			return fmt.Errorf("只有 case 当前负责人可修订规格")
		}
		if state.Status == string(statusEscalated) {
			return fmt.Errorf("case 正在等待上级核验 escalation；必须先 accept/return，再修订规格")
		}
		if err := s.rejectActiveAssignment(event.CaseID, "修订规格"); err != nil {
			return err
		}
		basis := state.SpecEventID
		if basis == "" {
			basis = state.LastEventID
		}
		if event.RelatedEventID != basis || event.PreviousCaseDigest != state.Digest || event.CaseVersion != state.Version+1 {
			return fmt.Errorf("case_revised 必须追加引用当前版本与 digest")
		}
		if event.ParentCaseID != state.ParentCaseID || event.RootCaseID != state.RootCaseID {
			return fmt.Errorf("case revise 不得改变父子关系")
		}
		if event.Project != state.Project {
			return fmt.Errorf("case revise 不得改变已冻结的 project identity")
		}
		if err := s.validateCaseProjectLineage(event.CaseID, event.ParentCaseID, event.Project); err != nil {
			return fmt.Errorf("case_revised project lineage 无效：%w", err)
		}
		if err := validateDigest("case_digest", event.CaseDigest); err != nil {
			return err
		}
		if expected := eventCaseSpecDigest(event); event.CaseDigest != expected {
			return fmt.Errorf("case_revised case_digest 与事件最终规格不匹配：event=%s computed=%s", event.CaseDigest, expected)
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}

	case "case_escalation_prepared":
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		parent, err := s.currentCase(event.ParentCaseID)
		if err != nil {
			return fmt.Errorf("escalation 父 case：%w", err)
		}
		superior, superiorOK := cfg.exactRule(event.Recipient)
		if !cfg.isManager(actorRule) || !actorRule.CanCreate || actorRule.ReportsTo == "" ||
			event.Recipient != actorRule.ReportsTo || !superiorOK || !superior.CanAccept ||
			(!cfg.isManager(superior) && !superior.CanIssue) {
			return fmt.Errorf("case escalation 只允许经理把新子 case 固定上交 registry 直属上级")
		}
		if state.ParentCaseID != event.ParentCaseID || state.RootCaseID != event.RootCaseID ||
			parent.Owner != event.Actor || parent.Status == string(statusClosed) || state.Owner != event.Actor ||
			state.Status != string(statusOpen) || event.FromState != state.Status {
			return fmt.Errorf("case escalation 的 parent/owner/open 前态不匹配")
		}
		if err := s.rejectActiveAssignment(parent.ID, "发起上行 escalation"); err != nil {
			return err
		}
		created, createdOK := s.events[state.SpecEventID]
		if !createdOK || created.Type != "case_created" || created.Actor != event.Actor ||
			created.Sequence+1 != event.Sequence || created.CommandID != event.CommandID+":part:1" ||
			created.CommandDigest != event.CommandDigest {
			return fmt.Errorf("case escalation 必须与直属经理创建的新子 case 同一原子事务、紧邻配对")
		}
		if event.CaseVersion != state.Version || event.CaseDigest != state.Digest ||
			event.Title != state.Title || event.Project != state.Project || event.SourceRef != state.SourceRef {
			return fmt.Errorf("case escalation 未冻结新子 case 的完整规格合同")
		}
		if event.AssignmentEventID != "" || event.AssignmentID != "" || event.AuthorizationType != "" ||
			event.ApprovalID != "" || event.DecisionRef != "" {
			return fmt.Errorf("case escalation 不得伪装 assignment/approval/decision")
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		if effectiveEventDeliveryMode(event) != deliveryModeWakeup ||
			effectiveEventDeliveryTarget(event) != deliveryTargetNextTurn ||
			event.DeliveryReason != escalationDeliveryReason {
			return fmt.Errorf("case escalation 固定 wakeup/next-turn 与 manager-escalation reason")
		}
		if err := validateDeliveryPrimitives(event); err != nil {
			return err
		}

	case "case_escalation_sent":
		if err := s.applyDeliveryTerminal(event, deliverySent); err != nil {
			return err
		}
		prepared, ok := s.events[event.RelatedEventID]
		if !ok || prepared.Type != "case_escalation_prepared" || !sameCaseEscalationContract(prepared, event) || prepared.Note != event.Note {
			return fmt.Errorf("case_escalation_sent 与 prepared 合同不一致")
		}
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if state.Owner != prepared.Actor || event.FromState != state.Status ||
			event.ToState != string(statusEscalated) || event.Owner != prepared.Recipient {
			return fmt.Errorf("case_escalation_sent 的前态、后态或 owner 不匹配")
		}
		if err := validateStateTransition(actionEscalate, event.FromState, event.ToState); err != nil {
			return err
		}

	case "report_prepared":
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if _, ok := configRuleIncludingDisabled(cfg, event.Recipient); !ok {
			return fmt.Errorf("report recipient 未登记：%s", event.Recipient)
		}
		if event.FromState != state.Status {
			return fmt.Errorf("report_prepared 前态不匹配：事件=%q 当前=%q", event.FromState, state.Status)
		}
		target, action, err := reportTargetState(event.Result)
		if err != nil {
			return err
		}
		if err := validateStateTransition(action, state.Status, target); err != nil {
			return err
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		if effectiveEventDeliveryMode(event) != deliveryModeWakeup || effectiveEventDeliveryTarget(event) != deliveryTargetNextTurn {
			return fmt.Errorf("report 固定 wakeup/next-turn，不接受降档")
		}
		if err := validateDeliveryPrimitives(event); err != nil {
			return err
		}
		if event.AssignmentEventID != "" {
			if err := s.validateAssignment(event.AssignmentEventID, event.CaseID, event.Actor); err != nil {
				return err
			}
			assignment := s.assignments[event.AssignmentEventID]
			if !assignment.Accepted {
				return fmt.Errorf("assignment %s 尚未由 assignee accept，不可 report", assignment.AssignmentID)
			}
			if assignment.Status != "accepted" && assignment.Status != "rework" {
				return fmt.Errorf("assignment %s status=%s，不可创建新 submission", assignment.AssignmentID, assignment.Status)
			}
			if assignment.SubmissionEventID != "" {
				return fmt.Errorf("assignment %s 本轮已有 submission=%s；必须复用其 delivery 或等待 reviewer return", assignment.AssignmentID, assignment.SubmissionEventID)
			}
			if event.Recipient != assignment.Acceptor || !eventMatchesAssignmentState(event, assignment) ||
				event.CaseVersion != assignment.CaseVersion || event.CaseDigest != assignment.CaseDigest {
				return fmt.Errorf("report 未引用匹配的 assignment contract/acceptor")
			}
			assignment.SubmissionEventID = event.ID
		} else {
			if err := s.rejectOwnerReportOverActiveAssignment(event.CaseID); err != nil {
				return err
			}
			if state.Owner != event.Actor {
				return fmt.Errorf("actor %s 不是当前 owner，且未引用有效 assignment", event.Actor)
			}
			if actorRule.ReportsTo == "" || actorRule.ReportsTo != event.Recipient {
				return fmt.Errorf("无 assignment 的 report recipient 不符合 actor %s 的 reports_to", event.Actor)
			}
			generation := s.caseGeneration(event.CaseID)
			if generation == "" {
				return fmt.Errorf("case %s 缺少稳定 business generation", event.CaseID)
			}
			key := ownerSubmissionKey(event.CaseID, event.Actor, generation)
			if submitted := s.ownerSubmissions[key]; submitted != "" {
				return fmt.Errorf("case %s owner=%s 本轮已有 submission=%s；必须复用其 delivery 或等待业务状态推进", event.CaseID, event.Actor, submitted)
			}
			s.ownerSubmissions[key] = event.ID
		}

	case "report_sent":
		if err := s.applyDeliveryTerminal(event, deliverySent); err != nil {
			return err
		}
		prepared, ok := s.events[event.RelatedEventID]
		if !ok || prepared.Type != "report_prepared" || prepared.CaseID != event.CaseID {
			return fmt.Errorf("report_sent 缺少匹配的 report_prepared")
		}
		if prepared.Actor != event.Actor || prepared.Recipient != event.Recipient || prepared.Title != event.Title ||
			prepared.Project != event.Project || prepared.Result != event.Result || prepared.Severity != event.Severity ||
			prepared.SourceRef != event.SourceRef || prepared.ArtifactRef != event.ArtifactRef ||
			prepared.Location != event.Location || prepared.Verification != event.Verification || prepared.NextAction != event.NextAction {
			return fmt.Errorf("report_sent 的 actor/recipient/业务字段与 prepared 不一致")
		}
		if !sameAssignmentBinding(prepared, event) || prepared.CaseVersion != event.CaseVersion || prepared.CaseDigest != event.CaseDigest {
			return fmt.Errorf("report_sent 的 assignment/case contract 与 prepared 不一致")
		}
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		target, action, err := reportTargetState(event.Result)
		if err != nil {
			return err
		}
		if event.FromState != state.Status || event.ToState != target || event.Owner != event.Recipient {
			return fmt.Errorf("report_sent 的前态、后态或 owner 不匹配")
		}
		if err := validateStateTransition(action, event.FromState, event.ToState); err != nil {
			return err
		}
		if prepared.AssignmentEventID != "" {
			if err := s.validateAssignment(prepared.AssignmentEventID, event.CaseID, event.Actor); err != nil {
				return err
			}
			assignment := s.assignments[prepared.AssignmentEventID]
			if assignment.SubmissionEventID != prepared.ID {
				return fmt.Errorf("assignment %s report_sent 未匹配本轮冻结 submission", assignment.AssignmentID)
			}
			assignment.Status, assignment.StatusEventID = "submitted", event.ID
		}

	case "event_accepted", "event_returned":
		originalEvent, ok := s.events[event.RelatedEventID]
		original, semanticOK := semanticDeliveredEvent(originalEvent, s.events)
		if !ok || !semanticOK {
			return fmt.Errorf("%s 缺少匹配的已送达事件", event.Type)
		}
		if s.resolved[originalEvent.ID] {
			return fmt.Errorf("已送达事件重复处理：%s", originalEvent.ID)
		}
		if original.CaseID != event.CaseID || original.Recipient != event.Actor || !actorRule.CanAccept {
			return fmt.Errorf("actor/recipient/can_accept 不满足核验要求")
		}
		if event.Recipient != original.Actor {
			return fmt.Errorf("核验通知 recipient 必须回到原发件人 %s", original.Actor)
		}
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if event.FromState != state.Status {
			return fmt.Errorf("%s 前态不匹配：事件=%q 当前=%q", event.Type, event.FromState, state.Status)
		}
		if event.Type == "event_accepted" && event.AcceptanceDigest == "" {
			return fmt.Errorf("event_version=%d 的 event_accepted 必须携带 acceptance_digest", eventSchemaVersion)
		}
		if event.Type == "event_returned" && original.AssignmentID != "" {
			if event.SourceRef != original.SourceRef || event.ArtifactRef != original.ArtifactRef ||
				event.Verification != original.Verification || event.CaseVersion != original.CaseVersion ||
				event.CaseDigest != original.CaseDigest || !sameAssignmentBinding(event, original) {
				return fmt.Errorf("return receipt 未冻结匹配的 case/artifact/verification/assignment")
			}
		}
		if event.AcceptanceDigest != "" {
			if err := validateDigest("acceptance_digest", event.AcceptanceDigest); err != nil {
				return err
			}
			if event.SourceRef != original.SourceRef || event.ArtifactRef != original.ArtifactRef ||
				event.Verification != original.Verification || event.CaseVersion != original.CaseVersion ||
				event.CaseDigest != original.CaseDigest || !sameAssignmentBinding(event, original) {
				return fmt.Errorf("acceptance receipt 未冻结匹配的 case/artifact/verification/assignment")
			}
			if event.AcceptanceDigest != acceptanceReceiptDigest(originalEvent.ID, original, event) {
				return fmt.Errorf("acceptance_digest 与核验收据字段不匹配")
			}
		}
		var action transitionAction
		if event.Type == "event_returned" {
			if event.Owner != original.Actor {
				return fmt.Errorf("event_returned owner 必须回到原发件人")
			}
			if original.Type == "issue_sent" {
				action = actionReturnOrder
			} else if original.Type == "case_escalation_sent" {
				action = actionReturnEscalation
			} else {
				action = actionReturnReport
			}
		} else {
			if event.Owner != event.Actor {
				return fmt.Errorf("event_accepted owner 必须是核验人")
			}
			if original.Type == "issue_sent" {
				action = actionAcceptOrder
			} else if original.Type == "case_escalation_sent" {
				action = actionAcceptEscalation
			} else {
				switch original.Result {
				case "completed":
					action = actionAcceptReport
				case "finding":
					action = actionAcceptFinding
				case "blocked":
					action = actionAcceptBlocked
				case "needs-decision":
					action = actionAcceptNeedsDecision
				case "returned":
					action = actionAcceptReturnedReport
				default:
					return fmt.Errorf("report_sent result 非法：%s", original.Result)
				}
			}
		}
		if err := validateStateTransition(action, event.FromState, event.ToState); err != nil {
			return err
		}
		if event.Type == "event_accepted" {
			if err := s.acknowledgeDelivery(originalEvent, event); err != nil {
				return err
			}
		}
		s.resolved[originalEvent.ID] = true
		if assignment := s.assignments[originalEvent.ID]; assignment != nil {
			if event.Type == "event_accepted" {
				assignment.Accepted, assignment.Status, assignment.StatusEventID = true, "accepted", event.ID
			} else {
				assignment.Consumed, assignment.Status, assignment.StatusEventID = true, "returned", event.ID
			}
		}
		if original.Type == "report_sent" && original.AssignmentEventID != "" {
			assignment := s.assignments[original.AssignmentEventID]
			if assignment != nil {
				if event.Type == "event_accepted" {
					assignment.Consumed, assignment.Status, assignment.StatusEventID, assignment.SubmissionEventID = true, "completed", event.ID, ""
				} else {
					assignment.Accepted, assignment.Consumed, assignment.Status, assignment.StatusEventID = true, false, "rework", event.ID
					assignment.SubmissionGeneration, assignment.SubmissionEventID = event.ID, ""
				}
			}
		}

	case "case_closed":
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if !cfg.canCloseAsAccount(actorRule) {
			return fmt.Errorf("actor %s 无 can_close 权限", event.Actor)
		}
		if err := s.rejectActiveAssignment(event.CaseID, "关闭"); err != nil {
			return err
		}
		if blockers := s.unsettledClosureDeliveries(event.CaseID, true); len(blockers) != 0 {
			return fmt.Errorf("case_closed 不得隐藏 target/descendants 的未收敛 workflow delivery：%s；先运行：%s", renderClosureDeliveryBlockers(blockers), renderClosureDeliveryRecoveryCommands(blockers))
		}
		if descendants := s.unclosedDescendantsPostOrder(event.CaseID); len(descendants) != 0 {
			return fmt.Errorf("case_closed 不得跳过未关闭 descendants（post-order=%s）", renderDescendantStates(descendants))
		}
		if event.FromState != state.Status || event.Owner != "" {
			return fmt.Errorf("case_closed 的前态不匹配或 owner 未清空")
		}
		if err := validateStateTransition(actionClose, event.FromState, event.ToState); err != nil {
			return err
		}

	case "delivery_attempted":
		if err := s.applyDeliveryAttempt(event); err != nil {
			return err
		}

	case "delivery_failed_pre_send":
		if err := s.applyDeliveryTerminal(event, deliveryFailedPreSend); err != nil {
			return err
		}

	case "delivery_unknown":
		if err := s.applyDeliveryTerminal(event, deliveryUnknown); err != nil {
			return err
		}

	case "delivery_queued":
		if err := s.applyDeliveryQueued(event); err != nil {
			return err
		}

	case "delivery_context_claimed":
		if err := s.applyDeliveryContextClaim(event); err != nil {
			return err
		}

	case "delivery_budget_reset":
		if event.Actor != event.Recipient || event.DeliveryReason != "natural-wake" {
			return fmt.Errorf("delivery_budget_reset 必须由目标自然唤醒并重置自己")
		}
		if s.deliveryWakeSpends[event.Recipient] == 0 || s.deliveryLastWake[event.Recipient] != event.RelatedEventID {
			return fmt.Errorf("delivery_budget_reset 缺少匹配的连续唤醒证据")
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		s.deliveryWakeSpends[event.Recipient] = 0

	case "accept_notice_sent", "return_notice_sent":
		if err := s.applyDeliveryTerminal(event, deliverySent); err != nil {
			return err
		}
		origin, ok := s.events[event.RelatedEventID]
		expected := "event_accepted"
		if event.Type == "return_notice_sent" {
			expected = "event_returned"
		}
		if !ok || origin.Type != expected {
			return fmt.Errorf("%s 缺少匹配的业务事件", event.Type)
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}

	case "delivery_resolved_not_sent":
		if _, err := s.applyDeliveryResolution(event, false, actorRule); err != nil {
			return err
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}

	case "delivery_resolved_sent":
		record, err := s.applyDeliveryResolution(event, true, actorRule)
		if err != nil {
			return err
		}
		if err := s.validateResolvedDeliveryState(event, record.Origin, cfg); err != nil {
			return err
		}

	case "assignment_activation_attempted", "assignment_activation_sent",
		"assignment_activation_failed_pre_send", "assignment_activation_unknown",
		"assignment_activation_resolved_sent", "assignment_activation_resolved_not_sent",
		"assignment_activation_exhausted":
		if err := s.applyAssignmentActivationEvent(event, actorRule); err != nil {
			return err
		}

	case "nudge_enqueued", "nudge_claimed", "nudge_claim_released", "nudge_delivery_attempted",
		"nudge_delivered", "nudge_failed", "nudge_unknown", "nudge_expired",
		"nudge_reconciled_delivered", "nudge_reconciled_not_run", "nudge_cancelled",
		"reminder_created", "reminder_resolved":
		if err := s.applyNudgeEvent(event, cfg); err != nil {
			return err
		}

	case "estop_activated", "estop_agent_planned", "estop_agent_frozen", "estop_agent_freeze_ambiguous", "estop_agent_freeze_failed",
		"estop_agent_freeze_retry", "estop_agent_restored", "estop_agent_restore_ambiguous", "estop_agent_restore_failed",
		"estop_agent_restore_retry", "estop_released":
		if err := s.applyEstopEvent(event, cfg); err != nil {
			return err
		}

	default:
		return fmt.Errorf("未知事件类型 %q", event.Type)
	}

	if isDeliveryOriginType(event.Type) {
		if err := s.registerDeliveryOrigin(event); err != nil {
			return err
		}
		if isBusinessDeliveryOrigin(event.Type) {
			s.businessDeliveryRounds[event.DeliveryID] = s.caseGeneration(event.CaseID)
		}
	}
	s.events[event.ID] = event
	s.commands[event.CommandID] = event
	applyEvent(&s.snapshot, event)
	if event.Type == "case_created" || event.Type == "case_revised" || event.ToState != "" {
		s.caseGenerations[event.CaseID] = event.ID
	}
	if event.Type == "case_closed" {
		s.snapshot.Cases[event.CaseID].Owner = ""
	}
	return nil
}
