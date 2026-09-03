package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type profileReaderControl struct {
	*fakeHerdrControl
	mu            sync.Mutex
	terminal      []byte
	afterStart    []byte
	readErr       error
	startProfiles [][]string
}

func (c *profileReaderControl) ReadAgent(context.Context, string) ([]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.readErr != nil {
		return nil, c.readErr
	}
	return append([]byte(nil), c.terminal...), nil
}

func (c *profileReaderControl) StartAgent(ctx context.Context, name, kind, paneID string, native []string) HerdrMutationResult {
	result := c.fakeHerdrControl.StartAgent(ctx, name, kind, paneID, native)
	if result.Err == nil && result.Outcome == herdrConfirmed {
		c.fakeHerdrControl.mu.Lock()
		for index := range c.fakeHerdrControl.snapshot.Panes {
			pane := &c.fakeHerdrControl.snapshot.Panes[index]
			if pane.ID == paneID {
				pane.TerminalID = "terminal-profile-new"
				pane.Revision = 2
			}
		}
		for index := range c.fakeHerdrControl.snapshot.Agents {
			agent := &c.fakeHerdrControl.snapshot.Agents[index]
			if agent.Name == name && agent.PaneID == paneID {
				agent.TerminalID = "terminal-profile-new"
				agent.Revision = 2
				agent.AgentSession = &HerdrAgentSession{Source: "native", Agent: name, Kind: kind, Value: "codex-profile-new"}
			}
		}
		c.fakeHerdrControl.mu.Unlock()
		c.mu.Lock()
		c.startProfiles = append(c.startProfiles, append([]string(nil), native...))
		if len(c.afterStart) != 0 {
			c.terminal = append([]byte(nil), c.afterStart...)
		}
		c.mu.Unlock()
	}
	return result
}

func configuredCodexProfile() map[string]RuntimeProfilePolicy {
	return map[string]RuntimeProfilePolicy{
		"codex": {Model: "gpt-5.6-sol", ReasoningEffort: "medium", OnDrift: runtimeProfileDriftRestartIdle},
	}
}

func TestRuntimeProfileConfigAndNativeArgsAreAgentReadable(t *testing.T) {
	cfg := testConfig()
	cfg.RuntimeProfiles = configuredCodexProfile()
	for index := range cfg.Agents {
		cfg.Agents[index].AgentArgs = []string{"-c", `model_reasoning_effort="medium"`, "-c", "feature_toggle=true"}
		cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
	}
	if err := validateConfig(cfg); err != nil {
		t.Fatal(err)
	}
	rule, _ := cfg.exactRule("zantianyou")
	args, err := nativeAgentArgsForConfig(cfg, rule)
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--model gpt-5.6-sol") || strings.Count(joined, "model_reasoning_effort") != 1 {
		t.Fatalf("runtime profile not compiled exactly once: %v", args)
	}

	cfg.RuntimeProfiles["codex"] = RuntimeProfilePolicy{Model: "gpt-5.6-sol", ReasoningEffort: "high", OnDrift: runtimeProfileDriftRestartIdle}
	err = validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "与 agent_args 中显式 model_reasoning_effort=medium 冲突") || !strings.Contains(err.Error(), "改成同一值") {
		t.Fatalf("expected corrective conflict error, got %v", err)
	}

	cfg = testConfig()
	cfg.RuntimeProfiles = map[string]RuntimeProfilePolicy{"codex": {Model: "gpt-5.6-sol", ReasoningEffort: "medium", OnDrift: "kill_now"}}
	err = validateConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "report|restart_idle") || !strings.Contains(err.Error(), "安全 idle|done 边界") {
		t.Fatalf("expected agent-readable drift policy error, got %v", err)
	}
}

func TestObservedCodexRuntimeProfileUsesLastFooter(t *testing.T) {
	raw := []byte("old transcript says gpt-5.6-sol medium · not a footer\n" +
		"gpt-5.6-luna low · 74% left · ~/work\n› Ask Codex to do anything\n")
	observed, err := observedCodexRuntimeProfile(raw)
	if err != nil || observed.Model != "gpt-5.6-luna" || observed.ReasoningEffort != "low" {
		t.Fatalf("observed=%+v err=%v", observed, err)
	}
	if _, err := observedCodexRuntimeProfile([]byte("› Ask Codex to do anything\n")); err == nil || !strings.Contains(err.Error(), "MODEL medium") {
		t.Fatalf("expected corrective footer error, got %v", err)
	}
}

func profileRepairFixture(t *testing.T, status string) (testEnv, Config, AgentRule, *App, *profileReaderControl, SessionEvent) {
	t.Helper()
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		cfg.RuntimeProfiles = configuredCodexProfile()
		for index := range cfg.Agents {
			cfg.Agents[index].PermissionMode = "yolo"
			cfg.Agents[index].AgentArgs = []string{"-c", `model_reasoning_effort="medium"`}
			cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	rule, _ := cfg.exactRule("zantianyou")
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, rule, status)
	control.snapshot.Panes[0].TerminalID = "terminal-profile-old"
	control.snapshot.Panes[0].Revision = 1
	control.snapshot.Agents[0].TerminalID = "terminal-profile-old"
	control.snapshot.Agents[0].Revision = 1
	control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: rule.Name, Kind: "codex", Value: "codex-profile-old"}
	control.nextID = 2
	reader := &profileReaderControl{fakeHerdrControl: control,
		terminal:   []byte("gpt-5.6-luna low · 70% left · ~/work\n› Ask Codex to do anything\n"),
		afterStart: []byte("gpt-5.6-sol medium · 100% left · ~/work\n› Ask Codex to do anything\n")}
	sessions := &FileSessionStore{Root: filepath.Join(e.data, "runtime-profile-sessions")}
	created := HerdrTabCreated{Tab: control.snapshot.Tabs[0], Pane: control.snapshot.Panes[0]}
	started, err := newSessionEvent(time.Now().Add(-time.Minute), sessionStarted, created, "w-test", rule, "hq-up", "test runtime", testAgentCWD(cfg, e.root, rule.Name))
	if err == nil {
		binding, bindingErr := ResolveLiveBinding(control.snapshot, cfg, e.root, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
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
	app.Config = cfg
	app.Herdr = reader
	app.Sessions = sessions
	return e, cfg, rule, app, reader, started
}

func TestRuntimeProfilePatrolReportsDriftAndWorkingRepairDefers(t *testing.T) {
	e, cfg, rule, app, reader, _ := profileRepairFixture(t, "working")
	report, err := (&PatrolService{Herdr: reader}).Run(context.Background(), cfg, e.root, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range report.Findings {
		if finding.Agent == rule.Name && finding.SignalType == "runtime_profile_mismatch" && strings.Contains(finding.Message, "idle|done") {
			found = true
		}
	}
	if !found || report.Drift == 0 {
		t.Fatalf("profile drift missing from patrol: %+v", report)
	}
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.fakeHerdrControl.mu.Lock()
	calls := strings.Join(reader.fakeHerdrControl.calls, "\n")
	reader.fakeHerdrControl.mu.Unlock()
	if strings.Contains(calls, "tab close") || strings.Contains(calls, "agent start") {
		t.Fatalf("working runtime was interrupted:\n%s", calls)
	}
}

func TestRuntimeProfileDriftRepairsAtIdleAndPreservesSeat(t *testing.T) {
	_, _, rule, app, reader, oldStarted := profileRepairFixture(t, "idle")
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.fakeHerdrControl.mu.Lock()
	calls := strings.Join(reader.fakeHerdrControl.calls, "\n")
	reader.fakeHerdrControl.mu.Unlock()
	if !strings.Contains(calls, "tab close "+oldStarted.TabID) || !strings.Contains(calls, "agent start "+rule.Name+" codex") ||
		!strings.Contains(calls, "--model gpt-5.6-sol") || strings.Count(calls, "model_reasoning_effort=\"medium\"") == 0 ||
		!strings.Contains(calls, "--dangerously-bypass-approvals-and-sandbox") || !strings.Contains(calls, "--dangerously-bypass-hook-trust") ||
		!strings.Contains(calls, "trigger=runtime_profile_drift") {
		t.Fatalf("profile repair did not close/start/recover with explicit profile:\n%s", calls)
	}
	events, err := app.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil {
		t.Fatal(err)
	}
	var oldStopped, newStarted, recoverySent bool
	for _, event := range events {
		oldStopped = oldStopped || (event.SessionID == oldStarted.SessionID && event.Type == sessionStopped && strings.HasPrefix(event.Reason, profileRepairPendingReason))
		newStarted = newStarted || (event.SessionID != oldStarted.SessionID && event.Type == sessionStarted && event.Actor == "hq-runtime-profile")
		recoverySent = recoverySent || (event.SessionID != oldStarted.SessionID && event.Type == sessionProfileRecoverySent)
	}
	if !oldStopped || !newStarted || !recoverySent {
		t.Fatalf("profile repair audit incomplete: %+v", events)
	}

	reader.fakeHerdrControl.mu.Lock()
	before := strings.Count(strings.Join(reader.fakeHerdrControl.calls, "\n"), "tab close")
	reader.fakeHerdrControl.mu.Unlock()
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.fakeHerdrControl.mu.Lock()
	after := strings.Count(strings.Join(reader.fakeHerdrControl.calls, "\n"), "tab close")
	reader.fakeHerdrControl.mu.Unlock()
	if after != before {
		t.Fatalf("healthy repaired runtime was replaced again: before=%d after=%d", before, after)
	}
}

func TestRuntimeProfileAmbiguousCloseNeverStartsSecondRuntime(t *testing.T) {
	_, _, rule, app, reader, _ := profileRepairFixture(t, "idle")
	reader.closeMutates = false
	reader.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("close response timed out")}
	err := app.recoverRuntimeProfileDriftsOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "禁止自动启动第二个 runtime") {
		t.Fatalf("expected ambiguous close fence, got %v", err)
	}
	reader.fakeHerdrControl.mu.Lock()
	firstCalls := strings.Join(reader.fakeHerdrControl.calls, "\n")
	reader.fakeHerdrControl.mu.Unlock()
	if strings.Contains(firstCalls, "agent start") {
		t.Fatalf("ambiguous close started a second runtime:\n%s", firstCalls)
	}
	err = app.recoverRuntimeProfileDriftsOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--retry-unknown") {
		t.Fatalf("expected explicit retry correction, got %v", err)
	}
	reader.fakeHerdrControl.mu.Lock()
	secondCalls := strings.Join(reader.fakeHerdrControl.calls, "\n")
	reader.fakeHerdrControl.mu.Unlock()
	if strings.Count(secondCalls, "tab close") != 1 {
		t.Fatalf("automatic watcher retried ambiguous close:\n%s", secondCalls)
	}
	if !strings.Contains(err.Error(), rule.Name) {
		t.Fatalf("correction omitted exact seat: %v", err)
	}

	// A later snapshot proving the old tab absent is stronger than the timed-out
	// close response. The watcher may then roll the same pending repair forward
	// without issuing CloseTab a second time.
	reader.fakeHerdrControl.mu.Lock()
	reader.fakeHerdrControl.snapshot.Tabs = nil
	reader.fakeHerdrControl.snapshot.Panes = nil
	reader.fakeHerdrControl.snapshot.Agents = nil
	reader.fakeHerdrControl.closeMutates = true
	reader.fakeHerdrControl.closeOutcome = HerdrMutationResult{}
	reader.fakeHerdrControl.mu.Unlock()
	if err := app.recoverRuntimeProfileDriftsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	reader.fakeHerdrControl.mu.Lock()
	rolledForwardCalls := strings.Join(reader.fakeHerdrControl.calls, "\n")
	reader.fakeHerdrControl.mu.Unlock()
	if strings.Count(rolledForwardCalls, "tab close") != 1 || !strings.Contains(rolledForwardCalls, "agent start "+rule.Name+" codex") {
		t.Fatalf("absent old tab did not roll pending repair forward exactly once:\n%s", rolledForwardCalls)
	}
}
