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

// admitManagerEscalation permits the one parent-preserving mutation that a
// department manager may need to perform while executing an assignment from
// their registered superior. The exception is deliberately narrower than
// ordinary ownership: it requires one accepted/rework contract for the exact
// case generation, issued and reviewed by that same superior. The manager must
// still report the parent assignment after the escalation child is handed off.
func (s *ledgerState) admitManagerEscalation(caseID, actor, superior string, caseVersion int, caseDigest string) error {
	active := s.activeAssignments(caseID)
	if len(active) == 0 {
		return nil
	}
	if len(active) != 1 {
		return conflictf("case %s 同时存在 %d 个未完成 assignment contract，不能安全判定上行 escalation 权限；运行 `hq assignment list --case %s` 与 `hq history --case %s` 核验冲突，不要创建替代 case", caseID, len(active), caseID, caseID)
	}

	assignment := active[0]
	if assignment.Recipient != actor || assignment.Issuer != superior ||
		assignment.Reviewer != superior || assignment.Acceptor != superior ||
		assignment.CaseVersion != caseVersion || assignment.CaseDigest != caseDigest {
		return conflictf("case %s 的未完成 assignment %s 不匹配当前经理、registry 直属上级或当前 case generation；运行 `hq assignment show --id %s` 与 `hq case show --id %s` 核验，收敛该合同后再执行上行 escalation，不要绕过为普通子案", caseID, assignment.AssignmentID, assignment.AssignmentID, caseID)
	}
	if !assignment.Accepted || (assignment.Status != "accepted" && assignment.Status != "rework") {
		if assignment.Status == "issued" {
			return conflictf("case %s 的经理 assignment %s status=issued，尚未接单；先运行 `hq accept --event %s --next TEXT`，接单后重试原 `hq case escalate`，不要先 report 或创建替代 case", caseID, assignment.AssignmentID, assignment.EventID)
		}
		return conflictf("case %s 的经理 assignment %s status=%s，不处于 accepted|rework 执行态；运行 `hq assignment show --id %s` 并按其 reviewer/acceptor 合同收敛，随后只重试原 `hq case escalate`", caseID, assignment.AssignmentID, assignment.Status, assignment.AssignmentID)
	}
	return nil
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
