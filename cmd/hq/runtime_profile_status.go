package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Observed is terminal evidence, not a copy of desired config. This is a
// read-only point-in-time view, not a second source of runtime truth.
type employeeProfileStatus struct {
	State         string                  `json:"state"`
	Reason        string                  `json:"reason"`
	Observed      *EmployeeRuntimeProfile `json:"observed,omitempty"`
	TabID         string                  `json:"tab_id,omitempty"`
	RuntimeStatus string                  `json:"runtime_status,omitempty"`
	CheckedAt     string                  `json:"checked_at"`
}

func (a *App) employeeRuntimeProfileStatus(ctx context.Context, rule AgentRule) *employeeProfileStatus {
	view := &employeeProfileStatus{State: "unverified", CheckedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if rule.Disabled {
		view.State, view.Reason = "disabled", "员工已停用，不启动 runtime"
		return view
	}
	expected, ok := runtimeProfileForEmployee(a.Config, rule.Kind, rule.Name)
	if !ok {
		view.State, view.Reason = "unconfigured", "未声明可核验的 runtime profile"
		return view
	}
	if a.Herdr == nil {
		view.Reason = "Herdr 不可用；运行 hq patrol --json 核验"
		return view
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		view.Reason = err.Error()
		return view
	}
	live := false
	for _, agent := range snapshot.Agents {
		if agent.Name == rule.Name {
			live = true
		}
	}
	if !live {
		if a.Sessions != nil {
			events, err := a.Sessions.List(SessionFilter{Agent: rule.Name})
			if err != nil {
				view.Reason = err.Error()
				return view
			}
			_, unresolved := unresolvedProfileRepairSession(events)
			if unresolved || profileRepairPending(events) {
				view.State, view.Reason = "repair_pending", "旧 runtime 的模型恢复尚未收敛；运行 hq patrol --json 核验"
				return view
			}
		}
		view.State, view.Reason = "next_activation", "无在线 runtime；下次正式激活使用新配置，不因编辑配置唤醒员工"
		return view
	}
	binding, err := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		view.Reason = err.Error()
		return view
	}
	view.TabID, view.RuntimeStatus = binding.TabID, binding.Status
	if binding.Kind != expected.Kind {
		view.State, view.Reason = "different_kind", fmt.Sprintf("当前运行 kind=%s，期望 profile 属于 %s；不自动撤销 fallback，运行 hq patrol --json 核验", binding.Kind, expected.Kind)
		return view
	}
	reader, ok := a.Herdr.(HerdrAgentReader)
	if !ok {
		view.Reason = "Herdr 缺少 terminal read，无法证明实际 model/effort"
		return view
	}
	_, observed, mismatch, err := inspectLiveRuntimeProfile(ctx, reader, a.Config, binding)
	if err != nil {
		view.Reason = err.Error()
		if errors.Is(err, errCodexSafetyBufferingVisible) {
			view.State, view.Reason = "waiting_ui", "模型等待界面遮挡 footer；保持原模型，不发送按键，界面消失后重新核验"
		}
		return view
	}
	fresh, err := a.herdrSnapshot(ctx)
	if err != nil {
		view.Reason = err.Error()
		return view
	}
	current, err := ResolveLiveBinding(fresh, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil || liveBindingIncarnationMismatch(binding, current) != "" {
		view.Reason = "核验期间 runtime incarnation 已变化；请重试 hq staff get --name " + rule.Name + " --live --json"
		return view
	}
	view.RuntimeStatus = current.Status
	view.Observed = &EmployeeRuntimeProfile{Model: observed.Model, ReasoningEffort: observed.ReasoningEffort}
	if a.Sessions != nil {
		events, err := a.Sessions.List(SessionFilter{Agent: rule.Name})
		if err != nil {
			view.Reason = err.Error()
			return view
		}
		if started, err := activeSessionForBinding(events, current); err == nil {
			diagnostic := latestSessionDiagnostic(events, started.SessionID)
			switch diagnostic.Type {
			case sessionProfileRepairAttempting, sessionProfileRepairUnknown:
				view.State, view.Reason = "repair_unknown", "关闭结果尚未确认；禁止自动重启，运行 hq patrol --json 后按单 seat reconcile 指引处理"
				return view
			case sessionProfileRepairFailed:
				// A definitely-not-run failure is historical if the owner has
				// since restored the matching profile or selected report-only.
				if mismatch != "" && expected.OnDrift == runtimeProfileDriftRestartIdle {
					view.State, view.Reason = "repair_failed", "上次修复未执行成功；gateway 将安全重试，运行 hq patrol --json 核验"
					return view
				}
			}
			if mismatch == "" && started.Actor == "hq-runtime-profile" && diagnostic.Type != sessionProfileRecoverySent {
				view.State, view.Reason = "recovery_pending", "模型已匹配，但任务恢复信封尚未确认送达；运行 hq patrol --json 核验"
				return view
			}
		}
	}
	if mismatch == "" {
		view.State, view.Reason = "applied", "终端实际 model/effort 与期望匹配"
		return view
	}
	if expected.OnDrift == runtimeProfileDriftReport {
		view.State, view.Reason = "report_only", "on_drift=report，仅报告；需要自动切换时配置 restart_idle"
	} else if current.Status == "idle" || current.Status == "done" {
		view.State, view.Reason = "pending", "等待 gateway 安全替换 runtime；保存成功不等于已切换"
	} else {
		view.State, view.Reason = "waiting_idle", "等待 idle|done 安全边界；不打断 working|blocked 回合"
	}
	return view
}
