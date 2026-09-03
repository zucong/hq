package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type projectCASTestStore struct {
	*Store
	once    sync.Once
	mutate  func() error
	hookErr error
}

func (s *projectCASTestStore) Transact(cfg Config, commandID, digest string, dryRun bool, build TransactionBuilder) (TransactionResult, error) {
	s.once.Do(func() {
		if s.mutate != nil {
			s.hookErr = s.mutate()
		}
	})
	if s.hookErr != nil {
		return TransactionResult{}, s.hookErr
	}
	return s.Store.Transact(cfg, commandID, digest, dryRun, build)
}

func projectClosureEvents(t *testing.T, e testEnv) ([]Event, Config) {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return events, cfg
}

func expectProjectClosureNoWrite(t *testing.T, e testEnv, want string, run func() error) {
	t.Helper()
	before, _ := projectClosureEvents(t, e)
	err := run()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected zero-write rejection containing %q, got %v", want, err)
	}
	after, _ := projectClosureEvents(t, e)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("rejected command changed authoritative ledger: before=%d after=%d", len(before), len(after))
	}
}

func latestProjectClosureEvent(t *testing.T, events []Event, caseID, eventType string) Event {
	t.Helper()
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].CaseID == caseID && events[index].Type == eventType {
			return events[index]
		}
	}
	t.Fatalf("missing %s for case %s", eventType, caseID)
	return Event{}
}

func projectClosureSpecFromEvent(event Event) CaseSpec {
	spec := CaseSpec{
		CaseID: event.CaseID, ParentCaseID: event.ParentCaseID, RootCaseID: event.RootCaseID,
		Title: event.Title, Project: event.Project, Objective: event.Objective,
		Acceptance: event.Acceptance, Constraints: event.Constraints, Priority: event.Priority,
		SpecRef: event.SpecRef, SourceRef: event.SourceRef, Version: event.CaseVersion,
	}
	spec.Digest = caseSpecDigest(spec)
	return spec
}

func acceptProjectClosureChild(t *testing.T, e testEnv, cfg Config, caseID, artifact string) {
	t.Helper()
	decision := writeIssueApproval(t, e, strings.ToLower(caseID)+"-issue.md", "DEC-"+caseID, caseID, artifact, "zantianyou")
	e.setActor(t, "penny", "project-closure:penny-issue", testAgentCWD(cfg, e.root, "penny"))
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "Complete the child")
	events, _ := projectClosureEvents(t, e)
	issue := latestProjectClosureEvent(t, events, caseID, "issue_sent")

	e.setActor(t, "zantianyou", "project-closure:manager-work", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Produce evidence")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact,
		"--verify", "evidence verified", "--next", "Owner review")
	events, _ = projectClosureEvents(t, e)
	report := latestProjectClosureEvent(t, events, caseID, "report_sent")

	e.setActor(t, "penny", "project-closure:penny-review", testAgentCWD(cfg, e.root, "penny"))
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "Close in post-order")
}

func TestCaseProjectInheritanceRevisionAndPostOrderClosure(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.office, "project-closure-source.md"), "# Project closure evidence\n")
	artifact := writeTestFile(t, filepath.Join(e.office, "project-closure-artifact.md"), "# Accepted child evidence\n")
	e.setActor(t, "penny", "project-closure:penny-create", testAgentCWD(cfg, e.root, "penny"))

	runTestCommand(t, e, "case", "create", "--id", "PROJECT-TREE-ROOT", "--title", "Root", "--project", "Project Alpha", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-TREE-CHILD", "--parent", "PROJECT-TREE-ROOT", "--title", "Child", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-TREE-GRAND", "--parent", "PROJECT-TREE-CHILD", "--title", "Grandchild", "--source", source)
	events, _ := projectClosureEvents(t, e)
	created := latestProjectClosureEvent(t, events, "PROJECT-TREE-GRAND", "case_created")
	createdSpec := projectClosureSpecFromEvent(created)
	if created.CaseDigest != createdSpec.Digest {
		t.Fatalf("inherited create case digest does not bind final spec: event=%s computed=%s", created.CaseDigest, createdSpec.Digest)
	}
	wantCreateCommandDigest := requestDigest("case-create", "penny", created.CaseID, createdSpec.Digest, "penny")
	if created.CommandDigest != wantCreateCommandDigest {
		t.Fatalf("inherited create command digest was frozen before final spec: got=%s want=%s", created.CommandDigest, wantCreateCommandDigest)
	}
	beforeRetry := len(events)
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-TREE-GRAND", "--parent", "PROJECT-TREE-CHILD", "--title", "Grandchild", "--source", source)
	events, _ = projectClosureEvents(t, e)
	if len(events) != beforeRetry {
		t.Fatalf("exact inherited create retry appended an event: before=%d after=%d", beforeRetry, len(events))
	}

	snapshot, err := NewStore(e.data).Snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, caseID := range []string{"PROJECT-TREE-ROOT", "PROJECT-TREE-CHILD", "PROJECT-TREE-GRAND"} {
		if state := snapshot.Cases[caseID]; state == nil || state.Project != "Project Alpha" {
			t.Fatalf("case %s did not inherit project: %+v", caseID, state)
		}
	}

	expectProjectClosureNoWrite(t, e, "自动继承", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "create", "--id", "PROJECT-TREE-MISMATCH", "--parent", "PROJECT-TREE-ROOT",
			"--title", "Wrong project", "--project", "Project Beta", "--source", source)
		return err
	})

	runTestCommand(t, e, "case", "revise", "--id", "PROJECT-TREE-GRAND", "--version", "2", "--title", "Grandchild revised",
		"--objective", "Preserve inherited project", "--acceptance", "Project remains Alpha", "--constraints", "No lineage drift", "--priority", "P1", "--source", source)
	snapshot, err = NewStore(e.data).Snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Cases["PROJECT-TREE-GRAND"]; state == nil || state.Project != "Project Alpha" || state.Version != 2 {
		t.Fatalf("project-less revise did not preserve project: %+v", state)
	}
	events, _ = projectClosureEvents(t, e)
	revised := latestProjectClosureEvent(t, events, "PROJECT-TREE-GRAND", "case_revised")
	if revised.Project != "Project Alpha" {
		t.Fatalf("revised event did not durably carry inherited project: %+v", revised)
	}
	revisedSpec := projectClosureSpecFromEvent(revised)
	if revised.CaseDigest != revisedSpec.Digest {
		t.Fatalf("project-preserving revise case digest does not bind final spec: event=%s computed=%s", revised.CaseDigest, revisedSpec.Digest)
	}
	wantReviseCommandDigest := requestDigest("case-revise", "penny", revised.CaseID, strconv.Itoa(revised.CaseVersion), revisedSpec.Digest)
	if revised.CommandDigest != wantReviseCommandDigest {
		t.Fatalf("project-preserving revise command digest was frozen before final spec: got=%s want=%s", revised.CommandDigest, wantReviseCommandDigest)
	}
	beforeRetry = len(events)
	runTestCommand(t, e, "case", "revise", "--id", "PROJECT-TREE-GRAND", "--version", "2", "--title", "Grandchild revised",
		"--objective", "Preserve inherited project", "--acceptance", "Project remains Alpha", "--constraints", "No lineage drift", "--priority", "P1", "--source", source)
	events, _ = projectClosureEvents(t, e)
	if len(events) != beforeRetry {
		t.Fatalf("exact project-preserving revise retry appended an event: before=%d after=%d", beforeRetry, len(events))
	}

	expectProjectClosureNoWrite(t, e, "case revise 不接受 --project", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "revise", "--id", "PROJECT-TREE-GRAND", "--version", "3", "--title", "Wrong",
			"--project", "Project Beta", "--source", source)
		return err
	})
	expectProjectClosureNoWrite(t, e, "case revise 不接受 --project", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "revise", "--id", "PROJECT-TREE-ROOT", "--version", "2", "--title", "Wrong root",
			"--project", "Project Beta", "--source", source)
		return err
	})

	acceptProjectClosureChild(t, e, cfg, "PROJECT-TREE-GRAND", artifact)
	e.setActor(t, "penny", "project-closure:penny-close", testAgentCWD(cfg, e.root, "penny"))
	expectProjectClosureNoWrite(t, e, "status=accepted", func() error {
		_, err := runProjectTestCommand(t, e, false, "close", "--case", "PROJECT-TREE-ROOT", "--reason", "Attempt out of order", "--source", source)
		if err != nil {
			grand := strings.Index(err.Error(), "hq close --case PROJECT-TREE-GRAND")
			child := strings.Index(err.Error(), "hq close --case PROJECT-TREE-CHILD")
			if grand < 0 || child < 0 || grand >= child {
				t.Fatalf("close correction is not executable post-order: %v", err)
			}
		}
		return err
	})

	for _, caseID := range []string{"PROJECT-TREE-GRAND", "PROJECT-TREE-CHILD", "PROJECT-TREE-ROOT"} {
		runTestCommand(t, e, "close", "--case", caseID, "--reason", "Post-order closure complete", "--source", source)
	}
	events, _ = projectClosureEvents(t, e)
	beforeRetry = len(events)
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-TREE-GRAND", "--parent", "PROJECT-TREE-CHILD", "--title", "Grandchild", "--source", source)
	runTestCommand(t, e, "case", "revise", "--id", "PROJECT-TREE-GRAND", "--version", "2", "--title", "Grandchild revised",
		"--objective", "Preserve inherited project", "--acceptance", "Project remains Alpha", "--constraints", "No lineage drift", "--priority", "P1", "--source", source)
	events, _ = projectClosureEvents(t, e)
	if len(events) != beforeRetry {
		t.Fatalf("exact create/revise retries after parent/case close appended events: before=%d after=%d", beforeRetry, len(events))
	}
	expectProjectClosureNoWrite(t, e, "已关闭", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "create", "--id", "PROJECT-TREE-AFTER-CLOSE", "--parent", "PROJECT-TREE-ROOT",
			"--title", "Forbidden child", "--source", source)
		return err
	})
}

func TestCaseProjectPreflightCASRejectsConcurrentTreeMutation(t *testing.T) {
	t.Run("child create versus parent close", func(t *testing.T) {
		e := setupTestEnv(t)
		cfg, err := loadConfig(e.config)
		if err != nil {
			t.Fatal(err)
		}
		source := writeTestFile(t, filepath.Join(e.office, "create-close-cas.md"), "# Create versus close CAS\n")
		e.setActor(t, "penny", "project-cas:create-close", testAgentCWD(cfg, e.root, "penny"))
		runTestCommand(t, e, "case", "create", "--id", "PROJECT-CAS-CLOSE", "--title", "Close race", "--project", "CAS Project", "--source", source)
		app := e.app(t)
		app.Store = &projectCASTestStore{Store: NewStore(e.data), mutate: func() error {
			return e.app(t).run([]string{"close", "--case", "PROJECT-CAS-CLOSE", "--reason", "Concurrent close wins", "--source", source})
		}}
		err = app.run([]string{"case", "create", "--id", "PROJECT-CAS-CLOSE-CHILD", "--parent", "PROJECT-CAS-CLOSE", "--title", "Losing child", "--source", source})
		if err == nil || !strings.Contains(err.Error(), "child create admission 期间已变化") {
			t.Fatalf("concurrent parent close did not trip child-create CAS: %v", err)
		}
		snapshot, snapErr := NewStore(e.data).Snapshot(cfg)
		if snapErr != nil {
			t.Fatal(snapErr)
		}
		if root := snapshot.Cases["PROJECT-CAS-CLOSE"]; root == nil || root.Status != string(statusClosed) {
			t.Fatalf("winning close missing: %+v", root)
		}
		if child := snapshot.Cases["PROJECT-CAS-CLOSE-CHILD"]; child != nil {
			t.Fatalf("losing concurrent child was written: %+v", child)
		}
	})

	t.Run("spec revise versus first child", func(t *testing.T) {
		e := setupTestEnv(t)
		cfg, err := loadConfig(e.config)
		if err != nil {
			t.Fatal(err)
		}
		source := writeTestFile(t, filepath.Join(e.office, "revise-child-cas.md"), "# Revise versus child CAS\n")
		e.setActor(t, "penny", "project-cas:revise-child", testAgentCWD(cfg, e.root, "penny"))
		runTestCommand(t, e, "case", "create", "--id", "PROJECT-CAS-REVISE", "--title", "Revise race", "--project", "Original Project", "--source", source)
		app := e.app(t)
		app.Store = &projectCASTestStore{Store: NewStore(e.data), mutate: func() error {
			return e.app(t).run([]string{"case", "create", "--id", "PROJECT-CAS-REVISE-CHILD", "--parent", "PROJECT-CAS-REVISE", "--title", "Concurrent child", "--source", source})
		}}
		err = app.run([]string{"case", "revise", "--id", "PROJECT-CAS-REVISE", "--version", "2", "--title", "Attempt concurrent spec update", "--source", source})
		if err == nil || !strings.Contains(err.Error(), "project lineage 在 revise admission 期间已变化") {
			t.Fatalf("concurrent child did not trip revise lineage CAS: %v", err)
		}
		snapshot, snapErr := NewStore(e.data).Snapshot(cfg)
		if snapErr != nil {
			t.Fatal(snapErr)
		}
		root, child := snapshot.Cases["PROJECT-CAS-REVISE"], snapshot.Cases["PROJECT-CAS-REVISE-CHILD"]
		if root == nil || root.Version != 1 || root.Project != "Original Project" || child == nil || child.Project != "Original Project" {
			t.Fatalf("concurrent lineage did not preserve winning child/original project: root=%+v child=%+v", root, child)
		}
	})
}

func TestEscalatedParentRejectsNewChildrenWithoutWrites(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "escalated-parent-source.md"), "# Escalation\n")
	e.setActor(t, "zantianyou", "project-closure:manager-escalate", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-ESC-PARENT", "--title", "Escalation parent", "--project", "Escalation Project", "--source", source)
	expectProjectClosureNoWrite(t, e, "自动继承", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "escalate", "--id", "PROJECT-ESC-MISMATCH", "--parent", "PROJECT-ESC-PARENT", "--title", "Mismatched escalation child",
			"--project", "Different Project", "--objective", "Obtain owner decision", "--acceptance", "Owner records decision", "--constraints", "Durable path only", "--priority", "P1",
			"--source", source, "--reason", "Manager authority is insufficient", "--next", "Owner reviews escalation")
		return err
	})
	runTestCommand(t, e, "case", "escalate", "--id", "PROJECT-ESC-CHILD", "--parent", "PROJECT-ESC-PARENT", "--title", "Escalated child",
		"--objective", "Obtain owner decision", "--acceptance", "Owner records decision", "--constraints", "Durable path only", "--priority", "P1",
		"--source", source, "--reason", "Manager authority is insufficient", "--next", "Owner reviews escalation")

	snapshot, err := NewStore(e.data).Snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Cases["PROJECT-ESC-CHILD"]; state == nil || state.Status != string(statusEscalated) || state.Project != "Escalation Project" {
		t.Fatalf("escalation child did not inherit project/status: %+v", state)
	}
	e.setActor(t, "penny", "project-closure:owner-escalated", testAgentCWD(cfg, e.root, "penny"))
	expectProjectClosureNoWrite(t, e, "等待上级核验 escalation", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "create", "--id", "PROJECT-ESC-GRAND", "--parent", "PROJECT-ESC-CHILD",
			"--title", "Forbidden while escalated", "--source", source)
		return err
	})
}

func TestSingleHQSpaceAdmissionFreezesOneRootAndProject(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.office, "project-required.md"), "# One HQ space owns one project root\n")
	e.setActor(t, "penny", "project-required:penny", testAgentCWD(cfg, e.root, "penny"))
	expectProjectClosureNoWrite(t, e, "必须显式提供 --project", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "create", "--id", "PROJECT-MISSING-ROOT", "--title", "Missing identity", "--source", source)
		return err
	})
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-REQUIRED-ROOT", "--title", "Frozen root", "--project", "Assigned Project", "--source", source)
	expectProjectClosureNoWrite(t, e, "自动继承", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "create", "--id", "PROJECT-EXPLICIT-CHILD", "--parent", "PROJECT-REQUIRED-ROOT", "--title", "Explicit child project", "--project", "Assigned Project", "--source", source)
		return err
	})
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-REQUIRED-CHILD", "--parent", "PROJECT-REQUIRED-ROOT", "--title", "Now allowed", "--source", source)
	snapshot, err := NewStore(e.data).Snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if child := snapshot.Cases["PROJECT-REQUIRED-CHILD"]; child == nil || child.Project != "Assigned Project" || child.RootCaseID != "PROJECT-REQUIRED-ROOT" {
		t.Fatalf("child did not inherit frozen project/root: %+v", child)
	}
	expectProjectClosureNoWrite(t, e, "case revise 不接受 --project", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "revise", "--id", "PROJECT-REQUIRED-ROOT", "--version", "2", "--title", "Rename forbidden", "--project", "Renamed Project", "--source", source)
		return err
	})
	for _, candidate := range []struct{ id, project string }{{"PROJECT-SECOND-SAME", "Assigned Project"}, {"PROJECT-SECOND-OTHER", "Other Project"}} {
		expectProjectClosureNoWrite(t, e, "不允许第二个 root/project", func() error {
			_, err := runProjectTestCommand(t, e, false, "case", "create", "--id", candidate.id, "--title", "Second root forbidden", "--project", candidate.project, "--source", source)
			if err != nil && (!strings.Contains(err.Error(), "hq init NEW_HQ_DIR") || !strings.Contains(err.Error(), "Herdr workspace")) {
				t.Fatalf("second-root rejection lacks new-space correction: %v", err)
			}
			return err
		})
	}
	expectProjectClosureNoWrite(t, e, "自动继承", func() error {
		_, err := runProjectTestCommand(t, e, false, "case", "escalate", "--id", "PROJECT-EXPLICIT-ESC", "--parent", "PROJECT-REQUIRED-ROOT",
			"--title", "Explicit escalation project", "--project", "Assigned Project", "--objective", "route", "--acceptance", "decision", "--constraints", "durable", "--priority", "P1",
			"--source", source, "--reason", "needs owner", "--next", "owner reviews")
		return err
	})
}

func TestSingleHQSpaceConcurrentRootAdmissionAllowsExactlyOne(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.office, "concurrent-root.md"), "# Concurrent root admission\n")
	e.setActor(t, "penny", "single-root:concurrent", testAgentCWD(cfg, e.root, "penny"))

	errorsByRoot := make(chan error, 2)
	var wait sync.WaitGroup
	for _, candidate := range []struct{ id, project string }{{"ROOT-RACE-A", "Project A"}, {"ROOT-RACE-B", "Project B"}} {
		candidate := candidate
		wait.Add(1)
		go func() {
			defer wait.Done()
			errorsByRoot <- e.app(t).run([]string{"case", "create", "--id", candidate.id, "--title", candidate.id, "--project", candidate.project, "--source", source})
		}()
	}
	wait.Wait()
	close(errorsByRoot)
	successes, conflicts := 0, 0
	for err := range errorsByRoot {
		if err == nil {
			successes++
		} else if strings.Contains(err.Error(), "不允许第二个 root/project") {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent root result: %v", err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("concurrent root admission successes=%d conflicts=%d", successes, conflicts)
	}
	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger.snapshot.Cases) != 1 || ledger.soleRootCase() == nil {
		t.Fatalf("concurrent root admission did not converge to one root: %+v", ledger.snapshot.Cases)
	}
}

func TestPostOrderClosureGuidanceNamesRequiredActorsAndPrerequisites(t *testing.T) {
	ledger := newLedgerState()
	ledger.snapshot.Cases = map[string]*CaseState{
		"GUIDE-ROOT":      {ID: "GUIDE-ROOT", Status: string(statusOpen), Owner: "penny"},
		"GUIDE-ACTIVE":    {ID: "GUIDE-ACTIVE", ParentCaseID: "GUIDE-ROOT", Status: string(statusInProgress), Owner: "eng-developer"},
		"GUIDE-ESCALATED": {ID: "GUIDE-ESCALATED", ParentCaseID: "GUIDE-ROOT", Status: string(statusEscalated), Owner: "zantianyou", LastEventID: "LATER-MESSAGE"},
		"GUIDE-SUBMITTED": {ID: "GUIDE-SUBMITTED", ParentCaseID: "GUIDE-ROOT", Status: string(statusReported), Owner: "zantianyou"},
	}
	accepted := &caseAssignment{EventID: "ISSUE-SENT", AssignmentID: "ASSIGN-ACTIVE", CaseID: "GUIDE-ACTIVE", Recipient: "eng-developer", Acceptor: "zantianyou", Status: "accepted"}
	submitted := &caseAssignment{EventID: "ISSUE-SENT-2", AssignmentID: "ASSIGN-SUBMITTED", CaseID: "GUIDE-SUBMITTED", Recipient: "eng-developer", Acceptor: "zantianyou", Status: "submitted", SubmissionEventID: "REPORT-PREP"}
	ledger.assignments[accepted.EventID], ledger.assignments[submitted.EventID] = accepted, submitted
	ledger.assignmentList = []string{accepted.EventID, submitted.EventID}
	ledger.events["REPORT-PREP"] = Event{ID: "REPORT-PREP", DeliveryID: "DELIVERY-REPORT"}
	ledger.events["ESC-SENT"] = Event{ID: "ESC-SENT", CaseID: "GUIDE-ESCALATED", Type: "case_escalation_sent", Sequence: 10}
	ledger.deliveries["DELIVERY-REPORT"] = &deliveryRecord{Origin: ledger.events["REPORT-PREP"], Status: deliverySent, Terminal: Event{ID: "REPORT-SENT"}}
	descendants := ledger.unclosedDescendantsPostOrder("GUIDE-ROOT")
	guidance := ledger.renderPostOrderClosureGuidance(descendants, "GUIDE-ROOT", "penny")
	for _, fragment := range []string{
		"actor=eng-developer 先完成本轮并运行 hq report --case GUIDE-ACTIVE",
		"actor=zantianyou 先运行 hq accept --event REPORT-SENT",
		"case=GUIDE-ESCALATED(status=escalated)：actor=zantianyou 先运行 hq accept --event ESC-SENT",
		"actor=penny 运行 hq close --case GUIDE-ROOT",
	} {
		if !strings.Contains(guidance, fragment) {
			t.Fatalf("state-aware closure guidance missing %q: %s", fragment, guidance)
		}
	}
	if strings.Index(guidance, "hq report --case GUIDE-ACTIVE") > strings.Index(guidance, "hq close --case GUIDE-ACTIVE") {
		t.Fatalf("active assignment close was suggested before report convergence: %s", guidance)
	}
	if strings.Index(guidance, "hq accept --event ESC-SENT") > strings.Index(guidance, "hq close --case GUIDE-ESCALATED") {
		t.Fatalf("escalation close was suggested before owner review: %s", guidance)
	}
}

func TestClosureWorkflowDeliveryRecoveryIsStatusSpecific(t *testing.T) {
	ledger := newLedgerState()
	ledger.snapshot.Cases["DELIVERY-GUIDE"] = &CaseState{ID: "DELIVERY-GUIDE", Status: string(statusAccepted)}
	statuses := []string{deliveryPrepared, deliveryQueued, deliveryAttempted, deliveryFailedPreSend, deliveryUnknown}
	for index, status := range statuses {
		id := "DELIVERY-GUIDE-" + strings.ToUpper(strings.ReplaceAll(status, "_", "-"))
		ledger.deliveries[id] = &deliveryRecord{Origin: Event{
			Sequence: int64(index + 1), DeliveryID: id, CaseID: "DELIVERY-GUIDE", Type: "event_accepted", Recipient: "zantianyou",
		}, Status: status}
	}
	ledger.deliveries["DELIVERY-POSTMORTEM-MESSAGE"] = &deliveryRecord{Origin: Event{
		Sequence: 10, DeliveryID: "DELIVERY-POSTMORTEM-MESSAGE", CaseID: "DELIVERY-GUIDE", Type: "message_prepared", Recipient: "zantianyou",
	}, Status: deliveryQueued}
	blockers := ledger.unsettledClosureDeliveries("DELIVERY-GUIDE", false)
	if len(blockers) != len(statuses) {
		t.Fatalf("workflow blockers=%d, want=%d (ordinary message must be excluded): %+v", len(blockers), len(statuses), blockers)
	}
	recovery := renderClosureDeliveryRecoveryCommands(blockers)
	for _, fragment := range []string{
		"hq reconcile",
		"禁止盲目重投",
		"hq delivery retry --id DELIVERY-GUIDE-FAILED-PRE-SEND",
		"hq delivery resolve --id DELIVERY-GUIDE-UNKNOWN --outcome delivered|not-delivered",
	} {
		if !strings.Contains(recovery, fragment) {
			t.Fatalf("status-specific delivery recovery missing %q: %s", fragment, recovery)
		}
	}
	if strings.Contains(recovery, "delivery consume") {
		t.Fatalf("formal workflow queued delivery incorrectly suggested message-only consume: %s", recovery)
	}
}

func TestCloseRejectsPendingDescendantDeliveryAndStrictReplay(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.office, "pending-delivery-close.md"), "# Pending delivery close guard\n")
	e.setActor(t, "penny", "project-closure:pending-delivery", testAgentCWD(cfg, e.root, "penny"))
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-DELIVERY-ROOT", "--title", "Delivery root", "--project", "Delivery Project", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-DELIVERY-CHILD", "--parent", "PROJECT-DELIVERY-ROOT", "--title", "Delivery child", "--source", source)
	decision := writeIssueApproval(t, e, "pending-delivery-issue.md", "DEC-PROJECT-DELIVERY", "PROJECT-DELIVERY-CHILD", source, "zantianyou")
	runTestCommand(t, e, "issue", "--case", "PROJECT-DELIVERY-CHILD", "--to", "zantianyou", "--decision", decision, "--next", "Produce closure evidence")
	events, _ := projectClosureEvents(t, e)
	issue := latestProjectClosureEvent(t, events, "PROJECT-DELIVERY-CHILD", "issue_sent")
	e.setActor(t, "zantianyou", "project-closure:delivery-worker", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "Produce evidence")
	runTestCommand(t, e, "report", "--case", "PROJECT-DELIVERY-CHILD", "--result", "completed", "--artifact", source,
		"--verify", "closure evidence exists", "--next", "Owner review")
	events, _ = projectClosureEvents(t, e)
	report := latestProjectClosureEvent(t, events, "PROJECT-DELIVERY-CHILD", "report_sent")
	e.setActor(t, "penny", "project-closure:delivery-owner", testAgentCWD(cfg, e.root, "penny"))
	e.transport.result, e.transport.err = transportAmbiguous, errors.New("accept notice outcome unknown")
	if _, err := runProjectTestCommand(t, e, false, "accept", "--event", report.ID, "--next", "Close after notice converges"); err == nil {
		t.Fatal("ambiguous accept notice unexpectedly succeeded")
	}
	e.transport.result, e.transport.err = "", nil

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	blockers := ledger.unsettledClosureDeliveries("PROJECT-DELIVERY-ROOT", true)
	if len(blockers) != 1 || blockers[0].Status != deliveryUnknown || blockers[0].Origin.Type != "event_accepted" || blockers[0].Origin.CaseID != "PROJECT-DELIVERY-CHILD" {
		t.Fatalf("descendant unknown workflow delivery not visible to closure guard: %+v", blockers)
	}
	deliveryID := blockers[0].Origin.DeliveryID
	expectProjectClosureNoWrite(t, e, "未收敛 workflow delivery", func() error {
		_, err := runProjectTestCommand(t, e, false, "close", "--case", "PROJECT-DELIVERY-ROOT", "--reason", "Attempt with pending outbox", "--source", source)
		if err != nil && (!strings.Contains(err.Error(), "case=PROJECT-DELIVERY-CHILD,status=unknown") ||
			!strings.Contains(err.Error(), "hq delivery status --id "+deliveryID) ||
			!strings.Contains(err.Error(), "hq delivery resolve --id "+deliveryID)) {
			t.Fatalf("close error did not provide descendant delivery recovery command: %v", err)
		}
		return err
	})

	events, _ = projectClosureEvents(t, e)
	child := ledger.snapshot.Cases["PROJECT-DELIVERY-CHILD"]
	closeEvent := testLedgerEvent(t, NewStore(e.data), actorFor(cfg, "penny", "strict-project:pending-close", e.office), "case_closed", child.ID)
	closeEvent.FromState, closeEvent.ToState = child.Status, string(statusClosed)
	closeEvent.SourceRef, closeEvent.Note, closeEvent.NextAction = source, "forged close over unknown delivery", "none"
	forged := appendForgedFenceEvent(t, append([]Event(nil), events...), closeEvent, "pending-delivery-close")
	if _, err := validateLedger(forged, cfg); err == nil || !strings.Contains(err.Error(), "workflow delivery") ||
		!strings.Contains(err.Error(), deliveryID) {
		t.Fatalf("strict replay accepted close over unknown descendant workflow delivery: %v", err)
	}
}

func TestClosedCaseAllowsPostmortemMessageQueue(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.office, "postmortem-message.md"), "# Postmortem communication\n")
	e.setActor(t, "penny", "project-closure:postmortem", testAgentCWD(cfg, e.root, "penny"))
	runTestCommand(t, e, "case", "create", "--id", "PROJECT-POSTMORTEM", "--title", "Postmortem", "--project", "Postmortem Project", "--source", source)
	runTestCommand(t, e, "close", "--case", "PROJECT-POSTMORTEM", "--reason", "Business work is closed", "--source", source)
	runTestCommand(t, e, "message", "--to", "zantianyou", "--kind", "info", "--case", "PROJECT-POSTMORTEM",
		"--text", "Postmortem note after business closure", "--delivery", "inject")
	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	if state := ledger.snapshot.Cases["PROJECT-POSTMORTEM"]; state == nil || state.Status != string(statusClosed) {
		t.Fatalf("postmortem message changed closed business state: %+v", state)
	}
	if blockers := ledger.unsettledClosureDeliveries("PROJECT-POSTMORTEM", false); len(blockers) != 0 {
		t.Fatalf("ordinary postmortem message incorrectly became closure workflow blocker: %+v", blockers)
	}
}

func TestStrictReplayRejectsProjectAndClosureForgery(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.office, "strict-project-closure.md"), "# Strict replay evidence\n")
	artifact := writeTestFile(t, filepath.Join(e.office, "strict-project-artifact.md"), "# Child evidence\n")
	e.setActor(t, "penny", "strict-project:penny", testAgentCWD(cfg, e.root, "penny"))
	runTestCommand(t, e, "case", "create", "--id", "STRICT-PROJECT-ROOT", "--title", "Root", "--project", "Strict Project", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "STRICT-PROJECT-CHILD", "--parent", "STRICT-PROJECT-ROOT", "--title", "Child", "--source", source)
	runTestCommand(t, e, "case", "revise", "--id", "STRICT-PROJECT-CHILD", "--version", "2", "--title", "Child revised", "--source", source)
	acceptProjectClosureChild(t, e, cfg, "STRICT-PROJECT-CHILD", artifact)
	events, _ := projectClosureEvents(t, e)

	tests := []struct {
		name      string
		eventType string
		want      string
	}{
		{name: "forged child project", eventType: "case_created", want: "child case project"},
		{name: "forged revised project", eventType: "case_revised", want: "不得改变已冻结的 project identity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			forged := append([]Event(nil), events...)
			index := -1
			for i := range forged {
				if forged[i].CaseID == "STRICT-PROJECT-CHILD" && forged[i].Type == test.eventType {
					index = i
				}
			}
			if index < 0 {
				t.Fatalf("missing %s", test.eventType)
			}
			forged[index].Project = "Forged Project"
			rehashEventChain(t, forged)
			if _, err := validateLedger(forged, cfg); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("strict replay accepted %s: %v", test.name, err)
			}
		})
	}
	t.Run("forged root project violates frozen spec digest", func(t *testing.T) {
		forged := append([]Event(nil), events...)
		for i := range forged {
			if forged[i].CaseID == "STRICT-PROJECT-ROOT" && forged[i].Type == "case_created" {
				forged[i].Project = "Forged Root Project"
				break
			}
		}
		rehashEventChain(t, forged)
		if _, err := validateLedger(forged, cfg); err == nil || !strings.Contains(err.Error(), "case_digest 与事件最终规格不匹配") {
			t.Fatalf("strict replay accepted root project rewrite with stale spec digest: %v", err)
		}
	})
	t.Run("coordinated subtree project rewrite violates frozen specs", func(t *testing.T) {
		forged := append([]Event(nil), events...)
		for i := range forged {
			if forged[i].Project == "Strict Project" {
				forged[i].Project = "Coordinated Forged Project"
			}
		}
		rehashEventChain(t, forged)
		if _, err := validateLedger(forged, cfg); err == nil || !strings.Contains(err.Error(), "case_digest 与事件最终规格不匹配") {
			t.Fatalf("strict replay accepted coordinated project tree rewrite with stale spec digests: %v", err)
		}
	})
	t.Run("root cannot forge root case id", func(t *testing.T) {
		forged := append([]Event(nil), events...)
		for i := range forged {
			if forged[i].CaseID == "STRICT-PROJECT-ROOT" && forged[i].Type == "case_created" {
				forged[i].RootCaseID = forged[i].CaseID
				forged[i].CaseDigest = eventCaseSpecDigest(forged[i])
				break
			}
		}
		rehashEventChain(t, forged)
		if _, err := validateLedger(forged, cfg); err == nil || !strings.Contains(err.Error(), "root case 不得携带 root_case_id") {
			t.Fatalf("strict replay accepted forged root_case_id on root: %v", err)
		}
	})
	t.Run("second root is rejected even with a matching project", func(t *testing.T) {
		second := testLedgerEvent(t, NewStore(e.data), actorFor(cfg, "penny", "strict-project:second-root", e.office), "case_created", "STRICT-PROJECT-SECOND-ROOT")
		second.Title, second.Project = "Second root", "Strict Project"
		second.Objective, second.Acceptance, second.Constraints, second.Priority = "forged", "forged", "forged", "P1"
		second.SourceRef, second.CaseVersion = source, 1
		second.ToState, second.Owner, second.NextAction = string(statusOpen), "penny", "forged"
		second.CaseDigest = eventCaseSpecDigest(second)
		forged := appendForgedFenceEvent(t, append([]Event(nil), events...), second, "second-root")
		if _, err := validateLedger(forged, cfg); err == nil || !strings.Contains(err.Error(), "不允许第二个 root/project") {
			t.Fatalf("strict replay accepted second root: %v", err)
		}
	})
	t.Run("root with an empty project is rejected", func(t *testing.T) {
		forged := append([]Event(nil), events...)
		for index := range forged {
			if forged[index].CaseID == "STRICT-PROJECT-ROOT" && forged[index].Type == "case_created" {
				forged[index].Project = ""
				forged[index].CaseDigest = eventCaseSpecDigest(forged[index])
				break
			}
		}
		rehashEventChain(t, forged)
		if _, err := validateLedger(forged, cfg); err == nil || !strings.Contains(err.Error(), "root case 必须冻结非空 project") {
			t.Fatalf("strict replay accepted empty root project: %v", err)
		}
	})

	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	root := ledger.snapshot.Cases["STRICT-PROJECT-ROOT"]
	closeEvent := testLedgerEvent(t, NewStore(e.data), actorFor(cfg, "penny", "strict-project:forged-close", e.office), "case_closed", root.ID)
	closeEvent.FromState, closeEvent.ToState = root.Status, string(statusClosed)
	closeEvent.SourceRef, closeEvent.Note, closeEvent.NextAction = source, "forged parent close", "none"
	forgedClose := appendForgedFenceEvent(t, append([]Event(nil), events...), closeEvent, "parent-before-accepted-child")
	if _, err := validateLedger(forgedClose, cfg); err == nil || !strings.Contains(err.Error(), "不得跳过未关闭 descendants") || !strings.Contains(err.Error(), "status=accepted") {
		t.Fatalf("strict replay accepted forged parent close before accepted child: %v", err)
	}
}
