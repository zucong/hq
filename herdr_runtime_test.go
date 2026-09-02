package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type fakeHerdrControl struct {
	mu             sync.Mutex
	root           string
	snapshot       HerdrSnapshot
	snapshots      []HerdrSnapshot
	calls          []string
	lastEnv        map[string]string
	lastAgentEnv   map[string]string
	nextID         int
	createOutcome  HerdrMutationResult
	createMutates  bool
	startOutcome   HerdrMutationResult
	startOutcomes  map[string]HerdrMutationResult
	startMutates   bool
	closeOutcome   HerdrMutationResult
	closeMutates   bool
	promptOutcome  HerdrMutationResult
	promptWakes    bool
	runPaneOutcome HerdrMutationResult
	onRunPane      func(string, string)
	delay          time.Duration
}

type blockingNthPromptControl struct {
	*fakeHerdrControl
	mu       sync.Mutex
	blockOn  int
	attempts int
}

func (c *blockingNthPromptControl) Prompt(ctx context.Context, target, message string) HerdrMutationResult {
	c.mu.Lock()
	c.attempts++
	attempt := c.attempts
	c.mu.Unlock()
	if attempt == c.blockOn {
		<-ctx.Done()
		return HerdrMutationResult{Outcome: herdrAmbiguous, Err: ctx.Err()}
	}
	return c.fakeHerdrControl.Prompt(ctx, target, message)
}

type timeoutNthPromptControl struct {
	*fakeHerdrControl
	mu        sync.Mutex
	timeout   time.Duration
	timeoutOn int
	attempts  int
}

func (c *timeoutNthPromptControl) Prompt(parent context.Context, target, message string) HerdrMutationResult {
	c.mu.Lock()
	c.attempts++
	attempt := c.attempts
	c.mu.Unlock()
	if attempt == c.timeoutOn {
		ctx, cancel := context.WithTimeout(parent, c.timeout)
		defer cancel()
		<-ctx.Done()
		return HerdrMutationResult{Outcome: herdrAmbiguous, Err: fmt.Errorf("herdr agent prompt deadline: %w", ctx.Err())}
	}
	return c.fakeHerdrControl.Prompt(parent, target, message)
}

type fakeGatewayState struct {
	mu       sync.Mutex
	health   GatewayHealth
	pingCall int
}

func (f *fakeGatewayState) Ping(_ context.Context, _ string, workspace string) GatewayHealth {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pingCall++
	value := f.health
	if value.Workspace != "" && value.Workspace != workspace {
		value.OK = false
	}
	return value
}

func (f *fakeGatewayState) setOnline(workspace, server string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health = GatewayHealth{OK: true, Connected: true, Version: gatewayProtocolVersion, Workspace: workspace, ServerID: server}
}

func newFakeHerdrControl(root, label string) *fakeHerdrControl {
	return &fakeHerdrControl{
		root:     root,
		snapshot: HerdrSnapshot{Workspaces: []HerdrWorkspace{{ID: "w-test", Label: label}}},
		nextID:   1, createMutates: true, startMutates: true, closeMutates: true,
	}
}

func cloneHerdrSnapshot(value HerdrSnapshot) HerdrSnapshot {
	raw, _ := json.Marshal(value)
	var clone HerdrSnapshot
	_ = json.Unmarshal(raw, &clone)
	return clone
}

func (f *fakeHerdrControl) wait(ctx context.Context) error {
	if f.delay <= 0 {
		return nil
	}
	timer := time.NewTimer(f.delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (f *fakeHerdrControl) Snapshot(ctx context.Context, _ HerdrSnapshotScope) (HerdrSnapshot, error) {
	if err := f.wait(ctx); err != nil {
		return HerdrSnapshot{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "snapshot")
	if len(f.snapshots) != 0 {
		value := f.snapshots[0]
		f.snapshots = f.snapshots[1:]
		return cloneHerdrSnapshot(value), nil
	}
	return cloneHerdrSnapshot(f.snapshot), nil
}

func (f *fakeHerdrControl) CreateWorkspace(ctx context.Context, label string) (HerdrWorkspace, HerdrMutationResult) {
	if err := f.wait(ctx); err != nil {
		return HerdrWorkspace{}, HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: err}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "workspace create "+label)
	workspace := HerdrWorkspace{ID: fmt.Sprintf("w-%d", f.nextID), Label: label}
	f.nextID++
	if f.createMutates {
		f.snapshot.Workspaces = append(f.snapshot.Workspaces, workspace)
		rootTab := HerdrTab{ID: workspace.ID + ":root", WorkspaceID: workspace.ID, Label: "1", CWD: f.root, Number: 1}
		rootPane := HerdrPane{ID: workspace.ID + ":root-pane", WorkspaceID: workspace.ID, TabID: rootTab.ID, CWD: f.root}
		f.snapshot.Tabs = append(f.snapshot.Tabs, rootTab)
		f.snapshot.Panes = append(f.snapshot.Panes, rootPane)
	}
	result := f.createOutcome
	if result.Outcome == "" {
		result.Outcome = herdrConfirmed
	}
	return workspace, result
}

func (f *fakeHerdrControl) CreateTab(ctx context.Context, spec HerdrTabSpec) (HerdrTabCreated, HerdrMutationResult) {
	if err := f.wait(ctx); err != nil {
		return HerdrTabCreated{}, HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: err}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "tab create "+spec.Label)
	f.lastEnv = map[string]string{}
	for key, value := range spec.Env {
		f.lastEnv[key] = value
	}
	id := f.nextID
	f.nextID++
	created := HerdrTabCreated{
		Tab:  HerdrTab{ID: fmt.Sprintf("%s:t%d", spec.WorkspaceID, id), WorkspaceID: spec.WorkspaceID, Label: spec.Label, CWD: spec.CWD},
		Pane: HerdrPane{ID: fmt.Sprintf("%s:p%d", spec.WorkspaceID, id), WorkspaceID: spec.WorkspaceID, TabID: fmt.Sprintf("%s:t%d", spec.WorkspaceID, id), CWD: spec.CWD},
	}
	if f.createMutates {
		f.snapshot.Tabs = append(f.snapshot.Tabs, created.Tab)
		f.snapshot.Panes = append(f.snapshot.Panes, created.Pane)
	}
	result := f.createOutcome
	if result.Outcome == "" {
		result.Outcome = herdrConfirmed
	}
	return created, result
}

func (f *fakeHerdrControl) StartAgent(ctx context.Context, name, kind, paneID string, native []string) HerdrMutationResult {
	if err := f.wait(ctx); err != nil {
		return HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: err}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "agent start "+name+" "+kind+" "+strings.Join(native, " "))
	f.lastAgentEnv = map[string]string{}
	for key, value := range f.lastEnv {
		f.lastAgentEnv[key] = value
	}
	if f.startMutates {
		for _, pane := range f.snapshot.Panes {
			if pane.ID == paneID {
				f.lastAgentEnv["HERDR_PANE_ID"] = pane.ID
				f.lastAgentEnv["HERDR_WORKSPACE_ID"] = pane.WorkspaceID
				f.snapshot.Agents = append(f.snapshot.Agents, HerdrAgent{
					Name: name, Kind: kind, Status: "idle", CWD: pane.CWD,
					WorkspaceID: pane.WorkspaceID, TabID: pane.TabID, PaneID: pane.ID,
					InteractiveReady: true,
				})
			}
		}
	}
	result := f.startOutcome
	if configured, ok := f.startOutcomes[name]; ok {
		result = configured
	}
	if result.Outcome == "" {
		result.Outcome = herdrConfirmed
	}
	return result
}

func (f *fakeHerdrControl) CloseTab(ctx context.Context, tabID string) HerdrMutationResult {
	if err := f.wait(ctx); err != nil {
		return HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: err}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "tab close "+tabID)
	if f.closeMutates {
		var tabs []HerdrTab
		for _, tab := range f.snapshot.Tabs {
			if tab.ID != tabID {
				tabs = append(tabs, tab)
			}
		}
		f.snapshot.Tabs = tabs
		paneIDs := map[string]bool{}
		var panes []HerdrPane
		for _, pane := range f.snapshot.Panes {
			if pane.TabID == tabID {
				paneIDs[pane.ID] = true
			} else {
				panes = append(panes, pane)
			}
		}
		f.snapshot.Panes = panes
		var agents []HerdrAgent
		for _, agent := range f.snapshot.Agents {
			if !paneIDs[agent.PaneID] {
				agents = append(agents, agent)
			}
		}
		f.snapshot.Agents = agents
	}
	result := f.closeOutcome
	if result.Outcome == "" {
		result.Outcome = herdrConfirmed
	}
	return result
}

func (f *fakeHerdrControl) RunPane(ctx context.Context, paneID, command string) HerdrMutationResult {
	if err := f.wait(ctx); err != nil {
		return HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: err}
	}
	f.mu.Lock()
	f.calls = append(f.calls, "pane run "+paneID)
	hook := f.onRunPane
	result := f.runPaneOutcome
	f.mu.Unlock()
	if hook != nil {
		hook(paneID, command)
	}
	if result.Outcome == "" {
		result.Outcome = herdrConfirmed
	}
	return result
}

func (f *fakeHerdrControl) Prompt(ctx context.Context, target, message string) HerdrMutationResult {
	if err := f.wait(ctx); err != nil {
		return HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: err}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "prompt "+target+" "+message)
	if f.promptWakes {
		for index := range f.snapshot.Agents {
			if f.snapshot.Agents[index].Name == target && f.snapshot.Agents[index].InteractiveReady {
				f.snapshot.Agents[index].Status = "working"
			}
		}
	}
	result := f.promptOutcome
	if result.Outcome == "" {
		result.Outcome = herdrConfirmed
	}
	return result
}

func healthySnapshot(root string, rule AgentRule, status string) HerdrSnapshot {
	workspaceID := "w-test"
	cwd := testRuleCWD(root, rule)
	tabID, paneID := workspaceID+":t1", workspaceID+":p1"
	return HerdrSnapshot{
		Workspaces: []HerdrWorkspace{{ID: workspaceID, Label: "hq-test"}},
		Tabs:       []HerdrTab{{ID: tabID, WorkspaceID: workspaceID, Label: rosterTabLabel(rule), CWD: cwd}},
		Panes:      []HerdrPane{{ID: paneID, WorkspaceID: workspaceID, TabID: tabID, CWD: cwd}},
		Agents: []HerdrAgent{{Name: rule.Name, Kind: rule.Kind, Status: status, CWD: cwd, WorkspaceID: workspaceID, TabID: tabID, PaneID: paneID,
			InteractiveReady: true}},
	}
}

func TestHerdrRuntimePatrolBlockedDriftOrphanAndGraceMatrix(t *testing.T) {
	root := canonicalTestTempDir(t)
	cfg := testConfig()
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "zantianyou" {
			cfg.Agents[index].ActivationPolicy = activationAlways
		} else {
			cfg.Agents[index].ActivationPolicy = activationManual
		}
		cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
	}
	materializeTestWorkstations(t, root, cfg)
	rule, _ := cfg.ruleFor("zantianyou")

	t.Run("blocked single signal is warning only", func(t *testing.T) {
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshot = healthySnapshot(root, rule, "blocked")
		report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, time.Second)
		if err != nil || report.Blocked != 1 || report.DeadCandidate != 0 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
		if got := len(control.calls); got != 1 {
			t.Fatalf("single signal should not take second snapshot, calls=%v", control.calls)
		}
	})

	t.Run("missing roster agent and its unique orphan tab persist", func(t *testing.T) {
		snapshot := healthySnapshot(root, rule, "idle")
		snapshot.Agents = nil
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshot = snapshot
		report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, 0)
		if err != nil || report.Drift == 0 || report.Orphan == 0 || report.DeadCandidate != 1 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
		var dead PatrolFinding
		orphanPreserved := false
		for _, finding := range report.Findings {
			if finding.Category == "dead_candidate" {
				dead = finding
			}
			if finding.Category == "orphan" && finding.ObjectID == "tab:w-test:t1" {
				orphanPreserved = true
			}
		}
		if dead.ObjectID != "roster:zantianyou" || strings.Join(dead.Signals, ",") != "missing_agent,orphan_tab" || len(dead.First) != 2 || len(dead.Second) != 2 || !orphanPreserved {
			t.Fatalf("linked evidence incomplete: dead=%+v orphan_preserved=%t findings=%+v", dead, orphanPreserved, report.Findings)
		}
	})

	t.Run("missing roster orphan signal recovers after grace", func(t *testing.T) {
		first := healthySnapshot(root, rule, "idle")
		first.Agents = nil
		second := cloneHerdrSnapshot(first)
		second.Tabs = nil
		second.Panes = nil
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshots = []HerdrSnapshot{first, second}
		report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, 0)
		if err != nil || report.DeadCandidate != 0 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
	})

	t.Run("missing roster signal alone is warning only", func(t *testing.T) {
		snapshot := healthySnapshot(root, rule, "idle")
		snapshot.Agents, snapshot.Tabs, snapshot.Panes = nil, nil, nil
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshot = snapshot
		report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, 0)
		if err != nil || report.DeadCandidate != 0 || len(control.calls) != 1 {
			t.Fatalf("report=%+v calls=%v err=%v", report, control.calls, err)
		}
	})

	t.Run("orphan signal alone is warning only", func(t *testing.T) {
		snapshot := healthySnapshot(root, rule, "idle")
		snapshot.Tabs = append(snapshot.Tabs, HerdrTab{ID: "w-test:t2", WorkspaceID: "w-test", Label: rosterTabLabel(rule), CWD: testRuleCWD(root, rule)})
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshot = snapshot
		report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, 0)
		if err != nil || report.Orphan != 1 || report.DeadCandidate != 0 || len(control.calls) != 1 {
			t.Fatalf("report=%+v calls=%v err=%v", report, control.calls, err)
		}
	})

	t.Run("ambiguous roster tab label fails closed without guessing", func(t *testing.T) {
		ambiguous := cfg
		other := rule
		other.Name, other.Nickname = "other-manager", "其他经理"
		other.Responsibilities = []string{"manager:other"}
		ambiguous.Agents = append(append([]AgentRule(nil), cfg.Agents...), other)
		ambiguous = bindTestRoleContracts(ambiguous)
		materializeTestWorkstations(t, root, ambiguous)
		snapshot := healthySnapshot(root, rule, "idle")
		snapshot.Agents = nil
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshot = snapshot
		report, err := (&PatrolService{Herdr: control}).Run(context.Background(), ambiguous, root, 0)
		ambiguousFinding := false
		for _, finding := range report.Findings {
			if finding.SignalType == "ambiguous_roster_tab_label" {
				ambiguousFinding = true
			}
		}
		if err != nil || report.Orphan != 1 || report.DeadCandidate != 0 || !ambiguousFinding || len(control.calls) != 1 {
			t.Fatalf("report=%+v calls=%v err=%v", report, control.calls, err)
		}
	})

	t.Run("two signals recover after grace", func(t *testing.T) {
		first := healthySnapshot(root, rule, "blocked")
		first.Panes = nil
		second := healthySnapshot(root, rule, "idle")
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshots = []HerdrSnapshot{first, second}
		slept := 0
		report, err := (&PatrolService{Herdr: control, Sleep: func(time.Duration) { slept++ }}).Run(context.Background(), cfg, root, time.Second)
		if err != nil || report.DeadCandidate != 0 || slept != 1 {
			t.Fatalf("report=%+v slept=%d err=%v", report, slept, err)
		}
	})

	t.Run("two independent signals persist", func(t *testing.T) {
		broken := healthySnapshot(root, rule, "blocked")
		broken.Panes = nil
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshots = []HerdrSnapshot{broken, broken}
		report, err := (&PatrolService{Herdr: control, Sleep: func(time.Duration) {}}).Run(context.Background(), cfg, root, time.Second)
		if err != nil || report.DeadCandidate != 1 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
		var dead PatrolFinding
		for _, finding := range report.Findings {
			if finding.Category == "dead_candidate" {
				dead = finding
			}
		}
		if len(dead.Signals) < 2 || len(dead.First) < 2 || len(dead.Second) < 2 || dead.GraceMS != 1000 {
			t.Fatalf("dead candidate evidence incomplete: %+v", dead)
		}
	})

	t.Run("same name in another workspace never matches", func(t *testing.T) {
		snapshot := healthySnapshot(root, rule, "idle")
		snapshot.Workspaces = append(snapshot.Workspaces, HerdrWorkspace{ID: "w-other", Label: "other"})
		snapshot.Agents[0].WorkspaceID = "w-other"
		snapshot.Tabs[0].WorkspaceID = "w-other"
		snapshot.Panes[0].WorkspaceID = "w-other"
		control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
		control.snapshot = snapshot
		report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, 0)
		if err != nil || report.Drift == 0 {
			t.Fatalf("report=%+v err=%v", report, err)
		}
	})
}

func TestHerdrRuntimeSessionStrictLifecycleAndPhysicalDiagnostics(t *testing.T) {
	root := canonicalTestTempDir(t)
	store := &FileSessionStore{Root: root}
	created := HerdrTabCreated{Tab: HerdrTab{ID: "w1:t1"}, Pane: HerdrPane{ID: "w1:p1"}}
	rule, _ := testConfig().exactRule("eng-developer")
	start, err := newSessionEvent(time.Date(2026, 8, 28, 1, 2, 3, 0, time.UTC), "started", created, "w1", rule, "zantianyou", "start", "/fixture/engineering")
	if err != nil {
		t.Fatal(err)
	}
	stop := start
	stop.EventID, stop.Type, stop.Reason, stop.At = "SESSION-STOP0001", "stopped", "rollback", "2026-08-28T01:03:03Z"
	if err := store.Append(start); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(start); err != nil {
		t.Fatalf("exact event retry must be idempotent: %v", err)
	}
	if err := store.Append(stop); err != nil {
		t.Fatal(err)
	}
	events, err := store.List(SessionFilter{SessionID: start.SessionID})
	if err != nil || len(events) != 2 || events[0].Type != "started" || events[1].Type != "stopped" {
		t.Fatalf("events=%+v err=%v", events, err)
	}
	if err := store.Append(stop); err != nil {
		t.Fatalf("exact stop retry must be idempotent: %v", err)
	}
	var query bytes.Buffer
	app := &App{Sessions: store, JSON: true, Out: &query, Err: io.Discard}
	if err := app.cmdSession([]string{"list", "--session", start.SessionID}); err != nil {
		t.Fatal(err)
	}
	var queried []SessionEvent
	if err := json.Unmarshal(query.Bytes(), &queried); err != nil || len(queried) != 2 {
		t.Fatalf("query=%s events=%+v err=%v", query.String(), queried, err)
	}

	t.Run("stop before start", func(t *testing.T) {
		fresh := &FileSessionStore{Root: canonicalTestTempDir(t)}
		if err := fresh.Append(stop); err == nil || !strings.Contains(err.Error(), "stop-before-start") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("unknown field and physical line", func(t *testing.T) {
		badRoot := canonicalTestTempDir(t)
		writeTestFile(t, filepath.Join(badRoot, "2026-08.jsonl"), `{"version":1,"unknown":true}`+"\n")
		_, err := (&FileSessionStore{Root: badRoot}).List(SessionFilter{})
		if err == nil || !strings.Contains(err.Error(), ":1") || !strings.Contains(err.Error(), "unknown") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("duplicate top-level fields and physical line", func(t *testing.T) {
		valid, err := json.Marshal(start)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"agent", "type"} {
			field := field
			t.Run(field, func(t *testing.T) {
				needle := fmt.Sprintf(`"%s":"%s"`, field, map[string]string{"agent": start.Agent, "type": start.Type}[field])
				duplicate := needle + "," + needle
				line := bytes.Replace(valid, []byte(needle), []byte(duplicate), 1)
				if bytes.Equal(line, valid) {
					t.Fatalf("fixture did not duplicate %s", field)
				}
				badRoot := canonicalTestTempDir(t)
				writeTestFile(t, filepath.Join(badRoot, "2026-08.jsonl"), string(line)+"\n")
				_, err := (&FileSessionStore{Root: badRoot}).List(SessionFilter{})
				if err == nil || !strings.Contains(err.Error(), ":1") || !strings.Contains(err.Error(), "重复 JSON 字段") || !strings.Contains(err.Error(), field) {
					t.Fatalf("got %v", err)
				}
			})
		}
	})
	t.Run("truncated final line", func(t *testing.T) {
		badRoot := canonicalTestTempDir(t)
		writeTestFile(t, filepath.Join(badRoot, "2026-08.jsonl"), `{"version":1}`)
		_, err := (&FileSessionStore{Root: badRoot}).List(SessionFilter{})
		if err == nil || !strings.Contains(err.Error(), ":1") || !strings.Contains(err.Error(), "截断") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fsync crash window is idempotently recovered", func(t *testing.T) {
		crashRoot := canonicalTestTempDir(t)
		crashStore := &FileSessionStore{Root: crashRoot, Failpoint: func(name string) error {
			if name == "after_file_fsync" {
				return errors.New("synthetic crash")
			}
			return nil
		}}
		if err := crashStore.Append(start); err != nil {
			t.Fatalf("durable event should reconcile: %v", err)
		}
		crashStore.Failpoint = nil
		if err := crashStore.Append(start); err != nil {
			t.Fatalf("retry failed: %v", err)
		}
		events, err := crashStore.List(SessionFilter{})
		if err != nil || len(events) != 1 {
			t.Fatalf("events=%+v err=%v", events, err)
		}
	})
	t.Run("concurrent independent sessions stay valid", func(t *testing.T) {
		concurrent := &FileSessionStore{Root: canonicalTestTempDir(t)}
		var wait sync.WaitGroup
		errs := make(chan error, 8)
		for i := 0; i < 8; i++ {
			wait.Add(1)
			go func(i int) {
				defer wait.Done()
				event := start
				event.EventID = fmt.Sprintf("SESSION-CONCURRENT-%02d", i)
				event.SessionID = fmt.Sprintf("w1:t%d", i+10)
				event.TabID = event.SessionID
				event.PaneID = fmt.Sprintf("w1:p%d", i+10)
				errs <- concurrent.Append(event)
			}(i)
		}
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		events, err := concurrent.List(SessionFilter{})
		if err != nil || len(events) != 8 {
			t.Fatalf("events=%d err=%v", len(events), err)
		}
	})
}

func TestHerdrRuntimeDoctorCompanyHealthFailsClosedAndNeverWrites(t *testing.T) {
	e := prepareDoctorEnv(t)
	if err := os.MkdirAll(e.data, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, filepath.Join(e.data, "hq.sock"), "started-but-invalid\n")
	app, out, _ := newTestDoctor(t, e, &fakeDoctorRunner{}, true)
	app.PatrolRunner = fakeDoctorPatrol{report: PatrolReport{Version: patrolReportVersion, WorkspaceID: "w-test", WorkspaceLabel: "hq-test", Drift: 1}}
	app.GatewayHealth = fakeDoctorGateway{health: GatewayHealth{OK: false, Connected: true, Error: "bad handshake"}}
	app.LedgerHealth = fakeDoctorLedger{err: errors.New("fixture ledger corrupt at events/2026-08.jsonl:2")}
	before := snapshotTree(t, e.root)
	err := app.cmdDoctor(nil)
	after := snapshotTree(t, e.root)
	var failed DoctorFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("doctor must fail closed, got %v", err)
	}
	if fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("doctor wrote inspected tree\nbefore=%v\nafter=%v", before, after)
	}
	report := decodeDoctorReport(t, out.Bytes())
	if report.CompanyHealth == nil || report.CompanyHealth.Patrol.Drift != 1 || report.CompanyHealth.Gateway.OK || len(report.CompanyHealth.Errors) < 2 {
		t.Fatalf("company_health incomplete: %+v", report.CompanyHealth)
	}
	if checksByName(report)["company_health"].Status != doctorStatusFail {
		t.Fatalf("company health must FAIL: %+v", checksByName(report)["company_health"])
	}

	t.Run("strict read-only ledger rejects physical corruption", func(t *testing.T) {
		root := canonicalTestTempDir(t)
		writeTestFile(t, filepath.Join(root, "events", "2026-08.jsonl"), "{bad-json}\n")
		_, err := (readOnlyLedgerHealth{Dir: root}).Read(testConfig())
		if err == nil || !strings.Contains(err.Error(), ":1") {
			t.Fatalf("got %v", err)
		}
		if _, err := os.Lstat(filepath.Join(root, ".hq.lock")); !os.IsNotExist(err) {
			t.Fatalf("read-only health created ledger lock: %v", err)
		}
	})
}

func TestHerdrRuntimeDoctorRemediationPrefixesRemainSemantic(t *testing.T) {
	var out bytes.Buffer
	app := &App{Out: &out}
	report := DoctorReport{Version: doctorReportVersion, Checks: []DoctorCheck{
		passDoctorCheck("advisory", "ok", "read-only note"),
		failDoctorCheck("failure", "bad", "repair action"),
	}}
	if err := app.writeDoctorReport(report); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "提示：read-only note") || !strings.Contains(text, "修复：repair action") || strings.Contains(text, "修复：read-only note") {
		t.Fatalf("semantic prefixes regressed:\n%s", text)
	}
}

func TestHerdrRuntimeHerdrSnapshotStrictContractAndForwardCompatibility(t *testing.T) {
	valid := `{"result":{"snapshot":{"workspaces":[{"workspace_id":"w1","label":"hq","future":true}],"tabs":[{"tab_id":"w1:t1","workspace_id":"w1","label":"工程部-李春","cwd":"/fixture/engineering"}],"panes":[{"pane_id":"w1:p1","workspace_id":"w1","tab_id":"w1:t1","cwd":"/fixture/engineering"}],"agents":[{"name":"lichun","agent":"codex","agent_status":"idle","cwd":"/fixture/engineering","workspace_id":"w1","tab_id":"w1:t1","pane_id":"w1:p1","upstream_new":"ok"}]}}}`
	scope := HerdrSnapshotScope{WorkspaceLabel: "hq"}
	if _, err := decodeHerdrSnapshot([]byte(valid), scope); err != nil {
		t.Fatalf("upstream additive fields must be allowed: %v", err)
	}
	missing := strings.Replace(valid, `,"pane_id":"w1:p1"`, "", 1)
	if _, err := decodeHerdrSnapshot([]byte(missing), scope); err == nil || !strings.Contains(err.Error(), "缺少") {
		t.Fatalf("missing contract field accepted: %v", err)
	}
	conflict := strings.Replace(valid, `"agents":[`, `"agents":[{"name":"lichun","agent":"codex","agent_status":"working","cwd":"/fixture/engineering","workspace_id":"w1","tab_id":"w1:t1","pane_id":"w1:p1"},`, 1)
	if _, err := decodeHerdrSnapshot([]byte(conflict), scope); err == nil || !strings.Contains(err.Error(), "稳定 ID") {
		t.Fatalf("conflicting stable ID accepted: %v", err)
	}
}

func addRuntimeTestAgent(t *testing.T, e testEnv, rule AgentRule) AgentRule {
	t.Helper()
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		cfg.Agents = append(cfg.Agents, rule)
		*cfg = bindTestRoleContracts(*cfg)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	bound, ok := cfg.exactRule(rule.Name)
	if !ok {
		t.Fatalf("bound runtime rule missing: %s", rule.Name)
	}
	if err := os.MkdirAll(testRuleCWD(e.root, bound), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.root, bound.ManualPath), testRoleManual(bound.Name), 0o644); err != nil {
		t.Fatal(err)
	}
	return bound
}

func TestHerdrRuntimeUpInjectsIdentityRecordsSessionAndHandlesFailures(t *testing.T) {
	e := setupTestEnv(t)
	rule := addRuntimeTestAgent(t, e, AgentRule{Name: "lichun-test", Label: "工程部-李春-test", Nickname: "李春-test", DepartmentLabel: "工程部", Workspace: "hq-test", Responsibilities: []string{"developer_test:engineering"}, Department: "engineering", Kind: "codex", ReportsTo: "zantianyou", ActivationPolicy: activationAlways})
	control := newFakeHerdrControl(e.root, "hq-test")
	app := e.app(t)
	e.setActor(t, "penny", "w1:p1", testAgentCWD(app.Config, e.root, "penny"))
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "session-fixture")}
	app.Clock = func() time.Time { return time.Date(2026, 8, 28, 2, 0, 0, 0, time.UTC) }
	app.FromGateway, app.Direct = false, true
	if err := app.run([]string{"up", "--no-gateway", rule.Name}); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"HQ_AGENT_NAME": rule.Name, "HQ_DEPARTMENT": rule.Department, "HQ_REPORTS_TO": rule.ReportsTo} {
		if control.lastEnv[key] != want {
			t.Fatalf("env %s=%q want=%q", key, control.lastEnv[key], want)
		}
	}
	if control.lastAgentEnv["HERDR_PANE_ID"] == "" || control.lastAgentEnv["HERDR_PANE_ID"] != control.snapshot.Agents[0].PaneID || control.lastAgentEnv["HERDR_WORKSPACE_ID"] != "w-test" {
		t.Fatalf("fake child did not inherit exact Herdr pane/workspace identity: env=%v agent=%+v", control.lastAgentEnv, control.snapshot.Agents[0])
	}
	events, err := app.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil || len(events) != 1 || events[0].PaneID == "" || events[0].PaneID != events[0].SessionID[:strings.LastIndex(events[0].SessionID, ":")]+":p1" {
		if err != nil || len(events) != 1 || events[0].PaneID == "" {
			t.Fatalf("events=%+v err=%v", events, err)
		}
	}
	if events[0].WorkspaceID != "w-test" || events[0].TabID == "" || events[0].Type != "started" {
		t.Fatalf("session fields incomplete: %+v", events[0])
	}

	t.Run("missing manual rejects before create", func(t *testing.T) {
		missingRule := rule
		missingRule.Name, missingRule.Department = "missing", "missing-dept"
		missingRule.WorkstationPath = "missing-dept/staff/missing/v1"
		missingRule.ManualPath = filepath.Join(missingRule.WorkstationPath, "AGENTS.md")
		cfg := app.Config
		cfg.Agents = append(cfg.Agents, missingRule)
		other := *app
		other.Config = cfg
		before := len(control.calls)
		if err := other.runUp([]string{"--no-gateway", missingRule.Name}); err == nil || !strings.Contains(err.Error(), "尚未创建 tab") {
			t.Fatalf("got %v", err)
		}
		for _, call := range control.calls[before:] {
			if strings.HasPrefix(call, "tab create") {
				t.Fatalf("manual failure created tab: %v", control.calls[before:])
			}
		}
	})

	t.Run("symlink workstation rejects before create", func(t *testing.T) {
		target := filepath.Join(e.root, "symlink-target")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(target, "AGENTS.md"), "# target\n")
		link := filepath.Join(e.root, "symlink-dept", "staff", "linked", "v1")
		if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		linkedRule := rule
		linkedRule.Name, linkedRule.Department = "linked", "symlink-dept"
		linkedRule.WorkstationPath = "symlink-dept/staff/linked/v1"
		linkedRule.ManualPath = "symlink-dept/staff/linked/v1/AGENTS.md"
		cfg := app.Config
		cfg.Agents = append(cfg.Agents, linkedRule)
		other := *app
		other.Config = cfg
		before := len(control.calls)
		if err := other.runUp([]string{"--no-gateway", linkedRule.Name}); err == nil || !strings.Contains(err.Error(), "canonical") {
			t.Fatalf("got %v", err)
		}
		for _, call := range control.calls[before:] {
			if strings.HasPrefix(call, "tab create") {
				t.Fatalf("symlink workstation created tab: %v", control.calls[before:])
			}
		}
	})

	t.Run("wrong workspace live name refuses duplicate", func(t *testing.T) {
		freshControl := newFakeHerdrControl(e.root, "hq-test")
		freshControl.snapshot.Workspaces = append(freshControl.snapshot.Workspaces, HerdrWorkspace{ID: "w-other", Label: "other"})
		cwd := testRuleCWD(e.root, rule)
		freshControl.snapshot.Tabs = append(freshControl.snapshot.Tabs, HerdrTab{ID: "w-other:t1", WorkspaceID: "w-other", Label: rule.Label, CWD: cwd})
		freshControl.snapshot.Panes = append(freshControl.snapshot.Panes, HerdrPane{ID: "w-other:p1", WorkspaceID: "w-other", TabID: "w-other:t1", CWD: cwd})
		freshControl.snapshot.Agents = append(freshControl.snapshot.Agents, HerdrAgent{Name: rule.Name, Kind: rule.Kind, Status: "idle", CWD: cwd, WorkspaceID: "w-other", TabID: "w-other:t1", PaneID: "w-other:p1"})
		other := *app
		other.Herdr = freshControl
		other.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "wrong-workspace-session")}
		err := other.runUp([]string{"--no-gateway", rule.Name})
		if err == nil || !strings.Contains(err.Error(), "其他 workspace") {
			t.Fatalf("got %v", err)
		}
		for _, call := range freshControl.calls {
			if strings.HasPrefix(call, "tab create") {
				t.Fatalf("duplicate tab created: %v", freshControl.calls)
			}
		}
	})

	t.Run("prompt failure is machine-distinguishable partial success", func(t *testing.T) {
		freshControl := newFakeHerdrControl(e.root, "hq-test")
		freshControl.promptOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("prompt timeout")}
		other := *app
		other.Herdr = freshControl
		other.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "partial-session")}
		err := other.runUp([]string{"--no-gateway", rule.Name})
		var partial *PartialStartError
		if !errors.As(err, &partial) || partial.TabID == "" || partial.PaneID == "" || partial.SessionID == "" {
			t.Fatalf("got %T %v", err, err)
		}
		if len(freshControl.snapshot.Agents) != 1 {
			t.Fatalf("partial success must retain confirmed agent: %+v", freshControl.snapshot)
		}
	})

	t.Run("session failure closes exactly owned tab", func(t *testing.T) {
		freshControl := newFakeHerdrControl(e.root, "hq-test")
		other := *app
		other.Herdr = freshControl
		other.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "bad-session"), Failpoint: func(name string) error {
			if name == "before_append" {
				return errors.New("disk failure")
			}
			return nil
		}}
		err := other.runUp([]string{"--no-gateway", rule.Name})
		if err == nil || !strings.Contains(err.Error(), "session start 记账失败") || len(freshControl.snapshot.Tabs) != 0 || len(freshControl.snapshot.Agents) != 0 {
			t.Fatalf("err=%v snapshot=%+v", err, freshControl.snapshot)
		}
	})

	t.Run("timeout after start reconciles instead of retry", func(t *testing.T) {
		freshControl := newFakeHerdrControl(e.root, "hq-test")
		freshControl.startOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("deadline after success")}
		other := *app
		other.Herdr = freshControl
		other.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "ambiguous-start-session")}
		if err := other.runUp([]string{"--no-gateway", rule.Name}); err != nil {
			t.Fatalf("ambiguous confirmed start should reconcile: %v", err)
		}
		starts := 0
		for _, call := range freshControl.calls {
			if strings.HasPrefix(call, "agent start") {
				starts++
			}
		}
		if starts != 1 {
			t.Fatalf("ambiguous start was blindly retried: %v", freshControl.calls)
		}
	})

	t.Run("timeout before create leaves no tab", func(t *testing.T) {
		freshControl := newFakeHerdrControl(e.root, "hq-test")
		freshControl.createMutates = false
		freshControl.createOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("deadline before run")}
		other := *app
		other.Herdr = freshControl
		other.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "before-create-session")}
		err := other.runUp([]string{"--no-gateway", rule.Name})
		if err == nil || len(freshControl.snapshot.Tabs) != 0 {
			t.Fatalf("err=%v snapshot=%+v", err, freshControl.snapshot)
		}
	})

	t.Run("close timeout reconciles absent tab", func(t *testing.T) {
		freshControl := newFakeHerdrControl(e.root, "hq-test")
		freshControl.startMutates = false
		freshControl.startOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("start rejected")}
		freshControl.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("close timeout after success")}
		freshControl.closeMutates = true
		other := *app
		other.Herdr = freshControl
		other.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "close-reconcile-session")}
		err := other.runUp([]string{"--no-gateway", rule.Name})
		if err == nil || strings.Contains(err.Error(), "orphan_id") || len(freshControl.snapshot.Tabs) != 0 {
			t.Fatalf("close should reconcile cleanly: err=%v snapshot=%+v", err, freshControl.snapshot)
		}
	})

	t.Run("cleanup failure returns orphan id", func(t *testing.T) {
		freshControl := newFakeHerdrControl(e.root, "hq-test")
		freshControl.startMutates = false
		freshControl.startOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("start rejected")}
		freshControl.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("close timeout")}
		freshControl.closeMutates = false
		other := *app
		other.Herdr = freshControl
		other.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "orphan-session")}
		err := other.runUp([]string{"--no-gateway", rule.Name})
		if err == nil || !strings.Contains(err.Error(), "orphan_id=") || len(freshControl.snapshot.Tabs) != 1 {
			t.Fatalf("err=%v snapshot=%+v", err, freshControl.snapshot)
		}
	})
}

func TestHerdrRuntimeConcurrentUpStartsSingleAgent(t *testing.T) {
	e := setupTestEnv(t)
	rule := addRuntimeTestAgent(t, e, AgentRule{Name: "concurrent", Label: "工程部-并发", Nickname: "并发", DepartmentLabel: "工程部", Workspace: "hq-test", Responsibilities: []string{"concurrent:engineering"}, Department: "engineering", Kind: "codex", ReportsTo: "zantianyou", ActivationPolicy: activationAlways})
	base := e.app(t)
	control := newFakeHerdrControl(e.root, "hq-test")
	base.Herdr = control
	base.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "concurrent-sessions")}
	base.DataDir = filepath.Join(e.root, "concurrent-records")
	var wait sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); errs <- base.runUp([]string{"--no-gateway", rule.Name}) }()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent up failed: %v", err)
		}
	}
	starts := 0
	for _, call := range control.calls {
		if strings.HasPrefix(call, "agent start") {
			starts++
		}
	}
	if starts != 1 || len(control.snapshot.Agents) != 1 {
		t.Fatalf("concurrent up did not converge: starts=%d snapshot=%+v calls=%v", starts, control.snapshot, control.calls)
	}
}

func TestHerdrRuntimeUpLockCrossProcessHelper(t *testing.T) {
	if os.Getenv("HQ_UP_LOCK_HELPER") != "1" {
		return
	}
	app := &App{DataDir: os.Getenv("HQ_UP_LOCK_DATA")}
	lock, err := app.lockUp()
	if err != nil {
		fmt.Fprintf(os.Stderr, "up-lock-helper: %v\n", err)
		os.Exit(2)
	}
	unlock(lock)
	fmt.Fprintln(os.Stdout, "up-lock-acquired")
}

func TestDefaultUpGatewayReconcileCanColdResumePreparedOfflineMessage(t *testing.T) {
	e := setupTestEnv(t)
	binary := filepath.Join(e.office, "tools", "hq", "bin", "hq")
	writeTestFile(t, binary, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}

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
	sender := e.app(t)
	actorRule, _ := sender.Config.exactRule("penny")
	targetRule, _ := sender.Config.exactRule("eng-developer")
	commandID := stableCommandID("startup-reconcile-message", targetRule.Name)
	result, err := sender.transact(commandID, requestDigest("startup-reconcile-message", targetRule.Name), func(*ledgerState) (Event, error) {
		event, err := sender.newEvent(Actor{Name: actorRule.Name, Label: actorRule.Label, Department: actorRule.Department, Rule: actorRule, PaneID: "startup-reconcile:penny"}, "message_prepared", "")
		if err != nil {
			return Event{}, err
		}
		event.Recipient, event.RecipientLabel = targetRule.Name, targetRule.Label
		event.MessageID = stableMessageID(commandID)
		event.MessageKind, event.Message = "request", "Recover this durable startup message"
		event.ThreadID = stableMessageID(stableCommandID("startup-reconcile-thread", targetRule.Name))
		event.DeliveryMode, event.DeliveryTarget, event.DeliveryReason = deliveryModeWakeup, deliveryTargetNextTurn, "explicit-wakeup"
		event.Delivery, event.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, targetRule.Name)
		payload, err := sender.deliveryPayload(event)
		if err != nil {
			return Event{}, err
		}
		event.PayloadDigest = digestText(payload)
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	origin := result.Event

	control := newFakeHerdrControl(e.root, "hq-test")
	gateway := &fakeGatewayState{health: GatewayHealth{Error: "offline"}}
	sessions := &FileSessionStore{Root: filepath.Join(e.root, "startup-reconcile-sessions")}
	gatewayApp := e.app(t)
	gatewayApp.Herdr, gatewayApp.Sessions = control, sessions
	var gatewayErr error
	var helperOutput string
	control.onRunPane = func(_ string, command string) {
		executable, executableErr := os.Executable()
		if executableErr != nil {
			gatewayErr = executableErr
			return
		}
		helperCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		helper := exec.CommandContext(helperCtx, executable, "-test.run=^TestHerdrRuntimeUpLockCrossProcessHelper$", "-test.count=1")
		helper.Env = append(os.Environ(), "HQ_UP_LOCK_HELPER=1", "HQ_UP_LOCK_DATA="+e.data)
		output, helperErr := helper.CombinedOutput()
		helperOutput = string(output)
		if helperErr != nil {
			gatewayErr = fmt.Errorf("gateway child could not acquire parent up lock: %w: %s", helperErr, output)
			return
		}
		if err := gatewayApp.reconcileDeliveries(); err != nil {
			gatewayErr = err
			return
		}
		marker := "--server-id '"
		start := strings.Index(command, marker)
		if start < 0 {
			gatewayErr = fmt.Errorf("gateway command lacks server id: %s", command)
			return
		}
		start += len(marker)
		end := strings.Index(command[start:], "'")
		if end < 0 {
			gatewayErr = fmt.Errorf("gateway command has unterminated server id: %s", command)
			return
		}
		gateway.setOnline("w-test", command[start:start+end])
	}

	app := e.app(t)
	app.Herdr, app.GatewayHealth, app.Sessions = control, gateway, sessions
	done := make(chan error, 1)
	go func() { done <- app.runUp([]string{targetRule.Name}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("default up did not recover prepared offline message: %v (gateway=%v helper=%q)", err, gatewayErr, helperOutput)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("default up deadlocked with gateway reconcile cold-resume")
	}
	if gatewayErr != nil || !strings.Contains(helperOutput, "up-lock-acquired") {
		t.Fatalf("gateway startup lock proof failed: gateway=%v helper=%q", gatewayErr, helperOutput)
	}
	view, ok, err := app.Store.Delivery(app.Config, origin.DeliveryID)
	if err != nil || !ok || view.Status != deliverySent || view.AttemptCount != 1 {
		t.Fatalf("startup reconcile delivery=%+v ok=%t err=%v", view, ok, err)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("startup reconcile transport calls=%d, want=1", len(e.transport.calls))
	}
	starts := 0
	for _, call := range control.calls {
		if strings.HasPrefix(call, "agent start "+targetRule.Name+" ") {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("cold-resume/default up duplicated target start: starts=%d calls=%v", starts, control.calls)
	}
	events, err := app.Store.ReadAll(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	if countCaseEvents(events, "", "delivery_unknown") != 0 {
		t.Fatalf("successful startup recovery fabricated unknown: %+v", events)
	}
}

func TestHerdrRuntimeConcurrentUpConvergesToSingleGateway(t *testing.T) {
	e := setupTestEnv(t)
	binary := filepath.Join(e.office, "tools", "hq", "bin", "hq")
	writeTestFile(t, binary, "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(binary, 0o755); err != nil {
		t.Fatal(err)
	}
	control := newFakeHerdrControl(e.root, "hq-test")
	gateway := &fakeGatewayState{health: GatewayHealth{Error: "offline"}}
	control.onRunPane = func(_ string, command string) {
		marker := "--server-id '"
		start := strings.Index(command, marker)
		if start < 0 {
			return
		}
		start += len(marker)
		end := strings.Index(command[start:], "'")
		if end < 0 {
			return
		}
		gateway.setOnline("w-test", command[start:start+end])
	}
	base := e.app(t)
	base.Herdr, base.GatewayHealth = control, gateway
	base.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "gateway-concurrent-sessions")}
	base.DataDir = filepath.Join(e.root, "gateway-concurrent-records")
	var wait sync.WaitGroup
	errs := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wait.Add(1)
		go func() { defer wait.Done(); errs <- base.runUp(nil) }()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent gateway up failed: %v", err)
		}
	}
	creates := 0
	for _, call := range control.calls {
		if call == "tab create hq-gateway" {
			creates++
		}
	}
	if creates != 1 || len(control.snapshot.Tabs) != 1 {
		t.Fatalf("gateway did not converge: creates=%d snapshot=%+v calls=%v", creates, control.snapshot, control.calls)
	}
}

func TestHerdrRuntimeGatewayProtocolFramingWorkspaceAndCleanup(t *testing.T) {
	root := shortSocketTempDir(t)
	socket := filepath.Join(root, "hq.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app := &App{Out: io.Discard, Err: io.Discard, GatewayContext: ctx}
	done := make(chan error, 1)
	go func() { done <- app.serveGateway(ctx, listener, "w-test", "gateway-test") }()
	deadline := time.Now().Add(time.Second)
	for {
		health := (unixGatewayPinger{}).Ping(context.Background(), socket, "w-test")
		if health.OK {
			if health.ServerID != "gateway-test" {
				t.Fatalf("health=%+v", health)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("gateway not ready: %+v", health)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if health := (unixGatewayPinger{}).Ping(context.Background(), socket, "w-other"); health.OK || !health.Connected {
		t.Fatalf("workspace mismatch must fail closed after connect: %+v", health)
	}

	sendRawRequest := func(t *testing.T, raw string) gatewayResponse {
		t.Helper()
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		_ = conn.SetDeadline(time.Now().Add(time.Second))
		if _, err := io.WriteString(conn, raw+"\n"); err != nil {
			t.Fatal(err)
		}
		if unix, ok := conn.(*net.UnixConn); ok {
			if err := unix.CloseWrite(); err != nil {
				t.Fatal(err)
			}
		}
		var response gatewayResponse
		if err := json.NewDecoder(conn).Decode(&response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	for _, tc := range []struct {
		name string
		key  string
		raw  string
	}{
		{name: "workspace identity", key: "workspace_id", raw: `{"version":1,"type":"ping","workspace_id":"w-evil","workspace_id":"w-test"}`},
		{name: "message type", key: "type", raw: `{"version":1,"type":"request","type":"ping","workspace_id":"w-test"}`},
	} {
		t.Run("server request rejects duplicate "+tc.name, func(t *testing.T) {
			response := sendRawRequest(t, tc.raw)
			if response.OK || response.Type == "pong" || !strings.Contains(response.Error, "重复 JSON 字段") || !strings.Contains(response.Error, tc.key) {
				t.Fatalf("duplicate %s request accepted or imprecise: %+v", tc.key, response)
			}
		})
	}

	for index, tc := range []struct {
		name string
		key  string
		raw  string
	}{
		{name: "workspace identity", key: "workspace_id", raw: `{"version":1,"type":"pong","workspace_id":"w-evil","workspace_id":"w-test","server_id":"gateway-test","ok":true}`},
		{name: "message type", key: "type", raw: `{"version":1,"type":"response","type":"pong","workspace_id":"w-test","server_id":"gateway-test","ok":true}`},
	} {
		t.Run("client response rejects duplicate "+tc.name, func(t *testing.T) {
			responseSocket := filepath.Join(root, fmt.Sprintf("response-%d.sock", index))
			responseListener, err := net.Listen("unix", responseSocket)
			if err != nil {
				t.Fatal(err)
			}
			defer responseListener.Close()
			served := make(chan error, 1)
			go func() {
				conn, err := responseListener.Accept()
				if err != nil {
					served <- err
					return
				}
				defer conn.Close()
				if _, err := io.Copy(io.Discard, conn); err != nil {
					served <- err
					return
				}
				_, err = io.WriteString(conn, tc.raw+"\n")
				served <- err
			}()
			health := (unixGatewayPinger{}).Ping(context.Background(), responseSocket, "w-test")
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			if health.OK || !health.Connected || !strings.Contains(health.Error, "重复 JSON 字段") || !strings.Contains(health.Error, tc.key) {
				t.Fatalf("duplicate %s response accepted or imprecise: %+v", tc.key, health)
			}
		})
	}

	t.Run("strict entry rejects nested duplicate", func(t *testing.T) {
		var target struct {
			Envelope struct {
				Identity string `json:"identity"`
			} `json:"envelope"`
		}
		err := decodeSingleJSON(strings.NewReader(`{"envelope":{"identity":"first","identity":"second"}}`), gatewayRequestLimit, &target)
		if err == nil || !strings.Contains(err.Error(), "重复 JSON 字段") || !strings.Contains(err.Error(), "identity") {
			t.Fatalf("nested duplicate accepted or imprecise: %v", err)
		}
	})

	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = io.WriteString(conn, `{"version":1,"type":"ping","workspace_id":"w-test"}`+"\n{}\n")
	if unix, ok := conn.(*net.UnixConn); ok {
		_ = unix.CloseWrite()
	}
	var response gatewayResponse
	if err := json.NewDecoder(conn).Decode(&response); err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if response.OK || !strings.Contains(response.Error, "trailing") && !strings.Contains(response.Error, "多 JSON") && !strings.Contains(response.Error, "第二个值") {
		t.Fatalf("trailing data accepted: %+v", response)
	}

	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("serve exit=%v", err)
	}
	_ = listener.Close()
}

func TestHerdrRuntimeGatewayBusinessDeadlineCoversColdIssueActivation(t *testing.T) {
	if gatewayBusinessExecutionTimeout < gatewayColdIssueNominalTimeout+defaultHerdrStartTimeout ||
		gatewayBusinessIOTimeout-gatewayBusinessExecutionTimeout < gatewayHealthIOTimeout {
		t.Fatalf("gateway budgets do not cover nominal cold issue plus bounded retry and response grace: nominal=%s execution=%s io=%s",
			gatewayColdIssueNominalTimeout, gatewayBusinessExecutionTimeout, gatewayBusinessIOTimeout)
	}

	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == "eng-developer" {
				cfg.Agents[index].ActivationPolicy = activationOnAssignment
				cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "gateway-cold-issue.md"), "# Cold issue\n")
	e.setActor(t, "zantianyou", "w1:p2", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "case", "create", "--id", "GATEWAY-COLD-001", "--title", "Gateway cold activation", "--source", source)
	manager, _ := cfg.exactRule("zantianyou")
	target, _ := cfg.exactRule("eng-developer")
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, manager, "idle")
	control.nextID = 2
	control.promptWakes = true
	control.delay = 15 * time.Millisecond
	t.Setenv("HERDR_PANE_ID", "w-test:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w-test")

	server := e.app(t)
	server.Out, server.Err = io.Discard, io.Discard
	server.Herdr = control
	server.Identity = herdrIdentityProvider{Control: control}
	server.Transport = herdrDeliveryTransport{Control: control}
	server.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "gateway-cold-sessions")}
	policy := gatewayTimeoutPolicy{Health: 25 * time.Millisecond, Business: 2 * time.Second}

	socket := filepath.Join(e.data, "hq.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			server.handleGatewayConnectionWithTimeouts(conn, "w-test", "gateway-cold", policy)
		}
	}()

	var out, errOut bytes.Buffer
	if err := forwardToGatewayWithTimeouts(socket,
		[]string{"issue", "--case", "GATEWAY-COLD-001", "--to", "eng-developer", "--next", "implement and report"},
		false, false, &out, &errOut, policy); err != nil {
		t.Fatalf("cold issue exceeded health timeout despite business budget: %v\nstderr=%s\nstdout=%s", err, errOut.String(), out.String())
	}
	<-done
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "tab create "+rosterTabLabel(target)) ||
		!strings.Contains(joined, "agent start eng-developer") || strings.Count(joined, "prompt eng-developer ") != 2 {
		t.Fatalf("cold issue did not traverse CreateTab, StartAgent, startup Prompt and delivery Prompt exactly once: %v", calls)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if countCaseEvents(events, "GATEWAY-COLD-001", "issue_sent") != 1 {
		t.Fatalf("cold issue did not converge to exactly one sent terminal: %+v", events)
	}
}

func TestHerdrRuntimeGatewayExecutionContextCancelsFinalPromptAndReturnsRecovery(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == "eng-developer" {
				cfg.Agents[index].ActivationPolicy = activationOnAssignment
				cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "gateway-cancel-issue.md"), "# Cancel final prompt\n")
	e.setActor(t, "zantianyou", "fixture:p2", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "case", "create", "--id", "GATEWAY-CANCEL-001", "--title", "Cancel final prompt", "--source", source)

	manager, _ := cfg.exactRule("zantianyou")
	base := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	base.snapshot = healthySnapshot(e.root, manager, "idle")
	base.nextID = 2
	base.promptWakes = true
	control := &blockingNthPromptControl{fakeHerdrControl: base, blockOn: 2}
	t.Setenv("HERDR_PANE_ID", "w-test:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w-test")

	server := e.app(t)
	server.Out, server.Err = io.Discard, io.Discard
	server.Herdr = control
	server.Identity = herdrIdentityProvider{Control: control}
	server.Transport = herdrDeliveryTransport{Control: control}
	server.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "gateway-cancel-sessions")}
	policy := gatewayTimeoutPolicy{Health: 100 * time.Millisecond, Business: 700 * time.Millisecond}

	socket := filepath.Join(e.data, "hq.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			server.handleGatewayConnectionWithTimeouts(conn, "w-test", "gateway-cancel", policy)
		}
	}()

	started := time.Now()
	err = forwardToGatewayWithTimeouts(socket,
		[]string{"issue", "--case", "GATEWAY-CANCEL-001", "--to", "eng-developer", "--next", "implement and report"},
		false, false, io.Discard, io.Discard, policy)
	if err == nil {
		t.Fatal("blocked final Prompt unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed >= policy.Business {
		t.Fatalf("dispatch context did not leave response grace: elapsed=%s business=%s err=%v", elapsed, policy.Business, err)
	}
	for _, want := range []string{"gateway 业务执行预算到期", "结果可能已部分落账或投递", "禁止直接重复执行", "hq assignment list --case GATEWAY-CANCEL-001"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("server timeout response missing %q: %v", want, err)
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway handler survived request-context cancellation")
	}
	control.mu.Lock()
	if control.attempts != 2 {
		control.mu.Unlock()
		t.Fatalf("expected startup plus final delivery Prompt attempts, got %d", control.attempts)
	}
	control.mu.Unlock()
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if countCaseEvents(events, "GATEWAY-CANCEL-001", "delivery_unknown") != 1 || countCaseEvents(events, "GATEWAY-CANCEL-001", "issue_sent") != 0 {
		t.Fatalf("canceled ambiguous final Prompt did not freeze delivery exactly once: %+v", events)
	}
}

func TestHerdrRuntimeNestedPromptTimeoutReturnsIssueRecovery(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		for index := range cfg.Agents {
			if cfg.Agents[index].Name == "eng-developer" {
				cfg.Agents[index].ActivationPolicy = activationOnAssignment
				cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "gateway-nested-timeout.md"), "# Nested prompt timeout\n")
	e.setActor(t, "zantianyou", "fixture:p2", testAgentCWD(cfg, e.root, "zantianyou"))
	runTestCommand(t, e, "case", "create", "--id", "GATEWAY-NESTED-001", "--title", "Nested prompt timeout", "--source", source)

	manager, _ := cfg.exactRule("zantianyou")
	base := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	base.snapshot = healthySnapshot(e.root, manager, "idle")
	base.nextID = 2
	base.promptWakes = true
	control := &timeoutNthPromptControl{fakeHerdrControl: base, timeout: 30 * time.Millisecond, timeoutOn: 2}
	t.Setenv("HERDR_PANE_ID", "w-test:p1")
	t.Setenv("HERDR_WORKSPACE_ID", "w-test")

	server := e.app(t)
	server.Out, server.Err = io.Discard, io.Discard
	server.Herdr = control
	server.Identity = herdrIdentityProvider{Control: control}
	server.Transport = herdrDeliveryTransport{Control: control}
	server.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "gateway-nested-timeout-sessions")}
	policy := gatewayTimeoutPolicy{Health: 100 * time.Millisecond, Business: 2 * time.Second}

	socket := filepath.Join(e.data, "hq.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			server.handleGatewayConnectionWithTimeouts(conn, "w-test", "gateway-nested", policy)
		}
	}()

	started := time.Now()
	err = forwardToGatewayWithTimeouts(socket,
		[]string{"issue", "--case", "GATEWAY-NESTED-001", "--to", "eng-developer", "--next", "implement and report"},
		false, false, io.Discard, io.Discard, policy)
	if err == nil {
		t.Fatal("nested final Prompt timeout unexpectedly succeeded")
	}
	if elapsed := time.Since(started); elapsed >= policy.executionTimeout() {
		t.Fatalf("nested Prompt timeout consumed the whole gateway execution budget: elapsed=%s execution=%s err=%v", elapsed, policy.executionTimeout(), err)
	}
	if strings.Contains(err.Error(), "gateway 业务执行预算到期") {
		t.Fatalf("nested Prompt timeout was incorrectly reported as the outer gateway deadline: %v", err)
	}
	for _, want := range []string{
		"herdr agent prompt deadline", "状态=unknown", "结果不确定", "禁止直接重复执行",
		"hq history --case GATEWAY-NESTED-001", "hq assignment list --case GATEWAY-NESTED-001",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("nested Prompt timeout response missing %q: %v", want, err)
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("gateway handler survived nested Prompt timeout")
	}
	control.mu.Lock()
	attempts := control.attempts
	control.mu.Unlock()
	if attempts != 2 {
		t.Fatalf("expected startup plus final delivery Prompt attempts, got %d", attempts)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if countCaseEvents(events, "GATEWAY-NESTED-001", "delivery_unknown") != 1 || countCaseEvents(events, "GATEWAY-NESTED-001", "issue_sent") != 0 {
		t.Fatalf("nested ambiguous Prompt did not freeze delivery exactly once: %+v", events)
	}
}

func TestGatewayConnectionParentCancelInterruptsFramingIO(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	app := &App{Out: io.Discard, Err: io.Discard}
	go func() {
		defer close(done)
		app.handleGatewayConnectionContext(ctx, serverConn, "w-test", "gateway-shutdown", gatewayTimeoutPolicy{
			Health: time.Second, Business: 10 * time.Second,
		})
	}()
	if _, err := io.WriteString(clientConn, "{"); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	cancel()
	select {
	case <-done:
		if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
			t.Fatalf("parent cancellation took too long to interrupt gateway I/O: %s", elapsed)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("gateway connection retained its socket deadline after parent cancellation")
	}
}

func TestHerdrRuntimeColdResumeUpLockHonorsRequestContext(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	held, err := app.lockUp()
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(held)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	contended, err := app.lockUpContext(ctx)
	if contended != nil {
		unlock(contended)
	}
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended cold-resume lock ignored request context: lock=%v err=%v", contended, err)
	}
	if elapsed := time.Since(started); elapsed > 300*time.Millisecond {
		t.Fatalf("contended cold-resume lock cancellation was delayed: %s", elapsed)
	}
}

func TestGatewayRequestContextCancelsOperationLedgerAndAdmissionLocks(t *testing.T) {
	e := setupTestEnv(t)
	store := NewStore(e.data)
	deliveryID := stableDeliveryID(stableCommandID("gateway-lock-context", "issue"), "eng-developer")
	heldOperation, err := store.lockOperation(operationScopeDelivery, deliveryID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	_, err = store.withRequestContext(ctx).lockOperation(operationScopeDelivery, deliveryID)
	cancel()
	heldOperation()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("operation lock ignored request context: %v", err)
	}

	releaseLedger, err := store.lock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Millisecond)
	_, err = store.withRequestContext(ctx).lock()
	cancel()
	releaseLedger()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ledger lock ignored request context: %v", err)
	}

	estop := &FileEstopStore{Root: filepath.Join(e.data, "context-estop")}
	if err := os.MkdirAll(estop.Root, 0o700); err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(estop.Root, ".lock")
	heldAdmission, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(heldAdmission.Fd()), syscall.LOCK_EX); err != nil {
		heldAdmission.Close()
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 60*time.Millisecond)
	_, err = estop.WithRuntimeAdmissionsContext(ctx, testConfig(), []RuntimeAdmissionRequest{{
		Action: runtimeAdmissionAgentPrompt, Target: "eng-developer",
	}}, nil)
	cancel()
	unlock(heldAdmission)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ESTOP admission lock ignored request context: %v", err)
	}
}

func TestHerdrRuntimeGatewayResponseTimeoutWarnsAgainstBlindRetry(t *testing.T) {
	root := shortSocketTempDir(t)
	socket := filepath.Join(root, "ambiguous.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	policy := gatewayTimeoutPolicy{Health: 20 * time.Millisecond, Business: 60 * time.Millisecond}
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(io.Discard, conn)
		time.Sleep(2 * policy.Business)
	}()
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_PANE_ID", "w1:p2")
	t.Setenv("HERDR_WORKSPACE_ID", "w1")

	err = forwardToGatewayWithTimeouts(socket,
		[]string{"issue", "--case", "GATEWAY-AMBIGUOUS-001", "--to", "eng-developer", "--next", "work"},
		false, false, io.Discard, io.Discard, policy)
	if err == nil || exitCodeForError(err) != exitConflict {
		t.Fatalf("ambiguous gateway timeout was not a conflict: %v", err)
	}
	for _, want := range []string{
		"未收到可验证的业务终态", "结果不确定", "禁止直接重复执行",
		"hq history --case GATEWAY-AMBIGUOUS-001", "hq assignment list --case GATEWAY-AMBIGUOUS-001",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("gateway ambiguity error missing %q: %v", want, err)
		}
	}
	<-done
}

func TestHerdrRuntimeGatewaySilentListenerAndLongPathFallback(t *testing.T) {
	root := shortSocketTempDir(t)
	socket := filepath.Join(root, "silent.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			defer conn.Close()
			time.Sleep(gatewayHealthIOTimeout + 100*time.Millisecond)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	health := (unixGatewayPinger{}).Ping(ctx, socket, "w-test")
	if health.OK || !health.Connected {
		t.Fatalf("silent listener must be connected but not healthy: %+v", health)
	}

	base := canonicalTestTempDir(t)
	directSuffixBytes := gatewaySocketMaxBytes - len([]byte(base)) - len([]byte("/hq.sock")) - 1
	if directSuffixBytes < 1 {
		t.Fatalf("test temp root too long for direct boundary: %s", base)
	}
	directRoot := filepath.Join(base, strings.Repeat("d", directSuffixBytes))
	directSocket, err := gatewaySocketPath(directRoot)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(directRoot, "hq.sock"); directSocket != want || len([]byte(directSocket)) != gatewaySocketMaxBytes {
		t.Fatalf("OS boundary should keep direct endpoint: got=%s bytes=%d want=%s bytes=%d", directSocket, len([]byte(directSocket)), want, gatewaySocketMaxBytes)
	}

	longRoot := directRoot + "x"
	shortSocket, err := gatewaySocketPath(longRoot)
	if err != nil {
		t.Fatal(err)
	}
	if shortSocket == filepath.Join(longRoot, "hq.sock") || len([]byte(shortSocket)) > gatewaySocketMaxBytes {
		t.Fatalf("long company root did not receive a bounded endpoint: %s (%d bytes)", shortSocket, len([]byte(shortSocket)))
	}
	if again, err := gatewaySocketPath(longRoot); err != nil || again != shortSocket {
		t.Fatalf("long-path endpoint is not deterministic: first=%s second=%s err=%v", shortSocket, again, err)
	}
}

func TestHerdrRuntimeGatewayServesLongCompanyRootThroughPrivateEndpoint(t *testing.T) {
	base := canonicalTestTempDir(t)
	longRoot := filepath.Join(base, strings.Repeat("x", gatewaySocketMaxBytes))
	socket, err := gatewaySocketPath(longRoot)
	if err != nil {
		t.Fatal(err)
	}
	if socket == filepath.Join(longRoot, "hq.sock") {
		t.Fatalf("fixture did not exceed direct Unix socket limit: %s", socket)
	}
	t.Cleanup(func() { _ = os.Remove(socket) })

	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		Office: filepath.Join(base, "office"), HQRoot: base, DataDir: longRoot,
		Config: testConfig(), Store: NewStore(longRoot), GatewayContext: ctx,
		Estop: &FileEstopStore{Root: filepath.Join(longRoot, "estop")},
		Out:   io.Discard, Err: io.Discard,
	}
	done := make(chan error, 1)
	go func() {
		done <- app.serveGatewayCommand([]string{"--workspace-id", "w-long", "--server-id", "gateway-long"})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		health := (unixGatewayPinger{}).Ping(context.Background(), socket, "w-long")
		if health.OK {
			if health.ServerID != "gateway-long" {
				t.Fatalf("unexpected long-path gateway identity: %+v", health)
			}
			break
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("long-path gateway did not become ready: %+v", health)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("long-path gateway shutdown: %v", err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("long-path endpoint remained after shutdown: %v", err)
	}
}

func TestHerdrRuntimeGatewayStaleSocketNeedsDoubleProtocolCheckAndOwnedCleanup(t *testing.T) {
	root := shortSocketTempDir(t)
	socket := filepath.Join(root, "hq.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(socket); err != nil {
		t.Fatalf("stale fixture missing: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	app := &App{
		Office: filepath.Join(root, "office"), HQRoot: root, DataDir: root,
		Config: testConfig(), Store: NewStore(root), GatewayContext: ctx,
		Estop: &FileEstopStore{Root: filepath.Join(root, "estop")},
		Out:   io.Discard, Err: io.Discard,
	}
	done := make(chan error, 1)
	go func() {
		done <- app.serveGatewayCommand([]string{"--workspace-id", "w-test", "--server-id", "gateway-owned"})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		health := (unixGatewayPinger{}).Ping(context.Background(), socket, "w-test")
		if health.OK {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stale socket did not recover: %+v", health)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("serve cleanup failed: %v", err)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("owned socket remained after shutdown: %v", err)
	}
}

func TestHerdrRuntimeGatewayHandlerConcurrencyLimitFailsClosed(t *testing.T) {
	root := shortSocketTempDir(t)
	socket := filepath.Join(root, "hq.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	app := &App{Out: io.Discard, Err: io.Discard}
	done := make(chan error, 1)
	go func() { done <- app.serveGateway(ctx, listener, "w-test", "gateway-limit") }()
	blockers := make([]net.Conn, 0, gatewayMaxHandlers)
	for i := 0; i < gatewayMaxHandlers; i++ {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.WriteString(conn, "{")
		blockers = append(blockers, conn)
	}
	defer func() {
		for _, conn := range blockers {
			_ = conn.Close()
		}
		cancel()
	}()
	time.Sleep(100 * time.Millisecond)
	overflow, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	_ = overflow.SetDeadline(time.Now().Add(time.Second))
	request := gatewayRequest{Version: gatewayProtocolVersion, Type: "ping", Workspace: "w-test"}
	writeErr := json.NewEncoder(overflow).Encode(request)
	if writeErr == nil {
		if unix, ok := overflow.(*net.UnixConn); ok {
			_ = unix.CloseWrite()
		}
		var response gatewayResponse
		if err := json.NewDecoder(overflow).Decode(&response); err != nil {
			t.Fatal(err)
		}
		if response.OK || !strings.Contains(response.Error, "并发上限") {
			t.Fatalf("overflow response=%+v", response)
		}
	} else if !strings.Contains(writeErr.Error(), "not connected") && !strings.Contains(writeErr.Error(), "broken pipe") {
		t.Fatal(writeErr)
	}
	_ = overflow.Close()
	for _, conn := range blockers {
		_ = conn.Close()
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("serve exit=%v", err)
	}
}
