package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

func caseCreateArgs(id, title, source string, parent ...string) []string {
	args := []string{"case", "create", "--id", id, "--title", title,
		"--objective", "完成目标", "--acceptance", "结果可复验", "--constraints", "遵守岗位边界",
		"--priority", "P1", "--source", source}
	if len(parent) == 0 {
		return append(args, "--project", "case-test-project")
	}
	return append(args, "--parent", parent[0])
}

func TestCaseManagerSplitsChildCasesWithoutMutatingParent(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "case.md"), "# case v2\n")
	e.setActor(t, "zantianyou", "case:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, caseCreateArgs("CASE-ROOT", "工程根事项", source)...)
	childArgs := caseCreateArgs("CASE-CHILD-A", "实现子事项", source, "CASE-ROOT")
	runTestCommand(t, e, childArgs...)
	runTestCommand(t, e, "issue", "--case", "CASE-CHILD-A", "--to", "eng-developer", "--next", "实现并回报")

	ledger, err := e.app(t).ledgerState()
	if err != nil {
		t.Fatal(err)
	}
	parent, child := ledger.snapshot.Cases["CASE-ROOT"], ledger.snapshot.Cases["CASE-CHILD-A"]
	if parent.Status != "open" || parent.Owner != "zantianyou" {
		t.Fatalf("parent was mutated by child issue: %+v", parent)
	}
	if child.ParentCaseID != parent.ID || child.RootCaseID != parent.ID || child.Status != "dispatched" || child.Owner != "eng-developer" {
		t.Fatalf("bad child projection: %+v", child)
	}
	if len(e.transport.calls) != 1 || !strings.HasPrefix(e.transport.calls[0].message, "[HQ notification]") || strings.Contains(e.transport.calls[0].message, "TASK=") {
		t.Fatalf("bad case-only HQ envelope: %+v", e.transport.calls)
	}
}

func TestIssueAcceptsTwoKiBNextBeyondFormerDoorbellLimit(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "two-kib-next.md"), "# 2 KiB next\n")
	e.setActor(t, "zantianyou", "case:two-kib-next", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, caseCreateArgs("CASE-2K-ROOT", "2 KiB 业务叙述", source)...)
	child := caseCreateArgs("CASE-2K-VALID", "合法 2 KiB next", source, "CASE-2K-ROOT")
	runTestCommand(t, e, child...)

	valid := strings.Repeat("x", maxBusinessTextBytes)
	runTestCommand(t, e, "issue", "--case", "CASE-2K-VALID", "--to", "eng-developer", "--next", valid)
	if len(e.transport.calls) != 1 || !strings.Contains(e.transport.calls[0].message, valid) || len([]byte(e.transport.calls[0].message)) <= 1000 {
		t.Fatalf("2 KiB next did not cross and survive the former doorbell limit: %+v", e.transport.calls)
	}
	events, err := NewStore(e.data).ReadAll(testConfig())
	if err != nil {
		t.Fatal(err)
	}
	issue := latestCaseEvent(events, "CASE-2K-VALID", "issue_sent")
	if issue.NextAction != valid {
		t.Fatalf("ledger did not preserve exact 2 KiB next: bytes=%d", len([]byte(issue.NextAction)))
	}

	invalidChild := caseCreateArgs("CASE-2K-INVALID", "超长 next", source, "CASE-2K-ROOT")
	runTestCommand(t, e, invalidChild...)
	app := e.app(t)
	err = app.run([]string{"issue", "--case", "CASE-2K-INVALID", "--to", "eng-data-engineer", "--next", valid + "x"})
	if err == nil || !strings.Contains(err.Error(), "2 KiB") || len(e.transport.calls) != 1 {
		t.Fatalf("2049-byte next did not fail before transport: err=%v calls=%d", err, len(e.transport.calls))
	}
}

func TestReportConditionalEvidenceRejectsAllMissingFieldsAtOnce(t *testing.T) {
	e := setupTestEnv(t)
	e.setActor(t, "zantianyou", "report:diagnostics", filepath.Join(e.root, "engineering"))
	app := e.app(t)
	for _, test := range []struct {
		result   string
		missing  []string
		template string
	}{
		{result: "blocked", missing: []string{"--source", "--note"}, template: "--result blocked --source PATH --note TEXT --next TEXT"},
		{result: "needs-decision", missing: []string{"--source", "--note"}, template: "--result needs-decision --source PATH --note TEXT --next TEXT"},
		{result: "returned", missing: []string{"--source", "--note"}, template: "--result returned --source PATH --note TEXT --next TEXT"},
		{result: "finding", missing: []string{"--severity", "--source", "--location", "--verify"}, template: "--result finding --severity P1 --source PATH --location TEXT --verify TEXT --next TEXT"},
		{result: "completed", missing: []string{"--artifact", "--source"}, template: "--result completed --artifact PATH --verify TEXT --next TEXT"},
	} {
		t.Run(test.result, func(t *testing.T) {
			err := app.run([]string{"report", "--case", "REPORT-DIAGNOSTICS", "--result", test.result, "--next", "continue"})
			if err == nil || exitCodeForError(err) != exitUsage {
				t.Fatalf("conditional evidence err=%v code=%d", err, exitCodeForError(err))
			}
			for _, want := range append(test.missing, test.template) {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("result=%s missing %q in %v", test.result, want, err)
				}
			}
		})
	}
}

func TestMessageStableIDRefsTwoKiBAndHQHeader(t *testing.T) {
	e := setupTestEnv(t)
	source := writeTestFile(t, filepath.Join(e.root, "engineering", "message-ref.md"), "# ref\n")
	e.setActor(t, "zantianyou", "message:manager", filepath.Join(e.root, "engineering"))
	runTestCommand(t, e, caseCreateArgs("CASE-MESSAGE", "沟通事项", source)...)

	sender := deliveryPolicyTestApp(t, e, "eng-developer", "message:sender")
	if _, err := runDeliveryPolicyTest(sender, "message", "--to", "zantianyou", "--kind", "question", "--case", "CASE-MESSAGE", "--text", strings.Repeat("x", 2048), "--ref-file", source, "--ref-case", "CASE-MESSAGE", "--delivery", "wakeup"); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 || !strings.HasPrefix(e.transport.calls[0].message, "[HQ message]") {
		t.Fatalf("missing HQ message header: %+v", e.transport.calls)
	}
	messages := deliveryPolicyMessages(t, e)
	first := messages[len(messages)-1]
	if first.MessageID == "" || len(first.RefFiles) != 1 || len(first.RefCases) != 1 {
		t.Fatalf("missing stable id/refs: %+v", first)
	}
	if ack := "hq message ack --message " + first.MessageID; !strings.Contains(e.transport.calls[0].message, ack) ||
		!strings.Contains(e.transport.calls[0].message, "未 ack 会阻止双方") {
		t.Fatalf("action message envelope lacks executable durable-ack correction %q: %s", ack, e.transport.calls[0].message)
	}

	tooLong := deliveryPolicyTestApp(t, e, "eng-developer", "message:sender")
	if _, err := runDeliveryPolicyTest(tooLong, "message", "--to", "zantianyou", "--kind", "info", "--text", strings.Repeat("x", 2049), "--delivery", "wakeup"); err == nil || !strings.Contains(err.Error(), "2 KiB") || !strings.Contains(err.Error(), "--ref-file") {
		t.Fatalf("bad 2 KiB rejection: %v", err)
	}

	replier := deliveryPolicyTestApp(t, e, "zantianyou", "message:reply")
	if _, err := runDeliveryPolicyTest(replier, "message", "--to", "eng-developer", "--kind", "info", "--text", "已对齐", "--reply-to", first.MessageID, "--ref-message", first.MessageID, "--ref-event", first.ID, "--delivery", "inject"); err != nil {
		t.Fatal(err)
	}
	messages = deliveryPolicyMessages(t, e)
	second := messages[len(messages)-1]
	if second.ReplyTo != first.MessageID || second.ThreadID != first.ThreadID || second.MessageID == first.MessageID {
		t.Fatalf("reply contract mismatch: first=%+v second=%+v", first, second)
	}
	if envelope, err := formatMessageEnvelope(Event{Type: "message_prepared", MessageID: "MSG-INFO", MessageKind: "info", ThreadID: "MSG-THREAD", DeliveryID: "DLV-INFO", Message: "FYI"}, "ledger-ref"); err != nil || strings.Contains(envelope, "message ack") {
		t.Fatalf("non-action info must not demand ack: envelope=%q err=%v", envelope, err)
	}

	acker := deliveryPolicyTestApp(t, e, "zantianyou", "message:ack")
	if _, err := runDeliveryPolicyTest(acker, "message", "ack", "--message", first.MessageID); err != nil {
		t.Fatal(err)
	}
}

func TestNewStaffPeerMessageQueuesUntilFirstManagerCase(t *testing.T) {
	e := setupTestEnv(t)
	cfg := testConfig()
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "eng-developer" {
			cfg.Agents[i].ApprovalRef = "ceo-office/decisions/staff-add.md"
		}
	}
	raw, _ := yaml.Marshal(cfg)
	if err := os.WriteFile(e.config, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	sender := deliveryPolicyTestApp(t, e, "eng-data-engineer", "quiet-merge:sender")
	out, err := runDeliveryPolicyTest(sender, "message", "--to", "eng-developer", "--kind", "question", "--text", "先看这份资料", "--delivery", "wakeup")
	if err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 0 || !strings.Contains(out, "首个 durable case") {
		t.Fatalf("new staff message was not clearly queued: calls=%d out=%q", len(e.transport.calls), out)
	}

	source := writeTestFile(t, filepath.Join(e.root, "engineering", "onboard.md"), "# onboard\n")
	manager := deliveryPolicyTestApp(t, e, "zantianyou", "quiet-merge:manager")
	manager.Out, manager.Err = io.Discard, io.Discard
	if err := manager.run(caseCreateArgs("CASE-ONBOARD-PARENT", "入职父事项", source)); err != nil {
		t.Fatal(err)
	}
	child := caseCreateArgs("CASE-ONBOARD-CHILD", "首个子事项", source, "CASE-ONBOARD-PARENT")
	if err := manager.run(child); err != nil {
		t.Fatal(err)
	}
	if err := manager.run([]string{"issue", "--case", "CASE-ONBOARD-CHILD", "--to", "eng-developer", "--next", "建立岗位上下文"}); err != nil {
		t.Fatal(err)
	}
	if len(e.transport.calls) != 1 {
		t.Fatalf("first case should produce exactly one wakeup prompt, calls=%d", len(e.transport.calls))
	}
	prompt := e.transport.calls[0].message
	if !strings.Contains(prompt, "[HQ notification]") || !strings.Contains(prompt, "[HQ message]") || !strings.Contains(prompt, "先看这份资料") {
		t.Fatalf("queued peer message was not merged into first case prompt: %q", prompt)
	}
	if !strings.Contains(prompt, "\n\n[HQ message]") {
		t.Fatalf("merged message lacks blank-line separator: %q", prompt)
	}

	consumer := deliveryPolicyTestApp(t, e, "eng-developer", "quiet-merge:target")
	var consumed bytes.Buffer
	consumer.Out, consumer.Err = &consumed, io.Discard
	if err := consumer.run([]string{"delivery", "consume"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(consumed.String(), "无静默消息") {
		t.Fatalf("automatically bundled message remained for manual consume: %q", consumed.String())
	}
}

func TestPublicCLIHasNoRemovedCommandsOrReferenceAlias(t *testing.T) {
	root := newCobraRootCommand(globalOptions{}, io.Discard, io.Discard)
	for _, cmd := range root.Commands() {
		if cmd.Name() == "task" {
			t.Fatal("public task command still exists")
		}
		for _, alias := range cmd.Aliases {
			if alias == "msg" {
				t.Fatal("public msg compatibility alias still exists")
			}
		}
	}
	for _, removed := range []string{"task", "msg", "ack"} {
		if shouldUseGateway([]string{removed}) {
			t.Fatalf("removed command %q still routed by gateway", removed)
		}
	}
	message, _, err := root.Find([]string{"message"})
	if err != nil {
		t.Fatal(err)
	}
	if message.Flags().Lookup("ref") != nil {
		t.Fatal("generic --ref alias still exists")
	}
	for _, name := range []string{"ref-file", "ref-case", "ref-message", "ref-event"} {
		if message.Flags().Lookup(name) == nil {
			t.Fatalf("missing --%s", name)
		}
	}
}

func TestAgentArgsExplicitlyOverrideKindDefaults(t *testing.T) {
	rule := AgentRule{Kind: "codex", AgentArgs: []string{"--sandbox", "danger-full-access", "--ask-for-approval", "never"}}
	got := nativeAgentArgs(rule)
	if strings.Join(got, " ") != strings.Join(rule.AgentArgs, " ") {
		t.Fatalf("agent_args not propagated: %v", got)
	}
	got[0] = "changed"
	if rule.AgentArgs[0] == "changed" {
		t.Fatal("native argv aliases config storage")
	}
	merged := nativeAgentArgs(AgentRule{Kind: "codex", PermissionMode: "yolo", AgentArgs: []string{"-c", `model_reasoning_effort="medium"`}})
	for _, required := range []string{"-c", `model_reasoning_effort="medium"`, "--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust"} {
		if !slices.Contains(merged, required) {
			t.Fatalf("yolo args missing %q: %v", required, merged)
		}
	}
	wants := map[string][]string{
		"claude":   {"--dangerously-skip-permissions"},
		"codex":    {"--dangerously-bypass-approvals-and-sandbox", "--dangerously-bypass-hook-trust"},
		"copilot":  {"--yolo"},
		"cursor":   {"--force"},
		"gemini":   {"--approval-mode=yolo"},
		"grok":     {"--always-approve"},
		"kimi":     {"--auto"},
		"opencode": {"--auto"},
		"qwen":     {"--approval-mode=yolo"},
	}
	for kind, want := range wants {
		got := nativeAgentArgs(AgentRule{Kind: kind, PermissionMode: "yolo"})
		if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Fatalf("%s default=%v want=%v", kind, got, want)
		}
		if len(got) != 0 {
			got[0] = "changed"
			if defaultAgentArgsByKind[kind][0] == "changed" {
				t.Fatalf("%s default argv aliases preset storage", kind)
			}
		}
	}
	if got := nativeAgentArgs(AgentRule{Kind: "codex", PermissionMode: "native"}); got != nil {
		t.Fatalf("native permission mode appended automatic approval args: %v", got)
	}
	if got := nativeAgentArgs(AgentRule{Kind: "unknown", PermissionMode: "yolo"}); got != nil {
		t.Fatalf("unknown kind guessed unsafe args: %v", got)
	}
}

func TestCaseReviseUsesGateway(t *testing.T) {
	if !shouldUseGateway([]string{"case", "revise", "--id", "CASE-1"}) {
		t.Fatal("case revise must be routed through the authenticated HQ gateway")
	}
}

func TestRegistryMutationsUseGatewayAndExclusiveConfigAccess(t *testing.T) {
	mutations := [][]string{
		{"staff", "add"},
		{"staff", "update"},
		{"staff", "remove"},
		{"role", "add"},
		{"role", "retire"},
	}
	for _, args := range mutations {
		if !shouldUseGateway(args) {
			t.Errorf("registry mutation %v is not routed through the authenticated HQ gateway", args)
		}
		if !isRegistryConfigMutation(args) {
			t.Errorf("registry mutation %v does not take the exclusive config lock", args)
		}
		app := App{ConfigAccess: &sync.RWMutex{}}
		unlock := app.lockGatewayConfigAccess(args)
		if app.ConfigAccess.TryRLock() {
			app.ConfigAccess.RUnlock()
			unlock()
			t.Fatalf("registry mutation %v acquired only a shared config lock", args)
		}
		unlock()
	}
	readOnly := [][]string{
		{"staff", "list"},
		{"staff", "show"},
		{"role", "list"},
		{"role", "show"},
	}
	for _, args := range readOnly {
		if shouldUseGateway(args) {
			t.Errorf("read-only registry command %v unexpectedly uses the mutation gateway", args)
		}
		if isRegistryConfigMutation(args) {
			t.Errorf("read-only registry command %v unexpectedly takes the exclusive config lock", args)
		}
		app := App{ConfigAccess: &sync.RWMutex{}}
		unlock := app.lockGatewayConfigAccess(args)
		if !app.ConfigAccess.TryRLock() {
			unlock()
			t.Fatalf("read-only registry command %v did not acquire a shared config lock", args)
		}
		app.ConfigAccess.RUnlock()
		unlock()
	}
}

func TestHQNudgeEnvelopeHeader(t *testing.T) {
	got := nudgeEnvelope(NudgeView{NudgeID: "NUDGE-1", ClaimID: "CLAIM-1", ExpiresAt: "2026-08-30T05:00:00+08:00", Message: "请处理"})
	if !strings.HasPrefix(got, "[HQ notification]") {
		t.Fatalf("nudge missing HQ notification header: %q", got)
	}
}
