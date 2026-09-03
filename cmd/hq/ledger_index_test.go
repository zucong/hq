package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type sqliteRunnerFunc func(string, string) ([]byte, error)

func (f sqliteRunnerFunc) Run(path, script string) ([]byte, error) { return f(path, script) }

func setupClosedIndexedCase(t *testing.T, caseID string) (testEnv, []Event) {
	t.Helper()
	e := setupTestEnv(t)
	events := createLegalClosedFlow(t, e, caseID)
	return e, events
}

func TestLedgerIndexCompleteFlowAndDeliveryAckProjection(t *testing.T) {
	e, events := setupClosedIndexedCase(t, "LEDGER-FLOW-001")
	store := NewStore(e.data)
	views, err := store.Deliveries(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	byOrigin := map[string]DeliveryView{}
	for _, view := range views {
		byOrigin[view.OriginType] = view
	}
	for _, originType := range []string{"issue_prepared", "report_prepared"} {
		view := byOrigin[originType]
		if view.ProjectionStatus != "acked" || view.Status != deliverySent || view.AckedBy == "" || view.AckEventID == "" {
			t.Fatalf("%s delivery is not a business ack projection: %+v", originType, view)
		}
		origin, _ := findEvent(events, view.OriginEventID)
		if view.AckedBy != origin.Recipient {
			t.Fatalf("acked_by=%s origin.recipient=%s", view.AckedBy, origin.Recipient)
		}
	}
	for _, noticeType := range []string{"event_accepted"} {
		view := byOrigin[noticeType]
		if view.ProjectionStatus != "sent" || view.AckEventID != "" || view.AckedBy != "" {
			t.Fatalf("accept notice delivery was confused with the delivery it acknowledges: %+v", view)
		}
	}

	var out bytes.Buffer
	app := e.app(t)
	app.JSON, app.Out = true, &out
	if err := app.run([]string{"flow", "show", "--case", "LEDGER-FLOW-001"}); err != nil {
		t.Fatal(err)
	}
	var flow CaseFlowView
	if err := json.Unmarshal(out.Bytes(), &flow); err != nil {
		t.Fatal(err)
	}
	caseEventCount := 0
	for _, event := range events {
		if event.CaseID == "LEDGER-FLOW-001" {
			caseEventCount++
		}
	}
	if flow.CaseID != "LEDGER-FLOW-001" || len(flow.Events) != caseEventCount || len(flow.Deliveries) != len(views) {
		t.Fatalf("incomplete flow view: %+v", flow)
	}
	previous := int64(0)
	seen := map[string]bool{}
	for _, event := range flow.Events {
		sequence, err := parsePositiveInt64(event.Sequence)
		if err != nil || sequence <= previous || event.Actor == "" || event.At == "" {
			t.Fatalf("invalid ordered flow event: %+v err=%v", event, err)
		}
		previous = sequence
		seen[event.Type] = true
	}
	for _, eventType := range []string{"case_created", "issue_prepared", "issue_sent", "event_accepted", "report_prepared", "report_sent", "case_closed"} {
		if !seen[eventType] {
			t.Fatalf("flow view missing %s", eventType)
		}
	}
}

func parsePositiveInt64(value string) (int64, error) {
	var parsed int64
	_, err := fmt.Sscan(value, &parsed)
	if err != nil || parsed <= 0 {
		return parsed, fmt.Errorf("not positive int64: %s", value)
	}
	return parsed, nil
}

func TestLedgerIndexAckRejectsWrongActorWrongReferenceAndUnsent(t *testing.T) {
	cfg := testConfig()
	t.Run("wrong actor", func(t *testing.T) {
		store := NewStore(filepath.Join(canonicalTestTempDir(t), "records"))
		sent := prepareDeliveredIssue(t, store, cfg, "LEDGER-ACK-001")
		wrong := actorFor(cfg, "baogong", "qa-pane", "/qa")
		_, err := store.Transact(cfg, stableCommandID("bad-ack-actor", sent.ID), requestDigest("bad-ack-actor", sent.ID), false, func(ledger *ledgerState) (Event, error) {
			event := testLedgerEvent(t, store, wrong, "event_accepted", sent.CaseID)
			event.RelatedEventID, event.FromState, event.ToState = sent.ID, ledger.snapshot.Cases[sent.CaseID].Status, string(statusInProgress)
			event.Owner, event.NextAction = wrong.Name, "work"
			event.Recipient, event.RecipientLabel = sent.Actor, sent.ActorLabel
			event.Delivery, event.DeliveryID, event.PayloadDigest = deliveryPrepared, stableDeliveryID("bad-ack", sent.Actor), digestText("notice")
			return event, nil
		})
		if err == nil || !strings.Contains(err.Error(), "actor/recipient") {
			t.Fatalf("wrong actor ack accepted: %v", err)
		}
	})
	t.Run("wrong related event and unsent origin", func(t *testing.T) {
		store := NewStore(filepath.Join(canonicalTestTempDir(t), "records"))
		penny := actorFor(cfg, "penny", "p", "/office")
		created, err := transactCreateCase(store, cfg, penny, "LEDGER-ACK-002", "setup", false)
		if err != nil {
			t.Fatal(err)
		}
		worker := actorFor(cfg, "zantianyou", "z", "/engineering")
		_, err = store.Transact(cfg, stableCommandID("bad-ack-related", created.Event.ID), requestDigest("bad-ack-related", created.Event.ID), false, func(ledger *ledgerState) (Event, error) {
			event := testLedgerEvent(t, store, worker, "event_accepted", created.Event.CaseID)
			event.RelatedEventID, event.FromState, event.ToState = created.Event.ID, ledger.snapshot.Cases[created.Event.CaseID].Status, string(statusInProgress)
			event.Owner, event.NextAction = worker.Name, "work"
			event.Recipient, event.RecipientLabel = penny.Name, penny.Label
			event.Delivery, event.DeliveryID, event.PayloadDigest = deliveryPrepared, stableDeliveryID("bad-related", penny.Name), digestText("notice")
			return event, nil
		})
		if err == nil || !strings.Contains(err.Error(), "已送达") {
			t.Fatalf("wrong/unsent related event ack accepted: %v", err)
		}
	})
}

func TestLedgerIndexDeliveryProjectionMatrix(t *testing.T) {
	cfg := testConfig()
	store := NewStore(filepath.Join(canonicalTestTempDir(t), "records"))
	penny := actorFor(cfg, "penny", "p", "/office")
	if _, err := transactCreateCase(store, cfg, penny, "LEDGER-MATRIX-001", "setup", false); err != nil {
		t.Fatal(err)
	}
	commandID := stableCommandID("matrix-origin", "LEDGER-MATRIX-001")
	deliveryID := stableDeliveryID(commandID, "zantianyou")
	prepared, err := store.Transact(cfg, commandID, requestDigest("matrix-origin"), false, func(ledger *ledgerState) (Event, error) {
		event := testLedgerEvent(t, store, penny, "issue_prepared", "LEDGER-MATRIX-001")
		state := ledger.snapshot.Cases[event.CaseID]
		event.FromState, event.Title = state.Status, "matrix"
		event.Recipient, event.RecipientLabel = "zantianyou", "工程部-詹天佑"
		event.CaseVersion, event.CaseDigest = state.Version, state.Digest
		setTestAssignmentContract(&event, state)
		event.AuthorizationType, event.AuthorizationDigest, event.DecisionRef = "standing_decision", digestText("matrix authorization"), "test:/decision"
		event.NextAction = "work"
		event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "issue-fixed"
		event.Delivery, event.DeliveryID, event.PayloadDigest = deliveryPrepared, deliveryID, digestText("payload")
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProjection := func(want string) {
		t.Helper()
		view, ok, err := store.Delivery(cfg, deliveryID)
		if err != nil || !ok || view.ProjectionStatus != want {
			t.Fatalf("projection=%+v ok=%t err=%v want=%s", view, ok, err, want)
		}
	}
	assertProjection("pending")
	attempt, err := store.Transact(cfg, stableCommandID("matrix-attempt", deliveryID), requestDigest("matrix-attempt"), false, func(*ledgerState) (Event, error) {
		event := testLedgerEvent(t, store, penny, "delivery_attempted", prepared.Event.CaseID)
		event.RelatedEventID, event.Recipient, event.RecipientLabel = prepared.Event.ID, prepared.Event.Recipient, prepared.Event.RecipientLabel
		event.Delivery, event.DeliveryID, event.PayloadDigest = deliveryAttempted, deliveryID, prepared.Event.PayloadDigest
		setTestEmptyTurnBundleManifest(&event, prepared.Event, cfg, "payload")
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProjection("pending")
	_, err = store.Transact(cfg, stableCommandID("matrix-failed", deliveryID), requestDigest("matrix-failed"), false, func(*ledgerState) (Event, error) {
		event := testLedgerEvent(t, store, penny, "delivery_failed_pre_send", prepared.Event.CaseID)
		event.RelatedEventID, event.AttemptEventID = prepared.Event.ID, attempt.Event.ID
		event.Recipient, event.RecipientLabel = prepared.Event.Recipient, prepared.Event.RecipientLabel
		event.Delivery, event.DeliveryID, event.PayloadDigest = deliveryFailedPreSend, deliveryID, prepared.Event.PayloadDigest
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertProjection("failed")
}

func TestLedgerIndexPayloadFieldsUseSeparatedHardLimits(t *testing.T) {
	validBusiness := strings.Repeat("x", maxBusinessTextBytes)
	invalidBusiness := validBusiness + "x"
	for _, field := range []string{"verify", "next", "note", "reason"} {
		if _, err := validateBusinessText(field, validBusiness, true); err != nil {
			t.Fatalf("%s rejected 2 KiB business text: %v", field, err)
		}
		if _, err := validateBusinessText(field, invalidBusiness, true); err == nil || !strings.Contains(err.Error(), "2 KiB") {
			t.Fatalf("%s accepted 2049-byte business text: %v", field, err)
		}
	}
	validCJK := strings.Repeat("界", 682) // 2046 UTF-8 bytes.
	if _, err := validateBusinessText("next", validCJK, true); err != nil {
		t.Fatalf("CJK business text below 2 KiB was rejected: %v", err)
	}
	if _, err := validateBusinessText("next", validCJK+"界", true); err == nil {
		t.Fatal("2049-byte CJK business text was accepted")
	}
	if err := validateEventShortFields(Event{Verification: validBusiness, NextAction: validBusiness, Note: validBusiness}); err != nil {
		t.Fatalf("replay rejected 2 KiB business fields: %v", err)
	}
	if err := validateEventShortFields(Event{NextAction: invalidBusiness}); err == nil {
		t.Fatal("replay accepted a 2049-byte business field")
	}

	validStructural := strings.Repeat("界", 200)
	invalidStructural := validStructural + "界"
	for _, field := range []string{"title", "project", "result", "location", "label"} {
		if _, err := validateShortText(field, validStructural, true); err != nil {
			t.Fatalf("%s rejected 200-rune structural text: %v", field, err)
		}
		if _, err := validateShortText(field, invalidStructural, true); err == nil {
			t.Fatalf("%s accepted 201-rune structural text", field)
		}
	}
	if err := validateEventShortFields(Event{Title: invalidStructural}); err == nil {
		t.Fatal("replay accepted a 201-rune structural event field")
	}
}

func writeEventLedger(t *testing.T, store *Store, events []Event) {
	t.Helper()
	byPath := map[string][]byte{}
	for _, event := range events {
		at, err := time.Parse(time.RFC3339, event.At)
		if err != nil {
			t.Fatal(err)
		}
		path := store.EventLogPath(at)
		raw, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		byPath[path] = append(byPath[path], append(raw, '\n')...)
	}
	for path, raw := range byPath {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func rehashIndexedEventChain(t *testing.T, events []Event) {
	t.Helper()
	previous := genesisEventHash
	for i := range events {
		events[i].PreviousEventHash = previous
		events[i].EventHash = ""
		hash, err := hashEvent(events[i])
		if err != nil {
			t.Fatal(err)
		}
		events[i].EventHash = hash
		previous = hash
	}
}

func TestLedgerIndexSequenceInt64GapsOverflowAndJSSafety(t *testing.T) {
	if reflect.TypeOf(Event{}.Sequence).Kind() != reflect.Int64 || reflect.TypeOf(Snapshot{}.LastSequence).Kind() != reflect.Int64 {
		t.Fatal("event/snapshot sequence is not Go int64")
	}
	if int64(36_525_000) >= math.MaxInt64 {
		t.Fatal("100-year capacity baseline does not fit int64")
	}
	cfg := testConfig()
	t.Run("gap replay and next allocation", func(t *testing.T) {
		store := NewStore(filepath.Join(canonicalTestTempDir(t), "records"))
		actor := actorFor(cfg, "penny", "p", "/office")
		if _, err := transactCreateCase(store, cfg, actor, "LEDGER-SEQ-001", "one", false); err != nil {
			t.Fatal(err)
		}
		if _, err := transactCreateCase(store, cfg, actor, "LEDGER-SEQ-002", "two", false); err != nil {
			t.Fatal(err)
		}
		events, err := store.ReadAll(cfg)
		if err != nil {
			t.Fatal(err)
		}
		events[1].Sequence = 3
		rehashIndexedEventChain(t, events)
		writeEventLedger(t, store, events)
		if replayed, err := store.ReadAll(cfg); err != nil || replayed[1].Sequence != 3 {
			t.Fatalf("gap replay failed: %v %+v", err, replayed)
		}
		third, err := transactCreateCase(store, cfg, actor, "LEDGER-SEQ-003", "three", false)
		if err != nil || third.Event.Sequence != 4 {
			t.Fatalf("next sequence=%d err=%v, want 4", third.Event.Sequence, err)
		}
	})
	t.Run("zero negative duplicate and descending rejected", func(t *testing.T) {
		baseStore := NewStore(filepath.Join(canonicalTestTempDir(t), "records"))
		actor := actorFor(cfg, "penny", "p", "/office")
		if _, err := transactCreateCase(baseStore, cfg, actor, "LEDGER-BADSEQ-001", "one", false); err != nil {
			t.Fatal(err)
		}
		if _, err := transactCreateCase(baseStore, cfg, actor, "LEDGER-BADSEQ-002", "two", false); err != nil {
			t.Fatal(err)
		}
		base, err := baseStore.ReadAll(cfg)
		if err != nil {
			t.Fatal(err)
		}
		cases := map[string]func([]Event){
			"zero":       func(events []Event) { events[0].Sequence = 0 },
			"negative":   func(events []Event) { events[0].Sequence = -1 },
			"duplicate":  func(events []Event) { events[1].Sequence = events[0].Sequence },
			"descending": func(events []Event) { events[0].Sequence, events[1].Sequence = 3, 2 },
		}
		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				events := append([]Event(nil), base...)
				mutate(events)
				rehashIndexedEventChain(t, events)
				if _, err := validateLedger(events, cfg); err == nil || !strings.Contains(err.Error(), "sequence") {
					t.Fatalf("invalid sequence accepted: %v", err)
				}
			})
		}
	})
	t.Run("MaxInt64 fails before builder or protocol writes", func(t *testing.T) {
		store := NewStore(filepath.Join(canonicalTestTempDir(t), "records"))
		actor := actorFor(cfg, "penny", "p", "/office")
		first := Event{Version: eventSchemaVersion, Sequence: 1, ID: "EVT-ONE", CommandID: "cmd-one", CommandDigest: digestText("one"), PreviousEventHash: genesisEventHash,
			CaseID: "LEDGER-MAX-001", At: "2026-08-28T00:00:00Z", Type: "case_created", Actor: actor.Name, ActorLabel: actor.Label,
			ToState: string(statusOpen), Owner: actor.Name, NextAction: "continue"}
		setTestCaseSpec(&first, "one", "test:/source")
		first.EventHash, _ = hashEvent(first)
		event := Event{Version: eventSchemaVersion, Sequence: math.MaxInt64, ID: "EVT-MAX", CommandID: "cmd-max", CommandDigest: digestText("max"), PreviousEventHash: first.EventHash,
			CaseID: "LEDGER-MAX-002", At: "2026-08-28T00:00:01Z", Type: "case_created", Actor: actor.Name, ActorLabel: actor.Label,
			ToState: string(statusOpen), Owner: actor.Name, NextAction: "stop"}
		setTestCaseSpec(&event, "max", "test:/source")
		event.ParentCaseID, event.RootCaseID, event.Project = first.CaseID, first.CaseID, first.Project
		event.CaseDigest = eventCaseSpecDigest(event)
		event.EventHash, _ = hashEvent(event)
		writeEventLedger(t, store, []Event{first, event})
		before, _ := os.ReadFile(store.EventLogPath(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)))
		called := false
		_, err := store.Transact(cfg, "cmd-after-max", digestText("after-max"), false, func(*ledgerState) (Event, error) { called = true; return Event{}, nil })
		if err == nil || !strings.Contains(err.Error(), "MaxInt64") || called {
			t.Fatalf("overflow guard failed: err=%v called=%t", err, called)
		}
		after, _ := os.ReadFile(store.EventLogPath(time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)))
		if !bytes.Equal(before, after) {
			t.Fatal("overflow attempt changed event ledger")
		}
		for _, path := range []string{filepath.Join(store.Dir, "state.json"), filepath.Join(store.Dir, "txn")} {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("overflow attempt wrote protocol path %s", path)
			}
		}
	})
	t.Run("JSON output preserves unsafe sequence", func(t *testing.T) {
		value, err := makeJSONSafeForJavaScript(Event{Sequence: maxJavaScriptSafeInteger + 1})
		if err != nil {
			t.Fatal(err)
		}
		object := value.(map[string]any)
		if got, ok := object["sequence"].(string); !ok || got != "9007199254740992" {
			t.Fatalf("unsafe sequence output=%T(%v)", object["sequence"], object["sequence"])
		}
	})
}

func newTestIndex(t *testing.T, e testEnv) *DerivedIndex {
	t.Helper()
	path := filepath.Join(e.office, "tools", "ledger-index-index.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	return &DerivedIndex{Path: path, Runner: execSQLiteRunner{Path: "/usr/bin/sqlite3"}}
}

func TestLedgerIndexSQLiteRebuildQueryAndNoBodyCopy(t *testing.T) {
	e, events := setupClosedIndexedCase(t, "LEDGER-INDEX-001")
	reportBody := "UNIQUE-LONG-REPORT-BODY-MUST-NOT-ENTER-SQLITE"
	report := writeTestFile(t, filepath.Join(e.root, "qa-ux", "reports", "evidence.md"), "# report\n"+reportBody+"\n")
	_ = report
	index := newTestIndex(t, e)
	result, err := index.Rebuild(NewStore(e.data), testConfig(), e.root)
	if err != nil {
		t.Fatal(err)
	}
	if result.FlowEvents != len(events) || result.Cases != 1 || result.Deliveries != 4 || result.Documents < 5 {
		t.Fatalf("bad rebuild result: %+v", result)
	}
	rows, err := index.Query(IndexQuery{Entity: "flow_events", CaseID: "LEDGER-INDEX-001", Actor: "penny"})
	if err != nil || len(rows) == 0 {
		t.Fatalf("structured query failed: rows=%v err=%v", rows, err)
	}
	deliveryRows, err := index.Query(IndexQuery{Entity: "deliveries", CaseID: "LEDGER-INDEX-001", Status: "acked"})
	if err != nil || len(deliveryRows) != 2 {
		t.Fatalf("acked delivery query=%v err=%v", deliveryRows, err)
	}
	documentRows, err := index.Query(IndexQuery{Entity: "documents", Path: report})
	if err != nil || len(documentRows) != 1 {
		t.Fatalf("document metadata query=%v err=%v", documentRows, err)
	}
	raw, err := os.ReadFile(index.Path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(reportBody)) {
		t.Fatal("Markdown body was copied into SQLite")
	}
	firstRows, _ := index.Query(IndexQuery{Entity: "flow_events", CaseID: "LEDGER-INDEX-001"})
	if err := os.Remove(index.Path); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Rebuild(NewStore(e.data), testConfig(), e.root); err != nil {
		t.Fatal(err)
	}
	secondRows, _ := index.Query(IndexQuery{Entity: "flow_events", CaseID: "LEDGER-INDEX-001"})
	if !reflect.DeepEqual(firstRows, secondRows) {
		t.Fatal("delete/rebuild results differ")
	}
	if _, err := index.Rebuild(NewStore(e.data), testConfig(), e.root); err != nil {
		t.Fatal(err)
	}
	thirdRows, _ := index.Query(IndexQuery{Entity: "flow_events", CaseID: "LEDGER-INDEX-001"})
	if !reflect.DeepEqual(secondRows, thirdRows) {
		t.Fatal("consecutive rebuild results differ")
	}
	if _, err := structuredIndexSQL(IndexQuery{Entity: "flow_events; DROP TABLE cases"}); err == nil {
		t.Fatal("arbitrary SQL entity accepted")
	}
	if _, err := (execSQLiteRunner{Path: "sqlite3"}).Run(index.Path, "SELECT 1;"); err == nil {
		t.Fatal("PATH fallback sqlite runner accepted")
	}
}

func TestLedgerIndexSQLiteFailuresKeepOldIndexAndCleanTemps(t *testing.T) {
	e, _ := setupClosedIndexedCase(t, "LEDGER-INDEX-FAIL-001")
	index := newTestIndex(t, e)
	store := NewStore(e.data)
	if _, err := index.Rebuild(store, testConfig(), e.root); err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(index.Path)
	if err != nil {
		t.Fatal(err)
	}
	realRunner := index.Runner
	assertOld := func(label string) {
		t.Helper()
		current, err := os.ReadFile(index.Path)
		if err != nil || !bytes.Equal(current, old) {
			t.Fatalf("%s did not preserve old index: err=%v", label, err)
		}
		matches, _ := filepath.Glob(filepath.Join(filepath.Dir(index.Path), ".hq-index-*.tmp"))
		restores, _ := filepath.Glob(filepath.Join(filepath.Dir(index.Path), ".hq-index-restore-*.tmp"))
		if len(matches)+len(restores) != 0 {
			t.Fatalf("%s left temp files: %v %v", label, matches, restores)
		}
	}
	index.Runner = sqliteRunnerFunc(func(string, string) ([]byte, error) { return nil, errors.New("runner failed") })
	if _, err := index.Rebuild(store, testConfig(), e.root); err == nil {
		t.Fatal("runner failure accepted")
	}
	assertOld("runner")
	index.Runner = realRunner
	for _, point := range []string{"before_rename", "after_rename", "after_parent_fsync"} {
		index.Failpoint = func(name string) error {
			if name == point {
				return errors.New("stop")
			}
			return nil
		}
		if _, err := index.Rebuild(store, testConfig(), e.root); err == nil {
			t.Fatalf("%s failpoint accepted", point)
		}
		assertOld(point)
	}
	index.Failpoint = nil

	// A missing canonical reference fails before sqlite construction and keeps
	// the last good index authoritative only as a disposable old view.
	events, err := store.ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	var source string
	for _, event := range events {
		if event.Type == "case_created" {
			source = event.SourceRef
			break
		}
	}
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Rebuild(store, testConfig(), e.root); err == nil {
		t.Fatal("missing event reference accepted")
	}
	assertOld("missing-reference")
}

func TestLedgerIndexSQLiteBadLedgerAndConcurrentRebuildFailClosed(t *testing.T) {
	e, _ := setupClosedIndexedCase(t, "LEDGER-INDEX-CONCURRENT-001")
	index := newTestIndex(t, e)
	store := NewStore(e.data)
	if _, err := index.Rebuild(store, testConfig(), e.root); err != nil {
		t.Fatal(err)
	}
	old, _ := os.ReadFile(index.Path)

	started := make(chan struct{})
	release := make(chan struct{})
	real := index.Runner
	index.Runner = sqliteRunnerFunc(func(path, script string) ([]byte, error) {
		close(started)
		<-release
		return real.Run(path, script)
	})
	var wg sync.WaitGroup
	wg.Add(1)
	var firstErr error
	go func() { defer wg.Done(); _, firstErr = index.Rebuild(store, testConfig(), e.root) }()
	<-started
	if _, err := index.Rebuild(store, testConfig(), e.root); err == nil || !strings.Contains(err.Error(), "并发") {
		t.Fatalf("concurrent rebuild did not fail closed: %v", err)
	}
	close(release)
	wg.Wait()
	if firstErr != nil {
		t.Fatal(firstErr)
	}
	index.Runner = real

	entries, err := os.ReadDir(filepath.Join(e.data, "events"))
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	path := filepath.Join(e.data, "events", entries[0].Name())
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.WriteString("{bad json}\n")
	_ = file.Close()
	if _, err := index.Rebuild(store, testConfig(), e.root); err == nil {
		t.Fatal("bad JSONL accepted")
	}
	current, _ := os.ReadFile(index.Path)
	if !bytes.Equal(current, old) {
		// The successful concurrent first rebuild is deterministic and should be
		// byte-identical to the old fresh database.
		t.Fatal("bad ledger path did not preserve old index")
	}
}
