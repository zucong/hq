package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClosureQueueSelectsOnlyPostOrderAcceptedCases(t *testing.T) {
	ledger := newLedgerState()
	ledger.snapshot.Cases = map[string]*CaseState{
		"ROOT":       {ID: "ROOT", Status: string(statusAccepted), LastEventID: "E-ROOT"},
		"CHILD":      {ID: "CHILD", ParentCaseID: "ROOT", Status: string(statusAccepted), LastEventID: "E-CHILD"},
		"FINDING":    {ID: "FINDING", ParentCaseID: "ROOT", Status: string(statusFindingAccepted), LastEventID: "E-FINDING"},
		"BLOCKED":    {ID: "BLOCKED", ParentCaseID: "ROOT", Status: string(statusBlocked), LastEventID: "E-BLOCKED"},
		"DECISION":   {ID: "DECISION", ParentCaseID: "ROOT", Status: string(statusNeedsDecision), LastEventID: "E-DECISION"},
		"OPEN":       {ID: "OPEN", ParentCaseID: "ROOT", Status: string(statusOpen), LastEventID: "E-OPEN"},
		"ACTIVE":     {ID: "ACTIVE", ParentCaseID: "ROOT", Status: string(statusAccepted), LastEventID: "E-ACTIVE"},
		"UNSETTLED":  {ID: "UNSETTLED", ParentCaseID: "ROOT", Status: string(statusAccepted), LastEventID: "E-UNSETTLED"},
		"CLOSED-OLD": {ID: "CLOSED-OLD", ParentCaseID: "ROOT", Status: string(statusClosed), LastEventID: "E-CLOSED"},
	}
	for index, id := range []string{"E-ROOT", "E-CHILD", "E-FINDING", "E-BLOCKED", "E-DECISION", "E-OPEN", "E-ACTIVE", "E-UNSETTLED", "E-CLOSED"} {
		ledger.events[id] = Event{ID: id, At: time.Date(2026, 9, 3, 0, index, 0, 0, time.UTC).Format(time.RFC3339)}
	}
	ledger.assignments["ISSUE-ACTIVE"] = &caseAssignment{EventID: "ISSUE-ACTIVE", AssignmentID: "ASSIGN-ACTIVE", CaseID: "ACTIVE", Status: "accepted"}
	ledger.assignmentList = []string{"ISSUE-ACTIVE"}
	ledger.deliveries["DELIVERY-UNSETTLED"] = &deliveryRecord{Origin: Event{CaseID: "UNSETTLED", Type: "event_accepted"}, Status: deliveryUnknown}
	backlog, err := ledger.closureQueueBacklog(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if backlog == nil || backlog.Closer != "penny" || len(backlog.Items) != 2 {
		t.Fatalf("closure backlog=%+v", backlog)
	}
	if backlog.Items[0].CaseID != "CHILD" || backlog.Items[1].CaseID != "FINDING" {
		t.Fatalf("closure candidates are not the post-order accepted leaves: %+v", backlog.Items)
	}
	for _, item := range backlog.Items {
		if item.CaseID == "ROOT" || item.CaseID == "ACTIVE" || item.CaseID == "UNSETTLED" || item.Status == string(statusBlocked) || item.Status == string(statusNeedsDecision) || item.Status == string(statusOpen) {
			t.Fatalf("unsafe closure candidate: %+v", item)
		}
	}
}

func closureQueueFixture(t *testing.T, caseID string) (testEnv, Config, Event) {
	t.Helper()
	e := setupTestEnv(t)
	cfg := testConfig()
	cfg.DeliveryPolicy = &DeliveryPolicy{
		DefaultMode:               deliveryModeAuto,
		MaxConsecutiveWakes:       10,
		ManagerQueueStallTimeout:  "15s",
		ManagerQueueEscalateAfter: "30s",
		MaxManagerQueueNudges:     2,
	}
	writeConfigFixture(t, e.config, cfg)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-source.md"), "# closure source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-artifact.md"), "# accepted evidence\n")
	e.setActor(t, "zantianyou", "closure:manager", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "case", "create", "--id", caseID+"-ROOT", "--title", "Open project root", "--project", "closure-watchdog", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--parent", caseID+"-ROOT", "--title", "Accepted closure leaf", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "eng-developer", "--next", "Produce accepted evidence")
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, caseID, "issue_sent")
	e.setActor(t, "eng-developer", "closure:worker", testAgentCWD(cfg, e.root, "eng-developer"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Execute closure fixture")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact,
		"--verify", "accepted evidence exists", "--next", "Manager review")
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report := latestCaseEvent(events, caseID, "report_sent")
	e.setActor(t, "zantianyou", "closure:manager-review", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "Account closer performs explicit post-order closure")
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return e, cfg, latestCaseEvent(events, caseID, "event_accepted")
}

func TestClosureQueueWatchdogNudgesAccountCloserWithoutClosing(t *testing.T) {
	e, cfg, accepted := closureQueueFixture(t, "CLOSURE-WATCHDOG-001")
	acceptedAt, err := parseOperationsTime("accepted.at", accepted.At)
	if err != nil {
		t.Fatal(err)
	}
	now := acceptedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "penny", "idle", "closure-account")
	app := operationsTestApp(t, e, control, &now)

	if err := app.runClosureQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts := fakePromptCalls(control)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "prompt penny ") ||
		!strings.Contains(prompts[0], "CLOSURE-WATCHDOG-001") || !strings.Contains(prompts[0], "hq close") ||
		!strings.Contains(prompts[0], "本轮按列出顺序逐项核验至多1项") ||
		!strings.Contains(prompts[0], "不得关闭open/blocked/needs_decision") {
		t.Fatalf("closure nudge is not bounded and actionable: %v", prompts)
	}
	if err := app.runClosureQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 1 {
		t.Fatalf("same closure basis duplicated first nudge: %d", got)
	}
	state, err := app.currentCase("CLOSURE-WATCHDOG-001")
	if err != nil || state.Status != string(statusAccepted) {
		t.Fatalf("watchdog made a business closure decision: state=%+v err=%v", state, err)
	}
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	delivered := 0
	for _, record := range ledger.nudges {
		if record != nil && strings.HasPrefix(record.Origin.DedupeKey, "closure-queue:penny:"+accepted.ID+":") {
			if record.State != "delivered" {
				t.Fatalf("closure nudge is not terminal: %+v", record.view())
			}
			delivered++
		}
	}
	if delivered != 1 {
		t.Fatalf("durable closure nudge facts=%d want=1", delivered)
	}
}

func TestPatrolReportsIdleAccountCloserWithClosureBacklog(t *testing.T) {
	e, cfg, accepted := closureQueueFixture(t, "CLOSURE-PATROL-001")
	acceptedAt, err := parseOperationsTime("accepted.at", accepted.At)
	if err != nil {
		t.Fatal(err)
	}
	now := acceptedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "penny", "done", "closure-patrol")
	store := NewStore(e.data)
	store.Now = func() time.Time { return now }
	report, err := (&PatrolService{Herdr: control, Store: store}).Run(context.Background(), cfg, e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stalled != 1 || len(report.Findings) != 1 || report.Findings[0].Agent != "penny" || report.Findings[0].SignalType != "idle_with_closure_backlog" {
		t.Fatalf("idle account closer closure backlog was not surfaced: %+v", report)
	}
	if !strings.Contains(report.Findings[0].Message, "hq close") || !strings.Contains(report.Findings[0].Message, "HQ不会自动关闭") {
		t.Fatalf("closure patrol finding is not self-correcting: %+v", report.Findings[0])
	}
	setFakeAgentStatus(control, "penny", "working")
	report, err = (&PatrolService{Herdr: control, Store: store}).Run(context.Background(), cfg, e.root, 0)
	if err != nil || report.Stalled != 0 {
		t.Fatalf("working account closer was falsely classified as stalled: report=%+v err=%v", report, err)
	}
}
