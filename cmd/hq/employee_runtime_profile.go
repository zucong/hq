package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/spf13/pflag"
)

func withoutCodexModelArgs(args []string) ([]string, error) {
	if _, _, err := codexRuntimeProfileOverrides(args); err != nil {
		return nil, err
	}
	isModelConfig := func(value string) bool {
		key := strings.TrimSpace(strings.SplitN(value, "=", 2)[0])
		return key == "model" || key == "model_reasoning_effort"
	}
	var out []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--model" || arg == "-m":
			i++
		case strings.HasPrefix(arg, "--model=") || strings.HasPrefix(arg, "-m="):
		case arg == "-c" || arg == "--config":
			i++
			if !isModelConfig(args[i]) {
				out = append(out, arg, args[i])
			}
		case strings.HasPrefix(arg, "--config=") && isModelConfig(strings.TrimPrefix(arg, "--config=")):
		default:
			out = append(out, arg)
		}
	}
	return out, nil
}

func (a *App) ensureEmployeeModelReady(rule AgentRule) error {
	if _, overridden := a.Config.RuntimeProfiles[rule.Kind].Employees[rule.Name]; !overridden {
		return nil
	}
	if a.Herdr == nil {
		if !a.ProductionRuntime {
			return nil
		}
		return fmt.Errorf("员工模型核验缺少 Herdr")
	}
	snapshot, err := a.herdrSnapshot(a.requestContext())
	if err != nil {
		return err
	}
	live := false
	for _, agent := range snapshot.Agents {
		if agent.Name == rule.Name {
			live = true
		}
	}
	if !live {
		return nil
	}
	binding, err := ResolveLiveBinding(snapshot, a.Config, a.HQRoot, LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		return err
	}
	if binding.Kind != rule.Kind {
		return conflictf("员工 %s 当前 kind=%s 与期望 %s 不符；先运行 hq patrol --json 核验并恢复后再 issue", rule.Name, binding.Kind, rule.Kind)
	}
	reader, ok := a.Herdr.(HerdrAgentReader)
	if !ok {
		return fmt.Errorf("员工 %s 模型覆盖无法核验：Herdr 缺少 terminal read", rule.Name)
	}
	_, _, mismatch, err := inspectLiveRuntimeProfile(a.requestContext(), reader, a.Config, binding)
	if err != nil || mismatch != "" {
		return conflictf("员工 %s 的模型覆盖尚未确认生效（%s；%v）；尚未创建 assignment。运行 hq patrol --json，等待 restart_idle 恢复后重试同一 issue；on_drift=report 时联系 can_manage_staff 处理", rule.Name, mismatch, err)
	}
	return nil
}

func validateEmployeeRuntimeProfile(p EmployeeRuntimeProfile) error {
	if p.Model == "" || strings.ContainsAny(p.Model, " \t\r\n\x00") || !utf8.ValidString(p.Model) || utf8.RuneCountInString(p.Model) > 100 {
		return fmt.Errorf("--model 必须是非空、无空白、至多 100 rune 的原生模型 ID")
	}
	if !validReasoningEffort(p.ReasoningEffort) {
		return fmt.Errorf("--effort 必须是 none|minimal|low|medium|high|xhigh；用法：hq staff update --name AGENT --model MODEL --effort medium")
	}
	return nil
}

func validateRuntimeUpdateFlags(fs *pflag.FlagSet, model, effort string) error {
	var invalid string
	fs.Visit(func(f *pflag.Flag) {
		switch f.Name {
		case "name", "model", "effort", "office", "json", "dry-run", "config", "data", "herdr":
		default:
			invalid = f.Name
		}
	})
	if invalid != "" {
		return fmt.Errorf("模型调整不能混合 --%s；运行 hq staff update --name AGENT --model MODEL --effort medium；人事变更请另行使用 --approval", invalid)
	}
	return validateEmployeeRuntimeProfile(EmployeeRuntimeProfile{Model: model, ReasoningEffort: effort})
}

func (a *App) updateEmployeeRuntimeProfile(name, model, effort string, fs *pflag.FlagSet) error {
	if err := validateRuntimeUpdateFlags(fs, model, effort); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	if !agentNamePattern.MatchString(name) {
		return fmt.Errorf("--name 必须是已注册员工 slug；运行 hq staff list")
	}
	// Same fence as issue/revision/activation: no new assignment or runtime
	// replacement can interleave with the capacity check and config commit.
	release, err := a.lockRuntimeSeatOriginFence(name)
	if err != nil {
		return err
	}
	defer release()
	ledger, err := a.strictLedgerStateReadOnly()
	if err != nil {
		return err
	}
	if ledger.assignmentCapacityUsed(name) != 0 {
		return conflictf("员工 %s 尚有在途 assignment 或待投递 issue；先运行 hq assignment list --assignee %s --json，收敛原任务后重试模型调整", name, name)
	}
	var effective runtimeProfile
	cfg, err := mutateConfigWithOptions(a.ConfigPath, a.staffConfigWriteOptions(), func(cfg *Config) error {
		liveActor, ok := cfg.exactRule(actor.Name)
		target, found := cfg.exactRule(name)
		if !ok || !found || target.Disabled || liveActor.Disabled {
			return permissionf("actor 或员工不在职；运行 hq staff list")
		}
		if !liveActor.CanManageStaff && !(cfg.isManager(liveActor) && target.ReportsTo == liveActor.Name) {
			return permissionf("只能由直属经理 %s 或 can_manage_staff 职责位调整员工 %s 的模型", target.ReportsTo, name)
		}
		policy, ok := cfg.RuntimeProfiles[target.Kind]
		if !ok || target.Kind != "codex" {
			return fmt.Errorf("员工 %s 需要公司先配置 runtime_profiles.codex；运行 hq staff get --name %s 核验 kind", name, name)
		}
		if policy.Employees == nil {
			policy.Employees = map[string]EmployeeRuntimeProfile{}
		}
		policy.Employees[name] = EmployeeRuntimeProfile{Model: model, ReasoningEffort: effort}
		cfg.RuntimeProfiles[target.Kind] = policy
		effective, _ = runtimeProfileForEmployee(*cfg, target.Kind, name)
		return nil
	})
	if err != nil {
		return err
	}
	if !a.DryRun {
		a.Config = cfg
	}
	return a.output(map[string]any{"agent": name, "model": effective.Model, "reasoning_effort": effective.ReasoningEffort, "on_drift": effective.OnDrift, "dry_run": a.DryRun}, fmt.Sprintf("员工 %s 期望模型=%s effort=%s；on_drift=%s；配置已%s。运行 hq patrol --json 核验实际运行值；restart_idle 在安全空闲边界恢复，离线员工在下次正式 issue 启动时生效。", name, model, effort, effective.OnDrift, map[bool]string{true: "校验（dry-run）", false: "保存"}[a.DryRun]))
}
