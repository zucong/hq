# HQ 产品设计

状态：**v1.0.0 已正式发布；本文定义当前正式合同**

产品：HQ for Herdr

范围：单机、单 OS 用户下的虚拟公司总部控制面

## 1. 产品定义

HQ 把一组运行在 Herdr 中的 agent 组织成一家公司。它提供稳定的组织身份、汇报关系、工作流、
可靠投递和审计恢复，同时保留 Markdown 与 Git 中的原始业务内容。

HQ 解决五类问题：

1. 组织事实不能散落在脚本、Prompt 和多份手册里；
2. Prompt 只能通知，不能证明业务已经接收或完成；
3. 每个 case 需要明确的授权、负责人、状态、交付物和销账责任；
4. agent 会话会停止、漂移或发生不确定投递，需要可恢复的运行模型；
5. 虚拟公司需要在不引入中心数据库服务的前提下留下严格审计链。

HQ 的产品承诺是“让组织行为可配置、可核验、可恢复”，不是替 agent 做业务判断。

## 2. 设计原则

- **原文与状态分离**：只有具备独立阅读价值的长内容进入文件；流程动作进入事件账本。
- **文书数量受控**：case、委派、接收、回报、退回、消息、提醒和销账不得自动生成 Markdown。
- **组织配置化**：员工、部门、岗位手册、kind、汇报线和权限来自 registry。
- **身份以运行事实为准**：gateway 用 Herdr snapshot 重新核验 pane，而不是信任自报姓名。
- **先记账后按铃**：所有投递先写 durable intent/attempt，再调用 transport。
- **派生数据可丢弃**：`state.json` 与 SQLite index 不拥有业务真相。
- **不确定性显式化**：ambiguous/unknown 是合法状态，不能通过盲重试掩盖。
- **权限最小化**：业务命令、组织维护和运维急停是不同 capability。
- **协议可演进**：config、event、gateway 和 snapshot 各自独立版本化。

## 3. 用户与职责

| 角色 | 需要的能力 |
|---|---|
| 公司所有者 | 查看全局状态、确认关键授权、决定组织与产品演进 |
| 总裁秘书 / `approval_witness` | 作为人类所有者与总部的双向沟通管道，上传决定、下达公司级事项、汇总证据 |
| 部门经理 | 向精确直属派活、接收下属报告、协调跨部门工作 |
| 执行角色 | 接收 assignment、提交报告、向冻结 acceptor 上行 |
| 销账职责位 | 核验关闭依据并关闭事项 |
| 运维角色 | 启动公司、巡视运行态、处理 unknown、执行急停与恢复 |

权限绑定 employee seat 中的职责和显式 `can_*` flags，不绑定 Go 源码里的具体人名。Role Card 的
capabilities 只描述行为与证据标准，不会隐式授权。公司所有者的稳定标识来自
registry 的 `owner_principal`；approval grant 与 decision 的 `confirmed_by` 必须精确匹配该配置，
见证人仍由唯一的 `approval_witness` 职责位确定。

总裁秘书是该职责位的组织称谓，不是固定 agent 名字。具体 name、nickname 和 sender label 均可配置，
也不产生隐含权限。总裁秘书只把人类所有者已经明确作出的决定上传为可审计的 decision/approval，
据此向部门经理下达公司级事项，并把已验收证据、风险、异议和待决问题汇总给人类。它不得把模型推断、
默认值或沉默解释成授权，也不得代替人类决定组织变更、产品方向、优先级或风险接受；授权含糊时必须回问。

## 4. 产品边界

### 目标

- 用 `hq up` 幂等启动 registry 中的 agent 并确保 gateway 在线；
- 用一个命令树管理人员、case、授权、消息、报告和销账；
- 让纵向派活、上行回铃、接收、退回和关闭形成可重放事件链；
- 支持离线队列、回合边界投递、重试与人工核对；
- 从 Herdr snapshot 发现身份漂移、orphan 和持续死亡候选；
- 在崩溃、并发和部分失败后确定性恢复。

### 非目标

- 不替代报告、方案、finding、决策和岗位手册原文；
- 不自动批准需求、评判质量、改变优先级或替公司所有者拍板；
- 不允许自由文本直接覆盖有限状态机；
- 不开放任意 SQL、任意路径写入或任意 shell transport；
- 当前不提供跨机器多租户或抵御本机恶意同用户进程的强身份隔离；
- 当前不包含 Web UI。
- 不为传令、接单、回铃或销账等过程步骤自动生成 Markdown 存根。

## 5. 体系结构

```text
部门 agent
   │ hq CLI + HERDR_PANE_ID / HERDR_WORKSPACE_ID
   ▼
Unix socket gateway
   ├── Herdr snapshot：身份、cwd、workspace、tab、pane、kind、运行态
   ├── registry：组织、汇报线、职责、权限、岗位手册
   ├── transition：有限状态机与授权合同
   ├── transaction store：append-only ledger + crash recovery
   └── outbox：Herdr Prompt / quiet / inject / cold-resume
           │
           ▼
       目标 agent

board / project / flow / history / index
   └──严格重放 ledger，再生成只读视图
```

### 源码与部署边界

产品源码位于独立仓库根。一个公司实例以 `<company-root>/ceo-office` 为运行锚点，
registry 固定为 `ceo-office/tools/hq/config.yaml`，业务数据固定为 `ceo-office/records`。
源码可以独立构建和发布，但正式实例不允许通过 `--config`、`--data` 或 `--herdr` 改写这些信任根。

`--office` 是公司实例选择器，不是测试依赖注入入口。测试必须显式注入 Store、IdentityProvider、
DeliveryTransport 和 fake Herdr。

`hq init <company-directory>` 是唯一公司初始化入口。它不调用 LLM，而是把交互式字段或 `--silent`
参数编译成同一份确定性 init plan。通用组织可从 `minimal`、`product-engineering`、`saas`、
`professional-services`、`commerce`、`virtual-company` 中选择模板；专属组织用严格 YAML
`--organization-spec` 直接编译，不能与模板同时使用，也不会先创建模板角色再覆盖。两条路径都生成严格
registry、固定岗位手册、公司成立决策和公司本地 binary。默认引导顺序是“总部联系职责位 → gateway →
其余 always 编制”；`--prepare-only` 不连接 Herdr。prepare-only 目录仍由同一个 `init` 命令完成首次启动，
不增加第三个公开命令，也不允许 `up` 代替 init。

首次启动是内部的一次性 init lifecycle。HQ 在 Herdr mutation 前要求精确的 `company:init` 决策、空业务账本、
空 session lifecycle 和不存在同名 workspace，然后以不可覆盖的 `intent.json` 冻结 config/decision digest。
失败续跑只接受同一 intent 产生的 workspace、always agent/session 和 gateway；配置或成立授权改变时 fail closed。
Herdr 创建 workspace 时自带的 1 号空 root tab 会在总部联系职责位精确在岗后被回收。若 init session 已记账、
但 snapshot 证明对应 agent incarnation 消失，HQ 先追加 `stopped` 并回收仍满足原 seat 合同的 stale tab，
然后才启动新 incarnation；不会把同名 agent 当作同一 session。
全部 always 岗位与 gateway 收敛后才写不可覆盖的 `completed.json`，以后 `init` 不再改变运行态。
公司本地 binary 使实例不依赖源码目录或全局安装的 `hq`；Herdr 与所选 Agent CLI 仍是明确的外部运行依赖。

organization spec v1 显式冻结部门、seat、汇报线、职责、激活与容量、runtime profile、权限和结构化 Role Card。
HQ 在任何目标写入前严格拒绝未知字段、多个 YAML 文档、JSON、symlink、重复/冲突 seat、汇报环、非法职责、
缺失唯一治理职责、无效激活和权限组合。每个 seat 编译为独立 `AGENTS.md`、Role Card digest 和 Employee Seat
digest。原始规范按字节保存为 `ceo-office/formation/organization-spec.yaml`，其 SHA-256 进入成立决策；它仅是
formation evidence，不是 live registry，运行时与后续 staff mutation 只认 `config.yaml`。

## 6. 命令模型

```text
hq
├── init / doctor / version
├── up / patrol / session
├── role / staff
├── case / approval / issue
├── project / assignment
├── message / report / inbox / accept / return / close
├── delivery / reconcile / flow
├── nudge / reminder
├── estop
├── board / history / rebuild / index
└── serve / ping / whoami
```

命令由 Cobra 提供结构化帮助、参数校验和 completion。组织配置只使用严格 YAML v3 解码，不使用
环境变量或多层配置覆盖，以免组织事实被静默改变。

公开 CLI 采用统一的 Agent repair contract。任何命令失败都包装为“精确命令路径与原因 / 当前用法 /
下一步 help 与账本安全恢复”三段式诊断，同时通过 error unwrap 保留 usage、permission、conflict、
temporarily unavailable 和 internal 的原始退出类别。纯参数错误必须在 office discovery、gateway、Herdr
和账本依赖之前失败，因此错误目录、离线 gateway 或 runtime 故障不能遮蔽写错的枚举、互斥项、条件必填项
与 root/child 结构。帮助矩阵和错误矩阵从 Cobra 命令树动态枚举全部公开路径，新增命令自动进入门禁。

通用纠正提示只允许重试尚未记账的命令。出现 event/delivery 已记账事实时，Agent 必须先用 `delivery status`
或 `flow show` 查询并复用稳定 ID；不得重发业务 intent，也不得降级为裸 Herdr Prompt。

业务写命令必须经 gateway。`--direct` 只允许实时在岗的 `can_manage_staff` 角色执行运维白名单：
`up`、`serve`、`rebuild`、`reconcile`、`board --reindex`、`index rebuild` 和 `estop`。
它不是通用权限提升，也不能执行 case/approval/issue/message/report/accept/return/close 或 staff mutation。
`delivery resolve` 是带外部证据的业务恢复动作，仍经 gateway 核验调用者身份。

唯一例外不是 `--direct`：公司已存在有效 init completion 且当前不在 Herdr 内时，无参数 `hq up` 可作为
宿主机冷启动入口。它只能恢复 gateway 与当前 registry 的 always 岗位，拒绝指定 seat 或 `--no-gateway`；
恢复前先以当前 Herdr snapshot 收敛已消失的 active session，为每个旧 incarnation 追加 `stopped`，再为新
workspace/runtime 写入新的 `started`。公司运行态中的 `up` 仍逐次核验实时 `can_manage_staff` 身份。这把
OS 运维启动与公司业务授权分开，同时消除“联络官必须已经在线才能启动联络官”的循环依赖。

### Project 可见性投影

`hq project list` 与 `hq project show` 是 dependency-free 的只读命令。它们严格重放 append-only ledger。一个 HQ space
最多只有一个冻结 `CaseState.Project`（CLI 中的 `case.project`）和一个 root，不读取 Herdr snapshot，也不把 runtime `done/blocked/offline`
映射为业务完成或阻塞。owner/业务状态来自 ledger，department 展示由当前 registry 映射，因此 registry 的
department 变更可以改变分布展示，但不能改变 durable case 结论。投影本身不写 `state.json`、SQLite 或 runtime
文件；派生 state 删除、损坏或重建前后应得到同一个 Project JSON。存在 durable `txn/` intent 时查询 fail closed，
不在只读路径恢复事务，也不返回忽略已提交 intent 的旧投影。

root 创建时必须显式绑定非空 project；之后只能创建有 parent 的 child，child 自动继承 project/root，
`case revise` 不能改 project。严格回放对空 project、第二 root/project、断裂 parent chain 和 project rewrite 全部
fail closed，不提供旧账本迁移或兼容。新项目必须使用新 HQ 目录和新 Herdr workspace。

Project summary 固定包含 `root_case_count=1`、total case 数、case status/priority 计数、owner/department 分布，以及
review、blocked、closure 三类 gap。项目级 `status` 只是这些 durable case 状态的确定性摘要，过滤器
只选择这个完整 summary，不改变项目内部统计。root 的关闭门禁要求全部 descendants 和正式 workflow
delivery 先收敛，因此 root closed 与 Project closed 在合法账本中等价。当前切片不是第二套业务状态源。

## 7. 组织注册表

当前 registry v3 是唯一严格组织 schema：

```text
Config
├── version
├── workspace_label
├── owner_principal
├── delivery_policy
│   ├── default_mode
│   ├── max_consecutive_wakes
│   ├── max_bundle_items
│   ├── max_bundle_bytes
│   ├── assignment_accept_timeout
│   ├── max_activation_redeliveries
│   ├── manager_queue_stall_timeout
│   ├── manager_queue_escalate_after
│   └── max_manager_queue_nudges
├── runtime_profiles.<kind>
│   └── model / reasoning_effort / on_drift
├── runtime_fallback
│   ├── auto / trigger
│   ├── from_kind / to_kind
│   └── permission_mode / agent_args
├── role_cards[]
│   ├── role_card_id / version / label / status
│   ├── department / capabilities / approval_ref
│   ├── manual_path / manual_digest
│   └── role_card_digest
└── agents[]                         # employee seats
    ├── name / nickname / sender_label / department_label
    ├── workspace / department / kind / permission_mode / agent_args
    ├── reports_to / responsibilities / permissions
    ├── workstation_path / manual_path
    ├── role_card_id / role_card_version / role_card_digest
    ├── activation_policy / keep_warm / max_wip / disabled
    └── seat_version / seat_digest
```

`owner_principal` 是当前公司所有者的稳定授权标识。稳定 agent slug 是历史身份；花名和 sender label 可以变化。汇报线不得成环，在职人员不得汇报给停用者。
`approval_witness`、`account_closer` 和每个 `manager:<department>` 都必须唯一且可解释。

Role Card、Employee Seat 和 Assignment 是三层不同合同。角色卡冻结岗位行为、证据标准和边界；seat 冻结
该员工的组织身份、独立 workstation、直属上级、激活策略和容量；issue 再把本次 case 与当时精确的
seat version/digest、role card version/digest/manual 一并冻结。新增角色卡 version 不会静默改变在途 assignment。
`activation_policy=always` 供总裁秘书和部门经理常驻，永不被自动 reaper 关闭；专业下属通常使用
`on_assignment`、`max_wip=1` 和有界 `keep_warm`，由正式 issue 激活，工作终态后自动休眠。
`keep_warm` 只允许 `0s..1h`，省略等价于 `30s`；永久常驻必须改用 `always`。`manual` 席位仅在
明确运维动作下启动，不参与自动启停。Herdr prompt 是门铃而不是第四份业务合同；
显式 `hq up <on_assignment-seat>` 会被拒绝，只有已经 durable 建立的 issue 可以触发 cold-resume。
人类公司所有者或其明确授权代理可以在 seat 已由 HQ 激活后，通过 Herdr prompt 补充精确的外部工具权限；该提示
不创建或改变任何 HQ 业务对象、冻结 digest、reviewer 或 acceptance。外部 connector 的 saved permission（包括 saved deny）是独立
执行层状态：授权文本不能保证清除它，设置变更可能需要重连当前 runtime，但重连不得成为切换冻结 surface 的绕行。

每个 seat 使用唯一的 `<department>/staff/<seat>/v<role-version>/` 工位，个人 `AGENTS.md` 是完整、
自包含的固定角色定义。`ceo-office/tools/hq/config.yaml` 是唯一组织编制与 employee seat 注册表，不存在平行的 Markdown 编制清单。registry 同时冻结
manual digest、role card digest 和 seat digest；任一文件漂移或绑定不一致都 fail closed。
角色手册限制为 1 MiB，并使用 `O_NOFOLLOW`、打开前后 inode 一致性核验和已验证文件描述符读取，
避免 check-then-read 替换窗口。
角色卡通过 `hq role add` 从已存在的独立 `AGENTS.md` 创建；`id@version` 的冻结字段不可变。
每张 role card（包括尚未绑定 employee seat 的 card）都必须独占严格个人路径中的手册；共享、部门根、
祖先或子孙路径均在 registry 校验阶段拒绝。
`hq role retire` 只允许退役没有被任何 employee seat（包括 disabled seat）绑定的 card。
`staff add/update` 显式绑定 `--role`、`--workstation`、`--activation` 和 `--max-wip`，每次变更推进
seat version 并重算 digest；role capabilities 始终不替代 seat 权限 flags。

配置写入在单一跨进程锁内完成：读取旧 bytes/metadata → 严格解析 → 权限与批准校验 →
同目录内容寻址 backup → temp + fsync → atomic rename → parent fsync。失败时保留原文件，不留下半成品。

正式 Store 固定绑定唯一 config path。每个 ledger reader/writer 先取得 process shared lock 与 config-directory
shared flock，在锁内严格重载并完整比较调用者 Config，然后才允许取得 ledger lock、恢复 intent、运行 builder
或重放；staff mutation 按相同顺序取得 exclusive lock，并把 candidate replay 的 ledger lock 保持到 config
atomic rename 完成。陈旧 Config 因此在任何权威账本副作用前 conflict。纯 candidate replay 已由调用者持有
exclusive registry lease，不能再次嵌套 shared lease。

该 lease 的边界是单次 Store 根操作，不是整个包含多次 ledger 操作与外部 Herdr mutation 的长命令。
后一种跨进程 command-boundary registry snapshot 仍是后续工作；当前实现依靠每次后续账本访问的 stale conflict
与 durable outbox 的 ambiguous recovery 保守收敛，不能把它描述成跨 registry/ledger/runtime 的原子事务。
这不等于外部副作用没有串行化：delivery/nudge 另有跨进程 operation lease；但 registry 仍可在长命令的
两次 Store 操作之间改变，后续 stale conflict 只能让协议进入保守恢复。

`staff remove` 只停用身份。改 slug 使用新增、更新引用、停用原身份的显式序列，绝不重写已落账事件。
`permission_mode=native` 只传递显式 `agent_args`；`yolo` 使用已知 kind 的内置必需授权参数，并在显式
`agent_args` 后补齐缺失项。因此显式的模型 effort 或其他参数不会意外取消 `yolo` 运行语义。未知 kind 不猜测权限参数。

## 8. 身份与 Herdr 合同

调用者身份必须同时满足：

- pane 属于目标 workspace；
- pane、tab、agent 和 workspace 的稳定 ID 关系完整；
- agent name 存在于当前 registry 且未停用；
- agent cwd 精确匹配该 seat 的 canonical `workstation_path`；
- kind、tab、pane、live session 与 `interactive_ready=true` 匹配；
- 当前命令需要的 capability 或职责位成立。

Herdr snapshot 允许上游新增字段，但目标 workspace 的合同字段不可缺失。非目标 workspace 的对象
仍需稳定归属，不能借缺省字段冒充目标角色。Herdr 未直接返回 tab cwd 时，只有该 tab 全部 pane
提供相同非空 cwd 才能派生。

`ResolveLiveBinding` 返回的是 point-in-time proof。普通 wakeup 与 nudge 在 durable attempt 前、Prompt 前分别核验一次；
两次核验不是绑定到 attempt 的 incarnation CAS。startup 则在 Start 后确认本次创建的 workspace/tab/pane，
写入 `session_started` 后再在 startup Prompt 前复核同一 created IDs，并在 Herdr 提供时检查 terminal/native
session 连续性与 revision 不倒退；startup Prompt 后还必须等待同一 runtime 回到可安全接收任务的输入边界。
已确认退出（包括 CLI 自动更新后要求 restart）会写 `session_stopped` 并回收 HQ 自己创建的空 tab；无法确认时保留
runtime 并以 partial-start fail closed。发现漂移即以零业务 Prompt 的 prepared/failed/partial-start 事实收敛。
Herdr 当前 Prompt API 仍只接受
agent name，不能携带 expected terminal/native session/revision 并在服务端 compare-and-send；所以最终 snapshot 后的
同名 runtime 替换无法由 HQ 单方面原子排除。该窗口是明确的 Herdr capability boundary，不属于 HQ 的 transport
exactly-once 或原子 binding 保证。

所有 Herdr 子进程使用启动时解析并固定的 canonical 绝对 binary、固定 argv 和 deadline。
mutating 调用的结果分为：

- `definitely-not-run`：进程未启动，可以安全地由显式动作重试；
- `confirmed`：动作完成；
- `ambiguous`：动作可能已执行，必须先读取新 snapshot 收敛，不能盲重试。

### Runtime hibernation 与 cold-resume

runtime 生命周期与员工 seat 分离：关闭 Herdr tab 只结束当前 runtime incarnation，不删除
registry 主键、Role Card、Workstation、Assignment 或业务账本。下一次 durable issue，或原 active assignment
精确绑定的 report return / 合同 authority actionable message，可复用同一 seat，创建新 runtime/session 后递送
原业务 envelope。return 必须匹配冻结 assignment/seat 且当前为 rework；message 必须匹配同 case/recipient 的
未完成 assignment，actor 为冻结 issuer/reviewer/acceptor。无关 case、非 authority、info message 与 consumed
assignment 不取得 runtime-start 权限。

每个 seat 有独立的进程内 FIFO/可取消租约和跨进程 flock。所有 Prompt、cold-resume 与 reap 共用该
runtime-seat 临界区；durable issue 还在写 `issue_prepared`/预占 WIP 之前先取 seat 租约并检查旧
`hibernate_attempting|hibernate_unknown`。如果关闭仍可能延迟生效，新 issue 零 origin、零 WIP 写入地 fail
closed，直到 operator 核验同一 incarnation 并显式 `runtime reap --agent ... --retry-unknown`，或 snapshot
证明旧 runtime 已消失而自动补记 stopped。

reaper 仅评估当前 config 仍为 enabled `on_assignment` 且 seat version/digest 未变的席位。它从最新
durable assignment terminal `event_accepted|event_returned` 的 `At` 计算保温时间；case 的其他更新不延长
runtime 存活期。资格评估要求：

- 精确 session 绑定（workspace/tab/pane/agent/terminal/native session）仍与最终 Herdr snapshot 一致，
  runtime 不得处于 working/blocked；
- 没有 active assignment、目标 seat 持有的未决 case、未决 workflow delivery、nudge 或 reminder；
- 已 sent 的行动型 `question|request|handoff` 必须有 durable Ack；非行动 quiet/inject info 可留在
  Turn Bundle 中随 seat 休眠，下次 issue 唤醒时消费。

session 诊断状态机为 `started → hibernate_attempting → stopped`，可以转入
`hibernate_deferred|hibernate_failed|hibernate_unknown`。CloseTab 前失败是 definitely-not-run/
`failed`；已发起但 snapshot 仍能看到原 incarnation 时是 `unknown`；已不在时前滚到 `stopped`。
failed/unknown 不自动重试，重试 flag 只允许与单一 `--agent` 绑定。如果 CloseTab 已成功但
stopped append 崩溃，下次扫描从“精确 runtime 已消失”补记，不重复 CloseTab。这些运行诊断事件
不改变已成立的业务终态。

gateway 启动即扫且周期性重扫，所以重启不会丢失超时 keep-warm 或未补记 stopped。agent 已消失但
空 tab 存在时，HQ 会收敛 session 但不自动关闭无法证明 ownership 的 tab；后续每次 status/reap 仍显示
`orphan_tab_without_agent`，要求 `hq patrol` 后人工清理。

Herdr 当前 CloseTab 只接受 `tab_id`，不接受 expected terminal/native session/revision。HQ 会在持有 seat
flock 时重读 config、ledger/session 与最终 snapshot，从而拒绝已可观察的替换；但同用户在最终
snapshot 与 CloseTab 之间的外部替换不能由 HQ 单方原子排除。这是明示 capability boundary；
完全消除需要 Herdr conditional close/CAS，HQ 不把最终 snapshot 宣称为原子 close binding。

### Runtime profile desired state

`runtime_profiles.<kind>` 是公司级 native runtime desired state，不是 employee seat 属性。当前 Codex adapter
把 `model` 和 `reasoning_effort` 编译成显式 CLI/config overrides，并从 Herdr 的有界 detection scrollback
最后一个 footer 反查实际值。同 kind 的 seat-local `agent_args` 若声明相同值则去重；冲突值在
Herdr mutation 前 fail closed。这个分层使运行载体变更不会改写 seat digest 或使在途 assignment 失效。

patrol 将无法读取的 profile 报为 `runtime_profile_unverified`，将不匹配报为
`runtime_profile_mismatch`。`on_drift=report` 仅报告；`restart_idle` 由 gateway watcher 仅在
`idle|done` 边界修复，对 `working|blocked` 延后而不中断。修复使用与 hibernation/fallback 相同的
runtime-seat、ESTOP admission、up 和 registry 锁序，并在 CloseTab 前二次核对 incarnation、status 和终端 profile。

session 诊断为 `profile_repair_attempting|failed|unknown`。只有 snapshot 证明旧 tab absence 才记 stopped
并用同 seat/workstation 启动新会话；recovery envelope 仅从严格重放的 actionable work 恢复：需要该 seat
继续处理的 `issued|accepted|rework` assignment，以及无 active assignment 的 owned `open` case。`accepted`、
`finding_accepted`、`blocked`、`needs_decision` 等历史或外部等待状态不得仅因 owner 未变而进入信封。清单最多
展开 8 项，溢出只记录数量并让 Agent 查询 ledger；投递后记 `profile_recovery_sent`。unknown 禁止自动重试；只能在人工核验同一 incarnation/tab 后对单一 seat 执行
`hq --direct runtime repair-profile --agent <seat> --retry-unknown`，避免延迟 CloseTab 与新 runtime 双占座。

### Runtime carrier fallback

`runtime_fallback` 是公司级运行策略，不是第二份员工编制。它只允许已登记 seat 的 primary `kind`
在明确 trigger 下改由一个 fallback kind 占用；seat digest、Role Card、workstation、reports_to 与 active
Assignment Contract 保持不变。这个分层允许既有在途 assignment 在不修改冻结人员合同的前提下恢复。

`content_safeguard` detector 只读 Herdr 的有界 detection scrollback，并要求完整 provider 标记与终端输入提示同时存在。
它不因任务文本单独引用 `This content can't be shown` 而触发，也不对无 durable active work 的空闲 seat 切换。
切换共用 runtime-seat lease、ESTOP admission、up lock 和 current-registry lease。旧 session 先进入 `fallback_attempting`；
CloseTab 后只有 snapshot 证明 tab absence 才写 `stopped` 并启动新 kind。`fallback_unknown` 禁止自动重试，
避免一个 seat 同时出现两个载体。
显式恢复由 `can_manage_staff` 角色在核对同一 Codex incarnation/tab 后执行
`hq --direct runtime fallback --agent <seat> --retry-unknown`；该命令重走终端证据、binding 和 tab-absence 栅栏，
不接受批量 target。若旧 tab 已消失，watcher 可用 snapshot 补记 stopped 后前滚，不会再调用 CloseTab。

新载体的 recovery envelope 沿用 runtime profile 的 actionable-only、有界清单合同，要求新会话重读当前
`AGENTS.md`、`assignment show`、`history` 与同一 workstation。HQ 不声称能复制前一模型的隐藏 transcript；
它保证的是组织身份与持久任务事实连续。投递确认后追加 `fallback_recovery_sent`；如果仅该记账失败，
下一轮可幂等重发 recovery envelope，不重建 case/assignment。

## 9. 业务模型

### Case

`case` 是唯一工作概念，同时保存目标、验收、约束、优先级、版本 digest 和 canonical `spec_ref`。
case 可通过 `parent_case_id/root_case_id` 形成层级。经理持有父 case，创建并分别委派子 case；
员工只推进自己的子 case，经理验收后汇总父 case。子 case 的状态和 owner 不得改变父 case。

### Issue 与授权

`issue` 是唯一纵向派活动词，合法授权来源只有：

1. 经理向精确直属下发；
2. scope 完整、未过期且未消费的一次性 approval；
3. canonical standing decision 中命中的精确 scope。

三者不是供经理任意叠加的替代通道。部门经理向自己的精确直属 seat 派工时，组织汇报线本身就是完整
授权：经理直接 `issue`，不申请也不附加 approval/decision。经理向非直属 seat 派工始终禁止，
approval/decision 不能扩大其管理边界；跨部门工作必须交由目标的直属经理拆分并 issue。总裁办向部门经理
总部联络官只创建一个公司项目 root；各部门 workstream 是该 root 下的 child。总部向部门经理下发
这些公司级 child 时，才使用 owner approval 或已生效 standing decision。

| 发起方 → 目标 | 协议结果 |
|---|---|
| 总裁办 → 部门经理 | owner approval/standing decision 后 issue |
| 部门经理 → 自己的精确直属 seat | 直接 issue，不申请或附加 approval/decision |
| 部门经理 → 非直属 seat | 拒绝；交由目标的直属经理安排 |
| 部门经理 → 自己的直属上级 | `case escalate` 原子创建新子 case 并固定上交；不可自选 target |

`activation_policy=on_assignment` 的直属专业 seat 由这个正式 issue 自动激活：HQ 先建立 durable
Assignment Contract，并在 durable issue intent/prepared 时预占 WIP 以防并发超卖，再通过 Herdr cold-resume
并递送门铃。员工后续 accept 只确认接单，不是容量开始计数的时点。不存在独立的业务 `activate` 步骤；裸 Herdr
prompt 不能建立授权或 assignment，也不应被经理用作补充激活命令。这样经理拥有部门内选人和排班自由，
同时跨汇报线边界仍然 fail closed。拒绝信息必须给出可执行的纠正方向；经理应按报错中的纠正命令执行，
而不是重复申请 approval 或转向裸 Herdr prompt。

### 跨部门返工 escalation

已经 `accepted` 的 submission 是历史事实，不能再 `return`；`message` 又是 projection-neutral，不能承担
所有权交接。为避免经理在权限全部正确时陷入“不能向上 issue、上级也不能替他创建子 case”的死结，
HQ 提供唯一的上行工作交接原语 `case escalate`：

```text
父 case（owner=部门经理，状态与历史不变）
  └─ 原子 batch
       1. case_created:              ∅ -> open,      owner=部门经理
       2. case_escalation_prepared:  durable outbox, recipient=registry reports_to
     送达
       3. case_escalation_sent:      open -> escalated, owner=直属上级
     上级核验
       4. accept(escalation):        escalated -> accepted, owner=直属上级
     正常公司级授权
       5. issue:                     accepted -> dispatched, owner=上级的直属经理
```

调用者必须具有 `manager:<department>` 与 `can_create`，必须是父 case 当前 owner，父 case 不得 closed、
不得存在 active assignment 或未收敛 business delivery。新 case 的 parent/root/spec/digest 与上行 intent
在同一事务中冻结；strict replay 要求二者 sequence 紧邻、command digest 相同且 command id 为 batch 配对。
目标参数不存在，recipient 必须精确等于调用者当前 `reports_to`，并具备继续路由 durable case 的能力。
`case_escalation_prepared` 是 business delivery fence：failed/unknown 期间新 case 保持 open 且由原经理持有，
只能恢复同一 delivery；sent 后进入 `escalated`，在上级 accept/return 前禁止 revise、拆分和 issue。
这条协议不改父 case，不改旧 report，也不伪造其他部门或总裁办身份。

返工闭环回到原质量 seat 时不复活旧 assignment。总部对工程证据的 `accept` 与随后发给质量经理的
`message` 只确认、传递上下文；原质量经理必须在自己仍持有的原 case 上建立新一轮合同：

```text
旧 QA assignment + finding submission + accept   immutable / consumed
message(handoff)                                   projection-neutral
case_revised(vN+1)                                 status/owner 不变，新 generation 与 digest
issue(同一直属 QA seat)                            finding_accepted -> dispatched，fresh assignment_id
accept -> report -> manager accept                 独立复验闭环
```

`case revise` 要求当前 owner、没有 active assignment，并追加引用当前 spec event/digest；strict replay 将新 issue
冻结到 vN+1 digest 和新 business generation。旧员工若在只有 message、没有 fresh issue 时再次 `report`，HQ
按 `case_id + recipient` 识别已消费 assignment，拒绝且不写账，并向当前 owner 给出可执行的
`case revise --version <N+1>` 与 `issue --to <原 seat>`。因此 case 可以通过显式版本进入下一轮工作，而已验收
submission 和旧 Assignment Contract 不会被倒转或复用。

一次性 approval 精确绑定 case、action、target、case version/digest、business generation、
target seat version/digest 和有效期。grant 与 issue 都重验当前 generation/seat，防止 case 往返后
ABA 复活或同 slug 换岗后误用旧批准。`approval_consumed` 与 `issue_prepared` 必须紧邻、共享
同一原子命令 digest 并在同一事务 batch 中提交，避免批准被消费但派活意图丢失。
不支持跨 generation 的 reusable approval。requested/granted approval 与 active/pending assignment 都在 strict
ledger tail 中占用冻结 seat；尾态同时核对 `max_wip`，不允许通过直接改 registry 绕过容量和换岗边界。

### Message

`message` 支持 `info`、`question`、`request`、`handoff`。正文硬上限 2 KiB；结构化引用使用
`--ref-file/--ref-case/--ref-message/--ref-event`，prepared 到 ack 共用稳定 message ID。它可以关联 case、thread 和原文引用，
但所有 message/delivery/ack 事件都从 case 业务投影中排除。接收方需要承接工作时，必须另行合法 `issue`。
行动型 `question|request|handoff` 的总线信封必须携带精确 `hq message ack --message <message-id>`；
接收方读懂后写入 durable Ack。Ack 只证明收到，不接管业务所有权；在 Ack 前，reaper 同时保留发送方与
接收方 runtime。普通 `info` 不需要 Ack。

业务叙述字段 `next_action`、`note`、`verification` 以及 return/close reason 与 message
使用同一个合法 UTF-8 2 KiB byte 级硬上限，仍保持单行。标识符、标签、结构化
引用和运维短提醒仍使用 200 rune 上限。issue 门铃还携带冻结合同、role card 和 seat
元数据，因此不再用旧的 1000-rune 总长限消耗业务字段配额；它统一受 64 KiB base payload
总线边界约束。

### Report、Accept、Return、Close

`issue` 建立 first-class Assignment Contract，冻结 case version/digest、project、issuer、
assignee、reviewer、acceptor、due 与 digest。新合同必须先由 assignee accept。`report` 在合同存在时
只能返回冻结 acceptor；没有 assignment 的当前 owner 才按 functional `reports_to` 回退。
`report_sent` 使合同进入 `submitted`，return 进入 `rework`，report accept 才进入 `completed`；
活动合同禁止 revise/close。当 `blocked/needs-decision/returned` submission 让 reviewer 成为 case owner 时，
owner fallback 仍必须拒绝 case 上任何未完成的 assignment；reviewer 必须先 accept/return 当前 submission，
不得把 functional `reports_to` 当作绕过合同的第二条上行路径。
`accept` 表示接收方完成要素核验，不代表质量一定正确；`return` 必须提供原因和复交条件；
`close` 只能由 `can_close` 角色产生，不能通过自由状态字段绕过。

主要状态链：

```text
case_created → open
issue_sent → dispatched
accept(issue) → in_progress
report(completed) → reported
accept(report) → accepted
close → closed
```

其他 report result 可以进入 `blocked`、`needs_decision`、`finding_reported` 或 `returned`。
状态变化由同一 transition table 在写入和重放时验证。

## 10. 投递模型

### Durable outbox

```text
prepared → attempted → sent
                    ├→ failed_pre_send
                    └→ unknown
```

origin 保存稳定 delivery ID、target、payload digest 和业务事件引用。attempt 在调用 transport 前落账。
`failed_pre_send` 可显式 retry；`unknown` 只能由运维者根据证据 resolve。HQ 对 Herdr Prompt 采取
保守 at-most-once 策略，不承诺跨进程 exactly-once。

`issue_prepared` 与 `report_prepared` 是业务投递栅栏。origin 仍匹配 case 当前 generation/version/digest 且
delivery 未 sent 时，同 case 不得新建 issue/report、revise/close、拆分子 case 或做其他业务推进。
`failed_pre_send` 和人工确认 `not-delivered` 后仍保留栅栏，恢复动作是 retry 原 delivery。`unknown` 只能按
外部证据 resolve。当前协议要求每个 case 业务轮只有一个适用 origin；重试前必须再次确认
它是唯一栅栏，且 case version/digest/from-state 仍一致。只有该 origin 自身的 sent 或有证据的
resolved-delivered 才能收敛栅栏；过期 retry 在 transport 前失败且零外部副作用。

每个 delivery 和 nudge 按稳定 ID 取得跨进程 striped operation lease。每个 seat 的 Prompt/cold-resume/reap
另共用独立 runtime-seat process/flock 租约，不复用 operation stripe。租约覆盖状态重读、durable attempt、
Runtime Admission、Herdr/transport 副作用与 durable terminal；retry/reconcile/resolve 共用同一 operation 锁。
崩溃时内核释放 flock，恢复方随后只能把无终态 attempt 保守解释为 unknown，不会与仍在执行的调用并发。
全局顺序是 `operation process/flock → runtime-seat process/flock → ESTOP admission → up lock
→ registry process/config-directory flock → ledger flock`；任一临界区都不得反向取锁。

业务状态与通知分离：`issue/report` 只有在 sent 或 resolved-delivered 后才推进 case；
`accept/return` 的业务状态先在 origin 事务中成立，后续通知失败不能回滚已经成立的核验事实。

### 回合边界投递

底层投递由两个维度组成：context position（next-turn / next-step）与 wake（true / false）。
对外暴露：

- `wakeup = next-turn + wake=true`
- `quiet = next-turn + wake=false`
- `inject = next-step + wake=false`
- `auto` 根据目标忙闲、消息种类和连续唤醒预算选择

`issue` 固定为 wakeup。quiet/inject 不主动唤醒离线目标；HQ 在下一次已有 wakeup prompt 中按
sequence FIFO 选择同时满足 `max_bundle_items` 与 `max_bundle_bytes` 的精确前缀，或在目标执行
`accept` 时附加上下文。attempt 事件冻结 delivery/payload manifest、真实 base/envelopes、最终 prompt digest、
policy limit 和 overflow；attempt 同时持久化 reservation，使并发 wake 只能选择互不重叠的 pending FIFO
prefix。sent 或 resolved-delivered 会在一个 journal batch 中生成所选 context 的 attempted/sent/claimed 事实并
最后写入父终态；failed/not-delivered 释放 reservation，unknown 保持 reservation 且不 claim。strict replay
依据持久化的真实 base/envelopes 重算 item bytes、prompt digest 与 manifest digest。默认上限为 8 条/16 KiB；
2 KiB 仍限制单条用户 `message --text`。这是 HQ ledger 内的 exactly-once context consumption，不是外部
Herdr Prompt 的 transport exactly-once；ambiguous 结果仍必须人工核对。

issue 的 transport `sent` 与员工 activation 分层记账。`issue_sent` 创建唯一 Assignment Contract；若超时仍为
`issued`，而已记账 runtime 已消失，gateway watcher 会先收敛旧 session，再凭该冻结 assignment cold-resume 同一
seat。只有新旧任一精确 session/incarnation 在线、Herdr status 为 idle/done 且终端证据为正常输入页时，才重放
origin 的原始 payload。重放使用同一 delivery/assignment，不推进 case、不预占第二份 WIP，
并以 `assignment_activation_attempted → sent|failed_pre_send|unknown` 记录外部副作用边界。attempted 崩溃残留
保守转 unknown；unknown 禁止自动重投并复用 `delivery resolve`，有界额度耗尽后写 exhausted，显式
`delivery retry` 可在经理再次核验后恢复。员工 durable `accept` 是唯一 activation acknowledgement，之后 watcher
不再重放。Codex 的 hook-trust/content-safeguard 页面不满足安全输入证据，因此不会接收重投。

activation acknowledgement 之后由独立 assignment-progress watchdog 兜底：非经理 seat 的 assignment 若仍为
`accepted|rework`，而精确 runtime 已在 `idle|done` 超过同一 queue timeout，则 gateway 以 durable nudge 要求原员工
查询冻结 assignment/history 并在原 case report；有界催办后沿 `reports_to` 升级。若 runtime 已消失且 policy 为
`on_assignment|always`，先收敛旧 session，再在同一 seat/workstation cold-resume，并追加
`[HQ runtime recovery] trigger=assignment_progress`，不复制隐藏会话。所有这些动作都不生成第二个 assignment、
不替员工 report、不替经理验收；恢复清单同样只含 actionable work、最多展开 8 项，溢出由 Agent 查询 ledger；
人工 nudge 仍只面向管理/总部职责位，不能成为绕过 issue 的派工通道。

`delivery consume` 与 accept-time context render 会在 ledger flock 内选择、输出并提交 claim，从而拒绝并发
bundle/consume 抢占；但 stdout/调用方输出不是 durable transport。若进程在 render 成功后、写入 journal intent
前崩溃，同一 envelope 可能在恢复后再次暴露。需要一次外部可见性的场景必须在后续增加两阶段 output
lease、ack/reconcile；HQ 不把人工 consume 或本地 render 宣称为 exactly-once。
`delivery consume` 仅保留为人工恢复/调试入口。协调事件进入审计账本，但不混入模型业务正文。
新入职成员在收到直属经理首个 durable case 前，peer/cross-department message 强制静默排队。
Agent 原生启动参数先传递 registry 的 `agent_args`；`permission_mode=yolo` 再为 claude/codex/copilot/cursor/
gemini/grok/kimi/opencode/qwen 补齐该 kind 的必需自动授权 argv。未知 kind 不猜测参数。
如存在 `runtime_profiles.<kind>`，最后编译经验证的显式 model/effort overrides；任何冲突都在启动 tab 前的 config validation 拒绝。
不同 CLI 的“自动”语义并不完全等价；如需收紧权限，必须把 seat 显式改为 `permission_mode=native`，
不得在声明为 `yolo` 的同时依靠自定义 argv 偶然降权。

### Nudge 与 Reminder

`nudge` 在统一账本内提供 TTL、active dedupe、原子 lease 和人工 reconcile。投递前必须再次确认目标是
精确在岗且处于 `working|idle|done` 的经理或唯一总部联络职责位。因此，目标在回合结束后进入 idle/done
仍然可以由 HQ 唤醒；角色手册不是唯一恢复机制。

`reminder scan` 以 case 的 `last_event_id/updated_at/owner` 为 basis，同一生命周期最多提醒一次。
它不会自动关闭 case、改变 owner/status、生成批准或作质量结论。

gateway 的 manager queue watchdog 将 ledger 的 assignment/case 投影与 Herdr live binding 联合判断。它把
submission review、经理自己的 active assignment、以及无 active assignment 的经理持有 open case 视为 actionable
队列，并固定按 `review > work > owned_case` 排序、只在同类内按时间 FIFO。只在精确目标为 idle/done 且当前
最高优先级 status event 超时后工作。每个 `manager + selected status event + stage`
形成 nudge dedupe key，提醒次数有界且跨重启恢复。额度耗尽后，守卫沿 `reports_to` 向上生成一次 durable escalation。
若 Prompt 已尝试但结果不确定，则冻结该 nudge、禁止重投，并升级要求人工 reconcile。守卫不能代替 reviewer 执行
accept/return，也不能改变业务状态；`patrol` 只额外报告 `stalled` finding。

## 11. 事件账本与恢复

当前权威事件只使用 event v3 envelope：

```text
event_version + sequence + event_id + command_id + command_digest
+ previous_event_hash + event_hash + event body
```

`sequence` 是跨月严格递增的正数 `int64`。崩溃允许留下空号，但重放拒绝 0、负数、重复或倒退。
`event_hash` 是移除自身字段后的完整事件 canonical JSON SHA-256；genesis 前哈希为 64 个 `0`。

严格重放拒绝非 v3 envelope、未知或重复字段、多 JSON 值、坏 UTF-8、空行、截断尾行、非法月份、
重复 event/command、哈希断链和非法 transition。已落账 label 按事件原值核验，不受后续改名影响。

写事务：

```text
LOCK
  → 恢复或核验残留 intent
  → 严格重放权威 JSONL
  → command 幂等、权限和业务前置条件
  → 分配 sequence/hash 并生成一个或多个事件
  → intent.tmp + fsync → intent.json + fsync(parent)
  → append JSONL + fsync
  → state.tmp + fsync → state.json + fsync(parent)
  → 删除 intent + fsync(parent)
UNLOCK
```

`command_id + command_digest` 提供请求幂等；ID 相同而摘要不同则拒绝。恢复只在旧 prefix、tail 和
batch 证据全部匹配时前滚，否则 fail-closed。`state.json` 每次都可以由事件重建，不参与业务真相判定。

## 12. 数据与索引

| 路径 | 性质 |
|---|---|
| `config.yaml` | 组织与权限事实源 |
| `records/events/YYYY-MM.jsonl` | 业务事件事实源 |
| `records/state.json` | case 派生状态 |
| `records/sessions/YYYY-MM.jsonl` | 独立会话生命周期 |
| `records/estop/state.json` | 可恢复急停运行状态 |
| `records/hq.sock` | 本地临时 IPC；超过系统 Unix socket 路径上限时，统一映射到当前 uid 独占目录中的 data-dir 哈希地址 |
| `tools/index.db` | 文档与事件派生索引 |

SQLite 只存 Markdown 路径/分类/机械元数据，以及严格重放得到的 flow/case/delivery 投影。
它不保存正文，不反写事实源，也不开放任意 SQL。重建使用同目录 temp、file fsync、atomic rename
和 parent fsync；失败或并发运行保留旧 index。

## 13. 运行健康与急停

`doctor` 汇总实例锚点、registry、岗位手册、决策、Herdr、gateway、账本和 company health；全程只读。
`patrol` 读取两份 snapshot，并结合只读 ledger 投影报告 blocked、经理队列 stalled、编制漂移、orphan 和持续死亡候选。单一信号或短暂 blocked
不会被直接判断为死亡，patrol 也不自动处置。

ESTOP 先持久化稳定冻结集与 active intent，再关闭非豁免子角色。精确在岗的经理和唯一
`account_closer` 豁免。ambiguous close 先 snapshot reconcile；release 只恢复本次 confirmed frozen
且仍符合 registry 的角色。任何部分失败都保持 active recovery state，不宣称完成。

Agent start/resume/prompt 与 control-plane 是不同 admission action。默认 `hq up` 和直接 `serve` 只要会创建或
恢复 gateway，就必须额外通过 control-plane admission，不能借经理/account-closer 的 Agent 豁免恢复写入面。
`--no-gateway` 与 delivery cold-resume 只允许复用已存在的精确 workspace，不获得 CreateWorkspace 权限。

Gateway `serve` 启动的 admission 为：只读 control-plane SH preflight 并立即释放 → 每条 delivery 先取 operation
lease 的 outbox reconcile → 再取 control-plane SH 完成 data root、stale socket、bind/chmod 与 listener identity。进入
长期 accept loop 前释放 SH，在线 gateway 因此不会阻止 ESTOP EX，仍可提供 activate/release 控制通道。
这个次序同时避免 `ESTOP SH → operation` 与 `operation → ESTOP SH` 的反向锁序。默认 `hq up` 在等待
子网关健康前释放 `.hq-up.lock`，由独立 gateway bootstrap lock 串行并发父进程；子网关重放遇到
prepared 且目标离线的 wakeup message 时，cold-resume 可以取得 up lock，不会把尚未调用 Prompt/Start 的恢复
误标成 unknown。

## 14. 安全边界

- gateway socket 为 `0600`，ping/pong 绑定协议版本、workspace 和随机 server identity；
- stale socket 只有连续两次确认无 listener 才回收；
- client/server 有固定 payload 上限、严格单 JSON + EOF 和 handler 并发上限；健康/framing deadline 保持短，
  已验证业务请求的 deadline 则覆盖 Herdr start + prompt 上限及快照/落账余量；
- 服务端用统一 request context 约束整条 dispatch，并传播到 identity、snapshot、cold-resume、startup/final
  Prompt，以及可取消的 config/operation/ESTOP admission/ledger/cold-resume 锁等待；socket deadline 额外
  保留协议响应 grace，外部 attempt 之后的 sent/failed/unknown 终态可在该 grace 内保守落账；
- 已连接的业务请求若没有返回可验证响应，客户端按结果不确定处理，禁止盲重试，并指向本地 ledger 的
  `history`/`case show`/`assignment list` 只读核验；
- config、岗位手册、引用和 records 必须位于 canonical 允许根，拒绝 symlink 与类型混淆；
- 普通引用只允许公司根或显式登记的 archive 根，Git 引用必须解析为真实 commit；
- 短字段拒绝疑似密钥、Cookie、口令、secret 和金额，但这只是辅助防线，不替代资料分级。

同一 OS 用户可以伪造另一个在线 pane 的本地调用；snapshot 只能证明 pane 存在，不能证明 socket 调用者拥有它。
同一用户进程还可能改写 config、approval、个人手册、digest 或 HQ binary。因此当前模型是“防误操作 + 强流程核验”，
哈希与批准文件是审计/漂移证据，不是敌对同用户环境中的信任根。强敌对部署需要不可伪造并绑定 runtime incarnation
的 Herdr capability，以及员工不可写的独立 HQ supervisor、registry/ledger、签名 approval key 和只读 binary/manual 挂载。

## 15. 当前协议合同

- registry 只接受 `config.yaml` 的严格 YAML v3；JSON 和其他 schema 直接拒绝；
- ledger 中的权威 envelope 只接受 event v3；
- 角色卡、employee seat 和 assignment 是当前唯一组织与委派模型；
- `on_assignment` runtime hibernation 不删除 seat/角色卡/工位，不改变业务终态；
- v1.0.0 只承诺当前 registry v3、event v3 与 CLI 合同；开发中的其他格式不是产品输入；
- 产品改进以实际使用反馈驱动，但不能牺牲可审计性、幂等、恢复和权限边界。

## 16. 验收标准

一个可投入使用的 HQ 公司实例应满足：

1. registry 中的常驻角色可由 `hq up` 幂等启动，gateway 可通过版本化 ping；
2. 配置外 agent、错误 cwd/kind、伪造 sender 或错误 workspace 被拒绝；
3. 部门 agent 无需直接写 CEO 办公室文件即可完成合法业务写入；
4. issue → accept → report → accept/return → close 能形成完整、可重放链路；
5. prepared/attempted/sent/failed/unknown 有完整投递审计，unknown 不自动重投；
6. 一次性 approval 并发消费只能成功一次，standing decision scope 可稳定重算；
7. 删除 `state.json` 和 `index.db` 后可以从权威数据重建；
8. 改花名、新增或停用员工不要求修改 Go 路由；
9. patrol、session、runtime reaper、reminder 和 ESTOP 在部分失败下保持保守、可恢复；
10. 真实公司场景模拟覆盖总裁办/产品/工程协作、退回复交、项目销账，以及投递超时后的证据恢复；
11. operation-lock、runtime-seat lock、ESTOP、business-delivery fence、Turn Bundle 与 live-binding 竞态测试和 full test、race、vet、gofmt、
    首验、CLI acceptance、release checksum 门禁全部通过。
12. 全部公开命令/子命令、可见 flag、必填 flag、纯参数失败与错误类别通过 Agent 自解释 CLI 动态矩阵；无文档
    首次使用 Agent 能仅凭 stderr 与 `--help` 推导 root/child、report、assignment 和 runtime 命令模板。
