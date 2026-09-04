package main

import (
	"os"
	"strings"
	"testing"
)

func TestManagerRoleManualPublishesDirectIssueAndAutomaticActivation(t *testing.T) {
	manager := AgentRule{
		Name:             "product-manager",
		Nickname:         "产品负责人",
		Department:       "product",
		DepartmentLabel:  "产品部",
		ReportsTo:        "penny",
		Responsibilities: []string{"manager:product"},
		RoleCardID:       "product-manager",
		RoleCardVersion:  1,
		WorkstationPath:  "product/staff/product-manager/v1",
		ActivationPolicy: activationAlways,
		MaxWIP:           8,
	}
	manual := string(agentRoleCardManual("Example", "example-hq", manager))
	for _, want := range []string{
		"直属 seat 直接 issue，不申请或附加 approval/decision",
		"**你 → 自己的精确直属 seat**",
		"**你 → 非直属 seat**",
		"hq issue --case <child-id> --to <direct-report-seat> --due <RFC3339> --next <下一步>",
		"durable issue intent/prepared 时立即预占目标 WIP",
		"`accept` 是确认接单，不是容量开始计数的时点",
		"HQ 会通过 Herdr 自动 cold-resume",
		"按报错中的纠正命令执行",
		"不得继续申请 approval",
		"case revise --id <quality-case-id> --version <N+1>",
		"不能要求旧 assignment 再次 report",
		"runtime status --agent <direct-report-seat>",
		"hibernate_attempting`/`hibernate_unknown",
		"hq up <on_assignment-seat>",
		"hq message ack --message <message-id>",
		"ack 只证明收到",
		"所有者的带外工具授权",
		"不创建或改变 assignment",
		"saved permission",
		"父项不会同时催你提前 report",
		"普通 message 或 runtime 事件不能伪造进展",
	} {
		if !strings.Contains(manual, want) {
			t.Fatalf("manager role manual missing %q:\n%s", want, manual)
		}
	}
	if strings.Contains(manual, "hq issue --case <child-id> --to <direct-report-seat> --approval") ||
		strings.Contains(manual, "hq issue --case <child-id> --to <direct-report-seat> --decision") {
		t.Fatalf("manager's standard direct-report issue command must not carry approval/decision:\n%s", manual)
	}
}

func TestCompanyAndRepositoryDocsPublishManagerAuthorizationMatrix(t *testing.T) {
	manager := AgentRule{Name: "product-manager", DepartmentLabel: "产品部", ReportsTo: "penny",
		RoleCardID: "product-manager", RoleCardVersion: 1, ActivationPolicy: activationAlways, MaxWIP: 8,
		ManualPath: "product/staff/product-manager/v1/AGENTS.md"}
	specialist := AgentRule{Name: "product-researcher", DepartmentLabel: "产品部", ReportsTo: "product-manager",
		RoleCardID: "product-researcher", RoleCardVersion: 1, ActivationPolicy: activationOnAssignment, MaxWIP: 1,
		ManualPath: "product/staff/product-researcher/v1/AGENTS.md"}
	handbook := string(companyAgentHandbook(initPlan{CompanyName: "Example", Owner: "ZC", Workspace: "example-hq",
		Config: Config{Agents: []AgentRule{manager, specialist}}}))
	for _, want := range []string{
		"部门经理对已批准、具备容量的直属 employee seat 拥有日常排班权",
		"部门经理 → 自己的精确直属 seat",
		"不申请、不传 `--approval`/`--decision`",
		"部门经理 → 非直属 seat",
		"自动通过 Herdr cold-resume",
		"报错给出的纠正命令",
		"case revise --id <quality-case-id> --version <N+1>",
		"旧 finding submission 和旧 assignment 不变",
		"runtime status [--agent <direct-report-seat>]",
		"keep-warm 后自动休眠",
		"hibernate_attempting`/`hibernate_unknown",
		"hq message ack --message <message-id>",
		"普通 `info` 无需 ack",
		"明确授权代理",
		"不创建或改变 case/issue/assignment",
		"saved deny",
		"父 assignment 不重复催报",
		"child submitted 后进入经理 review",
	} {
		if !strings.Contains(handbook, want) {
			t.Fatalf("company handbook missing %q:\n%s", want, handbook)
		}
	}

	for _, path := range []string{repositoryPath("README.md"), repositoryPath("docs", "DESIGN.md")} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(raw)
		for _, want := range []string{
			"部门经理 → 自己的精确直属 seat",
			"部门经理 → 非直属 seat",
			"approval/decision 不能扩大",
			"cold-resume",
			"纠正命令",
			"明确授权代理",
			"saved deny",
			"父 assignment",
			"重新开始 stall 计时",
		} {
			if !strings.Contains(doc, want) {
				t.Fatalf("%s missing manager delegation contract %q", path, want)
			}
		}
	}
}
