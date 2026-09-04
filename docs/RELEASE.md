# HQ v1.2.1 正式发布与安装

HQ v1.2.1 是 v1.2.0 的投递活性修复版本：保留 registry v3、event v3 与公司实例合同，
修复连续唤醒预算的回合边界重置，并为被预算降级的行动消息增加可审计的延迟唤醒。本文定义它的构建、
验证和新公司安装流程。该制品只配套
当前 registry v3、event v3、不可变 Role Card、独立 Employee Workstation 和 Employee Seat 合同。
开发期间的其他命令、配置或账本格式不是发布输入。

## v1.2.1 投递活性修复

- 每个成功 wakeup 即使未合并 pending context，也在 delivery terminal 的同一原子 batch 中写入
  `delivery_budget_reset`，避免预算按进程寿命永久累积；
- gateway 新增 queued-action watchdog：只恢复因 `wake-budget-exhausted` 降级的行动消息，在目标
  `idle|done` 且超时后发送去重、有界的 durable nudge，要求目标用 `hq delivery consume` 取得 FIFO 原文；
- 显式 `quiet|inject` 保持调用者要求的静默语义；待消费行动消息优先于通用 assignment/manager 催办，避免旧上下文
  驱动错误推进；
- 回归覆盖无 bundle 的连续成功 wake、尚未收敛 wake 导致的预算降级、延迟恢复、去重，以及显式 inject 不被提升。

## v1.2.0 加急在途变更

- `message --kind directive --urgency urgent` 精确绑定 active assignment 和冻结 authority，强制在目标下一安全
  回合投递，即使目标 working 或普通 wake budget 已耗尽也不静默降级；它要求 ack，但保持业务投影不变；
- gateway 对 sent 但未 ack 的加急 directive 做稳定去重、有界 durable 催办，并沿接收者 `reports_to` 升级；
  守卫不代 ack、不改 case/assignment，也不以 Ctrl-C 中断半个模型回合；
- `case revise --supersede-active --next ...` 在一个原子 batch 中消费旧 assignment、建立 case vN+1、准备发给
  同一直属 assignee 的 replacement；旧合同立即永久失效，failed-pre-send 只允许 retry 原 delivery；
- strict replay 强制 supersede/revise/replacement 紧邻配对、完整合同绑定、同一汇报线与 v3 replacement digest；
  submitted 必须先 return，防止已到 review 的证据被吞掉；
- 回归覆盖忙碌投递、预算绕过、ack 催办/升级、失败恢复、幂等重试，以及“总部联络职责位 → 部门经理 →
  直属员工”的两跳真实组织变更场景。

## v1.1.5 事件驱动的经理停车

- 经理成功 `issue` 后，HQ 重放最新队列并在文本及 JSON 中返回 `actor_directive=continue_queue|end_turn`；
  `continue_queue` 给出最高优先级 durable 动作，`end_turn` 明确要求结束回合并禁止 sleep/进程/Herdr/产物轮询；
- child submission、blocked/needs-decision、投递异常或执行升级复用现有 durable 回铃唤醒经理，不新增 wait/park
  子命令，也不写第二套业务状态；
- patrol v3 新增 `busy_without_action` 计数与 `manager_busy_without_action` finding，只读报告持续 working 的无动作
  监督者；因为 working 仍可能包含合法独立规划，HQ 不自动中断 runtime；
- startup、runtime-profile repair、content fallback 和 assignment-progress recovery 都注入独立 runtime protocol
  version；协议升级不改写不可变个人 Role Card，也不改变其中冻结的角色、职责、人格和业务边界；
- 回归覆盖单一下属停车、多项经理队列继续执行、结构化 JSON 指令、下属执行期间零误催、正式 submission 后单次 review
  唤醒以及 busy-without-action 诊断。

## v1.1.4 委派感知的经理队列修复

- 经理在自己的 `accepted|rework` 父 assignment 下创建并正式委派 child 后，不再因父 status event 未变化而被
  manager queue watchdog 重复催报；执行中的 child 继续由 activation/progress watchdog 负责；
- open child、active child、submitted child 分别路由为经理的委派动作、员工执行和经理 review，避免同一工作同时
  催经理、员工和总部联络职责位；
- child 收敛后以最近一次真实业务 transition 重置父项 stall basis，再给经理一个完整汇总窗口；message 和
  delivery/runtime 维护事件不能伪造进展；
- 回归场景覆盖“上级派经理 → 经理拆解并委派 → 下属执行 → 经理 review → 父任务汇总”，并验证执行期间不催经理、
  review 到达时准确提示、收敛后不会沿用过期父 basis 立即升级。

## v1.1.3 经理 assignment 上行升级修复

- `case escalate` 在 parent 没有 active assignment 时保持原 manager ownership 规则；
- parent 存在 active assignment 时，仅放行直属上级签发给当前经理、冻结同一 case version/digest、并已处于
  `accepted|rework` 执行态的唯一合同，其他组合继续 fail closed；
- escalation 只原子创建并上交 child，不修改 parent、不消费原 assignment；经理随后仍须按原合同 report，
  冻结 reviewer 继续执行 accept/return；
- `issued` 状态的经理会获得可直接执行的 `hq accept --event ...` 纠正命令，避免先 report、虚假完成或创建替代案；
- strict replay 与真实状态回归覆盖“上级派经理处理 → 经理 accept → escalation → 原 assignment report/review”的完整闭环。

## v1.1.2 经理运行恢复修复

- runtime recovery manifest 将经理作为 reviewer/acceptor 监督的 `issued|accepted|rework|submitted`
  下属 assignment 纳入 durable work，避免经理没有亲自接单时被错误判为空闲无任务；
- 恢复信封以 `SUPERVISED_ASSIGNMENT` 区分监督责任与执行权：下属继续执行，经理不得接管或重复委派，
  只有正式 submission 到达后才按冻结合同 accept/return；
- 同一有界清单同时用于 content-safeguard fallback 与 runtime-profile repair，最多仍展开 8 项；
- v1.1.1 的 safety-buffering 与维护 pane 修复保持为当前发布基线。

## v1.1.1 运行守护修复

- Codex 完整显示 `Retry with a faster model / Dismiss and keep waiting` 时固定保留原 model/effort；HQ 按界面
  `No action is required` 的合同不发送任何按键，避免 screen-match 非原子竞态中断正常回合；
- safety-buffering 暂时遮挡 footer 不再被 patrol/profile watcher 误报为 drift，也不会接收叠加 Prompt；
- gateway 每轮从同一 `MaintenanceActor` 的实时 binding 刷新维护 pane；授权 seat 换新 incarnation 后内部
  watchdog 可继续 durable nudge，binding 不唯一或离线时清空过期 pane 并 fail closed；
- v1.1.0 的源码目录结构保持为当前发布基线。

## v1.1.0 结构基线

- Go CLI 与同包测试统一位于 `cmd/hq`，源码构建目标为 `./cmd/hq`；
- 设计和发布文档位于 `docs`，可复制示例位于 `examples`，发布门禁仍统一从 `scripts` 进入；
- 公司实例中的 `ceo-office/tools/hq/bin/hq`、`config.yaml`、records、socket 和 workspace 合同没有变化；
- 这是源码仓库布局变更，不要求迁移 registry、event ledger 或公司工位。

本次正式发布验收的是 HQ 控制面与 Herdr 执行面的组织协议，不把外部 Chrome connector 的逐会话
saved-permission 状态或 Herdr 尚未提供的原子 conditional close 伪装成 HQ 能力。真实 R4 中这些路径均
fail closed，并由公司所有者明确从 HQ 发布门禁中豁免；对应业务 case 与证据仍保留原阻断状态。

## 发布原则

- 只从已核对、工作树干净的 commit 构建；
- build 不嵌入 wall clock，确保相同源码和工具链可复现；
- 发布前必须通过 full test、race、vet、gofmt、README 首验和跨平台 checksum；
- 发布前必须通过动态公开 CLI 矩阵、纯参数前置校验和无文档 Agent 首次使用审计；错误必须包含命令、用法、
  精确 help 下一步及已记账恢复约束；
- 公司实例必须由同一制品中的 `hq init` 创建，不接收开发期资料目录；
- 任何失败都保留构建输出、checksum、退出码和审计记录，不删除权威证据。

## 可复现构建

在 HQ 独立源码仓库根执行：

```bash
./scripts/release.sh build v1.2.1 <完整小写commit> /tmp/hq-v1.2.1-release
./scripts/release.sh verify /tmp/hq-v1.2.1-release
./scripts/test-gates.sh
```

构建固定使用 `-trimpath`、`CGO_ENABLED=0` 和 linker flags 注入 version/commit，覆盖：

- `darwin/arm64`
- `linux/amd64`
- `linux/arm64`

输出包含三份 binary、`BUILD-MANIFEST.txt` 和 `SHA256SUMS`。正式 build 会拒绝 commit 不匹配或
HQ 源码不干净。`HQ_RELEASE_REHEARSAL=1` 只用于门禁中的可复现构建演练，不得将演练制品安装到公司。

## 新公司安装

1. 在源码目录构建并验证发布包；
2. 使用该制品的 `hq init <company-directory>` 交互向导或 `--silent` 模式创建空白公司；
3. init 生成 `AGENT-HANDBOOK.md`、每个 seat 的独立版本化 Role Card/Workstation、
   registry v3、成立决策，并安装当前 binary 到 `ceo-office/tools/hq/bin/hq`（0755）；
4. 推荐虚拟总部使用 `--template virtual-company`；离线验证使用 `--prepare-only`；
5. 检查每个 employee seat 的 `role_card_id@version`、唯一 `workstation_path`、
   `activation_policy=always|on_assignment|manual`、仅 `on_assignment` 可用的有界 `keep_warm`、
   `max_wip` 与 seat digest；需要永久常驻时必须使用 `always`；
6. 从实例 binary 运行 `version --json` 和只读 `doctor`；
7. 默认让 init 完成“常驻秘书 → gateway → 其余 always 管理层”的首次启动；
   `on_assignment` 专业 seat 在正式 issue 前保持休眠；
8. 创建业务时，第一个 case 必须显式带非空 `--project` 并作为唯一 root；所有后续 case 必须带
   `--parent` 且省略 `--project`。第二 root/project、project 改名或 root 关闭后继续写业务树都应被拒绝；
9. 用 root 下的一个非关键 child 完成 issue → accept → report → accept/close 的首条真实 v3 链路；
10. 对 `on_assignment` canary 核验 `runtime status`、有界 keep-warm 后的自动休眠、同 seat
   cold-resume 以及第二个完整周期；`always` 管理层必须全程在线。
11. 如配置 `runtime_fallback`，用合成 terminal evidence 验证 primary 关闭、fallback 启动、
    `fallback_recovery_sent` 以及原 assignment/case 不变；CloseTab ambiguous 必须 fail closed 且不得出现双 runtime。
12. 模拟 issue transport sent 但员工未 accept：watchdog 只能在同一安全 idle/done seat 上复用原 payload 有界重投；
    已记账 runtime 消失时必须先收敛旧 session、cold-resume 同一冻结 seat 再重投；模拟 CLI 自动更新后退出时，
    startup 不得误判成功或遗留 HQ-owned orphan tab。ambiguous 必须冻结为 activation unknown，trust/safeguard
    页面不得收到 Prompt，accept 后必须停止重投。
13. 模拟下属 report 已 submitted、经理回合已结束：manager queue watchdog 必须在超时后发送带精确
    accept/return 命令的 durable nudge，次数耗尽后沿 reports_to 升级；重复扫描不得重复 Prompt，ambiguous
    必须冻结并要求 reconcile，且整个过程不得替经理验收或改变业务状态；混合队列必须保持
    `review > work > owned_case`，旧 open case 不得遮蔽待审或返工。
    经理 issue 最后一个 child 时必须得到结构化 `end_turn`，仍有其他 durable 经理事项时必须得到
    `continue_queue`；只有 active child 却持续 working 超时必须由 patrol 报告 `manager_busy_without_action`，
    且不得自动 interrupt。
14. 模拟 assignment 全部 completed、accepted case 长期未 close：closure queue watchdog 必须只选择无活动合同、
    无未决 workflow delivery、child 已 closed 的 `accepted|finding_accepted` 后序候选，向唯一 account_closer
    发送有界 durable nudge；`open|blocked|needs_decision` 必须排除，patrol 必须报告
    `idle_with_closure_backlog`，且守卫不得自动 close、生成 reason/source 或改变业务状态。
15. `staff list/get` 必须分别显示 ledger 实时 `active_wip`、registry 上限 `max_wip` 与实时
    `available_wip`；禁止再用无定义的 `WIP` 列让 Agent 把容量上限误当当前占用。
16. 模拟员工 accept 后在 report 前遇到 gateway 重启或 runtime 消失：idle/done seat 必须收到绑定原
    assignment/history/report 动作的 durable progress nudge，离线 seat 必须以同一工位和 recovery envelope
    cold-resume；催办有界并沿 reports_to 升级，且不得代写 report、改变业务状态或开放人工 nudge 绕过 issue。
17. 如配置 `runtime_profiles.codex`，验证新 thread 原生 argv 显式包含期望 model/effort；模拟在线
    thread 变为其他 model/effort，patrol 必须报 `runtime_profile_mismatch`，watcher 不中断
    working/blocked，只在 idle/done 关闭旧 tab、用同一 seat/workstation 启动并投递 durable recovery。
    ambiguous CloseTab 必须进入 `profile_repair_unknown`、不得启动第二个 runtime，且报错必须给出单 seat
    `runtime repair-profile --retry-unknown` 纠正命令。恢复信封必须排除 `accepted|finding_accepted|blocked|needs_decision`
    等非 actionable owned case，只包含可继续的 assignment/owned open case，并把展开项限制为 8；溢出要求查询 ledger。
18. 模拟 Codex safety-buffering 完整选择器遮挡 footer：HQ 必须保持原 model/effort、零 send-keys、零 Prompt、
    零 profile drift/restart；选择器消失后恢复 footer 核验。模拟 gateway 的获授权 maintenance seat 更换
    pane：watchdog 必须解析同一 seat 的新 binding 后继续 nudge；无法解析时必须清空 stale pane 并停止投递。
19. 模拟 active assignment 执行中需求变化：urgent directive 对 working 目标必须固定 next-turn、绕过普通 wake
    budget、要求 ack；超时后有界催办并沿 reports_to 升级。合同变更必须以 `case revise --supersede-active`
    原子产生 superseded → revision_pending/vN+1 → replacement，同一直属 assignee 不变；failed-pre-send 只能
    retry 原 delivery，旧 assignment 永不恢复。最后验证总部联络职责位到经理、经理到员工的两跳传播。

公司实例不依赖源码目录或全局 `hq`，但运行环境必须单独提供 `herdr` 与 registry 所选
Agent kind 的 CLI。首条业务写入前应保留 registry、Role Card 手册、成立决策和发布 checksum 的审计记录。

## 首次启动失败处置

- 在任何业务事件写入前失败且已有 `records/init/intent.json`：保留失败证据，恢复 intent 所冻结的配置与
  成立决策，然后对同一 company-directory 重跑 `hq init`；HQ 只续跑可证明属于本次 init 的缺失步骤；
  已消失的 init agent incarnation 会先记 `stopped` 并回收其精确 stale tab；
- 尚未写 init intent 就失败：修复静态输入后，可在同一目录用相同成立参数重跑 `hq init`；
- 已有业务事件后失败：停止 gateway/writer，保留完整实例，使用同一 v3 协议的当前工具执行
  `doctor`、投递核对和 forward recovery；
- 任何路径都不得就地截断、删除、重签或手工改写 event v3 链。

runtime 关闭是独立运维状态，不得回滚已成立的业务验收。`hibernate_failed` 只能对单一
agent 显式 `runtime reap --retry-failed`；`hibernate_attempting|hibernate_unknown` 必须先核对同一 runtime
incarnation，再对单一 agent `--retry-unknown`。不得批量重试或以裸 Herdr Prompt/显式
`hq up <on_assignment-seat>` 绕过恢复栅栏。

整家公司关闭后的恢复使用宿主机无参数 `hq up`，不得重跑 `hq init`。该路径会先依据 Herdr snapshot 为所有
已消失但仍记作 active 的旧 session 追加 `stopped`，再启动新的 always-role incarnation；发布验证必须确认
旧 session 全部终止、新 session 使用新的 runtime identity，且业务 ledger 不因冷启动发生变化。

## 发布完成条件

正式发布只有同时满足以下条件才算完成：

- release manifest 与三平台 checksum 验证通过；
- 当前平台 binary 报告正确 version、commit、Go 和 platform；
- 新公司 registry 为严格 v3，新 ledger 只包含 event v3；
- 业务 ledger 为空或严格只有一个非空 Project、一个 root 和一棵完整 parent tree；
- 每名 employee 都有不可变 Role Card、独立 Workstation、activation policy、规范化 keep warm、
  max WIP 和有效 seat digest；
- `doctor` 没有硬失败，gateway 协议版本、workspace 和 server identity 匹配；
- 至少完成一个不改变业务状态的健康验证，并在需要时完成受控 canary case；
- canary 证明 `on_assignment` 可 reap → 同 seat cold-resume → 再次 reap，同时 `always` 席位不被自动关闭；
- `runtime status/reap` 的 failed/unknown 诊断、单 seat 恢复和 gateway 重启补偿已通过测试。
- `permission_mode=yolo` 在显式 effort/自定义 argv 存在时仍补齐必需授权参数；Codex 包含
  approvals/sandbox 与 hook-trust 两个 bypass。
- 启用 runtime fallback 时，精确 provider safeguard 证据可从 primary 收敛到 fallback，同一 seat/
  workstation/assignment/case 保持连续，且旧 stop、新 start 与 recovery delivery 都有 session 审计事实。
- 启用 runtime profile 时，显式 model/effort argv、terminal footer 反查、patrol drift、idle/done 安全恢复、
  working/blocked 延后、unknown 单 seat 人工栅栏、actionable-only 有界恢复清单和 `profile_recovery_sent` 审计均通过。
- Codex safety-buffering 选择器只被解释为“保留原模型继续等待”：HQ 零按键、零降级、零误报、零中断；
  gateway maintenance seat 换 pane 后 watcher 能自动刷新 binding，过期 pane 永不继续获得内部投递权限。
- issue `sent` 与 assignment activation 必须分层可见；同 ID/payload 重投、unknown resolve、exhausted retry、
  ledger 防伪和 accept 后停止均通过测试。
- 空闲经理的 durable actionable queue 必须由 patrol 标记 stalled，并由 gateway 有界提醒、向上升级；同一
  queue basis 跨重启去重，系统不得自动 accept/return 或伪造质量结论。
- 经理 issue 后必须得到与最新 durable 队列一致的 `continue_queue|end_turn`；事件等待期间不得要求模型轮询，
  持续 working 的无动作监督者由 patrol v3 报告但不自动中断。
- 已验收且满足 post-order 前置的 closure queue 必须由 patrol 标记 stalled，并由 gateway 有界提醒唯一
  account_closer；单轮明确逐项处理至多 8 个候选，同一 status event basis 跨重启去重，系统不得自动 close 或把
  blocked/needs_decision 当作完成。
- `on_assignment` seat 在原 assignment 仍未完成时，可由精确绑定的 report return 或合同 issuer 的 actionable
  message 从休眠中恢复；无关 case、非合同 actor、info message 不得取得启动权；prepared delivery reconcile
  必须复用原 ID 且至多 Prompt 一次。
- urgent directive 必须绑定 active assignment、使用下一安全回合且不得被 wake budget 静默降级；未 ack 由 gateway
  有界提醒并向直属上级升级。重大合同变更只能原子 supersede/revise/reissue，旧合同不可回退，组织传播不可越级。
- 全部公开命令路径和可见参数都有本地帮助；未知命令/flag、缺失/条件必填参数和业务错误均满足 Agent
  自解释错误合同，纯参数错误不被 office/gateway/Herdr 依赖发现遮蔽。
- `approval show`、`delivery status` 与 `nudge status` 必须在无 Herdr 身份的宿主机保持纯只读可用；同命令族的 mutation 仍经
  gateway，未知子命令必须在本地返回精确用法而不是误报身份缺失。
