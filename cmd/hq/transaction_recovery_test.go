package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testLedgerEvent(t *testing.T, store *Store, actor Actor, eventType, caseID string) Event {
	t.Helper()
	id, err := newEventID()
	if err != nil {
		t.Fatal(err)
	}
	return Event{
		ID: id, CaseID: caseID, At: store.NowTime().Format(time.RFC3339), Type: eventType,
		Actor: actor.Name, ActorLabel: actor.Label, ActorPaneID: actor.PaneID,
	}
}

func setTestCaseSpec(event *Event, title, source string) {
	spec := CaseSpec{CaseID: event.CaseID, Title: title, Project: "test-project", Objective: "test objective", Acceptance: "test acceptance", Constraints: "test constraints", Priority: "P1", SourceRef: source, Version: 1}
	spec.Digest = caseSpecDigest(spec)
	event.Title, event.Project, event.Objective, event.Acceptance, event.Constraints = spec.Title, spec.Project, spec.Objective, spec.Acceptance, spec.Constraints
	event.Priority, event.SourceRef, event.CaseVersion, event.CaseDigest = spec.Priority, spec.SourceRef, spec.Version, spec.Digest
}

func setTestAssignmentContract(event *Event, state *CaseState) {
	event.Project = state.Project
	event.AssignmentID = stableCommandID("assignment-test", event.CaseID, event.ID)
	event.AssignmentIssuer = event.Actor
	rule, ok := testConfig().exactRule(event.Recipient)
	if !ok {
		panic("test assignment recipient is not registered: " + event.Recipient)
	}
	event.AssigneeSeatVersion, event.AssigneeSeatDigest = rule.SeatVersion, rule.SeatDigest
	event.RoleCardID, event.RoleCardVersion = rule.RoleCardID, rule.RoleCardVersion
	event.RoleCardDigest, event.RoleCardManualPath = rule.RoleCardDigest, rule.ManualPath
	event.Reviewer, event.ReviewerLabel = event.Actor, event.ActorLabel
	event.Acceptor, event.AcceptorLabel = event.Actor, event.ActorLabel
	event.AssignmentDigest = assignmentContractDigest(*event)
}

func copyTestAssignmentContract(target *Event, source Event) {
	target.Project = source.Project
	copyAssignmentBinding(target, source)
}

func setTestEmptyTurnBundleManifest(event *Event, origin Event, cfg Config, basePayload string) {
	policy := cfg.effectiveDeliveryPolicy()
	event.TurnBundleVersion = turnBundleVersion
	event.TurnBundleBasePayload = basePayload
	event.TurnPromptDigest = digestText(basePayload)
	event.TurnBundleMaxItems, event.TurnBundleMaxBytes = policy.MaxBundleItems, policy.MaxBundleBytes
	event.TurnBundleDigest = turnBundleManifestDigest(*event, origin)
}

func transactCreateCase(store *Store, cfg Config, actor Actor, caseID, commandSuffix string, dryRun bool) (TransactionResult, error) {
	commandID := stableCommandID("case-create-test", caseID, commandSuffix)
	digest := requestDigest("case-create-test", actor.Name, caseID, commandSuffix)
	return store.Transact(cfg, commandID, digest, dryRun, func(ledger *ledgerState) (Event, error) {
		event := Event{
			ID: stableCommandID("event", caseID, commandSuffix), CaseID: caseID,
			At: store.NowTime().Format(time.RFC3339), Type: "case_created",
			Actor: actor.Name, ActorLabel: actor.Label,
			ToState: string(statusOpen), Owner: actor.Name, NextAction: "test",
		}
		setTestCaseSpec(&event, caseID, "test:/source")
		if root := ledger.soleRootCase(); root != nil {
			event.ParentCaseID, event.RootCaseID, event.Project = root.ID, root.ID, root.Project
			event.CaseDigest = eventCaseSpecDigest(event)
		}
		return event, nil
	})
}

func prepareDeliveredIssue(t *testing.T, store *Store, cfg Config, caseID string) Event {
	t.Helper()
	penny := actorFor(cfg, "penny", "penny-pane", "/test")
	if _, err := transactCreateCase(store, cfg, penny, caseID, "setup", false); err != nil {
		t.Fatal(err)
	}
	commandID := stableCommandID("order-prepare-test", caseID)
	deliveryID := stableDeliveryID(commandID, "zantianyou")
	payloadDigest := digestText("payload:" + caseID)
	preparedResult, err := store.Transact(cfg, commandID, requestDigest("order-prepare-test", caseID), false, func(ledger *ledgerState) (Event, error) {
		state, err := ledger.currentCase(caseID)
		if err != nil {
			return Event{}, err
		}
		event := testLedgerEvent(t, store, penny, "issue_prepared", caseID)
		event.FromState, event.Title = state.Status, state.Title
		event.Recipient, event.RecipientLabel = "zantianyou", "工程部-詹天佑"
		event.CaseVersion, event.CaseDigest = state.Version, state.Digest
		setTestAssignmentContract(&event, state)
		event.AuthorizationType, event.AuthorizationDigest, event.DecisionRef = "standing_decision", digestText("test authorization"), "test:/decision"
		event.NextAction = "test"
		event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "issue-fixed"
		event.Delivery, event.DeliveryID, event.PayloadDigest = deliveryPrepared, deliveryID, payloadDigest
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	prepared := preparedResult.Event
	attemptResult, err := store.Transact(cfg, stableCommandID("delivery-attempt-test", deliveryID), requestDigest("delivery-attempt-test", deliveryID), false, func(*ledgerState) (Event, error) {
		event := testLedgerEvent(t, store, penny, "delivery_attempted", caseID)
		event.RelatedEventID = prepared.ID
		event.Recipient, event.RecipientLabel = prepared.Recipient, prepared.RecipientLabel
		event.Delivery, event.DeliveryID, event.PayloadDigest = deliveryAttempted, deliveryID, payloadDigest
		setTestEmptyTurnBundleManifest(&event, prepared, cfg, "payload:"+caseID)
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sentResult, err := store.Transact(cfg, stableCommandID("order-sent-test", deliveryID), requestDigest("order-sent-test", deliveryID), false, func(*ledgerState) (Event, error) {
		event := testLedgerEvent(t, store, penny, "issue_sent", caseID)
		event.RelatedEventID, event.AttemptEventID = prepared.ID, attemptResult.Event.ID
		event.Recipient, event.RecipientLabel = prepared.Recipient, prepared.RecipientLabel
		event.Delivery, event.DeliveryID, event.PayloadDigest = deliverySent, deliveryID, payloadDigest
		event.FromState, event.ToState, event.Owner = string(statusOpen), string(statusDispatched), "zantianyou"
		event.CaseVersion, event.CaseDigest = prepared.CaseVersion, prepared.CaseDigest
		copyTestAssignmentContract(&event, prepared)
		event.AuthorizationType, event.AuthorizationDigest, event.DecisionRef = prepared.AuthorizationType, prepared.AuthorizationDigest, prepared.DecisionRef
		event.NextAction = prepared.NextAction
		event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = prepared.DeliveryMode, prepared.DeliveryTarget, prepared.DeliveryReason
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return sentResult.Event
}

func TestTransactionRecoveryConcurrentSameCaseCreate100Rounds(t *testing.T) {
	cfg := testConfig()
	data := filepath.Join(canonicalTestTempDir(t), "records")
	penny := actorFor(cfg, "penny", "p", "/test")
	for round := 0; round < 100; round++ {
		caseID := fmt.Sprintf("CONCURRENT-CREATE-%03d", round)
		stores := []*Store{NewStore(data), NewStore(data)}
		results := make(chan error, 2)
		var group sync.WaitGroup
		for contender := 0; contender < 2; contender++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				_, err := transactCreateCase(stores[index], cfg, penny, caseID, fmt.Sprintf("contender-%d", index), false)
				results <- err
			}(contender)
		}
		group.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("round %d: successes=%d", round, successes)
		}
	}
	events, err := NewStore(data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 100 {
		t.Fatalf("same-case create produced %d events, want 100", len(events))
	}
}

func TestTransactionRecoveryConcurrentAcceptVsReturn100Rounds(t *testing.T) {
	cfg := testConfig()
	root := canonicalTestTempDir(t)
	worker := actorFor(cfg, "zantianyou", "z", "/test")
	resolved := 0
	for round := 0; round < 100; round++ {
		// Each round is an independent race. Reusing one ledger would retain an
		// accepted active assignment from a prior round and legitimately exceed
		// the seat's max_wip=1 under the strict tail invariant.
		data := filepath.Join(root, fmt.Sprintf("round-%03d", round), "records")
		caseID := fmt.Sprintf("ACCEPT-RETURN-%03d", round)
		sent := prepareDeliveredIssue(t, NewStore(data), cfg, caseID)
		results := make(chan error, 2)
		var group sync.WaitGroup
		for _, action := range []string{"accept", "return"} {
			group.Add(1)
			go func(action string) {
				defer group.Done()
				store := NewStore(data)
				commandID := stableCommandID(action+"-test", sent.ID)
				_, err := store.Transact(cfg, commandID, requestDigest(action+"-test", sent.ID), false, func(ledger *ledgerState) (Event, error) {
					state, err := ledger.currentCase(caseID)
					if err != nil {
						return Event{}, err
					}
					semantic, ok := semanticDeliveredEvent(sent, ledger.events)
					if !ok {
						return Event{}, fmt.Errorf("sent event missing")
					}
					eventType, target, owner := "event_accepted", string(statusInProgress), worker.Name
					if action == "return" {
						eventType, target, owner = "event_returned", string(statusReturned), semantic.Actor
					}
					event := testLedgerEvent(t, store, worker, eventType, caseID)
					event.RelatedEventID, event.FromState, event.ToState = sent.ID, state.Status, target
					event.Owner, event.Recipient, event.RecipientLabel = owner, semantic.Actor, semantic.ActorLabel
					event.NextAction = "test"
					if action == "return" {
						event.Note = "test return"
					}
					event.SourceRef, event.ArtifactRef, event.Verification = semantic.SourceRef, semantic.ArtifactRef, semantic.Verification
					event.CaseVersion, event.CaseDigest = semantic.CaseVersion, semantic.CaseDigest
					copyAssignmentBinding(&event, semantic)
					if action == "accept" {
						event.AcceptanceDigest = acceptanceReceiptDigest(sent.ID, semantic, event)
					}
					event.Delivery = deliveryPrepared
					event.DeliveryID = stableDeliveryID(commandID, semantic.Actor)
					event.PayloadDigest = digestText(action + ":" + caseID)
					return event, nil
				})
				results <- err
			}(action)
		}
		group.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("round %d: successes=%d", round, successes)
		}
		events, err := NewStore(data).ReadAll(cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == "event_accepted" || event.Type == "event_returned" {
				resolved++
			}
		}
	}
	if resolved != 100 {
		t.Fatalf("resolved events=%d, want 100", resolved)
	}
}

func TestTransactionRecoveryConcurrentDoubleClose100Rounds(t *testing.T) {
	cfg := testConfig()
	data := filepath.Join(canonicalTestTempDir(t), "records")
	penny := actorFor(cfg, "penny", "p", "/test")
	if _, err := transactCreateCase(NewStore(data), cfg, penny, "DOUBLE-CLOSE-ROOT", "root", false); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 100; round++ {
		caseID := fmt.Sprintf("DOUBLE-CLOSE-%03d", round)
		if _, err := transactCreateCase(NewStore(data), cfg, penny, caseID, "setup", false); err != nil {
			t.Fatal(err)
		}
		results := make(chan error, 2)
		var group sync.WaitGroup
		for contender := 0; contender < 2; contender++ {
			group.Add(1)
			go func(index int) {
				defer group.Done()
				store := NewStore(data)
				commandID := stableCommandID("close-test", caseID, fmt.Sprintf("%d", index))
				_, err := store.Transact(cfg, commandID, requestDigest("close-test", caseID, fmt.Sprintf("%d", index)), false, func(ledger *ledgerState) (Event, error) {
					state, err := ledger.currentCase(caseID)
					if err != nil {
						return Event{}, err
					}
					event := testLedgerEvent(t, store, penny, "case_closed", caseID)
					event.FromState, event.ToState = state.Status, string(statusClosed)
					event.SourceRef, event.Note, event.NextAction = "test:/close", "done", "done"
					return event, nil
				})
				results <- err
			}(contender)
		}
		group.Wait()
		close(results)
		successes := 0
		for err := range results {
			if err == nil {
				successes++
			}
		}
		if successes != 1 {
			t.Fatalf("round %d: successes=%d", round, successes)
		}
	}
	snapshot, err := NewStore(data).Snapshot(cfg)
	if err != nil {
		t.Fatal(err)
	}
	closed := 0
	for _, state := range snapshot.Cases {
		if state.Status == string(statusClosed) {
			closed++
		}
	}
	if closed != 100 {
		t.Fatalf("closed cases=%d, want 100", closed)
	}
}

func TestTransactionRecoveryCommandIdempotencyConcurrent100Callers(t *testing.T) {
	cfg := testConfig()
	data := filepath.Join(canonicalTestTempDir(t), "records")
	penny := actorFor(cfg, "penny", "p", "/test")
	commandID := stableCommandID("idempotent-create", "IDEMPOTENT-001")
	digest := requestDigest("idempotent-create", "IDEMPOTENT-001")
	ids := make(chan string, 100)
	errorsCh := make(chan error, 100)
	var group sync.WaitGroup
	for i := 0; i < 100; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			store := NewStore(data)
			result, err := store.Transact(cfg, commandID, digest, false, func(*ledgerState) (Event, error) {
				event := testLedgerEvent(t, store, penny, "case_created", "IDEMPOTENT-001")
				setTestCaseSpec(&event, "idempotent", "test:/source")
				event.ToState, event.Owner, event.NextAction = string(statusOpen), penny.Name, "test"
				return event, nil
			})
			if err != nil {
				errorsCh <- err
				return
			}
			ids <- result.Event.ID
		}()
	}
	group.Wait()
	close(ids)
	close(errorsCh)
	for err := range errorsCh {
		t.Fatal(err)
	}
	unique := map[string]bool{}
	for id := range ids {
		unique[id] = true
	}
	if len(unique) != 1 {
		t.Fatalf("idempotent callers observed %d outcomes", len(unique))
	}
	events, err := NewStore(data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("event count=%d, want 1", len(events))
	}
}

func TestTransactionRecoveryClockRollbackAcrossMonthsUsesSequence(t *testing.T) {
	cfg := testConfig()
	data := filepath.Join(canonicalTestTempDir(t), "records")
	store := NewStore(data)
	penny := actorFor(cfg, "penny", "p", "/test")
	store.Now = func() time.Time { return time.Date(2026, 9, 1, 0, 0, 1, 0, time.FixedZone("+08", 8*3600)) }
	created, err := transactCreateCase(store, cfg, penny, "CLOCK-ROLLBACK-001", "create", false)
	if err != nil {
		t.Fatal(err)
	}
	store.Now = func() time.Time { return time.Date(2026, 8, 31, 23, 59, 59, 0, time.FixedZone("+08", 8*3600)) }
	closed, err := store.Transact(cfg, stableCommandID("clock-close", "CLOCK-ROLLBACK-001"), requestDigest("clock-close", "CLOCK-ROLLBACK-001"), false, func(ledger *ledgerState) (Event, error) {
		state, err := ledger.currentCase("CLOCK-ROLLBACK-001")
		if err != nil {
			return Event{}, err
		}
		event := testLedgerEvent(t, store, penny, "case_closed", "CLOCK-ROLLBACK-001")
		event.FromState, event.ToState = state.Status, string(statusClosed)
		event.SourceRef, event.Note, event.NextAction = "test:/close", "done", "done"
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Event.Sequence != 1 || closed.Event.Sequence != 2 || closed.Event.PreviousEventHash != created.Event.EventHash {
		t.Fatalf("bad chain across clock rollback: created=%+v closed=%+v", created.Event, closed.Event)
	}
	events, err := NewStore(data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Sequence != 1 || events[1].Sequence != 2 {
		t.Fatalf("replay order changed by filenames: %+v", events)
	}
}

func TestTransactionRecoveryStateTailAutoRebuildMatrix(t *testing.T) {
	mutations := map[string]func(string, Snapshot, []Event) error{
		"missing": func(path string, _ Snapshot, _ []Event) error { return os.Remove(path) },
		"corrupt": func(path string, _ Snapshot, _ []Event) error { return os.WriteFile(path, []byte("{"), 0o600) },
		"same_count_old_tail": func(path string, snapshot Snapshot, events []Event) error {
			snapshot.LastSequence, snapshot.LastEventHash = events[0].Sequence, events[0].EventHash
			raw, _ := json.Marshal(snapshot)
			return os.WriteFile(path, raw, 0o600)
		},
		"tail_matches_state_wrong": func(path string, snapshot Snapshot, _ []Event) error {
			snapshot.Cases["STATE-TAIL-001"].Status = string(statusOpen)
			raw, _ := json.Marshal(snapshot)
			return os.WriteFile(path, raw, 0o600)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig()
			data := filepath.Join(canonicalTestTempDir(t), "records")
			store := NewStore(data)
			penny := actorFor(cfg, "penny", "p", "/test")
			if _, err := transactCreateCase(store, cfg, penny, "STATE-TAIL-001", "create", false); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Transact(cfg, stableCommandID("state-close", name), requestDigest("state-close", name), false, func(ledger *ledgerState) (Event, error) {
				state, _ := ledger.currentCase("STATE-TAIL-001")
				event := testLedgerEvent(t, store, penny, "case_closed", "STATE-TAIL-001")
				event.FromState, event.ToState = state.Status, string(statusClosed)
				event.SourceRef, event.Note, event.NextAction = "test:/close", "done", "done"
				return event, nil
			}); err != nil {
				t.Fatal(err)
			}
			expected, err := store.Snapshot(cfg)
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.ReadAll(cfg)
			if err != nil {
				t.Fatal(err)
			}
			statePath := filepath.Join(data, "state.json")
			expectedRaw, _ := json.Marshal(expected)
			var tampered Snapshot
			if err := json.Unmarshal(expectedRaw, &tampered); err != nil {
				t.Fatal(err)
			}
			if err := mutate(statePath, tampered, events); err != nil {
				t.Fatal(err)
			}
			actual, err := NewStore(data).Snapshot(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !snapshotsEqual(actual, expected) {
				t.Fatalf("state was not rebuilt\nactual=%+v\nexpected=%+v", actual, expected)
			}
		})
	}
}

func TestTransactionRecoveryCrashRecoveryFailpointMatrix(t *testing.T) {
	recoverable := []string{
		"journal_intent_rename", "journal_parent_fsync", "log_append_partial", "log_file_fsync", "log_parent_fsync",
		"state_temp_write", "state_temp_fsync", "state_rename", "state_parent_fsync",
		"journal_cleanup", "journal_cleanup_parent_fsync",
	}
	for _, point := range recoverable {
		t.Run(point, func(t *testing.T) {
			cfg := testConfig()
			fixedNow := time.Date(2026, time.August, 28, 12, 0, 0, 0, time.UTC)
			caseID := "RECOVER-" + strings.ToUpper(strings.ReplaceAll(point, "_", "-"))
			expectedStore := NewStore(filepath.Join(canonicalTestTempDir(t), "expected-records"))
			expectedStore.Now = func() time.Time { return fixedNow }
			penny := actorFor(cfg, "penny", "p", "/test")
			if _, err := transactCreateCase(expectedStore, cfg, penny, caseID, point, false); err != nil {
				t.Fatal(err)
			}
			expectedEvents, err := expectedStore.ReadAll(cfg)
			if err != nil {
				t.Fatal(err)
			}
			expectedSnapshot, err := expectedStore.Snapshot(cfg)
			if err != nil {
				t.Fatal(err)
			}
			data := filepath.Join(canonicalTestTempDir(t), "records")
			store := NewStore(data)
			store.Now = func() time.Time { return fixedNow }
			fired := false
			store.Failpoint = func(name string) error {
				if name == point && !fired {
					fired = true
					return errors.New("simulated crash")
				}
				return nil
			}
			if _, err := transactCreateCase(store, cfg, penny, caseID, point, false); err == nil {
				t.Fatal("failpoint did not interrupt transaction")
			}
			if !fired {
				t.Fatalf("failpoint %s was not reached", point)
			}
			restarted := NewStore(data)
			events, err := restarted.ReadAll(cfg)
			if err != nil {
				t.Fatalf("restart recovery failed: %v", err)
			}
			if len(events) != 1 || len(expectedEvents) != 1 || events[0].Sequence != 1 || events[0].EventHash != expectedEvents[0].EventHash {
				t.Fatalf("recovery outcome=%+v", events)
			}
			actualSnapshot, err := restarted.Snapshot(cfg)
			if err != nil {
				t.Fatal(err)
			}
			if !snapshotsEqual(actualSnapshot, expectedSnapshot) {
				t.Fatalf("recovered state differs from no-fault state\nactual=%+v\nexpected=%+v", actualSnapshot, expectedSnapshot)
			}
			entries, err := os.ReadDir(filepath.Join(data, "txn"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("journal residue: %+v", entries)
			}
			temps, _ := filepath.Glob(filepath.Join(data, ".state-*.tmp"))
			if len(temps) != 0 {
				t.Fatalf("state temp residue: %v", temps)
			}
		})
	}

	for _, point := range []string{"journal_intent_write", "journal_intent_fsync"} {
		t.Run(point+"_fail_closed", func(t *testing.T) {
			cfg := testConfig()
			data := filepath.Join(canonicalTestTempDir(t), "records")
			store := NewStore(data)
			store.Failpoint = func(name string) error {
				if name == point {
					return errors.New("simulated crash")
				}
				return nil
			}
			penny := actorFor(cfg, "penny", "p", "/test")
			_, _ = transactCreateCase(store, cfg, penny, "INCOMPLETE-INTENT-001", point, false)
			_, err := NewStore(data).ReadAll(cfg)
			if err == nil || !strings.Contains(err.Error(), "intent 不完整") {
				t.Fatalf("incomplete intent was not fail-closed: %v", err)
			}
		})
	}
}

func TestTransactionRecoveryRecoveryRejectsTamperedPrefix(t *testing.T) {
	cfg := testConfig()
	data := filepath.Join(canonicalTestTempDir(t), "records")
	store := NewStore(data)
	penny := actorFor(cfg, "penny", "p", "/test")
	if _, err := transactCreateCase(store, cfg, penny, "PREFIX-001", "first", false); err != nil {
		t.Fatal(err)
	}
	fired := false
	store.Failpoint = func(name string) error {
		if name == "log_append_partial" && !fired {
			fired = true
			return errors.New("crash")
		}
		return nil
	}
	_, _ = transactCreateCase(store, cfg, penny, "PREFIX-002", "second", false)
	path := store.EventLogPath(store.NowTime())
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[0] ^= 1
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = NewStore(data).ReadAll(cfg)
	if err == nil || !strings.Contains(err.Error(), "digest 不匹配") {
		t.Fatalf("tampered old prefix was not rejected: %v", err)
	}
}

func TestTransactionRecoveryDryRunUsesTransactionAndWritesNoEventsOrDelivery(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	e.setActor(t, "penny", "dry:p", e.office)
	app := e.app(t)
	app.DryRun = true
	if err := app.run([]string{"case", "create", "--id", "DRY-RUN-001", "--title", "preview", "--project", "test-project", "--source", source}); err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 || len(e.transport.calls) != 0 {
		t.Fatalf("dry-run side effects: events=%d deliveries=%d", len(events), len(e.transport.calls))
	}

	e.setActor(t, "zantianyou", "dry:z", filepath.Join(e.root, "engineering"))
	app = e.app(t)
	app.DryRun = true
	err = app.run([]string{"close", "--case", "DRY-RUN-001", "--reason", "no", "--source", source})
	if err == nil || !strings.Contains(err.Error(), "无权销账") {
		t.Fatalf("unauthorized dry-run did not fail precisely: %v", err)
	}

	e.setActor(t, "penny", "dry:p2", e.office)
	runTestCommand(t, e, "case", "create", "--id", "DRY-STATE-001", "--title", "state", "--source", source)
	runTestCommand(t, e, "close", "--case", "DRY-STATE-001", "--reason", "done", "--source", source)
	order := writeTestFile(t, filepath.Join(e.office, "dry-order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "dry-approved.md", "DEC-DRY-STATE-001", "DRY-STATE-001", order, "zantianyou")
	before, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	callsBefore := len(e.transport.calls)
	app = e.app(t)
	app.DryRun = true
	err = app.run([]string{"issue", "--case", "DRY-STATE-001", "--to", "zantianyou", "--decision", decision, "--next", "invalid"})
	if err == nil || !strings.Contains(err.Error(), "非法状态转移") {
		t.Fatalf("wrong-state dry-run did not fail before delivery: %v", err)
	}
	err = app.run([]string{"case", "create", "--id", "DRY-STATE-001", "--title", "changed", "--project", "test-project", "--source", source})
	if err == nil || !strings.Contains(err.Error(), "request digest 不同") {
		t.Fatalf("duplicate-command dry-run did not fail precisely: %v", err)
	}
	after, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(after) != len(before) || len(e.transport.calls) != callsBefore {
		t.Fatalf("invalid dry-runs wrote side effects: events=%d/%d calls=%d/%d", len(after), len(before), len(e.transport.calls), callsBefore)
	}
}

func TestTransactionRecoveryStrictJSONAndFileDiagnostics(t *testing.T) {
	base := setupTestEnv(t)
	valid := createLegalClosedFlow(t, base, "STRICT-LEDGER-001")
	firstAt, err := time.Parse(time.RFC3339, valid[0].At)
	if err != nil {
		t.Fatal(err)
	}
	monthFile := firstAt.Format("2006-01") + ".jsonl"
	first, _ := json.Marshal(valid[0])
	second, _ := json.Marshal(valid[1])
	addField := func(extra string) []byte {
		raw := append([]byte(nil), first[:len(first)-1]...)
		raw = append(raw, []byte(extra)...)
		return append(raw, '\n')
	}
	withNewline := func(raw []byte) []byte { return append(append([]byte(nil), raw...), '\n') }
	tests := []struct {
		name string
		file string
		raw  []byte
		line int
		want string
	}{
		{name: "unknown field", file: monthFile, raw: addField(`,"unknown":true}`), line: 1, want: "unknown field"},
		{name: "duplicate field", file: monthFile, raw: addField(`,"event_id":"duplicate"}`), line: 1, want: "重复 JSON 字段"},
		{name: "multiple values", file: monthFile, raw: withNewline(append(append([]byte(nil), first...), []byte(` {}`)...)), line: 1, want: "第二个值"},
		{name: "middle bad line", file: monthFile, raw: bytes.Join([][]byte{first, []byte(`{"bad":`), second, nil}, []byte{'\n'}), line: 2, want: "事件损坏"},
		{name: "truncated tail", file: monthFile, raw: first, line: 1, want: "末行截断"},
		{name: "non utf8", file: monthFile, raw: []byte{0xff, '\n'}, line: 1, want: "不是 UTF-8"},
		{name: "illegal monthly filename", file: "events.jsonl", raw: withNewline(first), line: 1, want: "非法月度文件名"},
		{name: "physical order swapped", file: monthFile, raw: bytes.Join([][]byte{second, first, nil}, []byte{'\n'}), line: 2, want: "物理顺序倒退"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := filepath.Join(canonicalTestTempDir(t), "records")
			dir := filepath.Join(data, "events")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, tc.file)
			if err := os.WriteFile(path, tc.raw, 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := NewStore(data).ReadAll(testConfig())
			wantLocation := fmt.Sprintf("%s:%d", path, tc.line)
			if err == nil || !strings.Contains(err.Error(), wantLocation) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want location %q and diagnostic %q, got %v", wantLocation, tc.want, err)
			}
		})
	}
}

func TestTransactionRecoveryInternalLedgerPathTypesFailClosed(t *testing.T) {
	cfg := testConfig()
	for _, name := range []string{"lock_symlink", "events_symlink", "txn_symlink"} {
		t.Run(name, func(t *testing.T) {
			root := canonicalTestTempDir(t)
			data := filepath.Join(root, "records")
			if err := os.MkdirAll(data, 0o755); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "lock_symlink":
				target := filepath.Join(root, "outside.lock")
				if err := os.WriteFile(target, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(data, ".hq.lock")); err != nil {
					t.Fatal(err)
				}
			case "events_symlink", "txn_symlink":
				target := filepath.Join(root, "outside-dir")
				if err := os.MkdirAll(target, 0o755); err != nil {
					t.Fatal(err)
				}
				base := strings.TrimSuffix(name, "_symlink")
				if err := os.Symlink(target, filepath.Join(data, base)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := NewStore(data).ReadAll(cfg); err == nil {
				t.Fatalf("%s was followed instead of rejected", name)
			}
		})
	}
}

func TestTransactionRecoveryEventEnvelopeTamperMatrix(t *testing.T) {
	base := setupTestEnv(t)
	valid := createLegalClosedFlow(t, base, "ENVELOPE-GUARD-001")
	tests := []struct {
		name      string
		target    int
		mutate    func([]Event, int)
		rehash    bool
		wantError string
	}{
		{name: "field without rehash", target: 0, mutate: func(events []Event, i int) { events[i].Title = "tampered" }, wantError: "event_hash 不匹配"},
		{name: "previous hash", target: 1, mutate: func(events []Event, i int) { events[i].PreviousEventHash = strings.Repeat("f", 64) }, wantError: "previous_event_hash 不匹配"},
		{name: "duplicate sequence", target: 1, mutate: func(events []Event, i int) { events[i].Sequence = events[0].Sequence }, rehash: true, wantError: "sequence"},
		{name: "duplicate event id", target: 1, mutate: func(events []Event, i int) { events[i].ID = events[0].ID }, rehash: true, wantError: "event_id 重复"},
		{name: "duplicate command outcome", target: 1, mutate: func(events []Event, i int) { events[i].CommandID = events[0].CommandID }, rehash: true, wantError: "command outcome 重复"},
		{name: "unknown event type", target: 0, mutate: func(events []Event, i int) { events[i].Type = "unknown" }, rehash: true, wantError: "未知事件类型"},
		{name: "unknown state", target: 0, mutate: func(events []Event, i int) { events[i].ToState = "mystery" }, rehash: true, wantError: "未知后态"},
		{name: "missing mandatory actor label", target: 0, mutate: func(events []Event, i int) { events[i].ActorLabel = "" }, rehash: true, wantError: "缺少"},
		{name: "missing type mandatory title", target: 0, mutate: func(events []Event, i int) { events[i].Title = "" }, rehash: true, wantError: "类型必填字段"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			events := append([]Event(nil), valid...)
			tc.mutate(events, tc.target)
			if tc.rehash {
				rehashEventChain(t, events)
			}
			data := filepath.Join(canonicalTestTempDir(t), "records")
			path := writeRawEvents(t, data, events)
			_, err := NewStore(data).ReadAll(testConfig())
			if err == nil || !strings.Contains(err.Error(), tc.wantError) || !strings.Contains(err.Error(), path+":") {
				t.Fatalf("tamper diagnostic mismatch: want=%q path=%s err=%v", tc.wantError, path, err)
			}
		})
	}
}

func createPreparedIssueOrigin(t *testing.T, e testEnv, caseID string) (*App, Event) {
	t.Helper()
	source := writeTestFile(t, filepath.Join(e.office, caseID+"-source.md"), "# source\n")
	e.setActor(t, "penny", "outbox:p", e.office)
	runTestCommand(t, e, "case", "create", "--id", caseID, "--title", "outbox", "--project", "outbox-test", "--source", source)
	app := e.app(t)
	commandID := stableCommandID("prepared-issue-test", caseID)
	deliveryID := stableDeliveryID(commandID, "zantianyou")
	result, err := app.transact(commandID, requestDigest("prepared-issue-test", caseID), func(ledger *ledgerState) (Event, error) {
		state, err := ledger.currentCase(caseID)
		if err != nil {
			return Event{}, err
		}
		event, err := app.newEvent(actorFor(testConfig(), "penny", "outbox:p", e.office), "issue_prepared", caseID)
		if err != nil {
			return Event{}, err
		}
		event.FromState, event.Title = state.Status, state.Title
		event.Recipient, event.RecipientLabel = "zantianyou", "工程部-詹天佑"
		event.CaseVersion, event.CaseDigest = state.Version, state.Digest
		setTestAssignmentContract(&event, state)
		event.AuthorizationType, event.AuthorizationDigest, event.DecisionRef = "standing_decision", digestText("prepared authorization"), "test:/decision"
		event.NextAction = "test"
		event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "issue-fixed"
		event.Delivery, event.DeliveryID = deliveryPrepared, deliveryID
		payload, err := app.deliveryPayload(event)
		if err != nil {
			return Event{}, err
		}
		event.PayloadDigest = digestText(payload)
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return app, result.Event
}

func runIssueWithOutcome(t *testing.T, outcome TransportOutcome, transportErr error, failpoint func(string) error) (testEnv, *App, Event, error) {
	t.Helper()
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
	order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
	decision := writeIssueApproval(t, e, "approved.md", "DEC-OUTBOX-ISSUE-001", "OUTBOX-ISSUE-001", order, "zantianyou")
	e.setActor(t, "penny", "outbox:p", e.office)
	runTestCommand(t, e, "case", "create", "--id", "OUTBOX-ISSUE-001", "--title", "outbox", "--source", source)
	e.transport.result, e.transport.err = outcome, transportErr
	app := e.app(t)
	app.DeliveryFailpoint = failpoint
	err := app.run([]string{"issue", "--case", "OUTBOX-ISSUE-001", "--to", "zantianyou", "--decision", decision, "--next", "work"})
	events, readErr := NewStore(e.data).ReadAll(testConfig())
	if readErr != nil {
		if err == nil {
			err = readErr
		}
		return e, app, Event{}, err
	}
	var origin Event
	for _, event := range events {
		if event.Type == "issue_prepared" {
			origin = event
		}
	}
	return e, app, origin, err
}

func TestTransactionRecoveryOutboxSentIsIdempotentAndPayloadStable(t *testing.T) {
	e, app, origin, err := runIssueWithOutcome(t, transportSent, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if origin.ID == "" || len(e.transport.calls) != 1 {
		t.Fatalf("origin/calls invalid: origin=%+v calls=%d", origin, len(e.transport.calls))
	}
	if !strings.Contains(e.transport.calls[0].message, "DELIVERY="+origin.DeliveryID) {
		t.Fatalf("payload lacks stable delivery id: %s", e.transport.calls[0].message)
	}
	events, readErr := app.Store.ReadAll(app.Config)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, event := range events {
		if (event.Type == "delivery_attempted" || event.Type == "issue_sent") && event.DeliveryID == origin.DeliveryID {
			if event.Actor != origin.Actor || event.ActorLabel != origin.ActorLabel || event.ActorPaneID != origin.ActorPaneID {
				t.Fatalf("lifecycle actor lost origin identity: origin=%+v lifecycle=%+v", origin, event)
			}
		}
	}
	firstPayload := e.transport.calls[0].message
	outcome, err := app.processDelivery(origin, "")
	if err != nil || outcome.DeliveryStatus != deliverySent {
		t.Fatalf("sent retry was not idempotent: outcome=%+v err=%v", outcome, err)
	}
	if len(e.transport.calls) != 1 || e.transport.calls[0].message != firstPayload {
		t.Fatalf("sent retry called transport again")
	}
}

func TestTransactionRecoveryOutboxFailedPreSendExplicitRetrySameDelivery(t *testing.T) {
	e, app, origin, err := runIssueWithOutcome(t, transportDefinitelyNotSent, errors.New("exec never started"), nil)
	if err == nil || origin.ID == "" {
		t.Fatalf("definitely-not-sent should return composite error: origin=%+v err=%v", origin, err)
	}
	view, ok, viewErr := app.Store.Delivery(app.Config, origin.DeliveryID)
	if viewErr != nil || !ok || view.Status != deliveryFailedPreSend {
		t.Fatalf("bad failed_pre_send state: view=%+v ok=%v err=%v", view, ok, viewErr)
	}
	firstRetryCommand := fmt.Sprintf("delivery-retry:%s:%d", origin.DeliveryID, view.AttemptCount+1)
	e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("second exec never started")
	if err := app.run([]string{"delivery", "retry", "--id", origin.DeliveryID}); err == nil {
		t.Fatal("second definitely-not-sent retry should report failed_pre_send")
	}
	view, _, err = app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || view.Status != deliveryFailedPreSend || view.AttemptCount != 2 {
		t.Fatalf("second failed retry outcome=%+v err=%v", view, err)
	}
	callsBeforeIdempotentRetry := len(e.transport.calls)
	if err := app.run([]string{"delivery", "retry", "--id", origin.DeliveryID, "--command", firstRetryCommand}); err == nil {
		t.Fatal("repeating the same failed retry command should return its committed failed outcome")
	}
	view, _, err = app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || view.Status != deliveryFailedPreSend || len(e.transport.calls) != callsBeforeIdempotentRetry {
		t.Fatalf("same retry command became unknown or redelivered: view=%+v calls=%d/%d err=%v", view, len(e.transport.calls), callsBeforeIdempotentRetry, err)
	}
	secondRetryCommand := fmt.Sprintf("delivery-retry:%s:%d", origin.DeliveryID, view.AttemptCount+1)
	e.transport.result, e.transport.err = transportDefinitelyNotSent, errors.New("third exec never started")
	if err := app.run([]string{"delivery", "retry", "--id", origin.DeliveryID}); err == nil {
		t.Fatal("second consecutive explicit retry should preserve failed_pre_send")
	}
	view, _, err = app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || view.Status != deliveryFailedPreSend || view.AttemptCount != 3 {
		t.Fatalf("second consecutive retry outcome=%+v err=%v", view, err)
	}
	callsBeforeIdempotentRetry = len(e.transport.calls)
	if err := app.run([]string{"delivery", "retry", "--id", origin.DeliveryID, "--command", secondRetryCommand}); err == nil {
		t.Fatal("repeating second failed retry command should return its committed failed outcome")
	}
	view, _, err = app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || view.Status != deliveryFailedPreSend || len(e.transport.calls) != callsBeforeIdempotentRetry {
		t.Fatalf("second retry command became unknown or redelivered: view=%+v calls=%d/%d err=%v", view, len(e.transport.calls), callsBeforeIdempotentRetry, err)
	}
	e.transport.result, e.transport.err = transportSent, nil
	if err := app.run([]string{"delivery", "retry", "--id", origin.DeliveryID}); err != nil {
		t.Fatal(err)
	}
	view, _, err = app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || view.Status != deliverySent || view.AttemptCount != 4 {
		t.Fatalf("third explicit retry outcome=%+v err=%v", view, err)
	}
	if len(e.transport.calls) != 4 || e.transport.calls[0].message != e.transport.calls[1].message ||
		e.transport.calls[1].message != e.transport.calls[2].message || e.transport.calls[2].message != e.transport.calls[3].message {
		t.Fatalf("retries did not reuse exact payload: %+v", e.transport.calls)
	}
}

func TestTransactionRecoveryHistoricalLabelsSurviveConfigLabelChanges(t *testing.T) {
	cfg := testConfig()
	data := filepath.Join(canonicalTestTempDir(t), "records")
	prepareDeliveredIssue(t, NewStore(data), cfg, "HISTORICAL-LABEL-001")
	updated := cfg
	updated.Agents = append([]AgentRule(nil), cfg.Agents...)
	for i := range updated.Agents {
		if updated.Agents[i].Name == "penny" {
			updated.Agents[i].Label = "Penny新花名"
		}
		if updated.Agents[i].Name == "zantianyou" {
			updated.Agents[i].Label = "工程部-詹天佑新花名"
		}
	}
	events, err := NewStore(data).ReadAll(updated)
	if err != nil {
		t.Fatalf("合法 label 更新使历史账本失效：%v", err)
	}
	if len(events) != 4 || events[0].ActorLabel != "Penny通报" || events[1].RecipientLabel != "工程部-詹天佑" {
		t.Fatalf("历史标签没有保持事件内证据：%+v", events)
	}
}

func TestTransactionRecoveryOutboxAmbiguousAndCrashWindowsNeverAutoRedeliver(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		e, app, origin, err := runIssueWithOutcome(t, transportAmbiguous, errors.New("timeout"), nil)
		if err == nil {
			t.Fatal("ambiguous transport should return composite error")
		}
		before := len(e.transport.calls)
		if err := app.reconcileDeliveries(); err != nil {
			t.Fatal(err)
		}
		view, _, err := app.Store.Delivery(app.Config, origin.DeliveryID)
		if err != nil || view.Status != deliveryUnknown || len(e.transport.calls) != before {
			t.Fatalf("ambiguous reconcile redelivered: view=%+v calls=%d err=%v", view, len(e.transport.calls), err)
		}
	})

	for _, point := range []string{"after_attempt_recorded", "after_transport_sent"} {
		t.Run(point, func(t *testing.T) {
			e, _, origin, err := runIssueWithOutcome(t, transportSent, nil, func(name string) error {
				if name == point {
					return errors.New("simulated process crash")
				}
				return nil
			})
			if err == nil {
				t.Fatal("crash hook should interrupt delivery")
			}
			callsBefore := len(e.transport.calls)
			restarted := e.app(t)
			if err := restarted.reconcileDeliveries(); err == nil {
				t.Fatal("attempted reconcile should surface unknown")
			}
			view, _, viewErr := restarted.Store.Delivery(restarted.Config, origin.DeliveryID)
			if viewErr != nil || view.Status != deliveryUnknown || len(e.transport.calls) != callsBefore {
				t.Fatalf("crash reconcile redelivered: view=%+v calls=%d/%d err=%v", view, len(e.transport.calls), callsBefore, viewErr)
			}
			if point == "after_attempt_recorded" && callsBefore != 0 {
				t.Fatalf("transport called before crash: %d", callsBefore)
			}
			if point == "after_transport_sent" && callsBefore != 1 {
				t.Fatalf("transport call count=%d, want 1", callsBefore)
			}
		})
	}
}

func TestTransactionRecoveryPreparedReconcileCallsTransportAtMostOnce(t *testing.T) {
	e := setupTestEnv(t)
	app, origin := createPreparedIssueOrigin(t, e, "PREPARED-RECONCILE-001")
	if err := app.reconcileDeliveries(); err != nil {
		t.Fatal(err)
	}
	if err := app.reconcileDeliveries(); err != nil {
		t.Fatal(err)
	}
	view, _, err := app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || view.Status != deliverySent || len(e.transport.calls) != 1 {
		t.Fatalf("prepared reconcile outcome=%+v calls=%d err=%v", view, len(e.transport.calls), err)
	}
}

func TestTransactionRecoveryOutboxRejectsChangedTargetOrPayload(t *testing.T) {
	e := setupTestEnv(t)
	app, origin := createPreparedIssueOrigin(t, e, "DIGEST-GUARD-001")
	changed := origin
	changed.Recipient, changed.RecipientLabel = "baogong", "质量与用户体验部-包公"
	if _, err := app.processDelivery(changed, ""); err == nil {
		t.Fatal("changed target was accepted")
	}
	changed = origin
	changed.NextAction = "changed"
	if _, err := app.processDelivery(changed, ""); err == nil {
		t.Fatal("changed payload was accepted")
	}
	if len(e.transport.calls) != 0 {
		t.Fatalf("digest mismatch reached transport: %d", len(e.transport.calls))
	}
}

func TestTransactionRecoveryTerminalAppendFailureIsCompositeAndNotSwallowed(t *testing.T) {
	tests := []struct {
		name         string
		outcome      TransportOutcome
		transportErr error
		want         string
	}{
		{name: "sent terminal", outcome: transportSent, want: "终态落账失败"},
		{name: "failed_pre_send terminal", outcome: transportDefinitelyNotSent, transportErr: errors.New("exec never started"), want: "终态落账"},
		{name: "unknown terminal", outcome: transportAmbiguous, transportErr: errors.New("timeout"), want: "unknown 落账失败"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := setupTestEnv(t)
			source := writeTestFile(t, filepath.Join(e.office, "source.md"), "# source\n")
			order := writeTestFile(t, filepath.Join(e.office, "order.md"), "# order\n")
			decision := writeIssueApproval(t, e, "approved.md", "DEC-TERMINAL-FAIL-001", "TERMINAL-FAIL-001", order, "zantianyou")
			e.setActor(t, "penny", "terminal:p", e.office)
			runTestCommand(t, e, "case", "create", "--id", "TERMINAL-FAIL-001", "--title", "terminal", "--source", source)
			app := e.app(t)
			store := app.Store.(*Store)
			fired := false
			e.transport.result, e.transport.err = tc.outcome, tc.transportErr
			e.transport.hook = func() {
				store.Failpoint = func(name string) error {
					if name == "journal_intent_write" && !fired {
						fired = true
						return errors.New("terminal journal unavailable")
					}
					return nil
				}
			}
			err := app.run([]string{"issue", "--case", "TERMINAL-FAIL-001", "--to", "zantianyou", "--decision", decision, "--next", "work"})
			if err == nil || !strings.Contains(err.Error(), "投递请求已记账，case 业务状态尚未推进") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("terminal append failure was swallowed or ambiguous: %v", err)
			}
			if len(e.transport.calls) != 1 {
				t.Fatalf("transport calls=%d, want 1", len(e.transport.calls))
			}
		})
	}
}

func TestTransactionRecoveryUnknownManualResolutionRequiresOperationsWhitelist(t *testing.T) {
	e, app, origin, err := runIssueWithOutcome(t, transportAmbiguous, errors.New("timeout"), nil)
	if err == nil {
		t.Fatal("ambiguous issue should fail with committed outcome")
	}
	evidence := writeTestFile(t, filepath.Join(e.office, "delivery-evidence.md"), "# checked\n")

	e.setActor(t, "zantianyou", "resolve:z", filepath.Join(e.root, "engineering"))
	workerApp := e.app(t)
	err = workerApp.run([]string{"delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "not-delivered", "--reason", "checked", "--evidence", evidence})
	if err == nil || !strings.Contains(err.Error(), "运维白名单") {
		t.Fatalf("non-operations actor resolved unknown: %v", err)
	}

	e.setActor(t, "penny", "resolve:p", e.office)
	app = e.app(t)
	if err := app.run([]string{"delivery", "resolve", "--id", origin.DeliveryID, "--outcome", "not-delivered", "--reason", "receiver confirmed absent", "--evidence", evidence}); err != nil {
		t.Fatal(err)
	}
	view, _, err := app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || view.Status != deliveryFailedPreSend {
		t.Fatalf("manual not-delivered resolution=%+v err=%v", view, err)
	}
}

func TestTransactionRecoveryCompositeOutcomeDistinguishesPreparedFromAppliedBusinessState(t *testing.T) {
	t.Run("issue_prepared_not_applied", func(t *testing.T) {
		_, _, _, err := runIssueWithOutcome(t, transportAmbiguous, errors.New("timeout"), nil)
		var outcomeErr *DeliveryOutcomeError
		if !errors.As(err, &outcomeErr) {
			t.Fatalf("want DeliveryOutcomeError, got %v", err)
		}
		if !outcomeErr.Outcome.CommandCommitted || outcomeErr.Outcome.CaseStateApplied ||
			outcomeErr.Outcome.BusinessOutcome != "delivery_prepared_committed" ||
			!strings.Contains(err.Error(), "case 业务状态尚未推进") {
			t.Fatalf("prepared outcome is misleading: %+v / %v", outcomeErr.Outcome, err)
		}
	})

	t.Run("report_prepared_not_applied", func(t *testing.T) {
		e := setupTestEnv(t)
		source := writeTestFile(t, filepath.Join(e.office, "report-source.md"), "# source\n")
		artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "report-artifact.md"), "# result\n")
		e.setActor(t, "zantianyou", "report:z", filepath.Join(e.root, "engineering"))
		runTestCommand(t, e, "case", "create", "--id", "REPORT-OUTCOME-001", "--title", "report", "--source", source)
		e.transport.result, e.transport.err = transportAmbiguous, errors.New("report timeout")
		app := e.app(t)
		err := app.run([]string{"report", "--case", "REPORT-OUTCOME-001", "--result", "completed", "--artifact", artifact, "--next", "manager verify"})
		var outcomeErr *DeliveryOutcomeError
		if !errors.As(err, &outcomeErr) || !outcomeErr.Outcome.CommandCommitted || outcomeErr.Outcome.CaseStateApplied ||
			outcomeErr.Outcome.BusinessOutcome != "delivery_prepared_committed" ||
			!strings.Contains(err.Error(), "case 业务状态尚未推进") {
			t.Fatalf("report prepared outcome is misleading: %+v / %v", outcomeErr, err)
		}
		snapshot, snapshotErr := app.Store.Snapshot(app.Config)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if state := snapshot.Cases["REPORT-OUTCOME-001"]; state == nil || state.Status != string(statusOpen) {
			t.Fatalf("report prepared unexpectedly advanced business state: %+v", state)
		}
	})

	t.Run("accept_applied_notice_unknown", func(t *testing.T) {
		e, _, origin, err := runIssueWithOutcome(t, transportSent, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		e.setActor(t, "zantianyou", "accept:z", filepath.Join(e.root, "engineering"))
		e.transport.result, e.transport.err = transportAmbiguous, errors.New("notice timeout")
		app := e.app(t)
		err = app.run([]string{"accept", "--event", origin.ID, "--next", "work"})
		var outcomeErr *DeliveryOutcomeError
		if !errors.As(err, &outcomeErr) {
			t.Fatalf("want DeliveryOutcomeError, got %v", err)
		}
		if !outcomeErr.Outcome.CommandCommitted || !outcomeErr.Outcome.CaseStateApplied ||
			outcomeErr.Outcome.BusinessOutcome != "case_state_committed" ||
			!strings.Contains(err.Error(), "case 业务状态已提交") {
			t.Fatalf("accept outcome lost committed business state: %+v / %v", outcomeErr.Outcome, err)
		}
		snapshot, snapshotErr := app.Store.Snapshot(app.Config)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if state := snapshot.Cases[origin.CaseID]; state == nil || state.Status != string(statusInProgress) {
			t.Fatalf("accept business state was rolled back: %+v", state)
		}
	})

	t.Run("return_applied_notice_unknown", func(t *testing.T) {
		e, _, origin, err := runIssueWithOutcome(t, transportSent, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		e.setActor(t, "zantianyou", "return:z", filepath.Join(e.root, "engineering"))
		e.transport.result, e.transport.err = transportAmbiguous, errors.New("return notice timeout")
		app := e.app(t)
		err = app.run([]string{"return", "--event", origin.ID, "--reason", "missing evidence", "--next", "resubmit"})
		var outcomeErr *DeliveryOutcomeError
		if !errors.As(err, &outcomeErr) || !outcomeErr.Outcome.CommandCommitted || !outcomeErr.Outcome.CaseStateApplied ||
			outcomeErr.Outcome.BusinessOutcome != "case_state_committed" ||
			!strings.Contains(err.Error(), "case 业务状态已提交") {
			t.Fatalf("return outcome lost committed business state: %+v / %v", outcomeErr, err)
		}
		snapshot, snapshotErr := app.Store.Snapshot(app.Config)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if state := snapshot.Cases[origin.CaseID]; state == nil || state.Status != string(statusReturned) {
			t.Fatalf("return business state was rolled back: %+v", state)
		}
	})
}
