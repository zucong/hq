package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func activeRevisionArgs(caseID, source string) []string {
	return []string{"case", "revise", "--id", caseID, "--version", "2", "--title", "Revised delivery",
		"--objective", "Implement the newly approved behavior", "--acceptance", "New behavior is independently verified",
		"--constraints", "Stop relying on the superseded contract", "--priority", "P0", "--source", source,
		"--supersede-active", "--next", "Accept the replacement assignment and restart from the revised contract",
		"--note", "Owner changed the requirement while execution was active"}
}

func TestActiveAssignmentRevisionAtomicallySupersedesAndReissues(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "active-revision.md"), "# Active revision\n")
	e.setActor(t, "zantianyou", "revision:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, caseCreateArgs("ACTIVE-REVISION-001", "Original delivery", source)...)
	runTestCommand(t, e, "issue", "--case", "ACTIVE-REVISION-001", "--to", "eng-developer", "--next", "Implement original behavior")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	oldIssue := latestCaseEvent(events, "ACTIVE-REVISION-001", "issue_sent")
	if oldIssue.ID == "" {
		t.Fatal("missing original issue")
	}
	e.setActor(t, "eng-developer", "revision:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", oldIssue.ID, "--next", "Start original behavior")

	e.setActor(t, "zantianyou", "revision:manager", filepath.Join(e.root, "engineering"))
	beforeCalls := len(e.transport.calls)
	runTestCommand(t, e, activeRevisionArgs("ACTIVE-REVISION-001", source)...)
	if len(e.transport.calls) != beforeCalls+1 {
		t.Fatalf("replacement revision prompts=%d want=%d", len(e.transport.calls), beforeCalls+1)
	}
	prompt := e.transport.calls[len(e.transport.calls)-1].message
	if !strings.HasPrefix(prompt, "[HQ URGENT REVISION]") || !strings.Contains(prompt, "SUPERSEDES="+oldIssue.AssignmentID) ||
		!strings.Contains(prompt, "hq case revise --supersede-active") {
		t.Fatalf("replacement prompt lacks urgent propagation contract: %s", prompt)
	}

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	oldAssignment := ledger.assignments[oldIssue.ID]
	if oldAssignment == nil || !oldAssignment.Consumed || oldAssignment.Status != "superseded" || oldAssignment.SupersededByAssignmentID == "" {
		t.Fatalf("old assignment not durably superseded: %+v", oldAssignment)
	}
	state := ledger.snapshot.Cases["ACTIVE-REVISION-001"]
	if state == nil || state.Version != 2 || state.Status != string(statusDispatched) || state.Owner != "eng-developer" {
		t.Fatalf("revised case projection mismatch: %+v", state)
	}
	var replacement *caseAssignment
	for _, assignment := range ledger.assignments {
		if assignment != nil && assignment.AssignmentID == oldAssignment.SupersededByAssignmentID {
			replacement = assignment
		}
	}
	if replacement == nil || replacement.Consumed || replacement.Status != "issued" ||
		replacement.SupersedesAssignmentID != oldAssignment.AssignmentID || replacement.ContractVersion != revisedAssignmentContractVersion ||
		replacement.CaseVersion != 2 || replacement.CaseDigest != state.Digest {
		t.Fatalf("replacement assignment mismatch: %+v", replacement)
	}

	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var superseded, revised, prepared Event
	for _, event := range events {
		if event.CaseID != "ACTIVE-REVISION-001" {
			continue
		}
		switch event.Type {
		case "assignment_superseded":
			superseded = event
		case "case_revised":
			revised = event
		case "issue_prepared":
			if event.AuthorizationType == "revision" {
				prepared = event
			}
		}
	}
	if superseded.ID == "" || revised.ID == "" || prepared.ID == "" ||
		revised.Sequence != superseded.Sequence+1 || prepared.Sequence != revised.Sequence+1 ||
		revised.BasisEventID != superseded.ID || prepared.BasisEventID != revised.ID {
		t.Fatalf("revision transaction is not an adjacent atomic chain: superseded=%+v revised=%+v prepared=%+v", superseded, revised, prepared)
	}

	beforeEvents := len(events)
	beforeCalls = len(e.transport.calls)
	runTestCommand(t, e, activeRevisionArgs("ACTIVE-REVISION-001", source)...)
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != beforeEvents || len(e.transport.calls) != beforeCalls {
		t.Fatalf("exact retry duplicated revision: events %d->%d prompts %d->%d", beforeEvents, len(events), beforeCalls, len(e.transport.calls))
	}

	e.setActor(t, "eng-developer", "revision:worker", filepath.Join(e.root, "engineering"))
	app := e.app(t)
	err = app.run([]string{"report", "--case", "ACTIVE-REVISION-001", "--result", "completed", "--artifact", source, "--verify", "old result", "--next", "review"})
	if err == nil || !strings.Contains(err.Error(), "尚未由 assignee accept") {
		t.Fatalf("worker could report under superseded contract before accepting replacement: %v", err)
	}
}

func TestUrgentDirectiveBypassesBusyAutoAndWakeBudgetButDoesNotChangeContract(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeAuto, 1)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "urgent-directive.md"), "# Urgent directive\n")
	manager := deliveryPolicyTestApp(t, e, "zantianyou", "directive:manager")
	if _, err := runDeliveryPolicyTest(manager, caseCreateArgs("URGENT-DIRECTIVE-001", "Directive case", source)...); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeliveryPolicyTest(manager, "issue", "--case", "URGENT-DIRECTIVE-001", "--to", "eng-developer", "--next", "Implement safely"); err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(manager.Config)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, "URGENT-DIRECTIVE-001", "issue_sent")
	worker := deliveryPolicyTestApp(t, e, "eng-developer", "directive:worker")
	if _, err := runDeliveryPolicyTest(worker, "accept", "--event", issue.ID, "--next", "working"); err != nil {
		t.Fatal(err)
	}

	manager.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeBusy, nil }
	beforeCalls := len(e.transport.calls)
	if _, err := runDeliveryPolicyTest(manager, "message", "--to", "eng-developer", "--case", "URGENT-DIRECTIVE-001",
		"--kind", "directive", "--urgency", "urgent", "--text", "Pause external writes and verify the new evidence first"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != beforeCalls+1 {
		t.Fatalf("urgent directive was silently queued or wake-budget downgraded: calls=%d want=%d", len(e.transport.calls), beforeCalls+1)
	}
	prompt := e.transport.calls[len(e.transport.calls)-1].message
	if !strings.HasPrefix(prompt, "[HQ URGENT DIRECTIVE]") || !strings.Contains(prompt, "URGENCY=urgent") ||
		!strings.Contains(prompt, "本消息不修改 assignment objective/acceptance/constraints") ||
		!strings.Contains(prompt, "hq message ack --message") {
		t.Fatalf("urgent directive envelope incomplete: %s", prompt)
	}
	messages := deliveryPolicyMessages(t, e)
	directive := messages[len(messages)-1]
	if directive.DeliveryMode != deliveryModeWakeup || directive.DeliveryReason != "urgent-next-turn" ||
		directive.AssignmentID != issue.AssignmentID || directive.CaseVersion != issue.CaseVersion || directive.CaseDigest != issue.CaseDigest {
		t.Fatalf("urgent directive binding mismatch: %+v", directive)
	}
	state, err := manager.currentCase("URGENT-DIRECTIVE-001")
	if err != nil {
		t.Fatal(err)
	}
	if state.Version != 1 || state.Status != string(statusInProgress) {
		t.Fatalf("directive changed case contract/state: %+v", state)
	}

	beforeMessages := len(messages)
	if _, err := runDeliveryPolicyTest(manager, "message", "--to", "eng-developer", "--case", "URGENT-DIRECTIVE-001",
		"--kind", "directive", "--urgency", "urgent", "--delivery", "quiet", "--text", "invalid"); err == nil || !strings.Contains(err.Error(), "下一安全回合") {
		t.Fatalf("urgent quiet conflict not rejected: %v", err)
	}
	if got := len(deliveryPolicyMessages(t, e)); got != beforeMessages {
		t.Fatalf("invalid urgent directive wrote ledger: %d->%d", beforeMessages, got)
	}
	if _, err := runDeliveryPolicyTest(manager, "message", "--to", "eng-developer", "--case", "URGENT-DIRECTIVE-001",
		"--kind", "info", "--urgency", "urgent", "--text", "invalid budget bypass"); err == nil || !strings.Contains(err.Error(), "只允许") {
		t.Fatalf("urgent non-directive bypass not rejected: %v", err)
	}
	if got := len(deliveryPolicyMessages(t, e)); got != beforeMessages {
		t.Fatalf("invalid urgent non-directive wrote ledger: %d->%d", beforeMessages, got)
	}
}

func TestUrgentDirectiveWatchdogRequiresAckAndEscalatesToDirectManager(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeAuto, 1)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "urgent-ack-watchdog.md"), "# Urgent ack watchdog\n")
	manager := deliveryPolicyTestApp(t, e, "zantianyou", "directive-watchdog:manager")
	if _, err := runDeliveryPolicyTest(manager, caseCreateArgs("URGENT-ACK-WATCHDOG", "Directive watchdog", source)...); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeliveryPolicyTest(manager, "issue", "--case", "URGENT-ACK-WATCHDOG", "--to", "eng-developer", "--next", "Implement safely"); err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(manager.Config)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, "URGENT-ACK-WATCHDOG", "issue_sent")
	worker := deliveryPolicyTestApp(t, e, "eng-developer", "directive-watchdog:worker")
	if _, err := runDeliveryPolicyTest(worker, "accept", "--event", issue.ID, "--next", "working"); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeliveryPolicyTest(manager, "message", "--to", "eng-developer", "--case", "URGENT-ACK-WATCHDOG",
		"--kind", "directive", "--urgency", "urgent", "--text", "Stop external writes and inspect the new evidence"); err != nil {
		t.Fatal(err)
	}
	events, err = NewStore(e.data).ReadAll(manager.Config)
	if err != nil {
		t.Fatal(err)
	}
	var directive Event
	for _, event := range events {
		if event.Type == "message_prepared" && event.MessageKind == "directive" {
			directive = event
		}
	}
	if directive.ID == "" {
		t.Fatal("missing urgent directive")
	}
	ledger, err := validateLedger(events, manager.Config)
	if err != nil {
		t.Fatal(err)
	}
	record := ledger.deliveries[directive.DeliveryID]
	if record == nil || record.Terminal.ID == "" {
		t.Fatalf("urgent directive is not durably sent: %+v", record)
	}
	sentAt, err := parseOperationsTime("urgent sent", record.Terminal.At)
	if err != nil {
		t.Fatal(err)
	}
	now := sentAt.Add(16 * time.Second)
	cfg := manager.Config
	cfg.DeliveryPolicy.ManagerQueueStallTimeout = "15s"
	cfg.DeliveryPolicy.ManagerQueueEscalateAfter = "30s"
	cfg.DeliveryPolicy.MaxManagerQueueNudges = 2
	writeConfigFixture(t, e.config, cfg)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "eng-developer", "done", "urgent-ack-worker")
	addOperationsLive(&control.snapshot, cfg, e.root, "zantianyou", "idle", "urgent-ack-manager")
	watchdog := operationsTestApp(t, e, control, &now)
	watchdog.Config = cfg

	if err := watchdog.runUrgentDirectiveWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts := fakePromptCalls(control)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "prompt eng-developer ") ||
		!strings.Contains(prompts[0], "hq message ack --message "+directive.MessageID) || !strings.Contains(prompts[0], "directive不修改任务合同") {
		t.Fatalf("urgent directive first reminder is not actionable: %v", prompts)
	}
	if err := watchdog.runUrgentDirectiveWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 1 {
		t.Fatalf("same urgent directive reminder duplicated: %d", got)
	}
	now = now.Add(16 * time.Second)
	if err := watchdog.runUrgentDirectiveWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if err := watchdog.runUrgentDirectiveWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts = fakePromptCalls(control)
	if len(prompts) != 3 || !strings.HasPrefix(prompts[2], "prompt zantianyou ") ||
		!strings.Contains(prompts[2], directive.MessageID) || !strings.Contains(prompts[2], "HQ未代替其确认") {
		t.Fatalf("unacked urgent directive did not escalate to direct manager: %v", prompts)
	}

	e.setActor(t, "eng-developer", "directive-watchdog:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "message", "ack", "--message", directive.MessageID)
	now = now.Add(time.Minute)
	if err := watchdog.runUrgentDirectiveWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 3 {
		t.Fatalf("acked urgent directive remained in watchdog backlog: prompts=%d", got)
	}
}

func TestActiveRevisionRejectsSubmittedAndNonIssuerWithSelfCorrectingGuidance(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "revision-reject.md"), "# Revision reject\n")
	e.setActor(t, "zantianyou", "revision:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, caseCreateArgs("ACTIVE-REVISION-REJECT", "Original", source)...)
	runTestCommand(t, e, "issue", "--case", "ACTIVE-REVISION-REJECT", "--to", "eng-developer", "--next", "Implement")
	events, _ := NewStore(e.data).ReadAll(testConfig())
	issue := latestCaseEvent(events, "ACTIVE-REVISION-REJECT", "issue_sent")

	e.setActor(t, "eng-developer", "revision:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")
	unauthorized := e.app(t)
	err := unauthorized.run(activeRevisionArgs("ACTIVE-REVISION-REJECT", source))
	if err == nil || !strings.Contains(err.Error(), "冻结 issuer/reviewer/acceptor") || !strings.Contains(err.Error(), "不要越级") {
		t.Fatalf("non-issuer revision guidance=%v", err)
	}
	runTestCommand(t, e, "report", "--case", "ACTIVE-REVISION-REJECT", "--result", "completed", "--artifact", source, "--verify", "verified", "--next", "review")

	e.setActor(t, "zantianyou", "revision:manager", filepath.Join(e.root, "engineering"))
	manager := e.app(t)
	err = manager.run(activeRevisionArgs("ACTIVE-REVISION-REJECT", source))
	if err == nil || !strings.Contains(err.Error(), "hq return --event REPORT_EVENT") || !strings.Contains(err.Error(), "requirements changed") {
		t.Fatalf("submitted revision guidance=%v", err)
	}
}

func TestActiveRevisionFailedPreSendKeepsOldContractInvalidAndRetriesReplacement(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "revision-retry.md"), "# Revision retry\n")
	e.setActor(t, "zantianyou", "revision-retry:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, caseCreateArgs("ACTIVE-REVISION-RETRY", "Original", source)...)
	runTestCommand(t, e, "issue", "--case", "ACTIVE-REVISION-RETRY", "--to", "eng-developer", "--next", "Implement")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	oldIssue := latestCaseEvent(events, "ACTIVE-REVISION-RETRY", "issue_sent")
	e.setActor(t, "eng-developer", "revision-retry:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", oldIssue.ID, "--next", "work")

	e.setActor(t, "zantianyou", "revision-retry:manager", filepath.Join(e.root, "engineering"))
	e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("offline before replacement send")
	app := e.app(t)
	if err := app.run(activeRevisionArgs("ACTIVE-REVISION-RETRY", source)); err == nil {
		t.Fatal("failed-pre-send replacement unexpectedly succeeded")
	}
	firstPayload := e.transport.calls[len(e.transport.calls)-1].message
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatalf("strict replay rejected complete atomic revision after delivery failure: %v", err)
	}
	old := ledger.assignments[oldIssue.ID]
	state := ledger.snapshot.Cases["ACTIVE-REVISION-RETRY"]
	if old == nil || !old.Consumed || old.Status != "superseded" || state == nil ||
		state.Version != 2 || state.Status != string(statusRevisionPending) || state.Owner != "zantianyou" ||
		len(ledger.activeAssignments("ACTIVE-REVISION-RETRY")) != 0 {
		t.Fatalf("failed replacement reopened stale execution authority: old=%+v state=%+v active=%d", old, state, len(ledger.activeAssignments("ACTIVE-REVISION-RETRY")))
	}
	var replacement Event
	for _, event := range ledger.events {
		if event.Type == "issue_prepared" && event.CaseID == "ACTIVE-REVISION-RETRY" && event.AuthorizationType == "revision" {
			replacement = event
		}
	}
	record := ledger.deliveries[replacement.DeliveryID]
	if replacement.ID == "" || record == nil || record.Status != deliveryFailedPreSend {
		t.Fatalf("replacement delivery fence missing: replacement=%+v delivery=%+v", replacement, record)
	}

	e.transport.result, e.transport.err = transportSent, nil
	runTestCommand(t, e, "delivery", "retry", "--id", replacement.DeliveryID)
	if got := e.transport.calls[len(e.transport.calls)-1].message; got != firstPayload {
		t.Fatal("replacement retry changed the frozen payload")
	}
	ledger, err = e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	state = ledger.snapshot.Cases["ACTIVE-REVISION-RETRY"]
	active := ledger.activeAssignments("ACTIVE-REVISION-RETRY")
	if state == nil || state.Status != string(statusDispatched) || state.Owner != "eng-developer" ||
		len(active) != 1 || active[0].AssignmentID != replacement.AssignmentID || active[0].Status != "issued" {
		t.Fatalf("replacement retry did not converge: state=%+v active=%+v", state, active)
	}
}

func TestUrgentRequirementChangeCascadesLiaisonToManagerToWorker(t *testing.T) {
	e := setupTestEnv(t)
	rootSource := writeTestFile(t, filepath.Join(e.office, "cascade-root.md"), "# Company requirement\n")
	childSource := writeTestFile(t, filepath.Join(e.root, "engineering", "cascade-child.md"), "# Department requirement\n")
	decision := writeIssueApproval(t, e, "cascade-decision.md", "DEC-CASCADE-REVISION", "CASCADE-ROOT", rootSource, "zantianyou")

	e.setActor(t, "penny", "cascade:liaison", e.office)
	runTestCommand(t, e, caseCreateArgs("CASCADE-ROOT", "Company requirement", rootSource)...)
	runTestCommand(t, e, "issue", "--case", "CASCADE-ROOT", "--to", "zantianyou", "--decision", decision, "--next", "Delegate the delivery")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	oldManagerIssue := latestCaseEvent(events, "CASCADE-ROOT", "issue_sent")

	e.setActor(t, "zantianyou", "cascade:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", oldManagerIssue.ID, "--next", "Delegate to the specialist")
	runTestCommand(t, e, "case", "create", "--id", "CASCADE-CHILD", "--title", "Department delivery", "--parent", "CASCADE-ROOT", "--source", childSource)
	runTestCommand(t, e, "issue", "--case", "CASCADE-CHILD", "--to", "eng-developer", "--next", "Implement the original department requirement")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	oldWorkerIssue := latestCaseEvent(events, "CASCADE-CHILD", "issue_sent")

	e.setActor(t, "eng-developer", "cascade:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", oldWorkerIssue.ID, "--next", "Execute the original requirement")

	// The headquarters liaison may only replace the direct manager's contract.
	// The department manager must then translate and propagate that change to
	// the worker's own child case; HQ never skips the frozen reporting line.
	e.setActor(t, "penny", "cascade:liaison", e.office)
	runTestCommand(t, e, activeRevisionArgs("CASCADE-ROOT", rootSource)...)
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	newManagerIssue := latestCaseEvent(events, "CASCADE-ROOT", "issue_sent")
	if newManagerIssue.AssignmentID == oldManagerIssue.AssignmentID || newManagerIssue.SupersedesAssignmentID != oldManagerIssue.AssignmentID {
		t.Fatalf("liaison did not replace the manager assignment: old=%+v new=%+v", oldManagerIssue, newManagerIssue)
	}

	e.setActor(t, "zantianyou", "cascade:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", newManagerIssue.ID, "--next", "Propagate the changed requirement to the specialist")
	runTestCommand(t, e, activeRevisionArgs("CASCADE-CHILD", childSource)...)
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	newWorkerIssue := latestCaseEvent(events, "CASCADE-CHILD", "issue_sent")
	if newWorkerIssue.AssignmentID == oldWorkerIssue.AssignmentID || newWorkerIssue.SupersedesAssignmentID != oldWorkerIssue.AssignmentID {
		t.Fatalf("manager did not replace the worker assignment: old=%+v new=%+v", oldWorkerIssue, newWorkerIssue)
	}
	ledger, err := validateLedger(events, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	oldManager := ledger.assignments[oldManagerIssue.ID]
	oldWorker := ledger.assignments[oldWorkerIssue.ID]
	if oldManager == nil || oldWorker == nil || oldManager.Status != "superseded" || oldWorker.Status != "superseded" ||
		!oldManager.Consumed || !oldWorker.Consumed {
		t.Fatalf("cascade left stale executable contracts: manager=%+v worker=%+v", oldManager, oldWorker)
	}
	root := ledger.snapshot.Cases["CASCADE-ROOT"]
	child := ledger.snapshot.Cases["CASCADE-CHILD"]
	if root == nil || child == nil || root.Version != 2 || child.Version != 2 ||
		root.Owner != "zantianyou" || child.Owner != "eng-developer" ||
		root.Status != string(statusInProgress) || child.Status != string(statusDispatched) {
		t.Fatalf("cascade projections are inconsistent: root=%+v child=%+v", root, child)
	}
	if got := e.transport.calls[len(e.transport.calls)-1].target; got != "eng-developer" {
		t.Fatalf("second hop did not target the manager's direct report: %s", got)
	}
}

func TestActiveRevisionRacesReportWithoutCreatingTwoExecutableContracts(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "revision-report-race.md"), "# Revision report race\n")
	manager := deliveryPolicyTestApp(t, e, "zantianyou", "revision-race:manager")
	if _, err := runDeliveryPolicyTest(manager, caseCreateArgs("ACTIVE-REVISION-RACE", "Original", source)...); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeliveryPolicyTest(manager, "issue", "--case", "ACTIVE-REVISION-RACE", "--to", "eng-developer", "--next", "Implement"); err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(manager.Config)
	if err != nil {
		t.Fatal(err)
	}
	oldIssue := latestCaseEvent(events, "ACTIVE-REVISION-RACE", "issue_sent")
	worker := deliveryPolicyTestApp(t, e, "eng-developer", "revision-race:worker")
	if _, err := runDeliveryPolicyTest(worker, "accept", "--event", oldIssue.ID, "--next", "work"); err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		<-start
		results <- manager.run(activeRevisionArgs("ACTIVE-REVISION-RACE", source))
	}()
	go func() {
		<-start
		results <- worker.run([]string{"report", "--case", "ACTIVE-REVISION-RACE", "--result", "completed", "--artifact", source, "--verify", "verified", "--next", "review"})
	}()
	close(start)
	first, second := <-results, <-results
	if (first == nil) == (second == nil) {
		t.Fatalf("revision/report race must have exactly one winner: first=%v second=%v", first, second)
	}
	ledger, err := manager.ledgerState()
	if err != nil {
		t.Fatalf("race left ledger unreplayable: %v", err)
	}
	active := ledger.activeAssignments("ACTIVE-REVISION-RACE")
	if len(active) != 1 {
		t.Fatalf("race created missing or duplicate executable contracts: %+v", active)
	}
	old := ledger.assignments[oldIssue.ID]
	state := ledger.snapshot.Cases["ACTIVE-REVISION-RACE"]
	if old == nil || state == nil {
		t.Fatalf("race projection missing: old=%+v state=%+v", old, state)
	}
	if old.Status == "superseded" {
		if active[0].AssignmentID == old.AssignmentID || state.Version != 2 || state.Status != string(statusDispatched) {
			t.Fatalf("revision winner did not converge to one replacement: old=%+v active=%+v state=%+v", old, active, state)
		}
	} else if old.Status != "submitted" || active[0].AssignmentID != old.AssignmentID || state.Status != string(statusReported) {
		t.Fatalf("report winner did not preserve the original review contract: old=%+v active=%+v state=%+v", old, active, state)
	}
}
