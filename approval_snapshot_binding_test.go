package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func approvalSnapshotFixture(t *testing.T, id, status string) (testEnv, string, string) {
	t.Helper()
	e := setupTestEnv(t)
	caseID := "CASE-" + id
	approvalID := "APR-" + id
	source := writeTestFile(t, filepath.Join(e.office, strings.ToLower(id)+".md"), "# approval snapshot\n")
	e.setActor(t, "penny", "approval-snapshot:"+strings.ToLower(id), e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", id, "--source", source)
	runTestCommand(t, e, "approval", "request", "--id", approvalID, "--case", caseID, "--target", "zantianyou", "--expires", time.Now().UTC().Add(time.Hour).Format(time.RFC3339))
	if status == "granted" {
		runTestCommand(t, e, "approval", "grant", "--id", approvalID, "--issuer", testConfig().ownerPrincipal())
	}
	return e, caseID, approvalID
}

func mutateTargetSeat(t *testing.T, cfg Config, name string) Config {
	t.Helper()
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == name {
			cfg.Agents[index].MaxWIP++
			finalizeTestSeatMutation(&cfg.Agents[index])
			return cfg
		}
	}
	t.Fatalf("missing target seat %s", name)
	return Config{}
}

func writeTestConfig(t *testing.T, path string, cfg Config) {
	t.Helper()
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalRequestFreezesGenerationAndManagerSeat(t *testing.T) {
	e, caseID, approvalID := approvalSnapshotFixture(t, "SNAPSHOT-FIELDS", "requested")
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	request := ledger.approvals[approvalID].Request
	target, _ := cfg.exactRule("zantianyou")
	if request.BasisEventID == "" || request.BasisEventID != ledger.caseGeneration(caseID) ||
		request.AssigneeSeatVersion != target.SeatVersion || request.AssigneeSeatDigest != target.SeatDigest {
		t.Fatalf("approval request did not freeze generation and target seat: %+v target=%+v", request, target)
	}
	view := approvalView(ledger.approvals[approvalID])
	if view.CaseGeneration != request.BasisEventID || view.TargetSeatVersion != target.SeatVersion || view.TargetSeatDigest != target.SeatDigest {
		t.Fatalf("approval view lost frozen snapshot: %+v", view)
	}
}

func completeStandingApprovalRound(t *testing.T, e testEnv, caseID, id string) {
	t.Helper()
	decision := writeIssueApproval(t, e, strings.ToLower(id)+".md", "DEC-"+id, caseID, "", "zantianyou")
	runTestCommand(t, e, "issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "execute ABA round")
	cfg := testConfig()
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, caseID, "issue_prepared")
	e.setActor(t, "zantianyou", "approval-aba-manager:"+strings.ToLower(id), filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", strings.ToLower(id)+"-result.md"), "# result\n")
	runTestCommand(t, e, "report", "--case", caseID, "--result", "completed", "--artifact", artifact, "--next", "owner review")
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report := latestCaseEvent(events, caseID, "report_sent")
	e.setActor(t, "penny", "approval-aba-owner:"+strings.ToLower(id), e.office)
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "round accepted")
}

func prepareApprovalABA(t *testing.T, id, approvalStatus string) (testEnv, string, string) {
	t.Helper()
	e, caseID, approvalID := approvalSnapshotFixture(t, id, approvalStatus)
	completeStandingApprovalRound(t, e, caseID, id)

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	request := ledger.approvals[approvalID].Request
	state := ledger.snapshot.Cases[caseID]
	if state.Owner != "penny" || state.Version != request.CaseVersion || state.Digest != request.CaseDigest || ledger.caseGeneration(caseID) == request.BasisEventID {
		t.Fatalf("fixture did not create owner/version/digest ABA with a new generation: state=%+v request=%+v generation=%s", state, request, ledger.caseGeneration(caseID))
	}
	return e, caseID, approvalID
}

func TestApprovalGrantRejectsCaseGenerationABACommandAndStrictReplay(t *testing.T) {
	t.Run("command", func(t *testing.T) {
		e, _, approvalID := prepareApprovalABA(t, "ABA-COMMAND", "requested")
		before := snapshotTree(t, e.data)
		err := e.app(t).run([]string{"approval", "grant", "--id", approvalID, "--issuer", testConfig().ownerPrincipal()})
		if err == nil || exitCodeForError(err) != exitConflict {
			t.Fatalf("ABA grant was not rejected: %v", err)
		}
		for _, want := range []string{"case generation", "防 ABA", "新 approval_id", "hq approval request"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("ABA correction missing %q: %v", want, err)
			}
		}
		if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) {
			t.Fatal("rejected ABA grant changed ledger")
		}
	})
	t.Run("strict replay", func(t *testing.T) {
		e, _, approvalID := prepareApprovalABA(t, "ABA-REPLAY", "requested")
		err := forgeApprovalGrant(t, e, approvalID)
		if err == nil || !strings.Contains(err.Error(), "case generation") || !strings.Contains(err.Error(), "新 approval_id") {
			t.Fatalf("strict replay accepted ABA grant: %v", err)
		}
	})
}

func appendForgedApprovalIssueBatch(t *testing.T, events []Event, consume, issue Event, tag string) []Event {
	t.Helper()
	last := events[len(events)-1]
	commandID := stableCommandID("forged-approval-issue", tag)
	digest := requestDigest("forged-approval-issue", tag)
	consume.Version, consume.Sequence, consume.At = eventSchemaVersion, last.Sequence+1, last.At
	consume.CommandID, consume.CommandDigest = commandID+":part:1", digest
	consume.PreviousEventHash, consume.EventHash = last.EventHash, ""
	var err error
	consume.EventHash, err = hashEvent(consume)
	if err != nil {
		t.Fatal(err)
	}
	issue.Version, issue.Sequence, issue.At = eventSchemaVersion, consume.Sequence+1, last.At
	issue.CommandID, issue.CommandDigest = commandID, digest
	issue.PreviousEventHash, issue.EventHash = consume.EventHash, ""
	issue.EventHash, err = hashEvent(issue)
	if err != nil {
		t.Fatal(err)
	}
	return append(events, consume, issue)
}

func TestGrantedApprovalRejectsCaseGenerationABAIssueCommandAndStrictReplay(t *testing.T) {
	t.Run("command", func(t *testing.T) {
		e, caseID, approvalID := prepareApprovalABA(t, "ABA-ISSUE-COMMAND", "granted")
		before := snapshotTree(t, e.data)
		err := e.app(t).run([]string{"issue", "--case", caseID, "--to", "zantianyou", "--approval", approvalID, "--next", "stale issue"})
		if err == nil || exitCodeForError(err) != exitConflict {
			t.Fatalf("ABA issue was not rejected: %v", err)
		}
		for _, want := range []string{"case generation", "防 ABA", "新 approval_id", "hq approval request"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("ABA issue correction missing %q: %v", want, err)
			}
		}
		if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) {
			t.Fatal("rejected ABA issue changed ledger")
		}
	})
	t.Run("strict replay", func(t *testing.T) {
		e, caseID, approvalID := prepareApprovalABA(t, "ABA-ISSUE-REPLAY", "granted")
		cfg := testConfig()
		store := NewStore(e.data)
		events, err := store.ReadAll(cfg)
		if err != nil {
			t.Fatal(err)
		}
		ledger, err := validateLedger(events, cfg)
		if err != nil {
			t.Fatal(err)
		}
		record := ledger.approvals[approvalID]
		issue, err := testBusinessIssueEvent(e.app(t), ledger, "penny", "zantianyou", caseID, "aba-approval-replay")
		if err != nil {
			t.Fatal(err)
		}
		issue.AuthorizationType, issue.ApprovalID, issue.DecisionRef = "approval", approvalID, ""
		issue.Issuer, issue.CapturedBy = record.Grant.Issuer, "penny"
		issue.AuthorizationDigest = approvalIssueAuthorizationDigest(record.Request)
		consume := testLedgerEvent(t, store, actorFor(cfg, "penny", "forged-aba-consume", e.office), "approval_consumed", caseID)
		copyApprovalScope(&consume, record.Request)
		consume.RelatedEventID, consume.ApprovalStatus = issue.ID, "consumed"
		events = appendForgedApprovalIssueBatch(t, events, consume, issue, "aba-issue-replay")
		writeRawEvents(t, e.data, events)
		if _, err := store.ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "case generation") || !strings.Contains(err.Error(), "新 approval_id") {
			t.Fatalf("strict replay accepted ABA consume+issue: %v", err)
		}
	})
}

func TestActiveApprovalPinsSeatUntilRequestedOrGrantedRevoke(t *testing.T) {
	for _, status := range []string{"requested", "granted"} {
		t.Run(status, func(t *testing.T) {
			e, _, approvalID := approvalSnapshotFixture(t, "PIN-"+strings.ToUpper(status), status)
			cfg, err := loadConfig(e.config)
			if err != nil {
				t.Fatal(err)
			}
			candidate := mutateTargetSeat(t, cfg, "zantianyou")
			updated, _ := candidate.exactRule("zantianyou")
			decision := writeApprovalDocument(t, filepath.Join(e.office, "decisions", "seat-pin-"+status+".md"), "DEC-SEAT-PIN-"+strings.ToUpper(status), "effective", []ApprovalScope{{
				Action: "staff:update", Target: updated.Name, RequestDigest: staffScopeDigest("staff:update", updated),
			}})
			beforeConfig, err := os.ReadFile(e.config)
			if err != nil {
				t.Fatal(err)
			}
			err = e.app(t).run([]string{"staff", "update", "--name", "zantianyou", "--max-wip", "2", "--approval", decision})
			if err == nil || !strings.Contains(err.Error(), "仍占用冻结 target seat") {
				t.Fatalf("active %s approval did not block seat update: %v", status, err)
			}
			afterConfig, _ := os.ReadFile(e.config)
			if !reflect.DeepEqual(afterConfig, beforeConfig) {
				t.Fatal("rejected seat update changed config")
			}

			runTestCommand(t, e, "approval", "revoke", "--id", approvalID, "--reason", "replace target seat")
			runTestCommand(t, e, "staff", "update", "--name", "zantianyou", "--max-wip", "2", "--approval", decision)
			current, err := loadConfig(e.config)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(e.data).ReadAll(current); err != nil {
				t.Fatalf("terminal approval failed to release frozen seat: %v", err)
			}
		})
	}
}

func TestDirectConfigSeatDriftFailsReplayGrantAndIssue(t *testing.T) {
	t.Run("requested grant", func(t *testing.T) {
		e, _, approvalID := approvalSnapshotFixture(t, "DIRECT-GRANT", "requested")
		cfg, _ := loadConfig(e.config)
		cfg = mutateTargetSeat(t, cfg, "zantianyou")
		writeTestConfig(t, e.config, cfg)
		e.setActor(t, "penny", "direct-drift-grant", e.office)
		if _, err := NewStore(e.data).ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "仍占用冻结 target seat") {
			t.Fatalf("strict replay accepted direct target seat drift: %v", err)
		}
		err := e.app(t).run([]string{"approval", "grant", "--id", approvalID, "--issuer", cfg.ownerPrincipal()})
		if err == nil || !strings.Contains(err.Error(), "冻结 target seat") {
			t.Fatalf("grant accepted direct config seat drift: %v", err)
		}
	})
	t.Run("granted issue", func(t *testing.T) {
		e, caseID, approvalID := approvalSnapshotFixture(t, "DIRECT-ISSUE", "granted")
		cfg, _ := loadConfig(e.config)
		cfg = mutateTargetSeat(t, cfg, "zantianyou")
		writeTestConfig(t, e.config, cfg)
		e.setActor(t, "penny", "direct-drift-issue", e.office)
		if _, err := NewStore(e.data).ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "仍占用冻结 target seat") {
			t.Fatalf("strict replay accepted granted target seat drift: %v", err)
		}
		err := e.app(t).run([]string{"issue", "--case", caseID, "--to", "zantianyou", "--approval", approvalID, "--next", "work"})
		if err == nil || !strings.Contains(err.Error(), "冻结 target seat") {
			t.Fatalf("issue accepted direct config seat drift: %v", err)
		}
	})
}

func TestStrictReplayRejectsDanglingApprovalConsume(t *testing.T) {
	e, _, approvalID := approvalSnapshotFixture(t, "DANGLING-CONSUME", "granted")
	cfg := testConfig()
	store := NewStore(e.data)
	events, err := store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	record := ledger.approvals[approvalID]
	witness, _ := cfg.approvalWitness()
	event := testLedgerEvent(t, store, actorFor(cfg, witness.Name, "dangling-consume", e.office), "approval_consumed", record.Request.CaseID)
	copyApprovalScope(&event, record.Request)
	event.ApprovalStatus = "consumed"
	event.RelatedEventID = "EVT-DANGLING-ISSUE"
	events = appendForgedFenceEvent(t, events, event, "dangling-consume")
	writeRawEvents(t, e.data, events)
	if _, err := store.ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "缺少匹配的 issue_prepared") {
		t.Fatalf("strict replay accepted dangling consume: %v", err)
	}
}

func TestStrictReplayRejectsForgedAssignmentAboveSeatMaxWIP(t *testing.T) {
	e := setupTestEnv(t)
	e.setActor(t, "zantianyou", "forged-wip:manager", filepath.Join(e.root, "engineering"))
	for index, caseID := range []string{"FORGED-WIP-ONE", "FORGED-WIP-TWO"} {
		source := writeTestFile(t, filepath.Join(e.root, "engineering", strings.ToLower(caseID)+".md"), "# forged WIP\n")
		args := []string{"case", "create", "--id", caseID, "--title", caseID, "--source", source}
		if index != 0 {
			args = append(args, "--parent", "FORGED-WIP-ONE")
		}
		runTestCommand(t, e, args...)
	}
	cfg := testConfig()
	store := NewStore(e.data)
	events, err := store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := testBusinessIssueEvent(e.app(t), ledger, "zantianyou", "eng-developer", "FORGED-WIP-ONE", "one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := testBusinessIssueEvent(e.app(t), ledger, "zantianyou", "eng-developer", "FORGED-WIP-TWO", "two")
	if err != nil {
		t.Fatal(err)
	}
	events = appendForgedFenceEvent(t, events, first, "wip-one")
	events = appendForgedFenceEvent(t, events, second, "wip-two")
	writeRawEvents(t, e.data, events)
	_, err = store.ReadAll(cfg)
	if err == nil || !strings.Contains(err.Error(), "active/pending assignment=2") || !strings.Contains(err.Error(), "max_wip=1") {
		t.Fatalf("strict replay accepted forged WIP above seat capacity: %v", err)
	}
}

func TestIssuePreparedStrictReplayRejectsDuplicateAssignmentIDBeforeDelivery(t *testing.T) {
	for _, test := range []struct {
		name, secondTarget string
	}{
		{name: "same assignee", secondTarget: "eng-developer"},
		{name: "different assignee", secondTarget: "eng-data-engineer"},
	} {
		t.Run(test.name, func(t *testing.T) {
			e := setupTestEnv(t)
			e.setActor(t, "zantianyou", "duplicate-pending:manager", filepath.Join(e.root, "engineering"))
			for index, caseID := range []string{"DUPLICATE-PENDING-ONE", "DUPLICATE-PENDING-TWO"} {
				source := writeTestFile(t, filepath.Join(e.root, "engineering", strings.ToLower(caseID)+".md"), "# duplicate pending assignment\n")
				args := []string{"case", "create", "--id", caseID, "--title", caseID, "--source", source}
				if index != 0 {
					args = append(args, "--parent", "DUPLICATE-PENDING-ONE")
				}
				runTestCommand(t, e, args...)
			}
			cfg := testConfig()
			store := NewStore(e.data)
			events, err := store.ReadAll(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := validateLedger(events, cfg)
			if err != nil {
				t.Fatal(err)
			}
			app := e.app(t)
			first, err := testBusinessIssueEvent(app, ledger, "zantianyou", "eng-developer", "DUPLICATE-PENDING-ONE", "one")
			if err != nil {
				t.Fatal(err)
			}
			second, err := testBusinessIssueEvent(app, ledger, "zantianyou", test.secondTarget, "DUPLICATE-PENDING-TWO", "two")
			if err != nil {
				t.Fatal(err)
			}
			second.AssignmentID = first.AssignmentID
			second.AssignmentDigest = assignmentContractDigest(second)
			payload, err := app.deliveryPayload(second)
			if err != nil {
				t.Fatal(err)
			}
			second.PayloadDigest = digestText(payload)
			events = appendForgedFenceEvent(t, events, first, "duplicate-pending-one")
			events = appendForgedFenceEvent(t, events, second, "duplicate-pending-two")
			writeRawEvents(t, e.data, events)
			if _, err := store.ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "assignment_id 重复") {
				t.Fatalf("strict replay accepted duplicate pending assignment_id across %s: %v", test.name, err)
			}
		})
	}
}

func TestRegistryRejectsWitnessManagerAndNonAcceptingOrderSeat(t *testing.T) {
	t.Run("approval witness cannot also be manager", func(t *testing.T) {
		cfg := portableRegistry("role-guard-hq", "executive", "delivery", "relay", "bridge", "maker")
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == "relay" {
				cfg.Agents[index].Responsibilities = append(cfg.Agents[index].Responsibilities, roleManagerPrefix+cfg.Agents[index].Department)
				break
			}
		}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "approval_witness 不得兼任 manager") {
			t.Fatalf("registry accepted witness-manager authorization overlap: %v", err)
		}
	})

	t.Run("order recipient must be able to accept", func(t *testing.T) {
		cfg := testConfig()
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == "zantianyou" {
				cfg.Agents[index].CanAccept = false
				break
			}
		}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "can_receive_order 但缺少 can_accept") {
			t.Fatalf("registry accepted an order seat that cannot accept its assignment: %v", err)
		}
	})
}

func TestApprovalRequestedStrictReplayRejectsForgedGenerationOrSeatSnapshot(t *testing.T) {
	for _, mutation := range []string{"generation", "seat"} {
		t.Run(mutation, func(t *testing.T) {
			e := setupTestEnv(t)
			caseID := "FORGED-SNAPSHOT-" + strings.ToUpper(mutation)
			source := writeTestFile(t, filepath.Join(e.office, strings.ToLower(caseID)+".md"), "# forged snapshot\n")
			e.setActor(t, "penny", "forged-snapshot:"+mutation, e.office)
			runTestCommand(t, e, "case", "create", "--id", caseID, "--title", caseID, "--source", source)
			cfg := testConfig()
			store := NewStore(e.data)
			events, err := store.ReadAll(cfg)
			if err != nil {
				t.Fatal(err)
			}
			ledger, err := validateLedger(events, cfg)
			if err != nil {
				t.Fatal(err)
			}
			state := ledger.snapshot.Cases[caseID]
			target, _ := cfg.exactRule("zantianyou")
			event := testLedgerEvent(t, store, actorFor(cfg, "penny", "forged-snapshot-event", e.office), "approval_requested", caseID)
			event.ApprovalID, event.ApprovalAction, event.ApprovalStatus, event.ApprovalMode = "APR-"+caseID, "issue", "requested", "one_time"
			event.Recipient, event.RecipientLabel = target.Name, target.Label
			event.CaseVersion, event.CaseDigest, event.Title = state.Version, state.Digest, state.Title
			event.BasisEventID = ledger.caseGeneration(caseID)
			event.AssigneeSeatVersion, event.AssigneeSeatDigest = target.SeatVersion, target.SeatDigest
			event.ExpiresAt = time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
			if mutation == "generation" {
				event.BasisEventID = "EVT-FORGED-GENERATION"
			} else {
				event.AssigneeSeatDigest = digestText("forged-seat")
			}
			events = appendForgedFenceEvent(t, events, event, "forged-snapshot-"+mutation)
			writeRawEvents(t, e.data, events)
			_, err = store.ReadAll(cfg)
			if err == nil || !strings.Contains(err.Error(), mutation) {
				t.Fatalf("strict replay accepted forged %s snapshot: %v", mutation, err)
			}
		})
	}
}

func TestApprovalAuthorizationDigestBindsGenerationAndSeat(t *testing.T) {
	e, caseID, approvalID := approvalSnapshotFixture(t, "AUTH-DIGEST", "granted")
	cfg := testConfig()
	store := NewStore(e.data)
	events, err := store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	record := ledger.approvals[approvalID]
	issue, err := testBusinessIssueEvent(e.app(t), ledger, "penny", "zantianyou", caseID, "forged-authorization-digest")
	if err != nil {
		t.Fatal(err)
	}
	issue.AuthorizationType, issue.ApprovalID, issue.DecisionRef = "approval", approvalID, ""
	issue.Issuer, issue.CapturedBy = record.Grant.Issuer, "penny"
	issue.AuthorizationDigest = requestDigest("approval-case-v1-without-generation-seat", approvalID, caseID)
	consume := testLedgerEvent(t, store, actorFor(cfg, "penny", "forged-auth-consume", e.office), "approval_consumed", caseID)
	copyApprovalScope(&consume, record.Request)
	consume.RelatedEventID, consume.ApprovalStatus = issue.ID, "consumed"
	events = appendForgedApprovalIssueBatch(t, events, consume, issue, "authorization-digest")
	writeRawEvents(t, e.data, events)
	if _, err := store.ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "authorization digest") {
		t.Fatalf("strict replay accepted authorization digest without generation/seat: %v", err)
	}
}

func TestConfigAndIssueDefendReceiveRequiresAccept(t *testing.T) {
	cfg := testConfig()
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "eng-developer" {
			cfg.Agents[index].CanAccept = false
			finalizeTestSeatMutation(&cfg.Agents[index])
		}
	}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "can_receive_order") || !strings.Contains(err.Error(), "can_accept") {
		t.Fatalf("config accepted receive-only target: %v", err)
	}

	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "receive-without-accept.md"), "# source\n")
	e.setActor(t, "zantianyou", "receive-without-accept-manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "case", "create", "--id", "RECEIVE-WITHOUT-ACCEPT", "--title", "receive without accept", "--source", source)
	app := e.app(t)
	for index := range app.Config.Agents {
		if app.Config.Agents[index].Name == "eng-developer" {
			app.Config.Agents[index].CanAccept = false
			finalizeTestSeatMutation(&app.Config.Agents[index])
		}
	}
	if err := app.run([]string{"issue", "--case", "RECEIVE-WITHOUT-ACCEPT", "--to", "eng-developer", "--next", "work"}); err == nil || !strings.Contains(err.Error(), "can_receive_order/can_accept") {
		t.Fatalf("issue command accepted receive-only target: %v", err)
	}

	validCfg := testConfig()
	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	forged, err := testBusinessIssueEvent(e.app(t), ledger, "zantianyou", "eng-developer", "RECEIVE-WITHOUT-ACCEPT", "receive-without-accept-replay")
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(validCfg)
	if err != nil {
		t.Fatal(err)
	}
	events = appendForgedFenceEvent(t, events, forged, "receive-without-accept-replay")
	writeRawEvents(t, e.data, events)
	if _, err := NewStore(e.data).ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "can_receive_order/can_accept") {
		t.Fatalf("strict replay accepted receive-only target: %v", err)
	}
}

func TestApprovalWitnessCannotUseManagerAuthorization(t *testing.T) {
	cfg := testConfig()
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "penny" {
			cfg.Agents[index].Responsibilities = append(cfg.Agents[index].Responsibilities, roleManagerPrefix+cfg.Agents[index].Department)
			finalizeTestSeatMutation(&cfg.Agents[index])
		}
	}
	if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "approval_witness") || !strings.Contains(err.Error(), "不得兼任 manager") {
		t.Fatalf("config accepted approval_witness manager overlap: %v", err)
	}

	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "witness-manager-bypass.md"), "# source\n")
	e.setActor(t, "penny", "witness-manager-valid", e.office)
	runTestCommand(t, e, "case", "create", "--id", "WITNESS-MANAGER-BYPASS", "--title", "bypass", "--source", source)
	app := e.app(t)
	witness, _ := cfg.exactRule("penny")
	app.Config = cfg
	app.Identity.(*fakeIdentityProvider).actors["witness-manager-invalid"] = Actor{Name: witness.Name, Label: witness.Label, Department: witness.Department, PaneID: "witness-manager-invalid", CWD: e.office, Rule: witness}
	app.CallerPane = "witness-manager-invalid"
	if err := app.run([]string{"issue", "--case", "WITNESS-MANAGER-BYPASS", "--to", "zantianyou", "--next", "bypass"}); err == nil || !strings.Contains(err.Error(), "approval_witness 不得兼任 manager") {
		t.Fatalf("witness used manager authorization in command: %v", err)
	}

	validCfg := testConfig()
	ledger, err := validateLedger(mustReadEvents(t, e.data, validCfg), validCfg)
	if err != nil {
		t.Fatal(err)
	}
	forged, err := testBusinessIssueEvent(app, ledger, "penny", "zantianyou", "WITNESS-MANAGER-BYPASS", "witness-manager-replay")
	if err != nil {
		t.Fatal(err)
	}
	events := mustReadEvents(t, e.data, validCfg)
	events = appendForgedFenceEvent(t, events, forged, "witness-manager-replay")
	writeRawEvents(t, e.data, events)
	if _, err := NewStore(e.data).ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "manager issue") {
		t.Fatalf("strict replay accepted witness manager authorization: %v", err)
	}
}

func mustReadEvents(t *testing.T, data string, cfg Config) []Event {
	t.Helper()
	events, err := NewStore(data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestFailedPreSendOccupiesExactlyOneWIPAndTailRejectsOverbook(t *testing.T) {
	t.Run("one pending is one WIP", func(t *testing.T) {
		e := setupTestEnv(t)
		caseID := "ONE-PENDING-WIP"
		source := writeTestFile(t, filepath.Join(e.office, "one-pending-wip.md"), "# source\n")
		decision := writeIssueApproval(t, e, "one-pending-wip-decision.md", "DEC-ONE-PENDING-WIP", caseID, "", "zantianyou")
		e.setActor(t, "penny", "one-pending-wip", e.office)
		runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "one pending", "--source", source)
		e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("offline before send")
		if err := e.app(t).run([]string{"issue", "--case", caseID, "--to", "zantianyou", "--decision", decision, "--next", "work"}); err == nil {
			t.Fatal("failed-pre-send fixture unexpectedly succeeded")
		}
		ledger, err := e.app(t).ledgerState()
		if err != nil {
			t.Fatalf("one pending issue violated promoted tail invariant: %v", err)
		}
		if used := ledger.assignmentCapacityUsed("zantianyou"); used != 1 {
			t.Fatalf("one failed-pre-send issue consumed %d WIP, want 1", used)
		}
	})

	t.Run("forged overbook fails tail", func(t *testing.T) {
		e := setupTestEnv(t)
		e.setActor(t, "penny", "forged-overbook", e.office)
		for index, caseID := range []string{"FORGED-WIP-ONE", "FORGED-WIP-TWO"} {
			source := writeTestFile(t, filepath.Join(e.office, strings.ToLower(caseID)+".md"), "# source\n")
			args := []string{"case", "create", "--id", caseID, "--title", caseID, "--source", source}
			if index != 0 {
				args = append(args, "--parent", "FORGED-WIP-ONE")
			}
			runTestCommand(t, e, args...)
		}
		cfg := testConfig()
		events := mustReadEvents(t, e.data, cfg)
		ledger := newLedgerState()
		for _, event := range events {
			if err := ledger.validateAndApply(event, cfg); err != nil {
				t.Fatal(err)
			}
		}
		for index, caseID := range []string{"FORGED-WIP-ONE", "FORGED-WIP-TWO"} {
			forged, err := testBusinessIssueEvent(e.app(t), ledger, "penny", "zantianyou", caseID, "overbook-"+caseID)
			if err != nil {
				t.Fatal(err)
			}
			events = appendForgedFenceEvent(t, events, forged, "overbook-"+caseID)
			applied := events[len(events)-1]
			if err := ledger.validateAndApply(applied, cfg); err != nil {
				t.Fatalf("forged pending issue %d failed before tail WIP check: %v", index+1, err)
			}
		}
		writeRawEvents(t, e.data, events)
		if _, err := NewStore(e.data).ReadAll(cfg); err == nil || !strings.Contains(err.Error(), "超过 registry max_wip") {
			t.Fatalf("promoted tail accepted forged WIP overbook: %v", err)
		}
	})
}
