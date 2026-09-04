package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

func agentFacingCommandError(cmd *cobra.Command, err error) error {
	if err == nil || cmd == nil {
		return err
	}
	var doctorFailed DoctorFailedError
	if errors.As(err, &doctorFailed) {
		return err
	}
	var guided *agentCommandError
	if errors.As(err, &guided) {
		return err
	}
	commandPath := cmd.CommandPath()
	if strings.TrimSpace(commandPath) == "" {
		commandPath = "hq"
	}
	return &agentCommandError{
		Command: commandPath,
		Usage:   cmd.UseLine(),
		Help:    commandPath + " --help",
		Cause:   err,
	}
}

func newCobraRootCommand(options globalOptions, out, errOut io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use: "hq", Short: "Herdr 虚拟公司的总部控制面",
		SilenceErrors: true, SilenceUsage: true,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return agentFacingCommandError(cmd, usagef("缺少子命令"))
			}
			return agentFacingCommandError(cmd, usagef("未知命令 %q", args[0]))
		},
		// Mark the root runnable so Cobra invokes Args instead of its default
		// help-success path when no registered subcommand was selected.
		RunE: func(*cobra.Command, []string) error { return nil },
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return agentFacingCommandError(cmd, usagef("%v", err))
	})

	flags := root.PersistentFlags()
	flags.StringVar(&options.Office, "office", options.Office, "公司实例的 ceo-office 目录（默认自动发现）")
	flags.StringVar(&options.Data, "data", options.Data, "事件数据目录（正式实例固定为 ceo-office/records）")
	flags.StringVar(&options.Config, "config", options.Config, "HQ YAML 注册表（正式实例固定为 ceo-office/tools/hq/config.yaml）")
	flags.StringVar(&options.Herdr, "herdr", options.Herdr, "herdr 二进制路径（默认 PATH 中的 herdr）")
	flags.BoolVar(&options.DryRun, "dry-run", options.DryRun, "只校验并展示，不写账本、不投递")
	flags.BoolVar(&options.JSON, "json", options.JSON, "输出 JSON")
	flags.BoolVar(&options.Direct, "direct", options.Direct, "仅对已授权运维角色开放的本地维护模式")
	flags.StringVar(&options.MaintenancePane, "maintenance-pane", options.MaintenancePane, "hq up 启动 gateway 时传递的调用者 pane 索引")
	_ = flags.MarkHidden("maintenance-pane")

	run := func(path ...string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, positional []string) error {
			if err := validateRequiredFlags(cmd, requiredFlags(path...)); err != nil {
				return agentFacingCommandError(cmd, err)
			}
			if err := validateConditionalFlags(cmd, path...); err != nil {
				return agentFacingCommandError(cmd, err)
			}
			invocation := append(append([]string(nil), path...), cobraLeafArgs(cmd, positional)...)
			var app *App
			var err error
			switch strings.Join(path, " ") {
			case "init":
				app, err = newInitApp(options, out, errOut)
			case "doctor":
				app, err = newDoctorApp(options, out, errOut)
			case "version":
				app = &App{JSON: options.JSON, Out: out, Err: errOut}
			default:
				if isDependencyFreeReadOnlyCommand(strings.Join(path, " "), cmd) {
					app, err = newDependencyFreeReadOnlyApp(options, out, errOut)
				} else {
					app, err = newApp(options, out, errOut)
				}
			}
			if err != nil {
				if strings.Join(path, " ") == "init" {
					err = usagef("%v", err)
				}
				return agentFacingCommandError(cmd, err)
			}
			return agentFacingCommandError(cmd, app.run(invocation))
		}
	}
	leaf := func(use, short string, path ...string) *cobra.Command {
		cmd := &cobra.Command{Use: use, Short: short, Args: leafArgsValidator(path...), RunE: run(path...)}
		addLeafFlags(cmd, path...)
		return cmd
	}
	group := func(use, short string) *cobra.Command {
		return &cobra.Command{Use: use, Short: short, Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return agentFacingCommandError(cmd, usagef("缺少子命令"))
			}
			return agentFacingCommandError(cmd, usagef("未知子命令 %q", args[0]))
		},
			// As with the root, RunE makes the group runnable so its Args
			// validator owns missing and unknown subcommand classification.
			RunE: func(*cobra.Command, []string) error { return nil },
		}
	}

	upCommand := leaf("up [agent]", "按 HQ 配置启动公司并确保网关在线", "up")
	upCommand.Long = `按 HQ registry 幂等启动已经完成 init 的公司，并确保本地写入网关在线。

正式实例不接受全局 --config、--data、--herdr 覆盖（exit 70）；--office 只选择一个经过
canonical 校验的公司实例。合成集成验证使用 README“构建与安全首验”中的 fake Herdr 测试，
不通过运行参数替换正式依赖。

首次连接 Herdr 由 hq init 自动完成，不能用 up 代替。公司关停后的宿主机冷启动可直接运行 hq up，此路径只恢复
activation_policy=always 的岗位和 gateway，并要求不可覆盖的 init 完成记录；不能指定单个岗位，
也不能使用 --no-gateway。公司运行期间从 Herdr 工位执行 up 时，调用者仍必须是 registry 中
can_manage_staff=true 的精确在岗角色。runtime sender_label 与运行连续性检查见 README“首次连接 Herdr”。`
	root.AddCommand(
		upCommand,
		leaf("patrol", "只读巡视 blocked、编制漂移、orphan 与死亡候选", "patrol"),
		leaf("whoami", "核验当前 herdr 工位身份", "whoami"),
		leaf("report", "按配置的汇报线上行回铃", "report"),
		leaf("issue", "由授权通道下发已批准事项", "issue"),
		leaf("inbox", "查看待核验事件", "inbox"),
		leaf("accept", "核验接收事件", "accept"),
		leaf("return", "退回事件并写明复交条件", "return"),
		leaf("close", "关闭公司事项", "close"),
		leaf("board", "显示运营看板与结构化事项", "board"),
		leaf("history", "显示一个事项的事件历史", "history"),
		leaf("rebuild", "从追加事件重建派生状态", "rebuild"),
		leaf("reconcile", "保守核对 durable outbox", "reconcile"),
		leaf("serve", "运行本地写入网关", "serve"),
		leaf("ping", "检查本地写入网关", "ping"),
		leaf("version", "显示构建版本、提交与 Go/平台信息", "version"),
	)
	message := leaf("message", "平级或协作沟通；不改变业务状态或授权", "message")
	message.AddCommand(leaf("ack", "确认已收到 message", "message", "ack"))
	root.AddCommand(message)

	nudge := group("nudge", "排队、认领、投递与人工核对回合边界提醒")
	nudge.AddCommand(leaf("enqueue", "排队一条回合边界提醒", "nudge", "enqueue"), leaf("claim", "原子认领待投递提醒", "nudge", "claim"), leaf("deliver", "投递已认领提醒", "nudge", "deliver"), leaf("reconcile", "人工核对不确定投递", "nudge", "reconcile"), leaf("status", "查询提醒状态", "nudge", "status"))
	root.AddCommand(nudge)
	reminder := group("reminder", "机械再催超期开口项")
	reminder.AddCommand(leaf("scan", "扫描并至多提醒一次；不自动关闭或拍板", "reminder", "scan"))
	root.AddCommand(reminder)
	estop := group("estop", "冻结非豁免子角色并精确恢复本次冻结集")
	estop.AddCommand(leaf("activate", "激活急停并冻结非豁免子角色", "estop", "activate"), leaf("release", "显式解除急停并恢复确认冻结角色", "estop", "release"), leaf("status", "只读查询急停状态", "estop", "status"))
	root.AddCommand(estop)
	session := group("session", "查询严格会话生命周期 JSONL")
	session.AddCommand(leaf("list", "按 session/agent/type 查询启停事件", "session", "list"))
	root.AddCommand(session)
	runtime := group("runtime", "查询、回收或恢复员工运行实例")
	runtime.AddCommand(
		leaf("status", "显示 seat 休眠资格与可执行纠正", "runtime", "status"),
		leaf("reap", "安全休眠已完成且无待办的运行实例", "runtime", "reap"),
		leaf("fallback", "核验 provider safeguard 并以配置的备用模型继续同一 durable work", "runtime", "fallback"),
		leaf("repair-profile", "在安全边界恢复 registry 声明的 model/effort", "runtime", "repair-profile"),
	)
	root.AddCommand(runtime)
	flow := group("flow", "查询事项流转信封与投递回执")
	flow.AddCommand(leaf("show", "机器可核验地显示事项事件链与投递回执", "flow", "show"))
	root.AddCommand(flow)
	index := group("index", "重建或结构化查询 HQ 派生索引")
	index.AddCommand(leaf("rebuild", "从严格事件账本与 Markdown 元数据重建派生索引", "index", "rebuild"), leaf("query", "按受控字段查询派生索引", "index", "query"))
	root.AddCommand(index)
	root.AddCommand(leaf("doctor", "只读检查 HQ 冷启动依赖", "doctor"))

	initCommand := &cobra.Command{
		Use:   "init <company-directory>",
		Short: "创建公司，或继续已准备公司的首次初始化",
		Long: `从模板或已批准的自定义组织规范生成完整、可校验的 HQ 公司实例，并默认通过 Herdr 首次启动。

对已有 config.yaml 的 prepare-only 公司，可只传 company-directory 再次运行 init。HQ 会核验已生成
的 registry、角色卡与 company-init 决策，然后按原 init 契约续跑；成功后首次初始化通道永久关闭。

交互模式会依次询问公司名、所有者、workspace、组织模板、总裁秘书 slug/显示名、
Agent 类型和权限模式。
它只解析这些结构化字段，不连接或调用任何 LLM API。模板或 organization spec 生成配置、
岗位手册、首份公司成立决策和公司本地 hq 二进制；启动后由总部联系职责位通过 staff 命令调整编制。


自动化创建使用 --silent：公司名、所有者、workspace，以及 --template/--organization-spec 二选一必须显式给出，缺项会在写文件
前失败。--prepare-only 只生成和校验文件，不连接 Herdr，也不启动 Agent；随后用同一个 init 命令完成首次启动。`,
		Example: `  # 交互式创建并启动
  hq init ./acme

  # 非交互式创建并启动
  hq init ./acme --silent --company-name "Acme" --owner ZC \
    --workspace acme-hq --template product-engineering \
    --secretary-name liaison --secretary-nickname "总部联络官" \
    --secretary-kind claude --default-agent-kind codex --permission-mode native

  # 用已批准的第一性原理组织规范直接创建专属公司
  hq init ./domain-company --silent --company-name "Domain Company" --owner OWNER \
    --workspace domain-company-hq --organization-spec ./approved-organization.yaml \
    --default-agent-kind codex --permission-mode native --prepare-only


  # 离线生成并校验，稍后在宿主机继续 init
  hq init ./acme --silent --company-name "Acme" --owner ZC \
	--workspace acme-hq --template minimal --prepare-only
  ./acme/ceo-office/tools/hq/bin/hq init ./acme`,
		Args: leafArgsValidator("init"), RunE: run("init"),
	}
	initFlags := initCommand.Flags()
	initFlags.String("company-name", "", "公司显示名称（--silent 必填）")
	initFlags.String("owner", "", "公司所有者 principal（--silent 必填）")
	initFlags.String("workspace", "", "Herdr workspace 小写 slug（--silent 必填）")
	initFlags.String("template", "", "内置模板；与 --organization-spec 互斥")
	initFlags.String("organization-spec", "", "已批准的自定义组织规范 YAML；与 --template 互斥")
	initFlags.String("secretary-name", "secretary", "总裁秘书稳定 slug 的基础名称；角色权限不依赖该名字")
	initFlags.String("secretary-nickname", "总裁秘书", "总裁秘书显示名称")
	initFlags.String("secretary-kind", "codex", "总裁秘书使用的 Agent kind")
	initFlags.String("default-agent-kind", "codex", "其他成员默认使用的 Agent kind")
	initFlags.StringArray("secretary-agent-arg", nil, "传给总裁秘书 Agent 的原生 argv；可重复")
	initFlags.StringArray("default-agent-arg", nil, "传给其他成员 Agent 的原生 argv；可重复")
	initFlags.String("permission-mode", "native", "native 不追加自动批准参数；yolo 必须显式选择")
	initFlags.Bool("silent", false, "非交互初始化；绝不读取 stdin")
	initFlags.Bool("prepare-only", false, "只生成并校验，不连接 Herdr 或启动公司")
	root.AddCommand(initCommand)

	caseCommand := group("case", "管理可分解、可委派的结构化事项")
	caseCommand.AddCommand(
		leaf("create", "创建唯一 project root 或其子 case", "case", "create"),
		leaf("escalate", "经理创建返工子 case 并固定上交直属上级；支持已接单的直属上级 assignment", "case", "escalate"),
		leaf("revise", "追加 case 规格新版本", "case", "revise"),
		leaf("show", "查看事项", "case", "show"),
	)
	root.AddCommand(caseCommand)
	project := group("project", "查看当前 HQ space 的唯一只读项目投影")
	project.AddCommand(
		leaf("list", "列出项目状态、责任分布与完成缺口", "project", "list"),
		leaf("show", "显示一个项目的汇总与 case 明细", "project", "show"),
	)
	root.AddCommand(project)
	assignment := group("assignment", "查询冻结 assignee/reviewer/acceptor 的委派合同")
	assignment.AddCommand(
		leaf("list", "列出 assignment contract", "assignment", "list"),
		leaf("show", "显示一个 assignment contract", "assignment", "show"),
	)
	root.AddCommand(assignment)
	approval := group("approval", "记录范围精确的许可生命周期")
	approval.AddCommand(leaf("request", "记录 approval request", "approval", "request"), leaf("grant", "记录公司所有者授权与见证人", "approval", "grant"), leaf("revoke", "撤销已 grant approval", "approval", "revoke"), leaf("expire", "到期终结 approval", "approval", "expire"), leaf("show <approval_id>", "只读显示必要批准事实", "approval", "show"))
	root.AddCommand(approval)
	delivery := group("delivery", "查询、重试或人工解除 durable outbox")
	deliveryBudget := group("budget", "查询目标连续唤醒预算")
	deliveryBudget.AddCommand(leaf("status", "查看目标连续唤醒预算", "delivery", "budget", "status"))
	delivery.AddCommand(
		leaf("status", "查看投递状态", "delivery", "status"),
		leaf("retry", "仅重试确证未投递的 delivery", "delivery", "retry"),
		leaf("resolve", "运维白名单人工解除 unknown", "delivery", "resolve"),
		deliveryBudget,
		leaf("consume", "人工恢复：读取尚未被自动并入的静默业务正文", "delivery", "consume"),
	)
	root.AddCommand(delivery)
	staff := group("staff", "查询和维护 HQ 人员注册表")
	staff.AddCommand(leaf("list", "列出员工", "staff", "list"), leaf("get", "查看员工", "staff", "get"), leaf("add", "新增员工（需生效决策）", "staff", "add"), leaf("update", "修改员工（需生效决策）", "staff", "update"), leaf("remove", "停用员工（需生效决策）", "staff", "remove"))
	root.AddCommand(staff)
	role := group("role", "查询和维护不可变角色卡")
	role.AddCommand(leaf("list", "列出 role card", "role", "list"), leaf("show", "查看 role card", "role", "show"), leaf("add", "从独立 AGENTS.md 新增 role card", "role", "add"), leaf("retire", "退役未绑定的 role card", "role", "retire"))
	root.AddCommand(role)
	return root
}

func isDependencyFreeReadOnlyCommand(path string, cmd *cobra.Command) bool {
	switch path {
	case "staff list", "staff get", "role list", "role show", "project list", "project show", "assignment list", "assignment show":
		return true
	case "board":
		casesOnly, casesErr := cmd.Flags().GetBool("cases-only")
		reindex, reindexErr := cmd.Flags().GetBool("reindex")
		return casesErr == nil && reindexErr == nil && casesOnly && !reindex
	default:
		return false
	}
}

func addLeafFlags(cmd *cobra.Command, path ...string) {
	f := cmd.Flags()
	addString := func(name, usage string) { f.String(name, "", usage) }
	addBool := func(name, usage string) { f.Bool(name, false, usage) }
	addDuration := func(name string, value time.Duration, usage string) { f.Duration(name, value, usage) }
	switch strings.Join(path, " ") {
	case "up":
		addBool("no-gateway", "只启动 agent，不启动 HQ 写入网关")
	case "patrol":
		addDuration("grace", 2*time.Second, "第二快照前的宽限期")
	case "report":
		addString("case", "case_id（必填）")
		addString("result", "completed|blocked|needs-decision|finding|returned（必填；不同结果的证据参数见各 flag）")
		addString("source", "依据原文路径[#定位]；blocked|needs-decision|returned|finding 必填，completed 可与 --artifact 二选一")
		addString("artifact", "产出路径[#定位] 或 git:/repo@sha；completed 至少与 --source 二选一")
		addString("severity", "P0|P1|P2；finding 必填")
		addString("location", "精确定位；finding 必填")
		addString("verify", "复验条件（合法 UTF-8 单行，不超过 2 KiB）；finding 必填")
		addString("next", "下一步（必填）")
		addString("note", "新增事实（合法 UTF-8 单行，不超过 2 KiB）；blocked|needs-decision|returned 必填")
	case "issue":
		addString("case", "case_id（必填）")
		addString("to", "接收方 agent 名（必填）")
		addString("approval", "approval_id；与 --decision 二选一")
		addString("decision", "standing authorization decision；与 --approval 二选一")
		addString("next", "下一步（必填）")
		addString("note", "补充说明（不得改变原文）")
		addString("due", "可选 RFC3339 截止时间；必须晚于当前时间")
		addString("delivery", "固定为 wakeup；case 委派不可静默")
	case "message":
		addString("to", "接收方 agent（必填）")
		addString("kind", "info|question|request|handoff（必填）")
		addString("case", "可选 case_id")
		addString("text", "消息正文（UTF-8 硬上限 2 KiB；必填）")
		f.StringSlice("ref-file", nil, "引用文件；可重复")
		f.StringSlice("ref-case", nil, "引用 case_id；可重复")
		f.StringSlice("ref-message", nil, "引用 message_id；可重复")
		f.StringSlice("ref-event", nil, "引用 event_id；可重复")
		addString("thread", "可选稳定 thread id")
		addString("reply-to", "可选被回复 message_id")
		addString("delivery", "auto|wakeup|quiet|inject")
	case "message ack":
		addString("message", "已送达 message_id（必填）")
	case "approval request":
		addString("id", "approval_id（必填）")
		addString("case", "case_id（必填）")
		addString("action", "目前仅 issue")
		addString("target", "target agent（必填）")
		addString("expires", "RFC3339 有效期（必填）")
		addString("mode", "仅 one_time；跨 generation 复用不受支持（默认 one_time）")
	case "approval grant":
		addString("id", "approval_id（必填）")
		addString("issuer", "公司所有者标识；必须匹配 config.yaml owner_principal（必填）")
	case "approval revoke":
		addString("id", "approval_id（必填）")
		addString("reason", "撤销原因（必填）")
	case "approval expire":
		addString("id", "approval_id（必填）")
	case "inbox":
		addString("for", "查看指定 agent 收件箱（需跨角色核验权限）")
	case "accept":
		addString("event", "待核验 event_id（必填）")
		addString("next", "下一步（必填）")
		addString("note", "核验说明")
	case "return":
		addString("event", "待退回 event_id（必填）")
		addString("reason", "退回原因（必填）")
		addString("next", "复交条件/下一步（必填）")
	case "close":
		addString("case", "case_id（必填）")
		addString("reason", "关闭依据摘要（必填）")
		addString("source", "关闭依据路径[#定位]（必填）")
	case "board":
		addBool("all", "包含已关闭 case")
		addBool("cases-only", "只显示 HQ 结构化事项")
		addBool("reindex", "从权威事件重新生成 SQLite 看板索引及 HQ 派生状态")
	case "history", "flow show":
		addString("case", "case_id（必填）")
	case "serve":
		addString("workspace-id", "绑定的 herdr workspace id")
		addString("server-id", "启动实例 identity")
	case "ping":
		addString("workspace", "预期 herdr workspace id")
	case "case create":
		addString("id", "稳定 case_id（必填）")
		addString("title", "标题（必填）")
		addString("project", "唯一 root 必填；child 禁止且自动继承")
		addString("parent", "父 case_id；省略时表示创建唯一 root")
		addString("objective", "目标（必填）")
		addString("acceptance", "验收条件（必填）")
		addString("constraints", "约束（必填）")
		addString("priority", "P0|P1|P2（必填）")
		addString("spec-ref", "复杂规格 Markdown 引用")
		addString("source", "权威来源路径[#定位]（必填）")
		addString("owner", "初始负责人 agent 名（默认当前角色）")
		addString("version", "创建版本；默认 1")
	case "case revise":
		addString("id", "稳定 case_id（必填）")
		addString("title", "标题（必填）")
		addString("project", "禁止；Project identity 在唯一 root 创建时冻结")
		addString("parent", "禁止；revise 不得改变 lineage")
		addString("objective", "目标（必填）")
		addString("acceptance", "验收条件（必填）")
		addString("constraints", "约束（必填）")
		addString("priority", "P0|P1|P2（必填）")
		addString("spec-ref", "复杂规格 Markdown 引用")
		addString("source", "权威来源路径[#定位]（必填）")
		addString("owner", "禁止；revise 不得改变 owner")
		addString("version", "新版本号（必填）")
	case "case escalate":
		addString("id", "新 escalation 子 case 的稳定 case_id（必填）")
		addString("parent", "当前经理持有的父 case_id（必填）")
		addString("title", "标题（必填）")
		addString("project", "禁止；escalation child 自动继承唯一 project")
		addString("objective", "返工目标（必填）")
		addString("acceptance", "返工验收条件（必填）")
		addString("constraints", "返工约束（必填）")
		addString("priority", "P0|P1|P2（必填）")
		addString("spec-ref", "复杂规格 Markdown 引用")
		addString("source", "升级依据路径[#定位]（必填）")
		addString("reason", "必须升级的新增事实（必填）")
		addString("next", "直属上级接手后的下一步（必填）")
	case "case show":
		addString("id", "case_id（必填）")
	case "project list":
		addString("status", "项目汇总状态：active|review|blocked|closed")
		addString("priority", "未关闭 case 的最高优先级：P0|P1|P2|unset")
		addString("owner", "仅保留包含该 owner 的项目")
		addString("department", "仅保留包含该部门的项目")
	case "project show":
		addString("project", "case.project 的精确值（必填）")
	case "assignment list":
		addString("case", "按 case_id 过滤")
		addString("assignee", "按 assignee agent 过滤")
		addString("status", "issued|accepted|submitted|rework|completed|reported|returned")
	case "assignment show":
		addString("id", "assignment_id（必填）")
	case "delivery status":
		addString("id", "delivery_id；留空列出全部")
	case "delivery retry":
		addString("id", "delivery_id（必填）")
		addString("command", "稳定 retry command_id（可选）")
	case "delivery resolve":
		addString("id", "delivery_id（必填）")
		addString("outcome", "delivered|not-delivered（必填）")
		addString("reason", "人工核对理由（必填）")
		addString("evidence", "核对依据路径[#定位]（必填）")
	case "delivery budget status":
		addString("target", "目标 agent；默认当前角色")
	case "delivery consume":
		f.String("limit", "100", "本次人工恢复最多读取条数（1..100）")
	case "session list":
		addString("session", "按 session id 过滤")
		addString("agent", "按 agent 过滤")
		addString("type", "按 started|stopped|hibernate_*|fallback_*|profile_* 过滤")
	case "runtime status":
		addString("agent", "精确 seat slug；默认全部 on_assignment seat")
	case "runtime reap":
		addString("agent", "精确 seat slug；默认全部 on_assignment seat")
		addBool("retry-failed", "显式重试 definitely-not-run 的关闭")
		addBool("retry-unknown", "人工核验同一 incarnation 仍在后重试 unknown")
	case "runtime fallback":
		addString("agent", "必填；要恢复的单一精确 seat slug")
		addBool("retry-unknown", "人工核验同一 Codex incarnation 仍在后重试 fallback_unknown")
	case "runtime repair-profile":
		addString("agent", "必填；要核验并恢复的单一精确 seat slug")
		addBool("retry-unknown", "人工核验同一 incarnation 仍在后重试 profile_repair_unknown")
	case "index query":
		f.String("entity", "flow_events", "flow_events|cases|deliveries|documents")
		addString("case", "case_id")
		addString("type", "event type")
		addString("actor", "actor")
		addString("recipient", "recipient")
		addString("status", "status")
		addString("from", "RFC3339 lower bound")
		addString("to", "RFC3339 upper bound")
		addString("path", "exact canonical source/artifact/document path")
	case "staff list":
		addBool("all", "包含已停用员工")
		addString("reports-to", "仅列出直属上级为该 agent slug 的员工")
	case "staff get":
		addString("name", "稳定 agent slug（必填）")
	case "staff add":
		addString("name", "稳定 agent slug（必填）")
		addString("label", "句头发件标识（必填；不含方括号）")
		addString("department", "工位目录（必填）")
		addString("kind", "herdr 启动 kind")
		f.String("permission-mode", "native", "native|yolo；默认 native")
		addString("reports-to", "直属上级 slug")
		addString("role", "不可变 role card id@version（必填）")
		addString("workstation", "独立员工工位目录（必填）")
		f.String("activation", activationOnAssignment, "always|on_assignment|manual")
		addString("keep-warm", "on_assignment 完成后的有界保温时长（0s..1h，默认 30s）")
		f.String("max-wip", "1", "最大并行在办 case（1..16）")
		addString("grant", "逗号分隔权限")
		addString("approval", "生效 decisions 文件（必填）")
	case "staff update":
		addString("name", "稳定 agent slug（必填；不可原地改名）")
		addString("label", "新发件标识")
		addString("department", "新工位目录")
		addString("kind", "新启动 kind")
		addString("permission-mode", "native|yolo")
		addString("reports-to", "新直属上级；- 表示清空")
		addString("role", "新 role card id@version")
		addString("workstation", "新独立员工工位目录")
		addString("activation", "always|on_assignment|manual")
		addString("keep-warm", "on_assignment 有界保温时长；- 清空为默认")
		addString("max-wip", "最大并行在办 case（1..16）")
		addString("grant", "新增权限，逗号分隔")
		addString("revoke", "撤销权限，逗号分隔")
		addBool("enable", "重新启用")
		addBool("disable", "停用")
		addString("approval", "生效 decisions 文件（必填）")
	case "staff remove":
		addString("name", "稳定 agent slug（必填）")
		addString("approval", "生效 decisions 文件（必填）")
	case "role list":
		addBool("all", "包含 retired role card")
	case "role show":
		addString("role", "role card id@version（必填）")
	case "role add":
		addString("id", "稳定 role card id（必填）")
		addString("version", "不可变 role card version（必填）")
		addString("label", "角色显示名称（必填）")
		addString("department", "所属部门（必填）")
		f.StringSlice("capability", nil, "行为能力标签；可重复（至少一项）")
		addString("manual", "独立 AGENTS.md 的 workspace 相对路径（必填）")
		addString("approval", "公司所有者生效 decisions 文件（必填）")
	case "role retire":
		addString("role", "role card id@version（必填）")
		addString("approval", "公司所有者生效 decisions 文件（必填）")
	case "nudge enqueue":
		addString("id", "稳定 nudge id（必填）")
		addString("dedupe", "未终结期间唯一 dedupe key（必填）")
		addString("to", "精确登记常驻经理（必填）")
		addString("message", "单行提醒（合法 UTF-8，必填；≤2 KiB）")
		addDuration("ttl", 15*time.Minute, "TTL（30s..24h）")
	case "nudge claim":
		addString("id", "nudge id（必填）")
		addString("claim", "稳定 claim id（必填）")
		addDuration("lease", 30*time.Second, "claim lease（5s..5m）")
	case "nudge deliver":
		addString("id", "nudge id（必填）")
		addString("claim", "当前 claim id（必填）")
	case "nudge status":
		addString("id", "nudge id（必填）")
	case "nudge reconcile":
		addString("id", "nudge id（必填）")
		addString("resolution", "delivered|not-run（必填）")
		addString("ref", "人工核对证据原文（必填）")
		addString("note", "短核对说明（必填）")
	case "reminder scan":
		addDuration("after", 24*time.Hour, "开口项超期阈值")
		addDuration("ttl", 15*time.Minute, "提醒 nudge TTL")
	case "estop activate":
		addString("id", "稳定 estop id（必填）")
		addString("reason", "单行短原因（必填）")
	case "estop release":
		addString("id", "稳定 estop id（必填）")
		addString("reason", "显式解除原因（必填）")
	}
}

func leafArgsValidator(path ...string) cobra.PositionalArgs {
	joined := strings.Join(path, " ")
	if joined == "approval show" {
		return func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return agentFacingCommandError(cmd, usagef("必须提供一个稳定 id"))
			}
			return nil
		}
	}
	if joined == "up" {
		return func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return agentFacingCommandError(cmd, usagef("位置参数过多"))
			}
			return nil
		}
	}
	if joined == "init" {
		return func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return agentFacingCommandError(cmd, usagef("必须提供一个 company-directory"))
			}
			return nil
		}
	}
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != 0 {
			return agentFacingCommandError(cmd, usagef("不接受位置参数 %q", args[0]))
		}
		return nil
	}
}

func requiredFlags(path ...string) []string {
	switch strings.Join(path, " ") {
	case "case create":
		return []string{"id", "title", "objective", "acceptance", "constraints", "priority", "source"}
	case "case escalate":
		return []string{"id", "parent", "title", "objective", "acceptance", "constraints", "priority", "source", "reason", "next"}
	case "case revise":
		return []string{"id", "title", "objective", "acceptance", "constraints", "priority", "source", "version"}
	case "case show":
		return []string{"id"}
	case "project show":
		return []string{"project"}
	case "assignment show":
		return []string{"id"}
	case "report":
		return []string{"case", "result", "next"}
	case "issue":
		return []string{"case", "to", "next"}
	case "message":
		return []string{"to", "kind", "text"}
	case "message ack":
		return []string{"message"}
	case "approval request":
		return []string{"id", "case", "target", "expires"}
	case "approval grant":
		return []string{"id", "issuer"}
	case "approval revoke":
		return []string{"id", "reason"}
	case "approval expire":
		return []string{"id"}
	case "accept":
		return []string{"event", "next"}
	case "return":
		return []string{"event", "reason", "next"}
	case "close":
		return []string{"case", "reason", "source"}
	case "history", "flow show":
		return []string{"case"}
	case "delivery retry":
		return []string{"id"}
	case "delivery resolve":
		return []string{"id", "outcome", "reason", "evidence"}
	case "staff get":
		return []string{"name"}
	case "staff add":
		return []string{"name", "label", "department", "role", "workstation", "approval"}
	case "staff update", "staff remove":
		return []string{"name", "approval"}
	case "role show":
		return []string{"role"}
	case "role add":
		return []string{"id", "version", "label", "department", "manual", "approval"}
	case "role retire":
		return []string{"role", "approval"}
	case "nudge enqueue":
		return []string{"id", "dedupe", "to", "message"}
	case "nudge claim", "nudge deliver":
		return []string{"id", "claim"}
	case "nudge status":
		return []string{"id"}
	case "nudge reconcile":
		return []string{"id", "resolution", "ref", "note"}
	case "estop activate", "estop release":
		return []string{"id", "reason"}
	case "runtime fallback", "runtime repair-profile":
		return []string{"agent"}
	default:
		return nil
	}
}

func validateRequiredFlags(cmd *cobra.Command, names []string) error {
	missing := make([]string, 0, len(names))
	for _, name := range names {
		value, err := cmd.Flags().GetString(name)
		if err != nil || strings.TrimSpace(value) == "" {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return usagef("缺少必填参数 %s", strings.Join(missing, "、"))
}

func validateConditionalFlags(cmd *cobra.Command, path ...string) error {
	joined := strings.Join(path, " ")
	stringValue := func(name string) string {
		value, _ := cmd.Flags().GetString(name)
		return strings.TrimSpace(value)
	}
	enum := func(name string, optional bool, values ...string) error {
		value := stringValue(name)
		if optional && value == "" {
			return nil
		}
		for _, allowed := range values {
			if value == allowed {
				return nil
			}
		}
		return usagef("--%s 只能是 %s，实际=%q", name, strings.Join(values, "|"), value)
	}
	priority := func() error {
		value := strings.ToUpper(stringValue("priority"))
		for _, allowed := range []string{"P0", "P1", "P2"} {
			if value == allowed {
				return nil
			}
		}
		return usagef("--priority 只能是 P0|P1|P2，实际=%q", stringValue("priority"))
	}

	switch joined {
	case "case create":
		if err := priority(); err != nil {
			return err
		}
		parent, project := stringValue("parent"), stringValue("project")
		if parent == "" && project == "" {
			return usagef("创建唯一 root case 必须显式提供 --project NAME；child 则使用 --parent PARENT_CASE_ID 并省略 --project")
		}
		if parent != "" && cmd.Flags().Changed("project") {
			return usagef("child case 的 project 自动继承；使用 --parent %s 时必须删除 --project", parent)
		}
	case "case revise":
		if err := priority(); err != nil {
			return err
		}
		for _, forbidden := range []string{"parent", "owner", "project"} {
			if cmd.Flags().Changed(forbidden) {
				return usagef("case revise 不接受 --%s；lineage、owner 和唯一 project 不可由 revise 改写", forbidden)
			}
		}
	case "case escalate":
		if err := priority(); err != nil {
			return err
		}
		if cmd.Flags().Changed("project") {
			return usagef("escalation child 自动继承唯一 project；删除 --project")
		}
	case "report":
		if err := enum("result", false, "completed", "blocked", "needs-decision", "finding", "returned"); err != nil {
			return err
		}
		if severity := strings.ToUpper(stringValue("severity")); severity != "" && severity != "P0" && severity != "P1" && severity != "P2" {
			return usagef("--severity 只能是 P0|P1|P2，实际=%q", stringValue("severity"))
		}
		caseID, result := stringValue("case"), stringValue("result")
		switch result {
		case "completed":
			if stringValue("artifact") == "" && stringValue("source") == "" {
				return usagef("report result=completed 缺少可复验证据：至少提供 --artifact PATH 或 --source PATH；可执行模板：hq report --case %s --result completed --artifact PATH --verify TEXT --next TEXT", caseID)
			}
		case "finding":
			missing := missingReportEvidence(
				reportEvidenceField{"--severity", stringValue("severity")},
				reportEvidenceField{"--source", stringValue("source")},
				reportEvidenceField{"--location", stringValue("location")},
				reportEvidenceField{"--verify", stringValue("verify")},
			)
			if len(missing) != 0 {
				return usagef("report result=finding 缺少条件必填参数 %s；可执行模板：hq report --case %s --result finding --severity P1 --source PATH --location TEXT --verify TEXT --next TEXT", strings.Join(missing, "、"), caseID)
			}
		case "blocked", "needs-decision", "returned":
			missing := missingReportEvidence(
				reportEvidenceField{"--source", stringValue("source")},
				reportEvidenceField{"--note", stringValue("note")},
			)
			if len(missing) != 0 {
				return usagef("report result=%s 缺少条件必填参数 %s；可执行模板：hq report --case %s --result %s --source PATH --note TEXT --next TEXT", result, strings.Join(missing, "、"), caseID, result)
			}
		}
	case "issue":
		if cmd.Flags().Changed("delivery") && stringValue("delivery") != deliveryModeWakeup {
			return usagef("case 委派固定使用 --delivery wakeup；不能降档为 %q", stringValue("delivery"))
		}
	case "message":
		if err := enum("kind", false, "info", "question", "request", "handoff"); err != nil {
			return err
		}
		if err := enum("delivery", true, "auto", "wakeup", "quiet", "inject"); err != nil {
			return err
		}
	case "project list":
		if status := strings.ToLower(stringValue("status")); status != "" && status != "active" && status != "review" && status != "blocked" && status != "closed" {
			return usagef("--status 只能是 active|review|blocked|closed，实际=%q", stringValue("status"))
		}
		if value := strings.ToUpper(stringValue("priority")); value != "" && value != "P0" && value != "P1" && value != "P2" && value != "UNSET" {
			return usagef("--priority 只能是 P0|P1|P2|unset，实际=%q", stringValue("priority"))
		}
	case "assignment list":
		return enum("status", true, "issued", "accepted", "submitted", "rework", "completed", "reported", "returned")
	case "delivery resolve":
		return enum("outcome", false, "delivered", "not-delivered")
	case "nudge reconcile":
		return enum("resolution", false, "delivered", "not-run")
	case "index query":
		return enum("entity", false, "flow_events", "cases", "deliveries", "documents")
	case "staff add":
		if err := enum("permission-mode", false, "native", "yolo"); err != nil {
			return err
		}
		return enum("activation", false, activationAlways, activationOnAssignment, activationManual)
	case "staff update":
		enable, _ := cmd.Flags().GetBool("enable")
		disable, _ := cmd.Flags().GetBool("disable")
		if enable && disable {
			return usagef("--enable 与 --disable 不能同时使用")
		}
		if cmd.Flags().Changed("permission-mode") {
			if err := enum("permission-mode", false, "native", "yolo"); err != nil {
				return err
			}
		}
		if cmd.Flags().Changed("activation") {
			if err := enum("activation", false, activationAlways, activationOnAssignment, activationManual); err != nil {
				return err
			}
		}
		meaningful := false
		for _, name := range []string{"label", "department", "kind", "permission-mode", "reports-to", "role", "workstation", "activation", "keep-warm", "max-wip", "grant", "revoke", "enable", "disable"} {
			meaningful = meaningful || cmd.Flags().Changed(name)
		}
		if !meaningful {
			return usagef("staff update 至少需要一项席位、属性或权限变更")
		}
	case "role add":
		capabilities, _ := cmd.Flags().GetStringSlice("capability")
		for _, capability := range capabilities {
			if strings.TrimSpace(capability) != "" {
				return nil
			}
		}
		return usagef("至少需要一个 --capability；该参数可重复")
	}
	return nil
}

func cobraLeafArgs(cmd *cobra.Command, positional []string) []string {
	result := make([]string, 0, len(positional)+cmd.Flags().NFlag())
	cmd.Flags().Visit(func(flag *pflag.Flag) {
		switch flag.Name {
		case "office", "config", "data", "herdr", "dry-run", "json", "direct", "maintenance-pane":
			return
		}
		if flag.Value.Type() == "stringSlice" {
			values, _ := cmd.Flags().GetStringSlice(flag.Name)
			for _, value := range values {
				result = append(result, fmt.Sprintf("--%s=%s", flag.Name, value))
			}
			return
		}
		if flag.Value.Type() == "stringArray" {
			values, _ := cmd.Flags().GetStringArray(flag.Name)
			for _, value := range values {
				result = append(result, fmt.Sprintf("--%s=%s", flag.Name, value))
			}
			return
		}
		result = append(result, fmt.Sprintf("--%s=%s", flag.Name, flag.Value.String()))
	})
	return append(result, positional...)
}
