package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type App struct {
	Office                string
	HQRoot                string
	DataDir               string
	Config                Config
	ConfigPath            string
	HerdrBin              string
	DryRun                bool
	JSON                  bool
	Direct                bool
	CallerPane            string
	MaintenancePane       string
	HostColdStart         bool
	FromGateway           bool
	ProductionRuntime     bool
	Out                   io.Writer
	Err                   io.Writer
	In                    io.Reader
	Store                 EventStore
	RecoveryStore         EventStore
	Identity              IdentityProvider
	Transport             DeliveryTransport
	DoctorRunner          DoctorRunner
	Herdr                 HerdrControl
	PatrolRunner          PatrolRunner
	GatewayHealth         GatewayPinger
	LedgerHealth          LedgerHealthReader
	Sessions              SessionStore
	Clock                 func() time.Time
	Sleep                 func(time.Duration)
	GatewayContext        context.Context
	RuntimeReaperInterval time.Duration
	RequestContext        context.Context
	GatewayWorkspaceID    string
	GatewayServerID       string
	MaintenanceActor      string
	DeliveryFailpoint     func(string) error
	DeliveryTargetState   func(string) (deliveryRuntimeState, error)
	DeliveryColdResume    func(string) error
	NudgeFailpoint        func(string) error
	EstopFailpoint        func(string) error
	Estop                 *FileEstopStore
	Index                 *DerivedIndex
	ConfigAccess          *sync.RWMutex
}

func (a *App) requestContext() context.Context {
	if a.RequestContext != nil {
		return a.RequestContext
	}
	return context.Background()
}

func (a *App) resolveIdentity(cfg Config, paneID string) (Actor, error) {
	if contextual, ok := a.Identity.(contextualIdentityProvider); ok {
		return contextual.ResolveContext(a.requestContext(), cfg, a.HQRoot, paneID)
	}
	return a.Identity.Resolve(cfg, a.HQRoot, paneID)
}

func (a *App) deliverAttempt(target, message string) DeliveryAttempt {
	if contextual, ok := a.Transport.(contextualDeliveryTransport); ok {
		return contextual.DeliverContext(a.requestContext(), target, message)
	}
	return a.Transport.Deliver(target, message)
}

func (a *App) durableRecoveryStore() EventStore {
	if a.RecoveryStore != nil {
		return a.RecoveryStore
	}
	return a.Store
}

type globalOptions struct {
	Office          string
	Data            string
	Config          string
	Herdr           string
	DryRun          bool
	JSON            bool
	Direct          bool
	MaintenancePane string
}

type AppDependencies struct {
	Store                 EventStore
	Identity              IdentityProvider
	Transport             DeliveryTransport
	DeliveryFailpoint     func(string) error
	NudgeFailpoint        func(string) error
	EstopFailpoint        func(string) error
	Estop                 *FileEstopStore
	Index                 *DerivedIndex
	Herdr                 HerdrControl
	PatrolRunner          PatrolRunner
	GatewayHealth         GatewayPinger
	LedgerHealth          LedgerHealthReader
	Sessions              SessionStore
	Clock                 func() time.Time
	Sleep                 func(time.Duration)
	GatewayContext        context.Context
	RuntimeReaperInterval time.Duration
}

func newApp(options globalOptions, out, errOut io.Writer) (*App, error) {
	paths, err := resolveProductionRuntime(options)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(paths.ConfigPath)
	if err != nil {
		return nil, err
	}
	control, err := newExecHerdrControl(paths.HerdrBin)
	if err != nil {
		return nil, err
	}
	estop := &FileEstopStore{Root: filepath.Join(paths.DataDir, "estop")}
	eventStore := NewStore(paths.DataDir)
	app, err := newAppWithDependencies(paths, cfg, options, AppDependencies{
		Store:     eventStore,
		Identity:  herdrIdentityProvider{Control: control},
		Transport: herdrDeliveryTransport{Control: control},
		Herdr:     control, PatrolRunner: &PatrolService{Herdr: control, Estop: estop, Store: eventStore, Sleep: time.Sleep},
		GatewayHealth: unixGatewayPinger{}, LedgerHealth: readOnlyLedgerHealth{Dir: paths.DataDir},
		Sessions: &FileSessionStore{Root: filepath.Join(paths.DataDir, "sessions")},
		Estop:    estop,
		Clock:    time.Now, Sleep: time.Sleep, RuntimeReaperInterval: 5 * time.Second,
		Index: &DerivedIndex{
			Path:   filepath.Join(paths.Office, "tools", "index.db"),
			Runner: execSQLiteRunner{Path: "/usr/bin/sqlite3"},
		},
	}, out, errOut)
	if err != nil {
		return nil, err
	}
	app.ProductionRuntime = true
	return app, nil
}

// newDependencyFreeReadOnlyApp constructs only the dependencies needed by the
// no-Herdr first-use queries. The unavailable control is deliberately wired
// through identity/transport so an accidental dependency call still fails
// closed instead of falling back to a real process.
func newDependencyFreeReadOnlyApp(options globalOptions, out, errOut io.Writer) (*App, error) {
	paths, err := resolveProductionPaths(options)
	if err != nil {
		return nil, err
	}
	cfg, err := loadConfig(paths.ConfigPath)
	if err != nil {
		return nil, err
	}
	control := unavailableHerdrControl{Err: fmt.Errorf("当前只读命令未构造 Herdr 依赖")}
	app, err := newAppWithDependencies(paths, cfg, options, AppDependencies{
		Store:     NewStore(paths.DataDir),
		Identity:  herdrIdentityProvider{Control: control},
		Transport: herdrDeliveryTransport{Control: control},
		Herdr:     control,
	}, out, errOut)
	if err != nil {
		return nil, err
	}
	app.ProductionRuntime = true
	return app, nil
}

func newAppWithDependencies(paths runtimePaths, cfg Config, options globalOptions, deps AppDependencies, out, errOut io.Writer) (*App, error) {
	if deps.Store == nil || deps.Identity == nil || deps.Transport == nil {
		return nil, fmt.Errorf("App 构造必须显式注入 Store、IdentityProvider 与 DeliveryTransport")
	}
	if deps.Estop == nil {
		deps.Estop = &FileEstopStore{Root: filepath.Join(paths.DataDir, "estop")}
	}
	hydrateRuntimeFallback(&cfg)
	if err := validateConfig(cfg); err != nil {
		return nil, err
	}
	if err := validateRegistryManuals(cfg, paths.HQRoot); err != nil {
		return nil, err
	}
	if concrete, ok := deps.Store.(*Store); ok {
		if err := concrete.bindConfigPath(paths.ConfigPath); err != nil {
			return nil, fmt.Errorf("绑定 Store registry：%w", err)
		}
		releaseRegistry, err := concrete.lockCurrentRegistry(cfg)
		if err != nil {
			return nil, fmt.Errorf("验证 Store registry：%w", err)
		}
		releaseRegistry()
	}
	return &App{
		Office: paths.Office, HQRoot: paths.HQRoot, DataDir: paths.DataDir, Config: cfg,
		ConfigPath: paths.ConfigPath, HerdrBin: paths.HerdrBin, DryRun: options.DryRun,
		JSON: options.JSON, Direct: options.Direct, MaintenancePane: options.MaintenancePane,
		Out: out, Err: errOut, Store: deps.Store, Identity: deps.Identity, Transport: deps.Transport,
		DeliveryFailpoint: deps.DeliveryFailpoint,
		NudgeFailpoint:    deps.NudgeFailpoint,
		EstopFailpoint:    deps.EstopFailpoint,
		Estop:             deps.Estop,
		Index:             deps.Index,
		Herdr:             deps.Herdr, PatrolRunner: deps.PatrolRunner, GatewayHealth: deps.GatewayHealth,
		LedgerHealth: deps.LedgerHealth, Sessions: deps.Sessions, Clock: deps.Clock, Sleep: deps.Sleep,
		ConfigAccess:   &sync.RWMutex{},
		GatewayContext: deps.GatewayContext, RuntimeReaperInterval: deps.RuntimeReaperInterval,
	}, nil
}

func (a *App) run(rest []string) error {
	if len(rest) == 0 {
		return fmt.Errorf("缺少命令；运行 hq help 查看用法")
	}
	if a.FromGateway {
		return a.dispatch(rest)
	}
	if rest[0] == "up" && a.ProductionRuntime && os.Getenv("HERDR_ENV") != "1" && a.MaintenancePane == "" {
		if a.Direct {
			return fmt.Errorf("宿主机冷启动直接使用 hq up；删除 --direct。--direct 不授予公司启动权")
		}
		if err := validateProductionRuntime(runtimePaths{Office: a.Office, HQRoot: a.HQRoot, DataDir: a.DataDir, ConfigPath: a.ConfigPath, HerdrBin: a.HerdrBin}); err != nil {
			return fmt.Errorf("宿主机冷启动根核验失败：%w", err)
		}
		if err := a.requireCompletedInit(); err != nil {
			return err
		}
		a.HostColdStart = true
		a.MaintenanceActor = "hq-up-host"
		return a.dispatch(rest)
	}
	if a.Direct && !isMaintenanceCommand(rest) {
		return fmt.Errorf("--direct 仅允许运维白名单命令 up/serve/rebuild/board --reindex；业务写只能经 gateway")
	}
	if isMaintenanceCommand(rest) {
		if err := a.authorizeMaintenance(); err != nil {
			return err
		}
		return a.dispatch(rest)
	}
	if isBusinessMutation(rest) || shouldUseGateway(rest) {
		if a.Direct {
			return fmt.Errorf("业务写只能经 gateway，--direct 不授予 mutation 权限")
		}
		socket, err := gatewaySocketPath(a.DataDir)
		if err != nil {
			return unavailablef("无法解析 HQ 网关地址：%v", err)
		}
		if _, err := os.Stat(socket); err == nil {
			return forwardToGateway(socket, rest, a.DryRun, a.JSON, a.Out, a.Err)
		}
		return unavailablef("HQ 网关未运行；先由运维白名单角色运行 hq up")
	}
	return a.dispatch(rest)
}

func isBusinessMutation(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "report", "issue", "message", "accept", "return", "close", "reminder":
		return true
	case "delivery":
		return len(args) > 1 && (args[1] == "retry" || args[1] == "resolve" || args[1] == "consume")
	case "nudge":
		return len(args) > 1 && (args[1] == "enqueue" || args[1] == "claim" || args[1] == "deliver" || args[1] == "reconcile")
	case "approval":
		return len(args) > 1 && args[1] != "show"
	case "case":
		return len(args) > 1 && (args[1] == "create" || args[1] == "escalate" || args[1] == "revise")
	case "staff":
		return len(args) > 1 && (args[1] == "add" || args[1] == "update" || args[1] == "remove")
	case "role":
		return len(args) > 1 && (args[1] == "add" || args[1] == "retire")
	default:
		return false
	}
}

func isMaintenanceCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "up", "serve", "rebuild", "reconcile", "estop", "runtime":
		return true
	case "index":
		return len(args) > 1 && args[1] == "rebuild"
	case "board":
		for _, arg := range args[1:] {
			if arg == "--reindex" || arg == "-reindex" || strings.HasPrefix(arg, "--reindex=") {
				return true
			}
		}
	}
	return false
}

func (a *App) authorizeMaintenance() error {
	if a.ProductionRuntime {
		paths := runtimePaths{
			Office: a.Office, HQRoot: a.HQRoot, DataDir: a.DataDir,
			ConfigPath: a.ConfigPath, HerdrBin: a.HerdrBin,
		}
		if err := validateProductionRuntime(paths); err != nil {
			return fmt.Errorf("运维根核验失败：%w", err)
		}
	}
	if a.Identity == nil {
		return fmt.Errorf("IdentityProvider 未注入，运维动作 fail-closed")
	}
	paneID := a.MaintenancePane
	if paneID == "" {
		if os.Getenv("HERDR_ENV") != "1" {
			return fmt.Errorf("运维动作必须在 herdr 工位内执行（HERDR_ENV=1）")
		}
		paneID = os.Getenv("HERDR_PANE_ID")
	}
	if paneID == "" {
		return fmt.Errorf("运维动作缺少调用者 pane_id")
	}
	actor, err := a.resolveIdentity(a.Config, paneID)
	if err != nil {
		return fmt.Errorf("运维白名单身份核验失败：%w", err)
	}
	rule, ok := a.Config.exactRule(actor.Name)
	if !ok || rule.Disabled || !rule.CanManageStaff {
		return permissionf("agent %s 不在运维白名单；仅 registry 精确登记、未停用且 can_manage_staff 的角色可执行", actor.Name)
	}
	a.MaintenancePane = paneID
	a.MaintenanceActor = actor.Name
	return nil
}

// deliver serves the hq-up lifecycle. Business notifications use the durable
// outbox path in processDelivery.
func (a *App) deliver(target, message string) error {
	if a.Transport == nil {
		return fmt.Errorf("DeliveryTransport 未注入，拒绝回落真实 herdr")
	}
	releaseRuntimeSeat, err := a.lockRuntimeSeat(target)
	if err != nil {
		return err
	}
	defer releaseRuntimeSeat()
	_, err = a.withRuntimeAdmission(RuntimeAdmissionRequest{Action: runtimeAdmissionAgentPrompt, Target: target}, func() error {
		attempt := a.deliverAttempt(target, message)
		if attempt.Outcome == transportSent {
			return nil
		}
		if attempt.Err != nil {
			return attempt.Err
		}
		return fmt.Errorf("transport outcome=%s", attempt.Outcome)
	})
	return err
}

func (a *App) dispatch(rest []string) error {
	if len(rest) == 0 {
		return fmt.Errorf("缺少命令；运行 hq help 查看用法")
	}
	switch rest[0] {
	case "whoami":
		return a.cmdWhoAmI(rest[1:])
	case "case":
		return a.cmdCase(rest[1:])
	case "project":
		return a.cmdProject(rest[1:])
	case "assignment":
		return a.cmdAssignment(rest[1:])
	case "report":
		return a.cmdReport(rest[1:])
	case "issue":
		return a.cmdIssue(rest[1:])
	case "approval":
		return a.cmdApproval(rest[1:])
	case "message":
		return a.cmdMessage(rest[1:])
	case "inbox":
		return a.cmdInbox(rest[1:])
	case "accept":
		return a.cmdAccept(rest[1:])
	case "return":
		return a.cmdReturn(rest[1:])
	case "close":
		return a.cmdClose(rest[1:])
	case "board":
		return a.cmdBoard(rest[1:])
	case "up":
		return a.cmdUp(rest[1:])
	case "doctor":
		return a.cmdDoctor(rest[1:])
	case "init":
		return a.cmdInit(rest[1:])
	case "patrol":
		return a.cmdPatrol(rest[1:])
	case "nudge":
		return a.cmdNudge(rest[1:])
	case "reminder":
		return a.cmdReminder(rest[1:])
	case "estop":
		return a.cmdEstop(rest[1:])
	case "session":
		return a.cmdSession(rest[1:])
	case "runtime":
		return a.cmdRuntime(rest[1:])
	case "staff":
		return a.cmdStaff(rest[1:])
	case "role":
		return a.cmdRole(rest[1:])
	case "history":
		return a.cmdHistory(rest[1:])
	case "flow":
		return a.cmdFlow(rest[1:])
	case "index":
		return a.cmdIndex(rest[1:])
	case "rebuild":
		return a.cmdRebuild(rest[1:])
	case "serve":
		return a.cmdServe(rest[1:])
	case "ping":
		return a.cmdPing(rest[1:])
	case "reconcile":
		return a.cmdReconcile(rest[1:])
	case "delivery":
		return a.cmdDelivery(rest[1:])
	case "version":
		return a.cmdVersion(rest[1:])
	case "help", "-h", "--help":
		printUsage(a.Out)
		return nil
	default:
		return fmt.Errorf("未知命令 %q；运行 hq help 查看用法", rest[0])
	}
}

func discoverOffice(explicit string) (string, error) {
	if explicit == "" {
		explicit = os.Getenv("HQ_OFFICE")
	}
	if explicit != "" {
		abs, err := filepath.Abs(explicit)
		if err != nil {
			return "", err
		}
		if !officeHasCanonicalConfig(abs) {
			return "", fmt.Errorf("--office 不是有效 CEO 办公室目录：%s；必须包含唯一组织注册表 %s", abs, defaultConfigPath(abs))
		}
		return abs, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for dir := cwd; ; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == "ceo-office" {
			if officeHasCanonicalConfig(dir) {
				return dir, nil
			}
		}
		candidate := filepath.Join(dir, "ceo-office")
		if officeHasCanonicalConfig(candidate) {
			return candidate, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
	}
	exe, err := os.Executable()
	if err == nil {
		for dir := filepath.Dir(exe); ; dir = filepath.Dir(dir) {
			if filepath.Base(dir) == "ceo-office" {
				if officeHasCanonicalConfig(dir) {
					return dir, nil
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}
	return "", fmt.Errorf("无法发现包含 tools/hq/config.yaml 的 ceo-office；请传 --office 或设置 HQ_OFFICE")
}

func officeHasCanonicalConfig(office string) bool {
	info, err := os.Lstat(defaultConfigPath(office))
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func (a *App) actor() (Actor, error) {
	if a.Identity == nil {
		return Actor{}, fmt.Errorf("IdentityProvider 未注入，拒绝回落真实 herdr")
	}
	paneID := a.CallerPane
	if paneID == "" {
		if os.Getenv("HERDR_ENV") != "1" {
			return Actor{}, fmt.Errorf("写操作必须在 herdr 工位内执行（HERDR_ENV=1）")
		}
		paneID = os.Getenv("HERDR_PANE_ID")
		if paneID == "" {
			return Actor{}, fmt.Errorf("缺少 HERDR_PANE_ID，无法识别发件人")
		}
	}
	return a.resolveIdentity(a.Config, paneID)
}

func (a *App) output(value any, text string) error {
	if a.JSON {
		value, err := makeJSONSafeForJavaScript(value)
		if err != nil {
			return err
		}
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(value)
	}
	_, err := fmt.Fprintln(a.Out, text)
	return err
}

func (a *App) newEvent(actor Actor, eventType, caseID string) (Event, error) {
	id, err := newEventID()
	if err != nil {
		return Event{}, err
	}
	return Event{
		Version: eventSchemaVersion, ID: id, CaseID: caseID,
		At: a.Store.NowTime().Format(time.RFC3339), Type: eventType,
		Actor: actor.Name, ActorLabel: actor.Label, ActorPaneID: actor.PaneID,
	}, nil
}

func (a *App) currentCase(caseID string) (*CaseState, error) {
	snapshot, err := a.Store.Snapshot(a.Config)
	if err != nil {
		return nil, err
	}
	state, ok := snapshot.Cases[caseID]
	if !ok {
		return nil, fmt.Errorf("case 不存在：%s；先运行 hq case create", caseID)
	}
	return state, nil
}

func (a *App) cmdWhoAmI(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("用法：hq whoami")
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	return a.output(actor, fmt.Sprintf("[%s] agent=%s department=%s pane=%s reports_to=%s", actor.Label, actor.Name, actor.Department, actor.PaneID, actor.Rule.ReportsTo))
}

func (a *App) cmdCase(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("用法：hq case create|escalate|revise|show ...")
	}
	switch args[0] {
	case "create":
		return a.cmdCaseCreate(args[1:])
	case "escalate":
		return a.cmdCaseEscalate(args[1:])
	case "revise":
		return a.cmdCaseRevise(args[1:])
	case "show":
		return a.cmdCaseShow(args[1:])
	default:
		return fmt.Errorf("未知 case 子命令 %q", args[0])
	}
}

type CaseSpec struct {
	CaseID, ParentCaseID, RootCaseID                                                 string
	Title, Project, Objective, Acceptance, Constraints, Priority, SpecRef, SourceRef string
	Version                                                                          int
	Digest                                                                           string
	projectExplicit                                                                  bool
}

const maxCaseBodyRunes = 2000

func validateCaseBody(name, value string, required bool) (string, error) {
	value = strings.TrimSpace(value)
	if required && value == "" {
		return "", fmt.Errorf("缺少 --%s", name)
	}
	if utf8.RuneCountInString(value) > maxCaseBodyRunes {
		return "", fmt.Errorf("--%s 超过 %d rune；复杂规格请写入 Markdown 并使用 --spec-ref", name, maxCaseBodyRunes)
	}
	if containsSensitive(value) {
		return "", fmt.Errorf("--%s 疑似包含密钥或金额，拒绝写入 case", name)
	}
	return value, nil
}

func caseSpecDigest(spec CaseSpec) string {
	copy := spec
	copy.Digest = ""
	return canonicalJSONDigest(copy)
}

func eventCaseSpecDigest(event Event) string {
	return caseSpecDigest(CaseSpec{
		CaseID: event.CaseID, ParentCaseID: event.ParentCaseID, RootCaseID: event.RootCaseID,
		Title: event.Title, Project: event.Project, Objective: event.Objective,
		Acceptance: event.Acceptance, Constraints: event.Constraints, Priority: event.Priority,
		SpecRef: event.SpecRef, SourceRef: event.SourceRef, Version: event.CaseVersion,
	})
}

func newHQSpaceGuidance() string {
	return "新项目请新建独立 HQ 目录和 Herdr workspace，然后运行 hq init NEW_HQ_DIR --workspace NEW_WORKSPACE --template virtual-company"
}

func occupiedHQSpaceError(root *CaseState) error {
	if root == nil {
		return conflictf("HQ space 已包含业务 case，不允许再创建 root/project；%s", newHQSpaceGuidance())
	}
	return conflictf("HQ space 已绑定唯一 project=%q root=%s，不允许第二个 root/project；%s", root.Project, root.ID, newHQSpaceGuidance())
}

func (a *App) parseCaseSpec(command string, args []string) (CaseSpec, string, error) {
	fs := newLeafParser(command)
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "稳定 case_id")
	title := fs.String("title", "", "标题")
	project := fs.String("project", "", "项目")
	parent := fs.String("parent", "", "父 case_id")
	objective := fs.String("objective", "", "目标")
	acceptance := fs.String("acceptance", "", "验收条件")
	constraints := fs.String("constraints", "", "约束")
	priority := fs.String("priority", "", "P0|P1|P2")
	specRef := fs.String("spec-ref", "", "复杂规格 Markdown 引用")
	source := fs.String("source", "", "权威来源")
	owner := fs.String("owner", "", "初始负责人")
	versionText := fs.String("version", "", "版本")
	if err := fs.Parse(args); err != nil {
		return CaseSpec{}, "", err
	}
	projectExplicit := fs.Changed("project")
	cleanID := strings.TrimSpace(*id)
	if err := validateCaseID(cleanID); err != nil {
		return CaseSpec{}, "", err
	}
	cleanTitle, err := validateShortText("title", *title, true)
	if err != nil {
		return CaseSpec{}, "", err
	}
	cleanProject, err := validateShortText("project", *project, false)
	if err != nil {
		return CaseSpec{}, "", err
	}
	if projectExplicit && cleanProject == "" {
		return CaseSpec{}, "", fmt.Errorf("--project 已显式提供时不得为空；若要继承或保留项目，请省略 --project")
	}
	cleanObjective, err := validateCaseBody("objective", *objective, false)
	if err != nil {
		return CaseSpec{}, "", err
	}
	cleanAcceptance, err := validateCaseBody("acceptance", *acceptance, false)
	if err != nil {
		return CaseSpec{}, "", err
	}
	cleanConstraints, err := validateCaseBody("constraints", *constraints, false)
	if err != nil {
		return CaseSpec{}, "", err
	}
	cleanPriority := strings.ToUpper(strings.TrimSpace(*priority))
	if cleanObjective == "" {
		cleanObjective = cleanTitle
	}
	if cleanAcceptance == "" {
		cleanAcceptance = "按 case 来源与负责人要求验收"
	}
	if cleanConstraints == "" {
		cleanConstraints = "遵守岗位手册与 HQ 权限边界"
	}
	if cleanPriority == "" {
		cleanPriority = "P1"
	}
	if cleanPriority != "P0" && cleanPriority != "P1" && cleanPriority != "P2" {
		return CaseSpec{}, "", fmt.Errorf("--priority 只能是 P0/P1/P2")
	}
	cleanSpec, err := normalizeRef(*specRef, a.HQRoot, false)
	if err != nil {
		return CaseSpec{}, "", fmt.Errorf("spec-ref：%w", err)
	}
	if cleanSpec != "" && strings.ToLower(filepath.Ext(strings.Split(cleanSpec, "#")[0])) != ".md" {
		return CaseSpec{}, "", fmt.Errorf("spec-ref 必须引用 Markdown")
	}
	cleanSource, err := normalizeRef(*source, a.HQRoot, true)
	if err != nil {
		return CaseSpec{}, "", fmt.Errorf("source：%w", err)
	}
	version := 1
	if strings.TrimSpace(*versionText) != "" {
		version, err = strconv.Atoi(strings.TrimSpace(*versionText))
		if err != nil || version < 1 {
			return CaseSpec{}, "", fmt.Errorf("--version 必须是正整数")
		}
	}
	spec := CaseSpec{CaseID: cleanID, ParentCaseID: strings.TrimSpace(*parent), Title: cleanTitle, Project: cleanProject,
		Objective: cleanObjective, Acceptance: cleanAcceptance, Constraints: cleanConstraints, Priority: cleanPriority,
		SpecRef: cleanSpec, SourceRef: cleanSource, Version: version, projectExplicit: projectExplicit}
	spec.Digest = caseSpecDigest(spec)
	return spec, strings.TrimSpace(*owner), nil
}

func (a *App) cmdCaseCreate(args []string) error {
	spec, requestedOwner, err := a.parseCaseSpec("case create", args)
	if err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if !actor.Rule.CanCreate {
		return permissionf("agent %s 无权创建公司 case", actor.Name)
	}
	if spec.ParentCaseID == "" && !spec.projectExplicit {
		return fmt.Errorf("一个 HQ space 只承载一个 Project；创建唯一 root case 必须显式提供 --project NAME")
	}
	if spec.ParentCaseID != "" && spec.projectExplicit {
		return fmt.Errorf("child case 的 project 由唯一 root/parent 自动继承；删除 --project 后重试")
	}
	cleanOwner := requestedOwner
	if cleanOwner == "" {
		cleanOwner = actor.Name
	}
	if _, ok := a.Config.ruleFor(cleanOwner); !ok {
		return fmt.Errorf("owner 未登记：%s", cleanOwner)
	}
	// Resolve all state-derived fields before freezing the command digest.  An
	// exact retry recovers those fields from the already committed creation
	// event, so a later parent transition cannot turn a successful retry into a
	// different request.  A new child instead freezes a parent basis that is
	// checked again under the transaction lock below.
	preflightLedger, err := a.ledgerState()
	if err != nil {
		return err
	}
	commandID := stableCommandID("case-create", spec.CaseID)
	var parentBasis *CaseState
	if committed, ok := preflightLedger.commands[commandID]; ok &&
		committed.Type == "case_created" && committed.CaseID == spec.CaseID &&
		committed.ParentCaseID == spec.ParentCaseID {
		spec.RootCaseID = committed.RootCaseID
		if !spec.projectExplicit {
			spec.Project = committed.Project
		}
		spec.Digest = caseSpecDigest(spec)
	} else if spec.ParentCaseID != "" {
		if err := validateCaseID(spec.ParentCaseID); err != nil {
			return fmt.Errorf("parent：%w", err)
		}
		parent, err := preflightLedger.currentCase(spec.ParentCaseID)
		if err != nil {
			return fmt.Errorf("父 case：%w", err)
		}
		if parent.Status == string(statusClosed) {
			return conflictf("父 case %s 已关闭，不能新增 child；本 HQ space 的项目树已归档；%s", parent.ID, newHQSpaceGuidance())
		}
		if parent.Status == string(statusEscalated) {
			eventID := preflightLedger.escalationReviewEventID(parent.ID, parent.LastEventID)
			return conflictf("父 case %s 正在等待上级核验 escalation；actor=%s 先运行 hq accept --event %s --next TEXT，或 hq return --event %s --reason TEXT --next TEXT，再拆分子 case", parent.ID, parent.Owner, eventID, eventID)
		}
		if parent.Owner != actor.Name {
			return permissionf("只有父 case 当前负责人可拆分子 case；父 case=%s 当前负责人=%s", parent.ID, parent.Owner)
		}
		spec.Project = parent.Project
		if cleanOwner != actor.Name {
			return fmt.Errorf("子 case 创建时 owner 必须仍是拆解它的经理；请创建后用 hq issue 委派")
		}
		spec.RootCaseID = parent.RootCaseID
		if spec.RootCaseID == "" {
			spec.RootCaseID = parent.ID
		}
		spec.Digest = caseSpecDigest(spec)
		basis := *parent
		parentBasis = &basis
	} else if len(preflightLedger.snapshot.Cases) != 0 {
		return occupiedHQSpaceError(preflightLedger.soleRootCase())
	}
	digest := requestDigest("case-create", actor.Name, spec.CaseID, spec.Digest, cleanOwner)
	result, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		if _, exists := ledger.snapshot.Cases[spec.CaseID]; exists {
			return Event{}, fmt.Errorf("case 已存在：%s", spec.CaseID)
		}
		if spec.ParentCaseID != "" {
			if err := validateCaseID(spec.ParentCaseID); err != nil {
				return Event{}, fmt.Errorf("parent：%w", err)
			}
			parent, err := ledger.currentCase(spec.ParentCaseID)
			if err != nil {
				return Event{}, fmt.Errorf("父 case：%w", err)
			}
			if parentBasis == nil || parent.Version != parentBasis.Version ||
				parent.Digest != parentBasis.Digest || parent.LastEventID != parentBasis.LastEventID {
				return Event{}, conflictf("父 case %s 在 child create admission 期间已变化；请运行 hq case show --id %s 后重新创建", parent.ID, parent.ID)
			}
			if parent.Status == string(statusClosed) {
				return Event{}, conflictf("父 case %s 已关闭，不能新增 child；本 HQ space 的项目树已归档；%s", parent.ID, newHQSpaceGuidance())
			}
			if parent.Status == string(statusEscalated) {
				eventID := ledger.escalationReviewEventID(parent.ID, parent.LastEventID)
				return Event{}, conflictf("父 case %s 正在等待上级核验 escalation；actor=%s 先运行 hq accept --event %s --next TEXT，或 hq return --event %s --reason TEXT --next TEXT，再拆分子 case", parent.ID, parent.Owner, eventID, eventID)
			}
			if parent.Owner != actor.Name {
				return Event{}, permissionf("只有父 case 当前负责人可拆分子 case；父 case=%s 当前负责人=%s", parent.ID, parent.Owner)
			}
			if spec.Project != parent.Project {
				return Event{}, conflictf("child case 的冻结 project 与 parent %s 不一致；请重新读取 parent 后重试", parent.ID)
			}
			if cleanOwner != actor.Name {
				return Event{}, fmt.Errorf("子 case 创建时 owner 必须仍是拆解它的经理；请创建后用 hq issue 委派")
			}
			expectedRoot := parent.RootCaseID
			if expectedRoot == "" {
				expectedRoot = parent.ID
			}
			if spec.RootCaseID != expectedRoot {
				return Event{}, conflictf("child case root 与 parent %s 的冻结 lineage 不一致；请重新读取父 case 后创建", parent.ID)
			}
		} else if len(ledger.snapshot.Cases) != 0 {
			return Event{}, occupiedHQSpaceError(ledger.soleRootCase())
		}
		event, err := a.newEvent(actor, "case_created", spec.CaseID)
		if err != nil {
			return Event{}, err
		}
		event.ParentCaseID, event.RootCaseID = spec.ParentCaseID, spec.RootCaseID
		event.Title, event.Project, event.SourceRef = spec.Title, spec.Project, spec.SourceRef
		event.Objective, event.Acceptance, event.Constraints, event.Priority, event.SpecRef = spec.Objective, spec.Acceptance, spec.Constraints, spec.Priority, spec.SpecRef
		event.CaseVersion, event.CaseDigest = spec.Version, spec.Digest
		event.ToState, event.Owner, event.NextAction = "open", cleanOwner, "由负责人接续处理"
		return event, nil
	})
	if err != nil {
		return err
	}
	event := result.Event
	if a.DryRun {
		return a.output(event, fmt.Sprintf("DRY-RUN：将创建 %s；owner=%s", event.CaseID, event.Owner))
	}
	return a.output(event, fmt.Sprintf("已创建 %s；event=%s；owner=%s", event.CaseID, event.ID, event.Owner))
}

func (a *App) cmdCaseRevise(args []string) error {
	spec, requestedOwner, err := a.parseCaseSpec("case revise", args)
	if err != nil {
		return err
	}
	if spec.ParentCaseID != "" || requestedOwner != "" {
		return fmt.Errorf("case revise 不接受 --parent 或 --owner；修订只更新规格，父子关系与当前 owner 保持不变，请删除这两个参数后重试")
	}
	if spec.projectExplicit {
		return fmt.Errorf("project identity 在唯一 root 创建时冻结；case revise 不接受 --project；%s", newHQSpaceGuidance())
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	// Revision inheritance is part of the request contract, not a transaction
	// builder side effect.  Read and freeze it before calculating the command
	// digest, then use a CAS check under the ledger lock.
	preflightLedger, err := a.ledgerState()
	if err != nil {
		return err
	}
	commandID := stableCommandID("case-revise", actor.Name, spec.CaseID, strconv.Itoa(spec.Version))
	var stateBasis *CaseState
	lineageBasis := ""
	if committed, ok := preflightLedger.commands[commandID]; ok &&
		committed.Type == "case_revised" && committed.CaseID == spec.CaseID &&
		committed.CaseVersion == spec.Version {
		spec.ParentCaseID, spec.RootCaseID = committed.ParentCaseID, committed.RootCaseID
		spec.Project = committed.Project
		spec.Digest = caseSpecDigest(spec)
	} else {
		state, err := preflightLedger.currentCase(spec.CaseID)
		if err != nil {
			return err
		}
		if state.Owner != actor.Name {
			return permissionf("只有 case 当前负责人可修订规格；当前负责人=%s", state.Owner)
		}
		if state.Status == string(statusEscalated) {
			eventID := preflightLedger.escalationReviewEventID(state.ID, state.LastEventID)
			return conflictf("case %s 正在等待上级核验 escalation；actor=%s 先运行 hq accept --event %s --next TEXT，或 hq return --event %s --reason TEXT --next TEXT，再 revise", state.ID, state.Owner, eventID, eventID)
		}
		if err := preflightLedger.rejectActiveAssignment(spec.CaseID, "修订规格"); err != nil {
			return err
		}
		if spec.Version != state.Version+1 {
			return fmt.Errorf("case 新版本必须是 %d，实际=%d", state.Version+1, spec.Version)
		}
		spec.ParentCaseID, spec.RootCaseID = state.ParentCaseID, state.RootCaseID
		spec.Project = state.Project
		if err := preflightLedger.validateCaseProjectLineage(spec.CaseID, spec.ParentCaseID, spec.Project); err != nil {
			return conflictf("case revise project 不合法：%v", err)
		}
		spec.Digest = caseSpecDigest(spec)
		basis := *state
		stateBasis = &basis
		lineageBasis = preflightLedger.caseProjectLineageBasis(spec.CaseID)
	}
	digest := requestDigest("case-revise", actor.Name, spec.CaseID, strconv.Itoa(spec.Version), spec.Digest)
	result, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		state, err := ledger.currentCase(spec.CaseID)
		if err != nil {
			return Event{}, err
		}
		if stateBasis == nil || state.Version != stateBasis.Version ||
			state.Digest != stateBasis.Digest || state.LastEventID != stateBasis.LastEventID {
			return Event{}, conflictf("case %s 在 revise admission 期间已变化；请运行 hq case show --id %s 后基于最新版本重试", state.ID, state.ID)
		}
		if lineageBasis == "" || ledger.caseProjectLineageBasis(spec.CaseID) != lineageBasis {
			return Event{}, conflictf("case %s 的 project lineage 在 revise admission 期间已变化；请重新读取 parent/children 后重试", state.ID)
		}
		if state.Owner != actor.Name {
			return Event{}, permissionf("只有 case 当前负责人可修订规格；当前负责人=%s", state.Owner)
		}
		if state.Status == string(statusEscalated) {
			eventID := ledger.escalationReviewEventID(state.ID, state.LastEventID)
			return Event{}, conflictf("case %s 正在等待上级核验 escalation；actor=%s 先运行 hq accept --event %s --next TEXT，或 hq return --event %s --reason TEXT --next TEXT，再 revise", state.ID, state.Owner, eventID, eventID)
		}
		if err := ledger.rejectActiveAssignment(spec.CaseID, "修订规格"); err != nil {
			return Event{}, err
		}
		if spec.Version != state.Version+1 {
			return Event{}, fmt.Errorf("case 新版本必须是 %d，实际=%d", state.Version+1, spec.Version)
		}
		if spec.ParentCaseID != state.ParentCaseID || spec.RootCaseID != state.RootCaseID {
			return Event{}, conflictf("case %s 的 parent/root lineage 在 revise admission 期间已变化；请重新读取后重试", state.ID)
		}
		if spec.Project != state.Project {
			return Event{}, conflictf("case %s 的 project identity 已冻结，不可修订；%s", state.ID, newHQSpaceGuidance())
		}
		if err := ledger.validateCaseProjectLineage(spec.CaseID, spec.ParentCaseID, spec.Project); err != nil {
			return Event{}, conflictf("case revise project 不合法：%v", err)
		}
		event, err := a.newEvent(actor, "case_revised", spec.CaseID)
		if err != nil {
			return Event{}, err
		}
		basis := state.SpecEventID
		if basis == "" {
			basis = state.LastEventID
		}
		event.RelatedEventID, event.PreviousCaseDigest = basis, state.Digest
		event.ParentCaseID, event.RootCaseID = spec.ParentCaseID, spec.RootCaseID
		event.Title, event.Project, event.SourceRef = spec.Title, spec.Project, spec.SourceRef
		event.Objective, event.Acceptance, event.Constraints, event.Priority, event.SpecRef = spec.Objective, spec.Acceptance, spec.Constraints, spec.Priority, spec.SpecRef
		event.CaseVersion, event.CaseDigest = spec.Version, spec.Digest
		return event, nil
	})
	if err != nil {
		return err
	}
	return a.output(result.Event, fmt.Sprintf("case=%s version=%d digest=%s event=%s", result.Event.CaseID, result.Event.CaseVersion, result.Event.CaseDigest, result.Event.ID))
}

func (a *App) cmdCaseShow(args []string) error {
	fs := newLeafParser("case show")
	fs.SetOutput(a.Err)
	id := fs.String("id", "", "case_id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateCaseID(*id); err != nil {
		return err
	}
	state, err := a.currentCase(*id)
	if err != nil {
		return err
	}
	text := fmt.Sprintf("%s [%s] parent=%s owner=%s version=%d priority=%s next=%s source=%s", state.ID, state.Status, state.ParentCaseID, state.Owner, state.Version, state.Priority, state.NextAction, state.SourceRef)
	return a.output(state, text)
}

func consumedAssignmentReportError(state *CaseState, actor string) error {
	return conflictf("case %s 上你原来的 assignment 已进入终态并被消费，不能靠 message 或再次 report 重开；旧 submission 保持不可变。当前 owner=%s 必须建立新的 case 版本和 fresh assignment：hq case revise --id %s --version %d --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 --source PATH；随后由 %s 运行 hq issue --case %s --to %s --next TEXT", state.ID, state.Owner, state.ID, state.Version+1, state.Owner, state.ID, actor)
}

type reportEvidenceField struct {
	name  string
	value string
}

func missingReportEvidence(fields ...reportEvidenceField) []string {
	missing := make([]string, 0, len(fields))
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			missing = append(missing, field.name)
		}
	}
	return missing
}

func (a *App) cmdReport(args []string) error {
	fs := newLeafParser("report")
	fs.SetOutput(a.Err)
	caseID := fs.String("case", "", "case_id")
	result := fs.String("result", "", "completed|blocked|needs-decision|finding|returned")
	source := fs.String("source", "", "依据原文路径[#定位]")
	artifact := fs.String("artifact", "", "产出路径[#定位] 或 git:/repo@sha")
	severity := fs.String("severity", "", "P0|P1|P2")
	location := fs.String("location", "", "精确定位")
	verification := fs.String("verify", "", "复验条件（合法 UTF-8 单行，不超过 2 KiB）")
	next := fs.String("next", "", "下一步")
	note := fs.String("note", "", "新增事实（合法 UTF-8 单行，不超过 2 KiB）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if err := validateCaseID(*caseID); err != nil {
		return err
	}
	_, _, err = reportTargetState(*result)
	if err != nil {
		return err
	}
	cleanSource, err := normalizeRef(*source, a.HQRoot, false)
	if err != nil {
		return fmt.Errorf("source：%w", err)
	}
	cleanArtifact, err := normalizeRef(*artifact, a.HQRoot, false)
	if err != nil {
		return fmt.Errorf("artifact：%w", err)
	}
	cleanSeverity, err := validateSeverity(*severity)
	if err != nil {
		return err
	}
	cleanLocation, err := validateShortText("location", *location, false)
	if err != nil {
		return err
	}
	cleanVerification, err := validateBusinessText("verify", *verification, false)
	if err != nil {
		return err
	}
	cleanNext, err := validateBusinessText("next", *next, true)
	if err != nil {
		return err
	}
	cleanNote, err := validateBusinessText("note", *note, false)
	if err != nil {
		return err
	}
	switch *result {
	case "completed":
		if cleanArtifact == "" && cleanSource == "" {
			return usagef("report result=completed 缺少可复验证据：至少提供 --artifact PATH 或 --source PATH；可执行模板：hq report --case %s --result completed --artifact PATH --verify TEXT --next TEXT", *caseID)
		}
	case "finding":
		missing := missingReportEvidence(
			reportEvidenceField{"--severity", cleanSeverity},
			reportEvidenceField{"--source", cleanSource},
			reportEvidenceField{"--location", cleanLocation},
			reportEvidenceField{"--verify", cleanVerification},
		)
		if len(missing) != 0 {
			return usagef("report result=finding 缺少条件必填参数 %s；可执行模板：hq report --case %s --result finding --severity P1 --source PATH --location TEXT --verify TEXT --next TEXT", strings.Join(missing, "、"), *caseID)
		}
	case "blocked", "needs-decision", "returned":
		missing := missingReportEvidence(
			reportEvidenceField{"--source", cleanSource},
			reportEvidenceField{"--note", cleanNote},
		)
		if len(missing) != 0 {
			return usagef("report result=%s 缺少条件必填参数 %s；可执行模板：hq report --case %s --result %s --source PATH --note TEXT --next TEXT", *result, strings.Join(missing, "、"), *caseID, *result)
		}
	}
	preflightLedger, err := a.ledgerState()
	if err != nil {
		return err
	}
	preflightState, err := preflightLedger.currentCase(*caseID)
	if err != nil {
		return err
	}
	preflightAssignment, preflightHasAssignment := preflightLedger.assignmentRecordFor(*caseID, actor.Name)
	preflightCaseGeneration := preflightLedger.caseGeneration(*caseID)
	reportGeneration := ""
	if preflightHasAssignment {
		if err := a.verifyCurrentFrozenSeat(frozenSeatFromAssignment(preflightAssignment)); err != nil {
			return fmt.Errorf("report 前冻结席位核验失败：%w", err)
		}
		submissionGeneration := preflightAssignment.SubmissionGeneration
		if submissionGeneration == "" {
			submissionGeneration = preflightAssignment.EventID
		}
		reportGeneration = strings.Join([]string{"assignment", preflightAssignment.AssignmentID, preflightAssignment.AssignmentDigest, submissionGeneration}, ":")
	} else {
		if err := preflightLedger.rejectOwnerReportOverActiveAssignment(*caseID); err != nil {
			return err
		}
		if preflightState.Owner != actor.Name {
			if preflightLedger.hasEverReceivedCaseAssignment(*caseID, actor.Name) {
				return consumedAssignmentReportError(preflightState, actor.Name)
			}
			return fmt.Errorf("agent %s 不是 case %s 当前 owner，且没有未消费的有效 assignment", actor.Name, *caseID)
		}
		if actor.Rule.ReportsTo == "" {
			return fmt.Errorf("agent %s 没有登记回铃上级", actor.Name)
		}
		if preflightCaseGeneration == "" {
			return fmt.Errorf("case %s 缺少稳定 business generation", *caseID)
		}
		reportGeneration = strings.Join([]string{"owner", preflightState.Digest, preflightCaseGeneration, actor.Rule.ReportsTo}, ":")
	}
	commandID := stableCommandID("report", actor.Name, *caseID, reportGeneration, *result, cleanSource, cleanArtifact, cleanNext)
	digest := requestDigest("report", actor.Name, *caseID, reportGeneration, *result, cleanSource, cleanArtifact, cleanSeverity, cleanLocation, cleanVerification, cleanNext, cleanNote)
	resultTxn, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		state, err := ledger.currentCase(*caseID)
		if err != nil {
			return Event{}, err
		}
		assignment, hasAssignment := ledger.assignmentRecordFor(*caseID, actor.Name)
		if hasAssignment != preflightHasAssignment || ledger.caseGeneration(*caseID) != preflightCaseGeneration {
			return Event{}, fmt.Errorf("case/assignment 在 report admission 期间已变化；请读取最新状态后重试")
		}
		if hasAssignment && (assignment.EventID != preflightAssignment.EventID || assignment.AssignmentDigest != preflightAssignment.AssignmentDigest ||
			assignment.Status != preflightAssignment.Status || assignment.SubmissionGeneration != preflightAssignment.SubmissionGeneration ||
			assignment.SubmissionEventID != preflightAssignment.SubmissionEventID) {
			return Event{}, fmt.Errorf("assignment 在 report admission 期间已变化；请读取最新合同后重试")
		}
		if hasAssignment {
			if _, err := currentSeatForFrozenContract(a.Config, frozenSeatFromAssignment(assignment)); err != nil {
				return Event{}, fmt.Errorf("report admission 冻结席位核验失败：%w", err)
			}
			if !assignment.Accepted {
				return Event{}, fmt.Errorf("assignment %s 尚未由 assignee accept，不可 report", assignment.AssignmentID)
			}
			if assignment.Status != "accepted" && assignment.Status != "rework" {
				return Event{}, fmt.Errorf("assignment %s status=%s，不可创建新 submission", assignment.AssignmentID, assignment.Status)
			}
			if assignment.SubmissionEventID != "" {
				return Event{}, fmt.Errorf("assignment %s 本轮已有 submission=%s；必须复用其 delivery 或等待 reviewer return", assignment.AssignmentID, assignment.SubmissionEventID)
			}
		}
		var recipientRule AgentRule
		if hasAssignment {
			var ok bool
			recipientRule, ok = a.Config.exactRule(assignment.Acceptor)
			if !ok || !recipientRule.CanAccept {
				return Event{}, fmt.Errorf("assignment acceptor 未登记、已停用或无 can_accept：%s", assignment.Acceptor)
			}
		} else {
			if err := ledger.rejectOwnerReportOverActiveAssignment(*caseID); err != nil {
				return Event{}, err
			}
			if state.Owner != actor.Name {
				if ledger.hasEverReceivedCaseAssignment(*caseID, actor.Name) {
					return Event{}, consumedAssignmentReportError(state, actor.Name)
				}
				return Event{}, fmt.Errorf("agent %s 不是 case %s 当前 owner，且没有未消费的有效 assignment", actor.Name, *caseID)
			}
			if actor.Rule.ReportsTo == "" {
				return Event{}, fmt.Errorf("agent %s 没有登记回铃上级", actor.Name)
			}
			var ok bool
			recipientRule, ok = a.Config.exactRule(actor.Rule.ReportsTo)
			if !ok {
				return Event{}, fmt.Errorf("回铃接收方未登记：%s", actor.Rule.ReportsTo)
			}
		}
		prepared, err := a.newEvent(actor, "report_prepared", *caseID)
		if err != nil {
			return Event{}, err
		}
		prepared.FromState, prepared.Title, prepared.Project = state.Status, state.Title, state.Project
		prepared.Recipient, prepared.RecipientLabel = recipientRule.Name, recipientRule.Label
		if hasAssignment {
			prepared.Recipient, prepared.RecipientLabel = assignment.Acceptor, assignment.AcceptorLabel
			copyAssignmentStateBinding(&prepared, assignment)
			prepared.CaseVersion, prepared.CaseDigest = assignment.CaseVersion, assignment.CaseDigest
		} else {
			prepared.CaseVersion, prepared.CaseDigest = state.Version, state.Digest
		}
		prepared.Result, prepared.Severity = *result, cleanSeverity
		prepared.SourceRef, prepared.ArtifactRef = cleanSource, cleanArtifact
		prepared.Location, prepared.Verification = cleanLocation, cleanVerification
		prepared.NextAction, prepared.Note = cleanNext, cleanNote
		prepared.Delivery, prepared.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, recipientRule.Name)
		payload, err := a.deliveryPayload(prepared)
		if err != nil {
			return Event{}, err
		}
		prepared.PayloadDigest = digestText(payload)
		return prepared, nil
	})
	if err != nil {
		return err
	}
	prepared := resultTxn.Event
	if a.DryRun {
		return a.output(prepared, fmt.Sprintf("DRY-RUN：将由[%s]回铃 %s，case=%s，delivery=%s", actor.Label, prepared.Recipient, *caseID, prepared.DeliveryID))
	}
	deliveryOutcome, deliveryErr := a.processDelivery(prepared, "")
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(deliveryOutcome, "")
		}
		return deliveryErr
	}
	return a.output(deliveryOutcome, fmt.Sprintf("业务已提交；回铃已送达 %s；case=%s；event=%s；delivery=%s", prepared.Recipient, *caseID, prepared.ID, prepared.DeliveryID))
}

func (a *App) cmdInbox(args []string) error {
	fs := newLeafParser("inbox")
	fs.SetOutput(a.Err)
	forAgent := fs.String("for", "", "查看指定 agent 收件箱（需跨角色核验权限）")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	target := strings.TrimSpace(*forAgent)
	if target == "" {
		target = actor.Name
	}
	if target != actor.Name && !a.Config.canCloseAsAccount(actor.Rule) {
		return permissionf("当前角色无权查看其他角色的收件箱")
	}
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return err
	}
	pending := pendingEvents(events, target)
	if a.JSON {
		return a.output(pending, "")
	}
	if len(pending) == 0 {
		_, err := fmt.Fprintf(a.Out, "%s 无待核验事件。\n", target)
		return err
	}
	fmt.Fprintf(a.Out, "%-34s %-18s %-18s %-16s %s\n", "EVENT", "CASE", "TYPE", "FROM", "NEXT")
	for _, event := range pending {
		fmt.Fprintf(a.Out, "%-34s %-18s %-18s %-16s %s\n", event.ID, event.CaseID, event.Type, event.Actor, event.NextAction)
	}
	return nil
}

func (a *App) cmdAccept(args []string) error {
	fs := newLeafParser("accept")
	fs.SetOutput(a.Err)
	eventID := fs.String("event", "", "待核验 event_id")
	next := fs.String("next", "", "下一步")
	note := fs.String("note", "", "核验说明")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if !actor.Rule.CanAccept {
		return permissionf("agent %s 无核验权限", actor.Name)
	}
	cleanNext, err := validateBusinessText("next", *next, true)
	if err != nil {
		return err
	}
	cleanNote, err := validateBusinessText("note", *note, false)
	if err != nil {
		return err
	}
	actionID := strings.TrimSpace(*eventID)
	if err := a.preflightActionFrozenSeat(actionID, actor.Name); err != nil {
		return fmt.Errorf("accept 前冻结席位核验失败（含角色手册）：%w", err)
	}
	commandID := stableCommandID("accept", actionID)
	digest := requestDigest("accept", actor.Name, actionID, cleanNext, cleanNote)
	resultTxn, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		originalEvent, ok := ledger.actionableEvent(actionID)
		original, semanticOK := semanticDeliveredEvent(originalEvent, ledger.events)
		if !ok || !semanticOK {
			return Event{}, fmt.Errorf("找不到可核验的已送达事件：%s", actionID)
		}
		if original.Recipient != actor.Name {
			return Event{}, permissionf("事件接收方是 %s，当前角色 %s 无权核验", original.Recipient, actor.Name)
		}
		contract, required, err := frozenSeatForDeliveredBusinessEvent(ledger, original)
		if err != nil {
			return Event{}, err
		}
		if required {
			if _, err := currentSeatForFrozenContract(a.Config, contract); err != nil {
				return Event{}, fmt.Errorf("accept admission 冻结席位核验失败：%w", err)
			}
		}
		if ledger.resolved[originalEvent.ID] {
			return Event{}, fmt.Errorf("事件 %s 已处理", originalEvent.ID)
		}
		toState, err := acceptTargetState(original)
		if err != nil {
			return Event{}, err
		}
		state, err := ledger.currentCase(original.CaseID)
		if err != nil {
			return Event{}, err
		}
		accepted, err := a.newEvent(actor, "event_accepted", original.CaseID)
		if err != nil {
			return Event{}, err
		}
		accepted.RelatedEventID, accepted.FromState, accepted.ToState = originalEvent.ID, state.Status, toState
		accepted.Owner, accepted.NextAction, accepted.Note = actor.Name, cleanNext, cleanNote
		accepted.Recipient = original.Actor
		accepted.RecipientLabel = original.ActorLabel
		accepted.SourceRef, accepted.ArtifactRef, accepted.Verification = original.SourceRef, original.ArtifactRef, original.Verification
		accepted.CaseVersion, accepted.CaseDigest = original.CaseVersion, original.CaseDigest
		copyAssignmentBinding(&accepted, original)
		accepted.AcceptanceDigest = acceptanceReceiptDigest(originalEvent.ID, original, accepted)
		accepted.Delivery, accepted.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, original.Actor)
		payload, err := a.deliveryPayload(accepted)
		if err != nil {
			return Event{}, err
		}
		accepted.PayloadDigest = digestText(payload)
		return accepted, nil
	})
	if err != nil {
		return err
	}
	accepted := resultTxn.Event
	if a.DryRun {
		return a.output(accepted, fmt.Sprintf("DRY-RUN：将接受 event=%s，delivery=%s", actionID, accepted.DeliveryID))
	}
	deliveryOutcome, deliveryErr := a.processDelivery(accepted, "")
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(deliveryOutcome, "")
		}
		return deliveryErr
	}
	summary := fmt.Sprintf("业务已提交并已通知；accepted=%s；case=%s；delivery=%s", accepted.ID, accepted.CaseID, accepted.DeliveryID)
	_, err = a.consumeDeliveryContextAtomically(actor, 0, "accept:"+accepted.ID, func(batch deliveryContextBatch) error {
		outcome := deliveryOutcome
		outcome.HQMessages = batch.Items
		if a.JSON || len(batch.Records) == 0 {
			return a.output(outcome, summary)
		}
		if err := writeDeliveryContext(a.Out, []byte(summary+"\n\n")); err != nil {
			return err
		}
		for _, envelope := range batch.Envelopes {
			if err := writeDeliveryContext(a.Out, []byte(envelope+"\n\n")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return a.resetDeliveryBudget(actor)
}

func (a *App) cmdReturn(args []string) error {
	fs := newLeafParser("return")
	fs.SetOutput(a.Err)
	eventID := fs.String("event", "", "待退回 event_id")
	reason := fs.String("reason", "", "退回原因")
	next := fs.String("next", "", "复交条件/下一步")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if !actor.Rule.CanAccept {
		return permissionf("agent %s 无核验退回权限", actor.Name)
	}
	cleanReason, err := validateBusinessText("reason", *reason, true)
	if err != nil {
		return err
	}
	cleanNext, err := validateBusinessText("next", *next, true)
	if err != nil {
		return err
	}
	actionID := strings.TrimSpace(*eventID)
	if err := a.preflightActionFrozenSeat(actionID, actor.Name); err != nil {
		return fmt.Errorf("return 前冻结席位核验失败（含角色手册）：%w", err)
	}
	commandID := stableCommandID("return", actionID)
	digest := requestDigest("return", actor.Name, actionID, cleanReason, cleanNext)
	resultTxn, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		originalEvent, ok := ledger.actionableEvent(actionID)
		original, semanticOK := semanticDeliveredEvent(originalEvent, ledger.events)
		if !ok || !semanticOK {
			return Event{}, fmt.Errorf("找不到可退回的已送达事件：%s", actionID)
		}
		if original.Recipient != actor.Name {
			if state, stateErr := ledger.currentCase(original.CaseID); stateErr == nil &&
				state.Owner == actor.Name && a.Config.isManager(actor.Rule) {
				return Event{}, permissionf("事件接收方是 %s，当前经理 %s 无权倒转他人已验收/待核验的旧 submission。你当前持有 case %s：若跨部门修复尚未建立，运行 hq case escalate --id NEW-REWORK-ID --parent %s --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 --source PATH --reason TEXT --next TEXT；若修复已验收、现在要启动独立复验，运行 hq case revise --id %s --version %d --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 --source PATH，随后 hq issue --case %s --to DIRECT-REVERIFY-SEAT --next TEXT", original.Recipient, actor.Name, original.CaseID, original.CaseID, state.ID, state.Version+1, state.ID)
			}
			return Event{}, permissionf("事件接收方是 %s，当前角色 %s 无权退回", original.Recipient, actor.Name)
		}
		contract, required, err := frozenSeatForDeliveredBusinessEvent(ledger, original)
		if err != nil {
			return Event{}, err
		}
		if required {
			if _, err := currentSeatForFrozenContract(a.Config, contract); err != nil {
				return Event{}, fmt.Errorf("return admission 冻结席位核验失败：%w", err)
			}
		}
		if ledger.resolved[originalEvent.ID] {
			if state, stateErr := ledger.currentCase(original.CaseID); stateErr == nil &&
				state.Owner == actor.Name && a.Config.isManager(actor.Rule) {
				return Event{}, conflictf("事件 %s 已处理，HQ 不允许倒转终态。若跨部门修复尚未建立，运行 hq case escalate --id NEW-REWORK-ID --parent %s --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 --source PATH --reason TEXT --next TEXT；若修复已验收、现在要启动独立复验，运行 hq case revise --id %s --version %d --title TEXT --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 --source PATH，随后 hq issue --case %s --to DIRECT-REVERIFY-SEAT --next TEXT", originalEvent.ID, original.CaseID, state.ID, state.Version+1, state.ID)
			}
			return Event{}, fmt.Errorf("事件 %s 已处理", originalEvent.ID)
		}
		state, err := ledger.currentCase(original.CaseID)
		if err != nil {
			return Event{}, err
		}
		returned, err := a.newEvent(actor, "event_returned", original.CaseID)
		if err != nil {
			return Event{}, err
		}
		returned.RelatedEventID, returned.FromState, returned.ToState = originalEvent.ID, state.Status, string(statusReturned)
		returned.Owner, returned.NextAction, returned.Note = original.Actor, cleanNext, cleanReason
		returned.Recipient, returned.RecipientLabel = original.Actor, original.ActorLabel
		returned.SourceRef, returned.ArtifactRef, returned.Verification = original.SourceRef, original.ArtifactRef, original.Verification
		returned.CaseVersion, returned.CaseDigest = original.CaseVersion, original.CaseDigest
		copyAssignmentBinding(&returned, original)
		returned.Delivery, returned.DeliveryID = deliveryPrepared, stableDeliveryID(commandID, original.Actor)
		payload, err := a.deliveryPayload(returned)
		if err != nil {
			return Event{}, err
		}
		returned.PayloadDigest = digestText(payload)
		return returned, nil
	})
	if err != nil {
		return err
	}
	returned := resultTxn.Event
	if a.DryRun {
		return a.output(returned, fmt.Sprintf("DRY-RUN：将退回 event=%s，delivery=%s", actionID, returned.DeliveryID))
	}
	deliveryOutcome, deliveryErr := a.processDelivery(returned, "")
	if deliveryErr != nil {
		if a.JSON {
			_ = a.output(deliveryOutcome, "")
		}
		return deliveryErr
	}
	return a.output(deliveryOutcome, fmt.Sprintf("业务已提交并已通知；returned=%s；case=%s；delivery=%s", returned.ID, returned.CaseID, returned.DeliveryID))
}

func (a *App) cmdClose(args []string) error {
	fs := newLeafParser("close")
	fs.SetOutput(a.Err)
	caseID := fs.String("case", "", "case_id")
	reason := fs.String("reason", "", "关闭依据摘要")
	source := fs.String("source", "", "关闭依据路径[#定位]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	actor, err := a.actor()
	if err != nil {
		return err
	}
	if !a.Config.canCloseAsAccount(actor.Rule) {
		return permissionf("当前角色无权销账：部门经理只能验收直属成员的回报并汇总父 case；只有配置中具备 can_close 的公司所有者或秘书可以关闭 case")
	}
	if err := validateCaseID(*caseID); err != nil {
		return err
	}
	cleanReason, err := validateBusinessText("reason", *reason, true)
	if err != nil {
		return err
	}
	cleanSource, err := normalizeRef(*source, a.HQRoot, true)
	if err != nil {
		return err
	}
	commandID := stableCommandID("close", *caseID)
	digest := requestDigest("close", actor.Name, *caseID, cleanReason, cleanSource)
	resultTxn, err := a.transact(commandID, digest, func(ledger *ledgerState) (Event, error) {
		state, err := ledger.currentCase(*caseID)
		if err != nil {
			return Event{}, err
		}
		if state.Status == "closed" {
			return Event{}, fmt.Errorf("case 已关闭：%s", *caseID)
		}
		if state.Status == string(statusEscalated) {
			eventID := ledger.escalationReviewEventID(state.ID, state.LastEventID)
			return Event{}, conflictf("case %s 正在等待上级核验 escalation；actor=%s 先运行 hq accept --event %s --next TEXT，或 hq return --event %s --reason TEXT --next TEXT，不得直接 close", state.ID, state.Owner, eventID, eventID)
		}
		if blockers := ledger.unsettledClosureDeliveries(*caseID, true); len(blockers) != 0 {
			return Event{}, conflictf("不能关闭 case %s：目标或 descendants 仍有未收敛 workflow delivery：%s；逐项运行：%s；按各 status 的 next_action 收敛原 delivery，禁止靠 close 隐藏 outbox", *caseID,
				renderClosureDeliveryBlockers(blockers), renderClosureDeliveryRecoveryCommands(blockers))
		}
		if active := ledger.activeAssignments(*caseID); len(active) != 0 {
			return Event{}, conflictf("不能关闭 case %s：存在未完成 assignment contract（active assignment）；按角色依次收敛（不是当前销账人可一次执行的脚本）：%s", *caseID,
				ledger.renderPostOrderClosureGuidance(nil, *caseID, actor.Name))
		}
		if descendants := ledger.unclosedDescendantsPostOrder(*caseID); len(descendants) != 0 {
			return Event{}, conflictf("不能关闭 case %s：仍有未 closed descendants（已按 post-order 排序）：%s；按角色依次收敛（不是当前销账人可一次执行的脚本）：%s", *caseID,
				renderDescendantStates(descendants), ledger.renderPostOrderClosureGuidance(descendants, *caseID, actor.Name))
		}
		closed, err := a.newEvent(actor, "case_closed", *caseID)
		if err != nil {
			return Event{}, err
		}
		closed.FromState, closed.ToState, closed.Owner = state.Status, "closed", ""
		closed.SourceRef, closed.NextAction, closed.Note = cleanSource, "无；命中复审条件时重开", cleanReason
		return closed, nil
	})
	if err != nil {
		return err
	}
	closed := resultTxn.Event
	if a.DryRun {
		return a.output(closed, fmt.Sprintf("DRY-RUN：将关闭 %s", *caseID))
	}
	return a.output(closed, fmt.Sprintf("已关闭 %s；event=%s", *caseID, closed.ID))
}

func (a *App) cmdBoard(args []string) error {
	fs := newLeafParser("board")
	fs.SetOutput(a.Err)
	all := fs.Bool("all", false, "包含已关闭 case")
	casesOnly := fs.Bool("cases-only", false, "只显示 HQ 结构化事项")
	reindex := fs.Bool("reindex", false, "从权威事件重新生成 SQLite 看板索引及 HQ 派生状态")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *reindex {
		if _, err := a.Store.Rebuild(a.Config); err != nil {
			return err
		}
	}
	var snapshot Snapshot
	var err error
	if *casesOnly && !*reindex {
		if store, ok := a.Store.(interface {
			SnapshotReadOnly(Config) (Snapshot, error)
		}); ok {
			snapshot, err = store.SnapshotReadOnly(a.Config)
		} else {
			snapshot, err = a.Store.Snapshot(a.Config)
		}
	} else {
		snapshot, err = a.Store.Snapshot(a.Config)
	}
	if err != nil {
		return err
	}
	states := make([]*CaseState, 0, len(snapshot.Cases))
	for _, state := range snapshot.Cases {
		if !*all && state.Status == "closed" {
			continue
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool {
		if states[i].Status == states[j].Status {
			return states[i].ID < states[j].ID
		}
		return states[i].Status < states[j].Status
	})
	if a.JSON {
		return a.output(states, "")
	}
	fmt.Fprintf(a.Out, "HQ 事项：%d cases / %d events（生成 %s）\n", len(states), snapshot.EventCount, snapshot.GeneratedAt)
	fmt.Fprintf(a.Out, "%-20s %-18s %-16s %-4s %s\n", "CASE", "STATUS", "OWNER", "PRI", "NEXT")
	for _, state := range states {
		fmt.Fprintf(a.Out, "%-20s %-18s %-16s %-4s %s\n", state.ID, state.Status, state.Owner, state.Priority, state.NextAction)
	}
	return nil
}

func (a *App) cmdHistory(args []string) error {
	fs := newLeafParser("history")
	fs.SetOutput(a.Err)
	caseID := fs.String("case", "", "case_id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := validateCaseID(*caseID); err != nil {
		return err
	}
	events, err := a.Store.ReadAll(a.Config)
	if err != nil {
		return err
	}
	var selected []Event
	for _, event := range events {
		if event.CaseID == *caseID {
			selected = append(selected, event)
		}
	}
	if len(selected) == 0 {
		return fmt.Errorf("case 无事件：%s", *caseID)
	}
	if a.JSON {
		return a.output(selected, "")
	}
	for _, event := range selected {
		fmt.Fprintf(a.Out, "%s  %-24s %-16s %s -> %s  %s\n", event.At, event.Type, event.Actor, event.FromState, event.ToState, event.ID)
	}
	return nil
}

func (a *App) cmdRebuild(args []string) error {
	if len(args) != 0 {
		return fmt.Errorf("用法：hq rebuild")
	}
	snapshot, err := a.Store.Rebuild(a.Config)
	if err != nil {
		return err
	}
	return a.output(snapshot, fmt.Sprintf("已重建：%d cases / %d events", len(snapshot.Cases), snapshot.EventCount))
}

func formatReportMessage(actor Actor, event Event, eventRef string) string {
	parts := []string{fmt.Sprintf("[HQ notification][%s] CASE=%s EVENT=%s DELIVERY=%s：%s回报", actor.Label, event.CaseID, event.ID, event.DeliveryID, event.Result)}
	if event.ArtifactRef != "" {
		parts = append(parts, "产出="+event.ArtifactRef)
	}
	if event.SourceRef != "" {
		parts = append(parts, "依据="+event.SourceRef)
	}
	if event.Severity != "" {
		parts = append(parts, "严重度="+event.Severity)
	}
	if event.Location != "" {
		parts = append(parts, "定位="+event.Location)
	}
	if event.Verification != "" {
		parts = append(parts, "复验条件="+event.Verification)
	}
	if event.Note != "" {
		parts = append(parts, "说明="+event.Note)
	}
	parts = append(parts, "下一步="+event.NextAction, "账本="+eventRef)
	return strings.Join(parts, "；")
}

func truncateError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.ReplaceAll(err.Error(), "\n", " ")
	if len([]rune(text)) > 180 {
		text = string([]rune(text)[:180])
	}
	return text
}

func resolveActionableEvent(events []Event, id string) (Event, bool) {
	event, ok := findEvent(events, id)
	if !ok {
		return Event{}, false
	}
	if event.Type == "case_escalation_sent" || event.Type == "report_sent" || event.Type == "issue_sent" || event.Type == "message_sent" {
		return event, true
	}
	if event.Type == "case_escalation_prepared" || event.Type == "report_prepared" || event.Type == "issue_prepared" || event.Type == "message_prepared" {
		for i := len(events) - 1; i >= 0; i-- {
			candidate := events[i]
			if candidate.RelatedEventID == event.ID && (candidate.Type == "case_escalation_sent" || candidate.Type == "report_sent" || candidate.Type == "issue_sent" || candidate.Type == "message_sent" || candidate.Type == "delivery_resolved_sent") {
				return candidate, true
			}
		}
	}
	return Event{}, false
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, `hq — 总部通信、回铃与事件看板

全局用法：
  hq [--office PATH] [--data PATH] [--config PATH] [--json] [--dry-run] COMMAND

主要命令：
  hq whoami
	  hq case create --id ID --title TEXT --objective TEXT --acceptance TEXT \
	    --constraints TEXT --priority P1 --source PATH [--parent PARENT]
	  hq case escalate --id NEW-ID --parent OWNED-PARENT --title TEXT \
	    --objective TEXT --acceptance TEXT --constraints TEXT --priority P1 \
	    --source PATH --reason TEXT --next TEXT
  hq case revise --id ID --version N --title TEXT --objective TEXT \
    --acceptance TEXT --constraints TEXT --priority P1 --source PATH
  hq case show --id ID
  hq project list [--status active|review|blocked|closed] [--priority P0|P1|P2|unset]
  hq project show --project PROJECT
  hq approval request --id APR --case ID --target AGENT --expires RFC3339
  hq approval grant --id APR --issuer <config.owner_principal>
  hq approval show APR
  hq report --case ID --result completed --artifact PATH --next TEXT
  hq issue --case ID --to DIRECT_REPORT --next TEXT
  hq issue --case ID --to AGENT --approval APR --next TEXT
  hq assignment list [--case ID] [--assignee AGENT] [--status STATUS]
  hq assignment show --id ASSIGNMENT
  hq message --to AGENT --kind info --case ID --text TEXT \
    [--ref-file PATH] [--ref-case ID] [--ref-message MSG] [--ref-event EVENT]
    [--delivery auto|wakeup|quiet|inject]
  hq message ack --message MESSAGE_ID
  hq inbox
  hq accept --event EVENT_ID --next TEXT
  hq return --event EVENT_ID --reason TEXT --next TEXT
  hq close --case ID --reason TEXT --source PATH
  hq delivery status [--id DELIVERY]
  hq delivery budget status [--target AGENT]
  hq delivery consume [--limit 100]
  hq nudge enqueue --id ID --dedupe KEY --to MANAGER --message TEXT [--ttl 15m]
  hq nudge claim --id ID --claim CLAIM [--lease 30s]
  hq nudge deliver --id ID --claim CLAIM
  hq nudge status --id ID
  hq nudge reconcile --id ID --resolution delivered|not-run --ref PATH --note TEXT
  hq reminder scan [--after 24h] [--ttl 15m]
  hq --direct estop activate --id ID --reason TEXT
  hq --direct estop status
  hq --direct estop release --id ID --reason TEXT
  hq --direct runtime status [--agent AGENT]
  hq --direct runtime reap [--agent AGENT] [--retry-failed|--retry-unknown]
  hq --direct runtime fallback --agent AGENT [--retry-unknown]
  hq --direct runtime repair-profile --agent AGENT [--retry-unknown]
  hq board [--all]
  hq doctor [--json]
  hq history --case ID
  hq rebuild

写操作必须从登记过的 herdr agent 工位执行。
nudge/reminder 不自动关闭、拍板或改变 owner/status；ESTOP 每次重验 can_manage_staff。
首次启用请先完成 hq doctor，并在独立 workspace 验证 registry、岗位手册和恢复流程。`)
}
