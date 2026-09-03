package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

const minimalTemplateDepartment = "delivery"

type initOptions struct {
	CompanyName, Owner, Workspace, Template, OrganizationSpec string
	SecretaryName, SecretaryNickname                          string
	SecretaryKind, DefaultAgentKind                           string
	SecretaryAgentArgs, DefaultAgentArgs                      []string
	PermissionMode                                            string
	Silent, PrepareOnly                                       bool
}

type initPlan struct {
	Root, CompanyName, Owner, Workspace, Template   string
	InitSource, OrganizationSpecDigest              string
	OrganizationSpecRaw                             []byte
	SecretaryKind, DefaultAgentKind, PermissionMode string
	Config                                          Config
	ManualProfiles                                  map[string]roleManualProfile
}

type templateAgent struct {
	Name, Nickname, Department, DepartmentLabel, Responsibility, ReportsTo string
	Manager, Secretary                                                     bool
}

type companyTemplate struct {
	Slug, Label, Summary string
	Agents               []templateAgent
}

var companyTemplates = []companyTemplate{
	{Slug: "minimal", Label: "最小交付公司", Summary: "总裁办 + 一个交付部门；含经理和执行成员。", Agents: []templateAgent{
		{Name: "delivery-manager", Nickname: "交付负责人", Department: "delivery", DepartmentLabel: "交付部", Responsibility: "manager:delivery", ReportsTo: "secretary", Manager: true},
		{Name: "delivery-specialist", Nickname: "交付专员", Department: "delivery", DepartmentLabel: "交付部", Responsibility: "specialist:delivery", ReportsTo: "delivery-manager"},
	}},
	{Slug: "product-engineering", Label: "产品工程公司", Summary: "产品质量体验部提需求，工程部负责实现。", Agents: []templateAgent{
		{Name: "product-manager", Nickname: "产品质量体验负责人", Department: "product-quality", DepartmentLabel: "产品质量体验部", Responsibility: "manager:product-quality", ReportsTo: "secretary", Manager: true},
		{Name: "product-specialist", Nickname: "产品体验专员", Department: "product-quality", DepartmentLabel: "产品质量体验部", Responsibility: "specialist:product-quality", ReportsTo: "product-manager"},
		{Name: "engineering-manager", Nickname: "工程负责人", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "manager:engineering", ReportsTo: "secretary", Manager: true},
		{Name: "engineer", Nickname: "工程师", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "developer:engineering", ReportsTo: "engineering-manager"},
	}},
	{Slug: "saas", Label: "SaaS 公司", Summary: "产品、工程、增长销售和客户成功四个精干部。", Agents: []templateAgent{
		{Name: "product-manager", Nickname: "产品负责人", Department: "product", DepartmentLabel: "产品部", Responsibility: "manager:product", ReportsTo: "secretary", Manager: true},
		{Name: "engineering-manager", Nickname: "工程负责人", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "manager:engineering", ReportsTo: "secretary", Manager: true},
		{Name: "growth-manager", Nickname: "增长销售负责人", Department: "growth-sales", DepartmentLabel: "增长销售部", Responsibility: "manager:growth-sales", ReportsTo: "secretary", Manager: true},
		{Name: "customer-success-manager", Nickname: "客户成功负责人", Department: "customer-success", DepartmentLabel: "客户成功部", Responsibility: "manager:customer-success", ReportsTo: "secretary", Manager: true},
	}},
	{Slug: "professional-services", Label: "专业服务公司", Summary: "客户服务、项目交付和质量知识管理。", Agents: []templateAgent{
		{Name: "client-services-manager", Nickname: "客户服务负责人", Department: "client-services", DepartmentLabel: "客户服务部", Responsibility: "manager:client-services", ReportsTo: "secretary", Manager: true},
		{Name: "delivery-manager", Nickname: "项目交付负责人", Department: "delivery", DepartmentLabel: "项目交付部", Responsibility: "manager:delivery", ReportsTo: "secretary", Manager: true},
		{Name: "consultant", Nickname: "交付顾问", Department: "delivery", DepartmentLabel: "项目交付部", Responsibility: "consultant:delivery", ReportsTo: "delivery-manager"},
		{Name: "quality-manager", Nickname: "质量知识负责人", Department: "quality-knowledge", DepartmentLabel: "质量知识部", Responsibility: "manager:quality-knowledge", ReportsTo: "secretary", Manager: true},
	}},
	{Slug: "commerce", Label: "商业零售公司", Summary: "商品、增长、履约运营和客户服务。", Agents: []templateAgent{
		{Name: "merchandising-manager", Nickname: "商品负责人", Department: "merchandising", DepartmentLabel: "商品部", Responsibility: "manager:merchandising", ReportsTo: "secretary", Manager: true},
		{Name: "growth-manager", Nickname: "增长负责人", Department: "growth", DepartmentLabel: "增长部", Responsibility: "manager:growth", ReportsTo: "secretary", Manager: true},
		{Name: "operations-manager", Nickname: "履约运营负责人", Department: "operations", DepartmentLabel: "履约运营部", Responsibility: "manager:operations", ReportsTo: "secretary", Manager: true},
		{Name: "operations-specialist", Nickname: "履约运营专员", Department: "operations", DepartmentLabel: "履约运营部", Responsibility: "specialist:operations", ReportsTo: "operations-manager"},
		{Name: "customer-service-manager", Nickname: "客户服务负责人", Department: "customer-service", DepartmentLabel: "客户服务部", Responsibility: "manager:customer-service", ReportsTo: "secretary", Manager: true},
	}},
	{Slug: "virtual-company", Label: "虚拟公司总部", Summary: "总裁办、产品部、工程部和质量部；十个按 assignment 激活的专业角色。", Agents: []templateAgent{
		{Name: "product-manager", Nickname: "产品负责人", Department: "product", DepartmentLabel: "产品部", Responsibility: "manager:product", ReportsTo: "secretary", Manager: true},
		{Name: "engineering-manager", Nickname: "数字工程负责人", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "manager:engineering", ReportsTo: "secretary", Manager: true},
		{Name: "quality-manager", Nickname: "质量与业务验收负责人", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "manager:quality", ReportsTo: "secretary", Manager: true},
		{Name: "eng-data-engineer", Nickname: "数据工程师", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "data_engineer:engineering", ReportsTo: "engineering-manager"},
		{Name: "eng-app-developer", Nickname: "应用开发工程师", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "application_developer:engineering", ReportsTo: "engineering-manager"},
		{Name: "eng-security-engineer", Nickname: "安全工程师", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "security_engineer:engineering", ReportsTo: "engineering-manager"},
		{Name: "product-researcher", Nickname: "产品调研员", Department: "product", DepartmentLabel: "产品部", Responsibility: "product_researcher:product", ReportsTo: "product-manager"},
		{Name: "qa-browser-blackbox", Nickname: "浏览器黑盒测试员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "browser_blackbox:quality", ReportsTo: "quality-manager"},
		{Name: "eng-code-reviewer", Nickname: "代码审查员", Department: "engineering", DepartmentLabel: "工程部", Responsibility: "code_reviewer:engineering", ReportsTo: "engineering-manager"},
		{Name: "product-copy-reviewer", Nickname: "文案及术语审查员", Department: "product", DepartmentLabel: "产品部", Responsibility: "copy_reviewer:product", ReportsTo: "product-manager"},
		{Name: "qa-data-gate", Nickname: "数据核验与门禁执行员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "data_gate:quality", ReportsTo: "quality-manager"},
		{Name: "qa-first-use", Nickname: "首次使用体验员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "first_use_tester:quality", ReportsTo: "quality-manager"},
		{Name: "qa-usability", Nickname: "可用性走查员", Department: "quality", DepartmentLabel: "质量与用户体验部", Responsibility: "usability_reviewer:quality", ReportsTo: "quality-manager"},
	}},
}

type initEntry struct {
	label     string
	path      string
	directory bool
	content   []byte
	mode      fs.FileMode
}

type initResult struct {
	entry   initEntry
	created bool
}

func newInitApp(options globalOptions, out, errOut io.Writer) (*App, error) {
	return &App{HerdrBin: options.Herdr, In: os.Stdin, Out: out, Err: errOut, Clock: time.Now, Sleep: time.Sleep}, nil
}

func (a *App) cmdInit(args []string) error {
	fs := newLeafParser("init")
	fs.SetOutput(a.Err)
	companyName := fs.String("company-name", "", "公司显示名称")
	owner := fs.String("owner", "", "公司所有者 principal")
	workspace := fs.String("workspace", "", "Herdr workspace slug")
	template := fs.String("template", "", "组织模板")
	organizationSpec := fs.String("organization-spec", "", "已批准的自定义组织规范 YAML；与 --template 互斥")
	secretaryName := fs.String("secretary-name", "secretary", "总裁秘书稳定 slug 的基础名称")
	secretaryNickname := fs.String("secretary-nickname", "总裁秘书", "总裁秘书显示名称")
	secretaryKind := fs.String("secretary-kind", "codex", "秘书 Agent kind")
	defaultKind := fs.String("default-agent-kind", "codex", "其他 Agent kind")
	secretaryAgentArgs := fs.StringArray("secretary-agent-arg", nil, "传给秘书 Agent 的原生 argv；可重复")
	defaultAgentArgs := fs.StringArray("default-agent-arg", nil, "传给其他 Agent 的原生 argv；可重复")
	permissionMode := fs.String("permission-mode", "native", "native|yolo")
	silent := fs.Bool("silent", false, "非交互")
	prepareOnly := fs.Bool("prepare-only", false, "只生成")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("用法：hq init <company-directory>")
	}
	root, err := filepath.Abs(fs.Arg(0))
	if err != nil {
		return fmt.Errorf("解析 company-directory：%w", err)
	}
	opts := initOptions{CompanyName: *companyName, Owner: *owner, Workspace: *workspace, Template: *template, OrganizationSpec: *organizationSpec,
		SecretaryName: *secretaryName, SecretaryNickname: *secretaryNickname,
		SecretaryKind: *secretaryKind, DefaultAgentKind: *defaultKind,
		SecretaryAgentArgs: append([]string(nil), (*secretaryAgentArgs)...), DefaultAgentArgs: append([]string(nil), (*defaultAgentArgs)...),
		PermissionMode: *permissionMode,
		Silent:         *silent, PrepareOnly: *prepareOnly}
	if existing, existingErr := existingInitPlan(root); existingErr != nil {
		return existingErr
	} else if existing != nil && !initFormationFlagsChanged(fs) {
		if opts.PrepareOnly {
			fmt.Fprintf(a.Out, "HQ init：公司已准备并通过静态校验（agents=%d）；未连接 Herdr。\n", len(existing.Config.Agents))
			fmt.Fprintf(a.Out, "下一步：在宿主机运行公司本地 ceo-office/tools/hq/bin/hq init %s 完成首次初始化。\n", root)
			return nil
		}
		started, err := a.startInitPlan(*existing)
		if err != nil {
			return fmt.Errorf("公司文件已存在，但首次启动未完成；修复后重跑 hq init %s 即可续跑：%w", root, err)
		}
		if started {
			fmt.Fprintf(a.Out, "HQ init：公司首次初始化完成（agents=%d，workspace=%s）。\n", len(existing.Config.Agents), existing.Workspace)
		}
		return nil
	}
	if !opts.Silent {
		if err := a.runInitWizard(root, &opts); err != nil {
			return err
		}
	}
	plan, err := buildInitPlan(root, opts)
	if err != nil {
		return err
	}
	initLock, err := lockInitTarget(plan.Root)
	if err != nil {
		return err
	}
	if err := a.materializeInitPlan(plan); err != nil {
		unlock(initLock)
		return err
	}
	unlock(initLock)
	if opts.PrepareOnly {
		fmt.Fprintf(a.Out, "HQ init：公司已准备并通过静态校验（source=%s，agents=%d）；未连接 Herdr。\n", plan.InitSource, len(plan.Config.Agents))
		fmt.Fprintf(a.Out, "下一步：在宿主机运行公司本地 ceo-office/tools/hq/bin/hq init %s 完成首次初始化。\n", plan.Root)
		return nil
	}
	started, err := a.startInitPlan(plan)
	if err != nil {
		return fmt.Errorf("公司文件已完整生成，但首次启动未完成；修复后重跑同一 init 命令即可续跑：%w", err)
	}
	if started {
		fmt.Fprintf(a.Out, "HQ init：公司已建立并启动（source=%s，agents=%d，workspace=%s）。\n", plan.InitSource, len(plan.Config.Agents), plan.Workspace)
	}
	return nil
}

func initFormationFlagsChanged(fs *leafParser) bool {
	for _, name := range []string{"company-name", "owner", "workspace", "template", "organization-spec", "secretary-name", "secretary-nickname", "secretary-kind", "default-agent-kind", "secretary-agent-arg", "default-agent-arg", "permission-mode"} {
		if fs.Changed(name) {
			return true
		}
	}
	return false
}

func existingInitPlan(root string) (*initPlan, error) {
	configPath := defaultConfigPath(filepath.Join(root, "ceo-office"))
	if _, err := os.Lstat(configPath); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	canonicalRoot, err := canonicalExistingDirectory(root, "existing company root")
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(defaultConfigPath(filepath.Join(canonicalRoot, "ceo-office")))
	if err != nil {
		return nil, fmt.Errorf("读取已准备公司的 registry：%w", err)
	}
	if err := validateRegistryManuals(cfg, canonicalRoot); err != nil {
		return nil, fmt.Errorf("已准备公司的岗位手册校验失败：%w", err)
	}
	return &initPlan{Root: canonicalRoot, Owner: cfg.ownerPrincipal(), Workspace: cfg.WorkspaceLabel, InitSource: "prepared-company", Config: cfg}, nil
}

func lockInitTarget(root string) (*os.File, error) {
	digest := sha256.Sum256([]byte(filepath.Clean(root)))
	path := filepath.Join(os.TempDir(), "hq-init-"+hex.EncodeToString(digest[:16])+".lock")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("创建 init 锁：%w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		file.Close()
		return nil, fmt.Errorf("获取 init 锁：%w", err)
	}
	return file, nil
}

func (a *App) runInitWizard(root string, opts *initOptions) error {
	reader := bufio.NewReader(a.In)
	if reader == nil {
		return fmt.Errorf("交互式 init 缺少 stdin；自动化请使用 --silent")
	}
	base := filepath.Base(root)
	var err error
	if opts.CompanyName, err = promptLine(reader, a.Out, "公司名称", opts.CompanyName, base); err != nil {
		return err
	}
	if opts.Owner, err = promptLine(reader, a.Out, "公司所有者 principal", opts.Owner, ""); err != nil {
		return err
	}
	if opts.Workspace, err = promptLine(reader, a.Out, "Herdr workspace", opts.Workspace, slugify(base)+"-hq"); err != nil {
		return err
	}
	if strings.TrimSpace(opts.OrganizationSpec) == "" {
		fmt.Fprintln(a.Out, "组织模板（也可预先传 --organization-spec 跳过模板）：")
		for index, item := range companyTemplates {
			fmt.Fprintf(a.Out, "  %d. %s (%s) — %s\n", index+1, item.Slug, item.Label, item.Summary)
		}
		if opts.Template, err = promptLine(reader, a.Out, "模板编号或名称", opts.Template, "product-engineering"); err != nil {
			return err
		}
		if number, numberErr := strconv.Atoi(opts.Template); numberErr == nil && number >= 1 && number <= len(companyTemplates) {
			opts.Template = companyTemplates[number-1].Slug
		}
		if opts.SecretaryName, err = promptLine(reader, a.Out, "总裁秘书 slug 基础名称", opts.SecretaryName, "secretary"); err != nil {
			return err
		}
		if opts.SecretaryNickname, err = promptLine(reader, a.Out, "总裁秘书显示名称", opts.SecretaryNickname, "总裁秘书"); err != nil {
			return err
		}
	}
	if opts.SecretaryKind, err = promptLine(reader, a.Out, "秘书 Agent kind", opts.SecretaryKind, "codex"); err != nil {
		return err
	}
	if opts.DefaultAgentKind, err = promptLine(reader, a.Out, "其他成员 Agent kind", opts.DefaultAgentKind, "codex"); err != nil {
		return err
	}
	if opts.PermissionMode, err = promptLine(reader, a.Out, "权限模式 (native/yolo)", opts.PermissionMode, "native"); err != nil {
		return err
	}
	answer, err := promptLine(reader, a.Out, "确认创建并按所选模式执行？(yes/no)", "", "no")
	if err != nil {
		return err
	}
	if answer != "yes" && answer != "y" {
		return fmt.Errorf("用户取消初始化；尚未写入任何文件")
	}
	return nil
}

func promptLine(reader *bufio.Reader, out io.Writer, label, current, fallback string) (string, error) {
	if strings.TrimSpace(current) != "" {
		fallback = strings.TrimSpace(current)
	}
	if fallback != "" {
		fmt.Fprintf(out, "%s [%s]: ", label, fallback)
	} else {
		fmt.Fprintf(out, "%s: ", label)
	}
	line, err := reader.ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		line = fallback
	}
	return line, nil
}

func slugify(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else if b.Len() > 0 && !strings.HasSuffix(b.String(), "-") {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func buildInitPlan(root string, opts initOptions) (initPlan, error) {
	missing := []string{}
	for name, value := range map[string]string{"--company-name": opts.CompanyName, "--owner": opts.Owner, "--workspace": opts.Workspace} {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	hasTemplate := strings.TrimSpace(opts.Template) != ""
	hasOrganizationSpec := strings.TrimSpace(opts.OrganizationSpec) != ""
	if hasTemplate && hasOrganizationSpec {
		return initPlan{}, fmt.Errorf("--template 与 --organization-spec 互斥；请选择恰好一个初始化来源")
	}
	if !hasTemplate && !hasOrganizationSpec {
		missing = append(missing, "恰好一个 --template 或 --organization-spec")
	}
	sort.Strings(missing)
	if len(missing) != 0 {
		return initPlan{}, fmt.Errorf("init 缺少必填字段 %s；--silent 不会询问", strings.Join(missing, "、"))
	}
	if err := validateOwnerPrincipal(opts.Owner); err != nil {
		return initPlan{}, err
	}
	if !agentNamePattern.MatchString(opts.Workspace) {
		return initPlan{}, fmt.Errorf("--workspace 必须是小写 ASCII slug 且不超过 32 字符")
	}
	if strings.TrimSpace(opts.CompanyName) != opts.CompanyName || opts.CompanyName == "" || strings.ContainsAny(opts.CompanyName, "\r\n") {
		return initPlan{}, fmt.Errorf("--company-name 必须是非空单行文本且无首尾空白")
	}
	if opts.PermissionMode != "yolo" && opts.PermissionMode != "native" {
		return initPlan{}, fmt.Errorf("--permission-mode 必须是 yolo|native")
	}
	if !agentNamePattern.MatchString(opts.SecretaryKind) || !agentNamePattern.MatchString(opts.DefaultAgentKind) {
		return initPlan{}, fmt.Errorf("Agent kind 必须是小写 ASCII slug")
	}
	secretaryBase := strings.TrimSpace(opts.SecretaryName)
	if !agentNamePattern.MatchString(secretaryBase) {
		return initPlan{}, fmt.Errorf("--secretary-name 必须是小写 ASCII slug 且不超过 32 字符")
	}
	secretaryNickname, err := validateShortText("secretary-nickname", opts.SecretaryNickname, true)
	if err != nil {
		return initPlan{}, err
	}
	if hasOrganizationSpec {
		compiled, err := loadAndCompileOrganizationSpec(opts.OrganizationSpec, opts)
		if err != nil {
			return initPlan{}, err
		}
		return initPlan{Root: filepath.Clean(root), CompanyName: opts.CompanyName, Owner: opts.Owner, Workspace: opts.Workspace,
			InitSource: "organization-spec:" + compiled.Spec.ID, OrganizationSpecDigest: compiled.Digest,
			OrganizationSpecRaw: compiled.Raw, SecretaryKind: opts.SecretaryKind, DefaultAgentKind: opts.DefaultAgentKind,
			PermissionMode: opts.PermissionMode, Config: compiled.Config, ManualProfiles: compiled.Profiles}, nil
	}
	var selected *companyTemplate
	for index := range companyTemplates {
		if companyTemplates[index].Slug == opts.Template {
			selected = &companyTemplates[index]
			break
		}
	}
	if selected == nil {
		available := make([]string, 0, len(companyTemplates))
		for _, item := range companyTemplates {
			available = append(available, item.Slug)
		}
		return initPlan{}, fmt.Errorf("未知模板 %q；可选：%s", opts.Template, strings.Join(available, "|"))
	}
	names := map[string]string{"secretary": scopedAgentName(opts.Workspace, secretaryBase)}
	for _, item := range selected.Agents {
		names[item.Name] = scopedAgentName(opts.Workspace, item.Name)
	}
	agents := []AgentRule{{Name: names["secretary"], Nickname: secretaryNickname, DepartmentLabel: "总裁办", Label: "总裁办-" + secretaryNickname, Workspace: opts.Workspace,
		Responsibilities: []string{roleApprovalWitness, roleAccountCloser, "executive_secretary"}, Department: "ceo-office",
		Kind: opts.SecretaryKind, AgentArgs: append([]string(nil), opts.SecretaryAgentArgs...), PermissionMode: opts.PermissionMode, ActivationPolicy: activationAlways, MaxWIP: 16,
		CanCreate: true, CanIssue: true, CanAccept: true, CanClose: true, CanManageStaff: true, CanReceiveOrder: true}}
	roleIDs := []string{secretaryBase}
	for _, item := range selected.Agents {
		rule := AgentRule{Name: names[item.Name], Nickname: item.Nickname, DepartmentLabel: item.DepartmentLabel, Label: item.DepartmentLabel + "-" + item.Nickname,
			Workspace: opts.Workspace, Responsibilities: []string{item.Responsibility}, Department: item.Department,
			Kind: opts.DefaultAgentKind, AgentArgs: append([]string(nil), opts.DefaultAgentArgs...), PermissionMode: opts.PermissionMode, ReportsTo: names[item.ReportsTo],
			ActivationPolicy: activationOnAssignment, KeepWarm: defaultOnAssignmentKeepWarm.String(), MaxWIP: 1, CanAccept: true, CanReceiveOrder: true}
		if item.Manager {
			rule.ActivationPolicy, rule.KeepWarm, rule.MaxWIP = activationAlways, "", 8
			rule.CanCreate, rule.CanIssue = true, true
		}
		agents = append(agents, rule)
		roleIDs = append(roleIDs, item.Name)
	}
	roleCards := make([]RoleCard, 0, len(agents))
	for index := range agents {
		rule := &agents[index]
		roleID := roleIDs[index]
		rule.RoleCardID, rule.RoleCardVersion = roleID, 1
		rule.WorkstationPath = filepath.Join(rule.Department, "staff", roleID, "v1")
		rule.ManualPath = filepath.Join(rule.WorkstationPath, "AGENTS.md")
		rule.SeatVersion = 1
		profile := profileForAgent(*rule)
		capabilities, err := canonicalStringSet(profile.Capabilities)
		if err != nil {
			return initPlan{}, fmt.Errorf("模板角色 %s capabilities：%w", roleID, err)
		}
		manual := agentRoleCardManual(opts.CompanyName, opts.Workspace, *rule)
		card := RoleCard{ID: roleID, Version: 1, Label: rule.Nickname, Department: rule.Department,
			Capabilities: capabilities, ManualPath: rule.ManualPath, ManualDigest: roleCardFileDigest(manual),
			Status: roleCardApproved, ApprovalRef: "ceo-office/decisions/company-init.md"}
		card.Digest = roleCardDigest(card)
		rule.RoleCardDigest = card.Digest
		rule.SeatDigest = employeeSeatDigest(*rule)
		roleCards = append(roleCards, card)
	}
	cfg := Config{Version: registrySchemaVersion, WorkspaceLabel: opts.Workspace, OwnerPrincipal: opts.Owner, RoleCards: roleCards, Agents: agents,
		DeliveryPolicy: &DeliveryPolicy{DefaultMode: deliveryModeAuto, MaxConsecutiveWakes: 3,
			MaxBundleItems: defaultDeliveryBundleItems, MaxBundleBytes: defaultDeliveryBundleBytes,
			AssignmentAcceptTimeout: defaultAssignmentAcceptTimeout.String(), MaxActivationRedeliveries: defaultMaxActivationRedeliveries,
			ManagerQueueStallTimeout: defaultManagerQueueStallTimeout.String(), ManagerQueueEscalateAfter: defaultManagerQueueEscalateAfter.String(),
			MaxManagerQueueNudges: defaultMaxManagerQueueNudges}}
	if err := validateConfig(cfg); err != nil {
		return initPlan{}, fmt.Errorf("模板生成了无效配置：%w", err)
	}
	return initPlan{Root: filepath.Clean(root), CompanyName: opts.CompanyName, Owner: opts.Owner, Workspace: opts.Workspace, Template: opts.Template, InitSource: "template:" + opts.Template,
		SecretaryKind: opts.SecretaryKind, DefaultAgentKind: opts.DefaultAgentKind, PermissionMode: opts.PermissionMode, Config: cfg}, nil
}

func scopedAgentName(workspace, base string) string {
	scope := strings.TrimSuffix(workspace, "-hq")
	candidate := scope + "-" + base
	if len(candidate) <= 32 {
		return candidate
	}
	sum := sha256.Sum256([]byte(workspace + "\x00" + base))
	prefix := scope
	if len(prefix) > 6 {
		prefix = strings.Trim(prefix[:6], "-")
	}
	readable := base
	available := 32 - len(prefix) - 1 - 1 - 5
	if available < 1 {
		available = 1
	}
	if len(readable) > available {
		readable = strings.Trim(readable[:available], "-")
	}
	return prefix + "-" + readable + "-" + hex.EncodeToString(sum[:])[:5]
}

func (a *App) materializeInitPlan(plan initPlan) error {
	office := filepath.Join(plan.Root, "ceo-office")
	configPath := defaultConfigPath(office)
	if info, err := os.Lstat(plan.Root); err == nil && info.IsDir() {
		entries, readErr := os.ReadDir(plan.Root)
		if readErr != nil {
			return readErr
		}
		if len(entries) != 0 {
			if _, configErr := os.Lstat(configPath); configErr != nil {
				return fmt.Errorf("目标目录非空且不是可续跑的 HQ 公司：%s", plan.Root)
			}
		}
	} else if err == nil {
		return fmt.Errorf("company-directory 已存在但不是目录：%s", plan.Root)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	configRaw, err := yaml.Marshal(plan.Config)
	if err != nil {
		return err
	}
	if existing, loadErr := loadConfig(configPath); loadErr == nil && !reflect.DeepEqual(existing, plan.Config) {
		return fmt.Errorf("现有公司配置与本次 init 参数不一致；拒绝静默跳过或覆盖：%s", configPath)
	}
	decisionRaw, err := formationDecision(plan, a.now())
	if err != nil {
		return err
	}
	binaryPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("定位当前 hq 二进制：%w", err)
	}
	binaryPath, err = filepath.EvalSymlinks(binaryPath)
	if err != nil {
		return fmt.Errorf("解析当前 hq 二进制：%w", err)
	}
	binaryRaw, err := os.ReadFile(binaryPath)
	if err != nil {
		return fmt.Errorf("读取当前 hq 二进制：%w", err)
	}
	entries := []initEntry{
		{label: "公司根目录", path: plan.Root, directory: true}, {label: "总裁办", path: office, directory: true},
		{label: "决策目录", path: filepath.Join(office, "decisions"), directory: true}, {label: "记录目录", path: filepath.Join(office, "records"), directory: true},
		{label: "HQ 工具目录", path: filepath.Join(office, "tools", "hq", "bin"), directory: true},
		{label: "公司说明", path: filepath.Join(plan.Root, "COMPANY.md"), content: companyReadme(plan)},
		{label: "Agent 上岗与协作手册", path: filepath.Join(plan.Root, "AGENT-HANDBOOK.md"), content: companyAgentHandbook(plan)},
		{label: "公司成立决策", path: filepath.Join(office, "decisions", "company-init.md"), content: decisionRaw},
		{label: "HQ 配置", path: configPath, content: configRaw},
		{label: "公司本地 HQ 二进制", path: filepath.Join(office, "tools", "hq", "bin", "hq"), content: binaryRaw, mode: 0o755},
	}
	if len(plan.OrganizationSpecRaw) != 0 {
		entries = append(entries,
			initEntry{label: "公司成立输入目录", path: filepath.Join(office, "formation"), directory: true},
			initEntry{label: "已批准组织规范（成立证据）", path: filepath.Join(office, "formation", "organization-spec.yaml"), content: plan.OrganizationSpecRaw},
		)
	}
	seen := map[string]bool{"ceo-office": true}
	for _, rule := range plan.Config.Agents {
		if seen[rule.Department] {
			continue
		}
		seen[rule.Department] = true
		entries = append(entries, initEntry{label: rule.DepartmentLabel + "目录", path: filepath.Join(plan.Root, rule.Department), directory: true})
	}
	for _, rule := range plan.Config.Agents {
		profile, hasProfile := plan.ManualProfiles[rule.Name]
		var manual []byte
		if hasProfile {
			manual = agentRoleCardManualWithProfile(plan.CompanyName, plan.Workspace, rule, profile)
		} else {
			manual = agentRoleCardManual(plan.CompanyName, plan.Workspace, rule)
		}
		entries = append(entries,
			initEntry{label: rule.Nickname + "独立工位", path: filepath.Join(plan.Root, rule.WorkstationPath), directory: true},
			initEntry{label: rule.Nickname + "角色卡", path: filepath.Join(plan.Root, rule.ManualPath), content: manual})
	}
	if err := preflightInitEntries(entries); err != nil {
		return err
	}

	results := make([]initResult, 0, len(entries))
	for _, entry := range entries {
		var created bool
		var entryErr error
		if entry.directory {
			created, entryErr = createDirectoryNoReplace(entry.path)
		} else {
			mode := entry.mode
			if mode == 0 {
				mode = 0o644
			}
			created, entryErr = createFileNoReplaceMode(entry.path, entry.content, mode)
		}
		if entryErr != nil {
			return fmt.Errorf("初始化 %s %s：%w", entry.label, entry.path, entryErr)
		}
		results = append(results, initResult{entry: entry, created: created})
	}
	loaded, err := loadConfig(configPath)
	if err != nil {
		return fmt.Errorf("初始化后注册表校验失败：%w", err)
	}
	if err := validateRegistryManuals(loaded, plan.Root); err != nil {
		return fmt.Errorf("初始化后岗位手册校验失败：%w", err)
	}
	if _, _, err := readApproval(filepath.Join(office, "decisions", "company-init.md"), office, plan.Owner, true); err != nil {
		return fmt.Errorf("初始化后公司成立决策校验失败：%w", err)
	}

	createdCount := 0
	for _, result := range results {
		if result.created {
			createdCount++
			fmt.Fprintf(a.Out, "CREATE %s %s\n", result.entry.label, result.entry.path)
		} else {
			fmt.Fprintf(a.Out, "SKIP exists %s %s\n", result.entry.label, result.entry.path)
		}
	}
	fmt.Fprintf(a.Out, "HQ 文件生成完成（created=%d skipped=%d，root=%s）\n", createdCount, len(results)-createdCount, plan.Root)
	return nil
}

func (a *App) now() time.Time {
	if a.Clock != nil {
		return a.Clock().UTC()
	}
	return time.Now().UTC()
}

func formationDecision(plan initPlan, at time.Time) ([]byte, error) {
	digestInput, _ := json.Marshal(struct {
		Company, Owner, Workspace, Source, OrganizationSpecDigest string
		Agents                                                    []AgentRule
		RoleCards                                                 []RoleCard
	}{plan.CompanyName, plan.Owner, plan.Workspace, plan.InitSource, plan.OrganizationSpecDigest, plan.Config.Agents, plan.Config.RoleCards})
	digest := sha256.Sum256(digestInput)
	metadata := ApprovalMetadata{Version: 1, DecisionID: "DEC-COMPANY-INIT-001", Status: "effective", ConfirmedBy: plan.Owner,
		ConfirmedAt: at.Format(time.RFC3339), Scopes: []ApprovalScope{{Action: "company:init", Target: plan.Workspace, RequestDigest: hex.EncodeToString(digest[:])}}}
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, err
	}
	body := fmt.Sprintf("# %s 公司成立决策\n\n所有者 `%s` 确认以 `%s` 建立 workspace `%s`。组织变更由总部联系职责位依正式人事协议执行。", plan.CompanyName, plan.Owner, plan.InitSource, plan.Workspace)
	if plan.OrganizationSpecDigest != "" {
		body += fmt.Sprintf("\n\n成立时自定义组织规范已保存至 `ceo-office/formation/organization-spec.yaml`，SHA-256 为 `%s`；它是成立证据，`ceo-office/tools/hq/config.yaml` 才是唯一实时组织注册表。", plan.OrganizationSpecDigest)
	}
	body += "\n"
	return []byte(approvalHeaderMarker + string(raw) + metadataHeaderEnd + body), nil
}

func companyReadme(plan initPlan) []byte {
	return []byte(fmt.Sprintf("# %s\n\n- 所有者：%s\n- Herdr workspace：%s\n- 初始化来源：%s\n- HQ：`ceo-office/tools/hq/bin/hq`\n- Agent 上岗与协作：`AGENT-HANDBOOK.md`\n\nHQ init 不使用 LLM API。公司启动后，由总部联系职责位按公司决策调整人员和组织架构。`ceo-office/tools/hq/config.yaml` 是唯一组织编制与 employee seat 注册表；每名员工的固定行为、职责和边界由个人版本目录中的 `AGENTS.md` 定义。自定义 organization spec 只作为成立证据保存，不是第二份实时 roster。\n", plan.CompanyName, plan.Owner, plan.Workspace, plan.InitSource))
}

func (a *App) startInitPlan(plan initPlan) (bool, error) {
	herdr := a.HerdrBin
	control := a.Herdr
	if control == nil {
		var err error
		herdr, err = resolveHerdrExecutable(a.HerdrBin)
		if err != nil {
			return false, err
		}
		control, err = newExecHerdrControl(herdr)
		if err != nil {
			return false, err
		}
	}
	office := filepath.Join(plan.Root, "ceo-office")
	data := filepath.Join(office, "records")
	socket, err := gatewaySocketPath(data)
	if err != nil {
		return false, fmt.Errorf("init 启动任何 agent 前解析 gateway socket：%w", err)
	}
	if err := ensureGatewaySocketRuntimeDir(socket, data); err != nil {
		return false, fmt.Errorf("init 启动任何 agent 前准备 gateway runtime：%w", err)
	}
	configPath := defaultConfigPath(office)
	installedConfig, err := loadConfig(configPath)
	if err != nil {
		return false, fmt.Errorf("重载已安装的 init registry：%w", err)
	}
	store := NewStore(data)
	gateway := a.GatewayHealth
	if gateway == nil {
		gateway = unixGatewayPinger{}
	}
	app, err := newAppWithDependencies(runtimePaths{Office: office, HQRoot: plan.Root, DataDir: data, ConfigPath: configPath, HerdrBin: herdr}, installedConfig, globalOptions{}, AppDependencies{
		Store: store, Identity: herdrIdentityProvider{Control: control}, Transport: herdrDeliveryTransport{Control: control}, Herdr: control,
		GatewayHealth: gateway, Sessions: &FileSessionStore{Root: filepath.Join(data, "sessions")}, Clock: a.Clock, Sleep: a.Sleep,
	}, a.Out, a.Err)
	if err != nil {
		return false, err
	}
	return app.completeFirstInit(context.Background())
}

func (a *App) startInitAgentIfNeeded(ctx context.Context, workspaceID string, rule AgentRule) error {
	snapshot, err := a.herdrSnapshot(ctx)
	if err != nil {
		return err
	}
	if matched, mismatch := exactLiveMatch(snapshot, workspaceID, rule, a.HQRoot); matched {
		fmt.Fprintf(a.Out, "跳过 %s（已精确在岗）\n", rule.Name)
		return nil
	} else if mismatch != "" {
		return fmt.Errorf("员工 %s 存在但不满足精确在岗合同：%s", rule.Name, mismatch)
	}
	return a.startHQAgent(ctx, workspaceID, rule)
}

func preflightInitEntries(entries []initEntry) error {
	targetKinds := make(map[string]bool, len(entries))
	for _, entry := range entries {
		clean := filepath.Clean(entry.path)
		if directory, ok := targetKinds[clean]; ok && directory != entry.directory {
			return fmt.Errorf("初始化目标类型冲突：%s 同时被要求为目录和文件", clean)
		}
		targetKinds[clean] = entry.directory
	}
	for path, directory := range targetKinds {
		if directory {
			continue
		}
		prefix := path + string(filepath.Separator)
		for other := range targetKinds {
			if strings.HasPrefix(other, prefix) {
				return fmt.Errorf("初始化目标类型冲突：文件 %s 不能作为 %s 的父目录", path, other)
			}
		}
	}
	for _, entry := range entries {
		if err := preflightInitEntry(entry); err != nil {
			return err
		}
	}
	return nil
}

func preflightInitEntry(entry initEntry) error {
	info, err := os.Lstat(entry.path)
	if err == nil {
		if entry.directory && !info.IsDir() {
			return fmt.Errorf("初始化目标类型冲突：期望目录，实际为 %s：%s", fileType(info), entry.path)
		}
		if !entry.directory && !info.Mode().IsRegular() {
			return fmt.Errorf("初始化目标类型冲突：期望普通文件，实际为 %s：%s", fileType(info), entry.path)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("检查初始化目标 %s：%w", entry.path, err)
	}
	for current := filepath.Dir(entry.path); ; current = filepath.Dir(current) {
		info, statErr := os.Lstat(current)
		if statErr == nil && !info.IsDir() {
			return fmt.Errorf("初始化目标父路径类型冲突：期望目录，实际为 %s：%s", fileType(info), current)
		}
		if statErr != nil && !errors.Is(statErr, fs.ErrNotExist) {
			return fmt.Errorf("检查初始化目标父路径 %s：%w", current, statErr)
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
	}
	return nil
}

func createDirectoryNoReplace(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err == nil {
		if !info.IsDir() {
			return false, fmt.Errorf("目标类型已变化，期望目录，实际为 %s", fileType(info))
		}
		return false, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return false, err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return false, err
	}
	info, err = os.Lstat(path)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("创建后目标不是目录，而是 %s", fileType(info))
	}
	return true, nil
}

func createFileNoReplace(path string, content []byte) (created bool, err error) {
	return createFileNoReplaceMode(path, content, 0o644)
}

func createFileNoReplaceMode(path string, content []byte, mode fs.FileMode) (created bool, err error) {
	info, statErr := os.Lstat(path)
	if statErr == nil {
		if !info.Mode().IsRegular() {
			return false, fmt.Errorf("目标类型已变化，期望普通文件，实际为 %s", fileType(info))
		}
		if info.Mode().Perm() != mode.Perm() {
			return false, fmt.Errorf("现有普通文件权限为 %04o，要求 %04o：%s", info.Mode().Perm(), mode.Perm(), path)
		}
		return false, nil
	}
	if !errors.Is(statErr, fs.ErrNotExist) {
		return false, statErr
	}

	// Do not expose an empty destination between O_EXCL and Write. Concurrent
	// init callers must either see no target or the complete, fsynced file.
	// A hard-link install in the same directory gives us atomic no-replace
	// semantics without overwriting any user-owned file.
	dir := filepath.Dir(path)
	file, openErr := os.CreateTemp(dir, ".hq-init-*")
	if openErr != nil {
		return false, openErr
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return false, err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return false, err
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	if err := os.Link(temporary, path); err != nil {
		if errors.Is(err, fs.ErrExist) {
			info, statErr = os.Lstat(path)
			if statErr == nil && info.Mode().IsRegular() {
				return false, nil
			}
			if statErr == nil {
				return false, fmt.Errorf("并发创建了非普通文件目标：%s", fileType(info))
			}
		}
		return false, err
	}
	if err := syncDirectory(dir); err != nil {
		return false, err
	}
	return true, nil
}

func fileType(info fs.FileInfo) string {
	switch {
	case info.IsDir():
		return "目录"
	case info.Mode().IsRegular():
		return "普通文件"
	case info.Mode()&os.ModeSymlink != 0:
		return "符号链接"
	default:
		return info.Mode().Type().String()
	}
}
