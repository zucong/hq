package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fallbackReaderControl struct {
	*fakeHerdrControl
	terminal []byte
}

func (c *fallbackReaderControl) ReadAgent(context.Context, string) ([]byte, error) {
	return append([]byte(nil), c.terminal...), nil
}

func (c *fallbackReaderControl) StartAgent(ctx context.Context, name, kind, paneID string, native []string) HerdrMutationResult {
	result := c.fakeHerdrControl.StartAgent(ctx, name, kind, paneID, native)
	if result.Err != nil || result.Outcome != herdrConfirmed {
		return result
	}
	c.fakeHerdrControl.mu.Lock()
	defer c.fakeHerdrControl.mu.Unlock()
	for index := range c.fakeHerdrControl.snapshot.Panes {
		if c.fakeHerdrControl.snapshot.Panes[index].ID == paneID {
			c.fakeHerdrControl.snapshot.Panes[index].TerminalID = "terminal-fallback-2"
			c.fakeHerdrControl.snapshot.Panes[index].Revision = 2
		}
	}
	for index := range c.fakeHerdrControl.snapshot.Agents {
		agent := &c.fakeHerdrControl.snapshot.Agents[index]
		if agent.Name == name && agent.PaneID == paneID {
			agent.TerminalID = "terminal-fallback-2"
			agent.Revision = 2
			agent.AgentSession = &HerdrAgentSession{Source: "native", Agent: name, Kind: kind, Value: "grok-session-2"}
		}
	}
	return result
}

func TestTerminalShowsContentSafeguardRequiresTerminalRefusalState(t *testing.T) {
	valid := []byte("• Ran checks\nⓘ This content can't be shown\n  We take extra caution with cybersecurity requests.\n  Trusted Access: https://chatgpt.com/cyber/\n› Ask Codex to do anything\n")
	if !terminalShowsContentSafeguard(valid) {
		t.Fatal("exact terminal refusal was not detected")
	}
	if terminalShowsContentSafeguard([]byte("task text quotes: This content can't be shown and Trusted Access:")) {
		t.Fatal("quoted marker without provider refusal/footer was accepted")
	}
}

func TestRuntimeRecoveryWorkFiltersHistoricalStatesAndBoundsManifest(t *testing.T) {
	e := setupTestEnv(t)
	manager := "zantianyou"
	worker := "eng-developer"
	e.setActor(t, manager, "recovery:manager", filepath.Join(e.root, "engineering"))
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "runtime-recovery-source.md"), "# runtime recovery source\n")
	artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "runtime-recovery-artifact.md"), "# runtime recovery artifact\n")

	runTestCommand(t, e, "case", "create", "--id", "RECOVERY-ROOT", "--title", "Recovery root", "--project", "virtual-company-v2", "--source", source)
	runTestCommand(t, e, "case", "create", "--id", "RECOVERY-HISTORICAL", "--parent", "RECOVERY-ROOT", "--title", "Historical accepted case", "--source", source)
	runTestCommand(t, e, "issue", "--case", "RECOVERY-HISTORICAL", "--to", worker, "--next", "Complete the historical case")
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issued := latestCaseEvent(events, "RECOVERY-HISTORICAL", "issue_sent")
	e.setActor(t, worker, "recovery:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issued.ID, "--next", "Execute")
	runTestCommand(t, e, "report", "--case", "RECOVERY-HISTORICAL", "--result", "completed", "--artifact", artifact, "--verify", "verified", "--next", "Review")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	reported := latestCaseEvent(events, "RECOVERY-HISTORICAL", "report_sent")
	e.setActor(t, manager, "recovery:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", reported.ID, "--next", "Accepted")

	// A submitted assignment is awaiting its reviewer, not more work from the
	// assignee, so it must not be reconstructed as active assignee work.
	runTestCommand(t, e, "case", "create", "--id", "RECOVERY-SUBMITTED", "--parent", "RECOVERY-ROOT", "--title", "Submitted case", "--source", source)
	runTestCommand(t, e, "issue", "--case", "RECOVERY-SUBMITTED", "--to", worker, "--next", "Submit for review")
	events, err = NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issued = latestCaseEvent(events, "RECOVERY-SUBMITTED", "issue_sent")
	e.setActor(t, worker, "recovery:worker", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, "accept", "--event", issued.ID, "--next", "Execute")
	runTestCommand(t, e, "report", "--case", "RECOVERY-SUBMITTED", "--result", "completed", "--artifact", artifact, "--verify", "verified", "--next", "Review")
	workerWork, err := e.app(t).runtimeRecoveryWorkFor(worker)
	if err != nil {
		t.Fatal(err)
	}
	if !workerWork.empty() || workerWork.omitted != 0 {
		t.Fatalf("submitted assignment was falsely actionable for assignee: %+v", workerWork)
	}

	e.setActor(t, manager, "recovery:manager", filepath.Join(e.root, "engineering"))
	for index := 0; index < maxRuntimeRecoveryItems+1; index++ {
		caseID := fmt.Sprintf("RECOVERY-OPEN-%02d", index)
		runTestCommand(t, e, "case", "create", "--id", caseID, "--parent", "RECOVERY-ROOT", "--title", caseID, "--source", source)
	}

	work, err := e.app(t).runtimeRecoveryWorkFor(manager)
	if err != nil {
		t.Fatal(err)
	}
	if len(work.assignments) != 0 || len(work.cases) != maxRuntimeRecoveryItems || work.omitted != 2 {
		t.Fatalf("unexpected bounded recovery work: assignments=%d cases=%d omitted=%d", len(work.assignments), len(work.cases), work.omitted)
	}
	for _, state := range work.cases {
		if state.Status != string(statusOpen) || state.ID == "RECOVERY-HISTORICAL" || state.ID == "RECOVERY-SUBMITTED" {
			t.Fatalf("historical/non-open case leaked into recovery manifest: %+v", state)
		}
	}
	prompt := runtimeProfileRecoveryEnvelope(AgentRule{Name: manager}, runtimeProfile{Model: "sol", ReasoningEffort: "medium"}, runtimeProfile{Model: "luna", ReasoningEffort: "low"}, work)
	if strings.Contains(prompt, "RECOVERY-HISTORICAL") || strings.Contains(prompt, "RECOVERY-SUBMITTED") ||
		!strings.Contains(prompt, "另有 2 项 actionable durable work 未在本信封展开") ||
		strings.Count(prompt, "ACTIVE_OWNED_CASE") != maxRuntimeRecoveryItems {
		t.Fatalf("runtime recovery prompt was misleading or unbounded:\n%s", prompt)
	}
}

func TestRuntimeFallbackReplacesCodexAndRecoversDurableAssignment(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		cfg.RuntimeFallback = &RuntimeFallbackPolicy{
			Auto: true, Trigger: "content_safeguard", FromKind: "codex", ToKind: "grok",
			PermissionMode: "yolo", AgentArgs: []string{"--always-approve"},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	hydrateRuntimeFallback(&cfg)
	manager, _ := cfg.exactRule("zantianyou")
	worker, _ := cfg.exactRule("eng-developer")
	e.setActor(t, manager.Name, "fallback:manager", testAgentCWD(cfg, e.root, manager.Name))
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "fallback-source.md"), "# fallback source\n")
	runTestCommand(t, e, "case", "create", "--id", "RUNTIME-FALLBACK-001", "--title", "provider fallback", "--source", source)
	runTestCommand(t, e, "issue", "--case", "RUNTIME-FALLBACK-001", "--to", worker.Name, "--next", "continue from durable state")

	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, worker, "idle")
	control.snapshot.Panes[0].TerminalID = "terminal-codex-1"
	control.snapshot.Panes[0].Revision = 1
	control.snapshot.Agents[0].TerminalID = "terminal-codex-1"
	control.snapshot.Agents[0].Revision = 1
	control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: worker.Name, Kind: "codex", Value: "codex-session-1"}
	control.nextID = 2
	reader := &fallbackReaderControl{fakeHerdrControl: control, terminal: []byte("ⓘ This content can't be shown\nWe take extra caution with cybersecurity requests.\nTrusted Access: https://chatgpt.com/cyber/\n› Ask Codex to do anything\n")}
	sessions := &FileSessionStore{Root: filepath.Join(e.data, "runtime-fallback-sessions")}
	created := HerdrTabCreated{Tab: control.snapshot.Tabs[0], Pane: control.snapshot.Panes[0]}
	started, err := newSessionEvent(time.Now().Add(-time.Minute), sessionStarted, created, "w-test", worker, "hq-up", "test primary runtime", testAgentCWD(cfg, e.root, worker.Name))
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
	app.Config = cfg
	app.Herdr = reader
	app.Sessions = sessions
	if err := app.recoverContentSafeguardsOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	snapshot, err := app.herdrSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	binding, err := ResolveLiveBinding(snapshot, cfg, e.root, LiveBindingRequest{Seat: worker.Name, RequireInteractiveReady: true})
	if err != nil || binding.Kind != "grok" {
		t.Fatalf("fallback binding=%+v err=%v", binding, err)
	}
	control.mu.Lock()
	calls := strings.Join(control.calls, "\n")
	control.mu.Unlock()
	if !strings.Contains(calls, "tab close w-test:t1") || !strings.Contains(calls, "agent start eng-developer grok --always-approve") {
		t.Fatalf("missing close/start transition:\n%s", calls)
	}
	if !strings.Contains(calls, "[HQ runtime recovery]") || !strings.Contains(calls, "assignment show --id") || !strings.Contains(calls, "RUNTIME-FALLBACK-001") {
		t.Fatalf("recovery prompt did not reconstruct durable assignment:\n%s", calls)
	}
	events, err := sessions.List(SessionFilter{Agent: worker.Name})
	if err != nil {
		t.Fatal(err)
	}
	var oldStopped, grokStarted bool
	for _, event := range events {
		oldStopped = oldStopped || (event.SessionID == started.SessionID && event.Type == sessionStopped && strings.HasPrefix(event.Reason, fallbackPendingReason))
		grokStarted = grokStarted || (event.SessionID != started.SessionID && event.Type == sessionStarted && event.RuntimeKind == "grok")
	}
	if !oldStopped || !grokStarted {
		t.Fatalf("session audit missing old stop/new grok start: %+v", events)
	}
}

func TestExplicitRuntimeFallbackRefusesWithoutTerminalEvidence(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		cfg.RuntimeFallback = &RuntimeFallbackPolicy{
			Auto: true, Trigger: "content_safeguard", FromKind: "codex", ToKind: "grok",
			PermissionMode: "yolo", AgentArgs: []string{"--always-approve"},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	hydrateRuntimeFallback(&cfg)
	manager, _ := cfg.exactRule("zantianyou")
	worker, _ := cfg.exactRule("eng-developer")
	e.setActor(t, manager.Name, "fallback:no-evidence", testAgentCWD(cfg, e.root, manager.Name))
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "fallback-no-evidence.md"), "# source\n")
	runTestCommand(t, e, "case", "create", "--id", "RUNTIME-FALLBACK-NO-EVIDENCE", "--title", "fallback evidence", "--source", source)
	runTestCommand(t, e, "issue", "--case", "RUNTIME-FALLBACK-NO-EVIDENCE", "--to", worker.Name, "--next", "continue")

	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, worker, "idle")
	control.snapshot.Panes[0].TerminalID = "terminal-codex-no-evidence"
	control.snapshot.Panes[0].Revision = 1
	control.snapshot.Agents[0].TerminalID = "terminal-codex-no-evidence"
	control.snapshot.Agents[0].Revision = 1
	control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: worker.Name, Kind: "codex", Value: "codex-session-no-evidence"}
	reader := &fallbackReaderControl{fakeHerdrControl: control, terminal: []byte("› Ask Codex to do anything\n")}
	sessions := &FileSessionStore{Root: filepath.Join(e.data, "runtime-fallback-no-evidence-sessions")}
	created := HerdrTabCreated{Tab: control.snapshot.Tabs[0], Pane: control.snapshot.Panes[0]}
	started, err := newSessionEvent(time.Now(), sessionStarted, created, "w-test", worker, "hq-up", "primary", testAgentCWD(cfg, e.root, worker.Name))
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
	app.Config = cfg
	app.Herdr = reader
	app.Sessions = sessions
	err = app.retryContentSafeguardFallback(context.Background(), worker.Name, false)
	if err == nil || !strings.Contains(err.Error(), "没有完整 content_safeguard 证据") || !strings.Contains(err.Error(), "未关闭 Codex") {
		t.Fatalf("expected agent-readable no-evidence refusal, got %v", err)
	}
	control.mu.Lock()
	calls := strings.Join(control.calls, "\n")
	control.mu.Unlock()
	if strings.Contains(calls, "tab close") || strings.Contains(calls, "agent start") {
		t.Fatalf("explicit refusal mutated runtime:\n%s", calls)
	}
}

func TestRuntimeFallbackAmbiguousCloseNeverStartsSecondRuntime(t *testing.T) {
	e := setupTestEnv(t)
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		cfg.RuntimeFallback = &RuntimeFallbackPolicy{
			Auto: true, Trigger: "content_safeguard", FromKind: "codex", ToKind: "grok",
			PermissionMode: "yolo", AgentArgs: []string{"--always-approve"},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := cfg.exactRule("zantianyou")
	worker, _ := cfg.exactRule("eng-developer")
	e.setActor(t, manager.Name, "fallback:ambiguous", testAgentCWD(cfg, e.root, manager.Name))
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "fallback-ambiguous.md"), "# source\n")
	runTestCommand(t, e, "case", "create", "--id", "RUNTIME-FALLBACK-AMBIGUOUS", "--title", "ambiguous close", "--source", source)
	runTestCommand(t, e, "issue", "--case", "RUNTIME-FALLBACK-AMBIGUOUS", "--to", worker.Name, "--next", "continue")

	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = healthySnapshot(e.root, worker, "idle")
	control.snapshot.Panes[0].TerminalID = "terminal-codex-ambiguous"
	control.snapshot.Panes[0].Revision = 1
	control.snapshot.Agents[0].TerminalID = "terminal-codex-ambiguous"
	control.snapshot.Agents[0].Revision = 1
	control.snapshot.Agents[0].AgentSession = &HerdrAgentSession{Source: "native", Agent: worker.Name, Kind: "codex", Value: "codex-session-ambiguous"}
	control.closeMutates = false
	control.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("close response timed out")}
	reader := &fallbackReaderControl{fakeHerdrControl: control, terminal: []byte("ⓘ This content can't be shown\nWe take extra caution with cybersecurity requests.\nTrusted Access: https://chatgpt.com/cyber/\n› Ask Codex to do anything\n")}
	sessions := &FileSessionStore{Root: filepath.Join(e.data, "runtime-fallback-ambiguous-sessions")}
	created := HerdrTabCreated{Tab: control.snapshot.Tabs[0], Pane: control.snapshot.Panes[0]}
	started, err := newSessionEvent(time.Now(), sessionStarted, created, "w-test", worker, "hq-up", "primary", testAgentCWD(cfg, e.root, worker.Name))
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
	app.Config = cfg
	app.Herdr = reader
	app.Sessions = sessions
	err = app.recoverContentSafeguardsOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "结果不确定") || !strings.Contains(err.Error(), "禁止自动启动 Grok") {
		t.Fatalf("ambiguous close did not fail closed: %v", err)
	}
	control.mu.Lock()
	calls := strings.Join(control.calls, "\n")
	control.mu.Unlock()
	if strings.Count(calls, "tab close ") != 1 || strings.Contains(calls, "agent start ") {
		t.Fatalf("ambiguous close started a second runtime or retried close:\n%s", calls)
	}
	events, err := sessions.List(SessionFilter{Agent: worker.Name})
	if err != nil || latestSessionDiagnostic(events, started.SessionID).Type != sessionFallbackUnknown {
		t.Fatalf("missing durable fallback_unknown: events=%+v err=%v", events, err)
	}

	err = app.recoverContentSafeguardsOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "禁止自动重试") {
		t.Fatalf("automatic watcher retried an unresolved fallback: %v", err)
	}
	control.mu.Lock()
	calls = strings.Join(control.calls, "\n")
	control.mu.Unlock()
	if strings.Count(calls, "tab close ") != 1 || strings.Contains(calls, "agent start ") {
		t.Fatalf("second watcher pass mutated unresolved runtime:\n%s", calls)
	}
}
