package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const patrolReportVersion = 3

type PatrolFinding struct {
	Category   string   `json:"category"`
	ObjectID   string   `json:"object_id"`
	Agent      string   `json:"agent,omitempty"`
	TabID      string   `json:"tab_id,omitempty"`
	SignalType string   `json:"signal_type"`
	Message    string   `json:"message"`
	Signals    []string `json:"signals,omitempty"`
	First      []string `json:"first_evidence,omitempty"`
	Second     []string `json:"second_evidence,omitempty"`
	GraceMS    int64    `json:"grace_ms,omitempty"`
}

type PatrolReport struct {
	Version           int             `json:"version"`
	WorkspaceID       string          `json:"workspace_id,omitempty"`
	WorkspaceLabel    string          `json:"workspace_label"`
	GraceMS           int64           `json:"grace_ms"`
	Blocked           int             `json:"blocked"`
	Drift             int             `json:"drift"`
	Orphan            int             `json:"orphan"`
	DeadCandidate     int             `json:"dead_candidate"`
	Frozen            int             `json:"frozen"`
	Stalled           int             `json:"stalled"`
	BusyWithoutAction int             `json:"busy_without_action"`
	Warnings          int             `json:"warnings"`
	Findings          []PatrolFinding `json:"findings"`
}

type PatrolRunner interface {
	Run(context.Context, Config, string, time.Duration) (PatrolReport, error)
}

type PatrolService struct {
	Herdr HerdrControl
	Estop *FileEstopStore
	Store EventStore
	Sleep func(time.Duration)
}

type patrolAnalysis struct {
	report  PatrolReport
	signals map[string]map[string]string
}

func (p *PatrolService) Run(ctx context.Context, cfg Config, hqRoot string, grace time.Duration) (PatrolReport, error) {
	if p == nil || p.Herdr == nil {
		return PatrolReport{}, fmt.Errorf("patrol snapshot provider 未注入")
	}
	frozen := map[string]EstopItem{}
	if p.Estop != nil {
		state, exists, readErr := p.Estop.Read()
		if readErr != nil {
			return PatrolReport{}, fmt.Errorf("patrol 读取 ESTOP 状态失败：%w", readErr)
		}
		if exists {
			if state.State == "active" {
				record, replayErr := replayEstopRecord(p.Store, cfg, state.EstopID)
				if replayErr != nil {
					return PatrolReport{}, fmt.Errorf("patrol 核对 ESTOP 主事件事实失败：%w", replayErr)
				}
				if record.Release.ID != "" || state.ActivationEventID == "" || state.ActivationEventID != record.Activation.ID {
					return PatrolReport{}, fmt.Errorf("patrol ESTOP activation/release sentinel 与主账本冲突")
				}
				if err := validateEstopPlanAgainstState(state, record, cfg); err != nil {
					return PatrolReport{}, fmt.Errorf("patrol ESTOP plan 核对失败：%w", err)
				}
				for _, item := range state.Items {
					if err := validateEstopItemAgainstLedger(item, record); err != nil {
						return PatrolReport{}, fmt.Errorf("patrol ESTOP agent 事实核对失败：%w", err)
					}
				}
				frozen = state.confirmedFrozen()
			}
		}
	}
	scope := HerdrSnapshotScope{WorkspaceLabel: cfg.WorkspaceLabel}
	firstSnapshot, err := p.Herdr.Snapshot(ctx, scope)
	if err != nil {
		return PatrolReport{}, err
	}
	first := analyzePatrolSnapshotWithFrozen(firstSnapshot, cfg, hqRoot, frozen)
	addRuntimeProfilePatrolFindings(ctx, &first, firstSnapshot, cfg, hqRoot, p.Herdr)
	if p.Store != nil {
		events, readErr := p.Store.ReadAll(cfg)
		if readErr != nil {
			return PatrolReport{}, fmt.Errorf("patrol 严格读取 HQ ledger 失败：%w", readErr)
		}
		ledger, replayErr := validateLedger(events, cfg)
		if replayErr != nil {
			return PatrolReport{}, fmt.Errorf("patrol 严格重放 HQ ledger 失败：%w", replayErr)
		}
		if queueErr := addManagerQueuePatrolFindings(&first, firstSnapshot, cfg, hqRoot, ledger, p.Store.NowTime()); queueErr != nil {
			return PatrolReport{}, fmt.Errorf("patrol manager queue 分析失败：%w", queueErr)
		}
		if parkingErr := addManagerParkingPatrolFindings(&first, firstSnapshot, cfg, hqRoot, ledger, p.Store.NowTime()); parkingErr != nil {
			return PatrolReport{}, fmt.Errorf("patrol manager parking 分析失败：%w", parkingErr)
		}
		if progressErr := addAssignmentProgressPatrolFindings(&first, firstSnapshot, cfg, hqRoot, ledger, p.Store.NowTime()); progressErr != nil {
			return PatrolReport{}, fmt.Errorf("patrol assignment progress 分析失败：%w", progressErr)
		}
		if closureErr := addClosureQueuePatrolFinding(&first, firstSnapshot, cfg, hqRoot, ledger, p.Store.NowTime()); closureErr != nil {
			return PatrolReport{}, fmt.Errorf("patrol closure queue 分析失败：%w", closureErr)
		}
	}
	first.report.GraceMS = grace.Milliseconds()
	needsSecond := false
	for _, signals := range first.signals {
		if len(signals) >= 2 {
			needsSecond = true
			break
		}
	}
	if !needsSecond {
		first.report.Warnings = len(first.report.Findings)
		return first.report, nil
	}
	if p.Sleep != nil && grace > 0 {
		p.Sleep(grace)
	}
	secondSnapshot, err := p.Herdr.Snapshot(ctx, scope)
	if err != nil {
		return PatrolReport{}, fmt.Errorf("patrol grace 后第二快照失败：%w", err)
	}
	second := analyzePatrolSnapshotWithFrozen(secondSnapshot, cfg, hqRoot, frozen)
	for objectID, firstSignals := range first.signals {
		secondSignals := second.signals[objectID]
		var persistent, firstEvidence, secondEvidence []string
		for signalType, evidence := range firstSignals {
			if secondValue, ok := secondSignals[signalType]; ok {
				persistent = append(persistent, signalType)
				firstEvidence = append(firstEvidence, evidence)
				secondEvidence = append(secondEvidence, secondValue)
			}
		}
		if len(persistent) < 2 {
			continue
		}
		sort.Strings(persistent)
		sort.Strings(firstEvidence)
		sort.Strings(secondEvidence)
		first.report.Findings = append(first.report.Findings, PatrolFinding{
			Category: "dead_candidate", ObjectID: objectID, SignalType: "persistent_multi_signal",
			Message: "同一稳定对象至少两种独立信号跨 grace 持续；仅标记候选，不自动处置",
			Signals: persistent, First: firstEvidence, Second: secondEvidence, GraceMS: grace.Milliseconds(),
		})
		first.report.DeadCandidate++
	}
	first.report.Warnings = len(first.report.Findings) - first.report.DeadCandidate
	sortPatrolFindings(first.report.Findings)
	return first.report, nil
}

func analyzePatrolSnapshot(snapshot HerdrSnapshot, cfg Config, hqRoot string) patrolAnalysis {
	return analyzePatrolSnapshotWithFrozen(snapshot, cfg, hqRoot, nil)
}

func analyzePatrolSnapshotWithFrozen(snapshot HerdrSnapshot, cfg Config, hqRoot string, frozen map[string]EstopItem) patrolAnalysis {
	report := PatrolReport{Version: patrolReportVersion, WorkspaceLabel: cfg.WorkspaceLabel, Findings: []PatrolFinding{}}
	analysis := patrolAnalysis{report: report, signals: map[string]map[string]string{}}
	frozenTabs := map[string]bool{}
	for name, item := range frozen {
		frozenTabs[item.TabID] = true
		addPatrolFinding(&analysis, PatrolFinding{Category: "frozen", ObjectID: "frozen:" + name, Agent: name, TabID: item.TabID,
			SignalType: "estop_active", Message: "ESTOP active：该子角色已确认冻结；抑制 missing/orphan/dead-candidate，绝不自动重启"})
	}
	workspaceIDs := []string{}
	for _, workspace := range snapshot.Workspaces {
		if workspace.Label == cfg.WorkspaceLabel {
			workspaceIDs = append(workspaceIDs, workspace.ID)
		}
	}
	if len(workspaceIDs) != 1 {
		message := fmt.Sprintf("workspace_label=%s 匹配 %d 个 workspace，要求恰好一个", cfg.WorkspaceLabel, len(workspaceIDs))
		addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: "workspace-label:" + cfg.WorkspaceLabel, SignalType: "workspace_count", Message: message})
		for _, rule := range cfg.Agents {
			if rule.ActivationPolicy == activationAlways && !rule.Disabled {
				addPatrolSignal(&analysis, "roster:"+rule.Name, "missing_agent", "无法在唯一 HQ workspace 中找到在职配置 "+rule.Name)
			}
		}
		analysis.report.Warnings = len(analysis.report.Findings)
		return analysis
	}
	workspaceID := workspaceIDs[0]
	analysis.report.WorkspaceID = workspaceID

	tabs := map[string]HerdrTab{}
	panes := map[string]HerdrPane{}
	for _, tab := range snapshot.Tabs {
		tabs[tab.ID] = tab
	}
	for _, pane := range snapshot.Panes {
		panes[pane.ID] = pane
	}
	agentsByName := map[string][]HerdrAgent{}
	for _, agent := range snapshot.Agents {
		agentsByName[agent.Name] = append(agentsByName[agent.Name], agent)
	}

	for _, rule := range cfg.Agents {
		if rule.Disabled || rule.ActivationPolicy != activationAlways {
			continue
		}
		if _, ok := frozen[rule.Name]; ok {
			continue
		}
		candidates := agentsByName[rule.Name]
		matches := 0
		for _, candidate := range candidates {
			if candidate.WorkspaceID == workspaceID {
				matches++
			}
		}
		if matches == 0 {
			objectID := "roster:" + rule.Name
			message := "在职配置缺席：" + rule.Name
			if len(candidates) != 0 {
				message = "同名 agent 仅存在于错误 workspace：" + rule.Name
				addPatrolSignal(&analysis, objectID, "wrong_workspace", message)
			} else {
				addPatrolSignal(&analysis, objectID, "missing_agent", message)
			}
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: rule.Name, SignalType: "roster_missing", Message: message})
		} else if matches > 1 {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: "roster:" + rule.Name, Agent: rule.Name, SignalType: "duplicate_live", Message: "同一 HQ workspace 内重复 agent"})
		} else if len(candidates) > matches {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: "roster:" + rule.Name, Agent: rule.Name, SignalType: "same_name_cross_workspace", Message: "同名 agent 同时存在于其他 workspace，拒绝误匹配"})
		}
	}

	matchedTabs := map[string]bool{}
	for _, agent := range snapshot.Agents {
		if agent.WorkspaceID != workspaceID {
			continue
		}
		objectID := "agent:" + workspaceID + ":" + agent.Name
		if frozenItem, ok := frozen[agent.Name]; ok {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, TabID: agent.TabID,
				SignalType: "frozen_agent_live", Message: fmt.Sprintf("ESTOP frozen set 中 agent 仍在岗：expected_tab=%s live_tab=%s", frozenItem.TabID, agent.TabID)})
			continue
		}
		rule, exact := configRuleIncludingDisabled(cfg, agent.Name)
		if !exact {
			rule, exact = cfg.ruleFor(agent.Name)
		}
		if !exact {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, SignalType: "unexpected_live", Message: "意外在岗 agent"})
			continue
		}
		if disabled, exists := configRuleIncludingDisabled(cfg, agent.Name); exists && disabled.Disabled {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, SignalType: "disabled_live", Message: "停用员工仍在岗"})
			addPatrolSignal(&analysis, objectID, "disabled_live", "disabled config but live")
			continue
		}
		if agent.Status == "blocked" {
			addPatrolFinding(&analysis, PatrolFinding{Category: "blocked", ObjectID: objectID, Agent: agent.Name, SignalType: "blocked", Message: "agent 工作状态为 blocked；单独不构成死亡"})
			addPatrolSignal(&analysis, objectID, "blocked", "status=blocked")
		} else if !acceptableLiveStatus(agent.Status) {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, SignalType: "bad_status", Message: "agent 状态不可作为已在岗匹配：" + agent.Status})
			addPatrolSignal(&analysis, objectID, "bad_status", "status="+agent.Status)
		}
		if !agent.InteractiveReady {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, SignalType: "interactive_not_ready", Message: "agent interactive_ready=false，不具备下一条原生投递能力"})
			addPatrolSignal(&analysis, objectID, "interactive_not_ready", "interactive_ready=false")
		}
		expectedCWD, workstationErr := resolveAgentWorkstation(hqRoot, rule)
		if workstationErr != nil {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, SignalType: "workstation_invalid", Message: workstationErr.Error()})
			addPatrolSignal(&analysis, objectID, "workstation_invalid", workstationErr.Error())
			continue
		}
		if filepath.Clean(agent.CWD) != expectedCWD {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, SignalType: "cwd_mismatch", Message: fmt.Sprintf("cwd=%s want=%s", agent.CWD, expectedCWD)})
			addPatrolSignal(&analysis, objectID, "cwd_mismatch", "cwd="+agent.CWD)
		}
		if rule.Kind != "" && !cfg.runtimeKindAllowed(rule, agent.Kind) {
			addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, SignalType: "kind_mismatch", Message: fmt.Sprintf("kind=%s want primary=%s or configured fallback", agent.Kind, rule.Kind)})
			addPatrolSignal(&analysis, objectID, "kind_mismatch", "kind="+agent.Kind)
		}
		tab, tabOK := tabs[agent.TabID]
		pane, paneOK := panes[agent.PaneID]
		if !tabOK {
			addPatrolSignal(&analysis, objectID, "missing_tab", "tab_id="+agent.TabID+" missing")
			addPatrolFinding(&analysis, PatrolFinding{Category: "orphan", ObjectID: objectID, Agent: agent.Name, TabID: agent.TabID, SignalType: "missing_tab", Message: "agent 引用的 tab 不存在"})
		} else {
			expectedLabel := rosterTabLabel(rule)
			if tab.WorkspaceID != workspaceID || tab.Label != expectedLabel || filepath.Clean(tab.CWD) != expectedCWD {
				addPatrolSignal(&analysis, objectID, "tab_mismatch", fmt.Sprintf("tab=%s workspace=%s label=%s cwd=%s", tab.ID, tab.WorkspaceID, tab.Label, tab.CWD))
				addPatrolFinding(&analysis, PatrolFinding{Category: "drift", ObjectID: objectID, Agent: agent.Name, TabID: tab.ID, SignalType: "tab_mismatch", Message: "agent tab 的 workspace/label/cwd 与编制不符"})
			}
		}
		if !paneOK {
			addPatrolSignal(&analysis, objectID, "missing_pane", "pane_id="+agent.PaneID+" missing")
			addPatrolFinding(&analysis, PatrolFinding{Category: "orphan", ObjectID: objectID, Agent: agent.Name, TabID: agent.TabID, SignalType: "missing_pane", Message: "agent 引用的 pane 不存在"})
		} else if pane.TabID != agent.TabID || pane.WorkspaceID != workspaceID || filepath.Clean(pane.CWD) != expectedCWD {
			addPatrolSignal(&analysis, objectID, "relation_broken", fmt.Sprintf("pane=%s tab=%s workspace=%s cwd=%s", pane.ID, pane.TabID, pane.WorkspaceID, pane.CWD))
			addPatrolFinding(&analysis, PatrolFinding{Category: "orphan", ObjectID: objectID, Agent: agent.Name, TabID: agent.TabID, SignalType: "relation_broken", Message: "agent/tab/pane 关系断裂"})
		} else if tabOK {
			matchedTabs[tab.ID] = true
		}
	}

	managedRulesByLabel := map[string][]AgentRule{}
	for _, rule := range cfg.Agents {
		label := rosterTabLabel(rule)
		managedRulesByLabel[label] = append(managedRulesByLabel[label], rule)
	}
	for label, rules := range managedRulesByLabel {
		if len(rules) <= 1 {
			continue
		}
		addPatrolFinding(&analysis, PatrolFinding{
			Category: "drift", ObjectID: "roster-label:" + label,
			SignalType: "ambiguous_roster_tab_label",
			Message:    fmt.Sprintf("编制 tab label=%s 被 %d 条规则重复使用；拒绝把孤儿 tab 猜配给任一 agent", label, len(rules)),
		})
	}
	for _, tab := range snapshot.Tabs {
		if frozenTabs[tab.ID] {
			continue
		}
		rules, managed := managedRulesByLabel[tab.Label]
		if tab.WorkspaceID != workspaceID || !managed || matchedTabs[tab.ID] {
			continue
		}
		addPatrolFinding(&analysis, PatrolFinding{Category: "orphan", ObjectID: "tab:" + tab.ID, TabID: tab.ID, SignalType: "orphan_tab", Message: "受 HQ 编制管理但没有匹配 live agent 的 tab"})
		if len(rules) == 1 && rules[0].ActivationPolicy == activationAlways && !rules[0].Disabled {
			addPatrolSignal(&analysis, "roster:"+rules[0].Name, "orphan_tab", fmt.Sprintf("orphan tab_id=%s label=%s", tab.ID, tab.Label))
		}
	}
	analysis.report.Warnings = len(analysis.report.Findings)
	sortPatrolFindings(analysis.report.Findings)
	return analysis
}

func addPatrolSignal(analysis *patrolAnalysis, objectID, signalType, evidence string) {
	if analysis.signals[objectID] == nil {
		analysis.signals[objectID] = map[string]string{}
	}
	analysis.signals[objectID][signalType] = evidence
}

func addPatrolFinding(analysis *patrolAnalysis, finding PatrolFinding) {
	analysis.report.Findings = append(analysis.report.Findings, finding)
	switch finding.Category {
	case "blocked":
		analysis.report.Blocked++
	case "drift":
		analysis.report.Drift++
	case "orphan":
		analysis.report.Orphan++
	case "frozen":
		analysis.report.Frozen++
	case "stalled":
		analysis.report.Stalled++
	case "warning":
		analysis.report.BusyWithoutAction++
	}
}

func addAssignmentProgressPatrolFindings(analysis *patrolAnalysis, snapshot HerdrSnapshot, cfg Config, hqRoot string, ledger *ledgerState, now time.Time) error {
	backlogs, err := ledger.assignmentProgressBacklogs(cfg)
	if err != nil {
		return err
	}
	stallAfter, _, _ := cfg.managerQueueWatchdogPolicy()
	for _, backlog := range backlogs {
		status, statusErr := liveQueueTargetStatus(snapshot, cfg, hqRoot, backlog.Agent)
		if statusErr != nil || (status != "idle" && status != "done") {
			continue
		}
		selectedAt, err := parseOperationsTime("assignment progress selected_at", backlog.SelectedAt)
		if err != nil {
			return err
		}
		if now.Sub(selectedAt) < stallAfter {
			continue
		}
		item := backlog.Items[0]
		signals := make([]string, 0, len(backlog.Items))
		for _, queued := range backlog.Items {
			signals = append(signals, queued.AssignmentID+":"+queued.Status)
		}
		addPatrolFinding(analysis, PatrolFinding{
			Category: "stalled", ObjectID: "assignment-progress:" + backlog.Agent, Agent: backlog.Agent,
			SignalType: "idle_with_active_assignment", Signals: signals,
			Message: fmt.Sprintf("agent status=%s 但仍有 %d 个 accepted/rework assignment 未 report；当前 case=%s status=%s basis=%s；纠正：继续原合同并运行 hq report，受阻必须 report --result blocked",
				status, len(backlog.Items), item.CaseID, item.Status, backlog.BasisEventID),
		})
	}
	analysis.report.Warnings = len(analysis.report.Findings)
	sortPatrolFindings(analysis.report.Findings)
	return nil
}

func addManagerQueuePatrolFindings(analysis *patrolAnalysis, snapshot HerdrSnapshot, cfg Config, hqRoot string, ledger *ledgerState, now time.Time) error {
	backlogs, err := ledger.managerQueueBacklogs(cfg)
	if err != nil {
		return err
	}
	stallAfter, _, _ := cfg.managerQueueWatchdogPolicy()
	for _, backlog := range backlogs {
		status, statusErr := liveQueueTargetStatus(snapshot, cfg, hqRoot, backlog.Manager)
		if statusErr != nil || (status != "idle" && status != "done") {
			continue
		}
		selectedAt, err := parseOperationsTime("manager queue selected_at", backlog.SelectedAt)
		if err != nil {
			return err
		}
		if now.Sub(selectedAt) < stallAfter {
			continue
		}
		item := backlog.Items[0]
		signals := make([]string, 0, len(backlog.Items))
		for _, queued := range backlog.Items {
			if queued.AssignmentID != "" {
				signals = append(signals, queued.AssignmentID+":"+queued.Status)
			} else {
				signals = append(signals, queued.CaseID+":"+queued.Status)
			}
		}
		addPatrolFinding(analysis, PatrolFinding{
			Category: "stalled", ObjectID: "manager-queue:" + backlog.Manager, Agent: backlog.Manager,
			SignalType: "idle_with_actionable_queue", Signals: signals,
			Message: fmt.Sprintf("manager status=%s 但仍有 %d 项 durable 待办；当前最高优先级 case=%s status=%s basis=%s；纠正：%s", status, len(backlog.Items), item.CaseID, item.Status, backlog.BasisEventID, managerQueueAction(item)),
		})
	}
	analysis.report.Warnings = len(analysis.report.Findings)
	sortPatrolFindings(analysis.report.Findings)
	return nil
}

func addManagerParkingPatrolFindings(analysis *patrolAnalysis, snapshot HerdrSnapshot, cfg Config, hqRoot string, ledger *ledgerState, now time.Time) error {
	states, err := ledger.managerParkingStates(cfg)
	if err != nil {
		return err
	}
	warnAfter, _, _ := cfg.managerQueueWatchdogPolicy()
	for _, state := range states {
		status, statusErr := liveQueueTargetStatus(snapshot, cfg, hqRoot, state.Manager)
		if statusErr != nil || status != "working" {
			continue
		}
		selectedAt, err := parseOperationsTime("manager parking selected_at", state.SelectedAt)
		if err != nil {
			return err
		}
		if now.Sub(selectedAt) < warnAfter {
			continue
		}
		signals := make([]string, 0, len(state.Items))
		for _, item := range state.Items {
			signals = append(signals, item.AssignmentID+":"+item.Status)
		}
		addPatrolFinding(analysis, PatrolFinding{
			Category: "warning", ObjectID: "manager-parking:" + state.Manager, Agent: state.Manager,
			SignalType: "manager_busy_without_action", Signals: signals,
			Message: fmt.Sprintf("manager status=working，但当前只有 %d 项直属下属执行责任且没有 durable 经理动作，basis=%s；应结束当前回合并等待 HQ 事件唤醒，禁止 sleep、进程/Herdr 状态或产物轮询；HQ 不会自动中断可能仍有合法工作的 runtime",
				len(state.Items), state.BasisEventID),
		})
	}
	analysis.report.Warnings = len(analysis.report.Findings)
	sortPatrolFindings(analysis.report.Findings)
	return nil
}

func addClosureQueuePatrolFinding(analysis *patrolAnalysis, snapshot HerdrSnapshot, cfg Config, hqRoot string, ledger *ledgerState, now time.Time) error {
	backlog, err := ledger.closureQueueBacklog(cfg)
	if err != nil || backlog == nil {
		return err
	}
	status, statusErr := liveQueueTargetStatus(snapshot, cfg, hqRoot, backlog.Closer)
	if statusErr != nil || (status != "idle" && status != "done") {
		return nil
	}
	selectedAt, err := parseOperationsTime("closure queue selected_at", backlog.SelectedAt)
	if err != nil {
		return err
	}
	stallAfter, _, _ := cfg.managerQueueWatchdogPolicy()
	if now.Sub(selectedAt) < stallAfter {
		return nil
	}
	candidates, omitted := boundedClosureQueueCases(backlog.Items)
	if omitted > 0 {
		candidates += fmt.Sprintf(",...(+%d)", omitted)
	}
	addPatrolFinding(analysis, PatrolFinding{
		Category: "stalled", ObjectID: "closure-queue:" + backlog.Closer, Agent: backlog.Closer,
		SignalType: "idle_with_closure_backlog", Signals: strings.Split(candidates, ","),
		Message: fmt.Sprintf("account_closer status=%s 但仍有 %d 个已验收case满足后序关闭前置；当前case=%s status=%s basis=%s；纠正：运行 hq case show --id %s 与 hq history --case %s，核验依据后显式 hq close；HQ不会自动关闭",
			status, len(backlog.Items), backlog.Items[0].CaseID, backlog.Items[0].Status, backlog.BasisEventID, backlog.Items[0].CaseID, backlog.Items[0].CaseID),
	})
	analysis.report.Warnings = len(analysis.report.Findings)
	sortPatrolFindings(analysis.report.Findings)
	return nil
}

func acceptableLiveStatus(status string) bool {
	switch status {
	case "idle", "working", "done", "blocked":
		return true
	default:
		return false
	}
}

func rosterTabLabel(rule AgentRule) string {
	if rule.Label != "" {
		return rule.Label
	}
	return rule.Name
}

func sortPatrolFindings(findings []PatrolFinding) {
	sort.SliceStable(findings, func(i, j int) bool {
		left := findings[i].Category + "\x00" + findings[i].ObjectID + "\x00" + findings[i].SignalType
		right := findings[j].Category + "\x00" + findings[j].ObjectID + "\x00" + findings[j].SignalType
		return left < right
	})
}

func (a *App) cmdPatrol(args []string) error {
	fs := newLeafParser("patrol")
	fs.SetOutput(a.Err)
	grace := fs.Duration("grace", 2*time.Second, "第二快照前的宽限期")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("用法：hq patrol [--grace 2s] [--json]")
	}
	if a.PatrolRunner == nil {
		return fmt.Errorf("patrol runner 未注入，拒绝回落真实 Herdr")
	}
	report, err := a.PatrolRunner.Run(context.Background(), a.Config, a.HQRoot, *grace)
	if err != nil {
		return err
	}
	if a.JSON {
		encoder := json.NewEncoder(a.Out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	if _, err := fmt.Fprintf(a.Out, "HQ patrol：workspace=%s blocked=%d stalled=%d busy_without_action=%d drift=%d orphan=%d frozen=%d dead_candidate=%d\n", report.WorkspaceID, report.Blocked, report.Stalled, report.BusyWithoutAction, report.Drift, report.Orphan, report.Frozen, report.DeadCandidate); err != nil {
		return err
	}
	for _, finding := range report.Findings {
		if _, err := fmt.Fprintf(a.Out, "%s %s %s：%s\n", strings.ToUpper(finding.Category), finding.ObjectID, finding.SignalType, finding.Message); err != nil {
			return err
		}
	}
	return nil
}
