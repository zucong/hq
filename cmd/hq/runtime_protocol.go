package main

import "fmt"

const agentRuntimeProtocolVersion = 1

const managerEventDrivenWaitRule = "经理把工作正式 issue 给直属下属后，若 HQ 返回 actor_directive.action=end_turn，表示当前没有其他 durable 经理待办：立即结束本回合，禁止用 sleep、进程查询、Herdr 状态读取或循环查看产物来轮询下属；HQ 会在 submission、blocked/needs-decision、投递异常或执行升级时重新唤醒。"

func runtimeProtocolLine() string {
	return fmt.Sprintf("[HQ runtime protocol] version=%d。运行协议由当前公司 HQ binary 注入；个人 AGENTS.md 继续定义稳定角色、职责、人格和业务边界。", agentRuntimeProtocolVersion)
}

type ActorDirective struct {
	ProtocolVersion int      `json:"protocol_version"`
	Action          string   `json:"action"`
	Reason          string   `json:"reason"`
	CaseID          string   `json:"case_id,omitempty"`
	AssignmentID    string   `json:"assignment_id,omitempty"`
	WakeOn          []string `json:"wake_on,omitempty"`
	Prohibited      []string `json:"prohibited,omitempty"`
	NextAction      string   `json:"next_action,omitempty"`
}

func (d ActorDirective) text() string {
	switch d.Action {
	case "end_turn":
		return fmt.Sprintf("actor_directive=end_turn protocol=%d：%s；现在结束本回合，不要 sleep/轮询；HQ 将在下属 report、blocked/needs-decision、投递异常或执行升级时重新唤醒", d.ProtocolVersion, d.Reason)
	case "continue_queue":
		return fmt.Sprintf("actor_directive=continue_queue protocol=%d：%s；下一步：%s", d.ProtocolVersion, d.Reason, d.NextAction)
	case "inspect_queue":
		return fmt.Sprintf("actor_directive=inspect_queue protocol=%d：%s；下一步：%s；不要重试已经提交的 issue，也不要轮询下属", d.ProtocolVersion, d.Reason, d.NextAction)
	default:
		return fmt.Sprintf("actor_directive=%s protocol=%d：%s", d.Action, d.ProtocolVersion, d.Reason)
	}
}

func managerIssueInspectionDirective(caseID, assignmentID string, cause error) ActorDirective {
	return ActorDirective{
		ProtocolVersion: agentRuntimeProtocolVersion,
		Action:          "inspect_queue",
		Reason:          "issue 已 durable 提交，但后置经理队列投影暂时不可用：" + cause.Error(),
		CaseID:          caseID,
		AssignmentID:    assignmentID,
		Prohibited:      []string{"repeat_issue", "sleep_polling", "process_polling", "herdr_status_polling", "artifact_polling"},
		NextAction:      "运行 hq assignment list 与 hq inbox；只处理其中明确列出的 durable 动作，否则结束本回合等待 HQ 唤醒",
	}
}
