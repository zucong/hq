package main

import (
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func configureEscalationTestCompany(t *testing.T, e testEnv) Config {
	t.Helper()
	cfg := testConfig()
	cfg.Agents = append(cfg.Agents, AgentRule{
		Name: "qa-reverify", Label: "质量部-复验员", Nickname: "复验员", DepartmentLabel: "质量部",
		Workspace: cfg.WorkspaceLabel, Responsibilities: []string{"reverify:qa-ux"},
		Department: "qa-ux", ReportsTo: "baogong", Kind: "codex",
		CanCreate: true, CanAccept: true, CanReceiveOrder: true,
	})
	cfg = bindTestRoleContracts(cfg)
	writeTestConfig(t, e.config, cfg)
	for _, rule := range cfg.Agents {
		writeTestFile(t, filepath.Join(e.root, rule.ManualPath), string(testRoleManual(rule.Name)))
	}
	return cfg
}

func seedFindingAcceptedQualityCase(t *testing.T, e testEnv, cfg Config, caseID, source string) Event {
	t.Helper()
	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "质量门禁发现跨部门缺陷",
		"--project", "manager-escalation", "--objective", "验证发布质量", "--acceptance", "缺陷有可复验依据",
		"--constraints", "不得绕过管理链", "--priority", "P1", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "qa-reverify", "--next", "执行初次质量门禁")
	events := scenarioEvents(t, e, cfg)
	issue := latestCaseEvent(events, caseID, "issue_sent")

	e.setActor(t, "qa-reverify", "escalation:qa-initial", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "复现缺陷")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "finding", "--severity", "P1",
		"--source", source, "--location", "calculator input parser", "--verify", "输入 +1 必须被拒绝", "--next", "质量经理核验")
	events = scenarioEvents(t, e, cfg)
	finding := latestCaseEvent(events, caseID, "report_sent")

	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, "accept", "--event", finding.ID, "--next", "建立跨部门返工交接")
	state, err := e.app(t).currentCase(caseID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != string(statusFindingAccepted) || state.Owner != "baogong" {
		t.Fatalf("quality finding fixture did not converge: %+v", state)
	}
	return finding
}

func escalationCommand(parentID, childID, source string) []string {
	return []string{"case", "escalate", "--id", childID, "--parent", parentID,
		"--title", "修复质量门禁缺陷", "--objective", "修复输入解析并保留回归证据",
		"--acceptance", "+1 被拒绝且原有效向量仍通过", "--constraints", "不倒转已验收历史",
		"--priority", "P1", "--source", source, "--reason", "质量 P1 已独立复现",
		"--next", "审批后委派工程经理修复"}
}

func TestManagerEscalationVirtualCompanyReworkAndReverify(t *testing.T) {
	e := setupTestEnv(t)
	cfg := configureEscalationTestCompany(t, e)
	source := writeTestFile(t, filepath.Join(e.root, "qa-ux", "p1-finding.md"), "# P1 finding\n+1 must be rejected.\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "plus-sign-fix.md"), "# Fix\nParser rejects leading plus.\n")
	reverifyArtifact := writeTestFile(t, filepath.Join(e.root, "qa-ux", "reverify.md"), "# Reverify\nRegression and valid vectors pass.\n")
	rootID, escalationID := "ESC-COMPANY-ROOT", "ESC-ENGINEERING-REWORK"
	oldFinding := seedFindingAcceptedQualityCase(t, e, cfg, rootID, source)
	parentBefore, err := e.app(t).currentCase(rootID)
	if err != nil {
		t.Fatal(err)
	}

	// A resolved finding cannot be rewound, and a manager cannot issue across
	// departments. Both errors point to the durable first-class recovery path.
	beforeRejected := snapshotTree(t, e.data)
	for name, command := range map[string][]string{
		"old terminal":     {"return", "--event", oldFinding.ID, "--reason", "late cross-department rework", "--next", "engineer fixes"},
		"cross department": {"issue", "--case", rootID, "--to", "zantianyou", "--next", "fix directly"},
	} {
		err := e.app(t).run(command)
		if err == nil || !strings.Contains(err.Error(), "hq case escalate") {
			t.Fatalf("%s error was not executable: %v", name, err)
		}
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(beforeRejected, after) {
		t.Fatal("rejected terminal/cross-department operations changed durable state")
	}

	// Quality manager creates a new child and can only hand it to the exact
	// registry superior. The parent and its finding_accepted terminal stay put.
	runTestCommand(t, e, escalationCommand(rootID, escalationID, source)...)
	events := scenarioEvents(t, e, cfg)
	prepared := latestCaseEvent(events, escalationID, "case_escalation_prepared")
	sent := latestCaseEvent(events, escalationID, "case_escalation_sent")
	if prepared.ID == "" || sent.ID == "" || prepared.Recipient != "penny" || sent.Owner != "penny" || sent.ToState != string(statusEscalated) {
		t.Fatalf("bad escalation delivery: prepared=%+v sent=%+v", prepared, sent)
	}
	parentAfterEscalation, err := e.app(t).currentCase(rootID)
	if err != nil {
		t.Fatal(err)
	}
	if parentAfterEscalation.Status != parentBefore.Status || parentAfterEscalation.Owner != parentBefore.Owner ||
		parentAfterEscalation.LastEventID != parentBefore.LastEventID {
		t.Fatalf("escalation rewrote parent terminal: before=%+v after=%+v", parentBefore, parentAfterEscalation)
	}

	// The owner liaison acknowledges receipt, records a one-time owner
	// approval, and delegates only to its direct engineering manager.
	e.setActor(t, "penny", "escalation:owner-liaison", e.office)
	runTestCommand(t, e, "accept", "--event", sent.ID, "--next", "申请公司所有者批准工程返工")
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	runTestCommand(t, e, "approval", "request", "--id", "APR-ESC-ENGINEERING", "--case", escalationID,
		"--target", "zantianyou", "--expires", expires)
	runTestCommand(t, e, "approval", "grant", "--id", "APR-ESC-ENGINEERING", "--issuer", cfg.ownerPrincipal())
	runTestCommand(t, e, "issue", "--case", escalationID, "--to", "zantianyou", "--approval", "APR-ESC-ENGINEERING", "--next", "修复并回报回归证据")
	events = scenarioEvents(t, e, cfg)
	engineeringIssue := latestCaseEvent(events, escalationID, "issue_sent")

	// Engineering manager activates the app developer through a normal child
	// assignment and independently accepts its evidence before reporting up.
	e.setActor(t, "zantianyou", "escalation:engineering-manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", engineeringIssue.ID, "--next", "拆分应用修复")
	runTestCommand(t, e, "case", "create", "--id", "ESC-APP-FIX", "--parent", escalationID,
		"--title", "修复输入解析", "--objective", "拒绝前导加号",
		"--acceptance", "新增回归测试通过", "--constraints", "保持数据合同", "--priority", "P1", "--source", source)
	runTestCommand(t, e, "issue", "--case", "ESC-APP-FIX", "--to", "eng-developer", "--next", "实现并运行回归测试")
	events = scenarioEvents(t, e, cfg)
	appIssue := latestCaseEvent(events, "ESC-APP-FIX", "issue_sent")

	e.setActor(t, "eng-developer", "escalation:app-developer", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", appIssue.ID, "--next", "修复 parser")
	runTestCommand(t, e, "report", "--case", "ESC-APP-FIX", "--result", "completed", "--artifact", artifact,
		"--verify", "+1 rejected; valid vectors pass", "--next", "工程经理复核")
	events = scenarioEvents(t, e, cfg)
	appReport := latestCaseEvent(events, "ESC-APP-FIX", "report_sent")

	e.setActor(t, "zantianyou", "escalation:engineering-manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", appReport.ID, "--next", "汇总工程返工")
	runTestCommand(t, e, "report", "--case", escalationID, "--result", "completed", "--artifact", artifact,
		"--verify", "应用子 case 已独立验收", "--next", "总部验收并通知质量复验")
	events = scenarioEvents(t, e, cfg)
	engineeringReport := latestCaseEvent(events, escalationID, "report_sent")

	// Headquarters accepts engineering evidence, then Quality owns and runs a
	// fresh reverify child under the untouched finding parent.
	e.setActor(t, "penny", "escalation:owner-liaison", e.office)
	runTestCommand(t, e, "accept", "--event", engineeringReport.ID, "--next", "质量复验")
	runTestCommand(t, e, "message", "--to", "baogong", "--kind", "handoff", "--case", rootID,
		"--text", "工程返工已验收，请执行独立复验", "--ref-file", artifact, "--delivery", "wakeup")

	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	// Reproduce the pilot mistake: a manager message cannot revive the original
	// consumed QA assignment. The rejected report writes nothing and directs the
	// manager to create and issue a fresh reverify child.
	runTestCommand(t, e, "message", "--to", "qa-reverify", "--kind", "handoff", "--case", rootID,
		"--text", "工程已修复，请在原 assignment 再次 report completed", "--ref-file", artifact, "--delivery", "wakeup")
	beforeOldReReport := snapshotTree(t, e.data)
	e.setActor(t, "qa-reverify", "escalation:qa-invalid-rereport", filepath.Join(e.root, "qa-ux"))
	err = e.app(t).run([]string{"report", "--case", rootID, "--result", "completed", "--artifact", reverifyArtifact,
		"--verify", "+1 rejected and valid vectors pass", "--next", "质量经理验收"})
	if err == nil || !strings.Contains(err.Error(), "不能靠 message 或再次 report 重开") ||
		!strings.Contains(err.Error(), "hq case revise --id "+rootID+" --version 2") ||
		!strings.Contains(err.Error(), "hq issue --case "+rootID+" --to qa-reverify") {
		t.Fatalf("terminal assignment re-report error was not executable: %v", err)
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(beforeOldReReport, after) {
		t.Fatal("rejected old-assignment report changed durable state")
	}

	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, "case", "revise", "--id", rootID, "--version", "2",
		"--title", "独立复验工程修复", "--objective", "复验 P1 与有效向量",
		"--acceptance", "P1 不再复现", "--constraints", "独立于工程结论", "--priority", "P1", "--source", artifact)
	runTestCommand(t, e, "issue", "--case", rootID, "--to", "qa-reverify", "--next", "执行黑盒回归")
	events = scenarioEvents(t, e, cfg)
	reverifyIssue := latestCaseEvent(events, rootID, "issue_sent")
	if reverifyIssue.AssignmentID == "" || reverifyIssue.AssignmentID == oldFinding.AssignmentID ||
		reverifyIssue.CaseVersion != 2 || reverifyIssue.CaseDigest == parentBefore.Digest {
		t.Fatalf("reverify did not establish a fresh versioned assignment: old=%+v new=%+v", oldFinding, reverifyIssue)
	}

	e.setActor(t, "qa-reverify", "escalation:qa-reverify", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, "accept", "--event", reverifyIssue.ID, "--next", "运行独立复验")
	runTestCommand(t, e, "report", "--case", rootID, "--result", "completed", "--artifact", reverifyArtifact,
		"--verify", "+1 rejected and valid vectors pass", "--next", "质量经理验收")
	events = scenarioEvents(t, e, cfg)
	reverifyReport := latestCaseEvent(events, rootID, "report_sent")

	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, "accept", "--event", reverifyReport.ID, "--next", "质量门禁通过")

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	if root := ledger.snapshot.Cases[rootID]; root.Status != string(statusAccepted) || root.Owner != "baogong" || root.Version != 2 {
		t.Fatalf("quality reverify round did not converge: %+v", root)
	}
	for id, wantOwner := range map[string]string{escalationID: "penny", "ESC-APP-FIX": "zantianyou"} {
		if state := ledger.snapshot.Cases[id]; state == nil || state.Status != string(statusAccepted) || state.Owner != wantOwner {
			t.Fatalf("case %s did not converge: %+v", id, state)
		}
	}
	beforeRebuild := ledger.snapshot
	rebuilt, err := NewStore(e.data).Rebuild(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if beforeRebuild.EventCount != rebuilt.EventCount || beforeRebuild.LastEventHash != rebuilt.LastEventHash || !reflect.DeepEqual(beforeRebuild.Cases, rebuilt.Cases) {
		t.Fatalf("strict replay diverged after escalation: before=%+v rebuilt=%+v", beforeRebuild, rebuilt)
	}
}

func assignedManagerEscalationFixture(t *testing.T, parentID string) (testEnv, Config, AgentRule, AgentRule, Event, string) {
	t.Helper()
	e := setupTestEnv(t)
	cfg := Config{
		Version: registrySchemaVersion, WorkspaceLabel: "hq-test", OwnerPrincipal: "ZC",
		Agents: []AgentRule{
			{Name: "owner-channel", Label: "总部联络职责位", Nickname: "联络官", DepartmentLabel: "总裁办", Workspace: "hq-test", Responsibilities: []string{roleApprovalWitness, roleAccountCloser, "operations_manager"}, Department: "ceo-office", Kind: "codex", CanCreate: true, CanIssue: true, CanAccept: true, CanClose: true, CanManageStaff: true, CanReceiveOrder: true},
			{Name: "delivery-manager", Label: "交付部负责人", Nickname: "交付负责人", DepartmentLabel: "交付部", Workspace: "hq-test", Responsibilities: []string{"manager:delivery"}, Department: "delivery", ReportsTo: "owner-channel", Kind: "codex", CanCreate: true, CanAccept: true, CanReceiveOrder: true},
		},
	}
	cfg = bindTestRoleContracts(cfg)
	writeTestConfig(t, e.config, cfg)
	for _, rule := range cfg.Agents {
		writeTestFile(t, filepath.Join(e.root, rule.ManualPath), string(testRoleManual(rule.Name)))
	}
	liaison, manager := cfg.Agents[0], cfg.Agents[1]
	source := writeTestFile(t, filepath.Join(e.root, manager.Department, "assigned-manager-escalation.md"), "# Escalation evidence\n")
	e.setActor(t, liaison.Name, "escalation:owner-channel", filepath.Join(e.root, liaison.Department))
	runTestCommand(t, e, "case", "create", "--id", parentID, "--title", "经理合同中的上行升级", "--project", "manager-escalation", "--source", source)
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	runTestCommand(t, e, "approval", "request", "--id", "APR-"+parentID, "--case", parentID, "--target", manager.Name, "--expires", expires)
	runTestCommand(t, e, "approval", "grant", "--id", "APR-"+parentID, "--issuer", cfg.ownerPrincipal())
	runTestCommand(t, e, "issue", "--case", parentID, "--to", manager.Name, "--approval", "APR-"+parentID, "--next", "核验并按需建立上行整改")
	events := scenarioEvents(t, e, cfg)
	return e, cfg, liaison, manager, latestCaseEvent(events, parentID, "issue_sent"), source
}

func TestManagerCanEscalateWithinAcceptedSuperiorAssignment(t *testing.T) {
	const parentID = "ESC-ASSIGNED-MANAGER-PARENT"
	const childID = "ESC-ASSIGNED-MANAGER-CHILD"
	e, cfg, liaison, manager, issue, source := assignedManagerEscalationFixture(t, parentID)
	artifact := writeTestFile(t, filepath.Join(e.root, manager.Department, "assigned-manager-result.md"), "# Escalation submitted\n")

	e.setActor(t, manager.Name, "escalation:assigned-manager", filepath.Join(e.root, manager.Department))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "核验后上交跨部门整改")
	runTestCommand(t, e, escalationCommand(parentID, childID, source)...)

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	parent, child := ledger.snapshot.Cases[parentID], ledger.snapshot.Cases[childID]
	assignment := ledger.assignments[issue.ID]
	if parent == nil || parent.Status != string(statusInProgress) || parent.Owner != manager.Name {
		t.Fatalf("escalation rewrote assigned parent: %+v", parent)
	}
	if child == nil || child.Status != string(statusEscalated) || child.Owner != liaison.Name || child.ParentCaseID != parentID {
		t.Fatalf("assigned manager escalation did not reach superior: %+v", child)
	}
	if assignment == nil || assignment.Status != "accepted" || assignment.Consumed {
		t.Fatalf("escalation consumed the manager assignment before its report: %+v", assignment)
	}

	// The escalation satisfies work inside the manager's original contract; the
	// manager still closes that contract through the ordinary report/review path.
	runTestCommand(t, e, "report", "--case", parentID, "--result", "completed", "--artifact", artifact,
		"--verify", "durable escalation child reached the registered superior", "--next", "review manager assignment")
	events := scenarioEvents(t, e, cfg)
	report := latestCaseEvent(events, parentID, "report_sent")
	e.setActor(t, liaison.Name, "escalation:assigned-manager-reviewer", filepath.Join(e.root, liaison.Department))
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "route escalated child")

	ledger, err = e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	if assignment = ledger.assignments[issue.ID]; assignment == nil || assignment.Status != "completed" || !assignment.Consumed {
		t.Fatalf("manager assignment did not converge after escalation report: %+v", assignment)
	}
	if _, err := NewStore(e.data).Rebuild(cfg); err != nil {
		t.Fatalf("strict rebuild rejected assigned manager escalation: %v", err)
	}
}

func TestManagerEscalationRejectsUnacceptedAssignmentWithExecutableRecovery(t *testing.T) {
	const parentID = "ESC-UNACCEPTED-MANAGER-PARENT"
	const childID = "ESC-UNACCEPTED-MANAGER-CHILD"
	e, _, _, manager, issue, source := assignedManagerEscalationFixture(t, parentID)

	e.setActor(t, manager.Name, "escalation:unaccepted-manager", filepath.Join(e.root, manager.Department))
	before := snapshotTree(t, e.data)
	err := e.app(t).run(escalationCommand(parentID, childID, source))
	if err == nil || !strings.Contains(err.Error(), "status=issued") ||
		!strings.Contains(err.Error(), "hq accept --event "+issue.ID) ||
		!strings.Contains(err.Error(), "重试原 `hq case escalate`") {
		t.Fatalf("unaccepted manager assignment error was not executable: %v", err)
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(before, after) {
		t.Fatal("rejected unaccepted-assignment escalation changed durable state")
	}
}

func TestManagerCanEscalateWithinReturnedSuperiorAssignment(t *testing.T) {
	const parentID = "ESC-REWORK-MANAGER-PARENT"
	const childID = "ESC-REWORK-MANAGER-CHILD"
	e, cfg, liaison, manager, issue, source := assignedManagerEscalationFixture(t, parentID)

	e.setActor(t, manager.Name, "escalation:rework-manager", filepath.Join(e.root, manager.Department))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "核验是否需要升级")
	runTestCommand(t, e, "report", "--case", parentID, "--result", "blocked", "--source", source,
		"--note", "需要直属上级确认升级路径", "--next", "review and return with escalation instruction")
	events := scenarioEvents(t, e, cfg)
	report := latestCaseEvent(events, parentID, "report_sent")

	e.setActor(t, liaison.Name, "escalation:rework-reviewer", filepath.Join(e.root, liaison.Department))
	runTestCommand(t, e, "return", "--event", report.ID, "--reason", "请在原经理合同内建立 durable escalation", "--next", "accept rework and escalate")

	e.setActor(t, manager.Name, "escalation:rework-manager", filepath.Join(e.root, manager.Department))
	runTestCommand(t, e, escalationCommand(parentID, childID, source)...)
	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[issue.ID]
	child := ledger.snapshot.Cases[childID]
	if assignment == nil || assignment.Status != "rework" || assignment.Consumed {
		t.Fatalf("escalation did not preserve returned manager assignment: %+v", assignment)
	}
	if child == nil || child.Status != string(statusEscalated) || child.Owner != liaison.Name {
		t.Fatalf("rework manager escalation did not reach superior: %+v", child)
	}
	if _, err := NewStore(e.data).Rebuild(cfg); err != nil {
		t.Fatalf("strict rebuild rejected rework manager escalation: %v", err)
	}
}

func TestCaseEscalationStrictReplayRejectsTargetAndAtomicPairForgery(t *testing.T) {
	e := setupTestEnv(t)
	cfg := configureEscalationTestCompany(t, e)
	source := writeTestFile(t, filepath.Join(e.root, "qa-ux", "strict-escalation.md"), "# escalation evidence\n")
	seedFindingAcceptedQualityCase(t, e, cfg, "ESC-STRICT-PARENT", source)
	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, escalationCommand("ESC-STRICT-PARENT", "ESC-STRICT-CHILD", source)...)
	valid := scenarioEvents(t, e, cfg)
	preparedIndex := indexOfEvent(valid, "case_escalation_prepared", 0)
	sentIndex := indexOfEvent(valid, "case_escalation_sent", 0)
	if preparedIndex < 1 || sentIndex < 1 {
		t.Fatal("missing escalation prepared/sent fixture")
	}

	for _, tc := range []struct {
		name string
		edit func([]Event)
		want string
	}{
		{name: "arbitrary target", edit: func(events []Event) {
			events[preparedIndex].Recipient = "zantianyou"
			events[preparedIndex].RecipientLabel = "工程部-詹天佑"
		}, want: "直属上级"},
		{name: "not atomic with child", edit: func(events []Event) {
			events[preparedIndex].CommandID = stableCommandID("forged-escalation", "detached")
		}, want: "同一原子事务"},
		{name: "delivery reason drift", edit: func(events []Event) {
			events[preparedIndex].DeliveryReason = "message-policy"
		}, want: "manager-escalation reason"},
		{name: "sent escalation reason drift", edit: func(events []Event) {
			events[sentIndex].Note = "forged escalation reason"
		}, want: "合同不一致"},
		{name: "spec contract drift", edit: func(events []Event) {
			events[preparedIndex].CaseDigest = digestText("forged-spec")
		}, want: "完整规格合同"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]Event(nil), valid...)
			tc.edit(events)
			rehashEventChain(t, events)
			if _, err := validateLedger(events, cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("strict replay accepted forged escalation: %v", err)
			}
		})
	}
}

func TestConsumedAssignmentReportHintIsCaseScoped(t *testing.T) {
	ledger := newLedgerState()
	ledger.assignments["ISSUE-A"] = &caseAssignment{EventID: "ISSUE-A", CaseID: "CASE-A", Recipient: "worker", Consumed: true}
	if !ledger.hasEverReceivedCaseAssignment("CASE-A", "worker") {
		t.Fatal("same-case consumed assignment was not found")
	}
	if ledger.hasEverReceivedCaseAssignment("CASE-B", "worker") {
		t.Fatal("assignment history from another case leaked into re-report guidance")
	}
}

func TestCaseEscalationUnknownDeliveryRecoversSameDurableChild(t *testing.T) {
	e := setupTestEnv(t)
	cfg := configureEscalationTestCompany(t, e)
	source := writeTestFile(t, filepath.Join(e.root, "qa-ux", "unknown-escalation.md"), "# escalation evidence\n")
	evidence := writeTestFile(t, filepath.Join(e.office, "escalation-delivered.md"), "# Delivery evidence\nThe owner liaison received the escalation.\n")
	seedFindingAcceptedQualityCase(t, e, cfg, "ESC-UNKNOWN-PARENT", source)
	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("simulated timeout after prompt write")
	command := escalationCommand("ESC-UNKNOWN-PARENT", "ESC-UNKNOWN-CHILD", source)
	if err := e.app(t).run(command); err == nil {
		t.Fatal("ambiguous escalation unexpectedly succeeded")
	}
	events := scenarioEvents(t, e, cfg)
	prepared := latestCaseEvent(events, "ESC-UNKNOWN-CHILD", "case_escalation_prepared")
	if prepared.ID == "" || latestCaseEvent(events, "ESC-UNKNOWN-CHILD", "case_escalation_sent").ID != "" {
		t.Fatalf("ambiguous escalation did not freeze one origin: %+v", events)
	}
	state, err := e.app(t).currentCase("ESC-UNKNOWN-CHILD")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != string(statusOpen) || state.Owner != "baogong" {
		t.Fatalf("unknown delivery prematurely handed off child: %+v", state)
	}

	// An operations-authorized liaison resolves the exact delivery after
	// external evidence. This converges the original child; no second case or
	// upward intent is created.
	e.transport.result, e.transport.err = transportSent, nil
	e.setActor(t, "penny", "escalation:owner-liaison", e.office)
	runTestCommand(t, e, "delivery", "resolve", "--id", prepared.DeliveryID, "--outcome", "delivered",
		"--reason", "owner liaison confirmed receipt", "--evidence", evidence)
	state, err = e.app(t).currentCase("ESC-UNKNOWN-CHILD")
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != string(statusEscalated) || state.Owner != "penny" {
		t.Fatalf("resolved escalation did not converge: %+v", state)
	}
	events = scenarioEvents(t, e, cfg)
	if countCaseEvents(events, "ESC-UNKNOWN-CHILD", "case_created") != 1 ||
		countCaseEvents(events, "ESC-UNKNOWN-CHILD", "case_escalation_prepared") != 1 ||
		countCaseEvents(events, "ESC-UNKNOWN-CHILD", "delivery_resolved_sent") != 1 {
		t.Fatalf("recovery duplicated escalation lifecycle: %+v", events)
	}
	if _, err := NewStore(e.data).Rebuild(cfg); err != nil {
		t.Fatalf("strict rebuild rejected resolved escalation: %v", err)
	}
}

func TestCaseEscalationFailedPreSendRetriesSameDurableChild(t *testing.T) {
	e := setupTestEnv(t)
	cfg := configureEscalationTestCompany(t, e)
	source := writeTestFile(t, filepath.Join(e.root, "qa-ux", "retry-escalation.md"), "# escalation retry evidence\n")
	seedFindingAcceptedQualityCase(t, e, cfg, "ESC-RETRY-PARENT", source)
	parentBefore, err := e.app(t).currentCase("ESC-RETRY-PARENT")
	if err != nil {
		t.Fatal(err)
	}

	e.setActor(t, "baogong", "escalation:qa-manager", filepath.Join(e.root, "qa-ux"))
	e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("simulated offline before send")
	if err := e.app(t).run(escalationCommand("ESC-RETRY-PARENT", "ESC-RETRY-CHILD", source)); err == nil {
		t.Fatal("definitely-not-sent escalation unexpectedly succeeded")
	}
	events := scenarioEvents(t, e, cfg)
	prepared := latestCaseEvent(events, "ESC-RETRY-CHILD", "case_escalation_prepared")
	if prepared.ID == "" || latestCaseEvent(events, "ESC-RETRY-CHILD", "case_escalation_sent").ID != "" {
		t.Fatalf("failed-pre-send escalation did not preserve one durable origin: %+v", events)
	}
	child, err := e.app(t).currentCase("ESC-RETRY-CHILD")
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != string(statusOpen) || child.Owner != "baogong" {
		t.Fatalf("failed-pre-send escalation moved child prematurely: %+v", child)
	}

	e.transport.result, e.transport.err = transportSent, nil
	runTestCommand(t, e, "delivery", "retry", "--id", prepared.DeliveryID)
	events = scenarioEvents(t, e, cfg)
	child, err = e.app(t).currentCase("ESC-RETRY-CHILD")
	if err != nil {
		t.Fatal(err)
	}
	if child.Status != string(statusEscalated) || child.Owner != "penny" {
		t.Fatalf("retry did not converge original escalation child: %+v", child)
	}
	if countCaseEvents(events, "ESC-RETRY-CHILD", "case_created") != 1 ||
		countCaseEvents(events, "ESC-RETRY-CHILD", "case_escalation_prepared") != 1 ||
		countCaseEvents(events, "ESC-RETRY-CHILD", "case_escalation_sent") != 1 {
		t.Fatalf("retry duplicated escalation business lifecycle: %+v", events)
	}
	parentAfter, err := e.app(t).currentCase("ESC-RETRY-PARENT")
	if err != nil {
		t.Fatal(err)
	}
	if parentAfter.Status != parentBefore.Status || parentAfter.Owner != parentBefore.Owner ||
		parentAfter.Version != parentBefore.Version || parentAfter.Digest != parentBefore.Digest ||
		parentAfter.LastEventID != parentBefore.LastEventID {
		t.Fatalf("escalation retry rewrote parent: before=%+v after=%+v", parentBefore, parentAfter)
	}
	if _, err := NewStore(e.data).Rebuild(cfg); err != nil {
		t.Fatalf("strict rebuild rejected retried escalation: %v", err)
	}
}

func TestCaseEscalationCLIIsGatewayMutationWithNoArbitraryTarget(t *testing.T) {
	if !shouldUseGateway([]string{"case", "escalate", "--id", "ESC-CLI"}) ||
		!isBusinessMutation([]string{"case", "escalate", "--id", "ESC-CLI"}) {
		t.Fatal("case escalate bypassed the write gateway")
	}
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	command, _, err := root.Find([]string{"case", "escalate"})
	if err != nil {
		t.Fatal(err)
	}
	if command.Flags().Lookup("to") != nil || command.Flags().Lookup("owner") != nil {
		t.Fatal("case escalate exposed an arbitrary target/owner flag")
	}
}
