package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
)

type startupIncarnationDriftControl struct {
	*fakeHerdrControl
	snapshotCalls int
}

func (c *startupIncarnationDriftControl) Snapshot(ctx context.Context, scope HerdrSnapshotScope) (HerdrSnapshot, error) {
	snapshot, err := c.fakeHerdrControl.Snapshot(ctx, scope)
	c.snapshotCalls++
	if err == nil && len(snapshot.Agents) == 1 && c.snapshotCalls >= 2 {
		snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "codex", Agent: "codex", Kind: "id", Value: "startup-session-a"}
		if c.snapshotCalls >= 3 {
			snapshot.Agents[0].AgentSession.Value = "startup-session-b"
		}
	}
	return snapshot, err
}

func lifecycleConfig() Config {
	cfg := testConfig()
	for index := range cfg.Agents {
		if cfg.Agents[index].Name == "eng-developer" {
			cfg.Agents[index].ActivationPolicy = activationAlways
		} else {
			cfg.Agents[index].ActivationPolicy = activationManual
		}
		cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
	}
	return cfg
}

func patrolHasSignal(report PatrolReport, signal string) bool {
	for _, finding := range report.Findings {
		if finding.SignalType == signal {
			return true
		}
	}
	return false
}

func lifecyclePromptCalls(calls []string) []string {
	var prompts []string
	for _, call := range calls {
		if strings.HasPrefix(call, "prompt ") {
			prompts = append(prompts, call)
		}
	}
	return prompts
}

func TestStartupEnvelopeDistinguishesOwnerGovernanceFromManagerIssue(t *testing.T) {
	cfg := testConfig()
	secretary, ok := cfg.exactRule("penny")
	if !ok {
		t.Fatal("test config missing approval witness")
	}
	secretaryEnvelope := startupEnvelopeWithBinary(secretary, cfg.ownerPrincipal(), "/company/hq")
	for _, required := range []string{"等待公司所有者的正式治理输入", "不得把普通 Herdr prompt 当作已批准的业务事实"} {
		if !strings.Contains(secretaryEnvelope, required) {
			t.Fatalf("secretary startup envelope missing %q: %s", required, secretaryEnvelope)
		}
	}
	if strings.Contains(secretaryEnvelope, "等待直属经理通过 hq issue") {
		t.Fatalf("secretary was incorrectly told to wait for a direct manager: %s", secretaryEnvelope)
	}

	worker, ok := cfg.exactRule("eng-developer")
	if !ok {
		t.Fatal("test config missing worker")
	}
	workerEnvelope := startupEnvelopeWithBinary(worker, cfg.ownerPrincipal(), "/company/hq")
	if !strings.Contains(workerEnvelope, "等待直属经理通过 hq issue") {
		t.Fatalf("worker startup envelope lost manager issue instruction: %s", workerEnvelope)
	}
}

func TestAgentLifecycleDoneIsHealthyOnlyWhileSessionAndInteractiveContractsHold(t *testing.T) {
	root := canonicalTestTempDir(t)
	cfg := lifecycleConfig()
	materializeTestWorkstations(t, root, cfg)
	rule, _ := cfg.ruleFor("eng-developer")

	for _, status := range []string{"working", "idle", "done"} {
		t.Run("healthy_"+status, func(t *testing.T) {
			control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
			control.snapshot = healthySnapshot(root, rule, status)
			report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, 0)
			if err != nil || report.Drift != 0 || report.Orphan != 0 || report.DeadCandidate != 0 {
				t.Fatalf("healthy standby rejected: report=%+v err=%v", report, err)
			}
			if matched, mismatch := exactLiveMatch(control.snapshot, "w-test", rule, root); !matched {
				t.Fatalf("exact live match rejected status=%s: %s", status, mismatch)
			}
		})
	}

	tests := []struct {
		name   string
		signal string
		edit   func(*HerdrSnapshot)
	}{
		{"missing_session", "roster_missing", func(snapshot *HerdrSnapshot) { snapshot.Agents = nil }},
		{"interactive_false", "interactive_not_ready", func(snapshot *HerdrSnapshot) { snapshot.Agents[0].InteractiveReady = false }},
		{"pane_missing", "missing_pane", func(snapshot *HerdrSnapshot) { snapshot.Panes = nil }},
		{"cwd_drift", "cwd_mismatch", func(snapshot *HerdrSnapshot) { snapshot.Agents[0].CWD = filepath.Join(root, "wrong") }},
		{"kind_drift", "kind_mismatch", func(snapshot *HerdrSnapshot) {
			snapshot.Agents[0].Kind = "kimi"
		}},
		{"workspace_drift", "roster_missing", func(snapshot *HerdrSnapshot) {
			snapshot.Workspaces = append(snapshot.Workspaces, HerdrWorkspace{ID: "w-other", Label: "other"})
			snapshot.Agents[0].WorkspaceID = "w-other"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			snapshot := healthySnapshot(root, rule, "done")
			test.edit(&snapshot)
			control := newFakeHerdrControl(root, cfg.WorkspaceLabel)
			control.snapshot = snapshot
			report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, root, 0)
			if err != nil {
				t.Fatalf("patrol failed before structured diagnosis: %v", err)
			}
			if report.Drift == 0 && report.Orphan == 0 {
				t.Fatalf("broken runtime contract passed: %+v", report)
			}
			if !patrolHasSignal(report, test.signal) {
				t.Fatalf("missing fail-closed signal %q: %+v", test.signal, report.Findings)
			}
			if matched, _ := exactLiveMatch(snapshot, "w-test", rule, root); matched {
				t.Fatalf("broken runtime contract remained exact-live")
			}
		})
	}
}

func TestStartupPromptRejectsRuntimeIncarnationDriftAfterSessionRecord(t *testing.T) {
	e := setupTestEnv(t)
	app := e.app(t)
	rule, _ := app.Config.exactRule("eng-developer")
	control := &startupIncarnationDriftControl{fakeHerdrControl: newFakeHerdrControl(e.root, app.Config.WorkspaceLabel)}
	app.Herdr = control
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.data, "startup-drift-sessions")}

	err := app.startHQAgentAdmitted(context.Background(), "w-test", rule)
	var partial *PartialStartError
	if !errors.As(err, &partial) || !strings.Contains(err.Error(), "runtime incarnation 漂移") || partial.SessionID == "" {
		t.Fatalf("err=%T %v partial=%+v", err, err, partial)
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	if prompts := lifecyclePromptCalls(calls); len(prompts) != 0 {
		t.Fatalf("drifted startup reached Prompt: %v", prompts)
	}
	sessions, listErr := app.Sessions.List(SessionFilter{Agent: rule.Name})
	if listErr != nil || len(sessions) != 1 || sessions[0].SessionID != partial.SessionID || sessions[0].Type != "started" {
		t.Fatalf("session reconcile evidence=%+v err=%v", sessions, listErr)
	}
}

func TestAgentLifecycleUpWritesZeroBusinessEventsAndNativeIssueWakesStandby(t *testing.T) {
	e := setupTestEnv(t)
	cfg := lifecycleConfig()
	writeConfigFixture(t, e.config, cfg)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.promptWakes = true
	identity := &fakeIdentityProvider{actors: map[string]Actor{}}
	store := NewStore(e.data)
	app, err := newAppWithDependencies(runtimePaths{
		Office: e.office, HQRoot: e.root, DataDir: e.data, ConfigPath: e.config, HerdrBin: e.herdr,
	}, cfg, globalOptions{}, AppDependencies{
		Store: store, Identity: identity, Transport: herdrDeliveryTransport{Control: control}, Herdr: control,
		Sessions: &FileSessionStore{Root: filepath.Join(e.data, "sessions")},
	}, io.Discard, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	app.FromGateway, app.Direct = false, true
	if err := app.runUp([]string{"--no-gateway"}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadAll(cfg)
	if err != nil || len(events) != 0 {
		t.Fatalf("hq up wrote business events before first issue: count=%d err=%v events=%+v", len(events), err, events)
	}
	control.mu.Lock()
	startupCalls := append([]string(nil), control.calls...)
	if len(control.snapshot.Agents) != 1 {
		control.mu.Unlock()
		t.Fatalf("unexpected started agents: %+v", control.snapshot.Agents)
	}
	control.snapshot.Agents[0].Status = "done"
	control.calls = nil
	control.mu.Unlock()
	startup := strings.Join(startupCalls, "\n")
	for _, required := range []string{"等待直属经理通过 hq issue", "首个 durable case", "不得运行 hq/herdr 业务命令", "不得发送报到、回铃或消息"} {
		if !strings.Contains(startup, required) {
			t.Fatalf("startup envelope missing zero-business guard %q: %s", required, startup)
		}
	}
	report, err := (&PatrolService{Herdr: control}).Run(context.Background(), cfg, e.root, 0)
	if err != nil || report.Drift != 0 || report.Orphan != 0 {
		t.Fatalf("done standby not healthy after cold start: report=%+v err=%v", report, err)
	}
	control.mu.Lock()
	addOperationsLive(&control.snapshot, cfg, e.root, "zantianyou", "idle", "lifecycle-manager")
	control.mu.Unlock()

	managerPane, workerPane := "w-test:manager", control.snapshot.Agents[0].PaneID
	identity.actors[managerPane] = actorFor(cfg, "zantianyou", managerPane, testAgentCWD(cfg, e.root, "zantianyou"))
	identity.actors[workerPane] = actorFor(cfg, "eng-developer", workerPane, testAgentCWD(cfg, e.root, "eng-developer"))
	app.FromGateway, app.Direct, app.CallerPane = true, false, managerPane
	source := writeTestFile(t, filepath.Join(e.office, "lifecycle-source.md"), "# lifecycle test source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "lifecycle-result.md"), "# lifecycle test result\n")
	if err := app.run([]string{"case", "create", "--id", "LIFECYCLE-WAKE-PARENT", "--title", "wake standby parent", "--project", "lifecycle-test", "--source", source}); err != nil {
		t.Fatal(err)
	}
	if err := app.run([]string{"case", "create", "--id", "LIFECYCLE-WAKE-001", "--parent", "LIFECYCLE-WAKE-PARENT", "--title", "wake standby child", "--source", source}); err != nil {
		t.Fatal(err)
	}
	if err := app.run([]string{"issue", "--case", "LIFECYCLE-WAKE-001", "--to", "eng-developer", "--next", "原生收件并实施"}); err != nil {
		t.Fatal(err)
	}
	events, err = store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var issueSent Event
	for _, event := range events {
		if event.Type == "issue_sent" {
			issueSent = event
		}
	}
	if issueSent.ID == "" {
		t.Fatal("native issue did not reach sent terminal")
	}
	control.mu.Lock()
	prompts := lifecyclePromptCalls(control.calls)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "prompt eng-developer ") || control.snapshot.Agents[0].Status != "working" {
		calls, status := append([]string(nil), control.calls...), control.snapshot.Agents[0].Status
		control.mu.Unlock()
		t.Fatalf("durable issue did not natively wake standby: calls=%v status=%s", calls, status)
	}
	control.mu.Unlock()

	app.CallerPane = workerPane
	if err := app.run([]string{"accept", "--event", issueSent.ID, "--next", "开始实施"}); err != nil {
		t.Fatal(err)
	}
	if err := app.run([]string{"report", "--case", "LIFECYCLE-WAKE-001", "--result", "completed", "--artifact", artifact, "--next", "经理核验"}); err != nil {
		t.Fatal(err)
	}
	events, err = store.ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	reported := false
	for _, event := range events {
		reported = reported || event.Type == "report_sent"
	}
	if !reported {
		t.Fatal("woken standby did not complete accept/report")
	}
	control.mu.Lock()
	calls := append([]string(nil), control.calls...)
	control.mu.Unlock()
	prompts = lifecyclePromptCalls(calls)
	if len(prompts) != 3 || !strings.HasPrefix(prompts[1], "prompt zantianyou ") || !strings.HasPrefix(prompts[2], "prompt zantianyou ") {
		t.Fatalf("accept/report did not stay on native delivery chain: %v", calls)
	}
}
