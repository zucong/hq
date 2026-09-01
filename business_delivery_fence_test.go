package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testBusinessIssueEvent(app *App, ledger *ledgerState, actorName, targetName, caseID, suffix string) (Event, error) {
	state, err := ledger.currentCase(caseID)
	if err != nil {
		return Event{}, err
	}
	actorRule, ok := app.Config.exactRule(actorName)
	if !ok {
		return Event{}, fmt.Errorf("actor missing: %s", actorName)
	}
	targetRule, ok := app.Config.exactRule(targetName)
	if !ok {
		return Event{}, fmt.Errorf("target missing: %s", targetName)
	}
	actor := Actor{Name: actorRule.Name, Label: actorRule.Label, Rule: actorRule, PaneID: "fence:" + actorName, CWD: app.HQRoot}
	event, err := app.newEvent(actor, "issue_prepared", caseID)
	if err != nil {
		return Event{}, err
	}
	event.FromState, event.Project = state.Status, state.Project
	event.Recipient, event.RecipientLabel = targetRule.Name, targetRule.Label
	event.CaseVersion, event.CaseDigest = state.Version, state.Digest
	setTestAssignmentContract(&event, state)
	event.NextAction, event.Note = "fence test "+suffix, "second business delivery must be fenced"
	event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "issue-fixed"
	if app.Config.isManager(actorRule) && targetRule.ReportsTo == actorRule.Name {
		event.AuthorizationType = "manager"
		event.AuthorizationDigest = requestDigest("manager", actorRule.Name, targetRule.Name)
	} else {
		event.AuthorizationType = "standing_decision"
		event.AuthorizationDigest = digestText("fence-decision:" + suffix)
		event.DecisionRef = "test:/fence-decision"
	}
	event.Delivery = deliveryPrepared
	event.DeliveryID = stableDeliveryID(stableCommandID("fence-issue", caseID, suffix), targetRule.Name)
	payload, err := app.deliveryPayload(event)
	if err != nil {
		return Event{}, err
	}
	event.PayloadDigest = digestText(payload)
	return event, nil
}

func attemptTestBusinessIssue(app *App, actorName, targetName, caseID, suffix string) error {
	commandID := stableCommandID("fence-conflicting-issue", caseID, suffix)
	_, err := app.transact(commandID, requestDigest("fence-conflicting-issue", caseID, suffix), func(ledger *ledgerState) (Event, error) {
		return testBusinessIssueEvent(app, ledger, actorName, targetName, caseID, suffix)
	})
	return err
}

func testCaseRevisionEvent(app *App, ledger *ledgerState, actorName, caseID, suffix string) (Event, error) {
	state, err := ledger.currentCase(caseID)
	if err != nil {
		return Event{}, err
	}
	rule, ok := app.Config.exactRule(actorName)
	if !ok {
		return Event{}, fmt.Errorf("actor missing: %s", actorName)
	}
	spec := CaseSpec{
		CaseID: caseID, ParentCaseID: state.ParentCaseID, RootCaseID: state.RootCaseID,
		Title: state.Title + " " + suffix, Project: state.Project,
		Objective: state.Objective, Acceptance: state.Acceptance, Constraints: state.Constraints,
		Priority: state.Priority, SpecRef: state.SpecRef, SourceRef: state.SourceRef,
		Version: state.Version + 1,
	}
	spec.Digest = caseSpecDigest(spec)
	event, err := app.newEvent(Actor{Name: rule.Name, Label: rule.Label, Rule: rule, PaneID: "fence:" + actorName}, "case_revised", caseID)
	if err != nil {
		return Event{}, err
	}
	basis := state.SpecEventID
	if basis == "" {
		basis = state.LastEventID
	}
	event.RelatedEventID, event.PreviousCaseDigest = basis, state.Digest
	event.ParentCaseID, event.RootCaseID = spec.ParentCaseID, spec.RootCaseID
	event.Title, event.Project, event.SourceRef = spec.Title, spec.Project, spec.SourceRef
	event.Objective, event.Acceptance, event.Constraints, event.Priority, event.SpecRef = spec.Objective, spec.Acceptance, spec.Constraints, spec.Priority, spec.SpecRef
	event.CaseVersion, event.CaseDigest = spec.Version, spec.Digest
	return event, nil
}

func attemptTestCaseRevision(app *App, actorName, caseID, suffix string) error {
	commandID := stableCommandID("fence-conflicting-revise", caseID, suffix)
	_, err := app.transact(commandID, requestDigest("fence-conflicting-revise", caseID, suffix), func(ledger *ledgerState) (Event, error) {
		return testCaseRevisionEvent(app, ledger, actorName, caseID, suffix)
	})
	return err
}

func attemptTestCaseClose(app *App, actorName, caseID, suffix string) error {
	commandID := stableCommandID("fence-conflicting-close", caseID, suffix)
	_, err := app.transact(commandID, requestDigest("fence-conflicting-close", caseID, suffix), func(ledger *ledgerState) (Event, error) {
		state, err := ledger.currentCase(caseID)
		if err != nil {
			return Event{}, err
		}
		rule, ok := app.Config.exactRule(actorName)
		if !ok {
			return Event{}, fmt.Errorf("actor missing: %s", actorName)
		}
		event, err := app.newEvent(Actor{Name: rule.Name, Label: rule.Label, Rule: rule, PaneID: "fence:" + actorName}, "case_closed", caseID)
		if err != nil {
			return Event{}, err
		}
		event.FromState, event.ToState = state.Status, string(statusClosed)
		event.SourceRef, event.Note, event.NextAction = state.SourceRef, "fence close "+suffix, "none"
		return event, nil
	})
	return err
}

func assertBusinessFenceRejectsWithoutEffects(t *testing.T, e testEnv, deliveryID string, call func() error) {
	t.Helper()
	before := snapshotTree(t, e.data)
	transportBefore := len(e.transport.calls)
	err := call()
	if err == nil || exitCodeForError(err) != exitConflict || !strings.Contains(err.Error(), "business delivery fence") ||
		!strings.Contains(err.Error(), deliveryID) || !strings.Contains(err.Error(), "retry/resolve") {
		t.Fatalf("business fence did not reject with recovery guidance: %v", err)
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) {
		t.Fatalf("rejected mutation changed ledger\nbefore=%v\nafter=%v", before, after)
	}
	if len(e.transport.calls) != transportBefore {
		t.Fatalf("rejected mutation reached transport: %d -> %d", transportBefore, len(e.transport.calls))
	}
}

func forceIssueFenceStatus(t *testing.T, status string) (testEnv, *App, Event) {
	t.Helper()
	e := setupTestEnv(t)
	app, origin := createPreparedIssueOrigin(t, e, "FENCE-ISSUE-"+strings.ToUpper(strings.ReplaceAll(status, "_", "-")))
	switch status {
	case deliveryPrepared:
	case deliveryAttempted:
		app.DeliveryFailpoint = func(name string) error {
			if name == "after_attempt_recorded" {
				return errors.New("stop after durable attempt")
			}
			return nil
		}
		if _, err := app.processDelivery(origin, ""); err == nil {
			t.Fatal("attempt failpoint did not interrupt delivery")
		}
		app.DeliveryFailpoint = nil
	case deliveryFailedPreSend:
		e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("definitely not sent")
		if _, err := app.processDelivery(origin, ""); err == nil {
			t.Fatal("failed_pre_send fixture unexpectedly succeeded")
		}
	case deliveryUnknown, "resolved_not_sent":
		e.transport.result, e.transport.err = transportAmbiguous, errors.New("ambiguous transport")
		if _, err := app.processDelivery(origin, ""); err == nil {
			t.Fatal("unknown fixture unexpectedly succeeded")
		}
		if status == "resolved_not_sent" {
			evidence := writeTestFile(t, filepath.Join(e.office, "fence-not-sent.md"), "# confirmed absent\n")
			if err := app.run([]string{"delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "not-delivered", "--reason", "receiver confirmed absent", "--evidence", evidence}); err != nil {
				t.Fatal(err)
			}
		}
	default:
		t.Fatalf("unsupported fence status %q", status)
	}
	want := status
	if status == "resolved_not_sent" {
		want = deliveryFailedPreSend
	}
	view, ok, err := app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || !ok || view.Status != want {
		t.Fatalf("fence fixture status=%s want=%s view=%+v ok=%t err=%v", status, want, view, ok, err)
	}
	return e, app, origin
}

func TestBusinessDeliveryFenceBlocksIssueReviseAndCloseAcrossPendingStates(t *testing.T) {
	for _, status := range []string{deliveryPrepared, deliveryAttempted, deliveryFailedPreSend, deliveryUnknown, "resolved_not_sent"} {
		t.Run(status, func(t *testing.T) {
			e, app, origin := forceIssueFenceStatus(t, status)
			childSuffix := strings.ToUpper(strings.ReplaceAll(status, "_", "-"))
			childSource := writeTestFile(t, filepath.Join(e.office, "fence-child-"+status+".md"), "# child source\n")
			assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
				return attemptTestBusinessIssue(app, "penny", "zantianyou", origin.CaseID, status+"-issue")
			})
			assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
				return attemptTestCaseRevision(app, "penny", origin.CaseID, status+"-revise")
			})
			assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
				return attemptTestCaseClose(app, "penny", origin.CaseID, status+"-close")
			})
			assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
				return app.run([]string{"case", "create", "--id", "FENCE-CHILD-" + childSuffix, "--parent", origin.CaseID,
					"--title", "blocked child split", "--source", childSource})
			})
		})
	}
}

func TestOwnerReportUnknownFenceBlocksIssueReviseAndClose(t *testing.T) {
	e := setupTestEnv(t)
	caseID := "FENCE-OWNER-REPORT-UNKNOWN"
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "owner-report-source.md"), "# source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "owner-report-artifact.md"), "# artifact\n")
	artifact2 := writeTestFile(t, filepath.Join(e.root, "engineering", "owner-report-artifact-2.md"), "# changed artifact\n")
	e.setActor(t, "zantianyou", "fence:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "owner report fence", "--source", source)
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("owner report ambiguous")
	app := e.app(t)
	if err := app.run([]string{"report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--next", "review"}); err == nil {
		t.Fatal("owner report unknown fixture unexpectedly succeeded")
	}
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	origin := latestEventOfType(events, "report_prepared")
	if origin.ID == "" {
		t.Fatal("owner report origin missing")
	}
	assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
		return app.run([]string{"report", "--case", caseID, "--result", "completed", "--artifact", artifact2, "--next", "changed report"})
	})
	assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
		return attemptTestBusinessIssue(app, "zantianyou", "eng-data-engineer", caseID, "owner-report-issue")
	})
	assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
		return attemptTestCaseRevision(app, "zantianyou", caseID, "owner-report-revise")
	})
	assertBusinessFenceRejectsWithoutEffects(t, e, origin.DeliveryID, func() error {
		return attemptTestCaseClose(app, "penny", caseID, "owner-report-close")
	})
}

func TestBusinessDeliveryFenceNormalFailedRetryConvergesAndClears(t *testing.T) {
	e, app, origin := forceIssueFenceStatus(t, deliveryFailedPreSend)
	transportBefore := len(e.transport.calls)
	e.transport.result, e.transport.err = transportSent, nil
	outcome, err := app.processDelivery(origin, "delivery-retry:fence-normal")
	if err != nil || outcome.DeliveryStatus != deliverySent || !outcome.CaseStateApplied {
		t.Fatalf("normal fenced retry did not converge: outcome=%+v err=%v", outcome, err)
	}
	if len(e.transport.calls) != transportBefore+1 {
		t.Fatalf("normal retry transport calls=%d want=%d", len(e.transport.calls), transportBefore+1)
	}
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	if fences := ledger.applicableBusinessDeliveryFences(origin.CaseID); len(fences) != 0 {
		t.Fatalf("sent terminal did not clear its fence: %v", businessDeliveryFenceIDs(fences))
	}
	state := ledger.snapshot.Cases[origin.CaseID]
	if state == nil || state.Status != string(statusDispatched) || state.Owner != origin.Recipient {
		t.Fatalf("sent retry did not apply business state: %+v", state)
	}
}

func appendForgedFenceEvent(t *testing.T, events []Event, event Event, tag string) []Event {
	t.Helper()
	last := events[len(events)-1]
	event.Version = eventSchemaVersion
	event.Sequence = last.Sequence + 1
	event.At = last.At
	event.CommandID = stableCommandID("forged-fence", tag)
	event.CommandDigest = requestDigest("forged-fence", tag)
	event.PreviousEventHash = last.EventHash
	event.EventHash = ""
	hash, err := hashEvent(event)
	if err != nil {
		t.Fatal(err)
	}
	event.EventHash = hash
	return append(events, event)
}

func TestDeliveryResolveCommandBindsEachUnknownAttempt(t *testing.T) {
	e, app, origin, deliveryErr := runIssueWithOutcome(t, transportAmbiguous, errors.New("first timeout"), nil)
	if deliveryErr == nil {
		t.Fatal("first ambiguous attempt unexpectedly succeeded")
	}
	evidence1 := writeTestFile(t, filepath.Join(e.office, "resolve-attempt-one.md"), "# first absent\n")
	evidence2 := writeTestFile(t, filepath.Join(e.office, "resolve-attempt-two.md"), "# second absent\n")
	e.setActor(t, "penny", "resolve-attempt:operations", e.office)
	app = e.app(t)
	if err := app.run([]string{"delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "not-delivered", "--reason", "first absent", "--evidence", evidence1}); err != nil {
		t.Fatal(err)
	}
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("second timeout")
	if err := app.run([]string{"delivery", "retry", "--id", origin.DeliveryID}); err == nil {
		t.Fatal("second ambiguous attempt unexpectedly succeeded")
	}
	if err := app.run([]string{"delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "not-delivered", "--reason", "second absent", "--evidence", evidence2}); err != nil {
		t.Fatalf("second unknown attempt reused/conflicted with first resolution: %v", err)
	}
	events, err := app.Store.ReadAll(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	var resolutions []Event
	for _, event := range events {
		if event.Type == "delivery_resolved_not_sent" && event.DeliveryID == origin.DeliveryID {
			resolutions = append(resolutions, event)
		}
	}
	if len(resolutions) != 2 || resolutions[0].AttemptEventID == resolutions[1].AttemptEventID || resolutions[0].CommandID == resolutions[1].CommandID {
		t.Fatalf("resolve commands not bound to distinct attempts: %+v", resolutions)
	}
}

func TestDeliveryOperationLockSerializesAttemptTransportTerminalAndReconcile(t *testing.T) {
	e := setupTestEnv(t)
	deliverApp, origin := createPreparedIssueOrigin(t, e, "FENCE-DELIVERY-OPERATION-LOCK")
	entered := make(chan struct{})
	release := make(chan struct{})
	deliverApp.DeliveryFailpoint = func(name string) error {
		if name == "after_attempt_recorded" {
			close(entered)
			<-release
		}
		return nil
	}
	e.transport.result, e.transport.err = transportSent, nil
	deliverDone := make(chan error, 1)
	go func() {
		_, err := deliverApp.processDelivery(origin, "")
		deliverDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery did not pause after durable attempt")
	}
	view, ok, err := deliverApp.Store.Delivery(deliverApp.Config, origin.DeliveryID)
	if err != nil || !ok || view.Status != deliveryAttempted {
		t.Fatalf("barrier delivery status=%+v ok=%t err=%v", view, ok, err)
	}

	concurrentApp := e.app(t)
	concurrentDone := make(chan error, 1)
	go func() {
		_, err := concurrentApp.processDelivery(origin, "")
		concurrentDone <- err
	}()
	reconcileApp := e.app(t)
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- reconcileApp.reconcileDeliveries() }()
	for name, done := range map[string]<-chan error{"concurrent delivery": concurrentDone, "reconcile": reconcileDone} {
		select {
		case early := <-done:
			close(release)
			<-deliverDone
			t.Fatalf("%s overtook live operation lock: %v", name, early)
		case <-time.After(150 * time.Millisecond):
		}
	}
	view, ok, err = deliverApp.Store.Delivery(deliverApp.Config, origin.DeliveryID)
	if err != nil || !ok || view.Status != deliveryAttempted || len(e.transport.calls) != 0 {
		close(release)
		<-deliverDone
		t.Fatalf("blocked competitors changed attempt: view=%+v ok=%t err=%v transport=%d", view, ok, err, len(e.transport.calls))
	}

	close(release)
	if err := <-deliverDone; err != nil {
		t.Fatalf("live delivery failed after barrier: %v", err)
	}
	if err := <-concurrentDone; err != nil {
		t.Fatalf("serialized duplicate was not idempotent: %v", err)
	}
	if err := <-reconcileDone; err != nil {
		t.Fatalf("serialized reconcile failed: %v", err)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("operation lock transport calls=%d want=1", len(e.transport.calls))
	}
	events, err := deliverApp.Store.ReadAll(deliverApp.Config)
	if err != nil {
		t.Fatal(err)
	}
	if countCaseEvents(events, origin.CaseID, "delivery_attempted") != 1 ||
		countCaseEvents(events, origin.CaseID, "issue_sent") != 1 || countCaseEvents(events, origin.CaseID, "delivery_unknown") != 0 {
		t.Fatalf("operation lock terminal history invalid: %+v", events)
	}
}

func TestAssignmentReworkRejectsSecondIssueAndForgedReplay(t *testing.T) {
	e := setupTestEnv(t)
	caseID := "FENCE-ASSIGNMENT-REWORK"
	source := writeTestFile(t, filepath.Join(e.office, "rework-source.md"), "# source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "rework-artifact.md"), "# artifact\n")
	decision := writeIssueApproval(t, e, "rework-decision.md", "DEC-FENCE-REWORK", caseID, source, "zantianyou")
	e.setActor(t, "penny", "fence:rework:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "assignment rework fence", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issue := latestEventOfType(events, "issue_sent")
	e.setActor(t, "zantianyou", "fence:rework:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "implement")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "checked", "--next", "review")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	report := latestEventOfType(events, "report_sent")
	e.setActor(t, "penny", "fence:rework:penny", e.office)
	runTestCommand(t, e, "return", "--event", report.ID, "--reason", "needs rework", "--next", "resubmit")

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[issue.ID]
	if assignment == nil || assignment.Consumed || assignment.Status != "rework" {
		t.Fatalf("assignment did not enter active rework: %+v", assignment)
	}
	e.setActor(t, "zantianyou", "fence:rework:manager", filepath.Join(e.root, "engineering"))
	app := e.app(t)
	// issue admission now takes a per-seat runtime lease before any business
	// write. Prime that coordination file so the zero-business-side-effect
	// assertion below compares like with like rather than treating lock metadata
	// as a rejected issue side effect.
	releaseSeat, err := app.lockRuntimeSeat("eng-data-engineer")
	if err != nil {
		t.Fatal(err)
	}
	releaseSeat()
	before := snapshotTree(t, e.data)
	transportBefore := len(e.transport.calls)
	err = app.run([]string{"issue", "--case", caseID, "--to", "eng-data-engineer", "--next", "create second contract"})
	if err == nil || exitCodeForError(err) != exitConflict || !strings.Contains(err.Error(), "未完成 assignment contract") {
		t.Fatalf("assignment rework allowed a second issue: %v", err)
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) || len(e.transport.calls) != transportBefore {
		t.Fatalf("rejected second issue had effects: tree_changed=%t transport=%d->%d", !reflect.DeepEqual(after, before), transportBefore, len(e.transport.calls))
	}

	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err = validateLedger(events, testConfig())
	if err != nil {
		t.Fatal(err)
	}
	forged, err := testBusinessIssueEvent(app, ledger, "zantianyou", "eng-data-engineer", caseID, "forged-rework")
	if err != nil {
		t.Fatal(err)
	}
	events = appendForgedFenceEvent(t, events, forged, "assignment-rework")
	path := writeRawEvents(t, e.data, events)
	if _, err := NewStore(e.data).ReadAll(testConfig()); err == nil || !strings.Contains(err.Error(), "未完成 assignment contract") || !strings.Contains(err.Error(), path+":") {
		t.Fatalf("strict replay accepted forged second assignment: %v", err)
	}
}

func TestReportPreparedCannotForgeQuietBusinessDelivery(t *testing.T) {
	e := setupTestEnv(t)
	caseID := "FENCE-REPORT-QUIET-FORGE"
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "quiet-report-source.md"), "# source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "quiet-report-artifact.md"), "# artifact\n")
	e.setActor(t, "zantianyou", "quiet-report:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "quiet report forge", "--source", source)
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("capture report origin")
	if err := e.app(t).run([]string{"report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--next", "review"}); err == nil {
		t.Fatal("report capture unexpectedly succeeded")
	}
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for i := range events {
		if events[i].DeliveryID != "" && events[i].CaseID == caseID {
			events[i].DeliveryMode, events[i].DeliveryTarget = deliveryModeQuiet, deliveryTargetNextTurn
		}
	}
	rehashEventChain(t, events)
	path := writeRawEvents(t, e.data, events)
	if _, err := NewStore(e.data).ReadAll(testConfig()); err == nil || !strings.Contains(err.Error(), "report 固定 wakeup") || !strings.Contains(err.Error(), path+":") {
		t.Fatalf("strict replay accepted quiet report origin: %v", err)
	}
}
