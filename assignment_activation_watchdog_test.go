package main

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type reconcileReadCountingStore struct {
	EventStore
	views         []DeliveryView
	deliveryReads int
}

func (s *reconcileReadCountingStore) Deliveries(Config) ([]DeliveryView, error) {
	return append([]DeliveryView(nil), s.views...), nil
}

func (s *reconcileReadCountingStore) ReadAll(Config) ([]Event, error) { return nil, nil }

func (s *reconcileReadCountingStore) Delivery(Config, string) (DeliveryView, bool, error) {
	s.deliveryReads++
	return DeliveryView{}, false, nil
}

func assignmentActivationFixture(t *testing.T, terminal string) (testEnv, Config, *App, *fallbackReaderControl, string, string) {
	t.Helper()
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 10,
			AssignmentAcceptTimeout: "15s", MaxActivationRedeliveries: 2}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := cfg.exactRule("zantianyou")
	worker, _ := cfg.exactRule("eng-developer")
	e.setActor(t, manager.Name, "activation:manager", testAgentCWD(cfg, e.root, manager.Name))
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "activation-source.md"), "# activation source\n")
	runTestCommand(t, e, "case", "create", "--id", "ACTIVATION-WATCHDOG-001", "--title", "activation watchdog", "--source", source)
	runTestCommand(t, e, "issue", "--case", "ACTIVATION-WATCHDOG-001", "--to", worker.Name, "--next", "implement and verify")

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	var deliveryID, assignmentEventID string
	for id, record := range ledger.deliveries {
		if record.Origin.Type == "issue_prepared" {
			deliveryID, assignmentEventID = id, record.Terminal.ID
		}
	}
	if deliveryID == "" || assignmentEventID == "" {
		t.Fatal("issue delivery/assignment not found")
	}

	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, worker, "idle")
	control.snapshot.Panes[0].TerminalID, control.snapshot.Panes[0].Revision = "terminal-activation-1", 1
	control.snapshot.Agents[0].TerminalID, control.snapshot.Agents[0].Revision = "terminal-activation-1", 1
	control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: worker.Name, Kind: worker.Kind, Value: "activation-session-1"}
	reader := &fallbackReaderControl{fakeHerdrControl: control, terminal: []byte(terminal)}
	sessions := &FileSessionStore{Root: filepath.Join(e.data, "activation-sessions")}
	created := HerdrTabCreated{Tab: control.snapshot.Tabs[0], Pane: control.snapshot.Panes[0]}
	started, err := newSessionEvent(time.Now().Add(-time.Minute), sessionStarted, created, "w-test", worker,
		"hq-up", "activation test runtime", testAgentCWD(cfg, e.root, worker.Name))
	if err == nil {
		binding, bindingErr := ResolveLiveBinding(control.snapshot, cfg, e.root, LiveBindingRequest{Seat: worker.Name, RequireInteractiveReady: true})
		if bindingErr != nil {
			err = bindingErr
		} else {
			started, err = bindSessionEventRuntime(started, binding)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := sessions.Append(started); err != nil {
		t.Fatal(err)
	}

	app := e.app(t)
	app.Config, app.Herdr, app.Sessions = cfg, reader, sessions
	app.Clock = func() time.Time { return time.Now().UTC().Add(10 * time.Minute) }
	return e, cfg, app, reader, deliveryID, assignmentEventID
}

func TestAssignmentActivationWatchdogRedeliversSamePayloadThenStopsAfterAccept(t *testing.T) {
	e, cfg, app, control, deliveryID, assignmentEventID := assignmentActivationFixture(t, "› Ask Codex to do anything\n")
	// Herdr reports a completed turn with a visible input prompt as done rather
	// than idle. Both are safe activation boundaries.
	control.mu.Lock()
	control.snapshot.Agents[0].Status = "done"
	control.mu.Unlock()
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	originalPayload, err := app.deliveryPayload(ledger.deliveries[deliveryID].Origin)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	record := fresh.deliveries[deliveryID]
	if record.ActivationStatus != activationSent || record.ActivationAttemptCount != 1 {
		t.Fatalf("activation=%s attempts=%d", record.ActivationStatus, record.ActivationAttemptCount)
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	var prompts []string
	for _, call := range calls {
		if strings.HasPrefix(call, "prompt ") {
			prompts = append(prompts, call)
		}
	}
	if len(prompts) != 1 || prompts[0] != "prompt eng-developer "+originalPayload {
		t.Fatalf("watchdog must replay exact original payload once: %#v", calls)
	}

	worker, _ := cfg.exactRule("eng-developer")
	e.setActor(t, worker.Name, "activation:worker", testAgentCWD(cfg, e.root, worker.Name))
	runTestCommand(t, e, "accept", "--event", assignmentEventID, "--next", "开始执行")
	if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	control.mu.Lock()
	defer control.mu.Unlock()
	promptCount := 0
	for _, call := range control.calls {
		if strings.HasPrefix(call, "prompt ") {
			promptCount++
		}
	}
	if promptCount != 1 {
		t.Fatalf("accepted assignment was redelivered: %#v", control.calls)
	}
}

func TestAssignmentActivationWatchdogFreezesAmbiguousAndRejectsUnsafeTerminal(t *testing.T) {
	t.Run("ambiguous", func(t *testing.T) {
		_, _, app, control, deliveryID, _ := assignmentActivationFixture(t, "› Ask Codex to do anything\n")
		control.promptOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("timeout")}
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		ledger, err := app.ledgerState()
		if err != nil {
			t.Fatal(err)
		}
		if got := ledger.deliveries[deliveryID].ActivationStatus; got != activationUnknown {
			t.Fatalf("activation=%s want unknown", got)
		}
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		control.mu.Lock()
		defer control.mu.Unlock()
		promptCount := 0
		for _, call := range control.calls {
			if strings.HasPrefix(call, "prompt ") {
				promptCount++
			}
		}
		if promptCount != 1 {
			t.Fatalf("unknown activation was automatically replayed: %#v", control.calls)
		}
	})

	t.Run("hook-trust-modal", func(t *testing.T) {
		_, _, app, control, deliveryID, _ := assignmentActivationFixture(t,
			"› Ask Codex to do anything\nDo you trust this hook?\n--dangerously-bypass-hook-trust\n")
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		ledger, err := app.ledgerState()
		if err != nil {
			t.Fatal(err)
		}
		if got := ledger.deliveries[deliveryID].ActivationAttemptCount; got != 0 {
			t.Fatalf("unsafe terminal activation attempts=%d", got)
		}
		control.mu.Lock()
		defer control.mu.Unlock()
		if strings.Contains(strings.Join(control.calls, "\n"), "prompt ") {
			t.Fatalf("watchdog prompted into hook trust modal: %#v", control.calls)
		}
	})
}

func TestAssignmentActivationUnknownUsesExistingResolveAndRetryCommands(t *testing.T) {
	e, cfg, app, control, deliveryID, _ := assignmentActivationFixture(t, "› Ask Codex to do anything\n")
	control.promptOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("timeout")}
	if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	evidence := writeTestFile(t, filepath.Join(e.root, "ceo-office", "decisions", "activation-check.md"), "terminal showed no injected prompt\n")
	operator, _ := cfg.exactRule("penny")
	e.setActor(t, operator.Name, "activation:operator", testAgentCWD(cfg, e.root, operator.Name))
	var out, errOut bytes.Buffer
	app.Out, app.Err = &out, &errOut
	if err := app.run([]string{"delivery", "resolve", "--id", deliveryID, "--outcome", "not-delivered",
		"--reason", "terminal evidence confirms prompt was not injected", "--evidence", evidence}); err != nil {
		t.Fatalf("resolve activation unknown: %v\nstderr=%s", err, errOut.String())
	}
	view, ok, err := app.Store.Delivery(app.Config, deliveryID)
	if err != nil || !ok || view.ActivationStatus != activationFailedPreSend {
		t.Fatalf("resolved view=%+v ok=%t err=%v", view, ok, err)
	}
	control.promptOutcome = HerdrMutationResult{Outcome: herdrConfirmed}
	out.Reset()
	errOut.Reset()
	if err := app.run([]string{"delivery", "retry", "--id", deliveryID}); err != nil {
		t.Fatalf("retry resolved activation: %v\nstderr=%s", err, errOut.String())
	}
	view, ok, err = app.Store.Delivery(app.Config, deliveryID)
	if err != nil || !ok || view.ActivationStatus != activationSent || view.ActivationAttemptCount != 2 {
		t.Fatalf("retried view=%+v ok=%t err=%v", view, ok, err)
	}
}

func TestAssignmentActivationLedgerRejectsForgedAssignmentBinding(t *testing.T) {
	_, _, app, _, deliveryID, _ := assignmentActivationFixture(t, "› Ask Codex to do anything\n")
	ledger, err := app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	record := ledger.deliveries[deliveryID]
	assignment := ledger.assignments[record.Terminal.ID]
	actor := Actor{Name: record.Origin.Actor, Label: record.Origin.ActorLabel, PaneID: record.Origin.ActorPaneID}
	_, err = app.Store.Transact(app.Config, "forged-activation", requestDigest("forged-activation"), false, func(current *ledgerState) (Event, error) {
		event, err := app.newOperationsEvent(actor, "assignment_activation_attempted", record.Origin.CaseID)
		if err != nil {
			return Event{}, err
		}
		populateAssignmentActivationEvent(&event, current.deliveries[deliveryID], assignment)
		event.AssignmentID = "assignment:forged"
		event.Delivery = deliveryAttempted
		return event, nil
	})
	if err == nil || !strings.Contains(err.Error(), "冻结 assignment 合同不一致") {
		t.Fatalf("forged activation err=%v", err)
	}
}

func TestAssignmentActivationCrashAndExhaustionRemainBounded(t *testing.T) {
	t.Run("crash-after-attempt", func(t *testing.T) {
		_, _, app, control, deliveryID, _ := assignmentActivationFixture(t, "› Ask Codex to do anything\n")
		app.DeliveryFailpoint = func(point string) error {
			if point == "after_assignment_activation_attempt_recorded" {
				return errors.New("synthetic crash")
			}
			return nil
		}
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err == nil || !strings.Contains(err.Error(), "synthetic crash") {
			t.Fatalf("crash err=%v", err)
		}
		app.DeliveryFailpoint = nil
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		ledger, err := app.ledgerState()
		if err != nil {
			t.Fatal(err)
		}
		if got := ledger.deliveries[deliveryID].ActivationStatus; got != activationUnknown {
			t.Fatalf("stale attempt activation=%s", got)
		}
		control.mu.Lock()
		defer control.mu.Unlock()
		for _, call := range control.calls {
			if strings.HasPrefix(call, "prompt ") {
				t.Fatalf("crash-before-prompt must not prompt: %#v", control.calls)
			}
		}
	})

	t.Run("exhausted-explicit-retry", func(t *testing.T) {
		e, cfg, app, control, deliveryID, _ := assignmentActivationFixture(t, "› Ask Codex to do anything\n")
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		app.Clock = func() time.Time { return time.Now().UTC().Add(30 * time.Minute) }
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		app.Clock = func() time.Time { return time.Now().UTC().Add(60 * time.Minute) }
		if err := app.recoverIssuedAssignmentActivationsOnce(context.Background()); err != nil {
			t.Fatal(err)
		}
		view, ok, err := app.Store.Delivery(app.Config, deliveryID)
		if err != nil || !ok || view.ActivationStatus != activationExhausted || view.ActivationAttemptCount != 2 {
			t.Fatalf("exhausted view=%+v ok=%t err=%v", view, ok, err)
		}
		operator, _ := cfg.exactRule("penny")
		e.setActor(t, operator.Name, "activation:operator", testAgentCWD(cfg, e.root, operator.Name))
		var out, errOut bytes.Buffer
		app.Out, app.Err = &out, &errOut
		if err := app.run([]string{"delivery", "retry", "--id", deliveryID}); err != nil {
			t.Fatalf("explicit exhausted retry: %v\nstderr=%s", err, errOut.String())
		}
		view, ok, err = app.Store.Delivery(app.Config, deliveryID)
		if err != nil || !ok || view.ActivationStatus != activationSent || view.ActivationAttemptCount != 3 {
			t.Fatalf("explicit retry view=%+v ok=%t err=%v", view, ok, err)
		}
		control.mu.Lock()
		defer control.mu.Unlock()
		promptCount := 0
		for _, call := range control.calls {
			if strings.HasPrefix(call, "prompt ") {
				promptCount++
			}
		}
		if promptCount != 3 {
			t.Fatalf("promptCount=%d calls=%#v", promptCount, control.calls)
		}
	})
}

func TestReconcileSkipsConvergedHistoricalDeliveriesBeforeStrictReread(t *testing.T) {
	store := &reconcileReadCountingStore{views: []DeliveryView{
		{DeliveryID: "delivery:sent", Status: deliverySent},
		{DeliveryID: "delivery:failed", Status: deliveryFailedPreSend},
		{DeliveryID: "delivery:unknown", Status: deliveryUnknown},
		{DeliveryID: "delivery:inject", Status: deliveryQueued, DeliveryMode: deliveryModeInject},
	}}
	app := &App{Store: store, Config: testConfig()}
	if err := app.reconcileDeliveries(); err != nil {
		t.Fatal(err)
	}
	if store.deliveryReads != 0 {
		t.Fatalf("converged historical deliveries caused %d strict per-item rereads", store.deliveryReads)
	}
}
