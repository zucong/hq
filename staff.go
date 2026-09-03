package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
)

func (a *App) registryConfigWriteOptions(subject string) configWriteOptions {
	return configWriteOptions{
		context: a.requestContext(),
		dryRun:  a.DryRun,
		candidateGuard: func(candidate *Config) (func(), error) {
			store, ok := a.Store.(candidateConfigReplayStore)
			if !ok {
				return nil, fmt.Errorf("事件存储不支持候选 config 的锁定严格 replay，拒绝修改%s", subject)
			}
			if a.DryRun {
				if err := store.ReplayCandidateReadOnly(*candidate); err != nil {
					return nil, err
				}
				return func() {}, nil
			}
			return store.LockAndReplayCandidate(*candidate)
		},
	}
}

func (a *App) staffConfigWriteOptions() configWriteOptions {
	return a.registryConfigWriteOptions("员工编制")
}

type staffCapacityView struct {
	AgentRule
	ActiveWIP    int `json:"active_wip"`
	AvailableWIP int `json:"available_wip"`
}

func staffCapacity(rule AgentRule, ledger *ledgerState) staffCapacityView {
	active := ledger.assignmentCapacityUsed(rule.Name)
	available := rule.MaxWIP - active
	if available < 0 {
		available = 0
	}
	return staffCapacityView{AgentRule: rule, ActiveWIP: active, AvailableWIP: available}
}

func (a *App) cmdStaff(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq staff list|get|add|update|remove")
	}
	switch args[0] {
	case "list":
		return a.cmdStaffList(args[1:])
	case "get":
		return a.cmdStaffGet(args[1:])
	case "add":
		return a.cmdStaffAdd(args[1:])
	case "update":
		return a.cmdStaffUpdate(args[1:])
	case "remove":
		return a.cmdStaffRemove(args[1:])
	default:
		return fmt.Errorf("未知 staff 子命令 %q", args[0])
	}
}

func (a *App) cmdStaffList(args []string) error {
	fs := newLeafParser("staff list")
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "包含已停用员工")
	reportsTo := fs.String("reports-to", "", "仅列出直属上级为该 agent slug 的员工")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cleanReportsTo := strings.TrimSpace(*reportsTo)
	staff := append([]AgentRule(nil), a.Config.Agents...)
	sort.Slice(staff, func(i, j int) bool {
		if staff[i].Department == staff[j].Department {
			return staff[i].Name < staff[j].Name
		}
		return staff[i].Department < staff[j].Department
	})
	selected := staff[:0]
	for _, rule := range staff {
		if !*all && rule.Disabled {
			continue
		}
		if cleanReportsTo != "" && rule.ReportsTo != cleanReportsTo {
			continue
		}
		selected = append(selected, rule)
	}
	ledger, err := a.strictLedgerStateReadOnly()
	if err != nil {
		return fmt.Errorf("staff list 无法严格重放 ledger 以计算实时 WIP：%w", err)
	}
	views := make([]staffCapacityView, 0, len(selected))
	for _, rule := range selected {
		views = append(views, staffCapacity(rule, ledger))
	}
	if a.JSON {
		return a.output(views, "")
	}
	fmt.Fprintf(a.Out, "%-24s %-24s %-14s %-14s %-10s %-7s %-13s %-18s %s\n", "SLUG", "ROLE", "DEPARTMENT", "ACTIVATION", "ACTIVE_WIP", "MAX_WIP", "AVAILABLE_WIP", "REPORTS_TO", "SENDER")
	for _, view := range views {
		rule := view.AgentRule
		status := ""
		if rule.Disabled {
			status = " [disabled]"
		}
		fmt.Fprintf(a.Out, "%-24s %-24s %-14s %-14s %-10d %-7d %-13d %-18s %s%s\n", rule.Name, roleCardKey(rule.RoleCardID, rule.RoleCardVersion), rule.Department, rule.ActivationPolicy, view.ActiveWIP, rule.MaxWIP, view.AvailableWIP, rule.ReportsTo, rule.Label, status)
	}
	return nil
}

func (a *App) cmdStaffGet(args []string) error {
	fs := newLeafParser("staff get")
	fs.SetOutput(a.Err)
	name := fs.String("name", "", "稳定 agent slug")
	if err := fs.Parse(args); err != nil {
		return err
	}
	rule, ok := configRuleIncludingDisabled(a.Config, strings.TrimSpace(*name))
	if !ok {
		return fmt.Errorf("员工未登记：%s", *name)
	}
	ledger, err := a.strictLedgerStateReadOnly()
	if err != nil {
		return fmt.Errorf("staff get 无法严格重放 ledger 以计算实时 WIP：%w", err)
	}
	view := staffCapacity(rule, ledger)
	keepWarm, _ := effectiveSeatKeepWarm(rule)
	return a.output(view, fmt.Sprintf("%s：sender=[%s] role=%s department=%s workstation=%s activation=%s keep_warm=%s active_wip=%d max_wip=%d available_wip=%d kind=%s reports_to=%s disabled=%t", rule.Name, rule.Label, roleCardKey(rule.RoleCardID, rule.RoleCardVersion), rule.Department, rule.WorkstationPath, rule.ActivationPolicy, keepWarm, view.ActiveWIP, rule.MaxWIP, view.AvailableWIP, rule.Kind, rule.ReportsTo, rule.Disabled))
}

func (a *App) staffMutationActor() (Actor, error) {
	actor, err := a.actor()
	if err != nil {
		return Actor{}, err
	}
	if !actor.Rule.CanManageStaff {
		return Actor{}, permissionf("当前 agent %s 无权招人或调整编制：部门经理不能自行招人；人事决定属于公司所有者，并只能由 config 中具备 can_manage_staff 的总裁办秘书凭有效 approval 执行", actor.Name)
	}
	return actor, nil
}

func bindEmployeeSeat(cfg Config, hqRoot string, rule *AgentRule, roleRef, workstation, activation, keepWarm string, maxWIP int) error {
	id, version, err := parseRoleCardRef(roleRef)
	if err != nil {
		return err
	}
	card, ok := cfg.roleCard(id, version)
	if !ok {
		return fmt.Errorf("role card 未登记：%s", roleCardKey(id, version))
	}
	if card.Status != roleCardApproved {
		return fmt.Errorf("role card 不是 approved，不得绑定在职 seat：%s", roleCardKey(id, version))
	}
	if card.Department != rule.Department {
		return fmt.Errorf("role card %s 属于部门 %s，不得绑定到 %s", roleCardKey(id, version), card.Department, rule.Department)
	}
	if _, err := verifyRoleCardArtifact(hqRoot, card); err != nil {
		return err
	}
	cleanWorkstation, err := cleanRegistryRelativePath(strings.TrimSpace(workstation), "workstation")
	if err != nil {
		return err
	}
	if err := pathUnderDepartment(cleanWorkstation, rule.Department, "workstation"); err != nil {
		return err
	}
	if err := validatePersonalWorkstationPath(rule.Department, cleanWorkstation, card.Version); err != nil {
		return err
	}
	if filepath.Clean(card.ManualPath) != filepath.Join(cleanWorkstation, "AGENTS.md") {
		return fmt.Errorf("workstation %s 必须与 role card %s 的独立手册目录 %s 一致", cleanWorkstation, roleCardKey(id, version), filepath.Dir(card.ManualPath))
	}
	activation = strings.TrimSpace(activation)
	if !validActivationPolicy(activation) {
		return fmt.Errorf("--activation 必须是 always|on_assignment|manual")
	}
	if maxWIP < 1 || maxWIP > 16 {
		return fmt.Errorf("--max-wip 必须在 1..16")
	}

	rule.RoleCardID, rule.RoleCardVersion, rule.RoleCardDigest = card.ID, card.Version, card.Digest
	rule.ManualPath, rule.WorkstationPath = card.ManualPath, cleanWorkstation
	rule.ActivationPolicy, rule.KeepWarm, rule.MaxWIP = activation, strings.TrimSpace(keepWarm), maxWIP
	if _, err := effectiveSeatKeepWarm(*rule); err != nil {
		return err
	}
	if _, err := resolveAgentWorkstation(hqRoot, *rule); err != nil {
		return err
	}
	return nil
}

func (a *App) cmdStaffAdd(args []string) error {
	fs := newLeafParser("staff add")
	fs.SetOutput(a.Err)
	name := fs.String("name", "", "稳定 agent slug")
	label := fs.String("label", "", "句头发件标识（不含方括号）")
	department := fs.String("department", "", "所属部门")
	kind := fs.String("kind", "", "herdr 启动 kind")
	permissionMode := fs.String("permission-mode", "native", "native|yolo")
	reportsTo := fs.String("reports-to", "", "直属上级 slug")
	roleRef := fs.String("role", "", "不可变 role card id@version")
	workstation := fs.String("workstation", "", "独立员工工位目录")
	activation := fs.String("activation", activationOnAssignment, "always|on_assignment|manual")
	keepWarm := fs.String("keep-warm", "", "on_assignment 完成后的有界保温时长（默认 30s，0s 立即休眠）")
	maxWIPText := fs.String("max-wip", "1", "最大并行在办 case（1..16）")
	grant := fs.String("grant", "", "逗号分隔权限")
	approval := fs.String("approval", "", "生效 decisions 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.staffMutationActor()
	if err != nil {
		return err
	}
	cleanName := strings.TrimSpace(*name)
	if cleanName == "" {
		return fmt.Errorf("缺少 --name")
	}
	cleanLabel, err := validateShortText("label", *label, true)
	if err != nil {
		return err
	}
	cleanDepartment := strings.TrimSpace(*department)
	if !safeDepartment(cleanDepartment) {
		return fmt.Errorf("--department 必须是总部直属目录名")
	}
	if strings.TrimSpace(*roleRef) == "" {
		return fmt.Errorf("缺少 --role")
	}
	if strings.TrimSpace(*workstation) == "" {
		return fmt.Errorf("缺少 --workstation")
	}
	maxWIP, err := strconv.Atoi(strings.TrimSpace(*maxWIPText))
	if err != nil {
		return fmt.Errorf("--max-wip 必须是整数")
	}
	activationPolicy := strings.TrimSpace(*activation)
	rule := AgentRule{
		Name: cleanName, Label: cleanLabel, Department: cleanDepartment,
		Kind: strings.TrimSpace(*kind), PermissionMode: strings.TrimSpace(*permissionMode), ReportsTo: strings.TrimSpace(*reportsTo),
	}
	rule.Workspace = a.Config.WorkspaceLabel
	rule.Nickname, rule.DepartmentLabel = cleanLabel, cleanDepartment
	rule.Responsibilities = []string{"staff:" + cleanName}
	if rule.Kind == "" {
		return fmt.Errorf("缺少 --kind")
	}
	if rule.PermissionMode != "native" && rule.PermissionMode != "yolo" {
		return fmt.Errorf("--permission-mode 必须是 native|yolo")
	}
	if err := applyPermissions(&rule, *grant, true); err != nil {
		return err
	}
	var cleanApproval string
	cfg, err := mutateConfigWithOptions(a.ConfigPath, a.staffConfigWriteOptions(), func(cfg *Config) error {
		liveActor, ok := cfg.exactRule(actor.Name)
		if !ok || liveActor.Disabled || !liveActor.CanManageStaff {
			return fmt.Errorf("当前 agent %s 已失去实时 can_manage_staff 权限", actor.Name)
		}
		if _, exists := configRuleIncludingDisabled(*cfg, rule.Name); exists {
			return fmt.Errorf("员工已登记：%s", rule.Name)
		}
		if err := bindEmployeeSeat(*cfg, a.HQRoot, &rule, *roleRef, *workstation, activationPolicy, *keepWarm, maxWIP); err != nil {
			return err
		}
		rule.SeatVersion = 1
		rule.SeatDigest = employeeSeatDigest(rule)
		var err error
		cleanApproval, err = validateApprovalScope(*approval, a.Office, cfg.ownerPrincipal(), ApprovalScope{
			Action: "staff:add", Target: rule.Name, RequestDigest: staffScopeDigest("staff:add", rule),
		})
		if err != nil {
			return fmt.Errorf("approval：%w", err)
		}
		rule.ApprovalRef, rule.UpdatedAt = cleanApproval, time.Now().UTC().Format(time.RFC3339)
		cfg.Agents = append(cfg.Agents, rule)
		return nil
	})
	if err != nil {
		return err
	}
	if a.DryRun {
		return a.output(rule, fmt.Sprintf("DRY-RUN：将新增员工 %s", rule.Name))
	}
	a.Config = cfg
	return a.output(rule, fmt.Sprintf("已新增员工 %s；配置=%s；批准=%s", rule.Name, a.ConfigPath, cleanApproval))
}

func (a *App) cmdStaffUpdate(args []string) error {
	fs := newLeafParser("staff update")
	fs.SetOutput(a.Err)
	name := fs.String("name", "", "稳定 agent slug（不可原地改名）")
	label := fs.String("label", "", "新发件标识")
	department := fs.String("department", "", "新工位目录")
	kind := fs.String("kind", "", "新启动 kind")
	permissionMode := fs.String("permission-mode", "", "native|yolo")
	reportsTo := fs.String("reports-to", "", "新直属上级；- 表示清空")
	roleRef := fs.String("role", "", "新 role card id@version")
	workstation := fs.String("workstation", "", "新独立员工工位目录")
	activation := fs.String("activation", "", "always|on_assignment|manual")
	keepWarm := fs.String("keep-warm", "", "on_assignment 有界保温时长；- 表示清空为默认")
	maxWIPText := fs.String("max-wip", "", "最大并行在办 case（1..16）")
	grant := fs.String("grant", "", "新增权限，逗号分隔")
	revoke := fs.String("revoke", "", "撤销权限，逗号分隔")
	enable := fs.Bool("enable", false, "重新启用")
	disable := fs.Bool("disable", false, "停用")
	approval := fs.String("approval", "", "生效 decisions 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.staffMutationActor()
	if err != nil {
		return err
	}
	if *enable && *disable {
		return fmt.Errorf("--enable 与 --disable 不能同时使用")
	}
	changed := map[string]bool{}
	fs.Visit(func(f *pflag.Flag) { changed[f.Name] = true })
	cleanName := strings.TrimSpace(*name)
	if cleanName == "" {
		return fmt.Errorf("缺少 --name")
	}
	meaningful := false
	for _, field := range []string{"label", "department", "kind", "permission-mode", "reports-to", "role", "workstation", "activation", "keep-warm", "max-wip", "grant", "revoke", "enable", "disable"} {
		meaningful = meaningful || changed[field]
	}
	if !meaningful {
		return fmt.Errorf("staff update 至少需要一项席位、属性或权限变更")
	}
	parsedMaxWIP := 0
	if changed["max-wip"] {
		var err error
		parsedMaxWIP, err = strconv.Atoi(strings.TrimSpace(*maxWIPText))
		if err != nil {
			return fmt.Errorf("--max-wip 必须是整数")
		}
	}
	var updated AgentRule
	var cleanApproval string
	mutate := func(cfg *Config) error {
		liveActor, ok := cfg.exactRule(actor.Name)
		if !ok || liveActor.Disabled || !liveActor.CanManageStaff {
			return fmt.Errorf("当前 agent %s 已失去实时 can_manage_staff 权限", actor.Name)
		}
		index := -1
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == cleanName {
				index = i
				break
			}
		}
		if index < 0 {
			return fmt.Errorf("员工未登记：%s", cleanName)
		}
		rule := cfg.Agents[index]
		oldDepartment := rule.Department
		if changed["label"] {
			value, err := validateShortText("label", *label, true)
			if err != nil {
				return err
			}
			rule.Label = value
		}
		if changed["department"] {
			value := strings.TrimSpace(*department)
			if !safeDepartment(value) {
				return fmt.Errorf("--department 非法")
			}
			rule.Department = value
			for i, role := range rule.Responsibilities {
				if role == roleManagerPrefix+oldDepartment {
					rule.Responsibilities[i] = roleManagerPrefix + value
				}
			}
		}
		if changed["kind"] {
			rule.Kind = strings.TrimSpace(*kind)
			if rule.Kind == "" {
				return fmt.Errorf("--kind 不能为空")
			}
		}
		if changed["permission-mode"] {
			rule.PermissionMode = strings.TrimSpace(*permissionMode)
			if rule.PermissionMode != "native" && rule.PermissionMode != "yolo" {
				return fmt.Errorf("--permission-mode 必须是 native|yolo")
			}
		}
		if changed["reports-to"] {
			rule.ReportsTo = strings.TrimSpace(*reportsTo)
			if rule.ReportsTo == "-" {
				rule.ReportsTo = ""
			}
		}
		if err := applyPermissions(&rule, *grant, true); err != nil {
			return err
		}
		if err := applyPermissions(&rule, *revoke, false); err != nil {
			return err
		}
		if *enable {
			rule.Disabled = false
		}
		role := roleCardKey(rule.RoleCardID, rule.RoleCardVersion)
		if changed["role"] {
			role = strings.TrimSpace(*roleRef)
		}
		seatWorkstation := rule.WorkstationPath
		if changed["workstation"] {
			seatWorkstation = strings.TrimSpace(*workstation)
		}
		seatActivation := rule.ActivationPolicy
		if changed["activation"] {
			seatActivation = strings.TrimSpace(*activation)
		}
		seatKeepWarm := rule.KeepWarm
		if changed["keep-warm"] {
			seatKeepWarm = strings.TrimSpace(*keepWarm)
			if seatKeepWarm == "-" {
				seatKeepWarm = ""
			}
		}
		seatMaxWIP := rule.MaxWIP
		if changed["max-wip"] {
			seatMaxWIP = parsedMaxWIP
		}
		if *disable {
			rule.Disabled = true
			seatActivation = activationManual
			seatKeepWarm = ""
		}
		if err := bindEmployeeSeat(*cfg, a.HQRoot, &rule, role, seatWorkstation, seatActivation, seatKeepWarm, seatMaxWIP); err != nil {
			return err
		}
		rule.SeatVersion++
		rule.SeatDigest = employeeSeatDigest(rule)
		digest := staffScopeDigest("staff:update", rule)
		var err error
		cleanApproval, err = validateApprovalScope(*approval, a.Office, cfg.ownerPrincipal(), ApprovalScope{
			Action: "staff:update", Target: rule.Name, RequestDigest: digest,
		})
		if err != nil {
			return fmt.Errorf("approval：%w", err)
		}
		rule.ApprovalRef, rule.UpdatedAt = cleanApproval, time.Now().UTC().Format(time.RFC3339)
		cfg.Agents[index] = rule
		updated = rule
		return nil
	}
	cfg, err := mutateConfigWithOptions(a.ConfigPath, a.staffConfigWriteOptions(), mutate)
	if err != nil {
		return err
	}
	if a.DryRun {
		return a.output(updated, fmt.Sprintf("DRY-RUN：将修改员工 %s", updated.Name))
	}
	a.Config = cfg
	return a.output(updated, fmt.Sprintf("已修改员工 %s；配置=%s；批准=%s", updated.Name, a.ConfigPath, cleanApproval))
}

func (a *App) cmdStaffRemove(args []string) error {
	fs := newLeafParser("staff remove")
	fs.SetOutput(a.Err)
	name := fs.String("name", "", "稳定 agent slug")
	approval := fs.String("approval", "", "生效 decisions 文件")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.staffMutationActor()
	if err != nil {
		return err
	}
	cleanName := strings.TrimSpace(*name)
	if cleanName == "" {
		return fmt.Errorf("缺少 --name")
	}
	var updated AgentRule
	var cleanApproval string
	mutate := func(cfg *Config) error {
		liveActor, ok := cfg.exactRule(actor.Name)
		if !ok || liveActor.Disabled || !liveActor.CanManageStaff {
			return fmt.Errorf("当前 agent %s 已失去实时 can_manage_staff 权限", actor.Name)
		}
		index := -1
		for i := range cfg.Agents {
			if cfg.Agents[i].Name == cleanName {
				index = i
			}
			if cfg.Agents[i].ReportsTo == cleanName && !cfg.Agents[i].Disabled {
				return fmt.Errorf("不能停用 %s：在职员工 %s 仍向其汇报", cleanName, cfg.Agents[i].Name)
			}
		}
		if index < 0 {
			return fmt.Errorf("员工未登记：%s", cleanName)
		}
		updated = cfg.Agents[index]
		if updated.Disabled {
			return fmt.Errorf("员工已经停用：%s", cleanName)
		}
		updated.Disabled = true
		updated.ActivationPolicy = activationManual
		updated.KeepWarm = ""
		updated.SeatVersion++
		updated.SeatDigest = employeeSeatDigest(updated)
		digest := staffScopeDigest("staff:remove", updated)
		var err error
		cleanApproval, err = validateApprovalScope(*approval, a.Office, cfg.ownerPrincipal(), ApprovalScope{
			Action: "staff:remove", Target: updated.Name, RequestDigest: digest,
		})
		if err != nil {
			return fmt.Errorf("approval：%w", err)
		}
		updated.ApprovalRef, updated.UpdatedAt = cleanApproval, time.Now().UTC().Format(time.RFC3339)
		cfg.Agents[index] = updated
		return nil
	}
	cfg, err := mutateConfigWithOptions(a.ConfigPath, a.staffConfigWriteOptions(), mutate)
	if err != nil {
		return err
	}
	if a.DryRun {
		return a.output(updated, fmt.Sprintf("DRY-RUN：将停用员工 %s（历史记录保留）", cleanName))
	}
	a.Config = cfg
	return a.output(updated, fmt.Sprintf("已停用员工 %s；未删除历史；批准=%s", cleanName, cleanApproval))
}

func applyPermissions(rule *AgentRule, list string, value bool) error {
	for _, permission := range strings.Split(list, ",") {
		permission = strings.TrimSpace(permission)
		if permission == "" {
			continue
		}
		switch permission {
		case "create":
			rule.CanCreate = value
		case "issue":
			rule.CanIssue = value
		case "accept":
			rule.CanAccept = value
		case "close":
			rule.CanClose = value
		case "manage-staff":
			rule.CanManageStaff = value
		case "receive-order":
			rule.CanReceiveOrder = value
		case "manager":
			managerRole := roleManagerPrefix + rule.Department
			if value && !rule.hasResponsibility(managerRole) {
				rule.Responsibilities = append(rule.Responsibilities, managerRole)
			}
			if !value {
				roles := rule.Responsibilities[:0]
				for _, role := range rule.Responsibilities {
					if role != managerRole {
						roles = append(roles, role)
					}
				}
				rule.Responsibilities = roles
			}
		default:
			return fmt.Errorf("未知权限 %q；可选 create,issue,accept,close,manage-staff,receive-order,manager", permission)
		}
	}
	return nil
}
