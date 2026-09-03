package main

import (
	"os"
	"strings"
	"testing"
)

func TestSecretaryRoleIsNamedByResponsibilityAndChannelsHumanDecisions(t *testing.T) {
	secretary := AgentRule{
		Name:             "owner-liaison-alpha",
		Nickname:         "总部联络官",
		Department:       "ceo-office",
		DepartmentLabel:  "总裁办",
		Responsibilities: []string{roleApprovalWitness, roleAccountCloser, "executive_secretary"},
		RoleCardID:       "owner-liaison",
		RoleCardVersion:  1,
		WorkstationPath:  "ceo-office/staff/owner-liaison/v1",
		ActivationPolicy: activationAlways,
		MaxWIP:           16,
	}
	manual := string(agentRoleCardManual("Example", "example-hq", secretary))
	for _, want := range []string{
		"人类公司所有者与虚拟公司总部之间的双向沟通管道",
		"上传并见证人类所有者已经明确作出的正式决定",
		"依据人类授权向部门经理下达公司级事项",
		"汇总各部门已验收证据、风险与待决问题并反馈给人类",
		"总裁秘书与人类所有者沟通协议（强制）",
		"具体 agent 名字、花名和 sender label 均可配置",
		"**禁止代决**",
		"停止公司级推进并向人类回问",
		"**批准快照**",
		"approval 只能是 `one_time`",
		"新 `approval_id`",
		"不得 message 原员工要求旧 assignment 再次 report",
		"**后序销账**",
		"HQ销账守卫",
		"提醒不是关闭批准",
	} {
		if !strings.Contains(manual, want) {
			t.Fatalf("secretary role manual missing %q:\n%s", want, manual)
		}
	}
	if strings.Contains(strings.ToLower(manual), "penny") {
		t.Fatalf("secretary role manual must not depend on a fixture name:\n%s", manual)
	}
}

func TestGenericHandbookAndDocsDoNotMakeSecretaryAProperName(t *testing.T) {
	secretary := AgentRule{Name: "owner-liaison-alpha", Nickname: "总部联络官", DepartmentLabel: "总裁办",
		Responsibilities: []string{roleApprovalWitness}, RoleCardID: "owner-liaison", RoleCardVersion: 1,
		ActivationPolicy: activationAlways, MaxWIP: 16, ManualPath: "ceo-office/staff/owner-liaison/v1/AGENTS.md"}
	handbook := string(companyAgentHandbook(initPlan{CompanyName: "Example", Owner: "HUMAN-OWNER", Workspace: "example-hq",
		Config: Config{Agents: []AgentRule{secretary}}}))
	for _, want := range []string{
		"唯一 `approval_witness` 职责位",
		"具体 agent 名字和花名可配置",
		"上传人类已经明确作出的决定",
		"向部门经理下达公司级事项",
		"已验收证据、风险和待决问题汇总给人类",
		"不得代替人类决定",
		"销账守卫",
		"不会自动 report、accept/return/close",
	} {
		if !strings.Contains(handbook, want) {
			t.Fatalf("company handbook missing secretary contract %q:\n%s", want, handbook)
		}
	}
	if strings.Contains(strings.ToLower(handbook), "penny") {
		t.Fatalf("company handbook must not depend on a fixture name:\n%s", handbook)
	}

	for _, path := range []string{"README.md", "DESIGN.md"} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		doc := string(raw)
		for _, want := range []string{"approval_witness", "双向沟通管道", "下达公司级事项", "汇总", "不得代替人类"} {
			if !strings.Contains(doc, want) {
				t.Fatalf("%s missing generic secretary contract %q", path, want)
			}
		}
		if strings.Contains(strings.ToLower(doc), "penny") {
			t.Fatalf("%s must not make a fixture name part of the generic product contract", path)
		}
	}
	readmeRaw, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--secretary-name owner-channel", `--secretary-nickname "总部联络官"`, "两者都不承载权限"} {
		if !strings.Contains(string(readmeRaw), want) {
			t.Fatalf("README missing configurable secretary bootstrap contract %q", want)
		}
	}
	for _, want := range []string{"business generation", "seat version/digest", "ABA", "one_time"} {
		if !strings.Contains(string(readmeRaw), want) {
			t.Fatalf("README missing approval snapshot contract %q", want)
		}
	}
}
