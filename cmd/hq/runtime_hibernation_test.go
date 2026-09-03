package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type runtimeHibernateFixture struct {
	e        testEnv
	cfg      Config
	app      *App
	control  *fakeHerdrControl
	sessions *FileSessionStore
	worker   AgentRule
	started  SessionEvent
}

type runtimeIncarnationControl struct {
	*fakeHerdrControl
	mu   sync.Mutex
	next int
}

func (c *runtimeIncarnationControl) StartAgent(ctx context.Context, name, kind, paneID string, native []string) HerdrMutationResult {
	result := c.fakeHerdrControl.StartAgent(ctx, name, kind, paneID, native)
	if result.Outcome != herdrConfirmed || result.Err != nil {
		return result
	}
	c.mu.Lock()
	c.next++
	incarnation := c.next
	c.mu.Unlock()
	c.fakeHerdrControl.mu.Lock()
	defer c.fakeHerdrControl.mu.Unlock()
	terminalID := fmt.Sprintf("terminal-cold-%d", incarnation)
	for index := range c.fakeHerdrControl.snapshot.Panes {
		if c.fakeHerdrControl.snapshot.Panes[index].ID == paneID {
			c.fakeHerdrControl.snapshot.Panes[index].TerminalID = terminalID
			c.fakeHerdrControl.snapshot.Panes[index].Revision = uint64(incarnation + 1)
		}
	}
	for index := range c.fakeHerdrControl.snapshot.Agents {
		agent := &c.fakeHerdrControl.snapshot.Agents[index]
		if agent.Name == name && agent.PaneID == paneID {
			agent.TerminalID = terminalID
			agent.Revision = uint64(incarnation + 1)
			agent.AgentSession = &HerdrAgentSession{Source: "native", Agent: name, Kind: kind, Value: fmt.Sprintf("cold-session-%d", incarnation)}
		}
	}
	return result
}

type failNthSnapshotControl struct {
	HerdrControl
	mu     sync.Mutex
	calls  int
	failAt int
}

func (c *failNthSnapshotControl) Snapshot(ctx context.Context, scope HerdrSnapshotScope) (HerdrSnapshot, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == c.failAt {
		return HerdrSnapshot{}, errors.New("injected snapshot failure")
	}
	return c.HerdrControl.Snapshot(ctx, scope)
}

type mutateNthSnapshotControl struct {
	HerdrControl
	mu       sync.Mutex
	calls    int
	mutateAt int
	mutate   func()
}

func (c *mutateNthSnapshotControl) Snapshot(ctx context.Context, scope HerdrSnapshotScope) (HerdrSnapshot, error) {
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == c.mutateAt && c.mutate != nil {
		c.mutate()
	}
	return c.HerdrControl.Snapshot(ctx, scope)
}

type trackingRuntimeSeatStore struct {
	*Store
	mu       sync.Mutex
	calls    int
	notifyAt int
	notify   chan struct{}
}

func (s *trackingRuntimeSeatStore) lockRuntimeSeat(agent string) (func(), error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	if call == s.notifyAt && s.notify != nil {
		close(s.notify)
	}
	s.mu.Unlock()
	return s.Store.lockRuntimeSeat(agent)
}

type blockingRuntimeCloseControl struct {
	*fakeHerdrControl
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *blockingRuntimeCloseControl) CloseTab(ctx context.Context, tabID string) HerdrMutationResult {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-ctx.Done():
		return HerdrMutationResult{Outcome: herdrAmbiguous, Err: ctx.Err()}
	case <-c.release:
		return c.fakeHerdrControl.CloseTab(ctx, tabID)
	}
}

type blockingRuntimePromptControl struct {
	*fakeHerdrControl
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type blockingFinalRegistryLeaseStore struct {
	*Store
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingFinalRegistryLeaseStore) lockCurrentRegistry(cfg Config) (func(), error) {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.Store.lockCurrentRegistry(cfg)
}

func (c *blockingRuntimePromptControl) Prompt(ctx context.Context, target, message string) HerdrMutationResult {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-ctx.Done():
		return HerdrMutationResult{Outcome: herdrAmbiguous, Err: ctx.Err()}
	case <-c.release:
		return c.fakeHerdrControl.Prompt(ctx, target, message)
	}
}

func prepareEligibleRuntimeSeat(t *testing.T) runtimeHibernateFixture {
	t.Helper()
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == "eng-developer" {
				cfg.Agents[index].ActivationPolicy = activationOnAssignment
				cfg.Agents[index].KeepWarm = "0s"
				finalizeTestSeatMutation(&cfg.Agents[index])
			} else if cfg.Agents[index].Name == "zantianyou" {
				cfg.Agents[index].ActivationPolicy = activationAlways
				cfg.Agents[index].KeepWarm = ""
				finalizeTestSeatMutation(&cfg.Agents[index])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "runtime-hibernate-source.md"), "# source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "runtime-hibernate-result.md"), "# result\n")
	managerPane, workerPane := "runtime:manager", "runtime:worker"
	e.setActor(t, "zantianyou", managerPane, testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "case", "create", "--id", "RUNTIME-HIBERNATE-001", "--title", "runtime lifecycle", "--source", source)
	runTestCommand(t, e, "issue", "--case", "RUNTIME-HIBERNATE-001", "--to", "eng-developer", "--next", "implement")
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestEventOfType(events, "issue_sent")
	if issue.ID == "" {
		t.Fatal("missing issue_sent")
	}
	e.setActor(t, "eng-developer", workerPane, testAgentCWD(cfg, e.root, "eng-developer"))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "work")
	runTestCommand(t, e, "report", "--case", "RUNTIME-HIBERNATE-001", "--result", "completed", "--artifact", artifact, "--next", "verify")
	events, err = NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	report := latestEventOfType(events, "report_sent")
	if report.ID == "" {
		t.Fatal("missing report_sent")
	}
	e.setActor(t, "zantianyou", managerPane, testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "accept", "--event", report.ID, "--next", "done")

	worker, _ := cfg.exactRule("eng-developer")
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, worker, "idle")
	control.snapshot.Panes[0].TerminalID = "terminal-runtime-1"
	control.snapshot.Panes[0].Revision = 1
	control.snapshot.Agents[0].TerminalID = "terminal-runtime-1"
	control.snapshot.Agents[0].Revision = 1
	control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: worker.Name, Kind: worker.Kind, Value: "runtime-session-1"}
	manager, _ := cfg.exactRule("zantianyou")
	managerCWD := testAgentCWD(cfg, e.root, manager.Name)
	control.snapshot.Tabs = append(control.snapshot.Tabs, HerdrTab{ID: "w-test:t-manager", WorkspaceID: "w-test", Label: rosterTabLabel(manager), CWD: managerCWD})
	control.snapshot.Panes = append(control.snapshot.Panes, HerdrPane{ID: "w-test:p-manager", WorkspaceID: "w-test", TabID: "w-test:t-manager", CWD: managerCWD, TerminalID: "terminal-manager", Revision: 1})
	control.snapshot.Agents = append(control.snapshot.Agents, HerdrAgent{Name: manager.Name, Kind: manager.Kind, Status: "idle", CWD: managerCWD, WorkspaceID: "w-test", TabID: "w-test:t-manager", PaneID: "w-test:p-manager", TerminalID: "terminal-manager", AgentSession: &HerdrAgentSession{Source: "native", Agent: manager.Name, Kind: manager.Kind, Value: "manager-session-1"}, Revision: 1, InteractiveReady: true})
	control.nextID = 2
	sessions := &FileSessionStore{Root: filepath.Join(e.data, "runtime-sessions")}
	created := HerdrTabCreated{Tab: control.snapshot.Tabs[0], Pane: control.snapshot.Panes[0]}
	started, err := newSessionEvent(time.Now().Add(-time.Minute), sessionStarted, created, "w-test", worker, "hq-up", "test runtime", testAgentCWD(cfg, e.root, worker.Name))
	if err == nil {
		binding, bindingErr := ResolveLiveBinding(control.snapshot, cfg, e.root, LiveBindingRequest{Seat: worker.Name, RequireInteractiveReady: true})
		if bindingErr != nil {
			err = bindingErr
		} else {
			started, err = bindSessionEventRuntime(started, binding)
		}
	}
	if err != nil || sessions.Append(started) != nil {
		t.Fatalf("seed session: event=%+v err=%v", started, err)
	}
	app := e.app(t)
	runtimeControl := &runtimeIncarnationControl{fakeHerdrControl: control}
	app.Herdr = runtimeControl
	app.Transport = herdrDeliveryTransport{Control: runtimeControl}
	app.Sessions = sessions
	app.Clock = func() time.Time { return time.Now().Add(time.Minute) }
	app.Out, app.Err = io.Discard, io.Discard
	return runtimeHibernateFixture{e: e, cfg: cfg, app: app, control: control, sessions: sessions, worker: worker, started: started}
}

func countRuntimeCalls(control *fakeHerdrControl, prefix string) int {
	control.mu.Lock()
	defer control.mu.Unlock()
	count := 0
	for _, call := range control.calls {
		if strings.HasPrefix(call, prefix) {
			count++
		}
	}
	return count
}

func TestRuntimeHibernationPreservesBusinessSeatAndColdResumes(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.app.CallerPane, f.app.FromGateway = "runtime:manager", true
	if err := f.app.run([]string{"message", "--to", f.worker.Name, "--kind", "info", "--text", "queued context survives hibernation", "--delivery", "quiet"}); err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := f.app.Store.ReadAll(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	beforeConfig, _ := os.ReadFile(f.e.config)
	manualPath := filepath.Join(f.e.root, f.worker.ManualPath)
	beforeManual, _ := os.ReadFile(manualPath)

	view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err != nil || view.LastOutcome != sessionStopped || view.RuntimeState != "offline" {
		t.Fatalf("reap=%+v err=%v", view, err)
	}
	if got := countRuntimeCalls(f.control, "tab close "); got != 1 {
		t.Fatalf("CloseTab calls=%d", got)
	}
	afterEvents, err := f.app.Store.ReadAll(f.cfg)
	if err != nil || !reflect.DeepEqual(beforeEvents, afterEvents) {
		t.Fatalf("runtime reap changed business ledger: err=%v before=%d after=%d", err, len(beforeEvents), len(afterEvents))
	}
	afterConfig, _ := os.ReadFile(f.e.config)
	afterManual, _ := os.ReadFile(manualPath)
	if !bytes.Equal(beforeConfig, afterConfig) || !bytes.Equal(beforeManual, afterManual) {
		t.Fatal("runtime reap changed registry or role card")
	}
	ledger, err := f.app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	completed := false
	for _, assignment := range ledger.assignments {
		completed = completed || (assignment.Recipient == f.worker.Name && assignment.Consumed && assignment.Status == "completed")
	}
	if !completed || ledger.snapshot.Cases["RUNTIME-HIBERNATE-001"].Status != string(statusAccepted) {
		t.Fatalf("business terminal changed: completed=%t case=%+v", completed, ledger.snapshot.Cases["RUNTIME-HIBERNATE-001"])
	}

	managerPane := "runtime:manager"
	f.app.CallerPane, f.app.FromGateway = managerPane, true
	source := writeTestFile(t, filepath.Join(f.e.root, "engineering", "runtime-cold-resume.md"), "# next\n")
	if err := f.app.run([]string{"case", "create", "--id", "RUNTIME-HIBERNATE-002", "--parent", "RUNTIME-HIBERNATE-001", "--title", "cold resume", "--source", source}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.run([]string{"issue", "--case", "RUNTIME-HIBERNATE-002", "--to", f.worker.Name, "--next", "resume same seat"}); err != nil {
		t.Fatal(err)
	}
	if countRuntimeCalls(f.control, "tab create ") != 1 || countRuntimeCalls(f.control, "agent start "+f.worker.Name) != 1 || countRuntimeCalls(f.control, "prompt "+f.worker.Name+" ") != 2 {
		f.control.mu.Lock()
		calls := append([]string(nil), f.control.calls...)
		f.control.mu.Unlock()
		t.Fatalf("cold resume did not create/start/startup+delivery prompt once: %v", calls)
	}
	f.control.mu.Lock()
	allCalls := strings.Join(f.control.calls, "\n")
	f.control.mu.Unlock()
	if !strings.Contains(allCalls, "queued context survives hibernation") {
		t.Fatalf("cold issue turn bundle omitted durable quiet info: %s", allCalls)
	}
	afterWakeLedger, err := f.app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	claimedInfo := false
	for _, delivery := range afterWakeLedger.deliveries {
		if delivery.Origin.Message == "queued context survives hibernation" && delivery.ContextState == deliveryContextClaimed {
			claimedInfo = true
		}
	}
	if !claimedInfo {
		t.Fatal("next issue did not durably claim queued info context")
	}
	sessionEvents, err := f.sessions.List(SessionFilter{Agent: f.worker.Name})
	if err != nil || len(activeSessionStarts(sessionEvents)) != 1 {
		t.Fatalf("cold resume active sessions=%+v err=%v", activeSessionStarts(sessionEvents), err)
	}
	active := activeSessionStarts(sessionEvents)[0]
	if active.SessionID == f.started.SessionID || active.Agent != f.worker.Name {
		t.Fatalf("cold resume did not reuse stable seat with new runtime: old=%s active=%+v", f.started.SessionID, active)
	}
	if active.TerminalID == "" || active.AgentSessionValue == "" {
		t.Fatalf("cold-resumed session lacks exact runtime incarnation proof: %+v", active)
	}

	// Complete a second real assignment cycle, then reap again. An always-on
	// manager sharing the workspace must remain present while only the stable
	// on_assignment specialist gets a new runtime incarnation and is closed.
	events, err := f.app.Store.ReadAll(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	secondIssue := Event{}
	for _, event := range events {
		if event.Type == "issue_sent" && event.CaseID == "RUNTIME-HIBERNATE-002" {
			secondIssue = event
		}
	}
	if secondIssue.ID == "" {
		t.Fatal("missing second issue delivery")
	}
	f.app.CallerPane, f.app.FromGateway = "runtime:worker", true
	if err := f.app.run([]string{"accept", "--event", secondIssue.ID, "--next", "second cycle work"}); err != nil {
		t.Fatal(err)
	}
	secondArtifact := writeTestFile(t, filepath.Join(f.e.root, "engineering", "runtime-second-result.md"), "# second result\n")
	if err := f.app.run([]string{"report", "--case", "RUNTIME-HIBERNATE-002", "--result", "completed", "--artifact", secondArtifact, "--next", "verify second cycle"}); err != nil {
		t.Fatal(err)
	}
	events, err = f.app.Store.ReadAll(f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	secondReport := Event{}
	for _, event := range events {
		if event.Type == "report_sent" && event.CaseID == "RUNTIME-HIBERNATE-002" {
			secondReport = event
		}
	}
	if secondReport.ID == "" {
		t.Fatal("missing second report delivery")
	}
	f.app.CallerPane = managerPane
	if err := f.app.run([]string{"accept", "--event", secondReport.ID, "--next", "second cycle done"}); err != nil {
		t.Fatal(err)
	}
	manager, ok := f.cfg.exactRule("zantianyou")
	if !ok || manager.ActivationPolicy != activationAlways {
		t.Fatalf("fixture manager is not always-on: %+v", manager)
	}
	report, err := f.app.reapRuntimeSeats("", false, false)
	if err != nil || len(report.Seats) != 1 || report.Seats[0].Agent != f.worker.Name || report.Seats[0].LastOutcome != sessionStopped {
		t.Fatalf("second-cycle reap=%+v err=%v", report, err)
	}
	if got := countRuntimeCalls(f.control, "tab close "); got != 2 {
		t.Fatalf("two completed cycles should close exactly twice, got=%d", got)
	}
	f.control.mu.Lock()
	managerStillLive := false
	for _, agent := range f.control.snapshot.Agents {
		managerStillLive = managerStillLive || agent.Name == manager.Name
	}
	f.control.mu.Unlock()
	if !managerStillLive {
		t.Fatal("bulk runtime reaper stopped an activation=always manager")
	}
}

func TestRuntimeHibernationCloseOutcomeRetryAndCrashRecovery(t *testing.T) {
	t.Run("ambiguous absent converges stopped", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		f.control.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("timeout")}
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err != nil || view.LastOutcome != sessionStopped || countRuntimeCalls(f.control, "tab close ") != 1 {
			t.Fatalf("view=%+v err=%v", view, err)
		}
	})

	t.Run("definitely not run requires explicit retry", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		f.control.closeMutates = false
		f.control.closeOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("not run")}
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err == nil || view.LastOutcome != sessionHibernateFailed || view.Eligible {
			t.Fatalf("first view=%+v err=%v", view, err)
		}
		before := countRuntimeCalls(f.control, "tab close ")
		view, err = f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err != nil || countRuntimeCalls(f.control, "tab close ") != before || !strings.Contains(strings.Join(view.Blockers, ","), "requires_explicit_retry") {
			t.Fatalf("auto retry was not suppressed: view=%+v err=%v", view, err)
		}
		f.control.closeMutates = true
		f.control.closeOutcome = HerdrMutationResult{Outcome: herdrConfirmed}
		view, err = f.app.reapRuntimeSeat(f.worker.Name, true, false)
		if err != nil || view.LastOutcome != sessionStopped || countRuntimeCalls(f.control, "tab close ") != before+1 {
			t.Fatalf("explicit retry did not converge: view=%+v err=%v", view, err)
		}
	})

	t.Run("ambiguous present is not automatically retried", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		f.control.closeMutates = false
		f.control.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("timeout")}
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err == nil || view.LastOutcome != sessionHibernateUnknown || view.Eligible {
			t.Fatalf("first view=%+v err=%v", view, err)
		}
		before := countRuntimeCalls(f.control, "tab close ")
		view, err = f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err != nil || countRuntimeCalls(f.control, "tab close ") != before || !strings.Contains(strings.Join(view.Blockers, ","), "requires_operator_verification") {
			t.Fatalf("unknown auto retry was not suppressed: view=%+v err=%v", view, err)
		}
	})

	t.Run("durable attempting requires explicit unknown verification", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		if _, err := f.app.appendDerivedSession(f.started, sessionHibernateAttempting, "crashed-reaper", "crash after durable attempting"); err != nil {
			t.Fatal(err)
		}
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err != nil || view.Eligible || countRuntimeCalls(f.control, "tab close ") != 0 || !strings.Contains(strings.Join(view.Blockers, ","), "requires_operator_verification") {
			t.Fatalf("attempting auto retry was not suppressed: view=%+v err=%v", view, err)
		}
		view, err = f.app.reapRuntimeSeat(f.worker.Name, false, true)
		if err != nil || view.LastOutcome != sessionStopped || countRuntimeCalls(f.control, "tab close ") != 1 {
			t.Fatalf("explicit unknown retry did not converge: view=%+v err=%v", view, err)
		}
	})

	t.Run("stopped append crash is reconciled without second close", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		appendCount := 0
		f.sessions.Failpoint = func(name string) error {
			if name == "before_append" {
				appendCount++
				if appendCount == 2 {
					return errors.New("stop append crash")
				}
			}
			return nil
		}
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err == nil || view.LastOutcome != "stopped_record_failed" || countRuntimeCalls(f.control, "tab close ") != 1 {
			t.Fatalf("first view=%+v err=%v", view, err)
		}
		f.sessions.Failpoint = nil
		view, err = f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err != nil || view.RuntimeState != "offline" || countRuntimeCalls(f.control, "tab close ") != 1 {
			t.Fatalf("reconcile view=%+v err=%v", view, err)
		}
	})
}

func TestRuntimeHibernationActionMessageRequiresDurableAck(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.app.CallerPane, f.app.FromGateway = "runtime:manager", true
	if err := f.app.run([]string{"message", "--to", f.worker.Name, "--kind", "request", "--text", "confirm the final evidence", "--delivery", "wakeup"}); err != nil {
		t.Fatal(err)
	}
	ledger, err := f.app.ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	messageID := ""
	for _, record := range ledger.deliveries {
		if record.Origin.Type == "message_prepared" && record.Origin.Message == "confirm the final evidence" {
			messageID = record.Origin.MessageID
		}
	}
	if messageID == "" {
		t.Fatal("missing actionable message")
	}
	view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err != nil || countRuntimeCalls(f.control, "tab close ") != 0 || !strings.Contains(strings.Join(view.Blockers, ","), "unacked_action_message") ||
		!strings.Contains(view.NextAction, "hq message ack --message "+messageID) {
		t.Fatalf("unacked action message did not block reap: view=%+v err=%v", view, err)
	}
	f.app.CallerPane = "runtime:worker"
	if err := f.app.run([]string{"message", "ack", "--message", messageID}); err != nil {
		t.Fatal(err)
	}
	view, err = f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err != nil || view.LastOutcome != sessionStopped || countRuntimeCalls(f.control, "tab close ") != 1 {
		t.Fatalf("acked action message did not release reap: view=%+v err=%v", view, err)
	}
}

func TestRuntimeHibernationDiagnosticAppendFailureKeepsAttemptingRecovery(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	failingSnapshots := &failNthSnapshotControl{HerdrControl: f.app.Herdr, failAt: 2}
	f.app.Herdr = failingSnapshots
	appendCount := 0
	f.sessions.Failpoint = func(name string) error {
		if name == "before_append" {
			appendCount++
			if appendCount == 2 {
				return errors.New("diagnostic append failed")
			}
		}
		return nil
	}
	view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err == nil || view.Eligible || view.LastOutcome != sessionHibernateAttempting || countRuntimeCalls(f.control, "tab close ") != 0 {
		t.Fatalf("diagnostic failure view=%+v err=%v", view, err)
	}
	if !strings.Contains(view.NextAction, "--retry-unknown") || strings.Contains(view.NextAction, "--retry-failed") {
		t.Fatalf("diagnostic failure gave non-durable recovery: %+v", view)
	}
	f.sessions.Failpoint = nil
	events, listErr := f.sessions.List(SessionFilter{Agent: f.worker.Name})
	if listErr != nil || latestSessionDiagnostic(events, f.started.SessionID).Type != sessionHibernateAttempting {
		t.Fatalf("durable diagnostic was not attempting: events=%+v err=%v", events, listErr)
	}
}

func TestRuntimeHibernationRejectsReplacedAgentInSamePane(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.control.mu.Lock()
	f.control.snapshot.Panes[0].TerminalID = "terminal-replacement"
	f.control.snapshot.Agents[0].TerminalID = "terminal-replacement"
	f.control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: f.worker.Name, Kind: f.worker.Kind, Value: "replacement-session"}
	f.control.mu.Unlock()
	view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err != nil || countRuntimeCalls(f.control, "tab close ") != 0 || !strings.Contains(strings.Join(view.Blockers, ","), "missing_session_for_live_incarnation") {
		t.Fatalf("replacement in same pane was not fail-closed: view=%+v err=%v", view, err)
	}
}

func TestRuntimeHibernationDefersReplacementVisibleInFinalSnapshot(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	control := &mutateNthSnapshotControl{HerdrControl: f.app.Herdr, mutateAt: 3, mutate: func() {
		f.control.mu.Lock()
		defer f.control.mu.Unlock()
		f.control.snapshot.Panes[0].TerminalID = "terminal-final-replacement"
		f.control.snapshot.Agents[0].TerminalID = "terminal-final-replacement"
		f.control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: f.worker.Name, Kind: f.worker.Kind, Value: "final-replacement-session"}
	}}
	f.app.Herdr = control
	view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err != nil || view.Eligible || view.LastOutcome != sessionHibernateDeferred || countRuntimeCalls(f.control, "tab close ") != 0 {
		t.Fatalf("replacement visible in final snapshot was not fail-closed: view=%+v err=%v", view, err)
	}
}

func TestRuntimeRetryFlagsRequireSingleSeat(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.app.MaintenanceActor = "penny"
	for _, flag := range []string{"--retry-failed", "--retry-unknown"} {
		err := f.app.cmdRuntime([]string{"reap", flag})
		if err == nil || !strings.Contains(err.Error(), "单一 --agent") {
			t.Fatalf("flag=%s err=%v", flag, err)
		}
	}
	if countRuntimeCalls(f.control, "tab close ") != 0 {
		t.Fatal("rejected bulk retry called CloseTab")
	}
}

func TestRuntimeReapCommandOutputsFailedReportAndReturnsNonzero(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.control.closeMutates = false
	f.control.closeOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("close rejected")}
	f.app.MaintenanceActor = "penny"
	f.app.JSON = true
	var out bytes.Buffer
	f.app.Out = &out
	err := f.app.cmdRuntime([]string{"reap", "--agent", f.worker.Name})
	if err == nil {
		t.Fatal("explicit runtime reap reported success after CloseTab failure")
	}
	var report RuntimeReapReport
	if decodeErr := json.Unmarshal(out.Bytes(), &report); decodeErr != nil || len(report.Seats) != 1 {
		t.Fatalf("runtime reap did not emit report before nonzero: json=%s err=%v", out.String(), decodeErr)
	}
	if report.Seats[0].Eligible || report.Seats[0].LastOutcome != sessionHibernateFailed || !strings.Contains(strings.Join(report.Seats[0].Blockers, ","), "reap_error") {
		t.Fatalf("failed runtime report is inconsistent: %+v", report.Seats[0])
	}
}

func TestRuntimeHibernationPolicyAndLiveStatusBlockersNeverClose(t *testing.T) {
	t.Run("working", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		f.control.mu.Lock()
		f.control.snapshot.Agents[0].Status = "working"
		f.control.mu.Unlock()
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err != nil || view.Eligible || countRuntimeCalls(f.control, "tab close ") != 0 || !strings.Contains(strings.Join(view.Blockers, ","), "runtime_working") {
			t.Fatalf("working runtime was not protected: view=%+v err=%v", view, err)
		}
	})
	t.Run("always", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		updated, err := mutateConfig(f.e.config, func(cfg *Config) error {
			for index := range cfg.Agents {
				if cfg.Agents[index].Name == f.worker.Name {
					cfg.Agents[index].ActivationPolicy = activationAlways
					cfg.Agents[index].KeepWarm = ""
					finalizeTestSeatMutation(&cfg.Agents[index])
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		f.app.Config = updated
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		if err != nil || view.Eligible || countRuntimeCalls(f.control, "tab close ") != 0 || !strings.Contains(strings.Join(view.Blockers, ","), "policy_always_never_auto_stops") {
			t.Fatalf("always runtime was not protected: view=%+v err=%v", view, err)
		}
	})
}

func TestRuntimeHibernationReconcilesDeadAgentWithEmptyTab(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.control.mu.Lock()
	f.control.snapshot.Agents = nil // tab/pane remain as a patrol-visible orphan
	f.control.mu.Unlock()
	view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err != nil || view.RuntimeState != "offline" || view.Eligible || countRuntimeCalls(f.control, "tab close ") != 0 ||
		!strings.Contains(strings.Join(view.Blockers, ","), "orphan_tab_without_agent") || !strings.Contains(view.NextAction, "hq patrol") {
		t.Fatalf("dead runtime reconcile=%+v err=%v", view, err)
	}
	events, err := f.sessions.List(SessionFilter{Agent: f.worker.Name})
	if err != nil || len(activeSessionStarts(events)) != 0 {
		t.Fatalf("dead runtime session still active: %+v err=%v", events, err)
	}
	view, err = f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err != nil || !strings.Contains(strings.Join(view.Blockers, ","), "orphan_tab_without_agent") || !strings.Contains(view.NextAction, "hq patrol") {
		t.Fatalf("persistent orphan evidence disappeared on second scan: view=%+v err=%v", view, err)
	}
}

func TestRuntimeSeatLockDomainIsIndependentAndCancellable(t *testing.T) {
	e := setupTestEnv(t)
	store := NewStore(e.data)
	wouldBeRuntimeStripe := operationLockStripe("runtime-seat", "eng-developer")
	collisionID := ""
	for index := 0; index < 10000; index++ {
		candidate := fmt.Sprintf("LOCK-COLLISION-%d", index)
		if operationLockStripe(operationScopeDelivery, candidate) == wouldBeRuntimeStripe {
			collisionID = candidate
			break
		}
	}
	if collisionID == "" {
		t.Fatal("failed to synthesize old-domain stripe collision")
	}
	heldOperation, err := store.lockOperation(operationScopeDelivery, collisionID)
	if err != nil {
		t.Fatal(err)
	}
	releaseSeat, err := store.lockRuntimeSeat("eng-developer")
	if err != nil {
		heldOperation()
		t.Fatalf("operation -> independent runtime seat deadlocked: %v", err)
	}
	releaseSeat()
	heldOperation()

	held, err := store.lockRuntimeSeat("eng-developer")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = store.withRequestContext(ctx).lockRuntimeSeat("eng-developer")
	if !errors.Is(err, context.DeadlineExceeded) {
		held()
		t.Fatalf("same-seat waiter ignored context cancellation: %v", err)
	}
	other, err := store.lockRuntimeSeat("eng-data-engineer")
	if err != nil {
		held()
		t.Fatalf("unrelated seat was blocked: %v", err)
	}
	other()
	held()
}

func TestRuntimeSeatLockHelperProcess(t *testing.T) {
	if os.Getenv("HQ_RUNTIME_SEAT_LOCK_HELPER") != "1" {
		return
	}
	store := NewStore(os.Getenv("HQ_RUNTIME_SEAT_LOCK_DIR"))
	release, err := store.lockRuntimeSeat("eng-developer")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := os.Stdout.WriteString("LOCKED\n"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(os.Stdin)
}

func TestRuntimeSeatLockUsesCrossProcessFlock(t *testing.T) {
	e := setupTestEnv(t)
	command := exec.Command(os.Args[0], "-test.run=^TestRuntimeSeatLockHelperProcess$")
	command.Env = append(os.Environ(), "HQ_RUNTIME_SEAT_LOCK_HELPER=1", "HQ_RUNTIME_SEAT_LOCK_DIR="+e.data)
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = stdin.Close()
			_ = command.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "LOCKED\n" {
		t.Fatalf("helper did not acquire lock: line=%q err=%v", line, err)
	}
	store := NewStore(e.data)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := store.withRequestContext(ctx).lockRuntimeSeat("eng-developer"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same seat ignored cross-process flock: %v", err)
	}
	other, err := store.lockRuntimeSeat("eng-data-engineer")
	if err != nil {
		t.Fatalf("unrelated seat blocked cross-process: %v", err)
	}
	other()
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	closed = true
}

func TestSessionListSharedLockHelperProcess(t *testing.T) {
	if os.Getenv("HQ_SESSION_PARTIAL_APPEND_HELPER") != "1" {
		return
	}
	payload, err := base64.StdEncoding.DecodeString(os.Getenv("HQ_SESSION_APPEND_PAYLOAD"))
	if err != nil {
		t.Fatal(err)
	}
	root := os.Getenv("HQ_SESSION_APPEND_ROOT")
	month := os.Getenv("HQ_SESSION_APPEND_MONTH")
	lock, err := os.OpenFile(filepath.Join(root, ".session.lock"), os.O_RDWR|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(filepath.Join(root, month), os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		t.Fatal(err)
	}
	half := len(payload) / 2
	if _, err := file.Write(payload[:half]); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stdout.WriteString("PARTIAL\n"); err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(os.Stdin)
	if _, err := file.Write(append(payload[half:], '\n')); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}

func TestSessionListUsesCrossProcessSharedFlock(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	created := HerdrTabCreated{
		Tab:  HerdrTab{ID: "w-test:t-partial", WorkspaceID: "w-test", Label: rosterTabLabel(f.worker), CWD: testAgentCWD(f.cfg, f.e.root, f.worker.Name)},
		Pane: HerdrPane{ID: "w-test:p-partial", WorkspaceID: "w-test", TabID: "w-test:t-partial", CWD: testAgentCWD(f.cfg, f.e.root, f.worker.Name)},
	}
	event, err := newSessionEvent(time.Now(), sessionStarted, created, "w-test", f.worker, "helper", "cross-process partial append", testAgentCWD(f.cfg, f.e.root, f.worker.Name))
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(os.Args[0], "-test.run=^TestSessionListSharedLockHelperProcess$")
	command.Env = append(os.Environ(),
		"HQ_SESSION_PARTIAL_APPEND_HELPER=1",
		"HQ_SESSION_APPEND_ROOT="+f.sessions.Root,
		"HQ_SESSION_APPEND_MONTH="+time.Now().UTC().Format("2006-01")+".jsonl",
		"HQ_SESSION_APPEND_PAYLOAD="+base64.StdEncoding.EncodeToString(payload))
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			_ = stdin.Close()
			_ = command.Wait()
		}
	})
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "PARTIAL\n" {
		t.Fatalf("helper did not expose partial append: line=%q err=%v", line, err)
	}
	type listResult struct {
		events []SessionEvent
		err    error
	}
	done := make(chan listResult, 1)
	go func() {
		events, listErr := f.sessions.List(SessionFilter{Agent: f.worker.Name})
		done <- listResult{events: events, err: listErr}
	}()
	select {
	case result := <-done:
		t.Fatalf("List read a cross-process partial JSONL instead of waiting for LOCK_SH: events=%+v err=%v", result.events, result.err)
	case <-time.After(75 * time.Millisecond):
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	result := <-done
	if result.err != nil || len(result.events) != 2 {
		t.Fatalf("List did not replay complete append after shared lock: events=%+v err=%v", result.events, result.err)
	}
	if err := command.Wait(); err != nil {
		t.Fatal(err)
	}
	closed = true
}

func TestGatewayRuntimeReaperScansImmediatelyAndStopsWithContext(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.app.RuntimeReaperInterval = time.Hour
	f.app.Err = io.Discard
	socket, err := gatewaySocketPath(f.e.data)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(f.e.data, 0o755); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.app.serveGateway(ctx, listener, "w-test", "runtime-reaper-test") }()
	deadline := time.Now().Add(2 * time.Second)
	for countRuntimeCalls(f.control, "tab close ") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if countRuntimeCalls(f.control, "tab close ") != 1 {
		cancel()
		t.Fatal("gateway reaper waited for first one-hour tick instead of startup compensation scan")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("serve result=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("gateway/reaper leaked after context cancellation")
	}
}

func TestRuntimeSeatLeaseSerializesPromptAndReap(t *testing.T) {
	t.Run("reap wins then issue cold resumes", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		baseStore := f.app.Store.(*Store)
		tracking := &trackingRuntimeSeatStore{Store: baseStore, notifyAt: 2, notify: make(chan struct{})}
		f.app.Store = tracking
		control := &blockingRuntimeCloseControl{fakeHerdrControl: f.control, entered: make(chan struct{}), release: make(chan struct{})}
		f.app.Herdr, f.app.Transport = control, herdrDeliveryTransport{Control: control}
		reapDone := make(chan error, 1)
		go func() {
			_, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
			reapDone <- err
		}()
		select {
		case <-control.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("reaper did not reach CloseTab")
		}
		f.app.CallerPane, f.app.FromGateway = "runtime:manager", true
		source := writeTestFile(t, filepath.Join(f.e.root, "engineering", "reap-race-issue.md"), "# race\n")
		if err := f.app.run([]string{"case", "create", "--id", "RUNTIME-RACE-REAP", "--parent", "RUNTIME-HIBERNATE-001", "--title", "race", "--source", source}); err != nil {
			t.Fatal(err)
		}
		issueDone := make(chan error, 1)
		go func() {
			issueDone <- f.app.run([]string{"issue", "--case", "RUNTIME-RACE-REAP", "--to", f.worker.Name, "--next", "resume after reap"})
		}()
		select {
		case <-tracking.notify:
		case <-time.After(2 * time.Second):
			t.Fatal("issue did not reach the pre-origin runtime-seat fence")
		}
		events, err := f.app.Store.ReadAll(f.cfg)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == "issue_prepared" && event.CaseID == "RUNTIME-RACE-REAP" {
				t.Fatalf("issue origin/WIP was written before the old close converged: %+v", event)
			}
		}
		close(control.release)
		if err := <-reapDone; err != nil {
			t.Fatal(err)
		}
		if err := <-issueDone; err != nil {
			t.Fatal(err)
		}
		if countRuntimeCalls(f.control, "tab close ") != 1 || countRuntimeCalls(f.control, "agent start "+f.worker.Name) != 1 {
			t.Fatalf("reap-first issue did not cold-resume safely: calls=%v", f.control.calls)
		}
	})

	t.Run("prompt wins and reaper rechecks active work", func(t *testing.T) {
		f := prepareEligibleRuntimeSeat(t)
		baseStore := f.app.Store.(*Store)
		tracking := &trackingRuntimeSeatStore{Store: baseStore, notifyAt: 2, notify: make(chan struct{})}
		f.app.Store = tracking
		control := &blockingRuntimePromptControl{fakeHerdrControl: f.control, entered: make(chan struct{}), release: make(chan struct{})}
		f.app.Herdr, f.app.Transport = control, herdrDeliveryTransport{Control: control}
		f.app.CallerPane, f.app.FromGateway = "runtime:manager", true
		source := writeTestFile(t, filepath.Join(f.e.root, "engineering", "prompt-race-issue.md"), "# race\n")
		if err := f.app.run([]string{"case", "create", "--id", "RUNTIME-RACE-PROMPT", "--parent", "RUNTIME-HIBERNATE-001", "--title", "race", "--source", source}); err != nil {
			t.Fatal(err)
		}
		issueDone := make(chan error, 1)
		go func() {
			issueDone <- f.app.run([]string{"issue", "--case", "RUNTIME-RACE-PROMPT", "--to", f.worker.Name, "--next", "hold prompt"})
		}()
		select {
		case <-control.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("issue did not reach Prompt while holding seat")
		}
		reapDone := make(chan struct {
			view RuntimeSeatView
			err  error
		}, 1)
		go func() {
			view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
			reapDone <- struct {
				view RuntimeSeatView
				err  error
			}{view, err}
		}()
		select {
		case <-tracking.notify:
		case <-time.After(2 * time.Second):
			t.Fatal("reaper did not wait on the same runtime-seat lease")
		}
		close(control.release)
		if err := <-issueDone; err != nil {
			t.Fatal(err)
		}
		result := <-reapDone
		if result.err != nil || countRuntimeCalls(f.control, "tab close ") != 0 || !strings.Contains(strings.Join(result.view.Blockers, ","), "active_assignment") {
			t.Fatalf("prompt-first reaper did not recheck durable work: view=%+v err=%v calls=%v", result.view, result.err, f.control.calls)
		}
	})
}

func TestRuntimeUnknownCloseFencesNewIssueBeforeOriginAndPrompt(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	f.control.closeMutates = false
	f.control.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("request accepted but response timed out")}
	view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
	if err == nil || view.LastOutcome != sessionHibernateUnknown || countRuntimeCalls(f.control, "tab close ") != 1 {
		t.Fatalf("failed to establish ambiguous close: view=%+v err=%v", view, err)
	}
	f.app.CallerPane, f.app.FromGateway = "runtime:manager", true
	source := writeTestFile(t, filepath.Join(f.e.root, "engineering", "unknown-close-next-issue.md"), "# next work\n")
	if err := f.app.run([]string{"case", "create", "--id", "RUNTIME-UNKNOWN-NEXT", "--parent", "RUNTIME-HIBERNATE-001", "--title", "next after ambiguous close", "--source", source}); err != nil {
		t.Fatal(err)
	}
	if err := f.app.run([]string{"message", "--to", f.worker.Name, "--kind", "info", "--text", "durable info may wait through close recovery", "--delivery", "inject"}); err != nil {
		t.Fatalf("non-wakeup informational queue was incorrectly fenced by hibernate_unknown: %v", err)
	}
	promptsBefore := countRuntimeCalls(f.control, "prompt "+f.worker.Name+" ")
	err = f.app.run([]string{"issue", "--case", "RUNTIME-UNKNOWN-NEXT", "--to", f.worker.Name, "--next", "must wait for close convergence"})
	if err == nil || !strings.Contains(err.Error(), "尚未写入新业务 origin/WIP") || !strings.Contains(err.Error(), "--retry-unknown") {
		t.Fatalf("new issue did not fail closed before origin: %v", err)
	}
	if countRuntimeCalls(f.control, "prompt "+f.worker.Name+" ") != promptsBefore {
		t.Fatal("ambiguous close fence allowed a new Prompt")
	}
	events, readErr := f.app.Store.ReadAll(f.cfg)
	if readErr != nil {
		t.Fatal(readErr)
	}
	for _, event := range events {
		if event.CaseID == "RUNTIME-UNKNOWN-NEXT" && event.Type == "issue_prepared" {
			t.Fatalf("ambiguous close fence wrote issue origin/WIP: %+v", event)
		}
	}

	// Simulate the old request completing after its client already observed an
	// ambiguous result. Once absence is observable, admission records stopped,
	// writes the issue, cold-resumes a new exact incarnation and prompts it.
	f.control.mu.Lock()
	f.control.snapshot.Tabs = nil
	f.control.snapshot.Panes = nil
	f.control.snapshot.Agents = nil
	f.control.closeOutcome = HerdrMutationResult{Outcome: herdrConfirmed}
	f.control.closeMutates = true
	f.control.mu.Unlock()
	if err := f.app.run([]string{"issue", "--case", "RUNTIME-UNKNOWN-NEXT", "--to", f.worker.Name, "--next", "must wait for close convergence"}); err != nil {
		t.Fatal(err)
	}
	if got := countRuntimeCalls(f.control, "prompt "+f.worker.Name+" ") - promptsBefore; got != 2 {
		t.Fatalf("safe retry should emit startup+issue prompts to the new incarnation, got=%d calls=%v", got, f.control.calls)
	}
	if got := countRuntimeCalls(f.control, "tab close "); got != 1 {
		t.Fatalf("safe issue retry repeated old CloseTab: %d", got)
	}
}

func TestRuntimeReaperFinalRegistryLeaseRejectsOnAssignmentToAlwaysRace(t *testing.T) {
	f := prepareEligibleRuntimeSeat(t)
	baseStore := f.app.Store.(*Store)
	barrier := &blockingFinalRegistryLeaseStore{Store: baseStore, entered: make(chan struct{}), release: make(chan struct{})}
	f.app.Store = barrier
	done := make(chan struct {
		view RuntimeSeatView
		err  error
	}, 1)
	go func() {
		view, err := f.app.reapRuntimeSeat(f.worker.Name, false, false)
		done <- struct {
			view RuntimeSeatView
			err  error
		}{view, err}
	}()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("reaper did not reach final cross-process registry lease")
	}
	_, err := mutateConfig(f.e.config, func(cfg *Config) error {
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == f.worker.Name {
				cfg.Agents[index].ActivationPolicy = activationAlways
				cfg.Agents[index].KeepWarm = ""
				finalizeTestSeatMutation(&cfg.Agents[index])
			}
		}
		return nil
	})
	if err != nil {
		close(barrier.release)
		t.Fatal(err)
	}
	close(barrier.release)
	result := <-done
	if result.err != nil || result.view.LastOutcome != sessionHibernateDeferred || result.view.Eligible || countRuntimeCalls(f.control, "tab close ") != 0 {
		t.Fatalf("stale on_assignment config closed new always seat: view=%+v err=%v", result.view, result.err)
	}
}
