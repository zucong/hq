package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func managerQueueSubmittedFixture(t *testing.T, caseID string) (testEnv, Config, Event, Event) {
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

	source := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-source.md"), "# queue watchdog source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-result.md"), "# verified worker result\n")
	e.setActor(t, "zantianyou", "queue:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "Manager queue watchdog", "--project", "queue-watchdog", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "eng-developer", "--next", "Implement and report durable evidence")
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, caseID, "issue_sent")
	if issue.ID == "" {
		t.Fatal("fixture has no delivered assignment")
	}

	e.setActor(t, "eng-developer", "queue:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Execute assigned work")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact,
		"--verify", "fixture evidence checked", "--next", "Manager must review this report")
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report := latestCaseEvent(events, caseID, "report_sent")
	if report.ID == "" {
		t.Fatal("fixture has no submitted report")
	}
	return e, cfg, issue, report
}

func TestManagerQueuePriorityDominatesAgeWithinActionableWork(t *testing.T) {
	oldest := "2026-01-01T00:00:00Z"
	newer := "2026-01-02T00:00:00Z"
	if !managerQueueItemLess(
		ManagerQueueItem{Kind: "review", StatusUpdatedAt: newer},
		ManagerQueueItem{Kind: "work", StatusUpdatedAt: oldest},
	) {
		t.Fatal("newer submission review must outrank older manager work")
	}
	if !managerQueueItemLess(
		ManagerQueueItem{Kind: "work", StatusUpdatedAt: newer},
		ManagerQueueItem{Kind: "owned_case", StatusUpdatedAt: oldest},
	) {
		t.Fatal("newer active/rework manager assignment must outrank older unassigned open case")
	}
	if !managerQueueItemLess(
		ManagerQueueItem{Kind: "work", CaseID: "OLDER", StatusUpdatedAt: oldest},
		ManagerQueueItem{Kind: "work", CaseID: "NEWER", StatusUpdatedAt: newer},
	) {
		t.Fatal("age must remain FIFO within the same manager queue priority")
	}
}

func TestManagerQueueWatchdogFreezesUncertainPromptAndEscalatesForReconcile(t *testing.T) {
	e, cfg, _, report := managerQueueSubmittedFixture(t, "QUEUE-WATCHDOG-UNKNOWN")
	reportedAt, err := parseOperationsTime("report.at", report.At)
	if err != nil {
		t.Fatal(err)
	}
	now := reportedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "zantianyou", "idle", "unknown-manager")
	addOperationsLive(&control.snapshot, cfg, e.root, "penny", "idle", "unknown-liaison")
	control.promptOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("synthetic prompt timeout")}
	app := operationsTestApp(t, e, control, &now)

	if err := app.runManagerQueueWatchdogOnce(context.Background()); err == nil {
		t.Fatal("ambiguous manager prompt was not surfaced")
	}
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	var unknownID string
	for _, record := range ledger.nudges {
		if record != nil && record.Origin.Recipient == "zantianyou" && record.State == "unknown" {
			unknownID = record.Origin.NudgeID
		}
	}
	if unknownID == "" || len(fakePromptCalls(control)) != 1 {
		t.Fatalf("ambiguous attempt was not durably frozen: nudge=%s calls=%v", unknownID, fakePromptCalls(control))
	}

	now = now.Add(31 * time.Second)
	control.promptOutcome = HerdrMutationResult{}
	if err := app.runManagerQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts := fakePromptCalls(control)
	if len(prompts) != 2 || !strings.HasPrefix(prompts[1], "prompt penny ") ||
		!strings.Contains(prompts[1], unknownID) || !strings.Contains(prompts[1], "reconcile") {
		t.Fatalf("uncertain prompt was redelivered or not escalated for reconcile: %v", prompts)
	}
}

func TestMaxWIPErrorNamesTheBlockingSubmissionAndExactManagerCommands(t *testing.T) {
	e, cfg, _, report := managerQueueSubmittedFixture(t, "QUEUE-WIP-ROOT")
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "queue-wip-child.md"), "# second work item\n")
	e.setActor(t, "zantianyou", "queue:wip-manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", "QUEUE-WIP-CHILD", "--parent", "QUEUE-WIP-ROOT", "--title", "Second work item", "--source", source)
	before, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	transportBefore := len(e.transport.calls)
	err = runAssignmentCommandError(t, e, "issue", "--case", "QUEUE-WIP-CHILD", "--to", "eng-developer", "--next", "Must wait for review")
	if err == nil {
		t.Fatal("max_wip accepted a second assignment")
	}
	for _, want := range []string{
		"max_wip=1",
		"hq assignment list --assignee eng-developer",
		"hq accept --event " + report.ID,
		"hq return --event " + report.ID,
		"不要改用裸 herdr prompt",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("max_wip correction missing %q: %v", want, err)
		}
	}
	after, readErr := NewStore(e.data).ReadAll(cfg)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) || len(e.transport.calls) != transportBefore {
		t.Fatalf("rejected max_wip correction had side effects: events=%d->%d transport=%d->%d", len(before), len(after), transportBefore, len(e.transport.calls))
	}
}

func TestManagerQueueWatchdogRecoversAnExpiredClaimWithoutDuplicateOrigin(t *testing.T) {
	e, cfg, _, report := managerQueueSubmittedFixture(t, "QUEUE-WATCHDOG-RECLAIM")
	reportedAt, err := parseOperationsTime("report.at", report.At)
	if err != nil {
		t.Fatal(err)
	}
	now := reportedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "zantianyou", "idle", "reclaim-manager")
	app := operationsTestApp(t, e, control, &now)
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	backlogs, err := ledger.managerQueueBacklogs(cfg)
	if err != nil || len(backlogs) != 1 {
		t.Fatalf("backlogs=%+v err=%v", backlogs, err)
	}
	dedupe := managerQueueDedupe("zantianyou", backlogs[0].BasisEventID, "nudge", 1)
	id := stableCommandID("manager-queue-nudge", dedupe)
	message := managerQueueReminderMessage(backlogs[0], 1, 2)
	if err := app.cmdNudge([]string{"enqueue", "--id", id, "--dedupe", dedupe, "--to", "zantianyou", "--message", message, "--ttl", managerQueueNudgeTTL.String()}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", id, "--claim", "CRASHED-QUEUE-CLAIM", "--lease", "5s"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)
	if err := app.runManagerQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	view, err := app.readNudge(id)
	if err != nil || view.State != "delivered" || view.ClaimID == "CRASHED-QUEUE-CLAIM" {
		t.Fatalf("expired claim was not safely reclaimed: view=%+v err=%v", view, err)
	}
	if got := len(fakePromptCalls(control)); got != 1 {
		t.Fatalf("recovery delivered prompt %d times", got)
	}
	events, err := app.Store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	origins := 0
	for _, event := range events {
		if event.Type == "nudge_enqueued" && event.NudgeID == id {
			origins++
		}
	}
	if origins != 1 {
		t.Fatalf("claim recovery duplicated durable nudge origin: %d", origins)
	}
}

func TestManagerQueueBacklogOnlyTreatsOpenUnassignedOwnedCaseAsActionable(t *testing.T) {
	e, cfg, _, report := managerQueueSubmittedFixture(t, "QUEUE-ACTIONABLE-ROOT")
	e.setActor(t, "zantianyou", "queue:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "Accepted result is historical, not a fresh task")

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	backlogs, err := ledger.managerQueueBacklogs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlogs) != 0 {
		t.Fatalf("accepted manager-owned case was falsely actionable: %+v", backlogs)
	}

	blockedSource := writeTestFile(t, filepath.Join(e.root, "engineering", "queue-blocked-source.md"), "# blocked work\n")
	runTestCommand(t, e, "case", "create", "--id", "QUEUE-ACTIONABLE-BLOCKED", "--parent", "QUEUE-ACTIONABLE-ROOT",
		"--title", "Blocked historical work", "--source", blockedSource)
	runTestCommand(t, e, "issue", "--case", "QUEUE-ACTIONABLE-BLOCKED", "--to", "eng-developer", "--next", "Attempt and report the blocker")
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	blockedIssue := latestCaseEvent(events, "QUEUE-ACTIONABLE-BLOCKED", "issue_sent")
	e.setActor(t, "eng-developer", "queue:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", blockedIssue.ID, "--next", "Attempt assigned work")
	runTestCommand(t, e, "report", "--case", "QUEUE-ACTIONABLE-BLOCKED", "--result", "blocked", "--source", blockedSource,
		"--note", "External prerequisite is unavailable", "--next", "Manager records the blocker")
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	blockedReport := latestCaseEvent(events, "QUEUE-ACTIONABLE-BLOCKED", "report_sent")
	e.setActor(t, "zantianyou", "queue:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", blockedReport.ID, "--next", "Wait for the external prerequisite")
	ledger, err = e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	backlogs, err = ledger.managerQueueBacklogs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlogs) != 0 {
		t.Fatalf("blocked manager-owned case was falsely actionable: %+v", backlogs)
	}

	source := writeTestFile(t, filepath.Join(e.root, "engineering", "queue-actionable-child.md"), "# unassigned manager work\n")
	runTestCommand(t, e, "case", "create", "--id", "QUEUE-ACTIONABLE-CHILD", "--parent", "QUEUE-ACTIONABLE-ROOT",
		"--title", "Open work waiting for delegation", "--source", source)
	ledger, err = e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	backlogs, err = ledger.managerQueueBacklogs(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(backlogs) != 1 || backlogs[0].Manager != "zantianyou" || len(backlogs[0].Items) != 1 {
		t.Fatalf("open unassigned manager-owned case was not actionable: %+v", backlogs)
	}
	item := backlogs[0].Items[0]
	if item.Kind != "owned_case" || item.CaseID != "QUEUE-ACTIONABLE-CHILD" || item.Status != string(statusOpen) {
		t.Fatalf("unexpected actionable owned case: %+v", item)
	}
}

func setFakeAgentStatus(control *fakeHerdrControl, name, status string) {
	control.mu.Lock()
	defer control.mu.Unlock()
	for index := range control.snapshot.Agents {
		if control.snapshot.Agents[index].Name == name {
			control.snapshot.Agents[index].Status = status
		}
	}
}

func fakePromptCalls(control *fakeHerdrControl) []string {
	control.mu.Lock()
	defer control.mu.Unlock()
	var calls []string
	for _, call := range control.calls {
		if strings.HasPrefix(call, "prompt ") {
			calls = append(calls, call)
		}
	}
	return calls
}

func TestManagerQueueWatchdogNudgesIdleManagerEscalatesAndNeverAccepts(t *testing.T) {
	e, cfg, issue, report := managerQueueSubmittedFixture(t, "QUEUE-WATCHDOG-001")
	reportedAt, err := parseOperationsTime("report.at", report.At)
	if err != nil {
		t.Fatal(err)
	}
	now := reportedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "zantianyou", "idle", "queue-manager")
	addOperationsLive(&control.snapshot, cfg, e.root, "penny", "idle", "queue-liaison")
	app := operationsTestApp(t, e, control, &now)

	if err := app.runManagerQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts := fakePromptCalls(control)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "prompt zantianyou ") ||
		!strings.Contains(prompts[0], report.ID) || !strings.Contains(prompts[0], "hq accept") || !strings.Contains(prompts[0], "hq return") {
		t.Fatalf("first durable remediation is not directly actionable: %v", prompts)
	}
	if err := app.runManagerQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 1 {
		t.Fatalf("same queue basis duplicated first nudge: prompts=%d", got)
	}

	now = now.Add(16 * time.Second)
	setFakeAgentStatus(control, "zantianyou", "done")
	if err := app.runManagerQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 2 {
		t.Fatalf("second bounded nudge missing or duplicated: prompts=%d", got)
	}

	now = now.Add(31 * time.Second)
	setFakeAgentStatus(control, "zantianyou", "idle")
	if err := app.runManagerQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts = fakePromptCalls(control)
	if len(prompts) != 3 || !strings.HasPrefix(prompts[2], "prompt penny ") ||
		!strings.Contains(prompts[2], "zantianyou") || !strings.Contains(prompts[2], "HQ未自动验收") {
		t.Fatalf("bounded escalation did not reach the manager's supervisor: %v", prompts)
	}
	if err := app.runManagerQueueWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 3 {
		t.Fatalf("escalation was not deduplicated: prompts=%d", got)
	}

	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[issue.ID]
	if assignment == nil || assignment.Status != "submitted" || assignment.Consumed {
		t.Fatalf("watchdog mutated business acceptance state: %+v", assignment)
	}
	delivered := 0
	for _, record := range ledger.nudges {
		if record != nil && strings.HasPrefix(record.Origin.DedupeKey, "manager-queue:zantianyou:"+report.ID+":") {
			if record.State != "delivered" {
				t.Fatalf("watchdog nudge has non-terminal state: %+v", record.view())
			}
			delivered++
		}
	}
	if delivered != 3 {
		t.Fatalf("durable watchdog facts=%d want=3", delivered)
	}
}

func TestPatrolReportsIdleDurableManagerQueueButNotWorkingQueue(t *testing.T) {
	e, cfg, _, reportEvent := managerQueueSubmittedFixture(t, "QUEUE-PATROL-001")
	reportedAt, err := parseOperationsTime("report.at", reportEvent.At)
	if err != nil {
		t.Fatal(err)
	}
	now := reportedAt.Add(16 * time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "zantianyou", "idle", "patrol-manager")
	store := NewStore(e.data)
	store.Now = func() time.Time { return now }
	patrol := &PatrolService{Herdr: control, Store: store}

	report, err := patrol.Run(context.Background(), cfg, e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stalled != 1 {
		t.Fatalf("idle durable queue was not classified as stalled: %+v", report)
	}
	finding := report.Findings[0]
	if finding.Category != "stalled" || finding.Agent != "zantianyou" ||
		!strings.Contains(finding.Message, reportEvent.ID) || !strings.Contains(finding.Message, "hq accept") {
		t.Fatalf("stalled finding is not actionable: %+v", finding)
	}

	setFakeAgentStatus(control, "zantianyou", "working")
	report, err = patrol.Run(context.Background(), cfg, e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Stalled != 0 {
		t.Fatalf("working manager was falsely classified as stalled: %+v", report)
	}
}
