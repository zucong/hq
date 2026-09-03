package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeStaffMutationDecision(t *testing.T, e testEnv, decisionID, action string, rule AgentRule) string {
	t.Helper()
	return writeApprovalDocument(t, filepath.Join(e.office, "decisions", strings.ToLower(decisionID)+".md"), decisionID, "effective", []ApprovalScope{{
		Action: action, Target: rule.Name, RequestDigest: staffScopeDigest(action, rule),
	}})
}

func expectStaffReplayRejection(t *testing.T, e testEnv, command ...string) error {
	t.Helper()
	before, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	backupsBefore, err := filepath.Glob(e.config + ".bak.*")
	if err != nil {
		t.Fatal(err)
	}
	app := e.app(t)
	var out, errOut bytes.Buffer
	app.Out, app.Err = &out, &errOut
	err = app.run(command)
	if err == nil || !strings.Contains(err.Error(), "候选 config 无法完整重放现有 ledger") {
		t.Fatalf("staff mutation was not rejected by candidate replay: command=%v err=%v stderr=%s stdout=%s", command, err, errOut.String(), out.String())
	}
	rejectionErr := err
	after, readErr := os.ReadFile(e.config)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(after, before) {
		t.Fatalf("rejected staff mutation changed config bytes\nbefore=%q\nafter=%q", before, after)
	}
	backupsAfter, err := filepath.Glob(e.config + ".bak.*")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(backupsAfter, "\x00") != strings.Join(backupsBefore, "\x00") {
		t.Fatalf("rejected candidate replay wrote a config backup: before=%v after=%v", backupsBefore, backupsAfter)
	}
	current, err := loadConfig(e.config)
	if err != nil {
		t.Fatalf("original config no longer loads: %v", err)
	}
	if _, err := NewStore(e.data).ReadAll(current); err != nil {
		t.Fatalf("candidate replay failure did not release ledger lock or damaged current replay: %v", err)
	}
	return rejectionErr
}

func TestStaffUpdateRejectsRevokingHistoricalPermission(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "preflight-revoke-source.md"), "# source\n")
	e.setActor(t, "penny", "w1:p1", e.office)
	runTestCommand(t, e, "case", "create", "--id", "PREFLIGHT-REVOKE-001", "--title", "permission history", "--source", source)

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	penny, _ := cfg.exactRule("penny")
	penny.CanCreate = false
	finalizeTestSeatMutation(&penny)
	decision := writeStaffMutationDecision(t, e, "DEC-PREFLIGHT-REVOKE", "staff:update", penny)
	expectStaffReplayRejection(t, e, "staff", "update", "--name", "penny", "--revoke", "create", "--approval", decision)

	unchanged, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := unchanged.exactRule("penny")
	if !actual.CanCreate {
		t.Fatal("rejected preflight still revoked can_create")
	}
}

func TestStaffUpdateRejectsChangingHistoricalReportsTo(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "preflight-reports-source.md"), "# source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "preflight-reports-artifact.md"), "# artifact\n")

	// Keep this history assignment-free so reports_to is itself part of the
	// replayed authorization contract. First-class assignment reports instead
	// route to their frozen acceptor and intentionally ignore later reports_to.
	e.setActor(t, "eng-data-engineer", "w1:p3", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", "PREFLIGHT-REPORTS-001", "--title", "reporting history", "--source", source)
	runTestCommand(t, e, "report", "--case", "PREFLIGHT-REPORTS-001", "--result", "completed", "--artifact", artifact, "--next", "manager review")

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := cfg.exactRule("eng-data-engineer")
	worker.ReportsTo = "baogong"
	finalizeTestSeatMutation(&worker)
	decision := writeStaffMutationDecision(t, e, "DEC-PREFLIGHT-REPORTS", "staff:update", worker)
	e.setActor(t, "penny", "w1:p1", e.office)
	expectStaffReplayRejection(t, e, "staff", "update", "--name", worker.Name, "--reports-to", worker.ReportsTo, "--approval", decision)

	unchanged, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := unchanged.exactRule(worker.Name)
	if actual.ReportsTo != "zantianyou" {
		t.Fatalf("rejected preflight changed reports_to: %s", actual.ReportsTo)
	}
}

func TestStaffUpdateRejectsRebindingAssigneeWithActiveAssignment(t *testing.T) {
	e, _ := issuedAssignmentFixture(t, "PREFLIGHT-ACTIVE-SEAT-001")
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	worker, ok := cfg.exactRule("zantianyou")
	if !ok {
		t.Fatal("missing assignment assignee")
	}
	originalSeatVersion := worker.SeatVersion
	worker.ActivationPolicy = activationAlways
	finalizeTestSeatMutation(&worker)
	decision := writeStaffMutationDecision(t, e, "DEC-PREFLIGHT-ACTIVE-SEAT", "staff:update", worker)

	e.setActor(t, "penny", "active-seat:penny", e.office)
	replayErr := expectStaffReplayRejection(t, e, "staff", "update", "--name", worker.Name,
		"--activation", activationAlways, "--approval", decision)
	if !strings.Contains(replayErr.Error(), "assignment:") || !strings.Contains(replayErr.Error(), "仍占用冻结员工席位") {
		t.Fatalf("active assignment test did not hit frozen-seat continuity guard: %v", replayErr)
	}

	unchanged, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := unchanged.exactRule(worker.Name)
	if actual.SeatVersion != originalSeatVersion || actual.ActivationPolicy == activationAlways {
		t.Fatalf("rejected active-seat mutation leaked into registry: %+v", actual)
	}
}

func TestStaffUpdateRejectsRebindingAssigneeWithPendingIssueDelivery(t *testing.T) {
	e := setupTestEnv(t)
	caseID := "PREFLIGHT-PENDING-SEAT-001"
	source := writeTestFile(t, filepath.Join(e.office, "pending-seat-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "pending-seat-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "pending-seat-issue.md", "DEC-PENDING-SEAT-ISSUE", caseID, order, "zantianyou")
	e.setActor(t, "penny", "pending-seat:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "pending frozen seat", "--source", source)
	e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("synthetic offline before send")
	if err := runAssignmentCommandError(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work"); err == nil {
		t.Fatal("failed-pre-send issue unexpectedly succeeded")
	}

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := cfg.exactRule("zantianyou")
	worker.ActivationPolicy = activationAlways
	finalizeTestSeatMutation(&worker)
	staffDecision := writeStaffMutationDecision(t, e, "DEC-PREFLIGHT-PENDING-SEAT", "staff:update", worker)
	e.setActor(t, "penny", "pending-seat:penny", e.office)
	replayErr := expectStaffReplayRejection(t, e, "staff", "update", "--name", worker.Name,
		"--activation", activationAlways, "--approval", staffDecision)
	if !strings.Contains(replayErr.Error(), "pending-delivery:") || !strings.Contains(replayErr.Error(), "仍占用冻结员工席位") {
		t.Fatalf("pending issue test did not hit frozen-seat continuity guard: %v", replayErr)
	}
}

func TestStaffUpdateRejectsRebindingAssigneeDuringRework(t *testing.T) {
	e, report := submittedAssignmentFixture(t, "PREFLIGHT-REWORK-SEAT-001")
	e.setActor(t, "penny", "rework-seat:penny", e.office)
	runTestCommand(t, e, "return", "--event", report.ID, "--reason", "evidence incomplete", "--next", "revise")

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	worker, _ := cfg.exactRule("zantianyou")
	worker.ActivationPolicy = activationAlways
	finalizeTestSeatMutation(&worker)
	decision := writeStaffMutationDecision(t, e, "DEC-PREFLIGHT-REWORK-SEAT", "staff:update", worker)
	e.setActor(t, "penny", "rework-seat:penny", e.office)
	replayErr := expectStaffReplayRejection(t, e, "staff", "update", "--name", worker.Name,
		"--activation", activationAlways, "--approval", decision)
	if !strings.Contains(replayErr.Error(), "assignment:") || !strings.Contains(replayErr.Error(), "仍占用冻结员工席位") {
		t.Fatalf("rework test did not hit frozen-seat continuity guard: %v", replayErr)
	}
}

func TestStaffRemoveRejectsDisablingHistoricalRecipient(t *testing.T) {
	e := setupTestEnv(t)
	e.setActor(t, "penny", "w1:p1", e.office)
	runTestCommand(t, e, "message", "--to", "baogong", "--kind", "info", "--text", "historical notification")

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	baogong, _ := cfg.exactRule("baogong")
	baogong.Disabled, baogong.ActivationPolicy = true, activationManual
	finalizeTestSeatMutation(&baogong)
	decision := writeStaffMutationDecision(t, e, "DEC-PREFLIGHT-DISABLE", "staff:remove", baogong)
	expectStaffReplayRejection(t, e, "staff", "remove", "--name", baogong.Name, "--approval", decision)

	unchanged, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	actual, active := unchanged.exactRule(baogong.Name)
	if !active || actual.Disabled {
		t.Fatal("rejected preflight still disabled historical recipient")
	}
}

func TestStaffPreflightVirtuallyReplaysDurablePreAppendJournal(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(e.data)
	fired := false
	store.Failpoint = func(name string) error {
		if name == "journal_parent_fsync" && !fired {
			fired = true
			return errors.New("simulated crash after durable intent")
		}
		return nil
	}
	penny := actorFor(cfg, "penny", "pending:penny", e.office)
	if _, err := transactCreateCase(store, cfg, penny, "PREFLIGHT-PENDING-001", "pending-intent", false); err == nil {
		t.Fatal("durable-intent failpoint did not interrupt transaction")
	}
	if !fired {
		t.Fatal("journal_parent_fsync failpoint was not reached")
	}

	txnDir := filepath.Join(e.data, "txn")
	entries, err := os.ReadDir(txnDir)
	if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".json") {
		t.Fatalf("pending journal=%v err=%v", entries, err)
	}
	journalPath := filepath.Join(txnDir, entries[0].Name())
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	eventEntriesBefore, err := os.ReadDir(filepath.Join(e.data, "events"))
	if err != nil || len(eventEntriesBefore) != 0 {
		t.Fatalf("pre-append crash unexpectedly wrote event bytes: entries=%v err=%v", eventEntriesBefore, err)
	}
	configBefore, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	backupsBefore, err := filepath.Glob(e.config + ".bak.*")
	if err != nil {
		t.Fatal(err)
	}

	candidate, _ := cfg.exactRule("penny")
	candidate.CanCreate = false
	finalizeTestSeatMutation(&candidate)
	decision := writeStaffMutationDecision(t, e, "DEC-PREFLIGHT-PENDING", "staff:update", candidate)
	e.setActor(t, "penny", "pending:penny", e.office)
	app := e.app(t)
	err = app.run([]string{"staff", "update", "--name", "penny", "--revoke", "create", "--approval", decision})
	if err == nil || !strings.Contains(err.Error(), "候选 config 无法完整重放现有 ledger") || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("pending durable event was not included in candidate replay: %v", err)
	}

	configAfter, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(configBefore, configAfter) {
		t.Fatalf("rejected pending-journal preflight changed config bytes\nbefore=%q\nafter=%q", configBefore, configAfter)
	}
	backupsAfter, err := filepath.Glob(e.config + ".bak.*")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(backupsBefore, "\x00") != strings.Join(backupsAfter, "\x00") {
		t.Fatalf("rejected pending-journal preflight wrote backup: before=%v after=%v", backupsBefore, backupsAfter)
	}
	journalAfter, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(journalBefore, journalAfter) {
		t.Fatal("candidate replay modified the durable transaction journal")
	}
	eventEntriesAfter, err := os.ReadDir(filepath.Join(e.data, "events"))
	if err != nil || len(eventEntriesAfter) != 0 {
		t.Fatalf("candidate replay recovered/appended the pending event: entries=%v err=%v", eventEntriesAfter, err)
	}

	// The current config must still validate the same pending intent, and the
	// guard must be releasable without recovering or deleting it.
	release, err := NewStore(e.data).LockAndReplayCandidate(cfg)
	if err != nil {
		t.Fatalf("current config did not virtually replay pending journal: %v", err)
	}
	release()
	if _, err := os.Stat(journalPath); err != nil {
		t.Fatalf("read-only candidate guard consumed journal: %v", err)
	}
}

func TestCandidateReplayProtectsSeatReservedInsideDurableIssueJournal(t *testing.T) {
	e := setupTestEnv(t)
	caseID := "PREFLIGHT-JOURNAL-SEAT-001"
	source := writeTestFile(t, filepath.Join(e.office, "journal-seat-source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "journal-seat-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "journal-seat-issue.md", "DEC-JOURNAL-SEAT-ISSUE", caseID, order, "zantianyou")
	e.setActor(t, "penny", "journal-seat:penny", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "journal frozen seat", "--source", source)

	app := e.app(t)
	store, ok := app.Store.(*Store)
	if !ok {
		t.Fatalf("production app store type=%T, want *Store", app.Store)
	}
	fired := false
	store.Failpoint = func(name string) error {
		if name == "journal_parent_fsync" && !fired {
			fired = true
			return errors.New("simulated crash after durable issue intent")
		}
		return nil
	}
	if err := app.run([]string{"issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work"}); err == nil {
		t.Fatal("durable issue journal failpoint did not interrupt transaction")
	}
	if !fired {
		t.Fatal("issue transaction did not reach journal_parent_fsync")
	}

	candidate := app.Config
	for index := range candidate.Agents {
		if candidate.Agents[index].Name != "zantianyou" {
			continue
		}
		candidate.Agents[index].ActivationPolicy = activationAlways
		finalizeTestSeatMutation(&candidate.Agents[index])
	}
	err := NewStore(e.data).ReplayCandidateReadOnly(candidate)
	if err == nil || !strings.Contains(err.Error(), "pending-delivery:") || !strings.Contains(err.Error(), "仍占用冻结员工席位") {
		t.Fatalf("candidate replay ignored seat reservation inside durable issue journal: %v", err)
	}

	entries, readErr := os.ReadDir(filepath.Join(e.data, "txn"))
	if readErr != nil || len(entries) != 1 {
		t.Fatalf("read-only candidate replay changed durable journal: entries=%v err=%v", entries, readErr)
	}
}
