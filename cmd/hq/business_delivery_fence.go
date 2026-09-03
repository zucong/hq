package main

import (
	"fmt"
	"sort"
	"strings"
)

func isBusinessDeliveryOrigin(eventType string) bool {
	return eventType == "issue_prepared" || eventType == "report_prepared" || eventType == "case_escalation_prepared"
}

func (s *ledgerState) businessDeliveryOriginMatchesCurrent(record *deliveryRecord) bool {
	if record == nil || !isBusinessDeliveryOrigin(record.Origin.Type) {
		return false
	}
	state := s.snapshot.Cases[record.Origin.CaseID]
	if state == nil || state.Status != record.Origin.FromState {
		return false
	}
	round := s.businessDeliveryRounds[record.Origin.DeliveryID]
	if round == "" || round != s.caseGeneration(record.Origin.CaseID) {
		return false
	}
	if record.Origin.CaseVersion > 0 && state.Version != record.Origin.CaseVersion {
		return false
	}
	if record.Origin.CaseDigest != "" && state.Digest != record.Origin.CaseDigest {
		return false
	}
	return true
}

func (s *ledgerState) businessDeliveryFenceApplicable(record *deliveryRecord) bool {
	return record != nil && record.Status != deliverySent && s.businessDeliveryOriginMatchesCurrent(record)
}

func (s *ledgerState) applicableBusinessDeliveryFences(caseID string) []*deliveryRecord {
	var candidates []*deliveryRecord
	for _, record := range s.deliveries {
		if record.Origin.CaseID == caseID && s.businessDeliveryFenceApplicable(record) {
			candidates = append(candidates, record)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Origin.Sequence != candidates[j].Origin.Sequence {
			return candidates[i].Origin.Sequence < candidates[j].Origin.Sequence
		}
		return candidates[i].Origin.DeliveryID < candidates[j].Origin.DeliveryID
	})
	return candidates
}

func businessDeliveryFenceIDs(records []*deliveryRecord) string {
	ids := make([]string, 0, len(records))
	for _, record := range records {
		ids = append(ids, record.Origin.DeliveryID)
	}
	return strings.Join(ids, ",")
}

func pendingBusinessDeliveryError(caseID, action string, records []*deliveryRecord) error {
	return conflictf("case %s 存在未送达 business delivery fence %s，不可%s；请先按 hq delivery status --id DELIVERY 的指引 retry/resolve 原 delivery",
		caseID, businessDeliveryFenceIDs(records), action)
}

func isBusinessDeliveryConvergence(event Event, record *deliveryRecord) bool {
	if record == nil || !isBusinessDeliveryOrigin(record.Origin.Type) {
		return false
	}
	switch event.Type {
	case "case_escalation_sent":
		return record.Origin.Type == "case_escalation_prepared"
	case "issue_sent":
		return record.Origin.Type == "issue_prepared"
	case "report_sent":
		return record.Origin.Type == "report_prepared"
	case "delivery_resolved_sent":
		return true
	default:
		return false
	}
}

func (s *ledgerState) validateBusinessDeliveryAttempt(event Event) error {
	record := s.deliveries[event.DeliveryID]
	if record == nil || !isBusinessDeliveryOrigin(record.Origin.Type) {
		return nil
	}
	fences := s.applicableBusinessDeliveryFences(record.Origin.CaseID)
	if len(fences) != 1 || fences[0] != record || event.RelatedEventID != record.Origin.ID {
		return conflictf("delivery %s 不是 case %s 当前唯一适用的 business delivery fence（current=%s）；拒绝 stale retry",
			event.DeliveryID, record.Origin.CaseID, businessDeliveryFenceIDs(fences))
	}
	return nil
}

func (s *ledgerState) validateBusinessDeliveryConvergence(event Event, record *deliveryRecord) error {
	fences := s.applicableBusinessDeliveryFences(record.Origin.CaseID)
	if len(fences) == 1 && fences[0] == record && event.RelatedEventID == record.Origin.ID {
		return nil
	}
	return conflictf("delivery %s 的 origin %s 已不是 case %s 当前适用的 business delivery fence（current=%s）；拒绝 stale 收敛",
		event.DeliveryID, record.Origin.ID, record.Origin.CaseID, businessDeliveryFenceIDs(fences))
}

func (s *ledgerState) validateBusinessDeliveryFence(event Event) error {
	if event.CaseID == "" {
		return nil
	}
	if event.Type == "case_created" && event.ParentCaseID != "" {
		fences := s.applicableBusinessDeliveryFences(event.ParentCaseID)
		if len(fences) != 0 {
			return pendingBusinessDeliveryError(event.ParentCaseID, fmt.Sprintf("拆分子 case %s", event.CaseID), fences)
		}
	}
	if event.Type == "delivery_attempted" {
		return s.validateBusinessDeliveryAttempt(event)
	}

	record := s.deliveries[event.DeliveryID]
	if isBusinessDeliveryConvergence(event, record) {
		return s.validateBusinessDeliveryConvergence(event, record)
	}

	fences := s.applicableBusinessDeliveryFences(event.CaseID)
	if len(fences) == 0 || event.Type == "case_created" {
		return nil
	}
	if isBusinessDeliveryOrigin(event.Type) {
		return pendingBusinessDeliveryError(event.CaseID, "创建新 issue/report/escalation", fences)
	}
	if event.Type == "case_revised" || event.ToState != "" {
		return pendingBusinessDeliveryError(event.CaseID, fmt.Sprintf("推进业务状态（event=%s）", event.Type), fences)
	}
	return nil
}
