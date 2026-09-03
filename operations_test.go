package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type operationsIdentity struct{ actor Actor }

func (p operationsIdentity) Resolve(cfg Config, _ string, paneID string) (Actor, error) {
	rule, ok := cfg.exactRule(p.actor.Name)
	if !ok {
		return Actor{}, fmt.Errorf("actor unavailable: %s", p.actor.Name)
	}
	actor := p.actor
	actor.PaneID, actor.Rule, actor.Label, actor.Department = paneID, rule, rule.Label, rule.Department
	return actor, nil
}

func operationsTestApp(t *testing.T, e testEnv, control *fakeHerdrControl, now *time.Time) *App {
	t.Helper()
	app := e.app(t)
	penny, _ := app.Config.exactRule("penny")
	app.Identity = operationsIdentity{actor: Actor{Name: "penny", Label: penny.Label, Department: penny.Department, Rule: penny}}
	app.CallerPane, app.MaintenancePane = "w-test:penny", "w-test:penny"
	if control != nil {
		app.Herdr = control
	} else {
		app.Herdr = nil
	}
	app.Estop = &FileEstopStore{Root: filepath.Join(e.root, "estop-state")}
	app.Sessions = &FileSessionStore{Root: filepath.Join(e.root, "estop-sessions")}
	app.Clock = func() time.Time { return *now }
	if store, ok := app.Store.(*Store); ok {
		store.Now = func() time.Time { return *now }
	}
	app.Out, app.Err = io.Discard, io.Discard
	return app
}

func addOperationsLive(snapshot *HerdrSnapshot, cfg Config, hqRoot, name, status, suffix string) {
	rule, _ := cfg.ruleFor(name)
	workspaceID := "w-test"
	cwd := testRuleCWD(hqRoot, rule)
	tabID, paneID := workspaceID+":t"+suffix, workspaceID+":p"+suffix
	kind := rule.Kind
	if kind == "" {
		kind = "grok"
	}
	snapshot.Tabs = append(snapshot.Tabs, HerdrTab{ID: tabID, WorkspaceID: workspaceID, Label: rosterTabLabel(rule), CWD: cwd})
	snapshot.Panes = append(snapshot.Panes, HerdrPane{ID: paneID, WorkspaceID: workspaceID, TabID: tabID, CWD: cwd})
	snapshot.Agents = append(snapshot.Agents, HerdrAgent{Name: name, Kind: kind, Status: status, CWD: cwd,
		WorkspaceID: workspaceID, TabID: tabID, PaneID: paneID,
		InteractiveReady: true})
}

func countFakeCalls(control *fakeHerdrControl, prefix string) int {
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

type nudgeFinalSnapshotBarrierControl struct {
	*fakeHerdrControl
	mu      sync.Mutex
	calls   int
	entered chan struct{}
	release chan struct{}
}

func (c *nudgeFinalSnapshotBarrierControl) Snapshot(ctx context.Context, scope HerdrSnapshotScope) (HerdrSnapshot, error) {
	snapshot, err := c.fakeHerdrControl.Snapshot(ctx, scope)
	c.mu.Lock()
	c.calls++
	call := c.calls
	c.mu.Unlock()
	if call == 2 {
		close(c.entered)
		<-c.release
	}
	return snapshot, err
}

func TestNudgeOperationLockPreventsConcurrentReconcileBeforePromptTerminal(t *testing.T) {
	e := setupTestEnv(t)
	now := time.Date(2026, 8, 31, 5, 30, 0, 0, time.UTC)
	base := newFakeHerdrControl(e.root, testConfig().WorkspaceLabel)
	addOperationsLive(&base.snapshot, testConfig(), e.root, "zantianyou", "working", "nudge-operation-lock")
	barrier := &nudgeFinalSnapshotBarrierControl{
		fakeHerdrControl: base,
		entered:          make(chan struct{}),
		release:          make(chan struct{}),
	}
	deliverApp := operationsTestApp(t, e, base, &now)
	deliverApp.Herdr = barrier
	if err := deliverApp.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPERATION-LOCK", "--dedupe", "nudge:operation-lock", "--to", "zantianyou", "--message", "serialize delivery and recovery", "--ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	if err := deliverApp.cmdNudge([]string{"claim", "--id", "NUDGE-OPERATION-LOCK", "--claim", "CLAIM-OPERATION-LOCK", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	deliverDone := make(chan error, 1)
	go func() {
		deliverDone <- deliverApp.cmdNudge([]string{"deliver", "--id", "NUDGE-OPERATION-LOCK", "--claim", "CLAIM-OPERATION-LOCK"})
	}()
	select {
	case <-barrier.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("deliver did not pause after durable attempt")
	}
	if view, err := deliverApp.readNudge("NUDGE-OPERATION-LOCK"); err != nil || view.State != "attempted" {
		t.Fatalf("barrier state=%+v err=%v", view, err)
	}
	evidence := writeTestFile(t, filepath.Join(e.office, "nudge-operation-lock.md"), "# reconcile evidence\n")
	reconcileApp := operationsTestApp(t, e, base, &now)
	reconcileDone := make(chan error, 1)
	go func() {
		reconcileDone <- reconcileApp.cmdNudge([]string{"reconcile", "--id", "NUDGE-OPERATION-LOCK", "--resolution", "not-run", "--ref", evidence, "--note", "must wait for live delivery"})
	}()
	select {
	case err := <-reconcileDone:
		close(barrier.release)
		<-deliverDone
		t.Fatalf("reconcile overtook active delivery: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	if view, err := deliverApp.readNudge("NUDGE-OPERATION-LOCK"); err != nil || view.State != "attempted" {
		close(barrier.release)
		<-deliverDone
		t.Fatalf("blocked reconcile changed state=%+v err=%v", view, err)
	}
	if prompts := countFakeCalls(base, "prompt "); prompts != 0 {
		close(barrier.release)
		<-deliverDone
		t.Fatalf("Prompt ran before final snapshot release: %d", prompts)
	}
	close(barrier.release)
	if err := <-deliverDone; err != nil {
		t.Fatalf("live delivery failed: %v", err)
	}
	if err := <-reconcileDone; err == nil || !strings.Contains(err.Error(), "不需人工 reconcile") {
		t.Fatalf("post-terminal reconcile result=%v", err)
	}
	view, err := deliverApp.readNudge("NUDGE-OPERATION-LOCK")
	if err != nil || view.State != "delivered" || countFakeCalls(base, "prompt ") != 1 {
		t.Fatalf("final nudge=%+v prompts=%d err=%v", view, countFakeCalls(base, "prompt "), err)
	}
	events, err := deliverApp.Store.ReadAll(deliverApp.Config)
	if err != nil {
		t.Fatal(err)
	}
	terminals := 0
	for _, event := range events {
		if event.NudgeID == "NUDGE-OPERATION-LOCK" && (event.Type == "nudge_delivered" || event.Type == "nudge_failed" ||
			event.Type == "nudge_unknown" || strings.HasPrefix(event.Type, "nudge_reconciled_")) {
			terminals++
		}
	}
	if terminals != 1 {
		t.Fatalf("terminal facts=%d events=%+v", terminals, events)
	}
}

func TestOperationsNudgeAtomicClaimDedupeTTLAndPromptRecovery(t *testing.T) {
	e := setupTestEnv(t)
	now := time.Date(2026, 8, 28, 4, 0, 0, 0, time.UTC)
	control := newFakeHerdrControl(e.root, "hq-test")
	addOperationsLive(&control.snapshot, testConfig(), e.root, "zantianyou", "working", "manager")
	app := operationsTestApp(t, e, control, &now)

	enqueue := []string{"enqueue", "--id", "NUDGE-OPS-001", "--dedupe", "manager:ops", "--to", "zantianyou", "--message", "请在当前回合结束后查看运维短提醒", "--ttl", "15m"}
	if err := app.cmdNudge(enqueue); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge(enqueue); err != nil {
		t.Fatalf("exact enqueue retry must be idempotent: %v", err)
	}
	conflict := append([]string(nil), enqueue...)
	conflict[8] = "内容冲突"
	if err := app.cmdNudge(conflict); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("same id content conflict accepted: %v", err)
	}
	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-002", "--dedupe", "manager:ops", "--to", "zantianyou", "--message", "重复 key", "--ttl", "15m"}); err == nil || !strings.Contains(err.Error(), "active nudge") {
		t.Fatalf("active dedupe conflict accepted: %v", err)
	}

	apps := []*App{operationsTestApp(t, e, control, &now), operationsTestApp(t, e, control, &now)}
	claims := []string{"CLAIM-A", "CLAIM-B"}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for i := range apps {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- apps[i].cmdNudge([]string{"claim", "--id", "NUDGE-OPS-001", "--claim", claims[i], "--lease", "30s"})
		}(i)
	}
	wg.Wait()
	close(errs)
	successes := 0
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("concurrent claim successes=%d want=1", successes)
	}
	view, err := app.readNudge("NUDGE-OPS-001")
	if err != nil || view.State != "claimed" || view.ClaimID == "" {
		t.Fatalf("view=%+v err=%v", view, err)
	}
	if err := app.cmdNudge([]string{"deliver", "--id", view.NudgeID, "--claim", view.ClaimID}); err != nil {
		t.Fatal(err)
	}
	if got := countFakeCalls(control, "prompt zantianyou [HQ notification] HQ_NUDGE_V1"); got != 1 {
		t.Fatalf("prompt calls=%d", got)
	}
	if err := app.cmdNudge([]string{"deliver", "--id", view.NudgeID, "--claim", view.ClaimID}); err != nil {
		t.Fatalf("terminal retry should return existing state: %v", err)
	}
	if got := countFakeCalls(control, "prompt zantianyou [HQ notification] HQ_NUDGE_V1"); got != 1 {
		t.Fatalf("terminal retry repeated prompt: %d", got)
	}

	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-CRASH", "--dedupe", "manager:crash", "--to", "zantianyou", "--message", "crash window", "--ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-OPS-CRASH", "--claim", "CLAIM-CRASH", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	app.NudgeFailpoint = func(name string) error {
		if name == "after_attempt_recorded" {
			return errors.New("synthetic crash")
		}
		return nil
	}
	beforePrompt := countFakeCalls(control, "prompt ")
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-OPS-CRASH", "--claim", "CLAIM-CRASH"}); err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("attempt crash not frozen: %v", err)
	}
	app.NudgeFailpoint = nil
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-OPS-CRASH", "--claim", "CLAIM-CRASH"}); err == nil || !strings.Contains(err.Error(), "禁止自动再投") {
		t.Fatalf("attempt retry should not prompt: %v", err)
	}
	if countFakeCalls(control, "prompt ") != beforePrompt {
		t.Fatal("attempt crash retried Prompt")
	}
	evidence := writeTestFile(t, filepath.Join(e.root, "engineering", "reconcile.md"), "# evidence\n")
	if err := app.cmdNudge([]string{"reconcile", "--id", "NUDGE-OPS-CRASH", "--resolution", "not-run", "--ref", evidence, "--note", "确认未执行"}); err != nil {
		t.Fatal(err)
	}
	if resolved, _ := app.readNudge("NUDGE-OPS-CRASH"); resolved.State != "reconciled_not_run" {
		t.Fatalf("resolved=%+v", resolved)
	}

	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-AFTER-PROMPT", "--dedupe", "manager:after-prompt", "--to", "zantianyou", "--message", "after prompt crash", "--ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-OPS-AFTER-PROMPT", "--claim", "CLAIM-AFTER-PROMPT", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	app.NudgeFailpoint = func(name string) error {
		if name == "after_prompt_before_result" {
			return errors.New("synthetic crash after prompt")
		}
		return nil
	}
	promptsBeforeCrash := countFakeCalls(control, "prompt ")
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-OPS-AFTER-PROMPT", "--claim", "CLAIM-AFTER-PROMPT"}); err == nil || !strings.Contains(err.Error(), "reconcile") {
		t.Fatalf("after-prompt crash not frozen: %v", err)
	}
	app.NudgeFailpoint = nil
	if countFakeCalls(control, "prompt ") != promptsBeforeCrash+1 {
		t.Fatal("expected exactly one Prompt before crash")
	}
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-OPS-AFTER-PROMPT", "--claim", "CLAIM-AFTER-PROMPT"}); err == nil || !strings.Contains(err.Error(), "禁止自动再投") {
		t.Fatalf("after-prompt retry should freeze: %v", err)
	}
	if countFakeCalls(control, "prompt ") != promptsBeforeCrash+1 {
		t.Fatal("after-prompt crash blindly re-prompted")
	}
	if err := app.cmdNudge([]string{"reconcile", "--id", "NUDGE-OPS-AFTER-PROMPT", "--resolution", "delivered", "--ref", evidence, "--note", "人工确认已送达"}); err != nil {
		t.Fatal(err)
	}

	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-CLAIM-CRASH", "--dedupe", "manager:claim-crash", "--to", "zantianyou", "--message", "claim crash", "--ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	store := app.Store.(*Store)
	fired := false
	store.Failpoint = func(name string) error {
		if name == "journal_cleanup" && !fired {
			fired = true
			return errors.New("claim commit crash")
		}
		return nil
	}
	claimArgs := []string{"claim", "--id", "NUDGE-OPS-CLAIM-CRASH", "--claim", "CLAIM-DURABLE", "--lease", "30s"}
	if err := app.cmdNudge(claimArgs); err == nil {
		t.Fatal("claim failpoint did not fire")
	}
	store.Failpoint = nil
	if err := app.cmdNudge(claimArgs); err != nil {
		t.Fatalf("durable claim did not recover idempotently: %v", err)
	}
	if claimed, _ := app.readNudge("NUDGE-OPS-CLAIM-CRASH"); claimed.State != "claimed" || claimed.ClaimID != "CLAIM-DURABLE" {
		t.Fatalf("claim recovery=%+v", claimed)
	}

	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-LEASE", "--dedupe", "manager:lease", "--to", "zantianyou", "--message", "lease", "--ttl", "1m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-OPS-LEASE", "--claim", "CLAIM-OLD", "--lease", "5s"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-OPS-LEASE", "--claim", "CLAIM-NEW", "--lease", "5s"}); err != nil {
		t.Fatalf("expired claim was not recoverable: %v", err)
	}
	if leased, _ := app.readNudge("NUDGE-OPS-LEASE"); leased.ClaimID != "CLAIM-NEW" {
		t.Fatalf("lease reclaim=%+v", leased)
	}

	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-IDLE", "--dedupe", "manager:idle", "--to", "zantianyou", "--message", "working only", "--ttl", "1m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-OPS-IDLE", "--claim", "CLAIM-IDLE", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	control.mu.Lock()
	for i := range control.snapshot.Agents {
		if control.snapshot.Agents[i].Name == "zantianyou" {
			control.snapshot.Agents[i].Status = "idle"
		}
	}
	control.mu.Unlock()
	idlePrompts := countFakeCalls(control, "prompt ")
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-OPS-IDLE", "--claim", "CLAIM-IDLE"}); err != nil {
		t.Fatalf("idle manager was not woken at the turn boundary: %v", err)
	}
	if countFakeCalls(control, "prompt ") != idlePrompts+1 {
		t.Fatal("idle manager did not receive exactly one Prompt")
	}
	control.mu.Lock()
	for i := range control.snapshot.Agents {
		if control.snapshot.Agents[i].Name == "zantianyou" {
			control.snapshot.Agents[i].Status = "working"
		}
	}
	control.mu.Unlock()

	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-UNKNOWN", "--dedupe", "manager:unknown", "--to", "zantianyou", "--message", "ambiguous", "--ttl", "15m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-OPS-UNKNOWN", "--claim", "CLAIM-UNKNOWN", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	control.promptOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("timeout")}
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-OPS-UNKNOWN", "--claim", "CLAIM-UNKNOWN"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous prompt not surfaced: %v", err)
	}
	unknownPrompts := countFakeCalls(control, "prompt ")
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-OPS-UNKNOWN", "--claim", "CLAIM-UNKNOWN"}); err != nil {
		t.Fatalf("unknown retry should report existing terminal without prompt: %v", err)
	}
	if countFakeCalls(control, "prompt ") != unknownPrompts {
		t.Fatal("unknown retry repeated prompt")
	}

	control.promptOutcome = HerdrMutationResult{}
	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-OPS-TTL", "--dedupe", "manager:ttl", "--to", "zantianyou", "--message", "will expire", "--ttl", "30s"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-OPS-TTL", "--claim", "CLAIM-TTL", "--lease", "5s"}); err != nil {
		t.Fatal(err)
	}
	if expired, _ := app.readNudge("NUDGE-OPS-TTL"); expired.State != "expired" {
		t.Fatalf("expired=%+v", expired)
	}
}

func appendOperationsCase(t *testing.T, app *App, caseID, owner string, at time.Time, source string) Event {
	t.Helper()
	actor, err := app.nudgeActor()
	if err != nil {
		t.Fatal(err)
	}
	commandID := stableCommandID("operations-case", caseID)
	result, err := app.Store.Transact(app.Config, commandID, requestDigest("operations-case", caseID, owner), false, func(*ledgerState) (Event, error) {
		event, err := app.newEvent(actor, "case_created", caseID)
		if err != nil {
			return Event{}, err
		}
		event.At = at.Format(time.RFC3339)
		setTestCaseSpec(&event, "reminder test case", source)
		event.ToState, event.Owner, event.NextAction = string(statusOpen), owner, "等待处理"
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Event
}

func closeOperationsCase(t *testing.T, app *App, caseID, source string) Event {
	t.Helper()
	actor, err := app.nudgeActor()
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.Store.Transact(app.Config, stableCommandID("operations-close", caseID), requestDigest("operations-close", caseID), false, func(ledger *ledgerState) (Event, error) {
		state := ledger.snapshot.Cases[caseID]
		event, err := app.newOperationsEvent(actor, "case_closed", caseID)
		if err != nil {
			return Event{}, err
		}
		event.FromState, event.ToState = state.Status, string(statusClosed)
		event.SourceRef, event.Note, event.NextAction = source, "显式处理完成", "无"
		return event, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result.Event
}

func TestOperationsReminderOncePerCaseAndResolvedWithoutAuthorityMutation(t *testing.T) {
	e := setupTestEnv(t)
	now := time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)
	control := newFakeHerdrControl(e.root, "hq-test")
	app := operationsTestApp(t, e, control, &now)
	source := writeTestFile(t, filepath.Join(e.root, "product", "reminder-source.md"), "# source\n")
	appendOperationsCase(t, app, "REMINDER-CASE-001", "zantianyou", now.Add(-25*time.Hour), source)

	apps := []*App{operationsTestApp(t, e, control, &now), operationsTestApp(t, e, control, &now)}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, candidate := range apps {
		wg.Add(1)
		go func(candidate *App) {
			defer wg.Done()
			errs <- candidate.cmdReminder([]string{"scan", "--after", "24h", "--ttl", "15m"})
		}(candidate)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	events, err := app.Store.ReadAll(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	created, closed := 0, 0
	var reminder Event
	for _, event := range events {
		switch event.Type {
		case "reminder_created":
			created++
			reminder = event
		case "case_closed":
			closed++
		}
	}
	if created != 1 || closed != 0 || reminder.BasisEventID == "" || reminder.ReminderID != reminderStableID(reminder.CaseID, reminder.BasisEventID) {
		t.Fatalf("created=%d closed=%d reminder=%+v", created, closed, reminder)
	}
	snapshot, err := app.Store.Snapshot(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	if state := snapshot.Cases["REMINDER-CASE-001"]; state.Status != string(statusOpen) || state.Owner != "zantianyou" {
		t.Fatalf("scan mutated authority fields: %+v", state)
	}

	zantianyou, _ := app.Config.exactRule("zantianyou")
	app.Identity = operationsIdentity{actor: Actor{Name: "zantianyou", Label: zantianyou.Label, Department: zantianyou.Department, Rule: zantianyou}}
	if err := app.cmdCaseRevise([]string{"--id", "REMINDER-CASE-001", "--title", "reminder test case", "--objective", "完成目标", "--acceptance", "结果可复验", "--constraints", "遵守岗位边界", "--priority", "P1", "--source", source, "--version", "2"}); err != nil {
		t.Fatal(err)
	}
	penny, _ := app.Config.exactRule("penny")
	app.Identity = operationsIdentity{actor: Actor{Name: "penny", Label: penny.Label, Department: penny.Department, Rule: penny}}
	if err := app.cmdReminder([]string{"scan", "--after", "24h"}); err != nil {
		t.Fatal(err)
	}
	events, err = app.Store.ReadAll(app.Config)
	if err != nil {
		t.Fatal(err)
	}
	resolved := 0
	closed = 0
	for _, event := range events {
		if event.Type == "reminder_resolved" {
			resolved++
		}
		if event.Type == "case_closed" {
			closed++
		}
	}
	if resolved != 1 || closed != 0 {
		t.Fatalf("resolved=%d explicit_closed=%d", resolved, closed)
	}
	view, err := app.readNudge(reminder.NudgeID)
	if err != nil || view.State != "cancelled" {
		t.Fatalf("reminder nudge=%+v err=%v", view, err)
	}
	now = now.Add(25 * time.Hour)
	if err := app.cmdReminder([]string{"scan", "--after", "1s"}); err != nil {
		t.Fatal(err)
	}
	events, _ = app.Store.ReadAll(app.Config)
	for _, event := range events {
		if event.Type == "reminder_created" && event.ID != reminder.ID {
			t.Fatalf("case lifecycle received a second reminder: %+v", event)
		}
	}
	closeOperationsCase(t, app, "REMINDER-CASE-001", source)
	if err := app.cmdReminder([]string{"scan", "--after", "1s"}); err != nil {
		t.Fatal(err)
	}
	events, _ = app.Store.ReadAll(app.Config)
	closed = 0
	for _, event := range events {
		if event.Type == "case_closed" {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("expected exactly one explicit close, got %d", closed)
	}

	t.Run("strict replay error emits no reminder", func(t *testing.T) {
		bad := setupTestEnv(t)
		badNow := now
		badApp := operationsTestApp(t, bad, newFakeHerdrControl(bad.root, "hq-test"), &badNow)
		path := filepath.Join(bad.data, "events", "2026-08.jsonl")
		writeTestFile(t, path, "{bad-json}\n")
		before, _ := os.ReadFile(path)
		err := badApp.cmdReminder([]string{"scan", "--after", "1h"})
		after, _ := os.ReadFile(path)
		if err == nil || !strings.Contains(err.Error(), "严格重放失败") || !bytes.Equal(before, after) {
			t.Fatalf("err=%v log_changed=%t", err, !bytes.Equal(before, after))
		}
	})
}

func prepareEstop(t *testing.T, childStatus string) (testEnv, *fakeHerdrControl, *App, *time.Time) {
	t.Helper()
	e := setupTestEnv(t)
	now := time.Date(2026, 8, 28, 10, 0, 0, 0, time.UTC)
	control := newFakeHerdrControl(e.root, "hq-test")
	addOperationsLive(&control.snapshot, testConfig(), e.root, "penny", "idle", "penny")
	addOperationsLive(&control.snapshot, testConfig(), e.root, "zantianyou", "working", "manager")
	addOperationsLive(&control.snapshot, testConfig(), e.root, "eng-developer", childStatus, "child")
	app := operationsTestApp(t, e, control, &now)
	app.FromGateway, app.Direct = false, true
	return e, control, app, &now
}

func TestOperationsEstopLedgerPlanAndStrictStateMachine(t *testing.T) {
	type fixture struct {
		app        *App
		actor      Actor
		item       EstopItem
		activation Event
		serial     int
	}
	newFixture := func(t *testing.T) *fixture {
		t.Helper()
		e := setupTestEnv(t)
		now := time.Date(2026, 8, 28, 9, 0, 0, 0, time.UTC)
		app := operationsTestApp(t, e, newFakeHerdrControl(e.root, "hq-test"), &now)
		rule, _ := app.Config.ruleFor("eng-developer")
		actor, err := app.nudgeActor()
		if err != nil {
			t.Fatal(err)
		}
		return &fixture{app: app, actor: actor, item: EstopItem{Agent: rule.Name, Department: rule.Department, ReportsTo: rule.ReportsTo,
			WorkspaceID: "w-test", TabID: "w-test:t-plan", PaneID: "w-test:p-plan", CWD: testRuleCWD(e.root, rule), Kind: rule.Kind}}
	}
	commit := func(t *testing.T, f *fixture, eventType, result, basis string) (Event, error) {
		t.Helper()
		f.serial++
		key := fmt.Sprintf("%02d-%s", f.serial, eventType)
		txn, err := f.app.Store.Transact(f.app.Config, stableCommandID("direct-estop", key), requestDigest("direct-estop", key, result, basis), false,
			func(*ledgerState) (Event, error) {
				event, err := f.app.newOperationsEvent(f.actor, eventType, estopSystemCaseID)
				if err != nil {
					return Event{}, err
				}
				event.EstopID = "ESTOP-DIRECT"
				switch eventType {
				case "estop_activated":
					event.Note, event.Result = "direct strict fixture", result
					event.PayloadDigest = estopPlanDigestForItems(f.app.Config, []EstopItem{f.item})
				case "estop_released":
					event.RelatedEventID, event.Note = f.activation.ID, "explicit release fixture"
				default:
					rule, _ := historicalRule(f.app.Config, f.item.Agent)
					event.RelatedEventID, event.Recipient, event.RecipientLabel = f.activation.ID, f.item.Agent, rule.Label
					event.WorkspaceID, event.TabID, event.PaneID = f.item.WorkspaceID, f.item.TabID, f.item.PaneID
					event.CWD, event.AgentKind, event.Result = f.item.CWD, f.item.Kind, result
					event.BasisEventID = basis
				}
				return event, nil
			})
		return txn.Event, err
	}
	activate := func(t *testing.T, f *fixture) {
		t.Helper()
		event, err := commit(t, f, "estop_activated", "1", "")
		if err != nil {
			t.Fatal(err)
		}
		f.activation = event
	}
	plan := func(t *testing.T, f *fixture) {
		t.Helper()
		if _, err := commit(t, f, "estop_agent_planned", "planned", ""); err != nil {
			t.Fatal(err)
		}
	}
	assertRejectedWithoutWrite := func(t *testing.T, f *fixture, eventType, result, basis string) {
		t.Helper()
		before, _ := f.app.Store.ReadAll(f.app.Config)
		if _, err := commit(t, f, eventType, result, basis); err == nil {
			t.Fatalf("%s unexpectedly accepted", eventType)
		}
		after, readErr := f.app.Store.ReadAll(f.app.Config)
		if readErr != nil || len(after) != len(before) {
			t.Fatalf("rejection wrote ledger: before=%d after=%d err=%v", len(before), len(after), readErr)
		}
	}

	t.Run("activation with expected child cannot release without plan or outcome", func(t *testing.T) {
		f := newFixture(t)
		activate(t, f)
		assertRejectedWithoutWrite(t, f, "estop_released", "", "")
	})
	t.Run("restore before freeze", func(t *testing.T) {
		f := newFixture(t)
		activate(t, f)
		plan(t, f)
		assertRejectedWithoutWrite(t, f, "estop_agent_restored", "confirmed", "")
	})
	t.Run("release before confirmed restore", func(t *testing.T) {
		f := newFixture(t)
		activate(t, f)
		plan(t, f)
		if _, err := commit(t, f, "estop_agent_frozen", "confirmed", ""); err != nil {
			t.Fatal(err)
		}
		assertRejectedWithoutWrite(t, f, "estop_released", "", "")
	})
	t.Run("conflicting terminal and result mismatch", func(t *testing.T) {
		f := newFixture(t)
		activate(t, f)
		plan(t, f)
		if _, err := commit(t, f, "estop_agent_frozen", "confirmed", ""); err != nil {
			t.Fatal(err)
		}
		assertRejectedWithoutWrite(t, f, "estop_agent_freeze_failed", "definitely-not-run", "")
		assertRejectedWithoutWrite(t, f, "estop_agent_restored", "ambiguous", "")
	})
	t.Run("explicit audited retry path", func(t *testing.T) {
		f := newFixture(t)
		activate(t, f)
		plan(t, f)
		freezeFailed, err := commit(t, f, "estop_agent_freeze_failed", "definitely-not-run", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := commit(t, f, "estop_agent_freeze_retry", "retry-authorized", freezeFailed.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := commit(t, f, "estop_agent_frozen", "confirmed", ""); err != nil {
			t.Fatal(err)
		}
		restoreFailed, err := commit(t, f, "estop_agent_restore_failed", "confirmed-rollback", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := commit(t, f, "estop_agent_restore_retry", "retry-authorized", restoreFailed.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := commit(t, f, "estop_agent_restored", "confirmed", ""); err != nil {
			t.Fatal(err)
		}
		if _, err := commit(t, f, "estop_released", "", ""); err != nil {
			t.Fatal(err)
		}
	})
}

func TestOperationsEstopManagersStayChildrenStopPatrolSuppressesAndReleaseRestores(t *testing.T) {
	e, control, app, _ := prepareEstop(t, "blocked")
	apps := []*App{app, operationsTestApp(t, e, control, &time.Time{})}
	apps[1].Clock = app.Clock
	apps[1].FromGateway, apps[1].Direct, apps[1].Estop, apps[1].Sessions = false, true, app.Estop, app.Sessions
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, candidate := range apps {
		wg.Add(1)
		go func(candidate *App) {
			defer wg.Done()
			errs <- candidate.run([]string{"estop", "activate", "--id", "ESTOP-OPERATIONS-001", "--reason", "synthetic emergency"})
		}(candidate)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if countFakeCalls(control, "tab close ") != 1 {
		t.Fatalf("activate was not idempotent: %v", control.calls)
	}
	control.mu.Lock()
	names := map[string]bool{}
	for _, agent := range control.snapshot.Agents {
		names[agent.Name] = true
	}
	control.mu.Unlock()
	if !names["penny"] || !names["zantianyou"] || names["eng-developer"] {
		t.Fatalf("exemption/freeze mismatch: %v", names)
	}
	state, exists, err := app.Estop.Read()
	if err != nil || !exists || state.State != "active" || len(state.Items) != 1 || state.Items[0].FreezeOutcome != "confirmed" {
		t.Fatalf("state=%+v exists=%t err=%v", state, exists, err)
	}
	patrol := &PatrolService{Herdr: control, Estop: app.Estop, Store: app.Store}
	report, err := patrol.Run(t.Context(), app.Config, app.HQRoot, 0)
	if err != nil || report.Frozen != 1 || report.DeadCandidate != 0 || report.Orphan != 0 {
		t.Fatalf("patrol=%+v err=%v", report, err)
	}

	apps[1].Clock = app.Clock
	errs = make(chan error, 2)
	for _, candidate := range apps {
		wg.Add(1)
		go func(candidate *App) {
			defer wg.Done()
			errs <- candidate.run([]string{"estop", "release", "--id", "ESTOP-OPERATIONS-001", "--reason", "explicit release"})
		}(candidate)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if countFakeCalls(control, "agent start eng-developer ") != 1 {
		t.Fatalf("release repeated start: %v", control.calls)
	}
	state, _, err = app.Estop.Read()
	if err != nil || state.State != "released" || state.Items[0].RestoreOutcome != "confirmed" {
		t.Fatalf("released=%+v err=%v", state, err)
	}
	report, err = patrol.Run(t.Context(), app.Config, app.HQRoot, 0)
	if err != nil || report.Frozen != 0 {
		t.Fatalf("released patrol=%+v err=%v", report, err)
	}
	if err := app.run([]string{"estop", "release", "--id", "ESTOP-OPERATIONS-001", "--reason", "repeat"}); err != nil {
		t.Fatal(err)
	}
	if countFakeCalls(control, "agent start eng-developer ") != 1 {
		t.Fatal("second release started child again")
	}
}

func TestOperationsEstopPartialReleaseUsesCurrentFrozenAndLiveFacts(t *testing.T) {
	e, control, app, _ := prepareEstop(t, "working")
	cfg, err := mutateConfig(e.config, func(cfg *Config) error {
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == "eng-data-engineer" || cfg.Agents[i].Name == "eng-developer" {
				cfg.Agents[i].ActivationPolicy = activationAlways
				cfg.Agents[i].SeatDigest = employeeSeatDigest(cfg.Agents[i])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	app.Config = cfg
	addOperationsLive(&control.snapshot, app.Config, e.root, "eng-data-engineer", "working", "data-child")
	if err := app.run([]string{"estop", "activate", "--id", "ESTOP-PARTIAL-RELEASE", "--reason", "synthetic multi child"}); err != nil {
		t.Fatal(err)
	}

	control.startOutcomes = map[string]HerdrMutationResult{
		"eng-developer": {Outcome: herdrDefinitelyNotRun, Err: errors.New("synthetic safe restore failure")},
	}
	release := []string{"estop", "release", "--id", "ESTOP-PARTIAL-RELEASE", "--reason", "explicit partial retry"}
	if err := app.run(release); err == nil {
		t.Fatal("partial release unexpectedly completed")
	}
	state, exists, err := app.Estop.Read()
	if err != nil || !exists || state.State != "active" {
		t.Fatalf("state=%+v exists=%t err=%v", state, exists, err)
	}
	items := map[string]EstopItem{}
	for _, item := range state.Items {
		items[item.Agent] = item
	}
	restored := items["eng-data-engineer"]
	failed := items["eng-developer"]
	if restored.RestoreOutcome != "confirmed" || failed.RestoreOutcome != "failed" || !failed.RestoreRetryable {
		t.Fatalf("partial outcomes: restored=%+v failed=%+v", restored, failed)
	}

	patrol := &PatrolService{Herdr: control, Estop: app.Estop, Store: app.Store}
	report, err := patrol.Run(t.Context(), app.Config, app.HQRoot, 0)
	if err != nil || report.Frozen != 1 {
		t.Fatalf("partial patrol=%+v err=%v", report, err)
	}
	for _, finding := range report.Findings {
		if finding.Agent == restored.Agent && (finding.Category == "frozen" || finding.SignalType == "frozen_agent_live") {
			t.Fatalf("restored live child remained frozen: %+v", finding)
		}
	}

	if result := control.CloseTab(t.Context(), restored.RestoredTab); result.Err != nil {
		t.Fatal(result.Err)
	}
	report, err = patrol.Run(t.Context(), app.Config, app.HQRoot, 0)
	if err != nil || report.Frozen != 1 {
		t.Fatalf("missing restored child patrol=%+v err=%v", report, err)
	}
	foundMissing := false
	for _, finding := range report.Findings {
		if finding.Agent != restored.Agent {
			continue
		}
		if finding.Category == "frozen" || finding.SignalType == "frozen_agent_live" {
			t.Fatalf("missing restored child was reclassified frozen: %+v", finding)
		}
		if finding.SignalType == "roster_missing" {
			foundMissing = true
		}
	}
	if !foundMissing {
		t.Fatalf("missing restored child did not receive normal patrol finding: %+v", report.Findings)
	}

	delete(control.startOutcomes, "eng-developer")
	failedStarts := countFakeCalls(control, "agent start eng-developer ")
	if err := app.run(release); err == nil || !strings.Contains(err.Error(), "当前无精确 live") {
		t.Fatalf("retry without restored child live proof accepted: %v", err)
	}
	if countFakeCalls(control, "agent start eng-developer ") != failedStarts {
		t.Fatal("retry mutated later child before re-proving prior confirmed restore")
	}
	state, _, err = app.Estop.Read()
	if err != nil || state.State != "active" || state.ReleaseEventID != "" {
		t.Fatalf("unsafe retry changed aggregate state: %+v err=%v", state, err)
	}
}

func TestOperationsEstopFactReconcileAndPatrolFailClosed(t *testing.T) {
	t.Run("freeze sentinel ahead is repaired before release but patrol refuses suppression", func(t *testing.T) {
		_, control, app, _ := prepareEstop(t, "working")
		fired := false
		app.EstopFailpoint = func(name string) error {
			if name == "after_freeze_state_before_event" && !fired {
				fired = true
				return errors.New("synthetic freeze crash")
			}
			return nil
		}
		if err := app.run([]string{"estop", "activate", "--id", "ESTOP-FREEZE-WINDOW", "--reason", "test"}); err == nil {
			t.Fatal("freeze failpoint did not fire")
		}
		state, _, err := app.Estop.Read()
		if err != nil || state.Items[0].FreezeOutcome != "confirmed" || state.Items[0].FreezeEventID != "" {
			t.Fatalf("state=%+v err=%v", state, err)
		}
		patrol := &PatrolService{Herdr: control, Estop: app.Estop, Store: app.Store}
		if report, err := patrol.Run(t.Context(), app.Config, app.HQRoot, 0); err == nil || report.Frozen != 0 {
			t.Fatalf("patrol suppressed from sentinel-only fact: report=%+v err=%v", report, err)
		}
		app.EstopFailpoint = nil
		if err := app.run([]string{"estop", "release", "--id", "ESTOP-FREEZE-WINDOW", "--reason", "explicit"}); err != nil {
			t.Fatal(err)
		}
		if countFakeCalls(control, "agent start eng-developer ") != 1 {
			t.Fatal("release did not repair fact before exactly one start")
		}
	})

	t.Run("sentinel conflict and corrupted ledger never suppress findings", func(t *testing.T) {
		e, control, app, now := prepareEstop(t, "working")
		if err := app.run([]string{"estop", "activate", "--id", "ESTOP-PATROL-CONFLICT", "--reason", "test"}); err != nil {
			t.Fatal(err)
		}
		if err := app.Estop.WithLock(func(locked *lockedEstopStore) error {
			state := locked.state
			state.Items[0].FreezeEventID = "EVT-CONFLICT"
			state.UpdatedAt = now.Add(time.Second).Format(time.RFC3339)
			return locked.Write(state)
		}); err != nil {
			t.Fatal(err)
		}
		patrol := &PatrolService{Herdr: control, Estop: app.Estop, Store: app.Store}
		if report, err := patrol.Run(t.Context(), app.Config, app.HQRoot, 0); err == nil || report.Frozen != 0 {
			t.Fatalf("sentinel conflict suppressed findings: report=%+v err=%v", report, err)
		}
		starts := countFakeCalls(control, "agent start ")
		if err := app.run([]string{"estop", "release", "--id", "ESTOP-PATROL-CONFLICT", "--reason", "unsafe"}); err == nil {
			t.Fatal("release accepted sentinel conflict")
		}
		if countFakeCalls(control, "agent start ") != starts {
			t.Fatal("sentinel conflict reached Herdr start")
		}

		logPath := app.Store.(*Store).EventLogPath(*now)
		raw, err := os.ReadFile(logPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(logPath, append(raw, []byte("{bad-json}\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		_ = e
		if report, err := patrol.Run(t.Context(), app.Config, app.HQRoot, 0); err == nil || report.Frozen != 0 {
			t.Fatalf("corrupt ledger suppressed findings: report=%+v err=%v", report, err)
		}
	})
}

func TestOperationsEstopSafeRetryAndReleaseLedgerLeadCrashWindows(t *testing.T) {
	t.Run("definitely-not-run freeze and restore are explicitly retryable", func(t *testing.T) {
		_, control, app, _ := prepareEstop(t, "working")
		control.closeMutates = false
		control.closeOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("close rejected")}
		activate := []string{"estop", "activate", "--id", "ESTOP-SAFE-RETRY", "--reason", "test"}
		if err := app.run(activate); err == nil {
			t.Fatal("definitely-not-run close unexpectedly completed")
		}
		control.closeMutates, control.closeOutcome = true, HerdrMutationResult{}
		if err := app.run(activate); err != nil {
			t.Fatal(err)
		}
		if countFakeCalls(control, "tab close w-test:tchild") != 2 {
			t.Fatalf("freeze retry mutation count: %v", control.calls)
		}

		control.createMutates = false
		control.createOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("create rejected")}
		release := []string{"estop", "release", "--id", "ESTOP-SAFE-RETRY", "--reason", "explicit"}
		if err := app.run(release); err == nil {
			t.Fatal("definitely-not-run restore unexpectedly completed")
		}
		state, _, _ := app.Estop.Read()
		if state.Items[0].RestoreFailureClass != "definitely-not-run" || !state.Items[0].RestoreRetryable {
			t.Fatalf("restore classification=%+v", state.Items[0])
		}
		control.createMutates, control.createOutcome = true, HerdrMutationResult{}
		if err := app.run(release); err != nil {
			t.Fatal(err)
		}
		if countFakeCalls(control, "tab create ") != 2 || countFakeCalls(control, "agent start eng-developer ") != 1 {
			t.Fatalf("restore retry mutation count: %v", control.calls)
		}
	})

	for _, failure := range []string{"session", "prompt"} {
		t.Run("confirmed rollback after "+failure+" failure is retryable", func(t *testing.T) {
			_, control, app, _ := prepareEstop(t, "working")
			id := "ESTOP-ROLLBACK-" + strings.ToUpper(failure)
			if err := app.run([]string{"estop", "activate", "--id", id, "--reason", "test"}); err != nil {
				t.Fatal(err)
			}
			if failure == "prompt" {
				control.promptOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("prompt timeout")}
			} else {
				fired := false
				app.Sessions.(*FileSessionStore).Failpoint = func(name string) error {
					if name == "before_append" && !fired {
						fired = true
						return errors.New("session append crash")
					}
					return nil
				}
			}
			release := []string{"estop", "release", "--id", id, "--reason", "explicit"}
			if err := app.run(release); err == nil {
				t.Fatal("rollback fixture unexpectedly completed")
			}
			state, _, _ := app.Estop.Read()
			if state.Items[0].RestoreFailureClass != "confirmed-rollback" || !state.Items[0].RestoreRetryable {
				t.Fatalf("rollback classification=%+v", state.Items[0])
			}
			events, err := app.Sessions.List(SessionFilter{Agent: "eng-developer"})
			if err != nil {
				t.Fatal(err)
			}
			if failure == "prompt" && (len(events) != 2 || events[0].Type != "started" || events[1].Type != "stopped") {
				t.Fatalf("prompt rollback session audit=%+v", events)
			}
			control.promptOutcome = HerdrMutationResult{}
			app.Sessions.(*FileSessionStore).Failpoint = nil
			if err := app.run(release); err != nil {
				t.Fatal(err)
			}
			if countFakeCalls(control, "agent start eng-developer ") != 2 {
				t.Fatalf("confirmed rollback retry count: %v", control.calls)
			}
		})
	}

	for _, failpoint := range []string{"after_restore_retry_event", "after_restore_retry_state"} {
		t.Run(failpoint, func(t *testing.T) {
			_, control, app, _ := prepareEstop(t, "working")
			if err := app.run([]string{"estop", "activate", "--id", "ESTOP-" + strings.ToUpper(failpoint), "--reason", "test"}); err != nil {
				t.Fatal(err)
			}
			control.startMutates = false
			control.startOutcome = HerdrMutationResult{Outcome: herdrDefinitelyNotRun, Err: errors.New("start rejected")}
			release := []string{"estop", "release", "--id", "ESTOP-" + strings.ToUpper(failpoint), "--reason", "explicit"}
			if err := app.run(release); err == nil {
				t.Fatal("first safe failure unexpectedly completed")
			}
			control.startMutates, control.startOutcome = true, HerdrMutationResult{}
			fired := false
			app.EstopFailpoint = func(name string) error {
				if name == failpoint && !fired {
					fired = true
					return errors.New("synthetic retry crash")
				}
				return nil
			}
			before := countFakeCalls(control, "agent start eng-developer ")
			if err := app.run(release); err == nil {
				t.Fatal("retry failpoint did not fire")
			}
			if countFakeCalls(control, "agent start eng-developer ") != before {
				t.Fatal("retry crash reached start")
			}
			app.EstopFailpoint = nil
			if err := app.run(release); err != nil {
				t.Fatal(err)
			}
			if countFakeCalls(control, "agent start eng-developer ") != before+1 {
				t.Fatal("retry recovery did not perform exactly one start")
			}
		})
	}

	t.Run("restore outcome ledger ahead reconciles without a second start", func(t *testing.T) {
		_, control, app, _ := prepareEstop(t, "working")
		if err := app.run([]string{"estop", "activate", "--id", "ESTOP-OUTCOME-LEAD", "--reason", "test"}); err != nil {
			t.Fatal(err)
		}
		fired := false
		app.EstopFailpoint = func(name string) error {
			if name == "after_restore_outcome_event" && !fired {
				fired = true
				return errors.New("synthetic outcome crash")
			}
			return nil
		}
		release := []string{"estop", "release", "--id", "ESTOP-OUTCOME-LEAD", "--reason", "explicit"}
		if err := app.run(release); err == nil {
			t.Fatal("outcome failpoint did not fire")
		}
		if countFakeCalls(control, "agent start eng-developer ") != 1 {
			t.Fatal("expected one start before outcome crash")
		}
		app.EstopFailpoint = nil
		if err := app.run(release); err != nil {
			t.Fatal(err)
		}
		if countFakeCalls(control, "agent start eng-developer ") != 1 {
			t.Fatal("ledger-leading outcome caused a second start")
		}
	})
}

func TestOperationsEstopAmbiguousClosePermissionLossRestoreFailureAndStrictState(t *testing.T) {
	t.Run("close timeout reconciles absence", func(t *testing.T) {
		_, control, app, _ := prepareEstop(t, "working")
		control.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("timeout after close")}
		if err := app.run([]string{"estop", "activate", "--id", "ESTOP-AMB-OK", "--reason", "test"}); err != nil {
			t.Fatal(err)
		}
		state, _, _ := app.Estop.Read()
		if state.Items[0].FreezeOutcome != "confirmed" {
			t.Fatalf("state=%+v", state)
		}
	})

	t.Run("ambiguous close never retries or releases", func(t *testing.T) {
		_, control, app, _ := prepareEstop(t, "working")
		control.closeMutates = false
		control.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("timeout")}
		args := []string{"estop", "activate", "--id", "ESTOP-AMB-STUCK", "--reason", "test"}
		if err := app.run(args); err == nil || !strings.Contains(err.Error(), "未全停") {
			t.Fatalf("got %v", err)
		}
		if err := app.run(args); err == nil {
			t.Fatal("repeat activate should remain partial")
		}
		if countFakeCalls(control, "tab close ") != 1 {
			t.Fatalf("ambiguous close blindly retried: %v", control.calls)
		}
		if err := app.run([]string{"estop", "release", "--id", "ESTOP-AMB-STUCK", "--reason", "unsafe"}); err == nil {
			t.Fatal("release accepted ambiguous freeze")
		}
		if countFakeCalls(control, "agent start ") != 0 {
			t.Fatal("release started ambiguous child")
		}
	})

	t.Run("release permission is re-read live", func(t *testing.T) {
		e, _, app, _ := prepareEstop(t, "working")
		if err := app.run([]string{"estop", "activate", "--id", "ESTOP-PERM", "--reason", "test"}); err != nil {
			t.Fatal(err)
		}
		if _, err := mutateConfig(e.config, func(cfg *Config) error {
			for i := range cfg.Agents {
				if cfg.Agents[i].Name == "penny" {
					cfg.Agents[i].CanManageStaff = false
					cfg.Agents[i].SeatDigest = employeeSeatDigest(cfg.Agents[i])
				}
				if cfg.Agents[i].Name == "zantianyou" {
					cfg.Agents[i].CanManageStaff = true
					cfg.Agents[i].SeatDigest = employeeSeatDigest(cfg.Agents[i])
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := app.cmdEstop([]string{"release", "--id", "ESTOP-PERM", "--reason", "test"}); err == nil || !strings.Contains(err.Error(), "can_manage_staff") {
			t.Fatalf("permission loss accepted: %v", err)
		}
		state, _, _ := app.Estop.Read()
		if state.State != "active" {
			t.Fatalf("permission loss changed state: %+v", state)
		}
	})

	t.Run("unproven restore rollback remains ambiguous and second release does not retry", func(t *testing.T) {
		_, control, app, _ := prepareEstop(t, "working")
		if err := app.run([]string{"estop", "activate", "--id", "ESTOP-RESTORE-FAIL", "--reason", "test"}); err != nil {
			t.Fatal(err)
		}
		control.promptOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("prompt timeout")}
		control.closeMutates = false
		control.closeOutcome = HerdrMutationResult{Outcome: herdrAmbiguous, Err: errors.New("rollback close timeout")}
		args := []string{"estop", "release", "--id", "ESTOP-RESTORE-FAIL", "--reason", "test"}
		if err := app.run(args); err == nil {
			t.Fatal("restore failure accepted")
		}
		starts := countFakeCalls(control, "agent start eng-developer ")
		if err := app.run(args); err == nil {
			t.Fatal("second failed release accepted")
		}
		if countFakeCalls(control, "agent start eng-developer ") != starts {
			t.Fatal("failed restore blindly retried")
		}
		state, _, _ := app.Estop.Read()
		if state.State != "active" || state.Items[0].RestoreOutcome != "ambiguous" {
			t.Fatalf("state=%+v", state)
		}
	})

	t.Run("sentinel symlink mode damage and duplicate fields fail closed", func(t *testing.T) {
		root := canonicalTestTempDir(t)
		state := EstopState{Version: estopStateVersion, EstopID: "ESTOP-STRICT", Actor: "penny",
			ActivatedAt: "2026-08-28T00:00:00Z", UpdatedAt: "2026-08-28T00:00:00Z", State: "active", Reason: "test", Items: []EstopItem{}}
		store := &FileEstopStore{Root: filepath.Join(root, "state")}
		if err := store.WithLock(func(locked *lockedEstopStore) error { return locked.Write(state) }); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(store.statePath(), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Read(); err == nil || !strings.Contains(err.Error(), "0600") {
			t.Fatalf("bad mode accepted: %v", err)
		}
		if err := os.Chmod(store.statePath(), 0o600); err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(store.statePath())
		duplicate := bytes.Replace(raw, []byte(`"state": "active"`), []byte(`"state": "active", "state": "active"`), 1)
		if err := os.WriteFile(store.statePath(), duplicate, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.Read(); err == nil || !strings.Contains(err.Error(), "重复 JSON 字段") {
			t.Fatalf("duplicate field accepted: %v", err)
		}
		target := filepath.Join(root, "target.json")
		if err := os.WriteFile(target, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		linkStore := &FileEstopStore{Root: filepath.Join(root, "link-root")}
		if err := os.Mkdir(linkStore.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, linkStore.statePath()); err != nil {
			t.Fatal(err)
		}
		if _, _, err := linkStore.Read(); err == nil || !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink accepted: %v", err)
		}
	})
}

func TestOperationsEstopAtomicWriteCrashFailpointsRecover(t *testing.T) {
	for _, failpoint := range []string{"after_temp_write", "after_temp_fsync", "after_rename", "after_parent_fsync"} {
		t.Run(failpoint, func(t *testing.T) {
			root := canonicalTestTempDir(t)
			triggered := false
			store := &FileEstopStore{Root: filepath.Join(root, "estop"), Failpoint: func(name string) error {
				if name == failpoint && !triggered {
					triggered = true
					return errors.New("synthetic crash")
				}
				return nil
			}}
			state := EstopState{Version: estopStateVersion, EstopID: "ESTOP-CRASH-" + strings.ToUpper(strings.ReplaceAll(failpoint, "_", "-")), Actor: "penny",
				ActivatedAt: "2026-08-28T00:00:00Z", UpdatedAt: "2026-08-28T00:00:00Z", State: "active", Reason: "test", Items: []EstopItem{}}
			err := store.WithLock(func(locked *lockedEstopStore) error { return locked.Write(state) })
			if err == nil {
				t.Fatalf("failpoint %s did not fire", failpoint)
			}
			store.Failpoint = nil
			if err := store.WithLock(func(locked *lockedEstopStore) error {
				if locked.state.EstopID != state.EstopID {
					return fmt.Errorf("recovery state=%+v", locked.state)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			got, exists, err := store.Read()
			if err != nil || !exists || got.EstopID != state.EstopID {
				t.Fatalf("got=%+v exists=%t err=%v", got, exists, err)
			}
		})
	}
}

func TestOperationsPermissionsAndNoRealHerdrFallback(t *testing.T) {
	e := setupTestEnv(t)
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	app := operationsTestApp(t, e, nil, &now)
	manager, _ := app.Config.exactRule("zantianyou")
	app.Identity = operationsIdentity{actor: Actor{Name: manager.Name, Rule: manager}}
	app.CallerPane = "manager-pane"
	before, _ := app.Store.ReadAll(app.Config)
	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-NOPERM", "--dedupe", "no-perm", "--to", "zantianyou", "--message", "x", "--ttl", "1m"}); err == nil || !strings.Contains(err.Error(), "can_issue") {
		t.Fatalf("non-can_issue actor accepted: %v", err)
	}
	after, _ := app.Store.ReadAll(app.Config)
	if len(after) != len(before) {
		t.Fatalf("permission rejection wrote events: %d -> %d", len(before), len(after))
	}

	app.Identity = operationsIdentity{actor: Actor{Name: "penny"}}
	app.CallerPane = "penny-pane"
	if err := app.cmdNudge([]string{"enqueue", "--id", "NUDGE-NOHERDR", "--dedupe", "no-herdr", "--to", "zantianyou", "--message", "x", "--ttl", "1m"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"claim", "--id", "NUDGE-NOHERDR", "--claim", "CLAIM-NOHERDR", "--lease", "30s"}); err != nil {
		t.Fatal(err)
	}
	if err := app.cmdNudge([]string{"deliver", "--id", "NUDGE-NOHERDR", "--claim", "CLAIM-NOHERDR"}); err == nil || !strings.Contains(err.Error(), "未注入") {
		t.Fatalf("nil Herdr fell back to PATH: %v", err)
	}
}
