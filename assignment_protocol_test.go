package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func issuedAssignmentFixture(t *testing.T, caseID string) (testEnv, []Event) {
	t.Helper()
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, caseID+"-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, caseID+"-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, caseID+"-decision.md", "DEC-"+caseID, caseID, order, "zantianyou")
	e.setActor(t, "penny", "protocol:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "assignment protocol", "--project", "protocol", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	return e, events
}

func submittedAssignmentFixture(t *testing.T, caseID string) (testEnv, Event) {
	t.Helper()
	e, events := issuedAssignmentFixture(t, caseID)
	issue := latestEventOfType(events, "issue_sent")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-result.md"), "# result\n")
	e.setActor(t, "zantianyou", "submitted:assignee", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact,
		"--verify", "checked", "--next", "review")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	return e, latestCaseEvent(events, caseID, "report_sent")
}

func directlyRebindAssignmentAssignee(t *testing.T, e testEnv) Config {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Agents {
		if cfg.Agents[index].Name != "zantianyou" {
			continue
		}
		cfg.Agents[index].ActivationPolicy = activationAlways
		finalizeTestSeatMutation(&cfg.Agents[index])
	}
	writeConfigFixture(t, e.config, cfg)
	return cfg
}

func rewriteProtocolFixture(t *testing.T, e testEnv, events []Event) error {
	t.Helper()
	rehashEventChain(t, events)
	writeRawEvents(t, e.data, events)
	_, err := NewStore(e.data).ReadAll(testConfig())
	return err
}

func TestAssignmentEventVersionContractsFailClosed(t *testing.T) {
	t.Run("v3 issue cannot omit assignment", func(t *testing.T) {
		e, events := issuedAssignmentFixture(t, "SCHEMA-ASSIGNMENT-MISSING")
		for index := range events {
			if events[index].Type == "issue_prepared" {
				events[index].AssignmentID = ""
				events[index].AssignmentDigest = ""
				events[index].AssignmentIssuer = ""
				events[index].Reviewer, events[index].ReviewerLabel = "", ""
				events[index].Acceptor, events[index].AcceptorLabel, events[index].DueAt = "", "", ""
				break
			}
		}
		err := rewriteProtocolFixture(t, e, events)
		if err == nil || !strings.Contains(err.Error(), "必须携带 assignment_id") {
			t.Fatalf("v3 issue without assignment accepted: %v", err)
		}
	})

	t.Run("v2 is rejected", func(t *testing.T) {
		e, events := issuedAssignmentFixture(t, "SCHEMA-V2-DOWNGRADE")
		for index := range events {
			events[index].Version = 2
		}
		err := rewriteProtocolFixture(t, e, events)
		if err == nil || !strings.Contains(err.Error(), "不支持事件版本 2") {
			t.Fatalf("v2 ledger accepted: %v", err)
		}
	})

	t.Run("v3 acceptance requires receipt", func(t *testing.T) {
		e, events := issuedAssignmentFixture(t, "SCHEMA-RECEIPT-MISSING")
		issueSent := latestEventOfType(events, "issue_sent")
		e.setActor(t, "zantianyou", "protocol:manager", filepath.Join(e.root, "engineering"))
		runTestCommand(t, e, "accept", "--event", issueSent.ID, "--next", "work")
		events, err := NewStore(e.data).ReadAll(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		for index := range events {
			if events[index].Type == "event_accepted" {
				events[index].AcceptanceDigest = ""
				break
			}
		}
		err = rewriteProtocolFixture(t, e, events)
		if err == nil || !strings.Contains(err.Error(), "必须携带 acceptance_digest") {
			t.Fatalf("v3 acceptance without receipt accepted: %v", err)
		}
	})
}

func TestAssignmentSemanticTamperingFailsAfterRehash(t *testing.T) {
	t.Run("unauthorized acceptor", func(t *testing.T) {
		e, events := issuedAssignmentFixture(t, "ASSIGNMENT-TAMPER-ACCEPTOR")
		for index := range events {
			if events[index].Type == "issue_prepared" || events[index].Type == "issue_sent" {
				events[index].Acceptor, events[index].AcceptorLabel = "baogong", "质量与用户体验部-包公"
				events[index].AssignmentDigest = assignmentContractDigest(events[index])
			}
		}
		err := rewriteProtocolFixture(t, e, events)
		if err == nil || !strings.Contains(err.Error(), "必须等于已授权 issuer") {
			t.Fatalf("re-signed unauthorized acceptor accepted: %v", err)
		}
	})

	t.Run("project drift", func(t *testing.T) {
		e, events := issuedAssignmentFixture(t, "ASSIGNMENT-TAMPER-PROJECT")
		for index := range events {
			if events[index].Type == "issue_prepared" || events[index].Type == "issue_sent" {
				events[index].Project = "different-project"
				events[index].AssignmentDigest = assignmentContractDigest(events[index])
			}
		}
		err := rewriteProtocolFixture(t, e, events)
		if err == nil || !strings.Contains(err.Error(), "必须冻结当前 case.project") {
			t.Fatalf("re-signed project drift accepted: %v", err)
		}
	})
}

func TestActiveAssignmentBlocksClose(t *testing.T) {
	e, before := issuedAssignmentFixture(t, "ASSIGNMENT-CLOSE-GUARD")
	e.setActor(t, "penny", "protocol:penny", e.office)
	err := runAssignmentCommandError(t, e, "close", "--case", "ASSIGNMENT-CLOSE-GUARD", "--reason", "premature", "--source", filepath.Join(e.office, "ASSIGNMENT-CLOSE-GUARD-source.md"))
	if err == nil || !strings.Contains(err.Error(), "未完成 assignment contract") {
		t.Fatalf("active assignment allowed close: %v", err)
	}
	after, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) {
		t.Fatalf("rejected close wrote events: %d -> %d", len(before), len(after))
	}
}

func TestAssignmentAndReportIdempotencyKeysIncludeGeneration(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "generation-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "generation-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "generation-decision.md", "DEC-GENERATION", "ASSIGNMENT-GENERATION", order, "zantianyou")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "generation-artifact.md"), "# result\n")

	e.setActor(t, "penny", "generation:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", "ASSIGNMENT-GENERATION", "--title", "generation", "--project", "generation", "--source", source)
	runTestCommand(t, e, "issue", "--case", "ASSIGNMENT-GENERATION", "--to", "zantianyou", "--decision", decision, "--next", "work")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	firstIssue := latestEventOfType(events, "issue_sent")

	e.setActor(t, "zantianyou", "generation:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", firstIssue.ID, "--next", "work")
	runTestCommand(t, e, "report", "--case", "ASSIGNMENT-GENERATION", "--result", "completed", "--artifact", artifact, "--next", "review")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	firstReport := latestEventOfType(events, "report_sent")

	e.setActor(t, "penny", "generation:penny", e.office)
	runTestCommand(t, e, "accept", "--event", firstReport.ID, "--next", "revise")
	runTestCommand(t, e, "case", "revise", "--id", "ASSIGNMENT-GENERATION", "--title", "generation", "--version", "2", "--source", source)
	runTestCommand(t, e, "issue", "--case", "ASSIGNMENT-GENERATION", "--to", "zantianyou", "--decision", decision, "--next", "work")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	secondIssue := latestEventOfType(events, "issue_sent")
	if secondIssue.ID == firstIssue.ID || secondIssue.AssignmentID == firstIssue.AssignmentID || secondIssue.CommandID == firstIssue.CommandID {
		t.Fatalf("new case generation was swallowed by old issue command: first=%+v second=%+v", firstIssue, secondIssue)
	}

	e.setActor(t, "zantianyou", "generation:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", secondIssue.ID, "--next", "work")
	runTestCommand(t, e, "report", "--case", "ASSIGNMENT-GENERATION", "--result", "completed", "--artifact", artifact, "--next", "review")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	reports := []Event{}
	for _, event := range events {
		if event.Type == "report_sent" {
			reports = append(reports, event)
		}
	}
	if len(reports) != 2 || reports[0].CommandID == reports[1].CommandID || reports[1].AssignmentID != secondIssue.AssignmentID {
		t.Fatalf("new assignment report was swallowed by old submission: %+v", reports)
	}
	for index := range events {
		if (events[index].Type == "issue_prepared" || events[index].Type == "issue_sent") && events[index].AssignmentID == secondIssue.AssignmentID {
			events[index].AssignmentID = firstIssue.AssignmentID
			events[index].AssignmentDigest = assignmentContractDigest(events[index])
		}
	}
	err = rewriteProtocolFixture(t, e, events)
	if err == nil || !strings.Contains(err.Error(), "assignment_id 重复") {
		t.Fatalf("duplicate re-signed assignment id accepted: %v", err)
	}
}

func countCaseEvents(events []Event, caseID, eventType string) int {
	count := 0
	for _, event := range events {
		if event.CaseID == caseID && event.Type == eventType {
			count++
		}
	}
	return count
}

func TestAssignmentReportCommandIsStableWithinSubmissionRound(t *testing.T) {
	for _, test := range []struct {
		name       string
		outcome    TransportOutcome
		transport  error
		wantError  bool
		wantStatus string
	}{
		{name: "sent", outcome: transportSent, wantStatus: "submitted"},
		{name: "definitely not sent", outcome: transportDefinitelyNotSent, transport: errors.New("offline"), wantError: true, wantStatus: "accepted"},
		{name: "ambiguous", outcome: transportAmbiguous, transport: errors.New("timeout"), wantError: true, wantStatus: "accepted"},
	} {
		t.Run(test.name, func(t *testing.T) {
			caseID := "REPORT-STABLE-" + strings.ToUpper(strings.ReplaceAll(test.name, " ", "-"))
			e, events := issuedAssignmentFixture(t, caseID)
			issue := latestEventOfType(events, "issue_sent")
			artifact := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+".md"), "# result\n")
			artifact2 := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-changed.md"), "# changed result\n")
			e.setActor(t, "zantianyou", "stable:manager", filepath.Join(e.root, "engineering"))
			runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")
			e.transport.result, e.transport.err = test.outcome, test.transport
			command := []string{"report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "checked", "--next", "review"}
			firstErr := runAssignmentCommandError(t, e, command...)
			if (firstErr != nil) != test.wantError {
				t.Fatalf("first report err=%v wantError=%t", firstErr, test.wantError)
			}
			firstEvents, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			firstCalls := len(e.transport.calls)
			secondErr := runAssignmentCommandError(t, e, command...)
			if (secondErr != nil) != test.wantError {
				t.Fatalf("idempotent report err=%v wantError=%t", secondErr, test.wantError)
			}
			secondEvents, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			if len(secondEvents) != len(firstEvents) || len(e.transport.calls) != firstCalls || countCaseEvents(secondEvents, caseID, "report_prepared") != 1 {
				t.Fatalf("same report created a second submission: events=%d/%d calls=%d/%d", len(firstEvents), len(secondEvents), firstCalls, len(e.transport.calls))
			}

			changed := []string{"report", "--case", caseID, "--result", "completed", "--artifact", artifact2, "--verify", "changed", "--next", "review"}
			changedErr := runAssignmentCommandError(t, e, changed...)
			if changedErr == nil || (!strings.Contains(changedErr.Error(), "不可创建新 submission") &&
				!strings.Contains(changedErr.Error(), "本轮已有 submission") && !strings.Contains(changedErr.Error(), "business delivery fence")) {
				t.Fatalf("different report bypassed reserved submission: %v", changedErr)
			}
			afterChanged, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			if len(afterChanged) != len(secondEvents) || len(e.transport.calls) != firstCalls {
				t.Fatalf("rejected second submission had side effects")
			}
			ledger, err := e.app(t).ledgerState()
			if err != nil {
				t.Fatal(err)
			}
			assignment := ledger.assignments[issue.ID]
			if assignment == nil || assignment.Status != test.wantStatus || assignment.Consumed {
				t.Fatalf("assignment status after report=%+v want=%s", assignment, test.wantStatus)
			}
		})
	}
}

func TestOwnerReportCommandIsStableWithinBusinessRound(t *testing.T) {
	for _, test := range []struct {
		name      string
		outcome   TransportOutcome
		transport error
	}{
		{name: "definitely not sent", outcome: transportDefinitelyNotSent, transport: errors.New("offline")},
		{name: "ambiguous", outcome: transportAmbiguous, transport: errors.New("timeout")},
	} {
		t.Run(test.name, func(t *testing.T) {
			caseID := "OWNER-REPORT-STABLE-" + strings.ToUpper(strings.ReplaceAll(test.name, " ", "-"))
			e := setupTestEnv(t)
			source := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-source.md"), "# source\n")
			artifact := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+".md"), "# result\n")
			artifact2 := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-changed.md"), "# changed\n")
			e.setActor(t, "zantianyou", "owner-stable:manager", filepath.Join(e.root, "engineering"))
			runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "owner report", "--project", "owner-stable", "--source", source)
			e.transport.result, e.transport.err = test.outcome, test.transport
			command := []string{"report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "checked", "--next", "review"}
			if err := runAssignmentCommandError(t, e, command...); err == nil {
				t.Fatal("first non-sent owner report unexpectedly succeeded")
			}
			firstEvents, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			firstCalls := len(e.transport.calls)
			if err := runAssignmentCommandError(t, e, command...); err == nil {
				t.Fatal("idempotent non-sent owner report unexpectedly succeeded")
			}
			secondEvents, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			if len(secondEvents) != len(firstEvents) || len(e.transport.calls) != firstCalls || countCaseEvents(secondEvents, caseID, "report_prepared") != 1 {
				t.Fatalf("same owner report created a second submission: events=%d/%d calls=%d/%d", len(firstEvents), len(secondEvents), firstCalls, len(e.transport.calls))
			}

			changed := []string{"report", "--case", caseID, "--result", "completed", "--artifact", artifact2, "--verify", "changed", "--next", "review"}
			changedErr := runAssignmentCommandError(t, e, changed...)
			if changedErr == nil || (!strings.Contains(changedErr.Error(), "本轮已有 submission") && !strings.Contains(changedErr.Error(), "business delivery fence")) {
				t.Fatalf("different owner report bypassed reserved submission: %v", changedErr)
			}
			afterChanged, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			if len(afterChanged) != len(secondEvents) || len(e.transport.calls) != firstCalls {
				t.Fatal("rejected owner submission had side effects")
			}
		})
	}
}

func TestStrictReplayRejectsOwnerStyleReportThatOmitsActiveAssignment(t *testing.T) {
	const caseID = "ASSIGNMENT-OWNER-STYLE-FORGE"
	e, events := issuedAssignmentFixture(t, caseID)
	issue := latestEventOfType(events, "issue_sent")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "owner-style-forge.md"), "# result\n")
	e.setActor(t, "zantianyou", "owner-style:assignee", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "checked", "--next", "review")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for index := range events {
		if events[index].Type != "report_prepared" && events[index].Type != "report_sent" {
			continue
		}
		events[index].AssignmentEventID = ""
		events[index].AssignmentID = ""
		events[index].AssignmentDigest = ""
		events[index].AssignmentIssuer = ""
		events[index].Reviewer, events[index].ReviewerLabel = "", ""
		events[index].Acceptor, events[index].AcceptorLabel = "", ""
		events[index].DueAt = ""
	}
	rehashEventChain(t, events)
	path := writeRawEvents(t, e.data, events)
	if _, err := NewStore(e.data).ReadAll(testConfig()); err == nil || !strings.Contains(err.Error(), "必须引用 active assignment contract") || !strings.Contains(err.Error(), path+":") {
		t.Fatalf("strict replay accepted owner-style report over active assignment: %v", err)
	}
}

func submittedChildAssignmentFixture(t *testing.T, caseID string) (testEnv, []Event) {
	t.Helper()
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-source.md"), "# child assignment source\n")
	reportSource := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-blocked.md"), "# blocked evidence\n")
	e.setActor(t, "zantianyou", "reviewer-bypass:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "reviewer owner fallback guard", "--project", "assignment-guard", "--source", source)
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "eng-developer", "--next", "Investigate and report")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issue := latestEventOfType(events, "issue_sent")
	e.setActor(t, "eng-developer", "reviewer-bypass:developer", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Investigate blocker")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "blocked", "--source", reportSource, "--note", "Blocked on a management decision", "--next", "Manager decision")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[issue.ID]
	state := ledger.snapshot.Cases[caseID]
	if assignment == nil || assignment.Status != "submitted" || assignment.Consumed || state == nil || state.Owner != "zantianyou" {
		t.Fatalf("fixture did not leave reviewer-owned active submission: assignment=%+v state=%+v", assignment, state)
	}
	return e, events
}

func TestReviewerOwnerFallbackCannotBypassSubmittedAssignment(t *testing.T) {
	const caseID = "ASSIGNMENT-REVIEWER-OWNER-BYPASS"
	e, before := submittedChildAssignmentFixture(t, caseID)
	reviewerSource := writeTestFile(t, filepath.Join(e.root, "engineering", "reviewer-escalation.md"), "# escalation\n")
	transportBefore := len(e.transport.calls)
	e.setActor(t, "zantianyou", "reviewer-bypass:manager", filepath.Join(e.root, "engineering"))
	err := runAssignmentCommandError(t, e, "report", "--case", caseID, "--result", "needs-decision", "--source", reviewerSource, "--note", "Escalation requires president decision", "--next", "President decision")
	if err == nil || !strings.Contains(err.Error(), "必须引用 active assignment contract") || !strings.Contains(err.Error(), "accept/return") {
		t.Fatalf("reviewer owner fallback bypassed submitted assignment: %v", err)
	}
	after, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) || len(e.transport.calls) != transportBefore {
		t.Fatalf("rejected reviewer owner report had effects: events=%d->%d transport=%d->%d",
			len(before), len(after), transportBefore, len(e.transport.calls))
	}
}

func TestStrictReplayRejectsReviewerOwnerFallbackOverSubmittedAssignment(t *testing.T) {
	const caseID = "ASSIGNMENT-REVIEWER-OWNER-FORGE"
	e, events := submittedChildAssignmentFixture(t, caseID)
	reviewerSource := writeTestFile(t, filepath.Join(e.root, "engineering", "reviewer-forged-escalation.md"), "# forged escalation\n")
	app := e.app(t)
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	state, err := ledger.currentCase(caseID)
	if err != nil {
		t.Fatal(err)
	}
	actorRule, _ := app.Config.exactRule("zantianyou")
	recipientRule, _ := app.Config.exactRule("penny")
	forged, err := app.newEvent(Actor{Name: actorRule.Name, Label: actorRule.Label, Department: actorRule.Department, Rule: actorRule, PaneID: "forged:reviewer"}, "report_prepared", caseID)
	if err != nil {
		t.Fatal(err)
	}
	forged.FromState, forged.Title, forged.Project = state.Status, state.Title, state.Project
	forged.Recipient, forged.RecipientLabel = recipientRule.Name, recipientRule.Label
	forged.CaseVersion, forged.CaseDigest = state.Version, state.Digest
	forged.Result, forged.SourceRef, forged.Note, forged.NextAction = "needs-decision", reviewerSource, "Escalation requires president decision", "President decision"
	forged.Delivery = deliveryPrepared
	forged.DeliveryID = stableDeliveryID(stableCommandID("forged-reviewer-owner", caseID), recipientRule.Name)
	payload, err := app.deliveryPayload(forged)
	if err != nil {
		t.Fatal(err)
	}
	forged.PayloadDigest = digestText(payload)
	events = appendForgedFenceEvent(t, events, forged, "reviewer-owner-report")
	path := writeRawEvents(t, e.data, events)
	if _, err := NewStore(e.data).ReadAll(testConfig()); err == nil ||
		!strings.Contains(err.Error(), "必须引用 active assignment contract") || !strings.Contains(err.Error(), path+":") {
		t.Fatalf("strict replay accepted reviewer owner fallback over active assignment: %v", err)
	}
}

func TestResolvedReportRemainsSubmittedUntilReviewerDecision(t *testing.T) {
	caseID := "REPORT-RESOLVED-REVIEW"
	e, events := issuedAssignmentFixture(t, caseID)
	issue := latestEventOfType(events, "issue_sent")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "resolved-report.md"), "# result\n")
	evidence := writeTestFile(t, filepath.Join(e.office, "resolved-report-evidence.md"), "# receiver evidence\n")
	e.setActor(t, "zantianyou", "resolved:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("timeout after prompt")
	if err := runAssignmentCommandError(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "checked", "--next", "review"); err == nil {
		t.Fatal("ambiguous report unexpectedly succeeded")
	}
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	origin := latestEventOfType(events, "report_prepared")
	e.setActor(t, "penny", "resolved:penny", e.office)
	runTestCommand(t, e, "delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "delivered", "--reason", "receiver confirmed report", "--evidence", evidence)
	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[issue.ID]
	if assignment == nil || assignment.Status != "submitted" || assignment.Consumed {
		t.Fatalf("resolved delivered report skipped reviewer decision: %+v", assignment)
	}
	resolvedEvents, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	resolved := latestEventOfType(resolvedEvents, "delivery_resolved_sent")
	e.transport.result, e.transport.err = transportSent, nil
	runTestCommand(t, e, "return", "--event", resolved.ID, "--reason", "needs revision", "--next", "resubmit")
	ledger, err = e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment = ledger.assignments[issue.ID]
	if assignment == nil || assignment.Status != "rework" || assignment.Consumed || assignment.SubmissionGeneration == "" || assignment.SubmissionEventID != "" {
		t.Fatalf("resolved report return did not reopen one new round: %+v", assignment)
	}
}

func TestActiveAssignmentSurvivesAuthorizedAcceptorLabelUpdate(t *testing.T) {
	caseID := "ASSIGNMENT-LABEL-UPDATE"
	e, events := issuedAssignmentFixture(t, caseID)
	issue := latestEventOfType(events, "issue_sent")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "label-update-result.md"), "# result\n")
	e.setActor(t, "zantianyou", "label:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	penny, _ := cfg.exactRule("penny")
	penny.Label = "Penny新花名"
	finalizeTestSeatMutation(&penny)
	decision := writeStaffMutationDecision(t, e, "DEC-ASSIGNMENT-LABEL", "staff:update", penny)
	e.setActor(t, "penny", "label:penny", e.office)
	runTestCommand(t, e, "staff", "update", "--name", "penny", "--label", penny.Label, "--approval", decision)

	e.setActor(t, "zantianyou", "label:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--verify", "checked", "--next", "review")
	updated, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err = NewStore(e.data).ReadAll(updated)
	if err != nil {
		t.Fatal(err)
	}
	report := latestEventOfType(events, "report_prepared")
	if report.RecipientLabel != issue.AcceptorLabel || report.ReviewerLabel != issue.ReviewerLabel || report.AcceptorLabel != issue.AcceptorLabel {
		t.Fatalf("report did not preserve frozen assignment labels after staff update: issue=%+v report=%+v", issue, report)
	}
}

func TestAcceptRejectsDirectRegistrySeatRebindBypass(t *testing.T) {
	e, events := issuedAssignmentFixture(t, "ASSIGNMENT-DIRECT-SEAT-REBIND")
	issue := latestEventOfType(events, "issue_sent")
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Agents {
		if cfg.Agents[index].Name != issue.Recipient {
			continue
		}
		cfg.Agents[index].ActivationPolicy = activationManual
		finalizeTestSeatMutation(&cfg.Agents[index])
	}
	// Simulate a same-user process bypassing the authorized staff command. The
	// runtime boundary must still refuse to let a new seat incarnation accept
	// the old immutable assignment.
	writeConfigFixture(t, e.config, cfg)
	e.setActor(t, issue.Recipient, "direct-rebind:assignee", filepath.Join(e.root, "engineering"))
	err = runAssignmentCommandError(t, e, "accept", "--event", issue.ID, "--next", "work")
	if err == nil || !strings.Contains(err.Error(), "冻结席位核验失败") {
		t.Fatalf("direct registry seat rebind inherited an old assignment: %v", err)
	}
}

func TestReportReviewRejectsDirectAssigneeSeatRebindBypass(t *testing.T) {
	for _, command := range []string{"accept", "return"} {
		t.Run(command, func(t *testing.T) {
			e, report := submittedAssignmentFixture(t, "REPORT-REVIEW-REBIND-"+strings.ToUpper(command))
			directlyRebindAssignmentAssignee(t, e)
			e.setActor(t, "penny", "review-rebind:penny", e.office)
			// Read the immutable pre-rebind history with the original registry.
			// The production command uses the tampered current registry and should
			// now fail even earlier at the promoted strict ledger-tail invariant.
			beforeEvents, err := NewStore(e.data).ReadAll(testConfig())
			if err != nil {
				t.Fatal(err)
			}
			beforeCalls := len(e.transport.calls)
			args := []string{command, "--event", report.ID, "--next", "must not reach replacement seat"}
			if command == "return" {
				args = append(args, "--reason", "rework required")
			}
			err = runAssignmentCommandError(t, e, args...)
			if err == nil || !strings.Contains(err.Error(), "冻结") || !strings.Contains(err.Error(), "seat") {
				t.Fatalf("%s allowed a replacement seat to inherit report review: %v", command, err)
			}
			afterEvents, readErr := NewStore(e.data).ReadAll(testConfig())
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(afterEvents) != len(beforeEvents) || len(e.transport.calls) != beforeCalls {
				t.Fatalf("rejected %s had side effects: events=%d->%d transport=%d->%d", command,
					len(beforeEvents), len(afterEvents), beforeCalls, len(e.transport.calls))
			}
		})
	}
}

func TestReturnedReportRetryRejectsDirectAssigneeSeatRebindBypass(t *testing.T) {
	e, report := submittedAssignmentFixture(t, "REPORT-RETURN-RETRY-REBIND")
	e.setActor(t, "penny", "return-retry:penny", e.office)
	e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("synthetic return notification offline")
	if err := runAssignmentCommandError(t, e, "return", "--event", report.ID, "--reason", "rework required", "--next", "revise"); err == nil {
		t.Fatal("failed-pre-send return notification unexpectedly succeeded")
	}
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	returned := latestCaseEvent(events, report.CaseID, "event_returned")
	if returned.DeliveryID == "" {
		t.Fatal("missing durable return notification")
	}
	directlyRebindAssignmentAssignee(t, e)
	e.setActor(t, "penny", "return-retry:penny", e.office)
	e.transport.result, e.transport.err = transportSent, nil
	beforeCalls := len(e.transport.calls)
	err = runAssignmentCommandError(t, e, "delivery", "retry", "--id", returned.DeliveryID)
	if err == nil || !strings.Contains(err.Error(), "冻结") || !strings.Contains(err.Error(), "seat") {
		t.Fatalf("return retry reached replacement seat: %v", err)
	}
	if len(e.transport.calls) != beforeCalls {
		t.Fatalf("rejected return retry invoked transport: %d -> %d", beforeCalls, len(e.transport.calls))
	}
}

func TestResolveDeliveredIssueRejectsDirectAssigneeSeatRebindBypass(t *testing.T) {
	e, _, origin, deliveryErr := runIssueWithOutcome(t, transportAmbiguous, errors.New("synthetic ambiguous issue"), nil)
	if deliveryErr == nil || origin.DeliveryID == "" {
		t.Fatalf("ambiguous issue fixture missing: origin=%+v err=%v", origin, deliveryErr)
	}
	directlyRebindAssignmentAssignee(t, e)
	evidence := writeTestFile(t, filepath.Join(e.office, "direct-rebind-resolve-evidence.md"), "# evidence\n")
	e.setActor(t, "penny", "resolve-rebind:penny", e.office)
	beforeEvents, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	beforeCalls := len(e.transport.calls)
	err = runAssignmentCommandError(t, e, "delivery", "resolve", "--id", origin.DeliveryID,
		"--outcome", "delivered", "--reason", "receiver claimed delivery", "--evidence", evidence)
	if err == nil || !strings.Contains(err.Error(), "冻结") || !strings.Contains(err.Error(), "seat") {
		t.Fatalf("resolve delivered rebound the issue to a replacement seat: %v", err)
	}
	afterEvents, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(afterEvents) != len(beforeEvents) || len(e.transport.calls) != beforeCalls {
		t.Fatalf("rejected resolve had side effects: events=%d->%d transport=%d->%d",
			len(beforeEvents), len(afterEvents), beforeCalls, len(e.transport.calls))
	}
}

func TestIssueRetryRejectsDirectAssigneeSeatRebindBypass(t *testing.T) {
	e, _, origin, deliveryErr := runIssueWithOutcome(t, transportDefinitelyNotSent, errors.New("synthetic offline issue"), nil)
	if deliveryErr == nil || origin.DeliveryID == "" {
		t.Fatalf("failed-pre-send issue fixture missing: origin=%+v err=%v", origin, deliveryErr)
	}
	directlyRebindAssignmentAssignee(t, e)
	e.setActor(t, "penny", "retry-rebind:penny", e.office)
	e.transport.result, e.transport.err = transportSent, nil
	beforeCalls := len(e.transport.calls)
	err := runAssignmentCommandError(t, e, "delivery", "retry", "--id", origin.DeliveryID)
	if err == nil || !strings.Contains(err.Error(), "冻结") || !strings.Contains(err.Error(), "seat") {
		t.Fatalf("issue retry reached a replacement seat: %v", err)
	}
	if len(e.transport.calls) != beforeCalls {
		t.Fatalf("rejected issue retry invoked transport: %d -> %d", beforeCalls, len(e.transport.calls))
	}
}

func TestResolvedIssueRejectsDuplicateAssignmentIDAfterRehash(t *testing.T) {
	e := setupTestEnv(t)
	firstSource := writeTestFile(t, filepath.Join(e.office, "resolved-duplicate-first-source.md"), "# first\n")
	secondSource := writeTestFile(t, filepath.Join(e.office, "resolved-duplicate-second-source.md"), "# second\n")
	firstOrder := writeTestFile(t, filepath.Join(e.office, "resolved-duplicate-first-order.md"), "# first order\n")
	secondOrder := writeTestFile(t, filepath.Join(e.office, "resolved-duplicate-second-order.md"), "# second order\n")
	firstDecision := writeIssueApproval(t, e, "resolved-duplicate-first.md", "DEC-RESOLVED-DUPLICATE-FIRST", "RESOLVED-DUPLICATE-FIRST", firstOrder, "zantianyou")
	secondDecision := writeIssueApproval(t, e, "resolved-duplicate-second.md", "DEC-RESOLVED-DUPLICATE-SECOND", "RESOLVED-DUPLICATE-SECOND", secondOrder, "zantianyou")
	e.setActor(t, "penny", "duplicate:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", "RESOLVED-DUPLICATE-FIRST", "--title", "first", "--project", "duplicate", "--source", firstSource)
	runTestCommand(t, e, "issue", "--case", "RESOLVED-DUPLICATE-FIRST", "--to", "zantianyou", "--decision", firstDecision, "--next", "work")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	first := latestCaseEvent(events, "RESOLVED-DUPLICATE-FIRST", "issue_sent")
	firstArtifact := writeTestFile(t, filepath.Join(e.root, "engineering", "resolved-duplicate-first-result.md"), "# first result\n")
	e.setActor(t, "zantianyou", "duplicate:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", first.ID, "--next", "work")
	runTestCommand(t, e, "report", "--case", first.CaseID, "--result", "completed", "--artifact", firstArtifact, "--verify", "complete", "--next", "review")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	firstReport := latestCaseEvent(events, first.CaseID, "report_sent")
	e.setActor(t, "penny", "duplicate:penny", e.office)
	runTestCommand(t, e, "accept", "--event", firstReport.ID, "--next", "archive")
	runTestCommand(t, e, "case", "create", "--id", "RESOLVED-DUPLICATE-SECOND", "--parent", "RESOLVED-DUPLICATE-FIRST", "--title", "second", "--source", secondSource)
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("timeout after second issue")
	if err := runAssignmentCommandError(t, e, "issue", "--case", "RESOLVED-DUPLICATE-SECOND", "--to", "zantianyou", "--decision", secondDecision, "--next", "work"); err == nil {
		t.Fatal("ambiguous second issue unexpectedly succeeded")
	}
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var secondOrigin Event
	for _, event := range events {
		if event.CaseID == "RESOLVED-DUPLICATE-SECOND" && event.Type == "issue_prepared" {
			secondOrigin = event
		}
	}
	if first.AssignmentID == "" || secondOrigin.DeliveryID == "" {
		t.Fatalf("missing issue fixtures: first=%+v second=%+v", first, secondOrigin)
	}
	evidence := writeTestFile(t, filepath.Join(e.office, "resolved-duplicate-evidence.md"), "# confirmed\n")
	e.setActor(t, "penny", "duplicate:penny", e.office)
	runTestCommand(t, e, "delivery", "resolve", "--id", secondOrigin.DeliveryID, "--outcome", "delivered", "--reason", "receiver confirmed second issue", "--evidence", evidence)
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	for index := range events {
		if events[index].CaseID != "RESOLVED-DUPLICATE-SECOND" ||
			(events[index].Type != "issue_prepared" && events[index].Type != "delivery_resolved_sent") {
			continue
		}
		events[index].AssignmentID = first.AssignmentID
		events[index].AssignmentDigest = assignmentContractDigest(events[index])
	}
	err = rewriteProtocolFixture(t, e, events)
	if err == nil || !strings.Contains(err.Error(), "assignment_id 重复") {
		t.Fatalf("resolved-sent issue accepted duplicate assignment id: %v", err)
	}
}

func TestResolvedIssuePreservesFrozenIssuerWhenOperatorDiffers(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "baogong" {
			cfg.Agents[index].CanManageStaff = true
			cfg.Agents[index].SeatVersion++
			cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
		}
	}
	writeConfigFixture(t, e.config, cfg)

	caseID := "RESOLVED-DIFFERENT-OPERATOR"
	source := writeTestFile(t, filepath.Join(e.office, "different-operator-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "different-operator-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "different-operator-decision.md", "DEC-DIFFERENT-OPERATOR", caseID, order, "zantianyou")
	e.setActor(t, "penny", "different-operator:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "different operator", "--project", "protocol", "--source", source)
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("timeout after issue")
	if err := runAssignmentCommandError(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work"); err == nil {
		t.Fatal("ambiguous issue unexpectedly succeeded")
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	origin := latestEventOfType(events, "issue_prepared")
	evidence := writeTestFile(t, filepath.Join(e.root, "qa-ux", "different-operator-evidence.md"), "# receiver confirmation\n")
	e.setActor(t, "baogong", "different-operator:baogong", filepath.Join(e.root, "qa-ux"))
	runTestCommand(t, e, "delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "delivered", "--reason", "receiver confirmed", "--evidence", evidence)
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	terminal := latestEventOfType(events, "delivery_resolved_sent")
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[terminal.ID]
	if assignment == nil || assignment.Issuer != "penny" || assignment.AssignmentID != origin.AssignmentID || assignment.AssignmentDigest != origin.AssignmentDigest {
		t.Fatalf("resolution operator replaced frozen assignment issuer/contract: terminal=%+v assignment=%+v", terminal, assignment)
	}
}
