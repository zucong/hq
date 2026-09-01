# HQ v1.0.0 正式发布与安装

HQ v1.0.0 是首个正式制品。本文定义它的构建、验证和新公司安装流程。该制品只配套
当前 registry v3、event v3、不可变 Role Card、独立 Employee Workstation 和 Employee Seat 合同。
开发期间的其他命令、配置或账本格式不是发布输入。

本次正式发布验收的是 HQ 控制面与 Herdr 执行面的组织协议，不把外部 Chrome connector 的逐会话
saved-permission 状态或 Herdr 尚未提供的原子 conditional close 伪装成 HQ 能力。真实 R4 中这些路径均
fail closed，并由公司所有者明确从 v1.0.0 的 HQ 发布门禁中豁免；对应业务 case 与证据仍保留原阻断状态。

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
./scripts/release.sh build v1.0.0 <完整小写commit> /tmp/hq-v1.0.0-release
./scripts/release.sh verify /tmp/hq-v1.0.0-release
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

公司实例不依赖源码目录或全局 `hq`，但运行环境必须单独提供 `herdr` 与 registry 所选
Agent kind 的 CLI。首条业务写入前应保留 registry、Role Card 手册、成立决策和发布 checksum 的审计记录。

## 首次启动失败处置

- 在任何业务事件写入前失败：保留失败证据，修复当前代码或模板后，在新空目录重新 `hq init`；
- 已有业务事件后失败：停止 gateway/writer，保留完整实例，使用同一 v3 协议的当前工具执行
  `doctor`、投递核对和 forward recovery；
- 任何路径都不得就地截断、删除、重签或手工改写 event v3 链。

runtime 关闭是独立运维状态，不得回滚已成立的业务验收。`hibernate_failed` 只能对单一
agent 显式 `runtime reap --retry-failed`；`hibernate_attempting|hibernate_unknown` 必须先核对同一 runtime
incarnation，再对单一 agent `--retry-unknown`。不得批量重试或以裸 Herdr Prompt/显式
`hq up <on_assignment-seat>` 绕过恢复栅栏。

## 发布完成条件

首次发布只有同时满足以下条件才算完成：

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
- `on_assignment` seat 在原 assignment 仍未完成时，可由精确绑定的 report return 或合同 issuer 的 actionable
  message 从休眠中恢复；无关 case、非合同 actor、info message 不得取得启动权；prepared delivery reconcile
  必须复用原 ID 且至多 Prompt 一次。
- 全部公开命令路径和可见参数都有本地帮助；未知命令/flag、缺失/条件必填参数和业务错误均满足 Agent
  自解释错误合同，纯参数错误不被 office/gateway/Herdr 依赖发现遮蔽。
