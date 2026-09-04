package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestQueuedActionWatchdogWakesIdleTargetToConsumeBudgetDowngradedAction(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeAuto, 1)
	sender := deliveryPolicyTestApp(t, e, "zantianyou", "queued-action:manager")
	sender.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
	sender.DeliveryFailpoint = func(name string) error {
		if name == "after_attempt_recorded" {
			return errors.New("synthetic in-flight wake")
		}
		return nil
	}
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "eng-developer", "--kind", "request", "--text", "first wake remains in flight", "--delivery", "wakeup"); err == nil {
		t.Fatal("in-flight wake fixture did not stop after attempted")
	}
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "eng-developer", "--kind", "request", "--text", "continue exact assignment checkpoint", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	backlogs := ledger.queuedWakeBudgetActions(cfg)
	if len(backlogs) != 1 || backlogs[0].Target != "eng-developer" {
		t.Fatalf("queued action projection=%+v", backlogs)
	}
	view, ok, err := NewStore(e.data).Delivery(cfg, backlogs[0].DeliveryID)
	if err != nil || !ok || !strings.Contains(view.StatusDescription, "durable nudge") || !strings.Contains(view.NextAction, "delivery consume") {
		t.Fatalf("agent-facing queued recovery view=%+v ok=%t err=%v", view, ok, err)
	}
	selectedAt, err := parseOperationsTime("queued action", backlogs[0].SelectedAt)
	if err != nil {
		t.Fatal(err)
	}
	now := selectedAt.Add(defaultManagerQueueStallTimeout + time.Second)
	control := newFakeHerdrControl(e.root, cfg.WorkspaceLabel)
	addOperationsLive(&control.snapshot, cfg, e.root, "eng-developer", "done", "queued-action-worker")
	app := operationsTestApp(t, e, control, &now)
	if err := app.runQueuedActionWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompts := fakePromptCalls(control)
	if len(prompts) != 1 || !strings.HasPrefix(prompts[0], "prompt eng-developer ") ||
		!strings.Contains(prompts[0], "hq delivery consume") || !strings.Contains(prompts[0], backlogs[0].DeliveryID) {
		t.Fatalf("queued action recovery prompt=%v", prompts)
	}
	if err := app.runQueuedActionWatchdogOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(fakePromptCalls(control)); got != 1 {
		t.Fatalf("same queued action basis duplicated recovery nudge: prompts=%d", got)
	}
}

func TestQueuedActionProjectionIgnoresCallerRequestedInject(t *testing.T) {
	e := setupTestEnv(t)
	sender := deliveryPolicyTestApp(t, e, "zantianyou", "queued-action:explicit")
	sender.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeBusy, nil }
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "eng-developer", "--kind", "request", "--text", "caller requested silence", "--delivery", "inject"); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger, err := validateLedger(events, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := ledger.queuedWakeBudgetActions(cfg); len(got) != 0 {
		t.Fatalf("explicit inject was incorrectly promoted into delayed wake: %+v", got)
	}
}
