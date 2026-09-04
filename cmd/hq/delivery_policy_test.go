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
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func setDeliveryPolicy(t *testing.T, e testEnv, mode string, budget int) {
	setDeliveryPolicyWithBundle(t, e, mode, budget, 0, 0)
}

func setDeliveryPolicyWithBundle(t *testing.T, e testEnv, mode string, budget, maxItems, maxBytes int) {
	t.Helper()
	cfg := testConfig()
	cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: mode, MaxConsecutiveWakes: budget,
		MaxBundleItems: maxItems, MaxBundleBytes: maxBytes}
	raw, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(e.config, raw, 0o644); err != nil {
		t.Fatal(err)
	}
}

func deliveryPolicyTestApp(t *testing.T, e testEnv, actorName, pane string) *App {
	t.Helper()
	app := e.app(t)
	rule, ok := app.Config.exactRule(actorName)
	if !ok {
		t.Fatalf("missing actor %s", actorName)
	}
	cwd := filepath.Join(e.root, rule.Department)
	app.Identity = &fakeIdentityProvider{actors: map[string]Actor{
		pane: {Name: actorName, Label: rule.Label, Department: rule.Department, PaneID: pane, CWD: cwd, Rule: rule},
	}}
	app.CallerPane = pane
	return app
}

func runDeliveryPolicyTest(app *App, args ...string) (string, error) {
	var out, errOut bytes.Buffer
	app.Out, app.Err = &out, &errOut
	err := app.run(args)
	if err != nil && errOut.Len() != 0 {
		return out.String(), errors.New(err.Error() + ": " + errOut.String())
	}
	return out.String(), err
}

func deliveryPolicyMessages(t *testing.T, e testEnv) []Event {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	var selected []Event
	for _, event := range events {
		if event.Type == "message_prepared" {
			selected = append(selected, event)
		}
	}
	return selected
}

type atomicDeliveryTransport struct {
	calls atomic.Int32
}

func (transport *atomicDeliveryTransport) Deliver(string, string) DeliveryAttempt {
	transport.calls.Add(1)
	return DeliveryAttempt{Outcome: transportSent}
}

func TestDeliveryPolicyThreeModesNaturalWakeAndLogOnlyContext(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeAuto, 3)
	state := deliveryRuntimeIdle
	app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:sender")
	app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return state, nil }

	if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "request", "--text", "立即处理", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("wakeup model turns=%d want=1", len(e.transport.calls))
	}
	if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "info", "--text", "静默资料", "--delivery", "quiet"); err != nil {
		t.Fatal(err)
	}
	if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "info", "--text", "下一边界资料", "--delivery", "inject"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("quiet/inject unexpectedly woke target: calls=%d", len(e.transport.calls))
	}

	messages := deliveryPolicyMessages(t, e)
	if len(messages) != 3 {
		t.Fatalf("message origins=%d want=3", len(messages))
	}
	for index, want := range []struct {
		mode, target string
		wakeup       bool
	}{{deliveryModeWakeup, deliveryTargetNextTurn, true}, {deliveryModeQuiet, deliveryTargetNextTurn, false}, {deliveryModeInject, deliveryTargetNextStep, false}} {
		if messages[index].DeliveryMode != want.mode || messages[index].DeliveryTarget != want.target || eventWakesTarget(messages[index]) != want.wakeup {
			t.Fatalf("mode mapping[%d]=%+v", index, messages[index])
		}
	}

	consumer := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:target")
	consumer.JSON = true
	contextOutput, err := runDeliveryPolicyTest(consumer, "delivery", "consume")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(contextOutput, "静默资料") || !strings.Contains(contextOutput, "下一边界资料") || strings.Contains(contextOutput, "立即处理") {
		t.Fatalf("unexpected natural-wake context: %s", contextOutput)
	}
	for _, forbidden := range []string{"delivery_queued", "message_sent", "delivery_context_claimed", "queued", "delivered"} {
		if strings.Contains(contextOutput, forbidden) {
			t.Fatalf("coordination event leaked into model context: %q in %s", forbidden, contextOutput)
		}
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("natural consume added wake: %d", len(e.transport.calls))
	}

	cfg, _ := loadConfig(e.config)
	views, err := NewStore(e.data).Deliveries(cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.ContextState != deliveryContextClaimed {
			t.Fatalf("target history/pending not claimed: %+v", view)
		}
	}
}

func TestDeliveryPolicyWakeupBundlesBoundedFIFOContext(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicyWithBundle(t, e, deliveryModeWakeup, 3, 4, 32*1024)
	sender := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:bundle-sender")
	sender.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }

	bodies := make([]string, 0, 6)
	for index := 0; index < 6; index++ {
		prefix := fmt.Sprintf("quiet-%d-", index)
		body := prefix + strings.Repeat("x", maxMessageTextBytes-len(prefix))
		bodies = append(bodies, body)
		if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "info", "--text", body, "--delivery", "quiet"); err != nil {
			t.Fatal(err)
		}
	}
	if len(e.transport.calls) != 0 {
		t.Fatalf("quiet messages woke target: %d", len(e.transport.calls))
	}

	waker := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:bundle-sender")
	waker.JSON = true
	waker.DeliveryTargetState = sender.DeliveryTargetState
	response, err := runDeliveryPolicyTest(waker, "message", "--to", "zantianyou", "--kind", "request", "--text", "wake-now", "--delivery", "wakeup")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("merged wake generated %d prompts, want 1", len(e.transport.calls))
	}
	prompt := e.transport.calls[0].message
	if len([]byte(prompt)) <= 8*1024 {
		t.Fatalf("test did not cross old aggregate envelope limit: %d bytes", len([]byte(prompt)))
	}
	for _, body := range bodies[:4] {
		if !strings.Contains(prompt, body) {
			t.Fatalf("merged prompt omitted quiet body prefix %q", body[:16])
		}
		if strings.Contains(response, body) {
			t.Fatalf("recipient history leaked into sender response")
		}
	}
	for _, body := range bodies[4:] {
		if strings.Contains(prompt, body) {
			t.Fatalf("bounded prompt included overflow body prefix %q", body[:16])
		}
	}
	if got := strings.Count(prompt, "\n\n[HQ message]"); got != 4 {
		t.Fatalf("blank-line merged message boundaries=%d want=4", got)
	}

	consumer := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:bundle-target")
	output, err := runDeliveryPolicyTest(consumer, "delivery", "consume")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, bodies[4]) || !strings.Contains(output, bodies[5]) ||
		strings.Contains(output, bodies[0]) || strings.Index(output, bodies[4]) > strings.Index(output, bodies[5]) {
		t.Fatalf("consume did not return exact FIFO overflow suffix: %q", output)
	}
	view, err := consumer.deliveryBudgetView("zantianyou")
	if err != nil {
		t.Fatal(err)
	}
	if view.Spent != 0 {
		t.Fatalf("successful automatic turn boundary did not reset wake budget: %+v", view)
	}
}

func TestDeliveryPolicyAcceptAutomaticallyEmitsPendingQuietContext(t *testing.T) {
	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeAuto, 3)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "accept-auto-context.md"), "# accept auto context\n")
	manager := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:accept-manager")
	manager.Out, manager.Err = io.Discard, io.Discard
	if err := manager.run(caseCreateArgs("CASE-AUTO-ACCEPT-PARENT", "父事项", source)); err != nil {
		t.Fatal(err)
	}
	child := caseCreateArgs("CASE-AUTO-ACCEPT-CHILD", "子事项", source, "CASE-AUTO-ACCEPT-PARENT")
	if err := manager.run(child); err != nil {
		t.Fatal(err)
	}
	if err := manager.run([]string{"issue", "--case", "CASE-AUTO-ACCEPT-CHILD", "--to", "eng-developer", "--next", "接收并实现"}); err != nil {
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
	pending := pendingEvents(events, "eng-developer")
	if len(pending) != 1 {
		t.Fatalf("pending issue events=%d want=1", len(pending))
	}

	peer := deliveryPolicyTestApp(t, e, "eng-data-engineer", "delivery:accept-peer")
	peer.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeBusy, nil }
	if _, err := runDeliveryPolicyTest(peer, "message", "--to", "eng-developer", "--kind", "info", "--text", "accept 时自动带入的资料", "--delivery", "inject"); err != nil {
		t.Fatal(err)
	}

	target := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:accept-target")
	output, err := runDeliveryPolicyTest(target, "accept", "--event", pending[0].ID, "--next", "开始实现")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "业务已提交并已通知") || !strings.Contains(output, "[HQ message]") || !strings.Contains(output, "accept 时自动带入的资料") {
		t.Fatalf("accept did not automatically emit pending context: %q", output)
	}
	if !strings.Contains(output, "\n\n[HQ message]") {
		t.Fatalf("accept context lacks blank-line boundary: %q", output)
	}
	recovery, err := runDeliveryPolicyTest(target, "delivery", "consume")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recovery, "无静默消息") {
		t.Fatalf("accept-emitted context remained pending: %q", recovery)
	}
}

func TestDeliveryPolicyIssueDowngradeAndInvalidModeFailClosed(t *testing.T) {
	e := setupTestEnv(t)
	app := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:manager")
	if _, err := runDeliveryPolicyTest(app, "issue", "--case", "DELIVERY-ISSUE-001", "--to", "eng-developer", "--next", "执行", "--delivery", "quiet"); err == nil || !strings.Contains(err.Error(), "固定为 wakeup") {
		t.Fatalf("issue downgrade not rejected clearly: %v", err)
	}
	if _, err := runDeliveryPolicyTest(app, "message", "--to", "eng-developer", "--kind", "info", "--text", "x", "--delivery", "unknown"); err == nil || !strings.Contains(err.Error(), "auto|wakeup|quiet|inject") {
		t.Fatalf("unknown mode not rejected: %v", err)
	}
	if messages := deliveryPolicyMessages(t, e); len(messages) != 0 {
		t.Fatalf("rejected input wrote messages: %d", len(messages))
	}
}

func TestDeliveryPolicyConfigRuntimeIdentityAndPathFailClosed(t *testing.T) {
	t.Run("missing policy is deterministic and illegal policy is rejected", func(t *testing.T) {
		cfg := testConfig()
		policy := cfg.effectiveDeliveryPolicy()
		if policy.DefaultMode != deliveryModeWakeup || policy.MaxConsecutiveWakes != 3 ||
			policy.MaxBundleItems != defaultDeliveryBundleItems || policy.MaxBundleBytes != defaultDeliveryBundleBytes ||
			policy.ManagerQueueStallTimeout != defaultManagerQueueStallTimeout.String() ||
			policy.ManagerQueueEscalateAfter != defaultManagerQueueEscalateAfter.String() ||
			policy.MaxManagerQueueNudges != defaultMaxManagerQueueNudges {
			t.Fatalf("missing policy defaults=%+v", policy)
		}
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: "bad", MaxConsecutiveWakes: 3}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "default_mode") {
			t.Fatalf("illegal mode accepted: %v", err)
		}
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 0}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "1..100") {
			t.Fatalf("illegal budget accepted: %v", err)
		}
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 3, MaxBundleItems: -1}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "max_bundle_items") {
			t.Fatalf("illegal bundle item limit accepted: %v", err)
		}
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 3, MaxBundleBytes: maxDeliveryBundleBytes + 1}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "max_bundle_bytes") {
			t.Fatalf("illegal bundle byte limit accepted: %v", err)
		}
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 3, ManagerQueueStallTimeout: "14s"}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "manager_queue_stall_timeout") {
			t.Fatalf("illegal manager queue stall timeout accepted: %v", err)
		}
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 3,
			ManagerQueueStallTimeout: "30s", ManagerQueueEscalateAfter: "30s"}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "必须大于") {
			t.Fatalf("non-increasing manager escalation timeout accepted: %v", err)
		}
		cfg.DeliveryPolicy = &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 3, MaxManagerQueueNudges: 6}
		if err := validateConfig(cfg); err == nil || !strings.Contains(err.Error(), "max_manager_queue_nudges") {
			t.Fatalf("unbounded manager queue nudge count accepted: %v", err)
		}
	})

	t.Run("unknown runtime and identity fail before write", func(t *testing.T) {
		e := setupTestEnv(t)
		setDeliveryPolicy(t, e, deliveryModeAuto, 3)
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:unknown-runtime")
		app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return "", errors.New("runtime unavailable") }
		if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "info", "--text", "x", "--delivery", "auto"); err == nil || !strings.Contains(err.Error(), "runtime unavailable") {
			t.Fatalf("unknown runtime accepted: %v", err)
		}
		if _, err := runDeliveryPolicyTest(app, "message", "--to", "not-registered", "--kind", "info", "--text", "x", "--delivery", "inject"); err == nil || !strings.Contains(err.Error(), "未登记") {
			t.Fatalf("unknown identity accepted: %v", err)
		}
		if messages := deliveryPolicyMessages(t, e); len(messages) != 0 {
			t.Fatalf("fail-closed paths wrote %d origins", len(messages))
		}
	})

	t.Run("reference outside allowlist fails before write", func(t *testing.T) {
		e := setupTestEnv(t)
		outside := filepath.Join(canonicalTestTempDir(t), "outside.md")
		if err := os.WriteFile(outside, []byte("# outside\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:path")
		if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "info", "--text", "x", "--ref", outside, "--delivery", "inject"); err == nil {
			t.Fatal("outside reference accepted")
		}
		if messages := deliveryPolicyMessages(t, e); len(messages) != 0 {
			t.Fatalf("bad path wrote %d origins", len(messages))
		}
	})
}

func TestDeliveryPolicyAdaptiveBusyMergeAndWakeBudget(t *testing.T) {
	t.Run("busy messages merge into one natural boundary", func(t *testing.T) {
		e := setupTestEnv(t)
		setDeliveryPolicy(t, e, deliveryModeAuto, 2)
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:busy-sender")
		app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeBusy, nil }
		for _, body := range []string{"busy-a", "busy-b", "busy-c"} {
			if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "request", "--text", body, "--delivery", "auto"); err != nil {
				t.Fatal(err)
			}
		}
		if len(e.transport.calls) != 0 {
			t.Fatalf("busy adaptive delivery woke model: %d", len(e.transport.calls))
		}
		consumer := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:busy-target")
		output, err := runDeliveryPolicyTest(consumer, "delivery", "consume")
		if err != nil {
			t.Fatal(err)
		}
		if !(strings.Index(output, "busy-a") < strings.Index(output, "busy-b") && strings.Index(output, "busy-b") < strings.Index(output, "busy-c")) {
			t.Fatalf("merged context lost FIFO: %s", output)
		}
		if strings.Count(output, "\n\n") != 3 || strings.Count(output, "\n\n[HQ message]") != 2 || !strings.HasSuffix(output, "\n\n") {
			t.Fatalf("merged context must separate every message with a blank line: %q", output)
		}
	})

	t.Run("successful standalone wakeup resets the consecutive budget", func(t *testing.T) {
		e := setupTestEnv(t)
		setDeliveryPolicy(t, e, deliveryModeAuto, 1)
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:budget-sender")
		app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
		for _, body := range []string{"wake-1", "wake-2", "wake-3"} {
			if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "request", "--text", body, "--delivery", "wakeup"); err != nil {
				t.Fatal(err)
			}
		}
		if len(e.transport.calls) != 3 {
			t.Fatalf("successful turn boundaries left the wake budget stuck: %d", len(e.transport.calls))
		}
		messages := deliveryPolicyMessages(t, e)
		for _, message := range messages {
			if message.DeliveryMode != deliveryModeWakeup {
				t.Fatalf("standalone wake was unexpectedly downgraded: %+v", message)
			}
		}
		query := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:budget-query")
		budget, err := runDeliveryPolicyTest(query, "delivery", "budget", "status", "--target", "zantianyou")
		if err != nil || !strings.Contains(budget, "wakes=0/1") || strings.Contains(budget, "downgrade=true") {
			t.Fatalf("budget query=%q err=%v", budget, err)
		}
	})

	t.Run("in-flight wake exhaustion queues until target consumes context", func(t *testing.T) {
		e := setupTestEnv(t)
		setDeliveryPolicy(t, e, deliveryModeAuto, 1)
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:budget-inflight")
		app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
		app.DeliveryFailpoint = func(name string) error {
			if name == "after_attempt_recorded" {
				return errors.New("synthetic in-flight wake")
			}
			return nil
		}
		if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "request", "--text", "wake-in-flight", "--delivery", "wakeup"); err == nil {
			t.Fatal("in-flight wake fixture did not stop after attempted")
		}
		if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "request", "--text", "queued-action", "--delivery", "wakeup"); err != nil {
			t.Fatal(err)
		}
		messages := deliveryPolicyMessages(t, e)
		if len(messages) != 2 || messages[1].DeliveryMode != deliveryModeInject || messages[1].DeliveryReason != "wake-budget-exhausted" {
			t.Fatalf("action was not durably downgraded behind the in-flight wake: %+v", messages)
		}
		query := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:budget-query")
		budget, err := runDeliveryPolicyTest(query, "delivery", "budget", "status", "--target", "zantianyou")
		if err != nil || !strings.Contains(budget, "wakes=1/1") || !strings.Contains(budget, "downgrade=true") {
			t.Fatalf("budget query=%q err=%v", budget, err)
		}
		consumer := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:budget-target")
		output, err := runDeliveryPolicyTest(consumer, "delivery", "consume")
		if err != nil || !strings.Contains(output, "queued-action") {
			t.Fatalf("consume output=%q err=%v", output, err)
		}
		budget, err = runDeliveryPolicyTest(query, "delivery", "budget", "status", "--target", "zantianyou")
		if err != nil || !strings.Contains(budget, "wakes=0/1") {
			t.Fatalf("natural wake did not reset budget: %q err=%v", budget, err)
		}
	})
}

func TestDeliveryPolicyCrashRetryTargetDedupeAndConcurrentIdempotency(t *testing.T) {
	t.Run("accepted before consume survives retry", func(t *testing.T) {
		e := setupTestEnv(t)
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:crash-sender")
		app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeBusy, nil }
		fired := false
		app.DeliveryFailpoint = func(name string) error {
			if name == "after_silent_queued" && !fired {
				fired = true
				return errors.New("synthetic crash")
			}
			return nil
		}
		args := []string{"message", "--to", "zantianyou", "--kind", "info", "--text", "crash-window", "--delivery", "inject"}
		if _, err := runDeliveryPolicyTest(app, args...); err == nil {
			t.Fatal("crash failpoint did not fire")
		}
		retry := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:crash-sender")
		if _, err := runDeliveryPolicyTest(retry, args...); err != nil {
			t.Fatal(err)
		}
		cfg, _ := loadConfig(e.config)
		events, err := NewStore(e.data).ReadAll(cfg)
		if err != nil {
			t.Fatal(err)
		}
		origins, queued := 0, 0
		for _, event := range events {
			if event.Type == "message_prepared" {
				origins++
			}
			if event.Type == "delivery_queued" {
				queued++
			}
		}
		if origins != 1 || queued != 1 {
			t.Fatalf("retry duplicated sender/target state: origins=%d queued=%d", origins, queued)
		}
	})

	t.Run("concurrent same message remains one", func(t *testing.T) {
		e := setupTestEnv(t)
		const workers = 8
		apps := make([]*App, workers)
		for index := range apps {
			apps[index] = deliveryPolicyTestApp(t, e, "eng-developer", "delivery:concurrent")
			apps[index].Out, apps[index].Err = io.Discard, io.Discard
			apps[index].DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeBusy, nil }
		}
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for index := 0; index < workers; index++ {
			wg.Add(1)
			go func(app *App) {
				defer wg.Done()
				errs <- app.run([]string{"message", "--to", "zantianyou", "--kind", "info", "--text", "same", "--thread", "DELIVERY-THREAD", "--delivery", "inject"})
			}(apps[index])
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if messages := deliveryPolicyMessages(t, e); len(messages) != 1 {
			t.Fatalf("concurrent idempotency origins=%d", len(messages))
		}
	})

	t.Run("concurrent wake admissions reserve one shared budget", func(t *testing.T) {
		e := setupTestEnv(t)
		setDeliveryPolicy(t, e, deliveryModeAuto, 2)
		const workers = 8
		transport := &atomicDeliveryTransport{}
		apps := make([]*App, workers)
		for index := range apps {
			apps[index] = deliveryPolicyTestApp(t, e, "eng-developer", "delivery:budget-concurrent")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			t.Cleanup(cancel)
			apps[index].RequestContext = ctx
			if store, ok := apps[index].Store.(*Store); ok {
				apps[index].Store = store.withRequestContext(ctx)
			}
			apps[index].Out, apps[index].Err = io.Discard, io.Discard
			apps[index].Transport = transport
			apps[index].DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
		}
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		for index := 0; index < workers; index++ {
			wg.Add(1)
			go func(index int, app *App) {
				defer wg.Done()
				errs <- app.run([]string{"message", "--to", "zantianyou", "--kind", "request", "--text", "wake-" + string(rune('a'+index)), "--delivery", "wakeup"})
			}(index, apps[index])
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				t.Fatal(err)
			}
		}
		if got := transport.calls.Load(); got != 2 {
			t.Fatalf("concurrent wake calls=%d want=2", got)
		}
		wakes, injected := 0, 0
		for _, message := range deliveryPolicyMessages(t, e) {
			switch message.DeliveryMode {
			case deliveryModeWakeup:
				wakes++
			case deliveryModeInject:
				injected++
			}
		}
		if wakes != 2 || injected != workers-2 {
			t.Fatalf("concurrent modes wakeup=%d inject=%d", wakes, injected)
		}
	})
}

func TestDeliveryPolicyOfflineFIFOAndWakeupColdResume(t *testing.T) {
	t.Run("offline quiet and inject wait FIFO without wake", func(t *testing.T) {
		e := setupTestEnv(t)
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:offline-sender")
		app.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeOffline, nil }
		for _, item := range []struct{ body, mode string }{{"offline-a", "quiet"}, {"offline-b", "inject"}, {"offline-c", "quiet"}} {
			if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "info", "--text", item.body, "--delivery", item.mode); err != nil {
				t.Fatal(err)
			}
		}
		if len(e.transport.calls) != 0 {
			t.Fatalf("offline silent delivery woke target: %d", len(e.transport.calls))
		}
		consumer := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:offline-target")
		output, err := runDeliveryPolicyTest(consumer, "delivery", "consume")
		if err != nil {
			t.Fatal(err)
		}
		if !(strings.Index(output, "offline-a") < strings.Index(output, "offline-b") && strings.Index(output, "offline-b") < strings.Index(output, "offline-c")) {
			t.Fatalf("offline FIFO mismatch: %s", output)
		}
		if len(e.transport.calls) != 0 {
			t.Fatal("FIFO catch-up woke target")
		}
	})

	t.Run("offline wakeup cold resumes then prompts once", func(t *testing.T) {
		e := setupTestEnv(t)
		if _, err := mutateConfig(e.config, func(cfg *Config) error {
			for index := range cfg.Agents {
				if cfg.Agents[index].Name == "zantianyou" {
					cfg.Agents[index].ActivationPolicy = activationAlways
					cfg.Agents[index].SeatDigest = employeeSeatDigest(cfg.Agents[index])
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		var resumed atomic.Bool
		var resumeCalls atomic.Int32
		app := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:cold-sender")
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
		if _, err := runDeliveryPolicyTest(app, "message", "--to", "zantianyou", "--kind", "request", "--text", "cold-action", "--delivery", "wakeup"); err != nil {
			t.Fatal(err)
		}
		if resumeCalls.Load() != 1 || len(e.transport.calls) != 1 {
			t.Fatalf("cold resume=%d prompt=%d", resumeCalls.Load(), len(e.transport.calls))
		}
	})
}

func TestOnAssignmentDeliveryResumesExistingContract(t *testing.T) {
	prepare := func(t *testing.T, caseID string) (testEnv, *App, *App, Event) {
		t.Helper()
		e := setupTestEnv(t)
		if _, err := mutateConfig(e.config, func(cfg *Config) error {
			for index := range cfg.Agents {
				switch cfg.Agents[index].Name {
				case "eng-developer":
					cfg.Agents[index].ActivationPolicy = activationOnAssignment
					finalizeTestSeatMutation(&cfg.Agents[index])
				case "zantianyou":
					cfg.Agents[index].ActivationPolicy = activationAlways
					finalizeTestSeatMutation(&cfg.Agents[index])
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		source := writeTestFile(t, filepath.Join(e.root, "engineering", caseID+"-source.md"), "# assignment source\n")
		manager := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:contract-manager")
		manager.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
		if _, err := runDeliveryPolicyTest(manager, "case", "create", "--id", caseID, "--title", "contract resume", "--project", "contract-resume", "--source", source); err != nil {
			t.Fatal(err)
		}
		if _, err := runDeliveryPolicyTest(manager, "issue", "--case", caseID, "--to", "eng-developer", "--next", "implement and verify"); err != nil {
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
		issue := latestEventOfType(events, "issue_sent")
		if issue.ID == "" {
			t.Fatal("missing issue_sent")
		}
		worker := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:contract-worker")
		worker.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
		if _, err := runDeliveryPolicyTest(worker, "accept", "--event", issue.ID, "--next", "start work"); err != nil {
			t.Fatal(err)
		}
		return e, manager, worker, issue
	}

	t.Run("returned report reconciles the original prepared delivery exactly once", func(t *testing.T) {
		e, manager, worker, issue := prepare(t, "CASE-CONTRACT-RETURN")
		artifact := writeTestFile(t, filepath.Join(e.root, "engineering", "contract-return-result.md"), "# result\n")
		if _, err := runDeliveryPolicyTest(worker, "report", "--case", "CASE-CONTRACT-RETURN", "--result", "completed", "--artifact", artifact, "--verify", "reviewed", "--next", "review result"); err != nil {
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
		report := latestEventOfType(events, "report_sent")
		if report.ID == "" {
			t.Fatal("missing report_sent")
		}

		manager.DeliveryTargetState = func(string) (deliveryRuntimeState, error) {
			return "", errors.New("synthetic inspector outage before delivery attempt")
		}
		beforePrompts := len(e.transport.calls)
		if _, err := runDeliveryPolicyTest(manager, "return", "--event", report.ID, "--reason", "needs correction", "--next", "revise original artifact"); err == nil || !strings.Contains(err.Error(), "synthetic inspector outage") {
			t.Fatalf("return did not preserve the pre-attempt failure: %v", err)
		}
		events, err = NewStore(e.data).ReadAll(cfg)
		if err != nil {
			t.Fatal(err)
		}
		returned := latestEventOfType(events, "event_returned")
		if returned.ID == "" {
			t.Fatal("return business event was not committed")
		}
		view, ok, err := NewStore(e.data).Delivery(cfg, returned.DeliveryID)
		if err != nil || !ok || view.Status != deliveryPrepared || view.AttemptCount != 0 {
			t.Fatalf("return delivery did not remain safely prepared: view=%+v ok=%v err=%v", view, ok, err)
		}
		returnLedger, err := manager.ledgerState()
		if err != nil {
			t.Fatal(err)
		}
		returnAssignment, returnAllowed, returnErr := assignmentAuthorizingResume(returnLedger, returned)
		if returnErr != nil || !returnAllowed || returnAssignment == nil {
			t.Fatalf("returned assignment did not authorize resume: returned=%+v assignment=%+v allowed=%v err=%v ledger_assignment=%+v",
				returned, returnAssignment, returnAllowed, returnErr, returnLedger.assignments[issue.ID])
		}

		var resumed atomic.Bool
		var resumeCalls atomic.Int32
		manager.DeliveryTargetState = func(string) (deliveryRuntimeState, error) {
			if resumed.Load() {
				return deliveryRuntimeIdle, nil
			}
			return deliveryRuntimeOffline, nil
		}
		manager.DeliveryColdResume = func(target string) error {
			if target != "eng-developer" {
				t.Fatalf("cold-resume target=%s", target)
			}
			resumeCalls.Add(1)
			resumed.Store(true)
			return nil
		}
		if _, err := runDeliveryPolicyTest(manager, "reconcile"); err != nil {
			t.Fatal(err)
		}
		view, ok, err = NewStore(e.data).Delivery(cfg, returned.DeliveryID)
		if err != nil || !ok || view.Status != deliverySent || view.AttemptCount != 1 {
			t.Fatalf("reconciled return delivery=%+v ok=%v err=%v", view, ok, err)
		}
		if resumeCalls.Load() != 1 || len(e.transport.calls) != beforePrompts+1 {
			t.Fatalf("resume=%d prompts=%d want prompts=%d", resumeCalls.Load(), len(e.transport.calls), beforePrompts+1)
		}

		if _, err := runDeliveryPolicyTest(manager, "reconcile"); err != nil {
			t.Fatal(err)
		}
		if resumeCalls.Load() != 1 || len(e.transport.calls) != beforePrompts+1 {
			t.Fatal("idempotent reconcile repeated cold-resume or prompt")
		}
		events, err = NewStore(e.data).ReadAll(cfg)
		if err != nil {
			t.Fatal(err)
		}
		issueCount := 0
		for _, event := range events {
			if event.Type == "issue_sent" {
				issueCount++
			}
		}
		ledger, err := manager.ledgerState()
		if err != nil {
			t.Fatal(err)
		}
		assignment := ledger.assignments[issue.ID]
		if issueCount != 1 || assignment == nil || assignment.Status != "rework" || assignment.Consumed {
			t.Fatalf("resume changed assignment identity/state: issues=%d assignment=%+v", issueCount, assignment)
		}
	})

	t.Run("action message from contract manager resumes but unrelated message does not authorize", func(t *testing.T) {
		e, manager, _, _ := prepare(t, "CASE-CONTRACT-MESSAGE")
		var resumed atomic.Bool
		var resumeCalls atomic.Int32
		manager.DeliveryTargetState = func(string) (deliveryRuntimeState, error) {
			if resumed.Load() {
				return deliveryRuntimeIdle, nil
			}
			return deliveryRuntimeOffline, nil
		}
		manager.DeliveryColdResume = func(string) error {
			resumeCalls.Add(1)
			resumed.Store(true)
			return nil
		}
		beforePrompts := len(e.transport.calls)
		if _, err := runDeliveryPolicyTest(manager, "message", "--to", "eng-developer", "--case", "CASE-CONTRACT-MESSAGE", "--kind", "request", "--text", "continue the active assignment", "--delivery", "wakeup"); err != nil {
			t.Fatal(err)
		}
		if resumeCalls.Load() != 1 || len(e.transport.calls) != beforePrompts+1 {
			t.Fatalf("manager action did not resume once: resume=%d prompts=%d", resumeCalls.Load(), len(e.transport.calls))
		}

		ledger, err := manager.ledgerState()
		if err != nil {
			t.Fatal(err)
		}
		messages := deliveryPolicyMessages(t, e)
		origin := messages[len(messages)-1]
		origin.CaseID = "CASE-UNRELATED"
		if assignment, allowed, err := assignmentAuthorizingResume(ledger, origin); err != nil || allowed || assignment != nil {
			t.Fatalf("unrelated message acquired assignment authority: assignment=%+v allowed=%v err=%v", assignment, allowed, err)
		}
		origin.CaseID = "CASE-CONTRACT-MESSAGE"
		origin.Actor = "penny"
		if assignment, allowed, err := assignmentAuthorizingResume(ledger, origin); err != nil || allowed || assignment != nil {
			t.Fatalf("non-contract actor acquired assignment authority: assignment=%+v allowed=%v err=%v", assignment, allowed, err)
		}
		origin.Actor = "zantianyou"
		origin.MessageKind = "info"
		if assignment, allowed, err := assignmentAuthorizingResume(ledger, origin); err != nil || allowed || assignment != nil {
			t.Fatalf("informational message acquired assignment authority: assignment=%+v allowed=%v err=%v", assignment, allowed, err)
		}

		resumed.Store(false)
		beforeDeniedPrompts, beforeDeniedResumes := len(e.transport.calls), resumeCalls.Load()
		_, deliveryErr := runDeliveryPolicyTest(manager, "message", "--to", "eng-developer", "--kind", "request", "--text", "unbound wake must not start seat", "--delivery", "wakeup")
		if deliveryErr == nil {
			t.Fatal("case-unbound action message unexpectedly started on_assignment seat")
		}
		for _, want := range []string{"hq assignment list --assignee eng-developer", "hq message --to eng-developer --case CASE_ID", "hq issue", "hq reconcile", "复用已记账 delivery=", "不要重发原业务", "不要裸用 herdr prompt"} {
			if !strings.Contains(deliveryErr.Error(), want) {
				t.Fatalf("offline correction missing %q: %v", want, deliveryErr)
			}
		}
		if strings.Contains(deliveryErr.Error(), "hq delivery reconcile") {
			t.Fatalf("offline correction named nonexistent subcommand: %v", deliveryErr)
		}
		if len(e.transport.calls) != beforeDeniedPrompts || resumeCalls.Load() != beforeDeniedResumes {
			t.Fatalf("unauthorized message reached runtime: prompts=%d resume=%d", len(e.transport.calls), resumeCalls.Load())
		}

		// Simulate the documented recovery's first step: another authorized
		// assignment-bound event has restored the target. Reconcile must now
		// deliver the original prepared ID without a second cold-resume.
		resumed.Store(true)
		if _, err := runDeliveryPolicyTest(manager, "reconcile"); err != nil {
			t.Fatal(err)
		}
		if len(e.transport.calls) != beforeDeniedPrompts+1 || resumeCalls.Load() != beforeDeniedResumes {
			t.Fatalf("prepared recovery did not reuse online target exactly once: prompts=%d resume=%d", len(e.transport.calls), resumeCalls.Load())
		}
	})
}

type deliveryFailingWriter struct {
	writes  int
	failAt  int
	partial bool
	buffer  bytes.Buffer
}

func (writer *deliveryFailingWriter) Write(payload []byte) (int, error) {
	writer.writes++
	if writer.writes == writer.failAt {
		if writer.partial && len(payload) > 1 {
			return writer.buffer.Write(payload[:len(payload)/2])
		}
		return 0, errors.New("synthetic context writer failure")
	}
	return writer.buffer.Write(payload)
}

func countDeliveryEvents(t *testing.T, e testEnv, eventType string) int {
	t.Helper()
	cfg, err := loadConfig(e.config)
	if err != nil {
		t.Fatal(err)
	}
	events, err := NewStore(e.data).ReadAll(cfg)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == eventType {
			count++
		}
	}
	return count
}

func prepareDeliveryWriterFailure(t *testing.T) testEnv {
	t.Helper()
	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeAuto, 3)
	state := deliveryRuntimeIdle
	sender := deliveryPolicyTestApp(t, e, "eng-developer", "delivery:writer-sender")
	sender.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return state, nil }
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "request", "--text", "budget-wake", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}
	state = deliveryRuntimeBusy
	for _, body := range []string{"silent-a", "silent-b", "silent-c"} {
		if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "info", "--text", body, "--delivery", "inject"); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestDeliveryPolicyConsumeWriterFailureClaimsOnlyProvenContext(t *testing.T) {
	t.Run("text claims each complete line and retries the untouched suffix", func(t *testing.T) {
		e := prepareDeliveryWriterFailure(t)
		consumer := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:text-consumer")
		writer := &deliveryFailingWriter{failAt: 2}
		consumer.Out, consumer.Err = writer, io.Discard
		if err := consumer.run([]string{"delivery", "consume"}); err == nil || !strings.Contains(err.Error(), "synthetic context writer failure") {
			t.Fatalf("text failure not surfaced: %v", err)
		}
		if got := writer.buffer.String(); !strings.Contains(got, "silent-a") || strings.Contains(got, "silent-b") || strings.Contains(got, "silent-c") {
			t.Fatalf("unexpected first attempt output: %q", got)
		}
		if got := countDeliveryEvents(t, e, "delivery_context_claimed"); got != 1 {
			t.Fatalf("claimed after text failure=%d want=1", got)
		}
		if got := countDeliveryEvents(t, e, "delivery_budget_reset"); got != 1 {
			t.Fatalf("writer failure changed the standalone-wake reset count=%d", got)
		}

		retry := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:text-consumer")
		output, err := runDeliveryPolicyTest(retry, "delivery", "consume")
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(output, "silent-a") || !(strings.Index(output, "silent-b") < strings.Index(output, "silent-c")) {
			t.Fatalf("retry did not return exact FIFO suffix: %q", output)
		}
		if got := countDeliveryEvents(t, e, "delivery_context_claimed"); got != 3 {
			t.Fatalf("claimed after retry=%d want=3", got)
		}
		if got := countDeliveryEvents(t, e, "delivery_budget_reset"); got != 1 {
			t.Fatalf("context retry duplicated the standalone-wake reset=%d", got)
		}
	})

	t.Run("json write failure claims none and retries the full array", func(t *testing.T) {
		e := prepareDeliveryWriterFailure(t)
		consumer := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:json-consumer")
		consumer.JSON = true
		writer := &deliveryFailingWriter{failAt: 1, partial: true}
		consumer.Out, consumer.Err = writer, io.Discard
		if err := consumer.run([]string{"delivery", "consume"}); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("partial JSON write should be short write, got %v", err)
		}
		if got := countDeliveryEvents(t, e, "delivery_context_claimed"); got != 0 {
			t.Fatalf("JSON failure claimed unproven items=%d", got)
		}
		if got := countDeliveryEvents(t, e, "delivery_budget_reset"); got != 1 {
			t.Fatalf("JSON failure changed the standalone-wake reset count=%d", got)
		}

		retry := deliveryPolicyTestApp(t, e, "zantianyou", "delivery:json-consumer")
		retry.JSON = true
		output, err := runDeliveryPolicyTest(retry, "delivery", "consume")
		if err != nil {
			t.Fatal(err)
		}
		var items []DeliveryContextItem
		if err := json.Unmarshal([]byte(output), &items); err != nil {
			t.Fatalf("retry JSON invalid: %v output=%q", err, output)
		}
		if len(items) != 3 || items[0].Text != "silent-a" || items[1].Text != "silent-b" || items[2].Text != "silent-c" {
			t.Fatalf("retry JSON did not preserve full FIFO: %+v", items)
		}
		if got := countDeliveryEvents(t, e, "delivery_context_claimed"); got != 3 {
			t.Fatalf("JSON retry claimed=%d want=3", got)
		}
	})
}

func TestDeliveryPolicyCLI(t *testing.T) {
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	message, _, err := root.Find([]string{"message"})
	if err != nil {
		t.Fatal(err)
	}
	if err := message.ParseFlags([]string{"--to", "zantianyou", "--kind", "info", "--text", "x", "--delivery", "inject"}); err != nil {
		t.Fatal(err)
	}
	if args := cobraLeafArgs(message, nil); !containsString(args, "--delivery=inject") {
		t.Fatalf("public Cobra did not forward delivery value: %v", args)
	}
	consume, _, err := root.Find([]string{"delivery", "consume"})
	if err != nil {
		t.Fatal(err)
	}
	if err := consume.ParseFlags([]string{"--limit", "7"}); err != nil {
		t.Fatal(err)
	}
	if args := cobraLeafArgs(consume, nil); !containsString(args, "--limit=7") {
		t.Fatalf("public Cobra did not forward consume limit: %v", args)
	}

	binary := filepath.Join(canonicalTestTempDir(t), "hq")
	build := exec.Command("go", "build", "-trimpath", "-o", binary, ".")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build public binary: %v\n%s", err, output)
	}
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"message", "--help"}, want: []string{"--delivery", "auto|wakeup|quiet|inject"}},
		{args: []string{"delivery", "--help"}, want: []string{"budget", "consume"}},
		{args: []string{"delivery", "budget", "status", "--help"}, want: []string{"--target"}},
		{args: []string{"delivery", "consume", "--help"}, want: []string{"--limit", `(default "100")`}},
	} {
		command := exec.Command(binary, test.args...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("built binary help %v failed: %v\n%s", test.args, err, output)
		}
		for _, want := range test.want {
			if !strings.Contains(string(output), want) {
				t.Fatalf("built binary help %v missing %q:\n%s", test.args, want, output)
			}
		}
	}
	for _, args := range [][]string{nil, {"delivery"}, {"delivery", "budget"}, {"delivery", "consume", "extra"}} {
		command := exec.Command(binary, args...)
		output, err := command.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok || exitErr.ExitCode() != exitUsage || !strings.Contains(string(output), "用法：") {
			t.Fatalf("built binary usage contract args=%v err=%v output=%s", args, err, output)
		}
	}

	e := setupTestEnv(t)
	setDeliveryPolicy(t, e, deliveryModeAuto, 3)
	productionConfigDir := filepath.Join(e.office, "tools", "hq")
	if err := os.MkdirAll(productionConfigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configBytes, err := os.ReadFile(e.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(productionConfigDir, "config.yaml"), configBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	fakeBinDir := canonicalTestTempDir(t)
	herdrBytes, err := os.ReadFile(e.herdr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fakeBinDir, "herdr"), herdrBytes, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBinDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HERDR_ENV", "1")
	t.Setenv("HERDR_WORKSPACE_ID", "delivery-public-workspace")

	e.setActor(t, "eng-developer", "delivery:public-sender", filepath.Join(e.root, "engineering"))
	e.setActor(t, "zantianyou", "delivery:public-target", filepath.Join(e.root, "engineering"))
	if err := os.MkdirAll(e.data, 0o755); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(e.data, "hq.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	server := e.app(t)
	server.GatewayWorkspaceID, server.GatewayServerID = "delivery-public-workspace", "delivery-public-gateway"
	server.DeliveryTargetState = func(string) (deliveryRuntimeState, error) { return deliveryRuntimeIdle, nil }
	server.Out, server.Err = io.Discard, io.Discard
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			server.handleGatewayConn(connection)
		}
	}()
	defer func() {
		_ = listener.Close()
		<-serverDone
	}()

	runPublic := func(pane string, args ...string) string {
		t.Helper()
		t.Setenv("HERDR_PANE_ID", pane)
		commandArgs := append([]string{"--office", e.office}, args...)
		command := exec.Command(binary, commandArgs...)
		command.Env = os.Environ()
		output, runErr := command.CombinedOutput()
		if runErr != nil {
			t.Fatalf("public binary %v failed: %v\n%s", args, runErr, output)
		}
		return string(output)
	}
	for _, request := range []struct {
		body string
		mode string
	}{
		{body: "public-wakeup", mode: deliveryModeWakeup},
		{body: "public-quiet", mode: deliveryModeQuiet},
		{body: "public-inject", mode: deliveryModeInject},
	} {
		runPublic("delivery:public-sender", "message", "--to", "zantianyou", "--kind", "info", "--text", request.body, "--delivery", request.mode)
	}
	if got := len(e.transport.calls); got != 1 {
		t.Fatalf("public binary mode model turns=%d want wakeup/quiet/inject=1/0/0", got)
	}
	budget := runPublic("delivery:public-sender", "delivery", "budget", "status", "--target", "zantianyou")
	if !strings.Contains(budget, "target=zantianyou") || !strings.Contains(budget, "wakes=0/3") {
		t.Fatalf("public budget status did not reach handler: %q", budget)
	}
	first := runPublic("delivery:public-target", "delivery", "consume", "--limit", "1")
	if !strings.Contains(first, "public-quiet") || strings.Contains(first, "public-inject") || strings.Contains(first, "public-wakeup") {
		t.Fatalf("public consume limit did not reach handler: %q", first)
	}
	second := runPublic("delivery:public-target", "delivery", "consume", "--limit", "1")
	if !strings.Contains(second, "public-inject") || strings.Contains(second, "public-quiet") || strings.Contains(second, "public-wakeup") {
		t.Fatalf("public consume FIFO suffix mismatch: %q", second)
	}
	if got := len(e.transport.calls); got != 1 {
		t.Fatalf("public quiet/inject consume added model wake: %d", got)
	}
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}
