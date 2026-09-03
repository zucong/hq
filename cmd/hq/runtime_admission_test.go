package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gatewayReconcileOperationBarrierStore struct {
	*Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatewayReconcileOperationBarrierStore) lockOperation(scope, stableID string) (func(), error) {
	if scope != operationScopeDelivery {
		return nil, fmt.Errorf("unexpected operation scope %s", scope)
	}
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return func() {}, nil
}

func runtimeEstopState(status string, items ...EstopItem) EstopState {
	state := EstopState{
		Version: estopStateVersion, EstopID: "ESTOP-RUNTIME-TEST", Actor: "penny",
		ActivatedAt: "2026-08-31T00:00:00Z", UpdatedAt: "2026-08-31T00:00:00Z",
		State: status, Reason: "runtime admission test", Items: items,
	}
	if status == "released" {
		state.UpdatedAt = "2026-08-31T00:01:00Z"
		state.ReleaseEventID = "EVENT-RUNTIME-RELEASE"
	}
	return state
}

func writeRuntimeEstopState(t *testing.T, store *FileEstopStore, state EstopState) {
	t.Helper()
	if err := store.WithLock(func(locked *lockedEstopStore) error {
		return locked.Write(state)
	}); err != nil {
		t.Fatal(err)
	}
}

func requireRuntimeAdmissionCode(t *testing.T, err error, code RuntimeAdmissionCode) *RuntimeAdmissionError {
	t.Helper()
	var admission *RuntimeAdmissionError
	if !errors.As(err, &admission) {
		t.Fatalf("error=%T %v, want RuntimeAdmissionError", err, err)
	}
	if admission.Decision.Code != code {
		t.Fatalf("admission=%+v, want code=%s", admission.Decision, code)
	}
	if !strings.Contains(err.Error(), string(code)) {
		t.Fatalf("stable code missing from error text: %v", err)
	}
	return admission
}

func TestRuntimeAdmissionDecisionMatrix(t *testing.T) {
	cfg := testConfig()
	active := runtimeEstopState("active")
	frozenManager := EstopItem{Agent: "zantianyou"}
	tests := []struct {
		name    string
		state   EstopState
		exists  bool
		request RuntimeAdmissionRequest
		allowed bool
		code    RuntimeAdmissionCode
		reason  string
	}{
		{name: "no sentinel", request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentStart, Target: "eng-developer"}, allowed: true, code: runtimeAdmissionAllowed, reason: "no-active-estop"},
		{name: "released", state: runtimeEstopState("released"), exists: true, request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentResume, Target: "eng-developer"}, allowed: true, code: runtimeAdmissionAllowed, reason: "no-active-estop"},
		{name: "manager exempt", state: active, exists: true, request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: "zantianyou"}, allowed: true, code: runtimeAdmissionAllowed, reason: "estop-exempt-control-role"},
		{name: "account closer exempt", state: active, exists: true, request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: "penny"}, allowed: true, code: runtimeAdmissionAllowed, reason: "estop-exempt-control-role"},
		{name: "child denied", state: active, exists: true, request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentStart, Target: "eng-developer"}, code: runtimeAdmissionEstopActive, reason: "target-not-exempt"},
		{name: "unknown denied while active", state: active, exists: true, request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentResume, Target: "removed-seat"}, code: runtimeAdmissionEstopActive, reason: "target-not-active-in-registry"},
		{name: "frozen set overrides later manager role", state: runtimeEstopState("active", frozenManager), exists: true, request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: "zantianyou"}, code: runtimeAdmissionEstopActive, reason: "target-in-frozen-set"},
		{name: "missing target", request: RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt}, code: runtimeAdmissionInvalidRequest, reason: "missing-target"},
		{name: "unknown action", request: RuntimeAdmissionRequest{Action: "restart", Target: "eng-developer"}, code: runtimeAdmissionInvalidRequest, reason: "unknown-action"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			decision := decideRuntimeAdmission(cfg, test.state, test.exists, test.request)
			if decision.Allowed != test.allowed || decision.Code != test.code || decision.Reason != test.reason {
				t.Fatalf("decision=%+v", decision)
			}
			if decision.Action != test.request.Action || decision.Target != strings.TrimSpace(test.request.Target) {
				t.Fatalf("decision does not preserve normalized request: %+v", decision)
			}
		})
	}
}

func TestRuntimeAdmissionStoreFailsClosedAndSerializesActivate(t *testing.T) {
	root := canonicalTestTempDir(t)
	store := &FileEstopStore{Root: filepath.Join(root, "estop")}
	cfg := testConfig()
	request := RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: "eng-developer"}

	t.Run("empty request set never calls closure", func(t *testing.T) {
		var calls atomic.Int32
		decisions, err := store.WithRuntimeAdmissions(cfg, nil, func() error {
			calls.Add(1)
			return nil
		})
		admission := requireRuntimeAdmissionCode(t, err, runtimeAdmissionInvalidRequest)
		if len(decisions) != 1 || admission.Decision.Reason != "empty-request-set" || calls.Load() != 0 {
			t.Fatalf("decisions=%+v admission=%+v calls=%d", decisions, admission.Decision, calls.Load())
		}
	})

	t.Run("invalid request never calls closure", func(t *testing.T) {
		var calls atomic.Int32
		_, err := store.WithRuntimeAdmissions(cfg, []RuntimeAdmissionRequest{{Action: "bad", Target: "eng-developer"}}, func() error {
			calls.Add(1)
			return nil
		})
		requireRuntimeAdmissionCode(t, err, runtimeAdmissionInvalidRequest)
		if calls.Load() != 0 {
			t.Fatalf("invalid request called closure %d times", calls.Load())
		}
	})

	admissionEntered := make(chan struct{})
	releaseAdmission := make(chan struct{})
	admissionDone := make(chan error, 1)
	go func() {
		_, err := store.WithRuntimeAdmissions(cfg, []RuntimeAdmissionRequest{request}, func() error {
			close(admissionEntered)
			<-releaseAdmission
			return nil
		})
		admissionDone <- err
	}()
	select {
	case <-admissionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("admission did not enter")
	}

	activateStarted := make(chan struct{})
	activateEntered := make(chan struct{})
	activateDone := make(chan error, 1)
	go func() {
		close(activateStarted)
		activateDone <- store.WithLock(func(locked *lockedEstopStore) error {
			close(activateEntered)
			return locked.Write(runtimeEstopState("active"))
		})
	}()
	<-activateStarted
	select {
	case <-activateEntered:
		t.Fatal("ESTOP activation entered while admitted runtime mutation still held shared lease")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseAdmission)
	select {
	case err := <-admissionDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("admitted runtime mutation did not finish")
	}
	select {
	case err := <-activateDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ESTOP activation did not acquire exclusive lease")
	}

	var deniedCalls atomic.Int32
	for index := 0; index < 2; index++ {
		decisions, err := store.WithRuntimeAdmissions(cfg, []RuntimeAdmissionRequest{request}, func() error {
			deniedCalls.Add(1)
			return nil
		})
		admission := requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
		if len(decisions) != 1 || decisions[0] != admission.Decision {
			t.Fatalf("decision/error drift: decisions=%+v error=%+v", decisions, admission.Decision)
		}
	}
	if deniedCalls.Load() != 0 {
		t.Fatalf("idempotent denials called closure %d times", deniedCalls.Load())
	}
}

func TestRuntimeAdmissionStoreRejectsUnrecoverableReadWindow(t *testing.T) {
	root := filepath.Join(canonicalTestTempDir(t), "estop")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	store := &FileEstopStore{Root: root}
	if err := os.WriteFile(store.tempPath(), []byte("incomplete"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	_, err := store.WithRuntimeAdmissions(testConfig(), []RuntimeAdmissionRequest{{Action: runtimeAdmissionAgentStart, Target: "eng-developer"}}, func() error {
		called = true
		return nil
	})
	admission := requireRuntimeAdmissionCode(t, err, runtimeAdmissionStateUnavailable)
	if admission.Decision.Reason != "estop-temp-recovery-required" || called {
		t.Fatalf("admission=%+v called=%t", admission.Decision, called)
	}
}

func TestRuntimeAdmissionBlocksUpBeforeAnyHerdrMutation(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	control := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "sessions")}
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))

	err := app.runUp([]string{"--no-gateway", "eng-developer"})
	admission := requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	if admission.Decision.Action != runtimeAdmissionAgentStart || admission.Decision.Target != "eng-developer" {
		t.Fatalf("wrong up decision: %+v", admission.Decision)
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("denied hq up reached Herdr: %v", calls)
	}
	sessions, err := app.Sessions.List(SessionFilter{Agent: "eng-developer"})
	if err != nil || len(sessions) != 0 {
		t.Fatalf("denied hq up wrote session lifecycle: sessions=%+v err=%v", sessions, err)
	}
}

func TestRuntimeAdmissionBlocksExemptManagerUpWithGatewayBeforeAnyHerdrMutation(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	control := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "sessions")}
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))

	// zantianyou is an ESTOP-exempt department manager, so agent_start alone
	// is allowed. Default up also mutates the gateway control plane and must be
	// denied by its independent admission before either side effect can occur.
	err := app.runUp([]string{"zantianyou"})
	admission := requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	if admission.Decision.Action != runtimeAdmissionControlPlane ||
		admission.Decision.Target != app.Config.WorkspaceLabel || admission.Decision.Reason != "control-plane-paused" {
		t.Fatalf("wrong manager up control-plane decision: %+v", admission.Decision)
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("denied manager hq up reached Herdr: %v", calls)
	}
	if _, err := os.Stat(app.Sessions.(*FileSessionStore).Root); !os.IsNotExist(err) {
		t.Fatalf("denied manager hq up touched session store: %v", err)
	}
}

func TestRuntimeAdmissionExemptManagerNoGatewayCannotCreateWorkspace(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	control := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
	control.snapshot.Workspaces = nil
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "sessions")}
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))

	err := app.runUp([]string{"--no-gateway", "zantianyou"})
	if err == nil || !strings.Contains(err.Error(), "--no-gateway 不允许创建 control plane") {
		t.Fatalf("manager-only up created or accepted a missing workspace: %v", err)
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	for _, call := range calls {
		if call != "snapshot" {
			t.Fatalf("manager-only up mutated Herdr while workspace was missing: %v", calls)
		}
	}
	if _, err := os.Stat(app.Sessions.(*FileSessionStore).Root); !os.IsNotExist(err) {
		t.Fatalf("missing-workspace manager up touched session store: %v", err)
	}
}

func TestRuntimeAdmissionBlocksDirectServeBeforeControlPlaneMutation(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	app.GatewayContext = ctx
	before := snapshotTree(t, e.data)

	err := app.serveGatewayCommand([]string{"--workspace-id", "w-test", "--server-id", "gateway-estop-test"})
	admission := requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	if admission.Decision.Action != runtimeAdmissionControlPlane ||
		admission.Decision.Target != app.Config.WorkspaceLabel || admission.Decision.Reason != "control-plane-paused" {
		t.Fatalf("wrong direct serve admission: %+v", admission.Decision)
	}
	if after := snapshotTree(t, e.data); !reflect.DeepEqual(after, before) {
		t.Fatalf("denied direct serve mutated data tree\nbefore=%v\nafter=%v", before, after)
	}
	for _, path := range []string{filepath.Join(e.data, ".gateway-start.lock"), filepath.Join(e.data, "hq.sock")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("denied direct serve created %s: %v", path, err)
		}
	}
}

func TestGatewayStartupReconcileTakesOperationLockBeforeEstopAdmission(t *testing.T) {
	e := setupTestEnv(t)
	_, origin := createPreparedIssueOrigin(t, e, "GATEWAY-RECONCILE-LOCK-ORDER")
	app := e.app(t)
	concrete, ok := app.Store.(*Store)
	if !ok {
		t.Fatalf("test app Store=%T, want *Store", app.Store)
	}
	barrier := &gatewayReconcileOperationBarrierStore{
		Store: concrete, entered: make(chan struct{}), release: make(chan struct{}),
	}
	app.Store = barrier
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.GatewayContext = ctx

	serveDone := make(chan error, 1)
	go func() {
		serveDone <- app.serveGatewayCommand([]string{"--workspace-id", "w-lock-order", "--server-id", "gateway-lock-order"})
	}()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway startup did not enter delivery operation lock")
	}

	// The gateway is now waiting for the delivery operation lock. ESTOP must
	// still be able to take its exclusive lease. If gateway startup acquired
	// control-plane admission first, this activation would block and recreate
	// the E(SH)->operation / operation->E(SH) lock-order deadlock.
	activateDone := make(chan error, 1)
	go func() {
		activateDone <- app.Estop.WithLock(func(locked *lockedEstopStore) error {
			return locked.Write(runtimeEstopState("active", EstopItem{
				Agent: origin.Recipient, Department: "engineering", ReportsTo: "penny",
				WorkspaceID: "w-lock-order", TabID: "tab-lock-order", PaneID: "pane-lock-order",
				CWD: filepath.Join(e.root, "engineering"), Kind: "codex", FreezeOutcome: "pending",
			}))
		})
	}()
	select {
	case err := <-activateDone:
		if err != nil {
			cancel()
			close(barrier.release)
			t.Fatalf("ESTOP activation while gateway waits for operation lock: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		close(barrier.release)
		t.Fatal("gateway startup held ESTOP shared admission while waiting for delivery operation lock")
	}

	close(barrier.release)
	select {
	case err := <-serveDone:
		if err == nil || !strings.Contains(err.Error(), string(runtimeAdmissionEstopActive)) {
			t.Fatalf("gateway reconcile did not fail closed after concurrent ESTOP: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway startup did not finish after operation lock release")
	}
	if len(e.transport.calls) != 0 {
		t.Fatalf("ESTOP-admission denial reached transport: %d calls", len(e.transport.calls))
	}
	if _, err := os.Lstat(filepath.Join(e.data, "hq.sock")); !os.IsNotExist(err) {
		t.Fatalf("failed startup created gateway socket: %v", err)
	}
}

func TestOnlineGatewayReleasesStartupAdmissionLeaseForEstopActivationAndRelease(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	ctx, cancel := context.WithCancel(context.Background())
	app.GatewayContext = ctx
	done := make(chan error, 1)
	go func() {
		done <- app.serveGatewayCommand([]string{"--workspace-id", "w-runtime", "--server-id", "gateway-runtime-lock"})
	}()
	socket := filepath.Join(e.data, "hq.sock")
	deadline := time.Now().Add(2 * time.Second)
	for {
		if health := (unixGatewayPinger{}).Ping(context.Background(), socket, "w-runtime"); health.OK {
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatal("gateway did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}

	activateDone := make(chan error, 1)
	go func() {
		activateDone <- app.Estop.WithLock(func(locked *lockedEstopStore) error {
			return locked.Write(runtimeEstopState("active"))
		})
	}()
	select {
	case err := <-activateDone:
		if err != nil {
			cancel()
			<-done
			t.Fatalf("activate while gateway online: %v", err)
		}
	case <-time.After(2 * time.Second):
		cancel()
		<-done
		t.Fatal("online gateway retained Runtime Admission shared lease and blocked ESTOP activation")
	}
	if health := (unixGatewayPinger{}).Ping(context.Background(), socket, "w-runtime"); !health.OK {
		cancel()
		<-done
		t.Fatalf("gateway must remain available as ESTOP control channel: %+v", health)
	}
	_, err := app.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: "eng-developer"}, nil)
	requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)

	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("released"))
	decision, err := app.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: "eng-developer"}, nil)
	if err != nil || !decision.Allowed {
		cancel()
		<-done
		t.Fatalf("release did not reopen runtime admission: decision=%+v err=%v", decision, err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("gateway shutdown: %v", err)
	}
}

func TestRuntimeAdmissionBlocksZeroAgentUpBeforeControlPlaneMutation(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(*testing.T, *App)
		code       RuntimeAdmissionCode
		wantReason string
	}{
		{
			name: "active estop",
			prepare: func(t *testing.T, app *App) {
				writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))
			},
			code: runtimeAdmissionEstopActive, wantReason: "control-plane-paused",
		},
		{
			name: "unrecoverable temp state",
			prepare: func(t *testing.T, app *App) {
				if err := os.MkdirAll(app.Estop.Root, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(app.Estop.tempPath(), []byte("incomplete"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			code: runtimeAdmissionStateUnavailable, wantReason: "estop-temp-recovery-required",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := setupTestEnv(t)
			app := e.app(t)
			control := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
			app.Herdr = control
			app.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "sessions")}
			test.prepare(t, app)

			err := app.runUp(nil)
			admission := requireRuntimeAdmissionCode(t, err, test.code)
			if admission.Decision.Action != runtimeAdmissionControlPlane ||
				admission.Decision.Target != app.Config.WorkspaceLabel || admission.Decision.Reason != test.wantReason {
				t.Fatalf("wrong zero-agent admission: %+v", admission.Decision)
			}
			control.mu.Lock()
			calls := append([]string(nil), control.calls...)
			control.mu.Unlock()
			if len(calls) != 0 {
				t.Fatalf("denied zero-agent up reached Herdr: %v", calls)
			}
			if _, err := os.Stat(app.Sessions.(*FileSessionStore).Root); !os.IsNotExist(err) {
				t.Fatalf("denied zero-agent up touched session store: %v", err)
			}
		})
	}
}

func TestRuntimeAdmissionDeliveryModesColdResumeAndRetryIdempotence(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeWakeup, 10)
	if _, err := mutateConfig(e.config, func(cfg *Config) error {
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == "eng-developer" {
				cfg.Agents[index].ActivationPolicy = activationAlways
				cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app := deliveryPolicyTestApp(t, e, "eng-data-engineer", "runtime:sender")
	var resumeCalls atomic.Int32
	var resumed atomic.Bool
	app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) {
		if resumed.Load() {
			return deliveryRuntimeIdle, nil
		}
		return deliveryRuntimeOffline, nil
	}
	app.DeliveryColdResume = func(string) error {
		resumeCalls.Add(1)
		resumed.Store(true)
		return nil
	}
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))

	if _, err := runDeliveryPolicyTest(app, "message", "--to", "eng-developer", "--kind", "info", "--text", "active inject", "--delivery", "inject"); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeliveryPolicyTest(app, "message", "--to", "eng-developer", "--kind", "info", "--text", "active quiet", "--delivery", "quiet"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 0 || resumeCalls.Load() != 0 {
		t.Fatalf("inject/quiet called runtime: transport=%d resume=%d", len(e.transport.calls), resumeCalls.Load())
	}

	err := app.coldResumeDeliveryTarget("eng-developer")
	requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	if resumeCalls.Load() != 0 {
		t.Fatalf("denied direct cold-resume ran %d times", resumeCalls.Load())
	}

	_, err = runDeliveryPolicyTest(app, "message", "--to", "eng-developer", "--kind", "request", "--text", "active wake", "--delivery", "wakeup")
	admission := requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	if admission.Decision.Action != runtimeAdmissionAgentPrompt || admission.Decision.Target != "eng-developer" {
		t.Fatalf("wrong wake decision: %+v", admission.Decision)
	}
	if len(e.transport.calls) != 0 || resumeCalls.Load() != 0 {
		t.Fatalf("denied wake called runtime: transport=%d resume=%d", len(e.transport.calls), resumeCalls.Load())
	}

	var origin Event
	for _, message := range deliveryPolicyMessages(t, e) {
		if message.Message == "active wake" {
			origin = message
		}
	}
	if origin.ID == "" {
		t.Fatal("missing blocked wake origin")
	}
	view, ok, err := app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || !ok || view.Status != deliveryFailedPreSend || view.AttemptCount != 1 {
		t.Fatalf("initial denied view=%+v ok=%t err=%v", view, ok, err)
	}

	const retryCommand = "delivery-retry:runtime-stable"
	for index := 0; index < 2; index++ {
		_, retryErr := app.processDelivery(origin, retryCommand)
		requireRuntimeAdmissionCode(t, retryErr, runtimeAdmissionEstopActive)
	}
	view, ok, err = app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || !ok || view.Status != deliveryFailedPreSend || view.AttemptCount != 2 {
		t.Fatalf("same retry command was not idempotent: view=%+v ok=%t err=%v", view, ok, err)
	}
	if len(e.transport.calls) != 0 || resumeCalls.Load() != 0 {
		t.Fatalf("denied retries called runtime: transport=%d resume=%d", len(e.transport.calls), resumeCalls.Load())
	}

	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("released"))
	outcome, retryErr := app.processDelivery(origin, "delivery-retry:after-release")
	if retryErr != nil {
		t.Fatal(retryErr)
	}
	if outcome.DeliveryStatus != deliverySent || len(e.transport.calls) != 1 || resumeCalls.Load() != 1 {
		t.Fatalf("release did not restore exact retry path: outcome=%+v transport=%d resume=%d", outcome, len(e.transport.calls), resumeCalls.Load())
	}
}

func prepareRuntimeAdmissionMessage(t *testing.T, app *App, actorName, target, text string) Event {
	t.Helper()
	actorRule, _ := app.Config.exactRule(actorName)
	targetRule, _ := app.Config.exactRule(target)
	actor := Actor{Name: actorRule.Name, Label: actorRule.Label, Department: actorRule.Department, PaneID: "runtime:reconcile", Rule: actorRule}
	commandID := stableCommandID("runtime-admission-prepared", actorName, target, text)
	result, err := app.transact(commandID, requestDigest("runtime-admission-prepared", commandID), func(*ledgerState) (Event, error) {
		event, err := app.newEvent(actor, "message_prepared", "")
		if err != nil {
			return Event{}, err
		}
		event.Recipient, event.RecipientLabel = targetRule.Name, targetRule.Label
		event.MessageID, event.MessageKind, event.Message = stableMessageID(commandID), "request", text
		event.ThreadID = stableMessageID(stableCommandID("runtime-thread", actorName, target))
		event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "runtime-admission-test"
		event.Delivery, event.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, target)
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
	return result.Event
}

func TestRuntimeAdmissionAutomaticDeliveryRecoveryStaysFrozen(t *testing.T) {
	e := setupTestEnv(t)
	app := deliveryPolicyTestApp(t, e, "eng-data-engineer", "runtime:reconcile")
	app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeOffline, nil }
	var resumeCalls atomic.Int32
	app.DeliveryColdResume = func(string) error {
		resumeCalls.Add(1)
		return nil
	}
	origin := prepareRuntimeAdmissionMessage(t, app, "eng-data-engineer", "eng-developer", "automatic recovery")
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))

	err := app.reconcileDeliveries()
	if err == nil || !strings.Contains(err.Error(), string(runtimeAdmissionEstopActive)) {
		t.Fatalf("reconcile did not surface stable admission code: %v", err)
	}
	view, ok, viewErr := app.Store.Delivery(app.Config, origin.DeliveryID)
	if viewErr != nil || !ok || view.Status != deliveryFailedPreSend || view.AttemptCount != 1 {
		t.Fatalf("reconcile view=%+v ok=%t err=%v", view, ok, viewErr)
	}
	if len(e.transport.calls) != 0 || resumeCalls.Load() != 0 {
		t.Fatalf("automatic recovery bypassed gate: transport=%d resume=%d", len(e.transport.calls), resumeCalls.Load())
	}
}

func TestRuntimeAdmissionNudgePromptUsesGate(t *testing.T) {
	e := setupTestEnv(t)
	now := time.Date(2026, 8, 31, 2, 0, 0, 0, time.UTC)
	control := newFakeHerdrControl(e.root, testConfig().WorkspaceLabel)
	addOperationsLive(&control.snapshot, testConfig(), e.root, "zantianyou", "working", "runtime-manager")
	app := operationsTestApp(t, e, control, &now)
	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-RUNTIME-GATE", "--dedupe", "runtime:gate", "--to", "zantianyou", "--message", "must stay frozen", "--ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-RUNTIME-GATE", "--claim", "CLAIM-RUNTIME-GATE", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	rule, _ := app.Config.exactRule("zantianyou")
	item := EstopItem{
		Agent: "zantianyou", Department: rule.Department, ReportsTo: rule.ReportsTo,
		WorkspaceID: "w-test", TabID: "w-test:truntime-manager", PaneID: "w-test:pruntime-manager",
		CWD: testRuleCWD(e.root, rule), Kind: rule.Kind, FreezeOutcome: "confirmed",
	}
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active", item))

	before := countFakeCalls(control, "prompt ")
	err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-RUNTIME-GATE", "--claim", "CLAIM-RUNTIME-GATE"})
	requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	if got := countFakeCalls(control, "prompt "); got != before {
		t.Fatalf("denied nudge called Prompt: before=%d after=%d", before, got)
	}
	view, readErr := app.readNudge("NUDGE-RUNTIME-GATE")
	if readErr != nil || view.State != "claimed" {
		t.Fatalf("denied nudge must remain safely retryable: view=%+v err=%v", view, readErr)
	}
}

func TestRuntimeAdmissionPublicStartAndDeliverAreGated(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	control := newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "public-gate-sessions")}
	writeRuntimeEstopState(t, app.Estop, runtimeEstopState("active"))
	rule, _ := app.Config.exactRule("eng-developer")

	err := app.startHQAgent(context.Background(), "w-test", rule)
	requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	err = app.deliver("eng-developer", "blocked")
	requireRuntimeAdmissionCode(t, err, runtimeAdmissionEstopActive)
	if len(e.transport.calls) != 0 {
		t.Fatalf("public deliver bypassed gate: %d calls", len(e.transport.calls))
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	if len(calls) != 0 {
		t.Fatalf("public start bypassed gate: %v", calls)
	}
}
