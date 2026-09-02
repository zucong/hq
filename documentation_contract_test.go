package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

func TestDocumentationContractREADMEDiscoversCurrentIsolatedRoot(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	if strings.Contains(readme, "/Users/zack/workspaces/") || strings.Contains(readme, "_harness/hq") {
		t.Fatal("README must describe the independent repository without a machine or former monorepo path")
	}
	for _, want := range []string{
		`HQ_SOURCE_DIR="$(git rev-parse --show-toplevel 2>/dev/null || pwd -P)"`,
		`test -f "$HQ_SOURCE_DIR/README.md" && test -f "$HQ_SOURCE_DIR/go.mod"`,
		"源码仓库和公司实例目录是两个概念",
		"需要验证完整 `up` 编排时",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing documentation contract %q", want)
		}
	}
	if count := strings.Count(readme, `HQ_SOURCE_DIR="$(git rev-parse --show-toplevel 2>/dev/null || pwd -P)"`); count < 2 {
		t.Fatalf("first-run and official fixture blocks must each discover their own root; count=%d", count)
	}
}

func TestDocumentationContractREADMEPublishesCurrentRegistryAndBootstrap(t *testing.T) {
	raw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(raw)
	for _, want := range []string{
		"## 首次连接 Herdr",
		"can_manage_staff=true",
		"总部联系职责位 → 回收 Herdr 自动创建的空 root tab → gateway",
		"manager:<department>",
		"runtime `sender_label`",
		"role_cards:",
		"role_card_id: secretary",
		"workstation_path: ceo-office/staff/secretary/v1",
		"activation_policy: always",
		"max_wip: 16",
		"./bin/hq role add",
		"./bin/hq staff add",
		`--office "$HQ_COMPANY_ROOT/ceo-office" up`,
		"不会自动创建 case 或 message",
	} {
		if !strings.Contains(readme, want) {
			t.Fatalf("README missing documentation contract %q", want)
		}
	}
	if strings.Contains(readme, "auto_start") {
		t.Fatal("README must use activation_policy as the only startup policy")
	}
	marker := "```yaml\nversion: 3\nworkspace_label: example-hq\n"
	start := strings.Index(readme, marker)
	if start < 0 {
		t.Fatal("README missing documentation registry YAML block")
	}
	start += len("```yaml\n")
	end := strings.Index(readme[start:], "\n```")
	if end < 0 {
		t.Fatal("README documentation registry YAML block is not closed")
	}
	if _, err := decodeCurrentConfig([]byte(readme[start : start+end])); err != nil {
		t.Fatalf("README documentation registry is not accepted by the strict runtime schema: %v", err)
	}
}

func TestDocumentationContractPublishesOnlyCurrentFormalSchemas(t *testing.T) {
	files := []string{
		"README.md",
		"DESIGN.md",
		"RELEASE.md",
	}
	forbidden := []string{
		"v1 → v2",
		"v1→v2",
		"v1 现状",
		"v2 实施",
		"v2 发布",
		"v2.2.1",
		"内部研究与开发里程碑",
		"恢复/迁移步骤",
		"旧看板",
		"mixed-version",
		"event_version=2",
		"严格 YAML v2",
		"严格 v2 注册表",
		"legacy projection",
		"legacy fence",
		"迁移旧公司",
		"auto_start",
		"ROSTER.md",
	}
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, phrase := range forbidden {
			if strings.Contains(text, phrase) {
				t.Fatalf("%s still publishes removed compatibility contract %q", path, phrase)
			}
		}
		for _, want := range []string{"registry v3", "event v3"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing current schema %q", path, want)
			}
		}
	}
}

func TestDocumentationContractNamesFormalReleaseArtifact(t *testing.T) {
	raw, err := os.ReadFile("RELEASE.md")
	if err != nil {
		t.Fatal(err)
	}
	release := string(raw)
	for _, want := range []string{"v1.0.0", "/tmp/hq-v1.0.0-release", "首个正式制品"} {
		if !strings.Contains(release, want) {
			t.Fatalf("RELEASE.md missing first-release contract %q", want)
		}
	}
}

func TestDocumentationContractUpHelpPointsToRealBootstrapChecklistMatchesDeploymentContract(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := execute([]string{"up", "--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("up help wrote stderr: %s", errOut.String())
	}
	help := out.String()
	for _, want := range []string{
		"README“首次连接 Herdr”",
		"hq init 自动完成",
		"runtime sender_label",
		"--config、--data、--herdr",
		"exit 70",
		"fake Herdr",
		"canonical 校验",
		"不通过运行参数替换正式依赖",
		"运行连续性检查",
	} {
		if !strings.Contains(help, want) {
			t.Fatalf("up help missing %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "恢复/迁移步骤") {
		t.Fatalf("up help still publishes a pre-release migration narrative:\n%s", help)
	}
}

func TestDocumentationContractBoardHelpDescribesCurrentRebuild(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := execute([]string{"board", "--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("board help wrote stderr: %s", errOut.String())
	}
	help := out.String()
	if !strings.Contains(help, "从权威事件重新生成 SQLite 看板索引及 HQ 派生状态") {
		t.Fatalf("board help missing current rebuild contract:\n%s", help)
	}
	if strings.Contains(help, "旧看板") {
		t.Fatalf("board help still labels the only board as old:\n%s", help)
	}
}

func TestDocumentationContractApprovalHelpDoesNotAdvertiseRemovedReusableMode(t *testing.T) {
	var out, errOut bytes.Buffer
	if err := execute([]string{"approval", "request", "--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("approval request help wrote stderr: %s", errOut.String())
	}
	help := out.String()
	if !strings.Contains(help, "仅 one_time") || strings.Contains(help, "one_time|reusable") {
		t.Fatalf("approval request help advertises a removed approval mode:\n%s", help)
	}
}

func TestDocumentationContractPublishesDurableManagerEscalation(t *testing.T) {
	for _, path := range []string{"README.md", "DESIGN.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range []string{"case escalate", "accepted", "reports_to", "case_escalation_prepared", "case revise --version <N+1>", "fresh issue", "旧 assignment"} {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing manager escalation contract %q", path, want)
			}
		}
	}
	var out, errOut bytes.Buffer
	if err := execute([]string{"case", "escalate", "--help"}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	if errOut.Len() != 0 {
		t.Fatalf("case escalate help wrote stderr: %s", errOut.String())
	}
	help := out.String()
	for _, want := range []string{"--parent", "--reason", "--next", "直属上级"} {
		if !strings.Contains(help, want) {
			t.Fatalf("case escalate help missing %q:\n%s", want, help)
		}
	}
	if strings.Contains(help, "--to") || strings.Contains(help, "--owner") {
		t.Fatalf("case escalate help exposed arbitrary routing:\n%s", help)
	}
}

func TestDocumentationContractPublishesBoundedRuntimeHibernationAndRecovery(t *testing.T) {
	contracts := map[string][]string{
		"README.md": {
			"keep_warm", "runtime status", "runtime reap --agent <slug> --retry-unknown",
			"orphan_tab_without_agent", "conditional close", "always` 永不自动关闭",
			"hq message ack --message <message-id>", "未 ack 会同时阻止发送方和接收方",
		},
		"DESIGN.md": {
			"Runtime hibernation 与 cold-resume", "hibernate_attempting|hibernate_unknown",
			"零 origin、零 WIP", "event_accepted|event_returned", "conditional close/CAS",
			"hq message ack --message <message-id>", "Ack 只证明收到",
		},
		"RELEASE.md": {
			"有界 `keep_warm`", "runtime status", "--retry-unknown", "同 seat cold-resume", "always` 席位不被自动关闭",
		},
	}
	for path, wants := range contracts {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		for _, want := range wants {
			if !strings.Contains(text, want) {
				t.Fatalf("%s missing runtime hibernation contract %q", path, want)
			}
		}
	}
}
