package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func setWatchedProfile(t *testing.T, path, name, model, effort string) Config {
	t.Helper()
	cfg, err := mutateConfig(path, func(cfg *Config) error {
		policy := cfg.RuntimeProfiles["codex"]
		if policy.Employees == nil {
			policy.Employees = map[string]EmployeeRuntimeProfile{}
		}
		policy.Employees[name] = EmployeeRuntimeProfile{Model: model, ReasoningEffort: effort}
		cfg.RuntimeProfiles["codex"] = policy
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func healthyWatchedProfile(t *testing.T, status string) (testEnv, AgentRule, *App, *profileReaderControl) {
	t.Helper()
	e, _, rule, app, reader, _ := profileRepairFixture(t, status)
	reader.terminal = []byte("gpt-5.6-sol medium · 100% left · ~/work\n› Ask Codex to do anything\n")
	reader.afterStart = []byte("review-model high · 100% left · ~/work\n› Ask Codex to do anything\n")
	app.ConfigAccess = &sync.RWMutex{}
	app.Err = io.Discard
	return e, rule, app, reader
}

func TestConfigWatcherWorkingSaveIdleRecoveryAndIdempotency(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "working")
	controller := &configProfileController{}
	app.reconcileConfigProfiles(context.Background(), controller)
	if len(controller.pending) != 0 {
		t.Fatalf("healthy seats pending: %v", controller.pending)
	}
	app.Config = setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	app.reconcileConfigProfiles(context.Background(), controller)
	if !controller.pending[rule.Name] || countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 0 {
		t.Fatal("working employee interrupted or update lost")
	}
	status := app.employeeRuntimeProfileStatus(context.Background(), rule)
	if status.State != "waiting_idle" || status.Observed.Model != "gpt-5.6-sol" {
		t.Fatalf("status=%+v", status)
	}
	reader.fakeHerdrControl.mu.Lock()
	reader.fakeHerdrControl.snapshot.Agents[0].Status = "idle"
	reader.fakeHerdrControl.mu.Unlock()
	app.reconcileConfigProfiles(context.Background(), controller)
	status = app.employeeRuntimeProfileStatus(context.Background(), rule)
	if status.State != "applied" || status.Observed.Model != "review-model" || status.Observed.ReasoningEffort != "high" {
		t.Fatalf("status=%+v", status)
	}
	if controller.pending[rule.Name] || countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 1 {
		t.Fatal("did not converge exactly once")
	}
	// Equivalent atomic saves and repeated callbacks must not restart a seat.
	setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	for i := 0; i < 3; i++ {
		app.reconcileConfigProfiles(context.Background(), controller)
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 1 {
		t.Fatal("duplicate save restarted employee")
	}
	events, err := app.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil {
		t.Fatal(err)
	}
	sent := 0
	for _, event := range events {
		if event.Type == sessionProfileRecoverySent {
			sent++
		}
	}
	if sent != 1 {
		t.Fatalf("recovery envelopes=%d", sent)
	}
}

func TestConfigWatcherRejectsInvalidAndCoalescesLatestDesired(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "working")
	controller := &configProfileController{}
	app.reconcileConfigProfiles(context.Background(), controller)
	setWatchedProfile(t, e.config, rule.Name, "obsolete-model", "low")
	app.reconcileConfigProfiles(context.Background(), controller)
	valid := setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	raw, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	app.Err = &log
	if err := os.WriteFile(e.config, []byte("version: 3\nunknown_typo: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		app.reconcileConfigProfiles(context.Background(), controller)
	}
	if strings.Count(log.String(), "配置无效") != 1 || !strings.Contains(log.String(), "hq doctor --json") {
		t.Fatalf("error not corrective/deduped: %s", log.String())
	}
	if controller.desired[rule.Name].Model != "obsolete-model" || countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 0 {
		t.Fatal("invalid config changed desired/runtime")
	}
	if err := os.WriteFile(e.config, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	reader.fakeHerdrControl.mu.Lock()
	reader.fakeHerdrControl.snapshot.Agents[0].Status = "idle"
	reader.fakeHerdrControl.mu.Unlock()
	app.reconcileConfigProfiles(context.Background(), controller)
	app.Config = valid
	status := app.employeeRuntimeProfileStatus(context.Background(), rule)
	if status.State != "applied" || !strings.Contains(log.String(), "配置校验已恢复") {
		t.Fatalf("did not recover: %+v %s", status, log.String())
	}
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if len(reader.startProfiles) != 1 || strings.Contains(strings.Join(reader.startProfiles[0], " "), "obsolete-model") {
		t.Fatalf("obsolete generation started: %v", reader.startProfiles)
	}
}

func TestConfigWatcherDiffOnlyAffectedEmployees(t *testing.T) {
	cfg := testConfig()
	cfg.RuntimeProfiles = configuredCodexProfile()
	c := &configProfileController{}
	c.accept(cfg)
	c.pending = map[string]bool{}
	name := cfg.Agents[0].Name
	policy := cfg.RuntimeProfiles["codex"]
	policy.Employees = map[string]EmployeeRuntimeProfile{name: {Model: "review-model", ReasoningEffort: "high"}}
	cfg.RuntimeProfiles["codex"] = policy
	c.accept(cfg)
	if len(c.pending) != 1 || !c.pending[name] {
		t.Fatalf("unrelated employees scheduled: %v", c.pending)
	}
	c.pending = map[string]bool{}
	policy.Model = "new-default"
	cfg.RuntimeProfiles["codex"] = policy
	c.accept(cfg)
	if c.pending[name] || len(c.pending) == 0 {
		t.Fatalf("default change ignores inheritance: %v", c.pending)
	}
	delete(policy.Employees, name)
	cfg.RuntimeProfiles["codex"] = policy
	c.accept(cfg)
	if !c.pending[name] || c.desired[name].Model != "new-default" {
		t.Fatal("removed override did not restore company default")
	}
	cfg.RuntimeProfiles = nil
	c.accept(cfg)
	if len(c.pending) != 0 {
		t.Fatal("removed policy still queued")
	}
}

func TestConfigWatcherOfflineReportAndBlockedDoNotRestart(t *testing.T) {
	for _, mode := range []string{"offline", "report", "blocked"} {
		t.Run(mode, func(t *testing.T) {
			e, rule, app, reader := healthyWatchedProfile(t, "blocked")
			app.Config = setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
			want := "waiting_idle"
			if mode == "offline" {
				reader.fakeHerdrControl.snapshot.Agents = nil
				reader.fakeHerdrControl.snapshot.Tabs = nil
				reader.fakeHerdrControl.snapshot.Panes = nil
				want = "next_activation"
			}
			if mode == "report" {
				var err error
				app.Config, err = mutateConfig(e.config, func(cfg *Config) error {
					p := cfg.RuntimeProfiles["codex"]
					p.OnDrift = "report"
					cfg.RuntimeProfiles["codex"] = p
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				want = "report_only"
			}
			app.reconcileConfigProfiles(context.Background(), &configProfileController{})
			status := app.employeeRuntimeProfileStatus(context.Background(), rule)
			if status.State != want {
				t.Fatalf("got=%+v want=%s", status, want)
			}
			if countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 0 || countRuntimeCalls(reader.fakeHerdrControl, "agent start ") != 0 {
				t.Fatal("unwanted runtime mutation")
			}
		})
	}
}

func TestConfigWatcherDebounceFilterOverflowAndCancellation(t *testing.T) {
	app := &App{ConfigPath: "/config/company.yaml", Err: io.Discard}
	events, failures, wake := make(chan fsnotify.Event), make(chan error), make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); app.watchConfigEvents(ctx, events, failures, wake, 20*time.Millisecond) }()
	defer func() { cancel(); <-done }()
	events <- fsnotify.Event{Name: "/config/unrelated", Op: fsnotify.Write}
	events <- fsnotify.Event{Name: app.ConfigPath, Op: fsnotify.Chmod}
	select {
	case <-wake:
		t.Fatal("irrelevant event woke worker")
	case <-time.After(35 * time.Millisecond):
	}
	for i := 0; i < 10; i++ {
		events <- fsnotify.Event{Name: app.ConfigPath, Op: fsnotify.Create | fsnotify.Rename}
	}
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("no debounced wake")
	}
	select {
	case <-wake:
		t.Fatal("duplicate debounced wake")
	case <-time.After(35 * time.Millisecond):
	}
	failures <- fsnotify.ErrEventOverflow
	select {
	case <-wake:
	case <-time.After(time.Second):
		t.Fatal("overflow not reconciled")
	}
	// Closed notification channels must not spin or prevent cancellation.
	close(events)
	close(failures)
}

func TestConfigWatcherRealDirectoryAtomicReplacement(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "idle")
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close()
	if err := watcher.Add(filepath.Dir(e.config)); err != nil {
		t.Fatal(err)
	}
	wake := make(chan struct{}, 1)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		app.watchConfigEvents(ctx, watcher.Events, watcher.Errors, wake, 20*time.Millisecond)
	}()
	go func() { defer workers.Done(); app.runConfigProfileWorker(ctx, wake, time.Hour, time.Hour) }()
	defer func() { cancel(); workers.Wait() }()
	deadline := time.Now().Add(time.Second)
	for countRuntimeCalls(reader.fakeHerdrControl, "snapshot") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "snapshot") == 0 {
		t.Fatal("initial scan did not start")
	}
	// The real writer replaces config.yaml through rename. No periodic scan
	// can rescue a broken file watcher in this test (both ticks are one hour).
	setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if countRuntimeCalls(reader.fakeHerdrControl, "prompt ") > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	workers.Wait()
	if countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 1 || countRuntimeCalls(reader.fakeHerdrControl, "agent start ") != 1 {
		t.Fatal("atomic replacement not applied")
	}
}

func TestConfigWatcherPollingRecoversMissedNotification(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "idle")
	wake := make(chan struct{}) // no notifications at all
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); app.runConfigProfileWorker(ctx, wake, 30*time.Millisecond, time.Hour) }()
	defer func() { cancel(); <-done }()
	// Give the first scan time to observe the original healthy generation.
	deadline := time.Now().Add(time.Second)
	for countRuntimeCalls(reader.fakeHerdrControl, "snapshot") == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	deadline = time.Now().Add(3 * time.Second)
	for countRuntimeCalls(reader.fakeHerdrControl, "agent start ") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "agent start ") != 1 {
		t.Fatal("poll did not recover missed notification")
	}
}

func TestStaffGetLiveShowsObservedWithoutMutating(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "working")
	app.Config = setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	var out bytes.Buffer
	app.JSON, app.Out = true, &out
	if err := app.cmdStaffGet([]string{"--name", rule.Name, "--live"}); err != nil {
		t.Fatal(err)
	}
	var view staffCapacityView
	if err := json.Unmarshal(out.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.ExpectedRuntimeProfile.Model != "review-model" || view.RuntimeProfileStatus.State != "waiting_idle" || view.RuntimeProfileStatus.Observed.Model != "gpt-5.6-sol" {
		t.Fatalf("misleading view: %s", out.String())
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 0 {
		t.Fatal("read mutated runtime")
	}
	out.Reset()
	app.Herdr = nil
	if err := app.cmdStaffGet([]string{"--name", rule.Name}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "not_checked") || !strings.Contains(out.String(), "--live") {
		t.Fatalf("offline view claims applied: %s", out.String())
	}
}

func TestConfigWatcherActiveAssignmentSurvivesModelReplacement(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "idle")
	issuer, ok := app.Config.exactRule(rule.ReportsTo)
	if !ok {
		t.Fatal("fixture needs a direct manager")
	}
	e.setActor(t, issuer.Name, "watcher-test:issuer", testAgentCWD(app.Config, e.root, issuer.Name))
	source := writeTestFile(t, filepath.Join(e.root, issuer.Department, "watcher-source.md"), "Review the supplied evidence; preserve the acceptance contract.")
	runTestCommand(t, e, "case", "create", "--id", "WATCHER-ACTIVE", "--title", "model upgrade during work", "--source", source)
	decision := writeIssueApproval(t, e, "watcher-approval.md", "watcher-approval", "WATCHER-ACTIVE", source, rule.Name)
	runTestCommand(t, e, "issue", "--case", "WATCHER-ACTIVE", "--to", rule.Name, "--decision", decision, "--next", "review evidence")
	before, err := app.Store.ReadAll(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	issue := latestEventOfType(before, "issue_sent")
	e.setActor(t, rule.Name, "watcher-test:worker", testAgentCWD(app.Config, e.root, rule.Name))
	runTestCommand(t, e, "accept", "--event", issue.ID, "--next", "review in progress")
	before, err = app.Store.ReadAll(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	app.Config = setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	app.reconcileConfigProfiles(context.Background(), &configProfileController{})
	after, err := app.Store.ReadAll(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("profile change mutated business ledger")
	}
	reader.fakeHerdrControl.mu.Lock()
	calls := strings.Join(reader.fakeHerdrControl.calls, "\n")
	reader.fakeHerdrControl.mu.Unlock()
	if !strings.Contains(calls, "ACTIVE_ASSIGNMENT") || !strings.Contains(calls, "WATCHER-ACTIVE") {
		t.Fatalf("task missing from recovery envelope: %s", calls)
	}
	if app.employeeRuntimeProfileStatus(context.Background(), rule).State != "applied" {
		t.Fatal("new model not verified")
	}
}

func TestConfigWatcherUnknownCloseRemainsFailClosed(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "idle")
	app.Config = setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	reader.closeMutates = false
	reader.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("test timeout")}
	c := &configProfileController{}
	for i := 0; i < 3; i++ {
		app.reconcileConfigProfiles(context.Background(), c)
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 1 || countRuntimeCalls(reader.fakeHerdrControl, "agent start ") != 0 {
		t.Fatal("ambiguous close was blindly retried")
	}
	status := app.employeeRuntimeProfileStatus(context.Background(), rule)
	if status.State != "repair_unknown" || !c.pending[rule.Name] {
		t.Fatalf("unknown incorrectly settled: %+v", status)
	}
}

func TestConfigWatcherCanceledConfigWaitDoesNotLeak(t *testing.T) {
	_, _, app, _ := healthyWatchedProfile(t, "idle")
	app.ConfigAccess.Lock()
	defer app.ConfigAccess.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); app.runConfigWatcher(ctx) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher leaked while waiting for configuration lease")
	}
}

func TestStaffGetLiveCLIContract(t *testing.T) {
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	cmd, _, err := root.Find([]string{"staff", "get"})
	if err != nil {
		t.Fatal(err)
	}
	if !isDependencyFreeReadOnlyCommand("staff get", cmd) {
		t.Fatal("offline staff get needs Herdr")
	}
	if err := cmd.Flags().Set("live", "true"); err != nil {
		t.Fatal(err)
	}
	if isDependencyFreeReadOnlyCommand("staff get", cmd) {
		t.Fatal("--live did not construct Herdr")
	}
	var out, errOut bytes.Buffer
	err = execute([]string{"staff", "get", "--live"}, &out, &errOut)
	if err == nil || !strings.Contains(err.Error(), "--name") || !strings.Contains(err.Error(), "hq staff get --help") {
		t.Fatalf("missing name not self-correcting before environment access: %v", err)
	}
}

func TestConfigWatcherAndMaintenanceSerializeSameSeat(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "idle")
	app.Config = setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make(chan error, 1)
	wg.Add(2)
	go func() { defer wg.Done(); app.reconcileConfigProfiles(ctx, &configProfileController{}) }()
	go func() {
		defer wg.Done()
		results <- app.runRuntimeMaintenanceStep(ctx, func(c *App) error { return c.recoverRuntimeProfileDriftsOnce(ctx) })
	}()
	wg.Wait()
	if err := <-results; err != nil {
		t.Fatal(err)
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 1 || countRuntimeCalls(reader.fakeHerdrControl, "agent start ") != 1 || countRuntimeCalls(reader.fakeHerdrControl, "prompt ") != 1 {
		t.Fatal("watcher and maintenance duplicated runtime replacement/recovery")
	}
}

func TestConfigWatcherPendingRetryWithoutAnotherSave(t *testing.T) {
	e, rule, app, reader := healthyWatchedProfile(t, "working")
	setWatchedProfile(t, e.config, rule.Name, "review-model", "high")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.runConfigProfileWorker(ctx, make(chan struct{}), time.Hour, 30*time.Millisecond)
	}()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(time.Second)
	for countRuntimeCalls(reader.fakeHerdrControl, "snapshot") < 4 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "tab close ") != 0 {
		t.Fatal("interrupted working employee")
	}
	reader.fakeHerdrControl.mu.Lock()
	reader.fakeHerdrControl.snapshot.Agents[0].Status = "idle"
	reader.fakeHerdrControl.mu.Unlock()
	deadline = time.Now().Add(3 * time.Second)
	for countRuntimeCalls(reader.fakeHerdrControl, "prompt ") == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if countRuntimeCalls(reader.fakeHerdrControl, "prompt ") != 1 {
		t.Fatal("pending update required a second save to resume")
	}
}

func TestStaffGetLiveRestoredProfileClearsHistoricalFailedState(t *testing.T) {
	_, rule, app, _ := healthyWatchedProfile(t, "idle")
	events, err := app.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.appendDerivedSession(events[0], sessionProfileRepairFailed, "hq-runtime-profile", "definitely-not-run; prior desired profile was reverted"); err != nil {
		t.Fatal(err)
	}
	status := app.employeeRuntimeProfileStatus(context.Background(), rule)
	if status.State != "applied" {
		t.Fatalf("historical failed attempt masked matching runtime: %+v", status)
	}
}
