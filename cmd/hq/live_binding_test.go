package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func liveBindingFixture(t *testing.T, name string) (testEnv, Config, HerdrSnapshot) {
	t.Helper()
	e := setupTestEnv(t)
	cfg := testConfig()
	rule, _ := cfg.exactRule(name)
	cwd := testRuleCWD(e.root, rule)
	snapshot := HerdrSnapshot{
		Workspaces: []HerdrWorkspace{{ID: "w-target", Label: cfg.WorkspaceLabel}},
		Tabs:       []HerdrTab{{ID: "w-target:t1", WorkspaceID: "w-target", Label: rosterTabLabel(rule), CWD: cwd}},
		Panes:      []HerdrPane{{ID: "w-target:p1", TerminalID: "term-1", WorkspaceID: "w-target", TabID: "w-target:t1", CWD: cwd, Revision: 6}},
		Agents: []HerdrAgent{{Name: name, Kind: rule.Kind, Status: "working", CWD: cwd, TerminalID: "term-1",
			WorkspaceID: "w-target", TabID: "w-target:t1", PaneID: "w-target:p1", InteractiveReady: true,
			Revision: 7, AgentSession: &HerdrAgentSession{Source: "codex", Agent: "codex", Kind: "id", Value: "session-7"}}},
	}
	return e, cfg, snapshot
}

func TestResolveLiveBindingReturnsRuntimeIncarnation(t *testing.T) {
	e, cfg, snapshot := liveBindingFixture(t, "eng-developer")
	binding, err := ResolveLiveBinding(snapshot, cfg, e.root, LiveBindingRequest{PaneID: "w-target:p1", RequireInteractiveReady: true})
	if err != nil {
		t.Fatal(err)
	}
	if binding.Seat != "eng-developer" || binding.TerminalID != "term-1" || binding.Revision != 7 || binding.AgentSession == nil || binding.AgentSession.Value != "session-7" {
		t.Fatalf("binding=%+v", binding)
	}
	control := &workspaceScopeValidatingControl{fakeHerdrControl: newFakeHerdrControl(e.root, cfg.WorkspaceLabel)}
	raw, err := marshalSnapshotEnvelope(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	control.raw = raw
	actor, err := (herdrIdentityProvider{Control: control}).Resolve(cfg, e.root, "w-target:p1")
	if err != nil || actor.Name != binding.Seat {
		t.Fatalf("actor=%+v err=%v", actor, err)
	}
}

func marshalSnapshotEnvelope(snapshot HerdrSnapshot) ([]byte, error) {
	return jsonMarshal(map[string]any{"result": map[string]any{"snapshot": snapshot}})
}

func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}

func TestResolveLiveBindingRejectsDriftMatrix(t *testing.T) {
	e, cfg, base := liveBindingFixture(t, "eng-developer")
	tests := []struct {
		name string
		want string
		edit func(*HerdrSnapshot)
	}{
		{name: "wrong workspace", want: "其他 workspace", edit: func(s *HerdrSnapshot) { s.Agents[0].WorkspaceID = "w-other" }},
		{name: "wrong kind", want: "kind=claude", edit: func(s *HerdrSnapshot) { s.Agents[0].Kind = "claude" }},
		{name: "not ready", want: "interactive_ready=false", edit: func(s *HerdrSnapshot) { s.Agents[0].InteractiveReady = false }},
		{name: "unknown status", want: "status=unknown", edit: func(s *HerdrSnapshot) { s.Agents[0].Status = "unknown" }},
		{name: "terminal replaced", want: "terminal incarnation", edit: func(s *HerdrSnapshot) { s.Agents[0].TerminalID = "term-2" }},
		{name: "tab relabelled", want: "tab label", edit: func(s *HerdrSnapshot) { s.Tabs[0].Label = "foreign" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := cloneHerdrSnapshot(base)
			tc.edit(&snapshot)
			_, err := ResolveLiveBinding(snapshot, cfg, e.root, LiveBindingRequest{PaneID: "w-target:p1", RequireInteractiveReady: true})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v want=%q", err, tc.want)
			}
		})
	}
}

type snapshotOnlyControl struct{ snapshot HerdrSnapshot }

func (c snapshotOnlyControl) Snapshot(context.Context, HerdrSnapshotScope) (HerdrSnapshot, error) {
	return c.snapshot, nil
}

func prepareLiveBindingIssue(t *testing.T, caseID string) (testEnv, *App, *fakeHerdrControl, HerdrSnapshot, []string) {
	t.Helper()
	e, cfg, healthy := liveBindingFixture(t, "eng-developer")
	e.setActor(t, "zantianyou", "manager:pane", testAgentCWD(cfg, e.root, "zantianyou"))
	app := e.app(t)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	control.snapshot = cloneHerdrSnapshot(healthy)
	app.Herdr = control
	source := writeTestFile(t, filepath.Join(e.root, "engineering", strings.ToLower(caseID)+".md"), "# live binding\n")
	if err := app.run([]string{"case", "create", "--id", caseID, "--title", "Live binding delivery", "--project", "runtime", "--source", source}); err != nil {
		t.Fatal(err)
	}
	issueArgs := []string{"issue", "--case", caseID, "--to", "eng-developer", "--next", "implement exact binding"}
	return e, app, control, healthy, issueArgs
}

func onlyDelivery(t *testing.T, app *App) DeliveryView {
	t.Helper()
	deliveries, err := app.Store.Deliveries(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("deliveries=%+v", deliveries)
	}
	return deliveries[0]
}

func requireCaseProjection(t *testing.T, app *App, caseID, status, owner string) {
	t.Helper()
	snapshot, err := app.Store.Snapshot(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	state := snapshot.Cases[caseID]
	if state == nil || state.Status != status || state.Owner != owner {
		t.Fatalf("case=%s state=%+v want status=%s owner=%s", caseID, state, status, owner)
	}
}

func TestWakeupDeliveryRejectsWrongLiveBindingBeforeAttemptAndRetriesSameIssue(t *testing.T) {
	e, app, control, healthy, issueArgs := prepareLiveBindingIssue(t, "LIVE-BINDING-PRE-001")
	wrong := cloneHerdrSnapshot(healthy)
	wrong.Agents[0].Kind = "claude"
	control.snapshot = wrong

	err := app.run(issueArgs)
	if err == nil || !strings.Contains(err.Error(), "live binding") || !strings.Contains(err.Error(), "kind=claude") {
		t.Fatalf("err=%v", err)
	}
	if len(e.transport.calls) != 0 {
		t.Fatalf("wrong binding reached transport: %+v", e.transport.calls)
	}
	if delivery := onlyDelivery(t, app); delivery.Status != deliveryPrepared || delivery.AttemptCount != 0 {
		t.Fatalf("delivery=%+v", delivery)
	}
	requireCaseProjection(t, app, "LIVE-BINDING-PRE-001", string(statusOpen), "zantianyou")

	control.snapshot = healthy
	if err := app.run(issueArgs); err != nil {
		t.Fatalf("same issue after binding repair: %v", err)
	}
	if len(e.transport.calls) != 1 || e.transport.calls[0].target != "eng-developer" {
		t.Fatalf("transport calls=%+v", e.transport.calls)
	}
	if delivery := onlyDelivery(t, app); delivery.Status != deliverySent || delivery.AttemptCount != 1 {
		t.Fatalf("delivery=%+v", delivery)
	}
	requireCaseProjection(t, app, "LIVE-BINDING-PRE-001", string(statusDispatched), "eng-developer")
}

func TestWakeupDeliveryDetectsBindingDriftAfterAttemptAndRequiresExplicitRetry(t *testing.T) {
	e, app, control, healthy, issueArgs := prepareLiveBindingIssue(t, "LIVE-BINDING-FINAL-001")
	drifted := cloneHerdrSnapshot(healthy)
	drifted.Agents[0].InteractiveReady = false
	control.snapshots = []HerdrSnapshot{healthy, drifted}

	err := app.run(issueArgs)
	if err == nil || !strings.Contains(err.Error(), "最终 live binding") || !strings.Contains(err.Error(), "interactive_ready=false") {
		t.Fatalf("err=%v", err)
	}
	if len(e.transport.calls) != 0 {
		t.Fatalf("binding drift reached transport: %+v", e.transport.calls)
	}
	delivery := onlyDelivery(t, app)
	if delivery.Status != deliveryFailedPreSend || delivery.AttemptCount != 1 {
		t.Fatalf("delivery=%+v", delivery)
	}
	requireCaseProjection(t, app, "LIVE-BINDING-FINAL-001", string(statusOpen), "zantianyou")

	control.snapshot = healthy
	if err := app.run([]string{"delivery", "retry", "--id", delivery.DeliveryID, "--command", "delivery-retry:binding-repaired"}); err != nil {
		t.Fatalf("explicit retry after binding repair: %v", err)
	}
	if len(e.transport.calls) != 1 || e.transport.calls[0].target != "eng-developer" {
		t.Fatalf("transport calls=%+v", e.transport.calls)
	}
	if delivery = onlyDelivery(t, app); delivery.Status != deliverySent || delivery.AttemptCount != 2 {
		t.Fatalf("delivery=%+v", delivery)
	}
	requireCaseProjection(t, app, "LIVE-BINDING-FINAL-001", string(statusDispatched), "eng-developer")
}

func TestNudgeDetectsLiveBindingDriftAfterAttemptWithoutPrompt(t *testing.T) {
	e := setupTestEnv(t)
	now := time.Date(2026, 8, 31, 5, 0, 0, 0, time.UTC)
	control := newFakeHerdrControl(e.root, testConfig().WorkspaceLabel)
	addOperationsLive(&control.snapshot, testConfig(), e.root, "zantianyou", "working", "binding-nudge")
	healthy := cloneHerdrSnapshot(control.snapshot)
	drifted := cloneHerdrSnapshot(healthy)
	drifted.Agents[0].CWD = filepath.Join(e.root, "qa-ux")
	app := operationsTestApp(t, e, control, &now)
	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-BINDING-DRIFT", "--dedupe", "binding:drift", "--to", "zantianyou", "--message", "inspect after current turn", "--ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-BINDING-DRIFT", "--claim", "CLAIM-BINDING-DRIFT", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	control.snapshots = []HerdrSnapshot{healthy, drifted}
	promptsBefore := countFakeCalls(control, "prompt ")
	err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-BINDING-DRIFT", "--claim", "CLAIM-BINDING-DRIFT"})
	if err == nil || !strings.Contains(err.Error(), "最终 live binding") {
		t.Fatalf("err=%v", err)
	}
	if got := countFakeCalls(control, "prompt "); got != promptsBefore {
		t.Fatalf("binding drift reached Prompt: before=%d after=%d", promptsBefore, got)
	}
	view, readErr := app.readNudge("NUDGE-BINDING-DRIFT")
	if readErr != nil || view.State != "failed" || view.AttemptEventID == "" || view.TerminalEvent == "" {
		t.Fatalf("nudge=%+v err=%v", view, readErr)
	}
}
