package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assignmentProgressFixture(t *testing.T, caseID string) (testEnv, Config, Event, Event) {
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
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "eng-developer" {
			cfg.Agents[index].ActivationPolicy = activationOnAssignment
			finalizeTestSeatMutation(&cfg.Agents[index])
		}
	}
	writeConfigFixture(t, e.config, cfg)

	source := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-source.md"), "# assignment progress source\n")
	e.setActor(t, "zantianyou", "progress:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "Assignment progress watchdog", "--project", "assignment-progress", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "eng-developer", "--next", "Implement and report through the original assignment")
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, caseID, "issue_sent")
	if issue.ID == "" {
		t.Fatal("fixture has no delivered assignment")
	}
	e.setActor(t, "eng-developer", "progress:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Execute durable assignment")
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	accepted := latestCaseEvent(events, caseID, "event_accepted")
	if accepted.ID == "" {
		t.Fatal("fixture has no accepted assignment")
	}
	return e, cfg, issue, accepted
}

func TestAssignmentProgressWatchdogNudgesIdleWorkerThenEscalatesWithoutReporting(t *testing.T) {
	e, cfg, issue, accepted := assignmentProgressFixture(t, "ASSIGNMENT-PROGRESS-001")
	acceptedAt, err := parseOperationsTime("accepted.at", accepted.At)
	if err != nil {
		t.Fatal(err)
	}
	now := acceptedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "eng-developer", "done", "progress-worker")
	addOperationsLive(&control.snapshot, cfg, e.root, "zantianyou", "idle", "progress-manager")
	app := operationsTestApp(t, e, control, &now)

	if err := app.cmdNudge([]string{"enqueue", "--id", "MANUAL-WORKER-NUDGE", "--dedupe", "manual:worker", "--to", "eng-developer", "--message", "bypass assignment"}); err == nil || !strings.Contains(err.Error(), "经理或总部联络") {
		t.Fatalf("public nudge unexpectedly bypassed worker assignment protocol: %v", err)
	}
	if err := app.runAssignmentProgressWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts := fakePromptCalls(control)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "prompt eng-developer ") || !strings.Contains(prompts[0], issue.CaseID) || !strings.Contains(prompts[0], "hq report") {
		t.Fatalf("first worker progress nudge is not actionable: %v", prompts)
	}
	if err := app.runAssignmentProgressWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 1 {
		t.Fatalf("same worker queue basis duplicated first nudge: prompts=%d", got)
	}

	now = now.Add(16 * time.Second)
	if err := app.runAssignmentProgressWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 2 {
		t.Fatalf("second bounded worker nudge missing: prompts=%d", got)
	}

	now = now.Add(31 * time.Second)
	if err := app.runAssignmentProgressWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts = fakePromptCalls(control)
	if len(prompts) != 3 || !strings.HasPrefix(prompts[2], "prompt zantianyou ") || !strings.Contains(prompts[2], "eng-developer") || !strings.Contains(prompts[2], "HQ未代报") {
		t.Fatalf("worker progress escalation did not reach its manager: %v", prompts)
	}
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[issue.ID]
	if assignment == nil || assignment.Status != "accepted" || assignment.Consumed {
		t.Fatalf("watchdog mutated assignment/report state: %+v", assignment)
	}
}

func TestAssignmentProgressWatchdogColdResumesOfflineAcceptedWorkerWithDurableContext(t *testing.T) {
	e, cfg, issue, accepted := assignmentProgressFixture(t, "ASSIGNMENT-PROGRESS-RECOVERY")
	acceptedAt, err := parseOperationsTime("accepted.at", accepted.At)
	if err != nil {
		t.Fatal(err)
	}
	now := acceptedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	app := operationsTestApp(t, e, control, &now)

	if err := app.runAssignmentProgressWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts := fakePromptCalls(control)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "prompt eng-developer ") ||
		!strings.Contains(prompts[0], "[HQ runtime recovery] trigger=assignment_progress") ||
		!strings.Contains(prompts[0], "assignment show --id") || !strings.Contains(prompts[0], issue.CaseID) ||
		!strings.Contains(prompts[0], "不得仅结束 Agent 回合") {
		t.Fatalf("offline accepted assignment did not cold-resume with durable recovery context: %v", prompts)
	}
	control.mu.Lock()
	live := append([]HerdrAgent(nil), control.snapshot.Agents...)
	control.mu.Unlock()
	if len(live) != 1 || live[0].Name != "eng-developer" {
		t.Fatalf("cold recovery did not create the exact worker seat: %+v", live)
	}
	sessions, err := app.Sessions.List(SessionFilter{Agent: "eng-developer"})
	if err != nil || len(activeSessionStarts(sessions)) != 1 {
		t.Fatalf("cold recovery session did not converge: sessions=%+v err=%v", sessions, err)
	}
}

func TestPatrolReportsIdleAcceptedWorkerAsStalled(t *testing.T) {
	e, cfg, _, accepted := assignmentProgressFixture(t, "ASSIGNMENT-PROGRESS-PATROL")
	acceptedAt, err := parseOperationsTime("accepted.at", accepted.At)
	if err != nil {
		t.Fatal(err)
	}
	now := acceptedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "eng-developer", "done", "progress-patrol")
	store := NewStore(e.data)
	store.Now = func() time.Time { return now }
	report, err := (&PatrolService{Herdr: control, Store: store}).Run(context.Background(), cfg, e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stalled != 1 || report.Findings[0].Agent != "eng-developer" || report.Findings[0].SignalType != "idle_with_active_assignment" {
		t.Fatalf("idle accepted worker was not surfaced as stalled: %+v", report)
	}
	setFakeAgentStatus(control, "eng-developer", "working")
	report, err = (&PatrolService{Herdr: control, Store: store}).Run(context.Background(), cfg, e.root, 0)
	if err != nil || report.Stalled != 0 {
		t.Fatalf("working accepted worker was falsely stalled: report=%+v err=%v", report, err)
	}
}
