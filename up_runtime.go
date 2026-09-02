package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type PartialStartError struct {
	Agent       string `json:"agent"`
	WorkspaceID string `json:"workspace_id"`
	TabID       string `json:"tab_id"`
	PaneID      string `json:"pane_id"`
	SessionID   string `json:"session_id"`
	Cause       error  `json:"-"`
}

func (e *PartialStartError) Error() string {
	return fmt.Sprintf("partial-success agent=%s workspace=%s tab=%s pane=%s session=%s：%v", e.Agent, e.WorkspaceID, e.TabID, e.PaneID, e.SessionID, e.Cause)
}

func (e *PartialStartError) Unwrap() error { return e.Cause }

type DefinitelyNotRunStartError struct {
	Agent string
	Cause error
}

func (e *DefinitelyNotRunStartError) Error() string {
	return fmt.Sprintf("start definitely-not-run agent=%s：%v", e.Agent, e.Cause)
}

func (e *DefinitelyNotRunStartError) Unwrap() error { return e.Cause }

func (a *App) herdrSnapshot(ctx context.Context) (HerdrSnapshot, error) {
	return a.Herdr.Snapshot(ctx, HerdrSnapshotScope{WorkspaceLabel: a.Config.WorkspaceLabel})
}

func (a *App) runUp(args []string) error {
	fs := newLeafParser("up")
	fs.SetOutput(a.Err)
	noGateway := fs.Bool("no-gateway", false, "只启动 agent，不启动 HQ 写入网关")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return fmt.Errorf("用法：hq up [agent]")
	}
	if a.HostColdStart && (fs.NArg() != 0 || *noGateway) {
		return fmt.Errorf("宿主机冷启动只接受无位置参数、无 --no-gateway 的 hq up；它只恢复 registry 中的 always 岗位和 gateway")
	}
	if a.Herdr == nil {
		return fmt.Errorf("Herdr control 未注入，拒绝 PATH 回落")
	}
	if a.Sessions == nil {
		return fmt.Errorf("session lifecycle store 未注入；在任何 tab create 前拒绝启动")
	}
	desired, err := a.desiredAgents(fs.Args())
	if err != nil {
		return err
	}
	for _, rule := range desired {
		if _, err := validateWorkstation(a.HQRoot, rule); err != nil {
			return err
		}
	}
	requests := make([]RuntimeAdmissionRequest, 0, len(desired)+1)
	for _, rule := range desired {
		requests = append(requests, RuntimeAdmissionRequest{Action: runtimeAdmissionAgentStart, Target: rule.Name})
	}
	if !*noGateway || len(requests) == 0 {
		// Default up always creates or restores the gateway control plane, even
		// when the requested agent is an ESTOP-exempt manager/account closer.
		// A no-gateway run with an empty roster can still create the workspace.
		// Give both paths an explicit control-plane admission instead of letting
		// an allowed agent_start (or an empty request set) cover extra mutations.
		requests = append(requests, RuntimeAdmissionRequest{Action: runtimeAdmissionControlPlane, Target: a.Config.WorkspaceLabel})
	}
	_, err = a.withRuntimeAdmissions(requests, func() error {
		if a.HostColdStart {
			workspaceID, startErr := a.startCompanyControlPlane(context.Background(), "hq-up-host")
			if startErr != nil {
				return startErr
			}
			_, writeErr := fmt.Fprintf(a.Out, "HQ 冷启动完成：always 岗位已收敛，gateway 在线；workspace=%s 配置=%s\n", workspaceID, a.ConfigPath)
			return writeErr
		}
		return a.runUpAdmitted(desired, *noGateway)
	})
	return err
}

func (a *App) runUpAdmitted(desired []AgentRule, noGateway bool) error {
	lock, err := a.lockUp()
	if err != nil {
		return err
	}
	upLocked := true
	defer func() {
		if upLocked {
			unlock(lock)
		}
	}()

	ctx := context.Background()
	var workspaceID string
	if noGateway {
		// --no-gateway promises an agent-only mutation. Requiring an existing
		// workspace prevents an ESTOP-exempt agent_start from smuggling a
		// CreateWorkspace control-plane mutation through that narrower admission.
		workspaceID, err = a.requireExistingHQWorkspace(ctx)
	} else {
		workspaceID, err = a.ensureHQWorkspace(ctx)
	}
	if err != nil {
		return err
	}
	if !noGateway {
		// The child gateway reconciles the durable outbox before binding its
		// socket. A prepared wakeup message may cold-resume an offline target,
		// and that operation needs the same up lock. Never hold .hq-up.lock while
		// waiting for gateway health or parent and child can deadlock. A separate
		// bootstrap lock still serializes concurrent parents.
		unlock(lock)
		upLocked = false
		gatewayLock, lockErr := a.lockGatewayUp()
		if lockErr != nil {
			return lockErr
		}
		gatewayErr := a.ensureHQGateway(ctx, workspaceID)
		unlock(gatewayLock)
		if gatewayErr != nil {
			return gatewayErr
		}
		lock, err = a.lockUp()
		if err != nil {
			return err
		}
		upLocked = true
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	started, skipped := 0, 0
	for _, rule := range desired {
		matched, mismatch := exactLiveMatch(snapshot, workspaceID, rule, a.HQRoot)
		if matched {
			fmt.Fprintf(a.Out, "跳过 %s（已精确在岗）\n", rule.Name)
			skipped++
			continue
		}
		if mismatch != "" {
			return fmt.Errorf("员工 %s 存在但不满足精确在岗合同：%s；拒绝重复启动", rule.Name, mismatch)
		}
		if err := a.startHQAgentAdmitted(ctx, workspaceID, rule); err != nil {
			return err
		}
		started++
		snapshot, err = a.herdrSnapshot(ctx)
		if err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(a.Out, "HQ 启动完成：新启动 %d，已在岗 %d；配置=%s\n", started, skipped, a.ConfigPath)
	return err
}

func (a *App) desiredAgents(args []string) ([]AgentRule, error) {
	if len(args) == 1 {
		rule, ok := a.Config.ruleFor(args[0])
		if !ok {
			return nil, fmt.Errorf("员工未登记或已停用：%s；先由授权角色运行 hq staff add/update", args[0])
		}
		if rule.ActivationPolicy == activationOnAssignment {
			return nil, fmt.Errorf("员工 %s activation=on_assignment，不能用 hq up 旁路启动；请由其直属经理通过 durable hq issue 激活，同一稳定 seat 会自动 cold-resume", rule.Name)
		}
		return []AgentRule{rule}, nil
	}
	var desired []AgentRule
	for _, rule := range a.Config.Agents {
		if rule.ActivationPolicy == activationAlways && !rule.Disabled {
			desired = append(desired, rule)
		}
	}
	sort.Slice(desired, func(i, j int) bool { return desired[i].Name < desired[j].Name })
	return desired, nil
}

func validateWorkstation(hqRoot string, rule AgentRule) (string, error) {
	cwd, err := resolveAgentWorkstation(hqRoot, rule)
	if err != nil {
		return "", fmt.Errorf("%w；修复后重试，尚未创建 tab", err)
	}
	manual := filepath.Join(cwd, "AGENTS.md")
	if rule.ManualPath != "" {
		manual, err = resolveRegistryManual(hqRoot, rule)
		if err != nil {
			return "", fmt.Errorf("%w；修复后重试，尚未创建 tab", err)
		}
	} else {
		if _, err := canonicalExistingRegularFile(manual, "员工 "+rule.Name+" AGENTS.md"); err != nil {
			return "", fmt.Errorf("员工 %s 的 AGENTS.md 必须是 canonical、可读、非 symlink 普通文件：%s；修复后重试，尚未创建 tab", rule.Name, manual)
		}
		file, err := os.Open(manual)
		if err != nil {
			return "", fmt.Errorf("员工 %s 的 AGENTS.md 不可读：%w；修复后重试，尚未创建 tab", rule.Name, err)
		}
		_ = file.Close()
	}
	return cwd, nil
}

func (a *App) lockUp() (*os.File, error) {
	return a.lockUpCoordinatorContext(context.Background(), ".hq-up.lock", "hq up 启动锁")
}

func (a *App) lockUpContext(ctx context.Context) (*os.File, error) {
	return a.lockUpCoordinatorContext(ctx, ".hq-up.lock", "hq up 启动锁")
}

func (a *App) lockGatewayUp() (*os.File, error) {
	return a.lockUpCoordinatorContext(context.Background(), ".hq-gateway-up.lock", "hq gateway 启动锁")
}

func (a *App) lockUpCoordinatorContext(ctx context.Context, name, label string) (*os.File, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := mkdirDurable(a.DataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(a.DataDir, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("%s 必须是普通文件：%s", label, path)
	}
	if err := flockContext(ctx, int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("等待 %s：%w", label, err)
	}
	return file, nil
}

func (a *App) ensureHQWorkspace(ctx context.Context) (string, error) {
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return "", err
	}
	var matches []HerdrWorkspace
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			matches = append(matches, workspace)
		}
	}
	if len(matches) == 1 {
		fmt.Fprintf(a.Out, "工作区 %s：%s\n", a.Config.WorkspaceLabel, matches[0].ID)
		return matches[0].ID, nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("workspace label %s 匹配多个稳定 ID，拒绝任选其一", a.Config.WorkspaceLabel)
	}
	created, result := a.Herdr.CreateWorkspace(ctx, a.Config.WorkspaceLabel)
	if result.Err == nil && result.Outcome == herdrConfirmed && created.ID != "" {
		fmt.Fprintf(a.Out, "已创建工作区 %s：%s\n", a.Config.WorkspaceLabel, created.ID)
		return created.ID, nil
	}
	if result.Outcome == herdrDefinitelyNotRun {
		return "", result.Err
	}
	reconciled, reconcileErr := a.herdrSnapshot(ctx)
	if reconcileErr != nil {
		return "", fmt.Errorf("workspace create 结果不确定且 reconcile 失败：%v；%w", result.Err, reconcileErr)
	}
	for _, workspace := range reconciled.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			if created.ID == "" || created.ID == workspace.ID {
				return workspace.ID, nil
			}
		}
	}
	return "", fmt.Errorf("workspace create 结果不确定且未精确收敛：%w", result.Err)
}

func (a *App) requireExistingHQWorkspace(ctx context.Context) (string, error) {
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return "", err
	}
	var matches []HerdrWorkspace
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			matches = append(matches, workspace)
		}
	}
	if len(matches) == 1 {
		fmt.Fprintf(a.Out, "工作区 %s：%s\n", a.Config.WorkspaceLabel, matches[0].ID)
		return matches[0].ID, nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("workspace label %s 匹配多个稳定 ID，拒绝任选其一", a.Config.WorkspaceLabel)
	}
	return "", fmt.Errorf("workspace %s 不存在；--no-gateway 不允许创建 control plane，请先在 ESTOP release 后运行默认 hq up", a.Config.WorkspaceLabel)
}

func exactLiveMatch(snapshot HerdrSnapshot, workspaceID string, rule AgentRule, hqRoot string) (bool, string) {
	nameCount := 0
	for _, agent := range snapshot.Agents {
		if agent.Name == rule.Name {
			nameCount++
		}
	}
	if nameCount == 0 {
		return false, ""
	}
	if nameCount != 1 {
		return false, "同名 agent 重复"
	}
	workspaceLabel := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.ID == workspaceID {
			workspaceLabel = workspace.Label
			break
		}
	}
	if workspaceLabel == "" {
		return false, "目标 workspace 稳定 ID 不存在"
	}
	_, err := ResolveLiveBinding(snapshot, Config{WorkspaceLabel: workspaceLabel, Agents: []AgentRule{rule}}, hqRoot,
		LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}

func (a *App) startHQAgent(ctx context.Context, workspaceID string, rule AgentRule) error {
	_, err := a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentStart, Target: rule.Name}, func() error {
		return a.startHQAgentAdmitted(ctx, workspaceID, rule)
	})
	return err
}

func (a *App) startHQAgentAdmitted(ctx context.Context, workspaceID string, rule AgentRule) error {
	return a.startHQAgentAdmittedWithOptions(ctx, workspaceID, rule, runtimeStartOptions{})
}

type runtimeStartOptions struct {
	Kind           string
	PermissionMode string
	AgentArgs      []string
	Actor          string
	Reason         string
	PromptSuffix   string
}

func (a *App) startHQAgentAdmittedWithOptions(ctx context.Context, workspaceID string, rule AgentRule, options runtimeStartOptions) error {
	runtimeRule := rule
	if options.Kind != "" {
		runtimeRule.Kind = options.Kind
		runtimeRule.PermissionMode = options.PermissionMode
		runtimeRule.AgentArgs = append([]string(nil), options.AgentArgs...)
	}
	if runtimeRule.Kind == "" {
		return fmt.Errorf("员工 %s 未配置 kind", rule.Name)
	}
	cwd, err := validateWorkstation(a.HQRoot, rule)
	if err != nil {
		return err
	}
	before, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	// Re-resolve the workstation and re-hash the immutable role artifact as the
	// direct precondition to CreateTab. The earlier preflight is useful for a
	// clean error before snapshot work, but it is not the runtime trust check.
	cwd, err = validateWorkstation(a.HQRoot, rule)
	if err != nil {
		return err
	}
	if _, err := verifyAgentRoleCardArtifact(a.Config, a.HQRoot, rule); err != nil {
		return fmt.Errorf("员工 %s 启动前 role card 复核失败：%w；尚未创建 tab", rule.Name, err)
	}
	created, result := a.Herdr.CreateTab(ctx, HerdrTabSpec{
		WorkspaceID: workspaceID, CWD: cwd, Label: rosterTabLabel(rule),
		Env: map[string]string{
			"HQ_AGENT_NAME": rule.Name, "HQ_DEPARTMENT": rule.Department, "HQ_REPORTS_TO": rule.ReportsTo,
		},
	})
	if result.Err != nil {
		if result.Outcome == herdrDefinitelyNotRun {
			return &DefinitelyNotRunStartError{Agent: rule.Name, Cause: result.Err}
		}
		created, err = reconcileCreatedTab(ctx, a.Herdr, HerdrSnapshotScope{WorkspaceLabel: a.Config.WorkspaceLabel}, before, workspaceID, rosterTabLabel(rule), cwd)
		if err != nil {
			return fmt.Errorf("tab create 结果不确定且无法精确 reconcile：%v；%w", result.Err, err)
		}
	}
	if created.Tab.ID == "" || created.Pane.ID == "" {
		return fmt.Errorf("tab create 未返回稳定 tab/pane id")
	}
	native := nativeAgentArgs(runtimeRule)
	start := a.Herdr.StartAgent(ctx, rule.Name, runtimeRule.Kind, created.Pane.ID, native)
	for attempt := 0; start.Err != nil && start.Outcome == herdrDefinitelyNotRun && start.ErrorCode == "agent_pane_busy" && attempt < 19; attempt++ {
		select {
		case <-ctx.Done():
			break
		case <-time.After(100 * time.Millisecond):
		}
		if ctx.Err() != nil {
			break
		}
		start = a.Herdr.StartAgent(ctx, rule.Name, runtimeRule.Kind, created.Pane.ID, native)
	}
	if start.Err != nil {
		if start.Outcome == herdrAmbiguous {
			snapshot, snapshotErr := a.herdrSnapshot(ctx)
			if snapshotErr == nil {
				if matched, mismatch := exactStartedAgentMatch(snapshot, workspaceID, runtimeRule, a.HQRoot, created); matched {
					start.Err = nil
				} else if mismatch != "" {
					start.Err = fmt.Errorf("ambiguous start reconcile mismatch: %s", mismatch)
				}
			}
		}
		if start.Err != nil {
			cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
			return combineLifecycleError(fmt.Errorf("启动员工 %s：%w", rule.Name, start.Err), cleanupErr, created.Tab.ID)
		}
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
		return combineLifecycleError(fmt.Errorf("start 后 snapshot 核验失败：%w", err), cleanupErr, created.Tab.ID)
	}
	initialBinding, bindingErr := resolveStartedAgentBinding(snapshot, workspaceID, runtimeRule, a.HQRoot, created)
	if bindingErr != nil {
		cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
		return combineLifecycleError(fmt.Errorf("start 未形成精确 live 匹配：%w", bindingErr), cleanupErr, created.Tab.ID)
	}
	now := time.Now()
	if a.Clock != nil {
		now = a.Clock()
	}
	actor := options.Actor
	if actor == "" {
		actor = a.MaintenanceActor
	}
	if actor == "" {
		actor = "hq-up"
	}
	reason := options.Reason
	if reason == "" {
		reason = "hq up confirmed agent start"
	}
	event, err := newSessionEvent(now, "started", created, workspaceID, rule, actor, reason, cwd)
	if err != nil {
		cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
		return combineLifecycleError(err, cleanupErr, created.Tab.ID)
	}
	event, err = bindSessionEventRuntime(event, initialBinding)
	if err != nil {
		cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
		return combineLifecycleError(fmt.Errorf("session runtime incarnation 绑定失败：%w", err), cleanupErr, created.Tab.ID)
	}
	if err := a.Sessions.Append(event); err != nil {
		cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
		return combineLifecycleError(fmt.Errorf("session start 记账失败：%w", err), cleanupErr, created.Tab.ID)
	}
	finalSnapshot, finalSnapshotErr := a.herdrSnapshot(ctx)
	if finalSnapshotErr != nil {
		return &PartialStartError{Agent: rule.Name, WorkspaceID: workspaceID, TabID: created.Tab.ID, PaneID: created.Pane.ID,
			SessionID: event.SessionID, Cause: fmt.Errorf("startup Prompt 前 live binding snapshot 失败：%w", finalSnapshotErr)}
	}
	finalBinding, finalBindingErr := resolveStartedAgentBinding(finalSnapshot, workspaceID, runtimeRule, a.HQRoot, created)
	if finalBindingErr != nil {
		return &PartialStartError{Agent: rule.Name, WorkspaceID: workspaceID, TabID: created.Tab.ID, PaneID: created.Pane.ID,
			SessionID: event.SessionID, Cause: fmt.Errorf("startup Prompt 前 live binding 核验失败：%w", finalBindingErr)}
	}
	if mismatch := liveBindingIncarnationMismatch(initialBinding, finalBinding); mismatch != "" {
		return &PartialStartError{Agent: rule.Name, WorkspaceID: workspaceID, TabID: created.Tab.ID, PaneID: created.Pane.ID,
			SessionID: event.SessionID, Cause: fmt.Errorf("startup Prompt 前 runtime incarnation 漂移：%s", mismatch)}
	}
	prompt := startupEnvelopeWithRuntime(rule, a.Config.ownerPrincipal(), filepath.Join(a.Office, "tools", "hq", "bin", "hq"), cwd)
	if strings.TrimSpace(options.PromptSuffix) != "" {
		prompt += "\n\n" + strings.TrimSpace(options.PromptSuffix)
	}
	promptResult := a.Herdr.Prompt(ctx, rule.Name, prompt)
	if promptResult.Err != nil {
		return &PartialStartError{Agent: rule.Name, WorkspaceID: workspaceID, TabID: created.Tab.ID, PaneID: created.Pane.ID, SessionID: event.SessionID, Cause: promptResult.Err}
	}
	_, err = fmt.Fprintf(a.Out, "已启动 %s（department=%s kind=%s pane=%s session=%s）\n", rule.Name, rule.Department, runtimeRule.Kind, created.Pane.ID, event.SessionID)
	return err
}

var defaultAgentArgsByKind = map[string][]string{
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

func nativeAgentArgs(rule AgentRule) []string {
	result := append([]string(nil), rule.AgentArgs...)
	if rule.PermissionMode == "native" {
		return result
	}
	if rule.PermissionMode == "yolo" {
		for _, required := range defaultAgentArgsByKind[rule.Kind] {
			found := false
			for _, existing := range result {
				if existing == required {
					found = true
					break
				}
			}
			if !found {
				result = append(result, required)
			}
		}
	}
	return result
}

func startupEnvelope(rule AgentRule, ownerPrincipal string) string {
	return startupEnvelopeWithBinary(rule, ownerPrincipal, "ceo-office/tools/hq/bin/hq")
}

func startupEnvelopeWithBinary(rule AgentRule, ownerPrincipal, hqBinary string) string {
	return startupEnvelopeWithRuntime(rule, ownerPrincipal, hqBinary, workstationReference(rule))
}

func startupEnvelopeWithRuntime(rule AgentRule, ownerPrincipal, hqBinary, workstation string) string {
	if rule.ManualPath == "" {
		return fmt.Sprintf("[HQ notification] WORKSTATION=%s 本信封只建立到岗运行态。请读取本工位 AGENTS.md，按职责边界到岗；本公司的唯一 HQ CLI 是 %s，不得搜索或调用其他公司、PATH 或 _harness 中的 hq。读完立即结束本回合并等待直属经理通过 hq issue 委派首个 durable case。在此之前不得处理平级或跨部门消息，不得运行任何 hq/herdr 业务命令，也不得发送报到、回铃或消息。HQ 会在后续唤醒 prompt 或 accept 输出中自动附带此前静默消息。", workstation, hqBinary)
	}
	reports := rule.ReportsTo
	if reports == "" {
		reports = ownerPrincipal
	}
	waitInstruction := "等待直属经理通过 hq issue 委派首个 durable case"
	if rule.hasResponsibility(roleApprovalWitness) {
		waitInstruction = "等待公司所有者的正式治理输入；你只能据此通过 HQ 建立或推进公司级事项，不得把普通 Herdr prompt 当作已批准的业务事实"
	}
	return fmt.Sprintf("[HQ notification] WORKSTATION=%s 本信封只建立到岗运行态。你是%s（agent=%s，部门=%s）；完整阅读岗位手册 %s；向 %s 汇报并只接受注册表授权的纵向指令。本公司的唯一 HQ CLI 是 %s，不得搜索或调用其他公司、PATH 或 _harness 中的 hq。读完立即结束本回合，%s；在首个 case 到达前不得处理平级或跨部门消息，不得运行 hq/herdr 业务命令，也不得发送报到、回铃或消息。HQ 会在后续唤醒 prompt 或 accept 输出中自动附带此前静默排队的 [HQ message]。",
		workstation, rule.Nickname, rule.Name, rule.DepartmentLabel, rule.ManualPath, reports, hqBinary, waitInstruction)
}

func exactStartedAgentMatch(snapshot HerdrSnapshot, workspaceID string, rule AgentRule, hqRoot string, created HerdrTabCreated) (bool, string) {
	_, err := resolveStartedAgentBinding(snapshot, workspaceID, rule, hqRoot, created)
	if err != nil {
		return false, err.Error()
	}
	return true, ""
}

func resolveStartedAgentBinding(snapshot HerdrSnapshot, workspaceID string, rule AgentRule, hqRoot string, created HerdrTabCreated) (LiveBinding, error) {
	workspaceLabel := ""
	for _, workspace := range snapshot.Workspaces {
		if workspace.ID == workspaceID {
			workspaceLabel = workspace.Label
			break
		}
	}
	if workspaceLabel == "" {
		return LiveBinding{}, fmt.Errorf("目标 workspace 稳定 ID 不存在：%s", workspaceID)
	}
	binding, err := ResolveLiveBinding(snapshot, Config{WorkspaceLabel: workspaceLabel, Agents: []AgentRule{rule}}, hqRoot,
		LiveBindingRequest{Seat: rule.Name, RequireInteractiveReady: true})
	if err != nil {
		return LiveBinding{}, err
	}
	if binding.WorkspaceID != workspaceID || binding.TabID != created.Tab.ID || binding.PaneID != created.Pane.ID {
		return LiveBinding{}, fmt.Errorf("agent 绑定 workspace/tab/pane=%s/%s/%s，要求本次创建 %s/%s/%s",
			binding.WorkspaceID, binding.TabID, binding.PaneID, workspaceID, created.Tab.ID, created.Pane.ID)
	}
	if created.Pane.TerminalID != "" {
		if binding.TerminalID == "" {
			return LiveBinding{}, fmt.Errorf("本次创建 pane terminal=%s，但 agent binding 缺少 terminal", created.Pane.TerminalID)
		}
		if created.Pane.TerminalID != binding.TerminalID {
			return LiveBinding{}, fmt.Errorf("agent terminal=%s 与本次创建 pane terminal=%s 不匹配", binding.TerminalID, created.Pane.TerminalID)
		}
	}
	return binding, nil
}

func liveBindingIncarnationMismatch(before, after LiveBinding) string {
	if before.WorkspaceID != after.WorkspaceID || before.TabID != after.TabID || before.PaneID != after.PaneID ||
		before.Seat != after.Seat || before.Kind != after.Kind || filepath.Clean(before.CWD) != filepath.Clean(after.CWD) {
		return "workspace/tab/pane/seat/kind/cwd 已变化"
	}
	if before.TerminalID != "" {
		if after.TerminalID == "" {
			return fmt.Sprintf("terminal %s → missing", before.TerminalID)
		}
		if before.TerminalID != after.TerminalID {
			return fmt.Sprintf("terminal %s → %s", before.TerminalID, after.TerminalID)
		}
	}
	if before.AgentSession != nil {
		if after.AgentSession == nil {
			return "native agent_session 消失"
		}
		if *before.AgentSession != *after.AgentSession {
			return fmt.Sprintf("native agent_session %s/%s → %s/%s", before.AgentSession.Kind, before.AgentSession.Value,
				after.AgentSession.Kind, after.AgentSession.Value)
		}
	}
	if after.Revision < before.Revision {
		return fmt.Sprintf("revision 倒退 %d → %d", before.Revision, after.Revision)
	}
	return ""
}

func reconcileCreatedTab(ctx context.Context, control HerdrControl, scope HerdrSnapshotScope, before HerdrSnapshot, workspaceID, label, cwd string) (HerdrTabCreated, error) {
	known := map[string]bool{}
	for _, tab := range before.Tabs {
		known[tab.ID] = true
	}
	after, err := control.Snapshot(ctx, scope)
	if err != nil {
		return HerdrTabCreated{}, err
	}
	var candidates []HerdrTab
	for _, tab := range after.Tabs {
		if !known[tab.ID] && tab.WorkspaceID == workspaceID && tab.Label == label && filepath.Clean(tab.CWD) == filepath.Clean(cwd) {
			candidates = append(candidates, tab)
		}
	}
	if len(candidates) != 1 {
		return HerdrTabCreated{}, fmt.Errorf("新增精确 tab 候选=%d", len(candidates))
	}
	var panes []HerdrPane
	for _, pane := range after.Panes {
		if pane.TabID == candidates[0].ID && pane.WorkspaceID == workspaceID && filepath.Clean(pane.CWD) == filepath.Clean(cwd) {
			panes = append(panes, pane)
		}
	}
	if len(panes) != 1 {
		return HerdrTabCreated{}, fmt.Errorf("新增 tab 的 root pane 候选=%d", len(panes))
	}
	return HerdrTabCreated{Tab: candidates[0], Pane: panes[0]}, nil
}

func (a *App) closeOwnedTab(ctx context.Context, tabID string) error {
	result := a.Herdr.CloseTab(ctx, tabID)
	if result.Err == nil {
		return nil
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err == nil {
		for _, tab := range snapshot.Tabs {
			if tab.ID == tabID {
				return fmt.Errorf("tab close 结果不确定且 orphan 仍存在：%s：%v", tabID, result.Err)
			}
		}
		return nil
	}
	return fmt.Errorf("tab close 失败且 reconcile 失败，orphan_id=%s：%v；%w", tabID, result.Err, err)
}

func combineLifecycleError(primary, cleanup error, tabID string) error {
	if cleanup == nil {
		return primary
	}
	return fmt.Errorf("%v；回收失败 orphan_id=%s：%w", primary, tabID, cleanup)
}

func (a *App) ensureHQGateway(ctx context.Context, workspaceID string) error {
	if a.GatewayHealth == nil {
		return fmt.Errorf("gateway pinger 未注入")
	}
	socket, err := gatewaySocketPath(a.DataDir)
	if err != nil {
		return fmt.Errorf("HQ 网关启动前解析 socket：%w", err)
	}
	if health := a.GatewayHealth.Ping(ctx, socket, workspaceID); health.OK {
		fmt.Fprintf(a.Out, "HQ 网关已在线：%s server=%s\n", socket, health.ServerID)
		return nil
	}
	binary := filepath.Join(a.Office, "tools", "hq", "bin", "hq")
	canonical, err := canonicalExistingRegularFile(binary, "HQ binary")
	if err != nil {
		return fmt.Errorf("找不到公司实例使用的 HQ 二进制：%s；请安装发布制品到 ceo-office/tools/hq/bin/hq", binary)
	}
	info, err := os.Stat(canonical)
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("HQ 二进制不可执行：%s", canonical)
	}
	before, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	created, result := a.Herdr.CreateTab(ctx, HerdrTabSpec{WorkspaceID: workspaceID, CWD: a.Office, Label: "hq-gateway"})
	if result.Err != nil {
		if result.Outcome == herdrDefinitelyNotRun {
			return result.Err
		}
		created, err = reconcileCreatedTab(ctx, a.Herdr, HerdrSnapshotScope{WorkspaceLabel: a.Config.WorkspaceLabel}, before, workspaceID, "hq-gateway", a.Office)
		if err != nil {
			return fmt.Errorf("gateway tab create 结果不确定且无法精确 reconcile：%v；%w", result.Err, err)
		}
	}
	if created.Tab.ID == "" || created.Pane.ID == "" {
		return fmt.Errorf("gateway tab create 未返回稳定 tab/pane id")
	}
	serverID, err := newGatewayServerID()
	if err != nil {
		cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
		return combineLifecycleError(err, cleanupErr, created.Tab.ID)
	}
	command := strings.Join([]string{
		shellQuote(canonical), "--direct", "--maintenance-pane", shellQuote(a.MaintenancePane),
		"serve", "--workspace-id", shellQuote(workspaceID), "--server-id", shellQuote(serverID),
	}, " ")
	run := a.Herdr.RunPane(ctx, created.Pane.ID, command)
	if run.Err != nil && run.Outcome == herdrDefinitelyNotRun {
		cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
		return combineLifecycleError(fmt.Errorf("启动 HQ 网关：%w", run.Err), cleanupErr, created.Tab.ID)
	}
	// A newly-created Herdr pane can need several seconds to reach an
	// interactive shell before pane run actually launches the gateway. Keep the
	// wait bounded, but do not misclassify a confirmed queued command as failed
	// after only three seconds and close its tab while it is starting.
	deadline := time.Now().Add(15 * time.Second)
	lastHealth := GatewayHealth{}
	for time.Now().Before(deadline) {
		health := a.GatewayHealth.Ping(ctx, socket, workspaceID)
		lastHealth = health
		if health.OK {
			if health.ServerID != serverID {
				cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
				if cleanupErr != nil {
					return combineLifecycleError(fmt.Errorf("另一正确网关已获胜 server=%s", health.ServerID), cleanupErr, created.Tab.ID)
				}
				fmt.Fprintf(a.Out, "HQ 网关并发启动已收敛到既有实例：server=%s\n", health.ServerID)
				return nil
			}
			fmt.Fprintf(a.Out, "HQ 网关已启动：pane=%s socket=%s server=%s\n", created.Pane.ID, socket, serverID)
			return nil
		}
		if a.Sleep != nil {
			a.Sleep(100 * time.Millisecond)
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	cleanupErr := a.closeOwnedTab(ctx, created.Tab.ID)
	detail := strings.TrimSpace(lastHealth.Error)
	if detail == "" {
		detail = "未收到有效 ping/pong"
	}
	primary := fmt.Errorf("HQ 网关启动结果未能通过版本/workspace/server identity 握手；pane=%s；最后探测=%s；HQ 已回收失败 tab，修复后可直接重试 hq up", created.Pane.ID, detail)
	if run.Err != nil {
		primary = fmt.Errorf("%v：%w", primary, run.Err)
	}
	return combineLifecycleError(primary, cleanupErr, created.Tab.ID)
}
