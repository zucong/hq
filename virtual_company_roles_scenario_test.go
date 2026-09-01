package main

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const roleCompanyProject = "role-card-release"

type roleCompanyAgentSpec struct {
	Name, Nickname, Department, DepartmentLabel, Responsibility, ReportsTo string
	Manager                                                                bool
}

var roleCompanySpecialists = []roleCompanyAgentSpec{
	{Name: "eng-data-engineer", Nickname: "数据工程师", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "data_engineer:engineering", ReportsTo: "zantianyou"},
	{Name: "eng-app-developer", Nickname: "应用开发工程师", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "application_developer:engineering", ReportsTo: "zantianyou"},
	{Name: "eng-security-engineer", Nickname: "安全工程师", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "security_engineer:engineering", ReportsTo: "zantianyou"},
	{Name: "product-researcher", Nickname: "产品调研员", Department: "product", DepartmentLabel: "产品部", Responsibility: "product_researcher:product", ReportsTo: "product-manager"},
	{Name: "qa-browser-blackbox", Nickname: "浏览器黑盒测试员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "browser_blackbox:quality", ReportsTo: "baogong"},
	{Name: "eng-code-reviewer", Nickname: "代码审查员", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "code_reviewer:engineering", ReportsTo: "zantianyou"},
	{Name: "product-copy-reviewer", Nickname: "文案及术语审查员", Department: "product", DepartmentLabel: "产品部", Responsibility: "copy_reviewer:product", ReportsTo: "product-manager"},
	{Name: "qa-data-gate", Nickname: "数据核验与门禁执行员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "data_gate:quality", ReportsTo: "baogong"},
	{Name: "qa-first-use", Nickname: "首次使用体验员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "first_use_tester:quality", ReportsTo: "baogong"},
	{Name: "qa-usability", Nickname: "可用性走查员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "usability_reviewer:quality", ReportsTo: "baogong"},
}

func configureRoleCompanyScenario(t *testing.T, e testEnv) Config {
	t.Helper()
	agents := []AgentRule{
		{Name: "penny", Label: "Penny通报", Nickname: "Penny", DepartmentLabel: "总裁办", Workspace: "hq-test",
			Responsibilities: []string{roleApprovalWitness, roleAccountCloser, "operations_manager"}, Department: "ceo-office", Kind: "codex",
			ActivationPolicy: activationAlways, MaxWIP: 16,
			CanCreate: true, CanIssue: true, CanAccept: true, CanClose: true, CanManageStaff: true, CanReceiveOrder: true},
		{Name: "product-manager", Label: "产品部-产品负责人", Nickname: "产品负责人", DepartmentLabel: "产品部", Workspace: "hq-test",
			Responsibilities: []string{"manager:product"}, Department: "product", ReportsTo: "penny", Kind: "codex",
			ActivationPolicy: activationAlways, MaxWIP: 8, CanCreate: true, CanIssue: true, CanAccept: true, CanReceiveOrder: true},
		{Name: "zantianyou", Label: "工程部-詹天佑", Nickname: "詹天佑", DepartmentLabel: "工程部", Workspace: "hq-test",
			Responsibilities: []string{"manager:engineering"}, Department: "engineering", ReportsTo: "penny", Kind: "codex",
			ActivationPolicy: activationAlways, MaxWIP: 8, CanCreate: true, CanIssue: true, CanAccept: true, CanReceiveOrder: true},
		{Name: "baogong", Label: "质量与用户体验部-包公", Nickname: "包公", DepartmentLabel: "质量与用户体验部", Workspace: "hq-test",
			Responsibilities: []string{"manager:quality"}, Department: "quality", ReportsTo: "penny", Kind: "codex",
			ActivationPolicy: activationAlways, MaxWIP: 8, CanCreate: true, CanIssue: true, CanAccept: true, CanReceiveOrder: true},
	}
	for _, item := range roleCompanySpecialists {
		agents = append(agents, AgentRule{
			Name: item.Name, Label: item.DepartmentLabel + "-" + item.Nickname, Nickname: item.Nickname,
			DepartmentLabel: item.DepartmentLabel, Workspace: "hq-test", Responsibilities: []string{item.Responsibility},
			Department: item.Department, ReportsTo: item.ReportsTo, Kind: "codex",
			ActivationPolicy: activationOnAssignment, MaxWIP: 1, CanAccept: true, CanReceiveOrder: true,
		})
	}
	cfg := Config{Version: registrySchemaVersion, WorkspaceLabel: "hq-test", OwnerPrincipal: "ZC", Agents: agents,
		DeliveryPolicy: &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 100, MaxBundleItems: 8, MaxBundleBytes: defaultDeliveryBundleBytes}}
	for index := range cfg.Agents {
		rule := &cfg.Agents[index]
		rule.PermissionMode = "native"
		rule.RoleCardID, rule.RoleCardVersion, rule.SeatVersion = rule.Name, 1, 1
		rule.WorkstationPath = filepath.Join(rule.Department, "staff", rule.RoleCardID, "v1")
		rule.ManualPath = filepath.Join(rule.WorkstationPath, "AGENTS.md")
		profile := profileForAgent(*rule)
		capabilities, err := canonicalStringSet(profile.Capabilities)
		if err != nil {
			t.Fatalf("role %s capabilities: %v", rule.Name, err)
		}
		manual := agentRoleCardManual("Role Card Scenario Company", cfg.WorkspaceLabel, *rule)
		card := RoleCard{ID: rule.RoleCardID, Version: 1, Label: rule.Nickname, Department: rule.Department,
			Capabilities: capabilities, ManualPath: rule.ManualPath, ManualDigest: roleCardFileDigest(manual), Status: roleCardApproved}
		card.Digest = roleCardDigest(card)
		rule.RoleCardDigest = card.Digest
		rule.SeatDigest = employeeSeatDigest(*rule)
		cfg.RoleCards = append(cfg.RoleCards, card)
		writeTestFile(t, filepath.Join(e.root, rule.ManualPath), string(manual))
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := loadConfig(e.config)
	if err != nil {
		t.Fatalf("ten-role scenario registry invalid: %v", err)
	}
	if err := validateRegistryManuals(loaded, e.root); err != nil {
		t.Fatalf("ten-role scenario manuals invalid: %v", err)
	}
	return loaded
}

func roleCompanyRule(t *testing.T, cfg Config, name string) AgentRule {
	t.Helper()
	rule, ok := cfg.exactRule(name)
	if !ok {
		t.Fatalf("scenario agent missing: %s", name)
	}
	return rule
}

func setRoleCompanyActor(t *testing.T, e testEnv, cfg Config, name string) {
	t.Helper()
	rule := roleCompanyRule(t, cfg, name)
	e.setActor(t, name, "role-company:"+name, filepath.Join(e.root, rule.WorkstationPath))
}

func roleCompanyArtifact(t *testing.T, e testEnv, cfg Config, agent, name, body string) string {
	t.Helper()
	rule := roleCompanyRule(t, cfg, agent)
	return writeTestFile(t, filepath.Join(e.root, rule.WorkstationPath, "artifacts", name), body)
}

func createRoleCompanyChild(t *testing.T, e testEnv, cfg Config, manager, parent, id, title, charter string) {
	t.Helper()
	setRoleCompanyActor(t, e, cfg, manager)
	runTestCommand(t, e, "case", "create", "--id", id, "--parent", parent, "--title", title,
		"--objective", title, "--acceptance", "提交角色专属证据并由直属经理验收",
		"--constraints", "只能在冻结角色卡与 assignment 边界内执行", "--priority", "P1", "--source", charter)
}

func quietRoleCompanyHandoff(t *testing.T, e testEnv, cfg Config, sender, target, caseID, marker, refFile string) {
	t.Helper()
	setRoleCompanyActor(t, e, cfg, sender)
	runTestCommand(t, e, "message", "--to", target, "--kind", "handoff", "--case", caseID,
		"--text", marker, "--ref-file", refFile, "--ref-case", caseID, "--delivery", "quiet")
}

func issueRoleCompanyCase(t *testing.T, e testEnv, cfg Config, caseID, target, next string, extra []string, bundleMarkers ...string) Event {
	t.Helper()
	beforeCalls := len(e.transport.calls)
	args := []string{"issue", "--case", caseID, "--to", target, "--next", next}
	args = append(args, extra...)
	runTestCommand(t, e, args...)
	if len(e.transport.calls) != beforeCalls+1 {
		t.Fatalf("issue %s transport calls=%d->%d, want exactly one doorbell", caseID, beforeCalls, len(e.transport.calls))
	}
	events := scenarioEvents(t, e, cfg)
	issue := latestCaseEvent(events, caseID, "issue_sent")
	if issue.ID == "" {
		t.Fatalf("issue %s did not reach issue_sent", caseID)
	}
	rule := roleCompanyRule(t, cfg, target)
	if issue.Recipient != target || issue.AssigneeSeatVersion != rule.SeatVersion || issue.AssigneeSeatDigest != rule.SeatDigest ||
		issue.RoleCardID != rule.RoleCardID || issue.RoleCardVersion != rule.RoleCardVersion ||
		issue.RoleCardDigest != rule.RoleCardDigest || issue.RoleCardManualPath != rule.ManualPath ||
		issue.AssignmentDigest != assignmentContractDigest(issue) {
		t.Fatalf("issue %s did not freeze selected seat/card: issue=%+v rule=%+v", caseID, issue, rule)
	}
	prompt := e.transport.calls[len(e.transport.calls)-1].message
	for _, fragment := range []string{
		"ROLE_CARD=" + rule.RoleCardID + "@1", "ROLE_DIGEST=" + rule.RoleCardDigest,
		"MANUAL=" + rule.ManualPath, fmt.Sprintf("SEAT_VERSION=%d", rule.SeatVersion),
		"SEAT_DIGEST=" + rule.SeatDigest, "ASSIGNMENT=" + issue.AssignmentID,
		"hq accept --event " + issue.RelatedEventID,
	} {
		if !strings.Contains(prompt, fragment) {
			t.Fatalf("issue %s prompt missing %q: %s", caseID, fragment, prompt)
		}
	}
	if strings.Contains(prompt, "行为锚点") {
		t.Fatalf("issue %s copied role prose instead of stable manual pointer: %s", caseID, prompt)
	}
	lastIndex := -1
	for _, marker := range bundleMarkers {
		index := strings.Index(prompt, marker)
		if index < 0 || index <= lastIndex {
			t.Fatalf("issue %s bundle missing or reordered %q: %s", caseID, marker, prompt)
		}
		lastIndex = index
	}
	for _, marker := range bundleMarkers {
		var message Event
		for _, candidate := range events {
			if candidate.Type == "message_prepared" && candidate.Recipient == target && candidate.Message == marker {
				message = candidate
			}
		}
		if message.MessageID == "" {
			t.Fatalf("issue %s bundle marker %q has no message origin", caseID, marker)
		}
		ackCommand := "hq message ack --message " + message.MessageID
		if !strings.Contains(prompt, ackCommand) {
			t.Fatalf("issue %s actionable bundle lacks ack command %q: %s", caseID, ackCommand, prompt)
		}
		setRoleCompanyActor(t, e, cfg, target)
		runTestCommand(t, e, "message", "ack", "--message", message.MessageID)
	}
	return issue
}

func completeRoleCompanyChild(t *testing.T, e testEnv, cfg Config, issue Event, worker, manager, artifact, verification string) Event {
	t.Helper()
	setRoleCompanyActor(t, e, cfg, worker)
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Execute the frozen specialist assignment")
	runTestCommand(t, e, "report", "--case", issue.CaseID, "--result", "completed", "--artifact", artifact,
		"--verify", verification, "--next", "Direct manager review")
	report := latestCaseEvent(scenarioEvents(t, e, cfg), issue.CaseID, "report_sent")
	if report.ID == "" || report.AssignmentEventID != issue.ID || report.AssignmentDigest != issue.AssignmentDigest ||
		report.AssigneeSeatVersion != issue.AssigneeSeatVersion || report.AssigneeSeatDigest != issue.AssigneeSeatDigest ||
		report.RoleCardID != issue.RoleCardID || report.RoleCardVersion != issue.RoleCardVersion ||
		report.RoleCardDigest != issue.RoleCardDigest || report.RoleCardManualPath != issue.RoleCardManualPath {
		t.Fatalf("worker %s report lost frozen assignment/card/seat: %+v", worker, report)
	}
	setRoleCompanyActor(t, e, cfg, manager)
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "Accepted into department evidence")
	return report
}

func reportRoleCompanyRoot(t *testing.T, e testEnv, cfg Config, manager, caseID, artifact, verification string) Event {
	t.Helper()
	setRoleCompanyActor(t, e, cfg, manager)
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact,
		"--verify", verification, "--next", "President office review")
	report := latestCaseEvent(scenarioEvents(t, e, cfg), caseID, "report_sent")
	if report.ID == "" {
		t.Fatalf("root %s did not report", caseID)
	}
	setRoleCompanyActor(t, e, cfg, "penny")
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "Accepted into company release evidence")
	return report
}

// TestVirtualCompanyTenRoleProjectScenario uses a deterministic fake Herdr
// transport. It validates the HQ organization and protocol closure, not the
// cognitive quality of a live model. Ten distinct role-card seats execute one
// project through three departments, evidence handoffs, rework, WIP admission,
// account closure, and strict ledger rebuild.
func TestVirtualCompanyTenRoleProjectScenario(t *testing.T) {
	e := setupTestEnv(t)
	cfg := configureRoleCompanyScenario(t, e)
	charter := writeTestFile(t, filepath.Join(e.office, "role-company-charter.md"), "# Role Card Release\nShip an evidence-backed onboarding product through Product, Engineering, and Quality.\n")
	productOrder := writeTestFile(t, filepath.Join(e.office, "role-product-order.md"), "# Product order\nResearch users and freeze language.\n")
	engineeringOrder := writeTestFile(t, filepath.Join(e.office, "role-engineering-order.md"), "# Engineering order\nBuild a secure and reversible release.\n")
	qualityOrder := writeTestFile(t, filepath.Join(e.office, "role-quality-order.md"), "# Quality order\nIndependently validate user experience and release evidence.\n")
	productDecision := writeIssueApproval(t, e, "role-product-decision.md", "DEC-ROLE-PRODUCT", "ROLE-PRODUCT", productOrder, "product-manager")
	engineeringDecision := writeIssueApproval(t, e, "role-engineering-decision.md", "DEC-ROLE-ENGINEERING", "ROLE-ENGINEERING", engineeringOrder, "zantianyou")
	qualityDecision := writeIssueApproval(t, e, "role-quality-decision.md", "DEC-ROLE-QUALITY", "ROLE-QUALITY", qualityOrder, "baogong")

	artifacts := map[string]string{
		"research":    roleCompanyArtifact(t, e, cfg, "product-researcher", "research-findings.md", "# Research\nObserved onboarding risks, counterexamples, sample limits, and measurable success criteria.\n"),
		"copy":        roleCompanyArtifact(t, e, cfg, "product-copy-reviewer", "terminology-review.md", "# Terminology\nApproved term map, UI copy diff, ambiguity decisions, and remaining product questions.\n"),
		"data":        roleCompanyArtifact(t, e, cfg, "eng-data-engineer", "data-contract.md", "# Data Contract\nSchema, lineage, late-data cases, quality checks, and replay evidence.\n"),
		"app-v1":      roleCompanyArtifact(t, e, cfg, "eng-app-developer", "application-v1.md", "# Application v1\nHappy path passes; rollback evidence is missing.\n"),
		"app-v2":      roleCompanyArtifact(t, e, cfg, "eng-app-developer", "application-v2.md", "# Application v2\nHappy path, migration failure, and rollback all pass.\n"),
		"security":    roleCompanyArtifact(t, e, cfg, "eng-security-engineer", "security-review.md", "# Security\nThreat model, authorization checks, reproduced findings, and residual risk.\n"),
		"code-review": roleCompanyArtifact(t, e, cfg, "eng-code-reviewer", "code-review.md", "# Code Review\nDiff-level correctness review, regression probes, and blocking items resolved.\n"),
		"first-use":   roleCompanyArtifact(t, e, cfg, "qa-first-use", "first-use-observations.md", "# First Use\nClean-room timeline, hesitations, failures, and time to first success.\n"),
		"browser":     roleCompanyArtifact(t, e, cfg, "qa-browser-blackbox", "browser-blackbox.md", "# Browser Black Box\nPublic-UI reproduction matrix across browsers, refresh, back, and network failure.\n"),
		"usability":   roleCompanyArtifact(t, e, cfg, "qa-usability", "usability-walkthrough.md", "# Usability\nCritical-path walkthrough, decision points, recovery, and severity-ranked findings.\n"),
		"gate":        roleCompanyArtifact(t, e, cfg, "qa-data-gate", "release-gate.md", "# Release Gate\nAll acceptance rows independently mapped, recalculated, and passed.\n"),
	}
	productSummary := roleCompanyArtifact(t, e, cfg, "product-manager", "product-summary.md", "# Product Summary\nResearch and terminology evidence accepted.\n")
	engineeringSummary := roleCompanyArtifact(t, e, cfg, "zantianyou", "engineering-summary.md", "# Engineering Summary\nData, application, security, code review, and rollback evidence accepted.\n")
	qualitySummary := roleCompanyArtifact(t, e, cfg, "baogong", "quality-summary.md", "# Quality Summary\nFirst-use, browser, usability, and independent gate evidence accepted.\n")

	// Penny creates one company project root, then the three accountable
	// department workstreams beneath it.
	setRoleCompanyActor(t, e, cfg, "penny")
	runTestCommand(t, e, "case", "create", "--id", "ROLE-COMPANY", "--title", "Ship the evidence-backed company release", "--project", roleCompanyProject,
		"--objective", "Coordinate product, engineering, and quality into one releasable result", "--acceptance", "All department evidence accepted by Penny", "--constraints", "Use only approved role-card seats",
		"--priority", "P0", "--source", charter)
	for _, root := range []struct{ id, title string }{
		{"ROLE-PRODUCT", "Define evidence-backed onboarding product"},
		{"ROLE-ENGINEERING", "Build secure reversible onboarding release"},
		{"ROLE-QUALITY", "Independently validate release usability and evidence"},
	} {
		runTestCommand(t, e, "case", "create", "--id", root.id, "--parent", "ROLE-COMPANY", "--title", root.title,
			"--objective", root.title, "--acceptance", "Department evidence accepted by Penny", "--constraints", "Use only approved role-card seats",
			"--priority", "P0", "--source", charter)
	}
	productRootIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-PRODUCT", "product-manager", "Decompose product discovery",
		[]string{"--decision", productDecision})
	setRoleCompanyActor(t, e, cfg, "product-manager")
	runTestCommand(t, e, "accept", "--event", productRootIssue.ID, "--next", "Delegate research and terminology review")

	// Product: research evidence becomes a quiet FIFO handoff in the copy
	// reviewer's first assignment turn.
	createRoleCompanyChild(t, e, cfg, "product-manager", "ROLE-PRODUCT", "ROLE-PRODUCT-RESEARCH", "Research onboarding users", charter)
	setRoleCompanyActor(t, e, cfg, "product-manager")
	researchIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-PRODUCT-RESEARCH", "product-researcher", "Collect falsifiable user evidence", nil)
	completeRoleCompanyChild(t, e, cfg, researchIssue, "product-researcher", "product-manager", artifacts["research"], "sample, counterexamples, and measurable risks present")

	createRoleCompanyChild(t, e, cfg, "product-manager", "ROLE-PRODUCT", "ROLE-PRODUCT-COPY", "Review copy and terminology", charter)
	quietRoleCompanyHandoff(t, e, cfg, "product-manager", "product-copy-reviewer", "ROLE-PRODUCT", "HANDOFF_PRODUCT_RESEARCH", artifacts["research"])
	setRoleCompanyActor(t, e, cfg, "product-manager")
	copyIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-PRODUCT-COPY", "product-copy-reviewer", "Apply one-concept-one-term review", nil, "HANDOFF_PRODUCT_RESEARCH")
	completeRoleCompanyChild(t, e, cfg, copyIssue, "product-copy-reviewer", "product-manager", artifacts["copy"], "terminology diff and ambiguity decisions present")
	quietRoleCompanyHandoff(t, e, cfg, "product-manager", "zantianyou", "ROLE-PRODUCT", "HANDOFF_PRODUCT_APPROVED", productSummary)
	reportRoleCompanyRoot(t, e, cfg, "product-manager", "ROLE-PRODUCT", productSummary, "research and terminology child evidence accepted")

	// Engineering starts only after Product is accepted; the cross-department
	// quiet handoff must ride in the root assignment Turn Bundle.
	setRoleCompanyActor(t, e, cfg, "penny")
	engineeringRootIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-ENGINEERING", "zantianyou", "Build against approved product evidence",
		[]string{"--decision", engineeringDecision}, "HANDOFF_PRODUCT_APPROVED")
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	runTestCommand(t, e, "accept", "--event", engineeringRootIssue.ID, "--next", "Delegate data, application, security, and review")

	createRoleCompanyChild(t, e, cfg, "zantianyou", "ROLE-ENGINEERING", "ROLE-ENG-DATA", "Define release data contract", charter)
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	dataIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-ENG-DATA", "eng-data-engineer", "Prove lineage, schema, and replay", nil)
	completeRoleCompanyChild(t, e, cfg, dataIssue, "eng-data-engineer", "zantianyou", artifacts["data"], "schema, lineage, quality, and replay checks present")

	createRoleCompanyChild(t, e, cfg, "zantianyou", "ROLE-ENGINEERING", "ROLE-ENG-APP", "Implement onboarding application", charter)
	createRoleCompanyChild(t, e, cfg, "zantianyou", "ROLE-ENGINEERING", "ROLE-ENG-SECURITY", "Threat-model onboarding application", charter)
	quietRoleCompanyHandoff(t, e, cfg, "zantianyou", "eng-app-developer", "ROLE-ENG-APP", "HANDOFF_DATA_CONTRACT", artifacts["data"])
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	appIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-ENG-APP", "eng-app-developer", "Implement, test, and prove rollback", nil, "HANDOFF_DATA_CONTRACT")
	setRoleCompanyActor(t, e, cfg, "eng-app-developer")
	runTestCommand(t, e, "accept", "--event", appIssue.ID, "--next", "Implement against frozen data contract")

	// One active application assignment must consume the seat's entire WIP.
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	beforeWIP := scenarioEvents(t, e, cfg)
	beforeWIPCalls := len(e.transport.calls)
	wipErr := runAssignmentCommandError(t, e, "issue", "--case", "ROLE-ENG-SECURITY", "--to", "eng-app-developer", "--next", "This second assignment must fail")
	if wipErr == nil || !strings.Contains(wipErr.Error(), "max_wip=1") {
		t.Fatalf("active application seat escaped WIP=1: %v", wipErr)
	}
	if after := scenarioEvents(t, e, cfg); len(after) != len(beforeWIP) || len(e.transport.calls) != beforeWIPCalls {
		t.Fatalf("WIP rejection had side effects: events=%d->%d transport=%d->%d", len(beforeWIP), len(after), beforeWIPCalls, len(e.transport.calls))
	}

	// The first app submission is returned. Rework remains on the exact same
	// assignment, seat digest, and role-card version.
	setRoleCompanyActor(t, e, cfg, "eng-app-developer")
	runTestCommand(t, e, "report", "--case", "ROLE-ENG-APP", "--result", "completed", "--artifact", artifacts["app-v1"],
		"--verify", "happy path passes", "--next", "Engineering manager review")
	firstAppReport := latestCaseEvent(scenarioEvents(t, e, cfg), "ROLE-ENG-APP", "report_sent")
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	runTestCommand(t, e, "return", "--event", firstAppReport.ID, "--reason", "缺少 migration failure 与 rollback 证据", "--next", "补齐失败迁移和 rollback 测试后复交")
	setRoleCompanyActor(t, e, cfg, "eng-app-developer")
	runTestCommand(t, e, "report", "--case", "ROLE-ENG-APP", "--result", "completed", "--artifact", artifacts["app-v2"],
		"--verify", "happy path, migration failure, and rollback pass", "--next", "Engineering manager re-review")
	secondAppReport := latestCaseEvent(scenarioEvents(t, e, cfg), "ROLE-ENG-APP", "report_sent")
	if secondAppReport.AssignmentEventID != appIssue.ID || secondAppReport.AssignmentID != appIssue.AssignmentID ||
		secondAppReport.AssignmentDigest != appIssue.AssignmentDigest || secondAppReport.AssigneeSeatVersion != appIssue.AssigneeSeatVersion ||
		secondAppReport.AssigneeSeatDigest != appIssue.AssigneeSeatDigest || secondAppReport.RoleCardID != appIssue.RoleCardID ||
		secondAppReport.RoleCardVersion != appIssue.RoleCardVersion || secondAppReport.RoleCardDigest != appIssue.RoleCardDigest ||
		secondAppReport.RoleCardManualPath != appIssue.RoleCardManualPath {
		t.Fatalf("application rework changed frozen assignment/card/seat: issue=%+v report=%+v", appIssue, secondAppReport)
	}
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	runTestCommand(t, e, "accept", "--event", secondAppReport.ID, "--next", "Application evidence accepted")

	quietRoleCompanyHandoff(t, e, cfg, "zantianyou", "eng-security-engineer", "ROLE-ENG-SECURITY", "HANDOFF_SECURITY_DATA", artifacts["data"])
	quietRoleCompanyHandoff(t, e, cfg, "zantianyou", "eng-security-engineer", "ROLE-ENG-SECURITY", "HANDOFF_SECURITY_APP", artifacts["app-v2"])
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	securityIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-ENG-SECURITY", "eng-security-engineer", "Threat-model and reproduce security risks", nil,
		"HANDOFF_SECURITY_DATA", "HANDOFF_SECURITY_APP")
	completeRoleCompanyChild(t, e, cfg, securityIssue, "eng-security-engineer", "zantianyou", artifacts["security"], "threat model, reproductions, and residual risk present")

	createRoleCompanyChild(t, e, cfg, "zantianyou", "ROLE-ENGINEERING", "ROLE-ENG-CODE-REVIEW", "Independently review implementation diff", charter)
	quietRoleCompanyHandoff(t, e, cfg, "zantianyou", "eng-code-reviewer", "ROLE-ENG-CODE-REVIEW", "HANDOFF_REVIEW_APP", artifacts["app-v2"])
	quietRoleCompanyHandoff(t, e, cfg, "zantianyou", "eng-code-reviewer", "ROLE-ENG-CODE-REVIEW", "HANDOFF_REVIEW_SECURITY", artifacts["security"])
	setRoleCompanyActor(t, e, cfg, "zantianyou")
	codeReviewIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-ENG-CODE-REVIEW", "eng-code-reviewer", "Review diff, invariants, and regression risk", nil,
		"HANDOFF_REVIEW_APP", "HANDOFF_REVIEW_SECURITY")
	completeRoleCompanyChild(t, e, cfg, codeReviewIssue, "eng-code-reviewer", "zantianyou", artifacts["code-review"], "findings include locations, triggers, and regression probes")
	quietRoleCompanyHandoff(t, e, cfg, "zantianyou", "baogong", "ROLE-ENGINEERING", "HANDOFF_ENGINEERING_APPROVED", engineeringSummary)
	reportRoleCompanyRoot(t, e, cfg, "zantianyou", "ROLE-ENGINEERING", engineeringSummary, "four engineering specialist submissions accepted, including rollback rework")

	// Quality preserves a clean first-use role, then combines independent
	// observations for usability and the final evidence gate.
	setRoleCompanyActor(t, e, cfg, "penny")
	qualityRootIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-QUALITY", "baogong", "Run independent user and release gates",
		[]string{"--decision", qualityDecision}, "HANDOFF_ENGINEERING_APPROVED")
	setRoleCompanyActor(t, e, cfg, "baogong")
	runTestCommand(t, e, "accept", "--event", qualityRootIssue.ID, "--next", "Delegate clean-room and evidence validation")

	createRoleCompanyChild(t, e, cfg, "baogong", "ROLE-QUALITY", "ROLE-QA-FIRST-USE", "Observe uncontaminated first use", charter)
	setRoleCompanyActor(t, e, cfg, "baogong")
	firstUseIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-QA-FIRST-USE", "qa-first-use", "Start clean; do not read internal implementation", nil)
	completeRoleCompanyChild(t, e, cfg, firstUseIssue, "qa-first-use", "baogong", artifacts["first-use"], "clean-room setup, timeline, and first-success time present")

	createRoleCompanyChild(t, e, cfg, "baogong", "ROLE-QUALITY", "ROLE-QA-BROWSER", "Run public-browser black-box matrix", charter)
	quietRoleCompanyHandoff(t, e, cfg, "baogong", "qa-browser-blackbox", "ROLE-QA-BROWSER", "HANDOFF_PUBLIC_BUILD", engineeringSummary)
	setRoleCompanyActor(t, e, cfg, "baogong")
	browserIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-QA-BROWSER", "qa-browser-blackbox", "Use only public UI and observable browser evidence", nil, "HANDOFF_PUBLIC_BUILD")
	completeRoleCompanyChild(t, e, cfg, browserIssue, "qa-browser-blackbox", "baogong", artifacts["browser"], "browser matrix and reproducible public-UI evidence present")

	createRoleCompanyChild(t, e, cfg, "baogong", "ROLE-QUALITY", "ROLE-QA-USABILITY", "Walk critical usability paths", charter)
	quietRoleCompanyHandoff(t, e, cfg, "baogong", "qa-usability", "ROLE-QA-USABILITY", "HANDOFF_FIRST_USE", artifacts["first-use"])
	quietRoleCompanyHandoff(t, e, cfg, "baogong", "qa-usability", "ROLE-QA-USABILITY", "HANDOFF_BROWSER_BLACKBOX", artifacts["browser"])
	setRoleCompanyActor(t, e, cfg, "baogong")
	usabilityIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-QA-USABILITY", "qa-usability", "Walk end-to-end task and recovery paths", nil,
		"HANDOFF_FIRST_USE", "HANDOFF_BROWSER_BLACKBOX")
	completeRoleCompanyChild(t, e, cfg, usabilityIssue, "qa-usability", "baogong", artifacts["usability"], "critical paths, decision points, and severity-ranked findings present")

	createRoleCompanyChild(t, e, cfg, "baogong", "ROLE-QUALITY", "ROLE-QA-DATA-GATE", "Run independent release evidence gate", charter)
	for _, handoff := range []struct{ marker, artifact string }{
		{"HANDOFF_GATE_PRODUCT", productSummary}, {"HANDOFF_GATE_DATA", artifacts["data"]},
		{"HANDOFF_GATE_SECURITY", artifacts["security"]}, {"HANDOFF_GATE_USABILITY", artifacts["usability"]},
	} {
		quietRoleCompanyHandoff(t, e, cfg, "baogong", "qa-data-gate", "ROLE-QA-DATA-GATE", handoff.marker, handoff.artifact)
	}
	setRoleCompanyActor(t, e, cfg, "baogong")
	gateIssue := issueRoleCompanyCase(t, e, cfg, "ROLE-QA-DATA-GATE", "qa-data-gate", "Fail closed unless every acceptance row has evidence", nil,
		"HANDOFF_GATE_PRODUCT", "HANDOFF_GATE_DATA", "HANDOFF_GATE_SECURITY", "HANDOFF_GATE_USABILITY")
	setRoleCompanyActor(t, e, cfg, "qa-data-gate")
	if consumed := runTestCommand(t, e, "delivery", "consume"); !strings.Contains(consumed, "无静默消息") {
		t.Fatalf("Turn Bundle left claimed gate context behind: %s", consumed)
	}
	completeRoleCompanyChild(t, e, cfg, gateIssue, "qa-data-gate", "baogong", artifacts["gate"], "acceptance matrix independently mapped and recalculated")
	reportRoleCompanyRoot(t, e, cfg, "baogong", "ROLE-QUALITY", qualitySummary, "four independent quality specialist submissions accepted")

	workerIssues := map[string]Event{
		"ROLE-PRODUCT-RESEARCH": researchIssue, "ROLE-PRODUCT-COPY": copyIssue,
		"ROLE-ENG-DATA": dataIssue, "ROLE-ENG-APP": appIssue, "ROLE-ENG-SECURITY": securityIssue, "ROLE-ENG-CODE-REVIEW": codeReviewIssue,
		"ROLE-QA-FIRST-USE": firstUseIssue, "ROLE-QA-BROWSER": browserIssue, "ROLE-QA-USABILITY": usabilityIssue, "ROLE-QA-DATA-GATE": gateIssue,
	}
	if len(workerIssues) != 10 {
		t.Fatalf("worker assignments=%d, want 10", len(workerIssues))
	}
	ledgerBeforeClose, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	recipients, roleCards, manuals := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for caseID, issue := range workerIssues {
		if state := ledgerBeforeClose.snapshot.Cases[caseID]; state == nil || state.Project != roleCompanyProject {
			t.Fatalf("worker child %s did not inherit project %q: %+v", caseID, roleCompanyProject, state)
		}
		assignment := ledgerBeforeClose.assignments[issue.ID]
		if assignment == nil || assignment.Status != "completed" || !assignment.Consumed {
			t.Fatalf("worker assignment %s not completed: %+v", caseID, assignment)
		}
		recipients[issue.Recipient], roleCards[issue.RoleCardID], manuals[issue.RoleCardManualPath] = true, true, true
	}
	if len(recipients) != 10 || len(roleCards) != 10 || len(manuals) != 10 {
		t.Fatalf("role selection not one-to-one: recipients=%d cards=%d manuals=%d", len(recipients), len(roleCards), len(manuals))
	}
	if len(ledgerBeforeClose.assignments) != 13 {
		t.Fatalf("assignments=%d, want 3 root + 10 specialist", len(ledgerBeforeClose.assignments))
	}
	for deliveryID, record := range ledgerBeforeClose.deliveries {
		if record != nil && record.Origin.Type == "message_prepared" && messageNeedsAction(record.Origin.MessageKind) && record.Ack.ID == "" {
			t.Fatalf("actionable company handoff would permanently block runtime reap: delivery=%s message=%s recipient=%s", deliveryID, record.Origin.MessageID, record.Origin.Recipient)
		}
	}

	// Penny closes in post-order. Project View and a strict rebuild must retain
	// the same 13 closed cases and every frozen role/seat binding.
	setRoleCompanyActor(t, e, cfg, "penny")
	beforeRejectedClose, _ := projectClosureEvents(t, e)
	_, closeErr := runProjectTestCommand(t, e, false, "close", "--case", "ROLE-PRODUCT", "--reason", "Attempt parent-first close", "--source", charter)
	if closeErr == nil || !strings.Contains(closeErr.Error(), "status=accepted") ||
		!strings.Contains(closeErr.Error(), "hq close --case ROLE-PRODUCT-RESEARCH") ||
		!strings.Contains(closeErr.Error(), "hq close --case ROLE-PRODUCT-COPY") {
		t.Fatalf("parent-first close did not explain accepted descendants and executable post-order: %v", closeErr)
	}
	afterRejectedClose, _ := projectClosureEvents(t, e)
	if !reflect.DeepEqual(beforeRejectedClose, afterRejectedClose) {
		t.Fatalf("rejected parent-first close changed authoritative ledger: before=%d after=%d", len(beforeRejectedClose), len(afterRejectedClose))
	}
	childCases := []string{"ROLE-PRODUCT-RESEARCH", "ROLE-PRODUCT-COPY", "ROLE-ENG-DATA", "ROLE-ENG-APP", "ROLE-ENG-SECURITY", "ROLE-ENG-CODE-REVIEW", "ROLE-QA-FIRST-USE", "ROLE-QA-BROWSER", "ROLE-QA-USABILITY", "ROLE-QA-DATA-GATE"}
	for _, caseID := range append(childCases, "ROLE-PRODUCT", "ROLE-ENGINEERING", "ROLE-QUALITY", "ROLE-COMPANY") {
		runTestCommand(t, e, "close", "--case", caseID, "--reason", "Role-specific evidence accepted in company release review", "--source", charter)
	}
	project := runTestCommand(t, e, "project", "show", "--project", roleCompanyProject)
	for _, fragment := range []string{"status=closed", "roots=1", "total=14", "closure_gap=0"} {
		if !strings.Contains(project, fragment) {
			t.Fatalf("closed role-card project missing %q: %s", fragment, project)
		}
	}
	for _, caseID := range append(append([]string(nil), childCases...), "ROLE-PRODUCT", "ROLE-ENGINEERING", "ROLE-QUALITY", "ROLE-COMPANY") {
		if !strings.Contains(project, caseID) {
			t.Fatalf("Project View omitted inherited child/root case %s: %s", caseID, project)
		}
	}
	before, err := NewStore(e.data).Snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(e.data, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("delete derived state before role scenario rebuild: %v", err)
	}
	rebuilt, err := NewStore(e.data).Rebuild(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if before.EventCount != rebuilt.EventCount || before.LastSequence != rebuilt.LastSequence || before.LastEventHash != rebuilt.LastEventHash || len(rebuilt.Cases) != 14 {
		t.Fatalf("role scenario rebuild diverged: before=%+v rebuilt=%+v", before, rebuilt)
	}
	for caseID, state := range rebuilt.Cases {
		if state.Status != string(statusClosed) {
			t.Fatalf("rebuilt case %s not closed: %+v", caseID, state)
		}
	}
	ledgerAfterRebuild, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	for caseID, issue := range workerIssues {
		assignment := ledgerAfterRebuild.assignments[issue.ID]
		if assignment == nil || assignment.Status != "completed" || assignment.AssigneeSeatVersion != issue.AssigneeSeatVersion ||
			assignment.AssigneeSeatDigest != issue.AssigneeSeatDigest || assignment.RoleCardID != issue.RoleCardID ||
			assignment.RoleCardVersion != issue.RoleCardVersion || assignment.RoleCardDigest != issue.RoleCardDigest ||
			assignment.RoleCardManualPath != issue.RoleCardManualPath || assignment.AssignmentDigest != issue.AssignmentDigest {
			t.Fatalf("rebuilt assignment %s lost frozen role/seat fields: issue=%+v assignment=%+v", caseID, issue, assignment)
		}
	}
}
