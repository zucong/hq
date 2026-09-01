package main

import (
	"sort"
	"strings"
)

func (s *ledgerState) activeAssignments(caseID string) []*caseAssignment {
	var result []*caseAssignment
	for _, assignment := range s.assignments {
		if assignment.CaseID == caseID && !assignment.Consumed {
			result = append(result, assignment)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].AssignmentID != result[j].AssignmentID {
			return result[i].AssignmentID < result[j].AssignmentID
		}
		return result[i].EventID < result[j].EventID
	})
	return result
}

func (s *ledgerState) rejectActiveAssignment(caseID, action string) error {
	active := s.activeAssignments(caseID)
	if len(active) == 0 {
		return nil
	}
	ids := make([]string, 0, len(active))
	for _, assignment := range active {
		ids = append(ids, assignment.AssignmentID)
	}
	return conflictf("case %s 存在未完成 assignment contract %s，不可%s；请先完成当前合同的 report/review/rework 闭环",
		caseID, strings.Join(ids, ","), action)
}

func (s *ledgerState) rejectOwnerReportOverActiveAssignment(caseID string) error {
	active := s.activeAssignments(caseID)
	if len(active) == 0 {
		return nil
	}
	contracts := make([]string, 0, len(active))
	for _, assignment := range active {
		contracts = append(contracts, assignment.EventID)
	}
	return conflictf("report 必须引用 active assignment contract %s，不得降级为 owner report；reviewer 必须先 accept/return 当前 submission",
		strings.Join(contracts, ","))
}
