package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"
)

const initLifecycleVersion = 1

type initLifecycleIntent struct {
	Version               int    `json:"version"`
	Workspace             string `json:"workspace"`
	Owner                 string `json:"owner"`
	ConfigDigest          string `json:"config_digest"`
	DecisionDigest        string `json:"decision_digest"`
	DecisionID            string `json:"decision_id"`
	DecisionRequestDigest string `json:"decision_request_digest"`
	StartedAt             string `json:"started_at"`
}

type initLifecycleCompletion struct {
	Version      int    `json:"version"`
	IntentDigest string `json:"intent_digest"`
	Workspace    string `json:"workspace"`
	WorkspaceID  string `json:"workspace_id"`
	CompletedAt  string `json:"completed_at"`
}

func (a *App) initLifecyclePaths() (directory, intent, completion string) {
	directory = filepath.Join(a.DataDir, "init")
	return directory, filepath.Join(directory, "intent.json"), filepath.Join(directory, "completed.json")
}

func validateInitLifecycleDirectoryIfPresent(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return fmt.Errorf("init lifecycle 目录必须是 canonical、非 symlink、权限 0700 的目录：%s", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil || canonical != path {
		return fmt.Errorf("init lifecycle 目录必须是 canonical、非 symlink 目录：%s", path)
	}
	return nil
}

func (a *App) lockInitLifecycle() (*os.File, error) {
	if err := mkdirDurable(a.DataDir, 0o755); err != nil {
		return nil, err
	}
	path := filepath.Join(a.DataDir, ".hq-init-lifecycle.lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建 init lifecycle 锁：%w", err)
	}
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		file.Close()
		return nil, fmt.Errorf("init lifecycle 锁必须是权限 0600 的普通文件：%s", path)
	}
	if err := flockContext(a.requestContext(), int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("等待 init lifecycle 锁：%w", err)
	}
	return file, nil
}

func readInitLifecycleRecord(path string, target any) ([]byte, bool, error) {
	raw, exists, err := readRegularFileIfExists(path)
	if err != nil || !exists {
		return raw, exists, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, false, err
	}
	if info.Mode().Perm() != 0o600 {
		return nil, false, fmt.Errorf("init lifecycle 记录权限必须是 0600：%s", path)
	}
	if err := decodeStrictJSON(raw, target); err != nil {
		return nil, false, fmt.Errorf("init lifecycle 记录无效 %s：%w", path, err)
	}
	return raw, true, nil
}

func writeInitLifecycleRecord(path string, value any) ([]byte, error) {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	raw = append(raw, '\n')
	created, err := createFileNoReplaceMode(path, raw, 0o600)
	if err != nil {
		return nil, err
	}
	if !created {
		return nil, fmt.Errorf("init lifecycle 记录已存在，拒绝覆盖：%s", path)
	}
	return raw, nil
}

func (a *App) currentInitIntent() (initLifecycleIntent, error) {
	configPath, err := canonicalExistingRegularFile(a.ConfigPath, "HQ config")
	if err != nil {
		return initLifecycleIntent{}, err
	}
	configRaw, err := os.ReadFile(configPath)
	if err != nil {
		return initLifecycleIntent{}, err
	}
	decisionPath := filepath.Join(a.Office, "decisions", "company-init.md")
	canonical, metadata, err := readApproval(decisionPath, a.Office, a.Config.ownerPrincipal(), true)
	if err != nil {
		return initLifecycleIntent{}, fmt.Errorf("公司首次启动要求有效的 company-init 决策：%w", err)
	}
	if len(metadata.Scopes) != 1 || metadata.Scopes[0].Action != "company:init" || metadata.Scopes[0].Target != a.Config.WorkspaceLabel {
		return initLifecycleIntent{}, fmt.Errorf("公司成立决策必须只含一个 company:init scope，target 必须精确为 %s", a.Config.WorkspaceLabel)
	}
	decisionRaw, err := os.ReadFile(canonical)
	if err != nil {
		return initLifecycleIntent{}, err
	}
	return initLifecycleIntent{
		Version: initLifecycleVersion, Workspace: a.Config.WorkspaceLabel, Owner: a.Config.ownerPrincipal(),
		ConfigDigest: digestBytes(configRaw), DecisionDigest: digestBytes(decisionRaw), DecisionID: metadata.DecisionID,
		DecisionRequestDigest: metadata.Scopes[0].RequestDigest,
	}, nil
}

func sameInitContract(left, right initLifecycleIntent) bool {
	left.StartedAt, right.StartedAt = "", ""
	return left == right
}

func validateInitIntent(intent initLifecycleIntent) error {
	if intent.Version != initLifecycleVersion || !agentNamePattern.MatchString(intent.Workspace) {
		return fmt.Errorf("init intent 版本或 workspace 无效")
	}
	if err := validateOwnerPrincipal(intent.Owner); err != nil {
		return fmt.Errorf("init intent owner：%w", err)
	}
	if !sha256Pattern.MatchString(intent.ConfigDigest) || !sha256Pattern.MatchString(intent.DecisionDigest) ||
		!sha256Pattern.MatchString(intent.DecisionRequestDigest) || !decisionIDPattern.MatchString(intent.DecisionID) {
		return fmt.Errorf("init intent digest 或 decision_id 无效")
	}
	if _, err := time.Parse(time.RFC3339, intent.StartedAt); err != nil {
		return fmt.Errorf("init intent started_at 无效：%w", err)
	}
	return nil
}

func validateInitCompletion(completion initLifecycleCompletion, intentRaw []byte) error {
	if completion.Version != initLifecycleVersion || completion.IntentDigest != digestBytes(intentRaw) ||
		!agentNamePattern.MatchString(completion.Workspace) || completion.WorkspaceID == "" {
		return fmt.Errorf("init completion 与 intent 不匹配或字段无效")
	}
	if _, err := time.Parse(time.RFC3339, completion.CompletedAt); err != nil {
		return fmt.Errorf("init completion completed_at 无效：%w", err)
	}
	return nil
}

func (a *App) requireCompletedInit() error {
	directory, intentPath, completionPath := a.initLifecyclePaths()
	if err := validateInitLifecycleDirectoryIfPresent(directory); err != nil {
		return err
	}
	var intent initLifecycleIntent
	intentRaw, exists, err := readInitLifecycleRecord(intentPath, &intent)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("公司尚未完成首次初始化；请在宿主机运行 hq init %s", a.HQRoot)
	}
	if err := validateInitIntent(intent); err != nil {
		return err
	}
	var completion initLifecycleCompletion
	_, exists, err = readInitLifecycleRecord(completionPath, &completion)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("公司首次初始化尚未完成；请在宿主机重跑 hq init %s 续跑，不能用 hq up 旁路", a.HQRoot)
	}
	if err := validateInitCompletion(completion, intentRaw); err != nil {
		return err
	}
	if completion.Workspace != a.Config.WorkspaceLabel || intent.Workspace != a.Config.WorkspaceLabel {
		return fmt.Errorf("init completion workspace=%s 与当前 registry=%s 不一致", completion.Workspace, a.Config.WorkspaceLabel)
	}
	if intent.Owner != a.Config.ownerPrincipal() {
		return fmt.Errorf("init intent owner=%s 与当前 registry owner=%s 不一致", intent.Owner, a.Config.ownerPrincipal())
	}
	return nil
}

func (a *App) firstInitPreflight(ctx context.Context) error {
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return fmt.Errorf("首次初始化前读取业务账本：%w", err)
	}
	if len(events) != 0 {
		return fmt.Errorf("首次初始化要求业务账本为空，实际 events=%d；拒绝把已有公司伪装成未初始化", len(events))
	}
	sessions, err := a.Sessions.List(SessionFilter{})
	if err != nil {
		return fmt.Errorf("首次初始化前读取 session lifecycle：%w", err)
	}
	if len(sessions) != 0 {
		return fmt.Errorf("首次初始化要求 session lifecycle 为空，实际 events=%d", len(sessions))
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == a.Config.WorkspaceLabel {
			return fmt.Errorf("首次初始化要求不存在同名 Herdr workspace，实际已存在 %s；如这是失败续跑，缺失的 init intent 表明状态不可信", workspace.ID)
		}
	}
	return nil
}

func (a *App) validateInitResumeState(ctx context.Context) error {
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return err
	}
	if len(events) != 0 {
		return fmt.Errorf("首次初始化续跑期间业务账本不再为空（events=%d），拒绝继续扩大运行态", len(events))
	}
	allowed := map[string]AgentRule{}
	for _, rule := range a.Config.Agents {
		if !rule.Disabled && rule.ActivationPolicy == activationAlways {
			allowed[rule.Name] = rule
		}
	}
	sessions, err := a.Sessions.List(SessionFilter{})
	if err != nil {
		return err
	}
	for _, event := range sessions {
		if (event.Type != sessionStarted && event.Type != sessionStopped) || event.Actor != "hq-init" {
			return fmt.Errorf("首次初始化续跑只接受 actor=hq-init 的 started/stopped session，发现 actor=%s type=%s", event.Actor, event.Type)
		}
		if _, ok := allowed[event.Agent]; !ok {
			return fmt.Errorf("首次初始化 session 指向非 always 编制：%s", event.Agent)
		}
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
			return fmt.Errorf("workspace label %s 匹配多个稳定 ID", a.Config.WorkspaceLabel)
		}
		workspaceID = workspace.ID
	}
	if len(sessions) != 0 && workspaceID == "" {
		return fmt.Errorf("首次初始化已有 session 证据但目标 workspace 已不存在；拒绝把新 workspace 当作原运行态")
	}
	for _, event := range sessions {
		if event.WorkspaceID != workspaceID {
			return fmt.Errorf("首次初始化 session workspace=%s 与目标 workspace=%s 不一致", event.WorkspaceID, workspaceID)
		}
	}
	allowedTabs := map[string]bool{"hq-gateway": true}
	for _, rule := range allowed {
		allowedTabs[rosterTabLabel(rule)] = true
	}
	gatewayTabs := 0
	for _, tab := range snapshot.Tabs {
		workspaceRoot := tab.WorkspaceID == workspaceID && tab.Number == 1 && tab.Label == "1"
		if workspaceID != "" && tab.WorkspaceID == workspaceID && !workspaceRoot && !allowedTabs[tab.Label] {
			return fmt.Errorf("首次初始化 workspace 中出现非预期 tab %q；拒绝接管", tab.Label)
		}
		if workspaceID != "" && tab.WorkspaceID == workspaceID && tab.Label == "hq-gateway" {
			gatewayTabs++
		}
	}
	if gatewayTabs > 1 {
		return fmt.Errorf("首次初始化 workspace 中出现 %d 个 hq-gateway tab；拒绝任选其一", gatewayTabs)
	}
	if gatewayTabs == 1 {
		socket, socketErr := gatewaySocketPath(a.DataDir)
		if socketErr != nil {
			return socketErr
		}
		if health := a.GatewayHealth.Ping(ctx, socket, workspaceID); !health.OK {
			return fmt.Errorf("首次初始化遗留离线 hq-gateway tab；先核对并关闭该 orphan 后重跑 init")
		}
	}
	active := activeSessionStarts(sessions)
	for _, agent := range snapshot.Agents {
		if workspaceID != "" && agent.WorkspaceID == workspaceID {
			if _, ok := allowed[agent.Name]; !ok {
				return fmt.Errorf("首次初始化 workspace 中出现非 always agent %q；拒绝接管", agent.Name)
			}
			matchedSession := false
			for _, event := range active {
				if event.Agent == agent.Name && event.WorkspaceID == agent.WorkspaceID && event.TabID == agent.TabID && event.PaneID == agent.PaneID {
					matchedSession = true
					break
				}
			}
			if !matchedSession {
				return fmt.Errorf("首次初始化发现没有 hq-init session 证据的 live agent %q；拒绝接管", agent.Name)
			}
		}
	}
	return nil
}

func (a *App) validateInitFinalState(ctx context.Context, workspaceID string) error {
	if err := a.validateInitResumeState(ctx); err != nil {
		return err
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	for _, rule := range a.Config.Agents {
		if rule.Disabled || rule.ActivationPolicy != activationAlways {
			continue
		}
		matched, mismatch := exactLiveMatch(snapshot, workspaceID, rule, a.HQRoot)
		if !matched {
			return fmt.Errorf("首次初始化终态缺少 always 员工 %s：%s", rule.Name, mismatch)
		}
	}
	socket, err := gatewaySocketPath(a.DataDir)
	if err != nil {
		return err
	}
	health := a.GatewayHealth.Ping(ctx, socket, workspaceID)
	if !health.OK {
		return fmt.Errorf("首次初始化终态 gateway 未通过握手：%s", health.Error)
	}
	return nil
}

// completeFirstInit owns the one-time formation channel. It is resumable only
// while the frozen intent matches the current registry and formation decision.
// Once completed.json exists, init becomes observational and hq up owns future
// cold starts.
func (a *App) completeFirstInit(ctx context.Context) (bool, error) {
	lock, err := a.lockInitLifecycle()
	if err != nil {
		return false, err
	}
	defer unlock(lock)
	directory, intentPath, completionPath := a.initLifecyclePaths()
	if err := validateInitLifecycleDirectoryIfPresent(directory); err != nil {
		return false, err
	}
	var intent initLifecycleIntent
	intentRaw, hasIntent, err := readInitLifecycleRecord(intentPath, &intent)
	if err != nil {
		return false, err
	}
	var completion initLifecycleCompletion
	_, hasCompletion, err := readInitLifecycleRecord(completionPath, &completion)
	if err != nil {
		return false, err
	}
	if hasCompletion {
		if !hasIntent {
			return false, fmt.Errorf("init completion 存在但 intent 缺失，拒绝推断")
		}
		if err := validateInitIntent(intent); err != nil {
			return false, err
		}
		if err := validateInitCompletion(completion, intentRaw); err != nil {
			return false, err
		}
		if completion.Workspace != intent.Workspace || completion.Workspace != a.Config.WorkspaceLabel || intent.Owner != a.Config.ownerPrincipal() {
			return false, fmt.Errorf("已完成的 init lifecycle 与当前 registry workspace/owner 不一致")
		}
		fmt.Fprintln(a.Out, "HQ init：首次初始化早已完成；本次未修改运行态。后续冷启动请使用 hq up。")
		return false, nil
	}
	current, err := a.currentInitIntent()
	if err != nil {
		return false, err
	}
	if hasIntent {
		if err := validateInitIntent(intent); err != nil {
			return false, err
		}
		if !sameInitContract(intent, current) {
			return false, fmt.Errorf("未完成的 init intent 与当前配置或公司成立决策不一致；恢复原始文件后重跑 hq init，禁止改约续跑")
		}
	} else {
		if err := a.firstInitPreflight(ctx); err != nil {
			return false, err
		}
		if err := mkdirDurable(directory, 0o700); err != nil {
			return false, err
		}
		info, err := os.Lstat(directory)
		if err != nil || info.Mode().Perm() != 0o700 {
			return false, fmt.Errorf("init lifecycle 目录权限必须是 0700：%s", directory)
		}
		intent = current
		intent.StartedAt = a.now().Format(time.RFC3339)
		intentRaw, err = writeInitLifecycleRecord(intentPath, intent)
		if err != nil {
			return false, err
		}
	}
	if err := a.validateInitResumeState(ctx); err != nil {
		return false, err
	}
	workspaceID, err := a.startCompanyControlPlane(ctx, "hq-init")
	if err != nil {
		return false, err
	}
	if err := a.validateInitFinalState(ctx, workspaceID); err != nil {
		return false, err
	}
	completion = initLifecycleCompletion{Version: initLifecycleVersion, IntentDigest: digestBytes(intentRaw), Workspace: a.Config.WorkspaceLabel,
		WorkspaceID: workspaceID, CompletedAt: a.now().Format(time.RFC3339)}
	if _, err := writeInitLifecycleRecord(completionPath, completion); err != nil {
		return false, err
	}
	return true, nil
}

func (a *App) startCompanyControlPlane(ctx context.Context, actor string) (string, error) {
	lock, err := a.lockUp()
	if err != nil {
		return "", err
	}
	upLocked := true
	defer func() {
		if upLocked {
			unlock(lock)
		}
	}()
	workspaceID, err := a.ensureHQWorkspace(ctx)
	if err != nil {
		return "", err
	}
	if actor == "hq-init" || actor == "hq-up-host" {
		if err := a.reconcileStartupAbsentAgents(ctx, workspaceID, actor, actor == "hq-init"); err != nil {
			return "", err
		}
	}
	witness, err := a.Config.approvalWitness()
	if err != nil {
		return "", err
	}
	a.MaintenanceActor = actor
	if err := a.startInitAgentIfNeeded(ctx, workspaceID, witness); err != nil {
		return "", err
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return "", err
	}
	for _, agent := range snapshot.Agents {
		if agent.Name == witness.Name && agent.WorkspaceID == workspaceID {
			a.MaintenancePane = agent.PaneID
			break
		}
	}
	if a.MaintenancePane == "" {
		return "", fmt.Errorf("总部联系职责位 %s 启动后未找到稳定 pane，拒绝启动网关", witness.Name)
	}
	if err := a.closeHerdrWorkspaceRootTab(ctx, workspaceID); err != nil {
		return "", err
	}
	// The gateway reconciles durable delivery before binding its socket and may
	// need the same up lock to cold-resume a target. Release the parent lock while
	// waiting, and serialize competing gateway parents with its dedicated lock.
	unlock(lock)
	upLocked = false
	gatewayLock, err := a.lockGatewayUp()
	if err != nil {
		return "", err
	}
	gatewayErr := a.ensureHQGateway(ctx, workspaceID)
	unlock(gatewayLock)
	if gatewayErr != nil {
		return "", gatewayErr
	}
	lock, err = a.lockUp()
	if err != nil {
		return "", err
	}
	upLocked = true
	var remaining []AgentRule
	for _, rule := range a.Config.Agents {
		if !rule.Disabled && rule.ActivationPolicy == activationAlways && rule.Name != witness.Name {
			remaining = append(remaining, rule)
		}
	}
	sort.Slice(remaining, func(i, j int) bool { return remaining[i].Name < remaining[j].Name })
	for _, rule := range remaining {
		if err := a.startInitAgentIfNeeded(ctx, workspaceID, rule); err != nil {
			return "", err
		}
	}
	return workspaceID, nil
}

func (a *App) reconcileStartupAbsentAgents(ctx context.Context, workspaceID, actor string, initOnly bool) error {
	events, err := a.Sessions.List(SessionFilter{})
	if err != nil {
		return err
	}
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	activeByID := map[string]SessionEvent{}
	for _, started := range activeSessionStarts(events) {
		activeByID[started.SessionID] = started
	}
	stopped, err := a.reconcileAbsentRuntimeSessions(events, snapshot, actor)
	if err != nil {
		return err
	}
	for _, sessionID := range stopped {
		started := activeByID[sessionID]
		var tab *HerdrTab
		for index := range snapshot.Tabs {
			if snapshot.Tabs[index].ID == started.TabID {
				tab = &snapshot.Tabs[index]
				break
			}
		}
		if tab == nil {
			fmt.Fprintf(a.Out, "已收敛消失的 startup session：agent=%s session=%s\n", started.Agent, started.SessionID)
			continue
		}
		if started.WorkspaceID != workspaceID || tab.WorkspaceID != workspaceID {
			return fmt.Errorf("startup stale tab workspace=%s/session workspace=%s 与目标 %s 不一致", tab.WorkspaceID, started.WorkspaceID, workspaceID)
		}
		rule, ok := configRuleIncludingDisabled(a.Config, started.Agent)
		if !ok || (initOnly && (rule.Disabled || rule.ActivationPolicy != activationAlways)) {
			return fmt.Errorf("startup stale session 指向不可由当前启动路径回收的编制：%s", started.Agent)
		}
		cwd, err := resolveAgentWorkstation(a.HQRoot, rule)
		if err != nil || tab.WorkspaceID != workspaceID || tab.Label != rosterTabLabel(rule) || filepath.Clean(tab.CWD) != filepath.Clean(cwd) {
			return fmt.Errorf("startup stale session tab 不再满足冻结 seat 合同：agent=%s tab=%s", started.Agent, started.TabID)
		}
		for _, agent := range snapshot.Agents {
			if agent.TabID == tab.ID {
				return fmt.Errorf("startup stale session tab=%s 已被 live agent %s 占用，拒绝回收", tab.ID, agent.Name)
			}
		}
		if err := a.closeOwnedTab(ctx, tab.ID); err != nil {
			return fmt.Errorf("回收 startup stale tab %s：%w", tab.ID, err)
		}
		fmt.Fprintf(a.Out, "已收敛消失的 startup session 并回收 stale tab：agent=%s tab=%s\n", started.Agent, tab.ID)
	}
	return nil
}

func (a *App) closeHerdrWorkspaceRootTab(ctx context.Context, workspaceID string) error {
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	var candidates []HerdrTab
	for _, tab := range snapshot.Tabs {
		if tab.WorkspaceID == workspaceID && tab.Number == 1 && tab.Label == "1" {
			candidates = append(candidates, tab)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	if len(candidates) != 1 {
		return fmt.Errorf("Herdr workspace root tab 候选=%d，拒绝任选其一", len(candidates))
	}
	root := candidates[0]
	paneCount := 0
	for _, pane := range snapshot.Panes {
		if pane.TabID == root.ID {
			paneCount++
		}
	}
	for _, agent := range snapshot.Agents {
		if agent.TabID == root.ID {
			return fmt.Errorf("Herdr workspace root tab %s 已被 agent %s 占用，拒绝回收", root.ID, agent.Name)
		}
	}
	if paneCount != 1 {
		return fmt.Errorf("Herdr workspace root tab %s pane_count=%d，拒绝回收", root.ID, paneCount)
	}
	if err := a.closeOwnedTab(ctx, root.ID); err != nil {
		return fmt.Errorf("回收 Herdr 自动创建的 root tab %s：%w", root.ID, err)
	}
	fmt.Fprintf(a.Out, "已回收 Herdr 自动创建的空 root tab：%s\n", root.ID)
	return nil
}
