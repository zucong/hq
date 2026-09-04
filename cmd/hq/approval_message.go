package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const standingHeaderMarker = "<!-- hq-standing-authorization:v1\n"

type ApprovalView struct {
	ApprovalID        string `json:"approval_id"`
	Status            string `json:"status"`
	CaseID            string `json:"case_id"`
	Action            string `json:"action"`
	Target            string `json:"target"`
	CaseVersion       int    `json:"case_version"`
	CaseDigest        string `json:"case_digest"`
	CaseGeneration    string `json:"case_generation"`
	TargetSeatVersion int    `json:"target_seat_version"`
	TargetSeatDigest  string `json:"target_seat_digest"`
	CaseTitle         string `json:"case_title"`
	ExpiresAt         string `json:"expires_at"`
	Mode              string `json:"mode"`
	Issuer            string `json:"issuer,omitempty"`
	CapturedBy        string `json:"captured_by,omitempty"`
	ConsumedBy        string `json:"consumed_by_event,omitempty"`
}

type standingAuthorizationMetadata struct {
	Version     int             `json:"version"`
	DecisionID  string          `json:"decision_id"`
	Status      string          `json:"status"`
	ConfirmedBy string          `json:"confirmed_by"`
	ConfirmedAt string          `json:"confirmed_at"`
	Scopes      []standingScope `json:"scopes"`
}

type standingScope struct {
	Action     string `json:"action"`
	Actor      string `json:"actor"`
	Target     string `json:"target"`
	CasePrefix string `json:"case_prefix,omitempty"`
}

type standingAuthorizationAudit struct {
	DecisionRef  string                        `json:"decision_ref"`
	Metadata     standingAuthorizationMetadata `json:"metadata"`
	MatchedScope standingScope                 `json:"matched_scope"`
	RequestScope standingScope                 `json:"request_scope"`
}

type issueAuthorization struct {
	Kind       string
	Ref        string
	ID         string
	Issuer     string
	CapturedBy string
	Digest     string
	Consume    *Event
}

type batchEventStore interface {
	TransactBatch(Config, string, string, bool, BatchTransactionBuilder) (BatchTransactionResult, error)
}

func isApprovalMessageProjectionNeutralEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "approval_") ||
		strings.HasPrefix(eventType, "message_") || eventType == "delivery_attempted" ||
		eventType == "delivery_failed_pre_send" || eventType == "delivery_unknown" ||
		eventType == "delivery_queued" || eventType == "delivery_context_claimed" ||
		eventType == "delivery_budget_reset" || eventType == "accept_notice_sent" || eventType == "return_notice_sent"
}

func sameApprovalScope(left, right Event) bool {
	return left.ApprovalID == right.ApprovalID && left.CaseID == right.CaseID &&
		left.ApprovalAction == right.ApprovalAction && left.Recipient == right.Recipient &&
		left.CaseVersion == right.CaseVersion && left.CaseDigest == right.CaseDigest &&
		left.BasisEventID == right.BasisEventID &&
		left.AssigneeSeatVersion == right.AssigneeSeatVersion && left.AssigneeSeatDigest == right.AssigneeSeatDigest &&
		left.Title == right.Title && left.ExpiresAt == right.ExpiresAt &&
		left.ApprovalMode == right.ApprovalMode
}

func approvalView(record *approvalLedgerRecord) ApprovalView {
	view := ApprovalView{
		ApprovalID: record.Request.ApprovalID, Status: record.Status, CaseID: record.Request.CaseID,
		Action: record.Request.ApprovalAction, Target: record.Request.Recipient,
		CaseVersion: record.Request.CaseVersion, CaseDigest: record.Request.CaseDigest,
		CaseGeneration:    record.Request.BasisEventID,
		TargetSeatVersion: record.Request.AssigneeSeatVersion, TargetSeatDigest: record.Request.AssigneeSeatDigest,
		CaseTitle: record.Request.Title, ExpiresAt: record.Request.ExpiresAt,
		Mode: record.Request.ApprovalMode, Issuer: record.Grant.Issuer, CapturedBy: record.Grant.CapturedBy,
	}
	if record.Status == "consumed" {
		view.ConsumedBy = record.Terminal.RelatedEventID
	}
	return view
}

func approvalRefreshCommand(request Event) string {
	return fmt.Sprintf("hq approval request --id NEW-APPROVAL-ID --case %s --action issue --target %s --expires FUTURE-RFC3339 --mode one_time", request.CaseID, request.Recipient)
}

func staleApprovalError(request Event, witness AgentRule, detail string) error {
	return conflictf("approval=%s %s。不要重试旧 grant；也不要重试旧 issue/approval；请生成未使用的新 approval_id，并针对当前 case generation 与 target seat 执行：%s（将 FUTURE-RFC3339 替换为未来时间）；总裁秘书职责位 %s agent=%s",
		request.ApprovalID, detail, approvalRefreshCommand(request), roleApprovalWitness, witness.Name)
}

func validateApprovalCaseSnapshotFreshness(ledger *ledgerState, request Event, witness AgentRule) error {
	state, err := ledger.currentCase(request.CaseID)
	if err != nil {
		return err
	}
	if state.Owner != witness.Name {
		return staleApprovalError(request, witness, fmt.Sprintf("已失效：case=%s 当前 owner=%s，不是总裁秘书", request.CaseID, state.Owner))
	}
	if state.Version != request.CaseVersion || state.Digest != request.CaseDigest {
		return staleApprovalError(request, witness, fmt.Sprintf("已过期于 case 规格；request=%s@v%d digest=%s，current=%s@v%d digest=%s", request.CaseID, request.CaseVersion, request.CaseDigest, state.ID, state.Version, state.Digest))
	}
	currentGeneration := ledger.caseGeneration(request.CaseID)
	if currentGeneration == "" || currentGeneration != request.BasisEventID {
		return staleApprovalError(request, witness, fmt.Sprintf("已过期于 case generation（防 ABA）；request_generation=%s current_generation=%s，case version/digest 即使相同也不可重放", request.BasisEventID, currentGeneration))
	}
	return nil
}

func validateApprovalTargetSnapshotFreshness(cfg Config, request Event, witness AgentRule) error {
	target, ok := cfg.exactRule(request.Recipient)
	if !ok || !target.CanReceiveOrder || !target.CanAccept || !cfg.isManager(target) || target.ReportsTo != witness.Name {
		return staleApprovalError(request, witness, fmt.Sprintf("target=%s 已停用、已失去部门经理/接令资格或不再直属总裁秘书", request.Recipient))
	}
	if target.SeatVersion != request.AssigneeSeatVersion || target.SeatDigest != request.AssigneeSeatDigest {
		return staleApprovalError(request, witness, fmt.Sprintf("已过期于 target seat；request=%s@seat-%d/%s current=%s@seat-%d/%s", request.Recipient, request.AssigneeSeatVersion, request.AssigneeSeatDigest, target.Name, target.SeatVersion, target.SeatDigest))
	}
	return nil
}

func validateApprovalSnapshotFreshness(ledger *ledgerState, cfg Config, request Event, witness AgentRule) error {
	if err := validateApprovalCaseSnapshotFreshness(ledger, request, witness); err != nil {
		return err
	}
	return validateApprovalTargetSnapshotFreshness(cfg, request, witness)
}

func approvalIssueAuthorizationDigest(request Event) string {
	return requestDigest("approval-case-v2", request.ApprovalID, request.CaseID, request.Recipient,
		strconv.Itoa(request.CaseVersion), request.CaseDigest, request.BasisEventID,
		strconv.Itoa(request.AssigneeSeatVersion), request.AssigneeSeatDigest)
}

func (s *ledgerState) validateApprovalFinalInvariants() error {
	approvalIDs := make([]string, 0, len(s.approvals))
	for id := range s.approvals {
		approvalIDs = append(approvalIDs, id)
	}
	sort.Strings(approvalIDs)
	for _, id := range approvalIDs {
		record := s.approvals[id]
		if record == nil || record.Status != "consumed" {
			continue
		}
		consume := record.Terminal
		issue, ok := s.events[consume.RelatedEventID]
		if !ok || issue.Type != "issue_prepared" || issue.AuthorizationType != "approval" || issue.ApprovalID != id {
			return fmt.Errorf("approval=%s 的 consumed 事件缺少匹配的 issue_prepared", id)
		}
		if issue.Sequence != consume.Sequence+1 || consume.CommandID != issue.CommandID+":part:1" || consume.CommandDigest != issue.CommandDigest {
			return fmt.Errorf("approval=%s 的 consumed 必须与紧邻、同原子 command/digest 的 issue_prepared 配对", id)
		}
		if issue.Actor != consume.Actor || issue.CaseID != record.Request.CaseID || issue.Recipient != record.Request.Recipient ||
			issue.CaseVersion != record.Request.CaseVersion || issue.CaseDigest != record.Request.CaseDigest ||
			issue.AssigneeSeatVersion != record.Request.AssigneeSeatVersion || issue.AssigneeSeatDigest != record.Request.AssigneeSeatDigest ||
			issue.AuthorizationDigest != approvalIssueAuthorizationDigest(record.Request) {
			return fmt.Errorf("approval=%s 的 consumed/issue scope、generation/seat authorization binding 不一致", id)
		}
	}
	return nil
}

func (s *ledgerState) validateLedgerFinalInvariants(cfg Config) error {
	if err := s.validateCaseTreeFinalInvariants(); err != nil {
		return err
	}
	if err := s.validateTurnBundleFinalInvariants(); err != nil {
		return err
	}
	if err := s.validateApprovalFinalInvariants(); err != nil {
		return err
	}
	if err := s.validateAssignmentRevisionFinalInvariant(); err != nil {
		return err
	}
	return s.validateCandidateSeatContinuity(cfg)
}

func (a *App) ledgerState() (*ledgerState, error) {
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return nil, err
	}
	return validateLedger(events, a.Config)
}

func (a *App) transactBatch(commandID, digest string, build BatchTransactionBuilder) (BatchTransactionResult, error) {
	store, ok := a.Store.(batchEventStore)
	if !ok {
		return BatchTransactionResult{}, fmt.Errorf("Store 不支持原子多事件事务，fail-closed")
	}
	return store.TransactBatch(a.Config, commandID, digest, a.DryRun, build)
}

func readStandingAuthorization(value, office, ownerPrincipal string, scope standingScope) (string, string, error) {
	clean, err := normalizeRef(value, filepath.Dir(office), true)
	if err != nil {
		return "", "", err
	}
	path := strings.Split(clean, "#")[0]
	decisions := filepath.Join(office, "decisions")
	if !pathWithin(path, decisions) || strings.Contains(clean, "#") {
		return "", "", fmt.Errorf("standing decision 必须是 decisions/ 内 canonical 非 fragment 文件")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	if !bytes.HasPrefix(raw, []byte(standingHeaderMarker)) {
		return "", "", fmt.Errorf("decision 缺少 hq-standing-authorization:v1 机器头")
	}
	block, err := parseMetadataHeader(raw, standingHeaderMarker, "hq-standing-authorization")
	if err != nil {
		return "", "", err
	}
	var metadata standingAuthorizationMetadata
	if err := decodeStrictJSON(block, &metadata); err != nil {
		return "", "", err
	}
	if err := requireJSONFields(block, "version", "decision_id", "status", "confirmed_by", "confirmed_at", "scopes"); err != nil {
		return "", "", err
	}
	if metadata.Version != 1 || metadata.Status != "effective" || !decisionIDPattern.MatchString(metadata.DecisionID) {
		return "", "", fmt.Errorf("standing decision 必须 version=1、status=effective 且 decision_id 合法")
	}
	if metadata.ConfirmedBy != ownerPrincipal {
		return "", "", fmt.Errorf("standing decision confirmed_by 必须精确匹配 owner_principal %q", ownerPrincipal)
	}
	if _, err := time.Parse(time.RFC3339, metadata.ConfirmedAt); err != nil {
		return "", "", fmt.Errorf("standing decision confirmed_at 必须是 RFC3339：%w", err)
	}
	if len(metadata.Scopes) == 0 {
		return "", "", fmt.Errorf("standing decision scopes 至少包含一项")
	}
	seen := map[standingScope]bool{}
	for _, candidate := range metadata.Scopes {
		if err := validateStandingScopeShape(candidate); err != nil {
			return "", "", err
		}
		if seen[candidate] {
			return "", "", fmt.Errorf("standing decision 包含重复 scope")
		}
		seen[candidate] = true
	}
	var matched *standingScope
	for _, candidate := range metadata.Scopes {
		if candidate.Action != scope.Action || candidate.Actor != scope.Actor || candidate.Target != scope.Target {
			continue
		}
		if candidate.CasePrefix != "" && !strings.HasPrefix(scope.CasePrefix, candidate.CasePrefix) {
			continue
		}
		matchedCandidate := candidate
		matched = &matchedCandidate
		break
	}
	if matched == nil {
		return "", "", fmt.Errorf("standing decision scope 不匹配")
	}
	digest := standingAuthorizationDigest(clean, metadata, *matched, scope)
	return clean, digest, nil
}

func validateStandingScopeShape(scope standingScope) error {
	if scope.Action != "issue" || !agentNamePattern.MatchString(scope.Actor) || !agentNamePattern.MatchString(scope.Target) {
		return fmt.Errorf("standing scope 必须包含 action=issue 与合法 actor/target")
	}
	if scope.CasePrefix == "" {
		return fmt.Errorf("standing scope 过宽：必须绑定 case_prefix")
	}
	for _, value := range []string{scope.CasePrefix} {
		if strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n") || utf8.RuneCountInString(value) > 200 {
			return fmt.Errorf("standing scope prefix 必须是至多 200 rune 的非空白单行文本")
		}
	}
	return nil
}

func standingAuthorizationDigest(decisionRef string, metadata standingAuthorizationMetadata, matched, requested standingScope) string {
	return canonicalJSONDigest(standingAuthorizationAudit{
		DecisionRef: decisionRef, Metadata: metadata, MatchedScope: matched, RequestScope: requested,
	})
}

func validateApprovalMessageRequiredFields(event Event) (bool, error) {
	require := func(fields ...struct{ name, value string }) error { return requireEventFields(event, fields...) }
	switch event.Type {
	case "approval_requested", "approval_granted", "approval_consumed", "approval_revoked", "approval_expired":
		if err := require(eventField("approval_id", event.ApprovalID), eventField("approval_action", event.ApprovalAction),
			eventField("approval_status", event.ApprovalStatus), eventField("approval_mode", event.ApprovalMode),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("basis_event_id", event.BasisEventID), eventField("assignee_seat_digest", event.AssigneeSeatDigest),
			eventField("title", event.Title),
			eventField("expires_at", event.ExpiresAt)); err != nil {
			return true, err
		}
		if err := require(eventField("case_digest", event.CaseDigest)); err != nil {
			return true, err
		}
		if event.CaseVersion <= 0 {
			return true, fmt.Errorf("事件 %s 缺少合法 case_version", event.Type)
		}
		if event.AssigneeSeatVersion <= 0 {
			return true, fmt.Errorf("事件 %s 缺少合法 target seat_version", event.Type)
		}
		if event.Type != "approval_requested" {
			if err := require(eventField("related_event_id", event.RelatedEventID)); err != nil {
				return true, err
			}
		}
		if event.Type == "approval_granted" {
			return true, require(eventField("issuer", event.Issuer), eventField("captured_by", event.CapturedBy))
		}
		if event.Type == "approval_revoked" {
			return true, require(eventField("note", event.Note))
		}
		return true, nil
	case "issue_prepared":
		if err := require(eventField("from_state", event.FromState), eventField("recipient", event.Recipient),
			eventField("recipient_label", event.RecipientLabel), eventField("authorization_type", event.AuthorizationType),
			eventField("authorization_digest", event.AuthorizationDigest), eventField("next_action", event.NextAction),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest)); err != nil {
			return true, err
		}
		if err := require(eventField("case_digest", event.CaseDigest)); err != nil {
			return true, err
		}
		if event.CaseVersion <= 0 {
			return true, fmt.Errorf("issue_prepared 缺少合法 case_version")
		}
		if event.AssignmentID != "" {
			if err := require(eventField("assignment_digest", event.AssignmentDigest), eventField("assignment_issuer", event.AssignmentIssuer),
				eventField("assignee_seat_digest", event.AssigneeSeatDigest), eventField("role_card_id", event.RoleCardID),
				eventField("role_card_digest", event.RoleCardDigest), eventField("role_card_manual_path", event.RoleCardManualPath),
				eventField("reviewer", event.Reviewer), eventField("reviewer_label", event.ReviewerLabel),
				eventField("acceptor", event.Acceptor), eventField("acceptor_label", event.AcceptorLabel)); err != nil {
				return true, err
			}
			if event.AssigneeSeatVersion < 1 || event.RoleCardVersion < 1 {
				return true, fmt.Errorf("issue_prepared 缺少合法 assignee_seat_version/role_card_version")
			}
		}
		return true, nil
	case "issue_sent":
		if err := require(eventField("related_event_id", event.RelatedEventID), eventField("attempt_event_id", event.AttemptEventID),
			eventField("from_state", event.FromState), eventField("to_state", event.ToState), eventField("owner", event.Owner),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("authorization_type", event.AuthorizationType), eventField("authorization_digest", event.AuthorizationDigest),
			eventField("delivery", event.Delivery), eventField("delivery_id", event.DeliveryID), eventField("payload_digest", event.PayloadDigest)); err != nil {
			return true, err
		}
		if err := require(eventField("case_digest", event.CaseDigest)); err != nil {
			return true, err
		}
		if event.CaseVersion <= 0 {
			return true, fmt.Errorf("issue_sent 缺少合法 case_version")
		}
		if event.AssignmentID != "" {
			if err := require(eventField("assignment_digest", event.AssignmentDigest), eventField("assignment_issuer", event.AssignmentIssuer),
				eventField("assignee_seat_digest", event.AssigneeSeatDigest), eventField("role_card_id", event.RoleCardID),
				eventField("role_card_digest", event.RoleCardDigest), eventField("role_card_manual_path", event.RoleCardManualPath),
				eventField("reviewer", event.Reviewer), eventField("reviewer_label", event.ReviewerLabel),
				eventField("acceptor", event.Acceptor), eventField("acceptor_label", event.AcceptorLabel)); err != nil {
				return true, err
			}
			if event.AssigneeSeatVersion < 1 || event.RoleCardVersion < 1 {
				return true, fmt.Errorf("issue_sent 缺少合法 assignee_seat_version/role_card_version")
			}
		}
		return true, nil
	case "message_prepared":
		return true, require(eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("message_kind", event.MessageKind), eventField("message", event.Message),
			eventField("thread_id", event.ThreadID), eventField("delivery", event.Delivery),
			eventField("delivery_id", event.DeliveryID), eventField("payload_digest", event.PayloadDigest))
	case "message_sent":
		return true, require(eventField("related_event_id", event.RelatedEventID), eventField("attempt_event_id", event.AttemptEventID),
			eventField("recipient", event.Recipient), eventField("recipient_label", event.RecipientLabel),
			eventField("message_kind", event.MessageKind), eventField("message", event.Message),
			eventField("thread_id", event.ThreadID), eventField("delivery", event.Delivery),
			eventField("delivery_id", event.DeliveryID), eventField("payload_digest", event.PayloadDigest))
	case "message_acked":
		return true, require(eventField("related_event_id", event.RelatedEventID), eventField("delivery_id", event.DeliveryID),
			eventField("payload_digest", event.PayloadDigest), eventField("message_kind", event.MessageKind),
			eventField("thread_id", event.ThreadID))
	default:
		return false, nil
	}
}

func isApprovalMessageBusinessEvent(eventType string) bool {
	return strings.HasPrefix(eventType, "approval_") ||
		strings.HasPrefix(eventType, "issue_") || strings.HasPrefix(eventType, "message_")
}

func (s *ledgerState) applyApprovalMessageEvent(event Event, cfg Config, actorRule AgentRule) error {
	switch event.Type {
	case "approval_requested":
		witness, err := cfg.approvalWitness()
		if err != nil {
			return err
		}
		if !actorRule.CanIssue || cfg.isManager(actorRule) || event.Actor != witness.Name || actorRule.Name != witness.Name {
			return fmt.Errorf("approval_requested 必须由总裁秘书（职责位 %s，agent=%s）记录；actor=%s", roleApprovalWitness, witness.Name, event.Actor)
		}
		if _, exists := s.approvals[event.ApprovalID]; exists {
			return fmt.Errorf("approval 已存在：%s", event.ApprovalID)
		}
		if event.ApprovalStatus != "requested" || event.ApprovalAction != "issue" || event.ApprovalMode != "one_time" {
			return fmt.Errorf("approval request 状态/action/mode 非法")
		}
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if state.Owner != witness.Name {
			return fmt.Errorf("approval_requested 要求当时 case owner 为总裁秘书（职责位 %s，agent=%s）；case=%s owner=%s", roleApprovalWitness, witness.Name, event.CaseID, state.Owner)
		}
		if state.Version != event.CaseVersion || state.Digest != event.CaseDigest {
			return fmt.Errorf("approval request 未精确绑定当前 case version/digest")
		}
		if err := validateLedgerID("approval case generation", event.BasisEventID); err != nil {
			return err
		}
		if generation := s.caseGeneration(event.CaseID); generation == "" || generation != event.BasisEventID {
			return fmt.Errorf("approval request 未精确绑定当前 case generation：event=%s current=%s", event.BasisEventID, generation)
		}
		if event.AssigneeSeatVersion < 1 {
			return fmt.Errorf("approval request 缺少合法 target seat version")
		}
		if err := validateDigest("approval target seat digest", event.AssigneeSeatDigest); err != nil {
			return err
		}
		target, ok := cfg.exactRule(event.Recipient)
		if !ok || !target.CanReceiveOrder || !target.CanAccept {
			return fmt.Errorf("approval target 未登记、已停用或缺少 can_receive_order/can_accept：%s", event.Recipient)
		}
		if target.ReportsTo != witness.Name {
			return fmt.Errorf("approval_requested 只能面向总裁秘书（职责位 %s，agent=%s）的直属部门经理；target=%s target_reports_to=%s。应先为该 case 创建 target=%s（直属经理，不是 specialist %s）的授权并 issue 给其，由经理拆分子 case 后直接 issue 给 %s", roleApprovalWitness, witness.Name, target.Name, target.ReportsTo, target.ReportsTo, target.Name, target.Name)
		}
		if !cfg.isManager(target) {
			return fmt.Errorf("approval target 必须是登记了 manager:<department> 职责位的直属部门经理：target=%s；总裁秘书 agent=%s", event.Recipient, witness.Name)
		}
		// A completed approval may legitimately predate a later seat incarnation,
		// so historical replay accepts a strictly newer current seat. Reusing the
		// same seat version with a different digest, or moving backwards, is never
		// a legal registry evolution. Active request/grant equality is enforced by
		// the ledger-tail continuity invariant.
		if target.SeatVersion < event.AssigneeSeatVersion ||
			(target.SeatVersion == event.AssigneeSeatVersion && target.SeatDigest != event.AssigneeSeatDigest) {
			return fmt.Errorf("approval request 的 target seat snapshot 与 registry 历史连续性不匹配：request=%d/%s current=%d/%s", event.AssigneeSeatVersion, event.AssigneeSeatDigest, target.SeatVersion, target.SeatDigest)
		}
		expires, err := time.Parse(time.RFC3339, event.ExpiresAt)
		if err != nil || !expires.After(mustEventTime(event)) {
			return fmt.Errorf("approval expires_at 必须晚于 request 时间")
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		s.approvals[event.ApprovalID] = &approvalLedgerRecord{Request: event, Status: "requested"}
		return nil

	case "approval_granted":
		record := s.approvals[event.ApprovalID]
		if record == nil || record.Status != "requested" || event.RelatedEventID != record.Request.ID || !sameApprovalScope(event, record.Request) {
			return fmt.Errorf("approval_granted 缺少匹配的 requested scope")
		}
		witness, err := cfg.approvalWitness()
		if err != nil {
			return err
		}
		if !actorRule.CanIssue || cfg.isManager(actorRule) || event.Actor != witness.Name || event.CapturedBy != witness.Name || event.Issuer != cfg.ownerPrincipal() || event.ApprovalStatus != "granted" {
			return fmt.Errorf("approval grant 必须由当前 workspace 的 %s 见证，issuer 为合法公司所有者 principal，captured_by 为真实见证人", roleApprovalWitness)
		}
		if err := validateApprovalCaseSnapshotFreshness(s, record.Request, witness); err != nil {
			return err
		}
		if !mustEventTime(event).Before(mustParseTime(event.ExpiresAt)) {
			return fmt.Errorf("过期 approval 不可 grant")
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		record.Grant, record.Status = event, "granted"
		return nil

	case "approval_consumed":
		record := s.approvals[event.ApprovalID]
		if record == nil || record.Status != "granted" || record.Request.ApprovalMode != "one_time" || !sameApprovalScope(event, record.Request) {
			return fmt.Errorf("approval_consumed 缺少可消费的一次性 grant")
		}
		if event.Actor != record.Grant.CapturedBy || event.ApprovalStatus != "consumed" || !mustEventTime(event).Before(mustParseTime(event.ExpiresAt)) {
			return fmt.Errorf("approval 消费者、状态或有效期不匹配")
		}
		if err := validateLedgerID("consuming issue event", event.RelatedEventID); err != nil {
			return err
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		record.Terminal, record.Status = event, "consumed"
		return nil

	case "approval_revoked", "approval_expired":
		record := s.approvals[event.ApprovalID]
		if record == nil || (record.Status != "requested" && record.Status != "granted") || !sameApprovalScope(event, record.Request) {
			return fmt.Errorf("%s 缺少匹配的 requested/granted scope", event.Type)
		}
		expectedRelated := record.Request.ID
		if record.Status == "granted" {
			expectedRelated = record.Grant.ID
		}
		if event.RelatedEventID != expectedRelated {
			return fmt.Errorf("%s related_event_id 未匹配当前 approval 状态", event.Type)
		}
		witness, witnessErr := cfg.approvalWitness()
		if witnessErr != nil {
			return witnessErr
		}
		if event.Actor != witness.Name && !actorRule.CanManageStaff {
			return fmt.Errorf("无权终结 approval")
		}
		expected := strings.TrimPrefix(event.Type, "approval_")
		if event.ApprovalStatus != expected {
			return fmt.Errorf("approval 终态字段不匹配")
		}
		if event.Type == "approval_expired" && mustEventTime(event).Before(mustParseTime(event.ExpiresAt)) {
			return fmt.Errorf("approval 尚未到期")
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		record.Terminal, record.Status = event, expected
		return nil

	case "issue_prepared":
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if err := s.rejectActiveAssignment(event.CaseID, "创建第二份 issue/assignment"); err != nil {
			return err
		}
		for _, assignment := range s.assignments {
			if assignment != nil && assignment.AssignmentID == event.AssignmentID {
				return fmt.Errorf("assignment_id 重复：%s", event.AssignmentID)
			}
		}
		for _, delivery := range s.deliveries {
			if delivery != nil && delivery.Origin.Type == "issue_prepared" && delivery.Origin.AssignmentID == event.AssignmentID {
				return fmt.Errorf("assignment_id 重复：%s", event.AssignmentID)
			}
		}
		target, ok := cfg.exactRule(event.Recipient)
		if !ok || !target.CanReceiveOrder || !target.CanAccept {
			return fmt.Errorf("issue target 未登记、已停用或缺少 can_receive_order/can_accept")
		}
		if target.ReportsTo != event.Actor {
			return fmt.Errorf("issue 只允许精确纵向一层 target.ReportsTo==actor.Name；actor=%s target=%s target_reports_to=%s", event.Actor, target.Name, target.ReportsTo)
		}
		if state.Version != event.CaseVersion || state.Digest != event.CaseDigest {
			return fmt.Errorf("issue 未引用当前 case version/digest")
		}
		if event.Project != state.Project {
			return fmt.Errorf("assignment project 必须冻结当前 case.project")
		}
		if state.Owner != event.Actor {
			return fmt.Errorf("只有 case 当前负责人可以委派")
		}
		if state.Status != event.FromState {
			return fmt.Errorf("issue_prepared 前态不匹配")
		}
		if event.AuthorizationType != "revision" && (event.SupersedesAssignmentEventID != "" || event.SupersedesAssignmentID != "" || event.BasisEventID != "") {
			return fmt.Errorf("普通 issue 不得携带 supersede/revision binding")
		}
		if err := validateStateTransition(actionIssue, state.Status, string(statusDispatched)); err != nil {
			return err
		}
		switch event.AuthorizationType {
		case "manager":
			if actorRule.hasResponsibility(roleApprovalWitness) || !cfg.isManager(actorRule) || target.ReportsTo != event.Actor || event.AuthorizationDigest != requestDigest("manager", event.Actor, event.Recipient) {
				return fmt.Errorf("manager issue 只允许精确直属下属")
			}
		case "approval":
			witness, witnessErr := cfg.approvalWitness()
			if witnessErr != nil {
				return witnessErr
			}
			record := s.approvals[event.ApprovalID]
			if !actorRule.CanIssue || event.Actor != witness.Name || record == nil || record.Grant.Issuer != cfg.ownerPrincipal() || record.Grant.CapturedBy != witness.Name ||
				record.Request.CaseID != event.CaseID || record.Request.Recipient != event.Recipient ||
				record.Request.CaseVersion != event.CaseVersion || record.Request.CaseDigest != event.CaseDigest || event.Issuer != record.Grant.Issuer || event.CapturedBy != event.Actor {
				return fmt.Errorf("issue approval scope/issuer/captured_by 不匹配")
			}
			if record.Request.ApprovalMode != "one_time" || record.Status != "consumed" || record.Terminal.RelatedEventID != event.ID {
				return fmt.Errorf("one_time approval 必须与 issue intent 同事务消费")
			}
			if !mustEventTime(event).Before(mustParseTime(record.Request.ExpiresAt)) {
				return fmt.Errorf("approval 已过期")
			}
			if err := validateApprovalCaseSnapshotFreshness(s, record.Request, witness); err != nil {
				return err
			}
			if event.AssigneeSeatVersion != record.Request.AssigneeSeatVersion || event.AssigneeSeatDigest != record.Request.AssigneeSeatDigest {
				return staleApprovalError(record.Request, witness, fmt.Sprintf("issue assignment seat=%d/%s 未匹配 approval 冻结 target seat=%d/%s", event.AssigneeSeatVersion, event.AssigneeSeatDigest, record.Request.AssigneeSeatVersion, record.Request.AssigneeSeatDigest))
			}
			expectedDigest := approvalIssueAuthorizationDigest(record.Request)
			if event.AuthorizationDigest != expectedDigest {
				return fmt.Errorf("approval authorization digest 不匹配")
			}
		case "standing_decision":
			if !actorRule.CanIssue || event.DecisionRef == "" {
				return fmt.Errorf("standing decision issue 缺少权限或 decision_ref")
			}
			if err := validateDigest("authorization_digest", event.AuthorizationDigest); err != nil {
				return err
			}
		case "revision":
			pending := s.pendingAssignmentRevisions[event.CaseID]
			if pending == nil || pending.Revised.ID == "" || event.BasisEventID != pending.Revised.ID {
				return fmt.Errorf("revision issue 必须紧邻同一原子 supersede + case_revised")
			}
			superseded := pending.Superseded
			old := s.assignments[superseded.AssignmentEventID]
			if old == nil || !old.Consumed || old.Status != "superseded" || old.SupersededByAssignmentID != event.AssignmentID ||
				event.SupersedesAssignmentEventID != old.EventID || event.SupersedesAssignmentID != old.AssignmentID ||
				event.Recipient != old.Recipient || event.Actor != old.Issuer || event.Reviewer != old.Reviewer || event.Acceptor != old.Acceptor ||
				superseded.ReplacementAssignmentID != event.AssignmentID {
				return fmt.Errorf("revision issue 未匹配被 supersede 的冻结 assignment")
			}
			expectedAuthorization := assignmentRevisionAuthorizationDigest(old, event.AssignmentID, event.CaseVersion, event.CaseDigest)
			if event.AuthorizationDigest != expectedAuthorization {
				return fmt.Errorf("revision authorization digest 不匹配")
			}
		default:
			return fmt.Errorf("未知 issue authorization_type %q", event.AuthorizationType)
		}
		if cfg.isManager(actorRule) && event.AuthorizationType != "manager" && event.AuthorizationType != "revision" {
			return fmt.Errorf("经理不得借 approval/decision 对非直属 issue")
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		if effectiveEventDeliveryMode(event) != deliveryModeWakeup || effectiveEventDeliveryTarget(event) != deliveryTargetNextTurn {
			return fmt.Errorf("issue 固定 wakeup/next-turn，不接受降档")
		}
		if err := validateDeliveryPrimitives(event); err != nil {
			return err
		}
		if err := validateAssignmentContractEvent(event, cfg); err != nil {
			return err
		}
		if event.AuthorizationType == "revision" {
			delete(s.pendingAssignmentRevisions, event.CaseID)
		}
		return nil

	case "issue_sent":
		if err := s.applyDeliveryTerminal(event, deliverySent); err != nil {
			return err
		}
		prepared, ok := s.events[event.RelatedEventID]
		if !ok || prepared.Type != "issue_prepared" || prepared.CaseID != event.CaseID || !sameIssueContract(prepared, event) {
			return fmt.Errorf("issue_sent 与 prepared 合同不一致")
		}
		state, err := s.currentCase(event.CaseID)
		if err != nil {
			return err
		}
		if event.FromState != state.Status || event.ToState != string(statusDispatched) || event.Owner != event.Recipient {
			return fmt.Errorf("issue_sent 的前态、后态或 owner 不匹配")
		}
		if err := validateStateTransition(actionIssue, event.FromState, event.ToState); err != nil {
			return err
		}
		assignment, err := assignmentFromDeliveredIssue(event, prepared, cfg)
		if err != nil {
			return err
		}
		for _, existing := range s.assignments {
			if existing.AssignmentID == assignment.AssignmentID {
				return fmt.Errorf("assignment_id 重复：%s", assignment.AssignmentID)
			}
		}
		s.assignments[event.ID] = assignment
		s.assignmentList = append(s.assignmentList, event.ID)
		return nil

	case "message_prepared":
		if event.CaseID != "" {
			if _, err := s.currentCase(event.CaseID); err != nil {
				return err
			}
		}
		if event.Recipient == event.Actor {
			return fmt.Errorf("message recipient 不能是自己")
		}
		if _, ok := cfg.exactRule(event.Recipient); !ok {
			return fmt.Errorf("message recipient 未登记或已停用")
		}
		if !validMessageKind(event.MessageKind) {
			return fmt.Errorf("message kind 非法")
		}
		if !validMessageUrgency(effectiveMessageUrgency(event.Urgency)) {
			return fmt.Errorf("message urgency 非法")
		}
		if effectiveMessageUrgency(event.Urgency) == messageUrgencyUrgent && event.MessageKind != "directive" {
			return fmt.Errorf("urgent message 必须是绑定 active assignment 的 directive")
		}
		if effectiveMessageUrgency(event.Urgency) == messageUrgencyUrgent &&
			(effectiveEventDeliveryMode(event) != deliveryModeWakeup || effectiveEventDeliveryTarget(event) != deliveryTargetNextTurn || event.DeliveryReason != "urgent-next-turn") {
			return fmt.Errorf("urgent message 必须固定为 wakeup/next-turn 且 reason=urgent-next-turn")
		}
		if event.MessageKind == "directive" {
			if event.CaseID == "" {
				return fmt.Errorf("directive 必须绑定 case 与 active assignment")
			}
			assignment := s.assignments[event.AssignmentEventID]
			if assignment == nil || assignment.Consumed || assignment.CaseID != event.CaseID || assignment.Recipient != event.Recipient ||
				(event.Actor != assignment.Issuer && event.Actor != assignment.Reviewer && event.Actor != assignment.Acceptor) ||
				event.CaseVersion != assignment.CaseVersion || event.CaseDigest != assignment.CaseDigest ||
				!eventMatchesAssignmentState(event, assignment) {
				return fmt.Errorf("directive 未冻结匹配的 active assignment 或 issuer/reviewer/acceptor authority")
			}
		} else if event.AssignmentEventID != "" || event.AssignmentID != "" || event.AssignmentDigest != "" {
			return fmt.Errorf("只有 directive message 可以携带 assignment binding")
		}
		if event.MessageID != "" {
			if err := validateLedgerID("message_id", event.MessageID); err != nil {
				return err
			}
			if _, exists := messageByID(s, event.MessageID); exists {
				return fmt.Errorf("message_id 重复：%s", event.MessageID)
			}
			for _, id := range event.RefCases {
				if _, err := s.currentCase(id); err != nil {
					return fmt.Errorf("ref-case：%w", err)
				}
			}
			for _, id := range event.RefMessages {
				if _, ok := messageByID(s, id); !ok {
					return fmt.Errorf("ref-message 不存在：%s", id)
				}
			}
			for _, id := range event.RefEvents {
				if _, ok := s.events[id]; !ok {
					return fmt.Errorf("ref-event 不存在：%s", id)
				}
			}
			if event.ReplyTo != "" {
				original, ok := messageByID(s, event.ReplyTo)
				if !ok || original.ThreadID != event.ThreadID {
					return fmt.Errorf("reply-to message/thread 不匹配")
				}
			}
		}
		if err := validateDeliveryPrimitives(event); err != nil {
			return err
		}
		if err := requireNoStateFields(event); err != nil {
			return err
		}
		return nil

	case "message_sent":
		if err := s.applyDeliveryTerminal(event, deliverySent); err != nil {
			return err
		}
		prepared, ok := s.events[event.RelatedEventID]
		if !ok || prepared.Type != "message_prepared" || !sameMessageContract(prepared, event) {
			return fmt.Errorf("message_sent 与 prepared 合同不一致")
		}
		return requireNoStateFields(event)

	case "message_acked":
		original, ok := s.events[event.RelatedEventID]
		semantic, semanticOK := semanticDeliveredEvent(original, s.events)
		if !ok || !semanticOK || semantic.Type != "message_sent" || semantic.Recipient != event.Actor || semantic.DeliveryID != event.DeliveryID || semantic.PayloadDigest != event.PayloadDigest {
			return fmt.Errorf("message_acked 缺少匹配的已送达 message")
		}
		if event.MessageID != semantic.MessageID || event.MessageKind != semantic.MessageKind || event.ThreadID != semantic.ThreadID {
			return fmt.Errorf("message ack contract 不匹配")
		}
		if err := s.acknowledgeDelivery(original, event); err != nil {
			return err
		}
		return requireNoStateFields(event)
	}
	return fmt.Errorf("未知 approval/issue/message 事件类型 %q", event.Type)
}

func mustEventTime(event Event) time.Time {
	parsed, _ := time.Parse(time.RFC3339, event.At)
	return parsed
}
func mustParseTime(value string) time.Time {
	parsed, _ := time.Parse(time.RFC3339, value)
	return parsed
}

func validMessageKind(value string) bool {
	switch value {
	case "info", "question", "request", "handoff", "directive":
		return true
	}
	return false
}

func sameIssueContract(a, b Event) bool {
	return a.Actor == b.Actor && a.ActorLabel == b.ActorLabel && a.ActorPaneID == b.ActorPaneID &&
		a.Recipient == b.Recipient && a.RecipientLabel == b.RecipientLabel && a.Project == b.Project &&
		a.CaseVersion == b.CaseVersion && a.CaseDigest == b.CaseDigest &&
		a.AuthorizationType == b.AuthorizationType &&
		a.AuthorizationDigest == b.AuthorizationDigest && a.ApprovalID == b.ApprovalID && a.DecisionRef == b.DecisionRef &&
		a.Issuer == b.Issuer && a.CapturedBy == b.CapturedBy && a.NextAction == b.NextAction && a.Note == b.Note && a.BasisEventID == b.BasisEventID &&
		a.SupersedesAssignmentEventID == b.SupersedesAssignmentEventID &&
		a.SupersedesAssignmentID == b.SupersedesAssignmentID &&
		effectiveMessageUrgency(a.Urgency) == effectiveMessageUrgency(b.Urgency) &&
		sameAssignmentBinding(a, b) &&
		effectiveEventDeliveryMode(a) == effectiveEventDeliveryMode(b) && effectiveEventDeliveryTarget(a) == effectiveEventDeliveryTarget(b)
}

func sameMessageContract(a, b Event) bool {
	return a.Actor == b.Actor && a.ActorLabel == b.ActorLabel && a.ActorPaneID == b.ActorPaneID &&
		a.Recipient == b.Recipient && a.RecipientLabel == b.RecipientLabel && a.CaseID == b.CaseID &&
		a.MessageID == b.MessageID && a.MessageKind == b.MessageKind && a.Message == b.Message && a.SourceRef == b.SourceRef &&
		effectiveMessageUrgency(a.Urgency) == effectiveMessageUrgency(b.Urgency) &&
		a.ThreadID == b.ThreadID && a.ReplyTo == b.ReplyTo &&
		stringListsEqual(a.RefFiles, b.RefFiles) && stringListsEqual(a.RefCases, b.RefCases) &&
		stringListsEqual(a.RefMessages, b.RefMessages) && stringListsEqual(a.RefEvents, b.RefEvents) &&
		a.CaseVersion == b.CaseVersion && a.CaseDigest == b.CaseDigest && sameAssignmentBinding(a, b) &&
		effectiveEventDeliveryMode(a) == effectiveEventDeliveryMode(b) && effectiveEventDeliveryTarget(a) == effectiveEventDeliveryTarget(b)
}

func stringListsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func copyApprovalScope(dst *Event, source Event) {
	dst.ApprovalID, dst.ApprovalAction, dst.ApprovalMode = source.ApprovalID, source.ApprovalAction, source.ApprovalMode
	dst.Recipient, dst.RecipientLabel = source.Recipient, source.RecipientLabel
	dst.CaseVersion, dst.CaseDigest = source.CaseVersion, source.CaseDigest
	dst.BasisEventID = source.BasisEventID
	dst.AssigneeSeatVersion, dst.AssigneeSeatDigest = source.AssigneeSeatVersion, source.AssigneeSeatDigest
	dst.Title = source.Title
	dst.ExpiresAt = source.ExpiresAt
}

func (a *App) cmdApproval(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq approval request|grant|revoke|expire|show")
	}
	switch args[0] {
	case "request":
		return a.cmdApprovalRequest(args[1:])
	case "grant":
		return a.cmdApprovalGrant(args[1:])
	case "revoke":
		return a.cmdApprovalTerminate(args[1:], false)
	case "expire":
		return a.cmdApprovalTerminate(args[1:], true)
	case "show":
		return a.cmdApprovalShow(args[1:])
	default:
		return fmt.Errorf("未知 approval 子命令 %q", args[0])
	}
}

func (a *App) cmdApprovalRequest(args []string) error {
	fs := newLeafParser("approval request")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "approval_id")
	caseID := fs.String("case", "", "case_id")
	action := fs.String("action", "issue", "目前仅 issue")
	target := fs.String("target", "", "target agent")
	expiresAt := fs.String("expires", "", "RFC3339 有效期")
	mode := fs.String("mode", "one_time", "仅 one_time")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if err := validateLedgerID("approval_id", strings.TrimSpace(*id)); err != nil {
		return err
	}
	if err := validateCaseID(strings.TrimSpace(*caseID)); err != nil {
		return err
	}
	if strings.TrimSpace(*action) != "issue" {
		return fmt.Errorf("approval action 当前只能是 issue")
	}
	targetRule, ok := a.Config.exactRule(strings.TrimSpace(*target))
	if !ok || !targetRule.CanReceiveOrder || !targetRule.CanAccept {
		return fmt.Errorf("target 未登记、已停用或缺少 can_receive_order/can_accept")
	}
	// A manager already has an intrinsic, narrowly scoped authority to issue a
	// case to an exact direct report.  Such an issue must use the manager
	// authorization path; creating an approval here would record a grant that
	// cmdIssue can never consume.  Approval must not turn into a way to cross a
	// reporting boundary either, so reject every manager request before it can
	// touch the ledger and give the actionable instruction for each case.
	if a.Config.isManager(actor.Rule) {
		if targetRule.ReportsTo == actor.Name {
			return permissionf("部门经理委派直属下属无需 approval；请直接运行 hq issue --case %s --to %s --next TEXT，且不要传 --approval/--decision", strings.TrimSpace(*caseID), targetRule.Name)
		}
		return permissionf("部门经理只能委派自己的直属下属；target=%s reports_to=%s，不得通过 approval 跨越管理边界，也不要用 message 冒充 durable ownership。若你是 case %s 当前负责人，请运行 hq case escalate --id NEW-REWORK-ID --parent %s --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 --source PATH --reason TEXT --next TEXT；HQ 固定上交你的直属上级 %s，再把工作路由给对应经理 %s", targetRule.Name, targetRule.ReportsTo, strings.TrimSpace(*caseID), strings.TrimSpace(*caseID), actor.Rule.ReportsTo, targetRule.ReportsTo)
	}
	witness, err := a.Config.approvalWitness()
	if err != nil {
		return err
	}
	if actor.Name != witness.Name {
		return permissionf("approval request 只能由总裁秘书（职责位 %s，agent=%s）创建；当前 actor=%s。请向 %s 发送授权请求，由其核验 case owner 后执行 hq approval request", roleApprovalWitness, witness.Name, actor.Name, witness.Name)
	}
	if targetRule.ReportsTo != witness.Name {
		return permissionf("approval request 只能面向总裁秘书（职责位 %s，agent=%s）的直属部门经理；target=%s target_reports_to=%s。请先为该 case 创建 target=%s（直属经理，不是 specialist %s）的 approval/decision 并 issue 给其，由 %s 接收后拆分子 case 并直接 issue 给 %s", roleApprovalWitness, witness.Name, targetRule.Name, targetRule.ReportsTo, targetRule.ReportsTo, targetRule.Name, targetRule.ReportsTo, targetRule.Name)
	}
	if !a.Config.isManager(targetRule) {
		return permissionf("approval request target 必须是登记了 manager:<department> 职责位的直属部门经理；target=%s", targetRule.Name)
	}
	expires, err := time.Parse(time.RFC3339, strings.TrimSpace(*expiresAt))
	if err != nil || !expires.After(a.Store.NowTime()) {
		return fmt.Errorf("--expires 必须是晚于当前时间的 RFC3339")
	}
	cleanMode := strings.TrimSpace(*mode)
	if cleanMode != "one_time" {
		return fmt.Errorf("--mode 只能是 one_time；HQ 不支持跨 case generation 复用 approval")
	}
	commandID := stableCommandID("approval-request", actor.Name, strings.TrimSpace(*id))
	digest := requestDigest("approval-request", actor.Name, strings.TrimSpace(*id), *caseID, *action, targetRule.Name, expires.Format(time.RFC3339), cleanMode)
	result, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		event, err := a.newEvent(actor, "approval_requested", strings.TrimSpace(*caseID))
		if err != nil {
			return Event{}, err
		}
		event.ApprovalID, event.ApprovalAction, event.ApprovalStatus, event.ApprovalMode = strings.TrimSpace(*id), "issue", "requested", cleanMode
		event.Recipient, event.RecipientLabel = targetRule.Name, targetRule.Label
		state, err := ledger.currentCase(event.CaseID)
		if err != nil {
			return Event{}, err
		}
		if state.Owner != witness.Name {
			return Event{}, permissionf("approval request 要求当前 case owner 为总裁秘书（职责位 %s，agent=%s）；case=%s owner=%s。请由当前 owner 先完成或回报现有交付，待 %s 正式接收 case 后再由其重试 hq approval request", roleApprovalWitness, witness.Name, event.CaseID, state.Owner, witness.Name)
		}
		if state.Version == 0 || state.Digest == "" {
			return Event{}, fmt.Errorf("case 缺少合法规格版本或 digest，拒绝创建 approval")
		}
		generation := ledger.caseGeneration(event.CaseID)
		if generation == "" {
			return Event{}, fmt.Errorf("case 缺少合法 business generation，拒绝创建 approval")
		}
		event.CaseVersion, event.CaseDigest, event.Title = state.Version, state.Digest, state.Title
		event.BasisEventID = generation
		event.AssigneeSeatVersion, event.AssigneeSeatDigest = targetRule.SeatVersion, targetRule.SeatDigest
		event.ExpiresAt = expires.Format(time.RFC3339)
		return event, nil
	})
	if err != nil {
		return err
	}
	return a.output(result.Event, fmt.Sprintf("approval=%s requested；case=%s@v%d；expires=%s", result.Event.ApprovalID, result.Event.CaseID, result.Event.CaseVersion, result.Event.ExpiresAt))
}

func (a *App) cmdApprovalGrant(args []string) error {
	fs := newLeafParser("approval grant")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "approval_id")
	issuer := fs.String("issuer", "", "公司所有者标识；必须匹配 config.yaml owner_principal")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	ownerPrincipal := a.Config.ownerPrincipal()
	if strings.TrimSpace(*issuer) != ownerPrincipal {
		return fmt.Errorf("--issuer 必须精确匹配 config.yaml owner_principal %q；captured_by 由当前身份自动记录", ownerPrincipal)
	}
	witness, err := a.Config.approvalWitness()
	if err != nil {
		return err
	}
	if actor.Name != witness.Name {
		return permissionf("当前 actor=%s 不是总裁秘书（职责位 %s，agent=%s）；请由 %s 执行 hq approval grant", actor.Name, roleApprovalWitness, witness.Name, witness.Name)
	}
	commandID := stableCommandID("approval-grant", actor.Name, strings.TrimSpace(*id))
	digest := requestDigest("approval-grant", actor.Name, strings.TrimSpace(*id), ownerPrincipal)
	result, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		record := ledger.approvals[strings.TrimSpace(*id)]
		if record == nil {
			return Event{}, fmt.Errorf("approval 不存在")
		}
		if err := validateApprovalSnapshotFreshness(ledger, a.Config, record.Request, witness); err != nil {
			return Event{}, err
		}
		event, err := a.newEvent(actor, "approval_granted", record.Request.CaseID)
		if err != nil {
			return Event{}, err
		}
		copyApprovalScope(&event, record.Request)
		event.RelatedEventID = record.Request.ID
		event.ApprovalStatus, event.Issuer, event.CapturedBy = "granted", ownerPrincipal, actor.Name
		return event, nil
	})
	if err != nil {
		return err
	}
	return a.output(result.Event, fmt.Sprintf("approval=%s granted；issuer=%s captured_by=%s", result.Event.ApprovalID, result.Event.Issuer, result.Event.CapturedBy))
}

func (a *App) cmdApprovalTerminate(args []string, expire bool) error {
	command := "approval revoke"
	eventType, status := "approval_revoked", "revoked"
	if expire {
		command, eventType, status = "approval expire", "approval_expired", "expired"
	}
	fs := newLeafParser(command)
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "approval_id")
	reason := fs.String("reason", "", "撤销原因")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	cleanReason := ""
	if !expire {
		cleanReason, err = validateBusinessText("reason", *reason, true)
		if err != nil {
			return err
		}
	}
	commandID := stableCommandID(strings.ReplaceAll(command, " ", "-"), actor.Name, strings.TrimSpace(*id))
	digest := requestDigest(command, actor.Name, strings.TrimSpace(*id), cleanReason)
	result, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		record := ledger.approvals[strings.TrimSpace(*id)]
		if record == nil {
			return Event{}, fmt.Errorf("approval 不存在")
		}
		if record.Status != "requested" && record.Status != "granted" {
			return Event{}, fmt.Errorf("approval 状态=%s 不可终结", record.Status)
		}
		event, err := a.newEvent(actor, eventType, record.Request.CaseID)
		if err != nil {
			return Event{}, err
		}
		copyApprovalScope(&event, record.Request)
		event.RelatedEventID = record.Request.ID
		if record.Status == "granted" {
			event.RelatedEventID = record.Grant.ID
		}
		event.ApprovalStatus, event.Note = status, cleanReason
		return event, nil
	})
	if err != nil {
		return err
	}
	return a.output(result.Event, fmt.Sprintf("approval=%s %s", result.Event.ApprovalID, status))
}

func (a *App) cmdApprovalShow(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("用法：hq approval show <approval_id>")
	}
	if err := validateLedgerID("approval_id", args[0]); err != nil {
		return err
	}
	ledger, err := a.ledgerState()
	if err != nil {
		return err
	}
	record := ledger.approvals[args[0]]
	if record == nil {
		return fmt.Errorf("approval 不存在：%s", args[0])
	}
	view := approvalView(record)
	return a.output(view, fmt.Sprintf("approval=%s status=%s action=%s target=%s target_seat=%d/%s case=%s@v%d generation=%s title=%s digest=%s expires=%s issuer=%s captured_by=%s", view.ApprovalID, view.Status, view.Action, view.Target, view.TargetSeatVersion, view.TargetSeatDigest, view.CaseID, view.CaseVersion, view.CaseGeneration, view.CaseTitle, view.CaseDigest, view.ExpiresAt, view.Issuer, view.CapturedBy))
}

func formatIssueEnvelope(event Event, ref string) (string, error) {
	due := event.DueAt
	if due == "" {
		due = "none"
	}
	prefix := "[HQ notification]"
	revision := ""
	revisionInstruction := ""
	if event.SupersedesAssignmentID != "" {
		prefix = "[HQ URGENT REVISION]"
		revision = fmt.Sprintf(" SUPERSEDES=%s", event.SupersedesAssignmentID)
		revisionInstruction = "旧 assignment 已失效，不得再按旧合同 report；若新要求影响你已委派的直属 child，必须对对应 active child 使用 `hq case revise --supersede-active` 逐级更新，不得用普通 message 冒充合同变更；"
	}
	message := fmt.Sprintf("%s[%s] CASE=%s VERSION=%d DIGEST=%s ASSIGNMENT=%s%s CONTRACT=%s ROLE_CARD=%s@%d ROLE_DIGEST=%s MANUAL=%s SEAT_VERSION=%d SEAT_DIGEST=%s ACCEPTOR=%s DUE=%s EVENT=%s DELIVERY=%s：正式委派；%s先完整阅读 MANUAL，再运行 `hq accept --event %s`；下一步：%s；HQ 会在本次唤醒 prompt 中自动附带此前静默消息（如有）；账本：%s",
		prefix, event.ActorLabel, event.CaseID, event.CaseVersion, event.CaseDigest, event.AssignmentID, revision, event.AssignmentDigest,
		event.RoleCardID, event.RoleCardVersion, event.RoleCardDigest, event.RoleCardManualPath,
		event.AssigneeSeatVersion, event.AssigneeSeatDigest, event.Acceptor, due, event.ID, event.DeliveryID, revisionInstruction, event.ID, event.NextAction, ref)
	if !utf8.ValidString(message) || strings.ContainsRune(message, '\x00') || len([]byte(message)) > maxTurnBundleBaseBytes {
		return "", fmt.Errorf("issue 门铃载荷超过 %d KiB 总线基线", maxTurnBundleBaseBytes/1024)
	}
	return message, nil
}

func formatMessageEnvelope(event Event, ref string) (string, error) {
	casePart := ""
	if event.CaseID != "" {
		casePart = " CASE=" + event.CaseID
	}
	messageID := event.MessageID
	if messageID == "" {
		messageID = event.ID
	}
	refs := formatMessageRefs(event)
	ack := ""
	if messageNeedsAction(event.MessageKind) {
		ack = fmt.Sprintf("；收到并读懂后必须先运行 `hq message ack --message %s` 写入 durable ack；未 ack 会阻止双方的 on_assignment runtime 休眠", messageID)
	}
	if event.Urgency == "" && event.MessageKind != "directive" {
		message := fmt.Sprintf("[HQ message][%s] MESSAGE=%s KIND=%s%s THREAD=%s DELIVERY=%s：%s%s%s；账本：%s",
			event.ActorLabel, messageID, event.MessageKind, casePart, event.ThreadID, event.DeliveryID, event.Message, refs, ack, ref)
		if len([]byte(message)) > 8*1024 {
			return "", fmt.Errorf("message 总线信封超过 8 KiB；减少引用数量")
		}
		return message, nil
	}
	prefix := "[HQ message]"
	if effectiveMessageUrgency(event.Urgency) == messageUrgencyUrgent {
		prefix = "[HQ URGENT DIRECTIVE]"
	}
	binding := ""
	if event.MessageKind == "directive" {
		binding = fmt.Sprintf(" ASSIGNMENT=%s CASE_VERSION=%d CASE_DIGEST=%s", event.AssignmentID, event.CaseVersion, event.CaseDigest)
	}
	message := fmt.Sprintf("%s[%s] MESSAGE=%s KIND=%s URGENCY=%s%s%s THREAD=%s DELIVERY=%s：%s%s%s；本消息不修改 assignment objective/acceptance/constraints；如需修改合同，由冻结 issuer 使用 `hq case revise --supersede-active`；账本：%s",
		prefix, event.ActorLabel, messageID, event.MessageKind, effectiveMessageUrgency(event.Urgency), casePart, binding, event.ThreadID, event.DeliveryID, event.Message, refs, ack, ref)
	if len([]byte(message)) > 8*1024 {
		return "", fmt.Errorf("message 总线信封超过 8 KiB；减少引用数量")
	}
	return message, nil
}

func formatMessageRefs(event Event) string {
	parts := []string{}
	for _, value := range event.RefFiles {
		parts = append(parts, "file="+value)
	}
	for _, value := range event.RefCases {
		parts = append(parts, "case="+value)
	}
	for _, value := range event.RefMessages {
		parts = append(parts, "message="+value)
	}
	for _, value := range event.RefEvents {
		parts = append(parts, "event="+value)
	}
	if event.SourceRef != "" {
		parts = append(parts, "file="+event.SourceRef)
	}
	if len(parts) == 0 {
		return ""
	}
	return "；引用：" + strings.Join(parts, ", ")
}
