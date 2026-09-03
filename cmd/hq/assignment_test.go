package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runAssignmentCommandError(t *testing.T, e testEnv, command ...string) error {
	t.Helper()
	var out, errOut bytes.Buffer
	app := e.app(t)
	app.Out, app.Err = &out, &errOut
	return app.run(command)
}

func latestEventOfType(events []Event, eventType string) Event {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type == eventType {
			return events[index]
		}
	}
	return Event{}
}

func TestAssignmentContractAcceptReportAndReceipt(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "assignment-source.md"), "# approved project\n")
	order := writeTestFile(t, filepath.Join(e.office, "assignment-order.md"), "# implementation order\n")
	decision := writeIssueApproval(t, e, "assignment-approved.md", "DEC-ASSIGNMENT-001", "ASSIGNMENT-001", order, "zantianyou")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "assignment-result.md"), "# verified result\n")
	due := time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)

	e.setActor(t, "penny", "assignment:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", "ASSIGNMENT-001", "--title", "Build onboarding", "--project", "virtual-company-v2", "--source", source)
	runTestCommand(t, e, "issue", "--case", "ASSIGNMENT-001", "--to", "zantianyou", "--decision", decision, "--due", due, "--next", "Implement and verify")

	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issueSent := latestEventOfType(events, "issue_sent")
	if issueSent.AssignmentID == "" || issueSent.AssignmentDigest != assignmentContractDigest(issueSent) ||
		issueSent.AssignmentIssuer != "penny" || issueSent.Reviewer != "penny" || issueSent.Acceptor != "penny" ||
		issueSent.Recipient != "zantianyou" || issueSent.Project != "virtual-company-v2" || issueSent.DueAt != due {
		t.Fatalf("issue did not freeze assignment contract: %+v", issueSent)
	}

	e.setActor(t, "zantianyou", "assignment:manager", filepath.Join(e.root, "engineering"))
	beforeEvents, beforeDeliveries := len(events), len(e.transport.calls)
	err = runAssignmentCommandError(t, e, "report", "--case", "ASSIGNMENT-001", "--result", "completed", "--artifact", artifact, "--next", "Review")
	if err == nil || !strings.Contains(err.Error(), "尚未由 assignee accept") {
		t.Fatalf("report before assignment accept should fail closed: %v", err)
	}
	afterRejected, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(afterRejected) != beforeEvents || len(e.transport.calls) != beforeDeliveries {
		t.Fatalf("rejected report had side effects: events %d->%d deliveries %d->%d", beforeEvents, len(afterRejected), beforeDeliveries, len(e.transport.calls))
	}

	runTestCommand(t, e, "accept", "--event", issueSent.ID, "--next", "Start implementation")
	err = runAssignmentCommandError(t, e, "case", "revise", "--id", "ASSIGNMENT-001", "--title", "Changed while active", "--version", "2", "--source", source)
	if err == nil || !strings.Contains(err.Error(), "未完成 assignment contract") {
		t.Fatalf("active assignment allowed case revision: %v", err)
	}
	runTestCommand(t, e, "report", "--case", "ASSIGNMENT-001", "--result", "completed", "--artifact", artifact, "--verify", "artifact reviewed", "--next", "Review and accept")

	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	reportSent := latestEventOfType(events, "report_sent")
	if reportSent.AssignmentEventID != issueSent.ID || reportSent.AssignmentID != issueSent.AssignmentID ||
		reportSent.AssignmentDigest != issueSent.AssignmentDigest || reportSent.CaseVersion != issueSent.CaseVersion ||
		reportSent.CaseDigest != issueSent.CaseDigest || reportSent.Recipient != issueSent.Acceptor {
		t.Fatalf("report did not carry frozen assignment: issue=%+v report=%+v", issueSent, reportSent)
	}

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment := ledger.assignments[issueSent.ID]
	if assignment == nil || assignment.Status != "submitted" || assignment.Consumed {
		t.Fatalf("assignment projection not awaiting review: %+v", assignment)
	}

	e.setActor(t, "penny", "assignment:penny", e.office)
	runTestCommand(t, e, "accept", "--event", reportSent.ID, "--next", "Close project")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var receipt Event
	for _, event := range events {
		if event.Type == "event_accepted" && event.RelatedEventID == reportSent.ID {
			receipt = event
		}
	}
	if receipt.AcceptanceDigest == "" || receipt.AcceptanceDigest != acceptanceReceiptDigest(reportSent.ID, reportSent, receipt) ||
		receipt.ArtifactRef != reportSent.ArtifactRef || receipt.Verification != reportSent.Verification ||
		receipt.AssignmentDigest != reportSent.AssignmentDigest {
		t.Fatalf("acceptance receipt is not evidence-bound: %+v", receipt)
	}
	ledger, err = e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	assignment = ledger.assignments[issueSent.ID]
	if assignment == nil || assignment.Status != "completed" || !assignment.Consumed {
		t.Fatalf("accepted report did not complete assignment: %+v", assignment)
	}

	list := runTestCommand(t, e, "assignment", "list", "--case", "ASSIGNMENT-001", "--status", "completed")
	show := runTestCommand(t, e, "assignment", "show", "--id", issueSent.AssignmentID)
	if !strings.Contains(list, issueSent.AssignmentID) || !strings.Contains(show, issueSent.AssignmentDigest) {
		t.Fatalf("assignment query omitted contract: list=%q show=%q", list, show)
	}
}

func TestAssignmentDueAndDigestFailClosed(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "due-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "due-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "due-approved.md", "DEC-ASSIGNMENT-DUE", "ASSIGNMENT-DUE", order, "zantianyou")
	e.setActor(t, "penny", "assignment:due", e.office)
	runTestCommand(t, e, "case", "create", "--id", "ASSIGNMENT-DUE", "--title", "Due guard", "--source", source)
	before, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	err = runAssignmentCommandError(t, e, "issue", "--case", "ASSIGNMENT-DUE", "--to", "zantianyou", "--decision", decision, "--due", "2020-01-01T00:00:00Z", "--next", "Never")
	if err == nil || !strings.Contains(err.Error(), "晚于当前时间") {
		t.Fatalf("past due accepted: %v", err)
	}
	after, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) || len(e.transport.calls) != 0 {
		t.Fatalf("invalid due had side effects: events %d->%d deliveries=%d", len(before), len(after), len(e.transport.calls))
	}
}

func TestAssignmentQueriesArePhysicallyReadOnly(t *testing.T) {
	e := setupTestEnv(t)
	if _, err := os.Lstat(e.data); !os.IsNotExist(err) {
		t.Fatalf("fixture unexpectedly has records directory: %v", err)
	}
	output := runTestCommand(t, e, "assignment", "list")
	if !strings.Contains(output, "HQ assignments：0") {
		t.Fatalf("empty assignment list changed: %q", output)
	}
	if _, err := os.Lstat(e.data); !os.IsNotExist(err) {
		t.Fatalf("read-only assignment list created records state: %v", err)
	}

	txnDir := filepath.Join(e.data, "txn")
	if err := os.MkdirAll(txnDir, 0o700); err != nil {
		t.Fatal(err)
	}
	intentPath := filepath.Join(txnDir, "pending.json")
	want := []byte("durable pending intent must remain untouched\n")
	if err := os.WriteFile(intentPath, want, 0o600); err != nil {
		t.Fatal(err)
	}
	err := runAssignmentCommandError(t, e, "assignment", "list")
	if err == nil || !strings.Contains(err.Error(), "待恢复 txn intent") {
		t.Fatalf("read-only query did not fail closed on txn residue: %v", err)
	}
	got, readErr := os.ReadFile(intentPath)
	if readErr != nil || !bytes.Equal(got, want) {
		t.Fatalf("read-only query changed txn intent: got=%q err=%v", got, readErr)
	}
	if _, err := os.Lstat(filepath.Join(e.data, ".hq.lock")); !os.IsNotExist(err) {
		t.Fatalf("read-only query created lock sidecar: %v", err)
	}
}
