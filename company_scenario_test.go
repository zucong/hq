package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func configureVirtualCompanyScenario(t *testing.T, e testEnv) Config {
	t.Helper()
	cfg := testConfig()
	cfg.Agents = append(cfg.Agents,
		AgentRule{
			Name: "product-manager", Label: "产品部-张衡", Nickname: "张衡", DepartmentLabel: "产品部",
			Workspace: cfg.WorkspaceLabel, Responsibilities: []string{"manager:product"},
			ManualPath: "product/AGENTS.md", Department: "product", ReportsTo: "penny", Kind: "codex",
			CanCreate: true, CanAccept: true, CanReceiveOrder: true,
		},
		AgentRule{
			Name: "product-researcher", Label: "产品部-沈括", Nickname: "沈括", DepartmentLabel: "产品部",
			Workspace: cfg.WorkspaceLabel, Responsibilities: []string{"researcher:product"},
			ManualPath: "product/AGENTS.md", Department: "product", ReportsTo: "product-manager", Kind: "codex",
			CanCreate: true, CanAccept: true, CanReceiveOrder: true,
		},
	)
	cfg = bindTestRoleContracts(cfg)
	for _, rule := range cfg.Agents {
		writeTestFile(t, filepath.Join(e.root, rule.ManualPath), string(testRoleManual(rule.Name)))
	}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(e.config); err != nil {
		t.Fatalf("scenario organization invalid: %v", err)
	}
	return cfg
}

func latestCaseEvent(events []Event, caseID, eventType string) Event {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].CaseID == caseID && events[index].Type == eventType {
			return events[index]
		}
	}
	return Event{}
}

func scenarioEvents(t *testing.T, e testEnv, cfg Config) []Event {
	t.Helper()
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

// TestVirtualCompanyHeadquartersLaunchScenario is a deterministic simulation
// of a real project executed by the president's office, Product, and
// Engineering. It exercises hierarchical delegation, peer communication,
// evidence review, rejection/rework, frozen return routing, project closure,
// and strict rebuild from the same durable ledger.
func TestVirtualCompanyHeadquartersLaunchScenario(t *testing.T) {
	e := setupTestEnv(t)
	cfg := configureVirtualCompanyScenario(t, e)
	charter := writeTestFile(t, filepath.Join(e.office, "launch-charter.md"), "# Virtual Company Launch\nShip an evidence-backed onboarding release.\n")
	productOrder := writeTestFile(t, filepath.Join(e.office, "product-order.md"), "# Product order\n")
	engineeringOrder := writeTestFile(t, filepath.Join(e.office, "engineering-order.md"), "# Engineering order\n")
	productDecision := writeIssueApproval(t, e, "launch-product.md", "DEC-LAUNCH-PRODUCT", "LAUNCH-PRODUCT", productOrder, "product-manager")
	engineeringDecision := writeIssueApproval(t, e, "launch-engineering.md", "DEC-LAUNCH-ENGINEERING", "LAUNCH-ENGINEERING", engineeringOrder, "zantianyou")
	productEvidence := writeTestFile(t, filepath.Join(e.root, "product", "research.md"), "# User research\nThree onboarding risks and acceptance metrics.\n")
	productBrief := writeTestFile(t, filepath.Join(e.root, "product", "brief.md"), "# Product brief\nApproved scope and success metrics.\n")
	engineeringArtifactV1 := writeTestFile(t, filepath.Join(e.root, "engineering", "implementation-v1.md"), "# Implementation v1\nMissing rollback proof.\n")
	engineeringArtifactV2 := writeTestFile(t, filepath.Join(e.root, "engineering", "implementation-v2.md"), "# Implementation v2\nRollback and verification evidence.\n")
	engineeringSummary := writeTestFile(t, filepath.Join(e.root, "engineering", "release-summary.md"), "# Engineering release\nValidated implementation.\n")

	// President's office creates the company's sole project root, then two
	// accountable department workstreams beneath it.
	e.setActor(t, "penny", "scenario:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", "LAUNCH-COMPANY", "--title", "Ship evidence-backed onboarding release", "--project", "virtual-company-launch", "--priority", "P0", "--source", charter)
	runTestCommand(t, e, "case", "create", "--id", "LAUNCH-PRODUCT", "--parent", "LAUNCH-COMPANY", "--title", "Define onboarding product", "--priority", "P0", "--source", charter)
	runTestCommand(t, e, "case", "create", "--id", "LAUNCH-ENGINEERING", "--parent", "LAUNCH-COMPANY", "--title", "Build onboarding release", "--priority", "P0", "--source", charter)
	runTestCommand(t, e, "issue", "--case", "LAUNCH-PRODUCT", "--to", "product-manager", "--decision", productDecision, "--next", "Research, define scope, and return evidence")
	events := scenarioEvents(t, e, cfg)
	productIssue := latestCaseEvent(events, "LAUNCH-PRODUCT", "issue_sent")

	// Product manager accepts accountability, decomposes work, and delegates
	// only to a direct report. The researcher returns evidence to the frozen
	// acceptor recorded by that child assignment.
	e.setActor(t, "product-manager", "scenario:product-manager", filepath.Join(e.root, "product"))
	runTestCommand(t, e, "accept", "--event", productIssue.ID, "--next", "Decompose research")
	runTestCommand(t, e, "case", "create", "--id", "LAUNCH-PRODUCT-RESEARCH", "--parent", "LAUNCH-PRODUCT", "--title", "Research onboarding friction", "--priority", "P1", "--source", charter)
	runTestCommand(t, e, "issue", "--case", "LAUNCH-PRODUCT-RESEARCH", "--to", "product-researcher", "--next", "Interview users and document measurable risks")
	events = scenarioEvents(t, e, cfg)
	productResearchIssue := latestCaseEvent(events, "LAUNCH-PRODUCT-RESEARCH", "issue_sent")

	e.setActor(t, "product-researcher", "scenario:product-researcher", filepath.Join(e.root, "product"))
	runTestCommand(t, e, "accept", "--event", productResearchIssue.ID, "--next", "Run research")
	runTestCommand(t, e, "report", "--case", "LAUNCH-PRODUCT-RESEARCH", "--result", "completed", "--artifact", productEvidence, "--verify", "metrics and risks are present", "--next", "Manager review")
	events = scenarioEvents(t, e, cfg)
	productResearchReport := latestCaseEvent(events, "LAUNCH-PRODUCT-RESEARCH", "report_sent")

	e.setActor(t, "product-manager", "scenario:product-manager", filepath.Join(e.root, "product"))
	runTestCommand(t, e, "accept", "--event", productResearchReport.ID, "--next", "Finalize product brief")
	runTestCommand(t, e, "message", "--to", "zantianyou", "--kind", "handoff", "--case", "LAUNCH-PRODUCT", "--text", "产品研究已完成，工程验收请覆盖三个 onboarding 风险", "--ref-file", productEvidence, "--delivery", "quiet")
	runTestCommand(t, e, "report", "--case", "LAUNCH-PRODUCT", "--result", "completed", "--artifact", productBrief, "--verify", "scope maps to research", "--next", "President review")
	events = scenarioEvents(t, e, cfg)
	productRootReport := latestCaseEvent(events, "LAUNCH-PRODUCT", "report_sent")

	e.setActor(t, "penny", "scenario:penny", e.office)
	runTestCommand(t, e, "accept", "--event", productRootReport.ID, "--next", "Authorize engineering")
	transportBeforeEngineeringIssue := len(e.transport.calls)
	runTestCommand(t, e, "issue", "--case", "LAUNCH-ENGINEERING", "--to", "zantianyou", "--decision", engineeringDecision, "--next", "Implement against product evidence")
	if len(e.transport.calls) != transportBeforeEngineeringIssue+1 || !strings.Contains(e.transport.calls[len(e.transport.calls)-1].message, "产品研究已完成") {
		t.Fatalf("department handoff was not bundled into engineering turn: calls=%d prompt=%q", len(e.transport.calls), e.transport.calls[len(e.transport.calls)-1].message)
	}
	events = scenarioEvents(t, e, cfg)
	engineeringIssue := latestCaseEvent(events, "LAUNCH-ENGINEERING", "issue_sent")

	// Engineering manager delegates implementation. The first result is
	// rejected with a concrete re-delivery condition; the developer re-reports
	// as current owner, and only verified work is accepted.
	e.setActor(t, "zantianyou", "scenario:engineering-manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", engineeringIssue.ID, "--next", "Decompose implementation")
	runTestCommand(t, e, "case", "create", "--id", "LAUNCH-ENGINEERING-IMPLEMENT", "--parent", "LAUNCH-ENGINEERING", "--title", "Implement onboarding flow", "--priority", "P0", "--source", charter)
	runTestCommand(t, e, "issue", "--case", "LAUNCH-ENGINEERING-IMPLEMENT", "--to", "eng-developer", "--next", "Implement, test, and prove rollback")
	events = scenarioEvents(t, e, cfg)
	implementationIssue := latestCaseEvent(events, "LAUNCH-ENGINEERING-IMPLEMENT", "issue_sent")

	e.setActor(t, "eng-developer", "scenario:developer", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", implementationIssue.ID, "--next", "Implement")
	runTestCommand(t, e, "report", "--case", "LAUNCH-ENGINEERING-IMPLEMENT", "--result", "completed", "--artifact", engineeringArtifactV1, "--verify", "happy path passes", "--next", "Manager review")
	events = scenarioEvents(t, e, cfg)
	firstImplementationReport := latestCaseEvent(events, "LAUNCH-ENGINEERING-IMPLEMENT", "report_sent")

	e.setActor(t, "zantianyou", "scenario:engineering-manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "return", "--event", firstImplementationReport.ID, "--reason", "缺少 rollback 证据", "--next", "补充 rollback 测试并重新提交")

	e.setActor(t, "eng-developer", "scenario:developer", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "report", "--case", "LAUNCH-ENGINEERING-IMPLEMENT", "--result", "completed", "--artifact", engineeringArtifactV2, "--verify", "happy path and rollback both pass", "--next", "Manager re-review")
	events = scenarioEvents(t, e, cfg)
	secondImplementationReport := latestCaseEvent(events, "LAUNCH-ENGINEERING-IMPLEMENT", "report_sent")
	if secondImplementationReport.AssignmentEventID != implementationIssue.ID ||
		secondImplementationReport.AssignmentDigest != implementationIssue.AssignmentDigest ||
		secondImplementationReport.AssigneeSeatDigest != implementationIssue.AssigneeSeatDigest ||
		secondImplementationReport.RoleCardID != implementationIssue.RoleCardID ||
		secondImplementationReport.RoleCardVersion != implementationIssue.RoleCardVersion ||
		secondImplementationReport.RoleCardDigest != implementationIssue.RoleCardDigest ||
		secondImplementationReport.RoleCardManualPath != implementationIssue.RoleCardManualPath ||
		secondImplementationReport.Recipient != implementationIssue.Acceptor {
		t.Fatalf("rework report did not preserve frozen assignment routing: %+v", secondImplementationReport)
	}

	e.setActor(t, "zantianyou", "scenario:engineering-manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", secondImplementationReport.ID, "--next", "Complete release summary")
	runTestCommand(t, e, "report", "--case", "LAUNCH-ENGINEERING", "--result", "completed", "--artifact", engineeringSummary, "--verify", "child evidence accepted", "--next", "President review")
	events = scenarioEvents(t, e, cfg)
	engineeringRootReport := latestCaseEvent(events, "LAUNCH-ENGINEERING", "report_sent")

	// President's office performs account closure. The Project View must derive
	// a fully closed company project from the same case ledger, and rebuild must
	// produce the identical terminal case set.
	e.setActor(t, "penny", "scenario:penny", e.office)
	runTestCommand(t, e, "accept", "--event", engineeringRootReport.ID, "--next", "Close launch")
	for _, caseID := range []string{"LAUNCH-PRODUCT-RESEARCH", "LAUNCH-PRODUCT", "LAUNCH-ENGINEERING-IMPLEMENT", "LAUNCH-ENGINEERING", "LAUNCH-COMPANY"} {
		runTestCommand(t, e, "close", "--case", caseID, "--reason", "Evidence accepted in launch review", "--source", charter)
	}
	project := runTestCommand(t, e, "project", "show", "--project", "virtual-company-launch")
	if !strings.Contains(project, "status=closed") || !strings.Contains(project, "roots=1") || !strings.Contains(project, "total=5") || !strings.Contains(project, "closure_gap=0") {
		t.Fatalf("project did not converge to closed: %s", project)
	}
	before, err := NewStore(e.data).Snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(e.data, "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatalf("delete derived state before scenario rebuild: %v", err)
	}
	if _, err := os.Lstat(statePath); !os.IsNotExist(err) {
		t.Fatalf("derived state still exists before scenario rebuild: %v", err)
	}
	rebuilt, err := NewStore(e.data).Rebuild(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if info, err := os.Lstat(statePath); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("scenario rebuild did not recreate derived state: info=%v err=%v", info, err)
	}
	if before.EventCount != rebuilt.EventCount || len(before.Cases) != len(rebuilt.Cases) {
		t.Fatalf("scenario rebuild diverged: before=%+v rebuilt=%+v", before, rebuilt)
	}
	for caseID, state := range rebuilt.Cases {
		if state.Status != "closed" {
			t.Fatalf("scenario case %s did not close after rebuild: %+v", caseID, state)
		}
	}
}

// TestVirtualCompanyDeliveryIncidentRecoveryScenario models a delivery outage
// during a president-to-engineering delegation. The company must freeze the
// business transition, survive an application restart, require human evidence
// before retry, and then finish the ordinary assignment workflow without
// duplicating either the issue or its external side effect.
func TestVirtualCompanyDeliveryIncidentRecoveryScenario(t *testing.T) {
	e := setupTestEnv(t)
	cfg := testConfig()
	caseID := "COMPANY-DELIVERY-INCIDENT"
	projectID := "delivery-incident-recovery"
	source := writeTestFile(t, filepath.Join(e.office, "incident-charter.md"), "# Delivery incident recovery\nShip only after evidence-backed recovery.\n")
	order := writeTestFile(t, filepath.Join(e.office, "incident-order.md"), "# Engineering order\n")
	decision := writeIssueApproval(t, e, "incident-decision.md", "DEC-DELIVERY-INCIDENT", caseID, order, "zantianyou")
	evidence := writeTestFile(t, filepath.Join(e.office, "incident-not-delivered.md"), "# Receiver evidence\nManager confirms no prompt arrived.\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "incident-result.md"), "# Recovered delivery result\nImplementation and rollback checks passed.\n")

	e.setActor(t, "penny", "incident:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "Recover a failed executive delegation", "--project", projectID, "--priority", "P0", "--source", source)
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("simulated gateway timeout after write")
	app := e.app(t)
	if err := app.run([]string{"issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "Implement and report evidence"}); err == nil {
		t.Fatal("ambiguous company delegation unexpectedly succeeded")
	}
	events := scenarioEvents(t, e, cfg)
	origin := latestCaseEvent(events, caseID, "issue_prepared")
	if origin.ID == "" {
		t.Fatal("ambiguous delegation did not durably record its business origin")
	}
	view, ok, err := NewStore(e.data).Delivery(cfg, origin.DeliveryID)
	if err != nil || !ok || view.Status != deliveryUnknown || latestCaseEvent(events, caseID, "issue_sent").ID != "" {
		t.Fatalf("ambiguous delegation advanced business state: view=%+v ok=%t err=%v", view, ok, err)
	}
	beforeBlockedMutation := len(events)
	closeErr := app.run([]string{"close", "--case", caseID, "--reason", "must not bypass ambiguous issue", "--source", source})
	if closeErr == nil || !strings.Contains(closeErr.Error(), "未收敛 workflow delivery") ||
		!strings.Contains(closeErr.Error(), "hq delivery status --id "+origin.DeliveryID) ||
		!strings.Contains(closeErr.Error(), "hq delivery resolve --id "+origin.DeliveryID) ||
		!strings.Contains(closeErr.Error(), "hq delivery retry --id "+origin.DeliveryID) ||
		strings.Contains(closeErr.Error(), "delivery consume") {
		t.Fatalf("business mutation escaped status-specific delivery closure guard: %v", closeErr)
	}
	if after := scenarioEvents(t, e, cfg); len(after) != beforeBlockedMutation {
		t.Fatalf("fenced close changed ledger: %d -> %d", beforeBlockedMutation, len(after))
	}

	// Reconstruct every dependency to model a process restart. Recovery first
	// proves that the receiver did not observe the ambiguous prompt; only then
	// may the same delivery_id be retried.
	e.setActor(t, "penny", "incident:operations", e.office)
	restarted := e.app(t)
	if err := restarted.run([]string{"delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "not-delivered", "--reason", "receiver confirmed absence", "--evidence", evidence}); err != nil {
		t.Fatal(err)
	}
	e.transport.result, e.transport.err = transportSent, nil
	if err := restarted.run([]string{"delivery", "retry", "--id", origin.DeliveryID}); err != nil {
		t.Fatal(err)
	}
	events = scenarioEvents(t, e, cfg)
	issue := latestCaseEvent(events, caseID, "issue_sent")
	view, ok, err = NewStore(e.data).Delivery(cfg, origin.DeliveryID)
	if err != nil || !ok || view.Status != deliverySent || view.AttemptCount != 2 || issue.ID == "" {
		t.Fatalf("evidence-backed retry did not converge once: issue=%+v view=%+v ok=%t err=%v", issue, view, ok, err)
	}
	if len(e.transport.calls) != 2 {
		t.Fatalf("incident transport calls=%d, want ambiguous attempt plus one authorized retry", len(e.transport.calls))
	}

	e.setActor(t, "zantianyou", "incident:engineering", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Complete recovered assignment")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "implementation and rollback pass", "--next", "President review")
	events = scenarioEvents(t, e, cfg)
	report := latestCaseEvent(events, caseID, "report_sent")

	e.setActor(t, "penny", "incident:penny", e.office)
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "Close recovered project")
	runTestCommand(t, e, "close", "--case", caseID, "--reason", "Recovered assignment evidence accepted", "--source", source)
	project := runTestCommand(t, e, "project", "show", "--project", projectID)
	if !strings.Contains(project, "status=closed") || !strings.Contains(project, "closure_gap=0") {
		t.Fatalf("recovered project did not close: %s", project)
	}
	snapshot, err := NewStore(e.data).Rebuild(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Cases[caseID]; state == nil || state.Status != string(statusClosed) {
		t.Fatalf("recovered company ledger did not rebuild closed: %+v", state)
	}
}
