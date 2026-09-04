package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	runtimeProfileDriftReport      = "report"
	runtimeProfileDriftRestartIdle = "restart_idle"
	profileRepairPendingReason     = "runtime_profile repair_pending"
)

type runtimeProfile struct {
	Kind            string
	Model           string
	ReasoningEffort string
	OnDrift         string
}

func (p runtimeProfile) String() string {
	return fmt.Sprintf("kind=%s model=%s reasoning_effort=%s", p.Kind, p.Model, p.ReasoningEffort)
}

func validReasoningEffort(value string) bool {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return true
	default:
		return false
	}
}

func validateRuntimeProfiles(cfg Config) error {
	for kind, policy := range cfg.RuntimeProfiles {
		prefix := "runtime_profiles." + kind
		if !agentNamePattern.MatchString(kind) {
			return fmt.Errorf("runtime_profiles 的 kind %q 必须是小写 ASCII slug", kind)
		}
		if kind != "codex" {
			return fmt.Errorf("%s 尚无可核验的 runtime profile adapter；请移除该项，或升级 HQ 后再声明", prefix)
		}
		if strings.TrimSpace(policy.Model) != policy.Model || policy.Model == "" ||
			strings.ContainsAny(policy.Model, "\r\n\x00 \t") || !utf8.ValidString(policy.Model) || utf8.RuneCountInString(policy.Model) > 100 {
			return fmt.Errorf("%s.model 必须是非空、无空白、至多 100 rune 的原生模型 ID", prefix)
		}
		if !validReasoningEffort(policy.ReasoningEffort) {
			return fmt.Errorf("%s.reasoning_effort 必须是 none|minimal|low|medium|high|xhigh", prefix)
		}
		if policy.OnDrift != runtimeProfileDriftReport && policy.OnDrift != runtimeProfileDriftRestartIdle {
			return fmt.Errorf("%s.on_drift 必须是 report|restart_idle；需要 HQ 在安全 idle|done 边界自动恢复时使用 restart_idle", prefix)
		}
		model, effort, err := codexRuntimeProfileOverridesForConfig(cfg, kind)
		if err != nil {
			return err
		}
		if model != "" && model != policy.Model {
			return fmt.Errorf("%s.model=%s 与 agent_args 中显式 model=%s 冲突；删除 agent_args 的 model 参数，或改成同一值", prefix, policy.Model, model)
		}
		if effort != "" && effort != policy.ReasoningEffort {
			return fmt.Errorf("%s.reasoning_effort=%s 与 agent_args 中显式 model_reasoning_effort=%s 冲突；删除冲突参数，或改成同一值", prefix, policy.ReasoningEffort, effort)
		}
	}
	return nil
}

// codexRuntimeProfileOverridesForConfig requires every seat of one native kind
// to agree on duplicate native overrides. This prevents a company-level desired
// profile from silently fighting seat-local argv.
func codexRuntimeProfileOverridesForConfig(cfg Config, kind string) (string, string, error) {
	var model, effort string
	for _, rule := range cfg.Agents {
		if rule.Disabled || rule.Kind != kind {
			continue
		}
		currentModel, currentEffort, err := codexRuntimeProfileOverrides(rule.AgentArgs)
		if err != nil {
			return "", "", fmt.Errorf("agent %s 的 agent_args：%w", rule.Name, err)
		}
		if currentModel != "" {
			if model != "" && model != currentModel {
				return "", "", fmt.Errorf("kind=%s 的 agent_args 含不一致 model：%s 与 %s；公司级 runtime profile 要求同一 kind 使用一致载体", kind, model, currentModel)
			}
			model = currentModel
		}
		if currentEffort != "" {
			if effort != "" && effort != currentEffort {
				return "", "", fmt.Errorf("kind=%s 的 agent_args 含不一致 model_reasoning_effort：%s 与 %s；公司级 runtime profile 要求同一 kind 使用一致 effort", kind, effort, currentEffort)
			}
			effort = currentEffort
		}
	}
	return model, effort, nil
}

func trimTOMLString(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && ((value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'')) {
		return value[1 : len(value)-1]
	}
	return value
}

func mergeRuntimeOverride(label, current, value string) (string, error) {
	value = trimTOMLString(value)
	if value == "" {
		return "", fmt.Errorf("%s 覆盖值不能为空", label)
	}
	if current != "" && current != value {
		return "", fmt.Errorf("同一 agent_args 重复声明冲突的 %s：%s 与 %s", label, current, value)
	}
	return value, nil
}

func codexRuntimeProfileOverrides(args []string) (string, string, error) {
	var model, effort string
	for index := 0; index < len(args); index++ {
		arg := args[index]
		switch {
		case arg == "--model" || arg == "-m":
			if index+1 >= len(args) {
				return "", "", fmt.Errorf("%s 缺少值；正确写法是 %s MODEL", arg, arg)
			}
			index++
			var err error
			model, err = mergeRuntimeOverride("model", model, args[index])
			if err != nil {
				return "", "", err
			}
		case strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "-m="):
			var err error
			model, err = mergeRuntimeOverride("model", model, strings.SplitN(arg, "=", 2)[1])
			if err != nil {
				return "", "", err
			}
		case arg == "-c" || arg == "--config":
			if index+1 >= len(args) {
				return "", "", fmt.Errorf("%s 缺少 key=value；runtime profile 示例：%s model_reasoning_effort=\"medium\"", arg, arg)
			}
			index++
			var err error
			model, effort, err = mergeCodexConfigOverride(model, effort, args[index])
			if err != nil {
				return "", "", err
			}
		case strings.HasPrefix(arg, "--config="):
			var err error
			model, effort, err = mergeCodexConfigOverride(model, effort, strings.TrimPrefix(arg, "--config="))
			if err != nil {
				return "", "", err
			}
		}
	}
	return model, effort, nil
}

func mergeCodexConfigOverride(model, effort, value string) (string, string, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return model, effort, nil
	}
	key := strings.TrimSpace(parts[0])
	var err error
	switch key {
	case "model":
		model, err = mergeRuntimeOverride("model", model, parts[1])
	case "model_reasoning_effort":
		effort, err = mergeRuntimeOverride("model_reasoning_effort", effort, parts[1])
	}
	return model, effort, err
}

func runtimeProfileForKind(cfg Config, kind string) (runtimeProfile, bool) {
	policy, ok := cfg.RuntimeProfiles[kind]
	if !ok {
		return runtimeProfile{}, false
	}
	return runtimeProfile{Kind: kind, Model: policy.Model, ReasoningEffort: policy.ReasoningEffort, OnDrift: policy.OnDrift}, true
}

func nativeAgentArgsForConfig(cfg Config, rule AgentRule) ([]string, error) {
	result := nativeAgentArgs(rule)
	profile, ok := runtimeProfileForKind(cfg, rule.Kind)
	if !ok {
		return result, nil
	}
	if rule.Kind != "codex" {
		return nil, fmt.Errorf("kind=%s 尚无 runtime profile argv adapter", rule.Kind)
	}
	model, effort, err := codexRuntimeProfileOverrides(result)
	if err != nil {
		return nil, fmt.Errorf("agent %s 的 agent_args：%w", rule.Name, err)
	}
	if model != "" && model != profile.Model {
		return nil, fmt.Errorf("agent %s 的 model=%s 与 runtime_profiles.%s.model=%s 冲突", rule.Name, model, rule.Kind, profile.Model)
	}
	if effort != "" && effort != profile.ReasoningEffort {
		return nil, fmt.Errorf("agent %s 的 model_reasoning_effort=%s 与 runtime_profiles.%s.reasoning_effort=%s 冲突", rule.Name, effort, rule.Kind, profile.ReasoningEffort)
	}
	if model == "" {
		result = append(result, "--model", profile.Model)
	}
	if effort == "" {
		result = append(result, "-c", fmt.Sprintf("model_reasoning_effort=%q", profile.ReasoningEffort))
	}
	return result, nil
}

func observedCodexRuntimeProfile(raw []byte) (runtimeProfile, error) {
	if terminalShowsCodexSafetyBuffering(raw) {
		return runtimeProfile{}, errCodexSafetyBufferingVisible
	}
	lines := strings.Split(strings.ReplaceAll(string(raw), "\r", ""), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		fields := strings.Fields(lines[index])
		if len(fields) < 3 || fields[2] != "·" || !validReasoningEffort(fields[1]) {
			continue
		}
		model := fields[0]
		if strings.ContainsAny(model, "[](){}=,;") || strings.Contains(model, " ") {
			continue
		}
		return runtimeProfile{Kind: "codex", Model: model, ReasoningEffort: fields[1]}, nil
	}
	return runtimeProfile{}, fmt.Errorf("未在 Herdr detection 终端中找到 Codex footer（期望形如 `MODEL medium · ...`）")
}

func runtimeProfileMismatch(expected, observed runtimeProfile) string {
	var fields []string
	if observed.Model != expected.Model {
		fields = append(fields, fmt.Sprintf("model=%s want=%s", observed.Model, expected.Model))
	}
	if observed.ReasoningEffort != expected.ReasoningEffort {
		fields = append(fields, fmt.Sprintf("reasoning_effort=%s want=%s", observed.ReasoningEffort, expected.ReasoningEffort))
	}
	return strings.Join(fields, ", ")
}

func inspectLiveRuntimeProfile(ctx context.Context, reader HerdrAgentReader, cfg Config, binding LiveBinding) (runtimeProfile, runtimeProfile, string, error) {
	expected, ok := runtimeProfileForKind(cfg, binding.Kind)
	if !ok {
		return runtimeProfile{}, runtimeProfile{}, "", nil
	}
	raw, err := reader.ReadAgent(ctx, binding.Seat)
	if err != nil {
		return expected, runtimeProfile{}, "", fmt.Errorf("读取 agent=%s runtime profile：%w", binding.Seat, err)
	}
	var observed runtimeProfile
	switch binding.Kind {
	case "codex":
		observed, err = observedCodexRuntimeProfile(raw)
	default:
		err = fmt.Errorf("kind=%s 尚无 runtime profile detection adapter", binding.Kind)
	}
	if err != nil {
		return expected, runtimeProfile{}, "", err
	}
	return expected, observed, runtimeProfileMismatch(expected, observed), nil
}

func runtimeProfileRecoveryEnvelope(rule AgentRule, expected, observed runtimeProfile, work runtimeRecoveryWork) string {
	lines := []string{
		fmt.Sprintf("[HQ runtime recovery] trigger=runtime_profile_drift seat=%s previous_model=%s previous_effort=%s current_model=%s current_effort=%s。你仍是同一员工；模型运行参数修复不改变角色、权限边界、汇报线或任务合同。", rule.Name, observed.Model, observed.ReasoningEffort, expected.Model, expected.ReasoningEffort),
		"旧聊天上下文不会被当作 durable 事实复制；只以当前 AGENTS.md、同一工位文件和 HQ durable ledger 为事实源。",
	}
	for _, assignment := range work.assignments {
		lines = append(lines, fmt.Sprintf("ACTIVE_ASSIGNMENT id=%s event=%s case=%s status=%s；先运行 `hq assignment show --id %s` 与 `hq history --case %s`，再从 durable 状态继续。",
			assignment.AssignmentID, assignment.AssignmentEventID, assignment.CaseID, assignment.Status, assignment.AssignmentID, assignment.CaseID))
	}
	for _, state := range work.cases {
		lines = append(lines, fmt.Sprintf("ACTIVE_OWNED_CASE id=%s version=%d status=%s title=%q；先运行 `hq case show --id %s` 与 `hq history --case %s`，再从 durable 状态继续。", state.ID, state.Version, state.Status, state.Title, state.ID, state.ID))
	}
	if work.omitted > 0 {
		lines = append(lines, fmt.Sprintf("另有 %d 项 actionable durable work 未在本信封展开；运行 `hq inbox` 与 `hq assignment list` 读取完整队列，按 durable 状态处理，不要猜测。", work.omitted))
	}
	if work.empty() {
		lines = append(lines, "当前没有需要恢复的 durable assignment/case；完整读取本工位 AGENTS.md 后结束本回合，等待 HQ 派工。")
	} else {
		lines = append(lines, "先完成上述核验，再继续未完成工作；不要创建替代 case 或重复 assignment。")
	}
	return strings.Join(lines, "\n")
}

func unresolvedProfileRepairSession(events []SessionEvent) (SessionEvent, bool) {
	active := activeSessionStarts(events)
	for index := len(active) - 1; index >= 0; index-- {
		started := active[index]
		diagnostic := latestSessionDiagnostic(events, started.SessionID)
		if diagnostic.Type == sessionProfileRepairAttempting || diagnostic.Type == sessionProfileRepairUnknown {
			return started, true
		}
	}
	return SessionEvent{}, false
}

func profileRepairPending(events []SessionEvent) bool {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type == sessionStarted {
			return false
		}
		if event.Type == sessionStopped {
			return strings.HasPrefix(event.Reason, profileRepairPendingReason)
		}
	}
	return false
}

func (a *App) markProfileRecoverySent(ctx context.Context, rule AgentRule) error {
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	binding, err := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		return err
	}
	events, err := a.Sessions.List(SessionFilter{Agent: rule.Name})
	if err != nil {
		return err
	}
	started, err := activeSessionForBinding(events, binding)
	if err != nil {
		return err
	}
	_, err = a.appendDerivedSession(started, sessionProfileRecoverySent, "hq-runtime-profile", "durable recovery envelope delivery confirmed")
	return err
}

func (a *App) startProfileRepairRuntime(ctx context.Context, workspaceID string, rule AgentRule, expected, observed runtimeProfile, work runtimeRecoveryWork) error {
	if err := a.startHQAgentAdmittedWithOptions(ctx, workspaceID, rule, runtimeStartOptions{
		Actor: "hq-runtime-profile", Reason: "runtime profile drift repaired at safe boundary",
		PromptSuffix: runtimeProfileRecoveryEnvelope(rule, expected, observed, work),
	}); err != nil {
		return err
	}
	return a.markProfileRecoverySent(ctx, rule)
}

func (a *App) ensureProfileRecovery(ctx context.Context, rule AgentRule, expected runtimeProfile, binding LiveBinding, events []SessionEvent, work runtimeRecoveryWork) error {
	started, err := activeSessionForBinding(events, binding)
	if err != nil || started.Actor != "hq-runtime-profile" {
		return err
	}
	if latestSessionDiagnostic(events, started.SessionID).Type == sessionProfileRecoverySent {
		return nil
	}
	prompt := runtimeProfileRecoveryEnvelope(rule, expected, expected, work)
	result := a.Herdr.Prompt(ctx, rule.Name, prompt)
	if result.Err != nil {
		return fmt.Errorf("修复后的 runtime 已到岗，但 durable recovery 信封尚未确认投递；下一轮会幂等重试：%w", result.Err)
	}
	_, err = a.appendDerivedSession(started, sessionProfileRecoverySent, "hq-runtime-profile", "durable recovery envelope delivery confirmed on retry")
	return err
}

func (a *App) recoverRuntimeProfileSeat(ctx context.Context, workspaceID string, rule AgentRule, reader HerdrAgentReader, retryUnknown, requireAction bool) error {
	releaseSeat, err := a.lockRuntimeSeat(rule.Name)
	if err != nil {
		return err
	}
	defer releaseSeat()
	_, err = a.withRuntimeAdmissions([]RuntimeAdmissionRequest{
		{Action: runtimeAdmissionAgentHibernate, Target: rule.Name},
		{Action: runtimeAdmissionAgentStart, Target: rule.Name},
	}, func() error {
		upLock, lockErr := a.lockUpContext(ctx)
		if lockErr != nil {
			return lockErr
		}
		defer unlock(upLock)

		work, workErr := a.runtimeRecoveryWorkFor(rule.Name)
		if workErr != nil {
			return fmt.Errorf("读取 runtime profile durable recovery work：%w", workErr)
		}
		events, eventsErr := a.Sessions.List(SessionFilter{Agent: rule.Name})
		if eventsErr != nil {
			return eventsErr
		}
		snapshot, snapshotErr := a.herdrSnapshot(ctx)
		if snapshotErr != nil {
			return snapshotErr
		}
		var live *HerdrAgent
		for index := range snapshot.Agents {
			if snapshot.Agents[index].Name == rule.Name {
				if live != nil {
					return fmt.Errorf("同名 live agent 不唯一，拒绝修复 runtime profile：%s", rule.Name)
				}
				live = &snapshot.Agents[index]
			}
		}
		if live == nil {
			if unresolved, ok := unresolvedProfileRepairSession(events); ok {
				if _, appendErr := a.appendDerivedSession(unresolved, sessionStopped, "hq-runtime-profile", profileRepairPendingReason+"; later snapshot confirmed old tab absent"); appendErr != nil {
					return fmt.Errorf("snapshot 已证明旧 tab 消失，但 runtime profile pending 未落账：%w", appendErr)
				}
				events, eventsErr = a.Sessions.List(SessionFilter{Agent: rule.Name})
				if eventsErr != nil {
					return fmt.Errorf("补记 runtime profile stopped 后重读 session：%w", eventsErr)
				}
			}
			if !profileRepairPending(events) {
				if requireAction {
					return fmt.Errorf("seat %s 当前没有 live runtime，也没有已确认的 runtime profile repair_pending；拒绝凭空启动", rule.Name)
				}
				return nil
			}
			expected, ok := runtimeProfileForKind(a.Config, rule.Kind)
			if !ok {
				return fmt.Errorf("seat %s 的 primary kind=%s 已无 runtime profile；拒绝用过期策略重启", rule.Name, rule.Kind)
			}
			return a.startProfileRepairRuntime(ctx, workspaceID, rule, expected, runtimeProfile{Kind: rule.Kind, Model: "unknown", ReasoningEffort: "unknown"}, work)
		}

		binding, bindingErr := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
		if bindingErr != nil {
			return bindingErr
		}
		expected, observed, mismatch, inspectErr := inspectLiveRuntimeProfile(ctx, reader, a.Config, binding)
		if inspectErr != nil {
			if errors.Is(inspectErr, errCodexSafetyBufferingVisible) {
				if requireAction {
					return fmt.Errorf("seat %s 正在显示 Codex 的 Dismiss and keep waiting 界面；该界面明确说明无需操作，HQ 不发送按键并继续等待原 model，以免竞态中断正常回合；界面消失后重试 profile 核验", rule.Name)
				}
				return nil
			}
			return inspectErr
		}
		if expected.Kind == "" {
			if requireAction {
				return fmt.Errorf("seat %s 当前 kind=%s 未配置 runtime_profiles.%s；没有可核验或修复的目标", rule.Name, binding.Kind, binding.Kind)
			}
			return nil
		}
		if mismatch == "" {
			return a.ensureProfileRecovery(ctx, rule, expected, binding, events, work)
		}
		if expected.OnDrift != runtimeProfileDriftRestartIdle {
			if requireAction {
				return fmt.Errorf("seat %s runtime profile 漂移：%s；on_drift=report 只报告不重启，请先把 runtime_profiles.%s.on_drift 改为 restart_idle", rule.Name, mismatch, binding.Kind)
			}
			return nil
		}
		if binding.Status != "idle" && binding.Status != "done" {
			if requireAction {
				return fmt.Errorf("seat %s runtime profile 漂移但 status=%s；HQ 不会中断工作或 blocked 回合，将在 idle|done 安全边界自动恢复", rule.Name, binding.Status)
			}
			return nil
		}
		started, startedErr := activeSessionForBinding(events, binding)
		if startedErr != nil {
			return fmt.Errorf("拒绝修复 runtime profile：%w", startedErr)
		}
		diagnostic := latestSessionDiagnostic(events, started.SessionID)
		if (diagnostic.Type == sessionProfileRepairAttempting || diagnostic.Type == sessionProfileRepairUnknown) && !retryUnknown {
			return fmt.Errorf("runtime profile close 尚未收敛：agent=%s session=%s latest=%s；先核验同一 incarnation/tab，确认仍在后由 can_manage_staff 角色运行 `hq --direct runtime repair-profile --agent %s --retry-unknown`", rule.Name, started.SessionID, diagnostic.Type, rule.Name)
		}
		if _, appendErr := a.appendDerivedSession(started, sessionProfileRepairAttempting, "hq-runtime-profile", "runtime profile mismatch confirmed: "+mismatch); appendErr != nil {
			return fmt.Errorf("profile_repair_attempting 未落账，尚未关闭旧 tab：%w", appendErr)
		}

		fresh, freshErr := a.herdrSnapshot(ctx)
		if freshErr != nil {
			_, _ = a.appendDerivedSession(started, sessionProfileRepairFailed, "hq-runtime-profile", "definitely-not-run; final snapshot failed")
			return freshErr
		}
		freshBinding, freshBindingErr := ResolveLiveBinding(fresh, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
		if freshBindingErr != nil || freshBinding.Status != binding.Status || liveBindingIncarnationMismatch(binding, freshBinding) != "" || !sessionMatchesBinding(started, freshBinding) {
			_, _ = a.appendDerivedSession(started, sessionProfileRepairFailed, "hq-runtime-profile", "definitely-not-run; final binding changed")
			return fmt.Errorf("runtime profile close 前 runtime incarnation/status 已变化；未调用 CloseTab")
		}
		freshExpected, freshObserved, freshMismatch, freshInspectErr := inspectLiveRuntimeProfile(ctx, reader, a.Config, freshBinding)
		if freshInspectErr != nil || freshMismatch == "" || freshExpected != expected || freshObserved != observed {
			_, _ = a.appendDerivedSession(started, sessionProfileRepairFailed, "hq-runtime-profile", "definitely-not-run; terminal profile evidence changed")
			if freshInspectErr != nil {
				return freshInspectErr
			}
			return fmt.Errorf("runtime profile 关闭前终端证据已变化；未调用 CloseTab，请重新巡检")
		}

		releaseRegistry, registryErr := a.lockRuntimeCurrentRegistry()
		if registryErr != nil {
			_, _ = a.appendDerivedSession(started, sessionProfileRepairFailed, "hq-runtime-profile", "definitely-not-run; registry changed")
			return registryErr
		}
		defer releaseRegistry()
		mutation := a.Herdr.CloseTab(ctx, started.TabID)
		after, afterErr := a.herdrSnapshot(ctx)
		if afterErr == nil && !snapshotHasTab(after, started.TabID) {
			if _, appendErr := a.appendDerivedSession(started, sessionStopped, "hq-runtime-profile", profileRepairPendingReason+"; old tab absence confirmed"); appendErr != nil {
				return fmt.Errorf("旧 tab 已关闭但 runtime profile repair_pending 未落账：%w", appendErr)
			}
			return a.startProfileRepairRuntime(ctx, workspaceID, rule, expected, observed, work)
		}
		reason := truncateError(mutation.Err)
		if afterErr != nil {
			reason = strings.TrimSpace(reason + "; snapshot=" + truncateError(afterErr))
		}
		if mutation.Outcome == herdrDefinitelyNotRun {
			_, _ = a.appendDerivedSession(started, sessionProfileRepairFailed, "hq-runtime-profile", "definitely-not-run; "+reason)
			return fmt.Errorf("runtime profile CloseTab definitely-not-run；旧 runtime 保持在岗，下一轮可安全重试")
		}
		_, _ = a.appendDerivedSession(started, sessionProfileRepairUnknown, "hq-runtime-profile", "CloseTab outcome unknown; "+reason)
		return fmt.Errorf("runtime profile CloseTab 结果不确定；禁止自动启动第二个 runtime；核验后按报错命令显式重试")
	})
	return err
}

func (a *App) recoverRuntimeProfileDriftsOnce(ctx context.Context) error {
	if len(a.Config.RuntimeProfiles) == 0 {
		return nil
	}
	reader, ok := a.Herdr.(HerdrAgentReader)
	if !ok {
		return fmt.Errorf("配置了 runtime_profiles，但 Herdr control 不支持 agent terminal read；HQ 无法核验或修复 profile")
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	workspaceID := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			if workspaceID != "" {
				return fmt.Errorf("runtime profile workspace label 不唯一：%s", a.Config.WorkspaceLabel)
			}
			workspaceID = workspace.ID
		}
	}
	if workspaceID == "" {
		return fmt.Errorf("runtime profile 找不到 workspace：%s", a.Config.WorkspaceLabel)
	}
	var names []string
	for _, rule := range a.Config.Agents {
		if rule.Disabled {
			continue
		}
		if _, configured := a.Config.RuntimeProfiles[rule.Kind]; configured {
			names = append(names, rule.Name)
		}
	}
	sort.Strings(names)
	var failures []string
	for _, name := range names {
		rule, _ := a.Config.exactRule(name)
		if err := a.recoverRuntimeProfileSeat(ctx, workspaceID, rule, reader, false, false); err != nil {
			failures = append(failures, name+": "+err.Error())
		}
	}
	if len(failures) != 0 {
		return fmt.Errorf("runtime profile 有 %d 个 seat 未收敛：%s", len(failures), strings.Join(failures, " | "))
	}
	return nil
}

func (a *App) retryRuntimeProfileRepair(ctx context.Context, agent string, retryUnknown bool) error {
	rule, ok := a.Config.exactRule(strings.TrimSpace(agent))
	if !ok {
		return fmt.Errorf("runtime profile seat 未登记或已停用：%s；运行 `hq staff list --all` 查看精确 seat slug", agent)
	}
	reader, ok := a.Herdr.(HerdrAgentReader)
	if !ok {
		return fmt.Errorf("Herdr control 不支持 agent terminal read；无法核验 model/effort，未关闭或启动任何 runtime")
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	workspaceID := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label != a.Config.WorkspaceLabel {
			continue
		}
		if workspaceID != "" {
			return fmt.Errorf("runtime profile workspace label 不唯一：%s", a.Config.WorkspaceLabel)
		}
		workspaceID = workspace.ID
	}
	if workspaceID == "" {
		return fmt.Errorf("runtime profile 找不到 workspace：%s", a.Config.WorkspaceLabel)
	}
	return a.recoverRuntimeProfileSeat(ctx, workspaceID, rule, reader, retryUnknown, true)
}

func addRuntimeProfilePatrolFindings(ctx context.Context, analysis *patrolAnalysis, snapshot HerdrSnapshot, cfg Config, hqRoot string, control HerdrControl) {
	if len(cfg.RuntimeProfiles) == 0 {
		return
	}
	reader, ok := control.(HerdrAgentReader)
	if !ok {
		addPatrolFinding(analysis, PatrolFinding{Category: "drift", ObjectID: "runtime-profile:adapter", SignalType: "runtime_profile_unverifiable", Message: "registry 配置了 runtime_profiles，但 Herdr control 不支持 agent terminal read；HQ 无法证明 model/effort"})
		return
	}
	var agents []HerdrAgent
	for _, agent := range snapshot.Agents {
		if _, configured := cfg.RuntimeProfiles[agent.Kind]; configured {
			agents = append(agents, agent)
		}
	}
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	for _, agent := range agents {
		binding, err := ResolveLiveBinding(snapshot, cfg, hqRoot, LiveBindingRequest{Seat: agent.Name, RequireInteractiveReady: true})
		if err != nil {
			continue
		}
		expected, observed, mismatch, inspectErr := inspectLiveRuntimeProfile(ctx, reader, cfg, binding)
		objectID := "agent:" + binding.WorkspaceID + ":" + binding.Seat
		if inspectErr != nil {
			if errors.Is(inspectErr, errCodexSafetyBufferingVisible) {
				// This is a transient Codex UI overlay, not evidence of model
				// drift. "No action is required" is the only race-free form of
				// Dismiss and keep waiting available through Herdr today.
				continue
			}
			addPatrolFinding(analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: binding.Seat, TabID: binding.TabID,
				SignalType: "runtime_profile_unverified", Message: inspectErr.Error() + "；不要把该 seat 判为 profile healthy；运行 `herdr agent read " + binding.Seat + " --source detection --lines 160 --format text` 核验"})
			continue
		}
		if mismatch == "" {
			continue
		}
		next := "on_drift=report，仅报告不重启"
		if expected.OnDrift == runtimeProfileDriftRestartIdle {
			next = "HQ gateway 会在 status=idle|done 的安全边界自动替换 runtime；working|blocked 不会被中断"
		}
		addPatrolFinding(analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: binding.Seat, TabID: binding.TabID,
			SignalType: "runtime_profile_mismatch", Message: fmt.Sprintf("runtime profile 漂移：observed model=%s effort=%s，expected model=%s effort=%s；%s", observed.Model, observed.ReasoningEffort, expected.Model, expected.ReasoningEffort, next)})
	}
}
