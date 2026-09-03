# HQ

HQ 是面向 Herdr 虚拟公司的总部控制面。它把公司启动、人员注册、汇报线、事项流转、
可靠投递、审计恢复和运行巡视收拢到一个 Go CLI 与本地网关中，让一组长期运行的 agent
能够像一家公司一样分工、协作和对结果负责。

**产品状态：v1.0.0 已正式发布。** 当前正式合同只有 registry v3 和
event v3。不存在可依赖的旧版命令、配置或事件协议。投入真实公司前应完成初始化、
`doctor`、隔离 workspace 验证和受控 canary。

HQ 不取代确有独立阅读价值的长篇规格、报告、方案、正式决策和岗位手册。文件仍是这些内容的
权威原文，HQ 记录身份、短规格、引用、授权、状态和投递事实。

## 产品模型

HQ 与 Herdr 的职责边界如下：

```text
Herdr：workspace / tab / pane / agent / prompt
   │
   └──提供可核验的运行身份和投递能力
          │
HQ：registry / gateway / workflow / ledger / outbox / operations
          │
          └──引用 Markdown、Git commit 等权威原文
```

- **Herdr 是执行面**：创建 workspace 和 agent 会话，提供实时 snapshot，并在回合边界投递 Prompt。
- **HQ 是控制面**：核验谁在什么工位，以组织权限处理业务命令，并把结果写入可恢复的事件账本。
- **公司文件是内容面**：岗位手册、长篇规格、报告、finding 和正式决策保留完整上下文；HQ 不复制长文。

核心原则：

- 配置决定组织，不在程序中写死员工姓名、部门路由或启动清单；
- Prompt 是门铃，不是账本；先持久化投递意图，再调用 Herdr；
- 写操作在网关端重新核验实时身份，不能只相信环境变量自报；
- 事件账本是事实源，state 和 SQLite 都是可删除、可重建的投影；
- 不确定投递不盲重试，权限或协议证据不足时 fail-closed。

### 文件克制原则

HQ 的一个直接目标是避免“每做一步就新增一份 Markdown”的文书膨胀：

- case 创建、委派、接收、回报、退回、消息、提醒、投递和销账只写结构化账本，不生成过程性 MD；
- 短目标、验收、约束、下一步和沟通正文直接进入受限字段；
- 只有需要独立阅读、长期引用的长内容才写文件，再通过 `--source`、`--ref-file`、`--artifact` 等引用；
- 初始化只生成固定制度底座，不在日常运行中自动制造传令存根、接单记录或销账报告。

## 源码与公司实例

本独立仓库的产品源码位于仓库根。HQ 部署后服务于一个具体的公司根，当前运行时布局为：

```text
company-root/
├── AGENT-HANDBOOK.md
├── COMPANY.md
├── ceo-office/
│   ├── decisions/
│   ├── records/
│   └── tools/
│       ├── hq/
│       │   ├── bin/hq
│       │   └── config.yaml
│       └── index.db
├── delivery/
├── engineering/
└── ...
```

源码仓库和公司实例目录是两个概念：当前仓库根用于开发、构建和发布；
`<company-root>/ceo-office` 是 `--office` 选择的运行实例。每个实例拥有独立的 registry、records、
socket 和 Herdr `workspace_label`。

init 会把当前 HQ executable 复制进公司目录，因此实例运行 HQ 本身不依赖源码目录或全局 `PATH`
里的另一个 `hq`。完整启动仍依赖可执行的 `herdr`，以及 registry 所选 Agent kind 对应的 CLI；
这些外部执行引擎不被打包进 HQ binary。

`--office` 是公司实例选择器。正式运行时只允许它选择一个 canonical 公司实例；`--config`、`--data`、`--herdr`
不能覆盖实例内部的固定信任根。测试通过显式依赖注入隔离，不借命令行参数伪装正式实例。

## 构建与安全首验

下面的首验从当前独立仓库发现源码根，把 Go cache、binary、初始化骨架和 records 全部放在
`${TMPDIR:-/tmp}` 下。它不会连接 Herdr，也不会读取或写入任何已有公司实例：

```bash
HQ_SOURCE_DIR="$(git rev-parse --show-toplevel 2>/dev/null || pwd -P)"
test -f "$HQ_SOURCE_DIR/README.md" && test -f "$HQ_SOURCE_DIR/go.mod"
cd "$HQ_SOURCE_DIR"
if command -v hq >/dev/null 2>&1; then
  echo '建议在未安装全局 hq 的干净 shell 中首验' >&2
fi
HQ_TMP_BASE="${TMPDIR:-/tmp}"
HQ_TMP_BASE="$(cd "$HQ_TMP_BASE" && pwd -P)"
HQ_SMOKE_ROOT="$(mktemp -d "$HQ_TMP_BASE/hq-first-run.XXXXXX")"
cleanup_hq_smoke() {
  if [ -n "${HQ_SMOKE_ROOT:-}" ]; then
    rm -rf -- "$HQ_SMOKE_ROOT"
  fi
}
trap cleanup_hq_smoke EXIT HUP INT TERM
mkdir -p "$HQ_SMOKE_ROOT/hq" "$HQ_SMOKE_ROOT/tmp" "$HQ_SMOKE_ROOT/go-cache"
cp ./*.go ./go.mod ./go.sum "$HQ_SMOKE_ROOT/hq/"
cd "$HQ_SMOKE_ROOT/hq"
TMPDIR="$HQ_SMOKE_ROOT/tmp" GOCACHE="$HQ_SMOKE_ROOT/go-cache" \
  go build -trimpath -o ./bin/hq .
./bin/hq help
./bin/hq init "$HQ_SMOKE_ROOT/company" --silent \
  --company-name "Smoke Company" --owner ZC \
  --workspace smoke-company-hq --template minimal --prepare-only
./bin/hq --office "$HQ_SMOKE_ROOT/company/ceo-office" staff list
./bin/hq --office "$HQ_SMOKE_ROOT/company/ceo-office" board --cases-only
```

需要验证完整 `up` 编排时，使用仓库自带的合成集成测试。该测试注入 fake identity、fake
transport、fake Herdr 和临时 Store，不连接实际 workspace：

```bash
HQ_SOURCE_DIR="$(git rev-parse --show-toplevel 2>/dev/null || pwd -P)"
test -f "$HQ_SOURCE_DIR/README.md" && test -f "$HQ_SOURCE_DIR/go.mod"
cd "$HQ_SOURCE_DIR"
HQ_TEST_BASE="${TMPDIR:-/tmp}"
HQ_TEST_BASE="$(cd "$HQ_TEST_BASE" && pwd -P)"
HQ_TEST_ROOT="$(mktemp -d "$HQ_TEST_BASE/hq-up-test.XXXXXX")"
cleanup_hq_up_test() {
  if [ -n "${HQ_TEST_ROOT:-}" ]; then
    rm -rf -- "$HQ_TEST_ROOT"
  fi
}
trap cleanup_hq_up_test EXIT HUP INT TERM
mkdir -p "$HQ_TEST_ROOT/tmp" "$HQ_TEST_ROOT/go-cache"
TMPDIR="$HQ_TEST_ROOT/tmp" GOCACHE="$HQ_TEST_ROOT/go-cache" \
  go test -count=1 -run '^TestRegistryPortabilityInitUpAndEnvelope$' -v ./...
```

发布构建与安装见 [RELEASE.md](RELEASE.md)。

## 初始化公司

`hq init <company-directory>` 是唯一初始化入口。它不连接 LLM API；交互向导只读取结构化字段，
可以从内置模板生成通用公司，也可以用一份已批准的 `--organization-spec` 从第一性原理编译专属公司。
默认生成后通过 Herdr 首次启动，`--prepare-only` 用于离线准备。prepare-only 目录仍未完成 init，
稍后只传同一个 company-directory 再运行 `init` 即可继续；不要用 `up` 代替首次初始化：

```bash
HQ_COMPANY_ROOT=/path/to/company
./bin/hq init "$HQ_COMPANY_ROOT"

# CI、脚本或离线准备
./bin/hq init "$HQ_COMPANY_ROOT" --silent \
  --company-name "Acme" --owner ZC --workspace acme-hq \
  --template product-engineering --secretary-kind claude \
  --default-agent-kind codex --permission-mode native --prepare-only

# 专属组织：不先套模板，也不保留模板角色兼容层
./bin/hq init "$HQ_COMPANY_ROOT" --silent \
  --company-name "Domain Company" --owner OWNER --workspace domain-company-hq \
  --organization-spec ./approved-company-formation.yaml \
  --default-agent-kind codex --permission-mode native --prepare-only

# 推荐的虚拟公司总部
./bin/hq init "$HQ_COMPANY_ROOT" --silent \
  --company-name "Virtual Company" --owner ZC --workspace virtual-hq \
  --template virtual-company \
  --secretary-name owner-channel --secretary-nickname "总部联络官" \
  --prepare-only

# 需要无人值守访问 HQ/Herdr socket 的 Codex 公司；每个 argv 单独重复
./bin/hq init "$HQ_COMPANY_ROOT" --silent \
  --company-name "Virtual Company" --owner ZC --workspace virtual-hq \
  --template virtual-company --permission-mode native \
  --secretary-agent-arg=-c --secretary-agent-arg='model_reasoning_effort="medium"' \
  --secretary-agent-arg=--sandbox --secretary-agent-arg=danger-full-access \
  --secretary-agent-arg=--ask-for-approval --secretary-agent-arg=never \
  --default-agent-arg=-c --default-agent-arg='model_reasoning_effort="medium"' \
  --default-agent-arg=--sandbox --default-agent-arg=danger-full-access \
  --default-agent-arg=--ask-for-approval --default-agent-arg=never --prepare-only

# 对上述任一 prepare-only 公司完成首次初始化
"$HQ_COMPANY_ROOT/ceo-office/tools/hq/bin/hq" init "$HQ_COMPANY_ROOT"
```

模板为 `minimal`、`product-engineering`、`saas`、`professional-services`、`commerce` 和
`virtual-company`。后者包含一名总裁秘书、产品/工程/质量三名经理，以及产品调研、文案术语、数据工程、
应用开发、安全、代码审查、浏览器黑盒、首次使用、可用性走查和数据核验门禁十个专业 seat。
管理层使用 `activation_policy=always`；专业下属使用 `on_assignment`、`max_wip=1`
和默认 `keep_warm=30s`，由正式 issue 激活，工作终态后在无待处理工作时自动休眠。

自定义 organization spec 使用严格单文档 YAML，`version: 1`。它显式列出部门和 seat；每个 seat
同时冻结 nickname、department、reports_to、唯一 responsibilities、activation/keep_warm/max_wip、
`owner_channel|default` runtime profile、权限，以及结构化 Role Card 的 capabilities、mission、
temperament、behavior anchor、duties、method、evidence 和 boundaries。HQ 在写目标目录前检查未知字段、
重复项、汇报环、唯一职责位、权限组合、工位路径和全部 digest。规范原文会复制到
`ceo-office/formation/organization-spec.yaml` 并绑定公司成立决策，作为不可变成立证据；运行时不读取它，
`ceo-office/tools/hq/config.yaml` 仍是唯一实时组织注册表，因此该文件不是第二份 roster。
完整的通用 schema 示例见 [`organization-spec.example.yaml`](organization-spec.example.yaml)；它只演示格式和最小治理不变量，不代表任何具体公司的组织模板。

每个模板都包含真实汇报线、根目录 `AGENT-HANDBOOK.md`，以及每个 seat 独立的
`<department>/staff/<seat>/v<role-version>/AGENTS.md`。`ceo-office/tools/hq/config.yaml`
是唯一组织编制与 employee seat 注册表，不生成平行的 Markdown 编制清单。
init 根据实际手册 bytes 生成 manual digest、role card
digest 和 employee seat digest，再写入单一当前 registry schema。init 还会生成结构化公司成立决策，
并把执行 init 的当前 HQ executable 复制为 `ceo-office/tools/hq/bin/hq`（0755）。文件使用非覆盖创建语义；同参数
重复运行会续跑或 no-op，不同参数不会覆盖既有公司。

`--silent` 绝不读取 stdin，且要求显式给出 `--company-name`、`--owner`、`--workspace`，并在
`--template` 与 `--organization-spec` 中恰好选择一个。两者互斥，HQ 不会先生成模板再覆盖为专属组织。
`--permission-mode yolo` 使用 HQ 针对 Agent kind 的自动批准参数；`native` 不追加
批准参数。可选的 `--secretary-name` 设置总裁秘书稳定 slug 的基础名称，`--secretary-nickname` 设置显示名称；
两者都不承载权限，权限只来自该 employee seat 的唯一 `approval_witness` 职责和显式 `can_*` flags。
可重复的 `--secretary-agent-arg` 和 `--default-agent-arg` 会分别把秘书/其他岗位的
原生 argv 写入 registry，并进入 employee seat digest；非空 argv 按既有协议完整覆盖 kind 默认参数。
完整逐步说明和示例见 `hq init --help`。

总裁秘书是唯一 `approval_witness` 职责位，也是人类公司所有者与虚拟公司总部之间的双向沟通管道；
具体 agent 名字、花名和 sender label 可配置，HQ 不依赖某个专名。总裁秘书只上传人类所有者已经明确
作出的决定，据此下达公司级事项，并把部门证据、风险与待决问题汇总给人类；不得代替人类决定组织变更、
产品方向、优先级或风险接受。公司 up 后，只有具备 `can_manage_staff` 的总裁秘书可以执行人类已经正式
批准的人事决策以增员、停用成员或调整组织；部门经理不能自行招聘。

## 人员注册表

`ceo-office/tools/hq/config.yaml` 是公司实例的运行时组织事实源。稳定 agent slug 是历史主键；
公司所有者 principal、花名、发件标识、部门、角色卡、独立工位、激活策略、WIP、kind、直属上级和权限都属于配置数据。

绑定到正式公司实例的 Store 会在每个账本根操作前取得 registry 共享租约、重新严格加载磁盘配置，并在
持有该租约期间完成 ledger lock、恢复、重放或提交。registry 子系统锁序为 `registry process → config directory
→ ledger`；staff mutation 使用排他租约。调用者若仍持有旧 Config，会在 builder、txn recovery 和任何写入前
收到 conflict，不能用旧权限继续追加。candidate replay 是唯一的内部豁免：它由已经持有排他 registry 租约的
staff mutation 调用，用候选配置核验 ledger。

当前 schema 的核心关系如下；digest 均由 HQ 根据完整冻结字段和实际 `AGENTS.md` bytes 计算，
不应手填猜测：

```text
Config v3
├── runtime_fallback                  # optional process-carrier policy
│   ├── auto / trigger: content_safeguard
│   ├── from_kind / to_kind
│   └── permission_mode / agent_args
├── role_cards[]
│   ├── role_card_id / version / status / department / capabilities
│   ├── manual_path / manual_digest
│   └── role_card_digest / approval_ref
└── agents[]
    ├── stable name / reports_to / permissions
    ├── role_card_id / role_card_version / role_card_digest
    ├── workstation_path / manual_path
    ├── activation_policy: always|on_assignment|manual
    ├── keep_warm: bounded duration (on_assignment only)
    ├── max_wip
    └── seat_version / seat_digest
```

下面是可被当前严格解码器直接接受的最小 registry v3 示例。digest 为示例冻结字段的确定性结果；
实际公司应由 `hq init`、`hq role add` 和 `hq staff add/update` 计算，不要手工伪造：

```yaml
version: 3
workspace_label: example-hq
owner_principal: ZC
runtime_fallback:
  auto: true
  trigger: content_safeguard
  from_kind: codex
  to_kind: grok
  permission_mode: yolo
  agent_args: [--always-approve]
role_cards:
  - role_card_id: secretary
    version: 1
    label: 总裁秘书
    department: ceo-office
    capabilities: [account_closure, approval_witness, organization_operations]
    manual_path: ceo-office/staff/secretary/v1/AGENTS.md
    manual_digest: a87a0a3d265dff1537a0d2c12b04b635a46d0113f38300b3a69ea3914b1733ee
    role_card_digest: b5f80c11c210f72ad7a9949e47876b18613276309c37bd1ca52f0a81a6b4de49
    status: approved
    approval_ref: ceo-office/decisions/company-init.md
agents:
  - name: example-secretary
    sender_label: 总裁办-总裁秘书
    nickname: 总裁秘书
    department_label: 总裁办
    workspace: example-hq
    responsibilities: [approval_witness, account_closer, executive_secretary]
    manual_path: ceo-office/staff/secretary/v1/AGENTS.md
    role_card_id: secretary
    role_card_version: 1
    role_card_digest: b5f80c11c210f72ad7a9949e47876b18613276309c37bd1ca52f0a81a6b4de49
    workstation_path: ceo-office/staff/secretary/v1
    activation_policy: always
    max_wip: 16
    seat_version: 1
    seat_digest: 7e0c16d566a031ce9539e3713dc6ab7def4693ea66e5c0fd2d78da589e16fd56
    department: ceo-office
    kind: codex
    permission_mode: native
    reports_to: ""
    disabled: false
    can_create: true
    can_issue: true
    can_accept: true
    can_close: true
    can_manage_staff: true
    can_receive_order: true
delivery_policy:
  default_mode: auto
  max_consecutive_wakes: 3
  max_bundle_items: 8
  max_bundle_bytes: 16384
  assignment_accept_timeout: 2m
  max_activation_redeliveries: 2
```

主要约束：

- `version` 当前为 `3`；`owner_principal` 必须显式配置为公司所有者的稳定单行标识；
- `workspace_label` 和 agent `name` 是最长 32 字符的小写 ASCII slug；
- 每名在职 agent 都绑定一张 approved role card、唯一独立工位和 `workstation_path/AGENTS.md`；
- 每张角色手册最多 1 MiB，并从拒绝 symlink/替换竞态的已验证文件描述符读取；
- 每张 role card（包括尚未绑定 seat 的 card）都独占一份个人 `AGENTS.md`，不得复用、嵌套或指向部门根；
- 手册 bytes、role card 冻结字段和 employee seat 冻结字段必须分别匹配三层 digest；
- role card 的 capabilities 只描述角色行为；权限只由 employee seat 上的 `can_*` flags 授予；
- `activation_policy` 只允许 `always|on_assignment|manual`；`always` 由 `hq up` 启动且永不自动休眠，
  `on_assignment` seat 只由正式委派按需唤醒，`manual` 不参与自动启停；
- `keep_warm` 仅用于 `on_assignment`，必须是 `0s..1h` 的有界 duration；省略时等价于
  `30s`，需要永久常驻应改用 `activation_policy=always`；
- `max_wip` 为 1..16，新的专业执行 seat 推荐为 1；
- `reports_to` 必须指向在职、已登记的合法上级，不得自指或成环；
- 每名经理持有 `manager:<department>`，且 department 与自己的部门精确一致；
- `approval_witness` 和 `account_closer` 各自只有一名在职持有人；
- 至少一名在职 agent 具有 `can_manage_staff=true`；
- 每名在职 seat 必须声明合法 Herdr `kind`，才能常驻或按需启动。
- `permission_mode` 为 `yolo|native`；`native` 只传递显式 `agent_args`；`yolo` 会在显式参数后补齐该 kind 的必需自动授权 argv，不允许自定义参数意外降权。
- 可选 `runtime_fallback` 只替换稳定 seat 的模型运行载体，不修改 employee seat、Role Card、workstation 或 Assignment Contract；当 `auto=true` 时，HQ 只对可核验的 `content_safeguard` 终端状态触发一次保守切换。

常用维护命令：

```bash
./bin/hq role list
./bin/hq role show --role <role-id>@<version>
./bin/hq role add --id <role-id> --version <version> --label <角色名> \
  --department <部门> --capability <行为标签> \
  --manual <department>/staff/<seat>/v<role-version>/AGENTS.md \
  --approval ceo-office/decisions/<生效决策>.md
./bin/hq role retire --role <role-id>@<version> \
  --approval ceo-office/decisions/<生效决策>.md
./bin/hq staff list [--reports-to <manager-slug>] [--all]
./bin/hq staff get --name <slug>
./bin/hq staff add --name <slug> --label <发件标识> --department <目录> \
  --kind <kind> --reports-to <slug> --role <role-id>@<version> \
  --workstation <department>/staff/<seat>/v<role-version> \
  --activation on_assignment --keep-warm 30s --max-wip 1 --grant create,accept \
  --approval ceo-office/decisions/<生效决策>.md
./bin/hq staff update --name <slug> --role <role-id>@<version> \
  --workstation <department>/staff/<seat>/v<role-version> \
  --activation always --max-wip 4 \
  --approval ceo-office/decisions/<生效决策>.md
./bin/hq staff remove --name <slug> \
  --approval ceo-office/decisions/<生效决策>.md
```

role card 一旦创建就不得原地修改；新定义使用新 version。仍被任何 employee seat（包括 disabled seat）
绑定的 card 不得 retire。`remove` 只停用身份，不删除审计主键；稳定 slug 需要改变时，使用
“新增 → 更新引用 → 停用原 slug”。

## 首次连接 Herdr

首次连接只能由 `hq init` 完成。init 在任何 Herdr mutation 前核验：成立决策只有一个精确的
`company:init` scope、业务账本与 session lifecycle 为空、同名 workspace 不存在。随后先落盘冻结的
`records/init/intent.json`，再按“总部联系职责位 → 回收 Herdr 自动创建的空 root tab → gateway → 其余 always 岗位”启动。中途失败时，
重跑同一个 `hq init <company-directory>` 只允许在配置、成立决策和已产生运行态均与 intent 一致时续跑；
若已记账的 init agent incarnation 已从 snapshot 消失，HQ 会先补写 `stopped` 并回收其精确 stale tab，
再创建新 incarnation，而不是复用旧 session 或留下重复工位。
成功后写入不可覆盖的 `records/init/completed.json`，并永久关闭首次初始化通道。

`hq up` 只服务已经完成 init 的公司：它创建或复用目标 workspace/tab，启动 registry 中
`activation_policy=always` 的 agent，并确保本地 gateway 在线。
显式 `hq up <on_assignment-seat>` 会被拒绝：这类 seat 只能在 durable `issue` 已建立后由 HQ 自动
cold-resume，防止先启动再用口头 prompt 绕过 Assignment Contract。

公司全部关停后，可从宿主机直接运行无参数的 `hq up`。该冷启动入口必须验证 init 完成记录，且只恢复
gateway 和 `always` 岗位；启动新 incarnation 前，它会用当前 Herdr snapshot 证明旧 runtime 已消失，逐一为
仍记作 active 的历史 session 补写 `stopped`，不会复用旧 session 或把旧、新工位混为同一次运行。它拒绝
单 agent 参数、`--no-gateway` 和 `--direct`。这解决了“联络官必须先在线才能把自己启动”的循环依赖，但不
授予业务写权限。公司正在运行时，从 Herdr 工位执行 `up` 仍要求调用者是 registry 中精确在岗、未停用且
`can_manage_staff=true` 的角色。

`hq up --no-gateway <agent>` 是严格的 agent-only 恢复入口，只复用已经存在且精确匹配的 workspace；
它不会创建 workspace 或 gateway，只能由上述实时在岗运维角色使用。delivery cold-resume 使用同一边界。

所有启动路径仍要求部门经理声明正确的 `manager:<department>`，tab label 精确使用 registry 的 runtime `sender_label`，
且 agent cwd、kind、workspace、tab、pane 和 `interactive_ready` 均能由 Herdr snapshot 证明。

```bash
HQ_COMPANY_ROOT=/path/to/company
"$HQ_COMPANY_ROOT/ceo-office/tools/hq/bin/hq" init "$HQ_COMPANY_ROOT" # 仅首次；prepare-only 后也用它
"$HQ_COMPANY_ROOT/ceo-office/tools/hq/bin/hq" \
  --office "$HQ_COMPANY_ROOT/ceo-office" up
"$HQ_COMPANY_ROOT/ceo-office/tools/hq/bin/hq" \
  --office "$HQ_COMPANY_ROOT/ceo-office" ping
```

HQ 创建 agent tab 时注入 `HQ_AGENT_NAME`、`HQ_DEPARTMENT` 和 `HQ_REPORTS_TO`；
`HERDR_PANE_ID`、`HERDR_WORKSPACE_ID` 由 Herdr 提供。启动成功只记录 session lifecycle，
不会自动创建 case 或 message。agent 到岗后从自己的独立 workstation 读取版本化角色卡；`on_assignment`
seat 平时保持休眠，收到第一条 durable `issue` 时才由 HQ cold-resume。

## 日常命令

```text
hq
├── init / doctor / version
├── up / patrol / session / runtime
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

### Agent 自解释错误合同

HQ 的公开 CLI 以 Agent 为主要操作者。Agent 不需要先读外部文档才能纠正一条失败命令：每个公开命令、
命令组和参数都可通过本地 `--help` 发现；每次失败保留原错误类别与退出码，并固定输出三部分：

1. 未完成的精确命令路径与原因；
2. 当前命令的 `用法`；
3. 可直接执行的 `<command> --help` 下一步，以及业务事件已记账时的恢复约束。

纯参数合同在发现 `ceo-office`、连接 gateway 或读取 Herdr 之前校验，包括枚举、互斥参数、唯一 root/child
的 `--project` 规则、`case revise` 禁止字段、Role Card capability，以及不同 `report --result` 的条件必填
证据。这样一条命令即使从错误目录运行，也先指出命令本身的问题，不会让缺失 office 掩盖参数错误。

若错误明确给出 `event_id` 或 `delivery_id`，不得把通用“重试同一命令”理解为重发业务：先运行
`hq delivery status --id DELIVERY_ID` 或 `hq flow show --case CASE_ID`，按错误中的恢复动作复用原 ID。
HQ 错误永远不会建议用裸 `herdr prompt` 绕过账本。

典型工作流：

```bash
./bin/hq whoami
./bin/hq case create --id CASE-ID --title <标题> --objective <目标> \
  --project <唯一项目名> --acceptance <验收> --constraints <约束> \
  --priority P1 --source <权威原文路径>

# 经理从 registry 列出自己的直属 seat；该命令不查询 Herdr runtime
./bin/hq staff list --reports-to <manager-agent-slug>

# 经理拆解父 case；新子 case 先由经理持有
./bin/hq case create --id CHILD-ID --parent CASE-ID --title <子事项> \
  --objective <目标> --acceptance <验收> --constraints <约束> --priority P1 \
  --source <权威原文路径>

# 经理向精确直属 seat 派活；issue 同时冻结 case、seat、role card 和 acceptor
# 部门内派工不申请 approval，也不传 --approval/--decision
./bin/hq issue --case CHILD-ID --to <direct-report> \
  --due <future-rfc3339> --next <下一步>

# 接收方核验并开始处理
./bin/hq inbox
./bin/hq accept --event <issue-event-id> --next <下一步>

# 有 assignment 时 report 返回冻结 acceptor；纯 owner 工作才回退到 reports_to
./bin/hq report --case CASE-ID --result completed \
  --artifact <产出路径> --next <下一步>

# 上级核验，或退回并写清复交条件
./bin/hq accept --event <report-event-id> --next <下一步>
./bin/hq return --event <event-id> --reason <原因> --next <复交条件>

# 已验收历史不可倒转。部门经理发现需要跨部门返工时，创建一个新的 durable
# escalation 子 case；目标不可指定，HQ 固定上交 registry 中的直属上级。
./bin/hq case escalate --id <new-rework-id> --parent <owned-parent-id> \
  --title <标题> --objective <返工目标> --acceptance <复验条件> \
  --constraints <边界> --priority P1 --source <缺陷依据> \
  --reason <为何必须跨部门返工> --next <上级接手后的下一步>

# 跨部门修复验收后，message 只能通知原部门经理，不能复活旧 assignment。
# 原 case 当前 owner 先建立新规格版本，再向直属复验 seat 发出 fresh issue。
./bin/hq case revise --id <quality-case-id> --version <N+1> \
  --title <复验标题> --objective <复验目标> --acceptance <复验条件> \
  --constraints <复验边界> --priority P1 --source <已验收修复证据>
./bin/hq issue --case <quality-case-id> --to <direct-reverify-seat> --next <复验下一步>

# 由 account closer 或其他 can_close 角色销账
./bin/hq close --case CASE-ID --reason <原因> --source <依据路径>
```

HQ 只有 `case` 一个工作概念。case 自带目标、验收、约束、优先级和版本 digest，也可通过
`parent_case_id/root_case_id` 形成父子层级。每个 HQ space 恰好承载一个 Project 执行总部：第一条业务 case
是唯一 root，必须显式提供非空 `--project`；之后所有 case 都必须提供 `--parent`。child 的
`project` 和 `root_case_id` 由 HQ 从 parent 自动继承，显式传 `--project` 会被拒绝。`case revise`
只能更新规格，不接受 `--project`；Project identity 在 root 创建时即冻结。经理把自己持有的
父 case 拆成多个 child 后分别委派；child 独立流转，父 case 不被子任务隐式改写。`issue`
是唯一纵向派活动词。严格回放同样拒绝空 project、第二个 root/project、断裂 lineage 或 project 改名；
不读取旧行为也不自动迁移。新项目必须创建新的 HQ 目录和 Herdr workspace。

经理派工授权矩阵：

| 发起方 → 目标 | 规则 |
|---|---|
| 总裁办 → 部门经理 | 使用 owner approval 或已生效 standing decision，再 `issue` |
| 部门经理 → 自己的精确直属 seat | 直接 `issue`，不申请也不附加 `--approval`/`--decision` |
| 部门经理 → 非直属 seat | 禁止；approval/decision 不能扩大管理边界，必须通过目标的直属经理安排 |
| 部门经理 → 自己的直属上级 | 仅用 `case escalate` 新建并上交 durable 子 case；不可自选 target，不可倒转旧 report |

因此，部门经理可以在已批准且有 WIP 容量的直属角色池中自由选择人员。`activation_policy=on_assignment`
的专业 seat 无需经理单独启动：正式 `issue` 建立 durable Assignment Contract 后，HQ 会通过 Herdr
自动 cold-resume 并递送门铃。WIP 在 durable issue intent/prepared 时即被预占，员工后续 `accept` 只确认接单，
不是容量开始计数的时点。裸 `herdr prompt` 既不能代替派工，也不应用作额外的激活步骤。HQ 拒绝
经理命令时，经理应停止重复该动作并执行报错中的纠正命令，不得继续申请 approval 或改走 Herdr prompt。
这里的禁止范围是业务派工与激活绕行。员工已经由 HQ 的原 assignment 激活后，人类公司所有者或其明确授权代理
可以通过 Herdr prompt 向当前会话补充精确外部工具权限；该提示不得携带新业务任务，也不创建或改变
case/issue/assignment、角色卡、digest、reviewer 或 acceptance。若 Chrome 等外部 connector 仍返回 saved deny，
授权文本不等于设置已生效：Agent 应 blocked，由有权方修改对应设置，必要时重连当前 runtime，再沿原 assignment
复验；禁止换 origin、surface 或 fallback 绕过。
assignment 被经理验收或进入终态后，`on_assignment` runtime 仅在保温时间结束且没有未决
assignment/case、正式投递、行动型消息确认或 reminder/nudge 时自动休眠。registry、角色卡、工位目录和
全部业务历史不变；下一条正式 issue 复用同一 seat cold-resume。若 HQ 报告旧关闭为
`hibernate_attempting`/`hibernate_unknown`，新 issue 会在写 origin 和预占 WIP 之前 fail closed；具有
`can_manage_staff` 的运维者必须先执行报错给出的 `runtime status/reap --retry-unknown`，不得绕过。
`hq staff list --reports-to <manager-agent-slug>` 只读精确筛选 registry 中的直属 seat，默认排除已停用 seat；
只在组织治理或审计时才同时传入 `--all`。

`issue_sent` 只证明 Herdr 已接受门铃注入，不证明员工看见任务或执行了 `accept`。gateway 的 assignment
activation watchdog 会在 `assignment_accept_timeout` 后核对原 assignment 仍为 `issued`、冻结 seat 精确在线、
runtime 为安全 `idle|done`，并且 Codex 终端确实处于正常输入页；满足条件时，它复用完全相同的 delivery ID、
assignment event 和原始 payload 有界重投，最多 `max_activation_redeliveries` 次。重复出现的同一
`[HQ notification]` 是激活重投，不是新任务；员工查询原 assignment 后只 accept 原 event。
任何 Herdr 歧义都会写成 `activation_unknown` 并停止自动重投；额度耗尽写成 `activation_exhausted`。
经理或 `can_manage_staff` 角色先运行 `hq delivery status --id ...`；unknown 用原 `delivery resolve` 核对，
确认未送达或 exhausted 时用原 `delivery retry`。不得创建第二个 case/assignment，也不得用裸 prompt 补派。

跨部门返工不是对旧 `accepted` report 执行 `return`，也不是一条 `message --kind handoff`。前者会倒转
审计终态，后者不会改变 durable owner。当前持有父 case 的部门经理使用 `case escalate`；HQ 在一个原子
事务中创建新子 case 并记录 `case_escalation_prepared`，送达后新子 case 进入 `escalated`、owner 固定为
经理的 `reports_to`，原父 case 和旧 submission 完全不变。直属上级必须先 `accept`，使新 case 进入
`accepted`，随后再按普通规则使用 owner approval/standing decision 对自己的直属部门经理 `issue`。
若投递为 failed/unknown，按原 delivery_id 执行 `delivery retry/resolve`；不得重复创建另一个 escalation。

工程返工被总部验收后，`message` 给质量经理只传递修复证据，不能让已经提交且被验收的旧 QA assignment
再次 `report`。仍持有原质量 case 的经理应运行 `case revise --version <N+1>`，把复验目标、验收条件和修复
证据冻结为新的 case generation，再对自己的直属复验 seat 执行 fresh `issue`。新 issue 产生独立
`assignment_id` 并绑定新 version/digest；旧 finding submission、旧 assignment 及其验收事件保持不可变。
直接重报会被 HQ 拒绝，报错会给出上述两条带当前 case/version/seat 的纠正命令。

`issue` 同时写入 first-class Assignment Contract：`assignment_id`、case version/digest、project、issuer、
assignee、assignee seat version/digest、role card id/version/digest/manual、reviewer、acceptor、due 和 contract
digest。Herdr 门铃只携带冻结身份、手册指针与下一条 HQ 命令，不复制或临时覆盖角色人格。
assignee 必须先 accept 才能 report；
首次提交进入 `submitted`，return 进入 `rework` 并继续沿同一冻结 acceptor 复交，只有 report accept
才进入 `completed`。活动合同阻止 case 静默 revise/close；reviewer 在 submission 上成为 case owner 后也不能
用 owner-report 绕过该合同，必须先对当前 submission 执行 accept 或 return。`hq assignment list/show` 可以在不连接
Herdr、不创建 lock/state、也不隐式恢复 txn 的情况下查询该生命周期。

`--next`、`--note`、`--verify` 以及 return/close 的原因是业务叙述字段：保持单行，
按合法 UTF-8 bytes 计算，每个硬上限 2 KiB。标识符、标签、结构化引用和运维短提醒仍保留
200 rune 的紧凑上限。issue 门铃的固定合同元数据不占用这个 2 KiB 字段配额；完整基础载荷受
64 KiB 总线基线保护。

approval 协议从 `config.yaml` 的 `owner_principal` 读取公司所有者标识，见证人与批准人分别记录。
以下示例假设 `owner_principal: ZC`：

approval 用于总裁办向部门经理等公司级授权，不是部门经理给直属员工日常派工的前置步骤。经理不得为
直属 issue 申请 approval，也不得借 approval/decision 向非直属 seat 越权派工。

```bash
./bin/hq approval request --id APR-ID --case CASE-ID --target <agent-slug> \
  --expires <future-rfc3339>
./bin/hq approval grant --id APR-ID --issuer ZC
./bin/hq approval show APR-ID
./bin/hq issue --case CASE-ID --to <agent-slug> --approval APR-ID --next <下一步>
```

`--issuer` 必须精确匹配当前 `owner_principal`；批准事实仍来自公司所有者，由 registry 中唯一的
`approval_witness`（总裁秘书）忠实上传和记录。总裁秘书不得把自己的推断、默认值或沉默当作批准；
授权含糊时必须回问人类所有者。修改 principal 不会重写历史事件；已经落账的 grant 保留其原始 issuer。

HQ 只接受 `one_time` approval。request 同时冻结 case business generation 与目标部门经理的
seat version/digest；grant 和最终 issue 都会重新核对这两个快照。即使 case owner、version 和
digest 在一次派发往返后恢复成原值，旧 approval 也不会 ABA 复活；同名岗位换了角色卡或
seat incarnation 后也不能沿用旧批准。HQ 会在拒绝时提示用新 `approval_id` 重新 request；
尚未 grant 的 request 可先 `approval revoke` 释放其冻结 seat。

平级与协作沟通使用 `message`，它不会改变 case owner、状态、优先级或授权：

```bash
./bin/hq message --to <agent-slug> --kind info --case CASE-ID \
  --text <消息正文> --ref-file <原文路径> --ref-case <case-id> --delivery auto
./bin/hq message ack --message <message-id>
```

正文按 UTF-8 bytes 计算，硬上限为 2 KiB；超限必须写入文件并用 `--ref-file` 引用。引用参数只有
`--ref-file`、`--ref-case`、`--ref-message`、`--ref-event`，均可重复；没有通用 `--ref` 别名。
`--reply-to` 单独表达回复关系并引用稳定 message ID。prepared/queued/sent/acked 全程使用同一
message ID。Agent 间信封固定以 `[HQ message]` 开头，派活、回报、核验等信封固定以
`[HQ notification]` 开头，以便与公司所有者直接对 Agent 说的话区分。

`question|request|handoff` 属于行动型消息：信封会给出精确的
`hq message ack --message <message-id>`。接收方读懂后必须先写入 durable ack；ack 只证明收到，
不表示接受结论，也不改变 case owner/status。未 ack 会同时阻止发送方和接收方的
`on_assignment` runtime 自动休眠。普通 `info` 不要求 ack。

`message --delivery` 支持：

- `wakeup`：下一回合并唤醒；
- `quiet`：进入下一回合上下文但不唤醒；
- `inject`：注入下一步骤但不唤醒；
- `auto`：根据目标忙闲、消息类型和连续唤醒预算选择。

`issue` 永远使用 `wakeup`。HQ 会在已有唤醒 prompt 中按 FIFO 选择不超过 policy item/byte 双上限的
静默上下文前缀，或在接收方执行 `accept` 时附加尚未合并的上下文；每条消息之间空一行。
attempt 会冻结 manifest 与最终 prompt digest，overflow 留待后续回合；默认上限为 8 条/16 KiB。
2 KiB 硬上限仍约束用户提交的单条 `--text`。
`delivery consume` 保留为故障恢复/调试入口。通过 `staff add` 新入职的成员在收到直属经理首个
durable case 前，平级或跨部门消息只会静默排队，不会唤醒或要求其处理。

`agents[].agent_args` 可显式配置传给 Herdr `agent start ... --` 后的原生 argv。`permission_mode=native`
只传递这些参数；`permission_mode=yolo` 会在其后补齐 kind 的必需授权参数。
HQ 为九种常用 kind 提供自动授权启动参数：

- `claude`: `--dangerously-skip-permissions`
- `codex`: `--dangerously-bypass-approvals-and-sandbox --dangerously-bypass-hook-trust`
- `copilot`: `--yolo`
- `cursor`: `--force`
- `gemini`: `--approval-mode=yolo`
- `grok`: `--always-approve`
- `kimi`: `--auto`
- `opencode`: `--auto`（仍服从显式 deny 规则）
- `qwen`: `--approval-mode=yolo`

未知 kind 不猜测权限参数。这些自动授权模式会
扩大 Agent 对本机文件、命令、网络或外部工具的操作能力，应只用于权限边界明确的岗位和环境。

## 投递与恢复

所有业务写默认经过 `ceo-office/records/hq.sock`。如果公司根目录使 Unix socket 地址超过操作系统上限，HQ 会自动改用 `/tmp/hq-gateway-<uid>/<data-dir-sha256>.sock`；该目录必须由当前用户拥有且权限为 `0700`，socket 本身仍要求 `0600`。调用方、`doctor`、`ping` 与 gateway 统一计算同一地址，不需要缩短公司目录名。gateway 使用实时 Herdr snapshot 把 pane
映射到 registry 身份，再执行权限、汇报线和状态机校验。业务命令不能用 `--direct` 绕过 gateway。

gateway 的健康检查/请求 framing 使用短 deadline；合法业务请求改用覆盖 Herdr cold start、最终 binding
复核和 prompt delivery 的独立长 deadline。服务端同时把稍短的执行 context 贯穿 identity、snapshot、
CreateTab、StartAgent 和两次 Prompt，并给协议响应保留 grace；配置访问、operation、ESTOP admission、
ledger 与 cold-resume 启动锁等待也服从该 context。外部投递已开始时，终态/unknown 落账使用这段受限
response grace，不会因执行 context 到期而把可收敛状态遗留成无说明的客户端超时。
若连接已建立却仍未收到可验证响应，HQ 会把结果标为不确定，
明确禁止直接重复命令，并给出 `history`、`case show` 或 `assignment list` 的只读核验命令。尤其是 `issue`，
必须先确认是否已有 assignment/`issue_sent`，避免把“已送达但响应丢失”误当成未执行。

durable outbox 的基本状态为：

```text
prepared → attempted → sent
                    ├→ failed_pre_send
                    └→ unknown
```

- `failed_pre_send` 表示能够证明 transport 没有启动，可显式 retry；
- `unknown` 表示调用开始后结果不确定，必须根据外部证据人工 resolve；
- 相同 delivery ID、target 和 payload 复用幂等记录；
- HQ 采用保守 at-most-once 策略，不宣称 Herdr 层面的 exactly-once。

每个 delivery 与 nudge 都按稳定 ID 取得跨进程 striped operation lease。每个 seat 另有独立的
runtime-seat process/flock 租约，串行该 seat 的 Prompt、cold-resume 和 reap，不与 operation stripe 复用。
issue 还在 durable origin/WIP 之前取该 seat 租约，确保正在关闭或结果不明时零业务写入地拒绝。
租约覆盖权威状态重读、durable attempt、Runtime Admission、Herdr/transport 调用和 durable terminal；
retry/reconcile/resolve 使用同一把 operation 协议锁。因此并发 reconcile 不会把仍在执行的 attempt 当成崩溃残留。
进程崩溃后 flock 由内核释放，恢复方仍只能按持久化 attempted 保守转入 unknown/人工 reconcile。
全局锁序为 `operation process/flock → runtime-seat process/flock → ESTOP admission → up lock
→ registry process/config-directory flock → ledger flock`；不得反向取锁。

`issue_prepared` 和 `report_prepared` 同时是业务投递栅栏。只要 origin 仍匹配 case 当前 generation/version/digest
且 delivery 未 `sent`，同一 case 不得新建 issue/report、revise/close 或拆分子 case。`failed_pre_send`
和人工确认 `not-delivered` 不会解除栅栏；应 retry 原 delivery，不能制造新业务命令。`unknown` 只能根据
接收方或 transport 证据 resolve。每个 case 当前业务轮只能有一个适用 origin；重试前必须重验该 origin 仍是
唯一栅栏，且 case version/digest/from-state 仍匹配。只有它自己的 `sent` 或有证据的
`resolved-delivered` 才能收敛栅栏并推进业务状态。

wakeup attempt 会在同一账本事务中冻结一个有 item/byte 上限的 FIFO Turn Bundle，并预留其所选 quiet/inject
context。并发 wake attempt 因此只能得到互不重叠的 manifest。transport `sent` 或人工确认 `delivered` 时，
所选 context 的 `attempted → message_sent → claimed` 与父 delivery 终态作为一个 durable batch 收敛；崩溃恢复
只能前滚完整 batch。`failed_pre_send` / `not-delivered` 释放预留且不 claim；`unknown` 保留预留，等待人工核验。
strict replay 会从持久化的真实 base payload 与 envelopes 重算每项 bytes、FIFO/overflow、最终 prompt digest
和 manifest digest。以上保证的是 HQ ledger 内的选取与收敛，不把外部 Herdr Prompt 冒充为 transport exactly-once。

对 issue，`delivery status` 还会显示独立的 `activation_status` 与 `activation_attempt_count`。主状态 `sent`
仅表示注入成功；assignment 只有出现 durable `accept` 才算真正激活。`delivery retry/resolve` 会根据主投递状态
自动处理 failed/unknown 主投递或 issued assignment 的激活状态，不需要另一套子命令。

```bash
./bin/hq reconcile
./bin/hq delivery status [--id <delivery-id>]
./bin/hq delivery retry --id <delivery-id> [--command <稳定命令号>]
./bin/hq delivery resolve --id <delivery-id> \
  --outcome delivered|not-delivered --reason <原因> --evidence <依据路径>
./bin/hq delivery budget status [--target <agent-slug>]
./bin/hq delivery consume [--limit 100] # 仅人工恢复/调试
```

## 运行与运维

```bash
./bin/hq doctor
./bin/hq doctor --json
./bin/hq board
./bin/hq project list [--status active|review|blocked|closed] \
  [--priority P0|P1|P2|unset] [--owner <slug>] [--department <department>]
./bin/hq project show --project <case.project 的精确值>
./bin/hq patrol --json
./bin/hq session list --agent <slug> \
  --type started|stopped|hibernate_attempting|hibernate_deferred|hibernate_failed|hibernate_unknown|fallback_attempting|fallback_failed|fallback_unknown|fallback_recovery_sent --json
./bin/hq --direct runtime status [--agent <slug>] [--json]
./bin/hq --direct runtime reap [--agent <slug>] [--json]
./bin/hq --direct runtime reap --agent <slug> --retry-failed
./bin/hq --direct runtime reap --agent <slug> --retry-unknown
./bin/hq --direct runtime fallback --agent <slug> [--retry-unknown]
./bin/hq flow show --case <case-id> --json
./bin/hq index rebuild
./bin/hq index query --entity flow_events --case <case-id>
```

- `doctor` 只读检查实例路径、registry、岗位手册、决策、Herdr、gateway 和账本健康；
- `patrol` 使用两份 snapshot 区分 blocked、编制漂移、orphan 和持续死亡候选，不自动重启或关停；
- `board` 展示结构化事项，`PRI` 列来自 case 规格的 `priority`，不会拿 finding `severity` 冒充事项优先级；`state.json` 缺失或损坏时可由账本重建；
- `project list/show` 严格重放主账本；合法空间只会返回零个或一个冻结 `case.project`，department 分布由当前 registry 映射；
- `assignment list/show` 展示冻结的委派、验收角色、due 和当前合同状态；
- `index` 只提供固定字段查询，不开放任意 SQL；
- `nudge` 和 `reminder` 负责回合边界提醒，不改变 case 权限和业务结论；
- `estop` 冻结非豁免子角色，并以显式 release 精确恢复本次确认冻结集。

### On-assignment runtime 休眠

gateway 启动时立即扫描，之后周期性运行幂等 reaper；gateway 重启会用 ledger、session 流和
Herdr snapshot 补偿未完成的运行态记账。仅 `on_assignment` seat 可自动休眠；`always` 永不自动关闭。
保温起点是最新 assignment 的 durable `event_accepted`/`event_returned` 时间，不使用可被其他事件推迟的
case `updated_at`。

关闭前必须同时满足：

- runtime 处于 idle/done，keep-warm 已到期，并且最终 snapshot 的精确 terminal/native session 绑定仍一致；
- 没有 active WIP、该 seat 持有的未决 case、未决正式 workflow delivery、nudge 或 reminder；
- 没有尚未 ack 的行动型 `question|request|handoff`；quiet/inject 的非行动 info 可保留在 durable
  Turn Bundle，不会让一个已完成的专业 seat 永久常驻，下次有权 issue 唤醒时再消费。

休眠不是撤销 assignment。若员工仍持有未完成合同，精确绑定该合同的 report return，或由冻结
issuer/reviewer/acceptor 针对同 case 发送的 actionable message，可以 cold-resume 原 seat；`reconcile` 复用原
prepared delivery。无关 case、非合同 actor、info message、已消费 assignment 和 manual seat 均不能借消息取得
启动权。

session 诊断流为 `started → hibernate_attempting → stopped`，可以保守收敛为
`hibernate_deferred|hibernate_failed|hibernate_unknown`。`failed` 表示能证明 CloseTab 未运行；`unknown`
或遗留 `attempting` 表示必须人工核验同一 incarnation。两个 retry flag 都必须同时给出单一
`--agent`，不允许批量猜测重试。CloseTab 成功但 `stopped` 追加失败时，下次扫描从已消失的
精确 runtime 补记，不会再关一次。CloseTab 或 session 记账失败只改变运行诊断，不回滚或篡改已成立的
业务终态。

若 agent 已消失但空 tab 仍在，HQ 会补记 runtime stopped，但不把空 tab 当作 HQ 拥有的当前
incarnation 自动关闭。`runtime status/reap` 会持续显示 `orphan_tab_without_agent`，运维者应先运行
`hq patrol`，核对后在 Herdr 人工清理该 tab。

### 模型 safeguard 的运行载体 fallback

当配置 `runtime_fallback.auto=true` 时，gateway 的周期守护会读取已登记 seat 的有界终端快照。它只在同时满足
以下条件时把 `from_kind` 替换为 `to_kind`：

- 终端尾部同时出现完整的 provider `This content can't be shown`、cybersecurity/Trusted Access 文本与 Codex 输入提示，而非仅在任务正文中引用这句话；
- runtime 仍精确绑定当前 workspace/tab/pane/terminal/native session，且处于 idle/blocked；
- seat 持有未完成的 durable assignment 或 case，可生成恢复清单。

状态机先记录 `fallback_attempting`，只有 Herdr snapshot 明确证明旧 tab 消失后，才追加 `stopped`
并启动新载体。关闭结果不确定时进入 `fallback_unknown`，禁止自动启动第二个运行实例。新 session
使用同一 seat、工位和 HQ 账本；恢复信封列出 active assignment/case 及精确查询命令。隐藏模型聊天记录不会被宣称为跨供应商复制；
连续性来自 durable ledger、版本化 `AGENTS.md` 和同一 workstation。`fallback_recovery_sent` 是恢复信封已确认投递的审计事实；
记账失败时允许幂等重发该恢复信封，但不重建 case 或 assignment。

`fallback_attempting|fallback_unknown` 不会被自动重试。新业务在写入 origin/WIP 前会被 fail closed，并告诉
`can_manage_staff` 运维角色先用 `runtime status --agent <slug>` 核验同一 Codex incarnation/tab，再显式运行
`hq --direct runtime fallback --agent <slug> --retry-unknown`。若旧 tab 已由人工或延迟 CloseTab 消失，下一轮 watcher 会依据
snapshot 补记 stopped 并继续 fallback，不需要重发业务任务。

`close` 必须按 child-before-parent 的 post-order 销账；`accepted` 仍不是 `closed`。若 descendant 有 active
assignment 或处于 escalation，报错会分别标出必须行动的 assignee/reviewer/owner，不能由 account closer
冒充这些角色直接执行 accept/return。目标 case 或 descendants 的正式 workflow delivery（issue、report、
escalation、accept/return notice）若仍为 prepared/queued/attempted/failed_pre_send/unknown，也会阻止关闭并
给出 reconcile、retry 或人工 resolve 的对应恢复动作。普通 info/handoff message 不属于关账门禁，
因此 closed case 之后仍可发送 postmortem 通讯。

### 只读 Project View

当前 Project View 是该 HQ space 唯一项目的可见性投影，不是第二套 Project Charter。它以 HQ ledger 中 case 的当前业务投影为
业务事实，并用当前 registry 映射 owner 的 department；registry label/department 变化会改变展示分布，但不会
改变 case 或项目业务状态。它不会读取 Herdr runtime，也不会因为 Agent 显示 `done`、离线或重启而改变完成结论。
只要账本存在业务 case，它必须恰有一个带非空 project 的 root，所有 non-root 必须通过 parent chain
连到该 root；任何多 root、多 project、空 project 或孤立子树都使整个账本 fail closed。

投影展示固定 `roots=1`、total cases、case status/priority 统计、owner/department 分布，以及三个可重叠缺口：

- `review_gap_count`：处于 `reported` 或 `finding_reported`，仍待业务核验的 case；
- `blocked_gap_count`：处于 `blocked`、`needs_decision` 或 `returned` 的 case；
- `closure_gap_count`：所有尚未显式进入 `closed` 的 case。`accepted` 仍是未关闭事项，因此仍计入该缺口。

项目汇总 `status` 的确定性优先级是 `closed`（无关闭缺口）、`blocked`、`review`、`active`；汇总
`priority` 是未关闭 case 中最高的 P0/P1/P2；项目全部关闭后保留所有 case 的历史最高 priority。`project list` 的过滤器只决定返回哪些完整项目，
不会先删 case 再重算统计。Project View 直接从 ledger 重建，因此损坏或删除派生 `state.json` 不会改变结果。
若存在 durable `txn/` intent，只读 Project/Assignment 查询会 fail closed，拒绝隐式恢复或返回过时投影。
唯一 root 只能在 descendants 和正式 workflow delivery 全部收敛后关闭，因此在合法账本中，root closed
就等价于 Project closed，并使该业务树归档；postmortem message 仍可发送，但不能再新增业务 case。

急停属于 `can_manage_staff` 运维能力，每一步都会重新核验实时身份：

```bash
./bin/hq --direct estop activate --id <estop-id> --reason <原因>
./bin/hq --direct estop status
./bin/hq --direct estop release --id <estop-id> --reason <解除原因>
```

## 数据与协议

| 路径 | 角色 | 恢复方式 |
|---|---|---|
| `config.yaml` | 组织、权限、汇报线和岗位事实源 | 备份、决策记录和获批的 staff mutation |
| `records/events/YYYY-MM.jsonl` | append-only 业务事实源 | 严格重放 |
| `records/state.json` | case 状态投影 | `hq rebuild` |
| `records/hq.sock`（长路径时为私有 runtime 哈希地址） | 本地 gateway socket | `hq up` / `hq serve` |
| `records/sessions/YYYY-MM.jsonl` | 独立会话启停流 | 严格重放 |
| `records/estop/state.json` | 可恢复急停运行状态 | 主账本与 snapshot 交叉核验 |
| `tools/index.db` | Markdown 和事件派生索引 | `hq index rebuild` |

当前唯一事件协议是 v3，每条权威事件都必须显式携带 `event_version: 3`。每条事件还包含
全局递增 `int64 sequence`、稳定 `command_id`、请求摘要、`previous_event_hash` 和 `event_hash`。
重放严格拒绝非 v3 envelope、坏 UTF-8、截断行、未知或重复 JSON 字段、重复/倒退序号、
哈希断链和非法状态转移。未发布的开发中数据不是产品输入；新建公司只能以当前 v3
registry 和空的 v3 ledger 开始。

每次写事务在一个跨进程锁内完成：

```text
恢复 intent → 严格重放 → 幂等与权限校验 → 生成 batch
→ durable intent → 追加并 fsync → 原子更新 state → 清理 intent
```

`state.json` 和 `index.db` 永远不能覆盖事件账本。`--dry-run` 使用相同的锁内视图和校验器，
但不写事件、state 或 intent。

## 安全边界

- gateway socket 为 `0600`，协议绑定版本、workspace 和 server identity；
- agent 身份同时绑定 workspace、name、cwd、kind、tab、pane、session 和 interactive readiness；
- Herdr mutation 区分 definitely-not-run、ambiguous 和 confirmed；ambiguous 先 snapshot reconcile；
- start/resume/prompt/cold-resume 在外部 mutation 前统一经过 ESTOP 与 live-binding admission；workspace/gateway
  创建及直接 `serve` 另需 control-plane admission，active ESTOP 下不能借豁免经理启动顺带恢复 gateway；
- config、records、岗位手册和引用必须位于允许的 canonical 根，拒绝 symlink 与类型混淆；
- 长文只保存 canonical 文件或 Git commit 引用，事件拒绝疑似密钥、Cookie、口令和金额；
- `--direct` 只开放给实时在岗的 `can_manage_staff` 角色处理运维白名单，不授予业务写权限；已完成 init 的
  公司从宿主机执行无参数 `hq up` 是单独的冷启动路径，明确拒绝 `--direct`。

HQ 的信任模型用于防误操作、保证流程一致性和提供可审计恢复，不是同一 OS 用户内的敌对安全边界。
当前 socket 调用者提供的 pane identity 只能与 Herdr snapshot 交叉核对，不能以密码学方式证明调用进程拥有该 pane；
同一用户下受 prompt injection 控制的进程也可能改写 config、approval、角色手册、digest 或 HQ binary。
因此哈希和 approval 在当前部署中是流程证据与漂移检测，不是抵抗恶意同用户重签的信任根。强敌对环境必须让
HQ supervisor/registry/ledger/approval key 由员工进程不可写的独立 OS principal 或只读挂载持有，并由 Herdr
提供不可伪造且绑定 runtime incarnation 的调用凭证。

live binding 是 Herdr snapshot 给出的时点证明。HQ 会在业务 wakeup、nudge 和 startup Prompt 的关键边界至少两次复核
seat、kind、cwd、tab/pane 与 readiness，已观察到的漂移不会进入 Prompt。wakeup/nudge 的两次验证是时点证明，
不是绑定到 attempt 的 incarnation CAS；startup 则在 Start 后冻结本次创建的 workspace/tab/pane，写入 session 事实后
再复核同一 created IDs，并在 Herdr 提供时检查 terminal/native session 连续性与 revision 不倒退。但当前 Herdr
`agent prompt` 只接受 agent name，不接受 `expected_terminal/session/revision` 形式的 compare-and-send 条件。因此最后一次
snapshot 与按 name Prompt 之间仍存在上游能力边界内的极短替换窗口。彻底关闭该窗口需要 Herdr 提供原子 expected-binding
参数；HQ 不把两阶段时点核验描述成 transport 级原子绑定。

runtime 关闭也有同类且更明确的上游能力边界：Herdr 当前 `tab close` 只接受 `tab_id`，没有
`expected_terminal/session/revision` 的 conditional-close/CAS。HQ 会持有跨进程 seat 租约，并在关闭前再读
registry、ledger/session 和最终 snapshot，因此能拒绝所有已观察到的配置或 runtime 替换；但同一 OS 用户在
最后 snapshot 之后、CloseTab 实际执行之前从 HQ 外部替换同 tab occupant，Herdr 无法原子拒绝。
runtime-seat flock 只协调 HQ 进程，不是对手式同用户边界；彻底关闭此窗口需要 Herdr 新增按预期
terminal/native session/revision 的原子 conditional close。

Gateway 启动分为三段：先取得并立即释放一次只读 control-plane SH preflight，保证已 active/corrupt
ESTOP 下零 socket/data mutation；然后做 outbox reconcile，每个 delivery 依次取 operation lease 和所需 ESTOP SH；
最后再取 control-plane SH，只在其内完成 data root、stale socket、bind/chmod 和 listener identity。SH 在长期
accept loop 前释放，所以在线 gateway 仍能作为 ESTOP activate/release 的控制通道。默认 `hq up` 等待子网关
健康前也会释放 `.hq-up.lock`，另用 gateway bootstrap lock 序列化并发父进程，避免子网关重放时
cold-resume 反向等待父进程。

## 当前协议合同

- registry 只接受严格 YAML v3，权威事件只使用 event v3；
- gateway 和 Herdr snapshot 也必须匹配当前代码中明确定义的版本与必填字段；
- v1.0.0 只承诺本文记录的 registry v3、event v3 与 CLI 合同；开发中的其他格式不是产品输入；
- 正式公司实例必须由当前 `hq init` 生成，不接收开发期间的资料目录作为运行输入。

## 验证

日常变更至少运行：

```bash
GOCACHE="${TMPDIR:-/tmp}/hq-go-cache" go test -count=1 ./...
GOCACHE="${TMPDIR:-/tmp}/hq-go-cache" go vet ./...
```

确定性真实公司流程模拟（注入 identity/runtime/transport，不连接在线 LLM）：

```bash
go test -count=1 -run '^TestVirtualCompanyHeadquartersLaunchScenario$' .
go test -count=1 -run '^TestVirtualCompanyDeliveryIncidentRecoveryScenario$' .
```

第一个场景覆盖“总裁办—产品部—工程部”、纵向拆解、跨部门 quiet handoff、Turn Bundle、
退回复交、冻结 acceptor、项目销账与 ledger rebuild。第二个场景模拟外部写入超时：业务栅栏冻结、
进程重启、人工证据 resolve、同 delivery ID 授权重试、单次收敛和最终重建。operation lock、ESTOP、
business-delivery fence 和 live-binding race 由独立并发/故障测试覆盖；这两个测试不冒充在线 Herdr 多进程 E2E。

完整发布门禁：

```bash
./scripts/test-gates.sh
```

门禁覆盖单元/集成测试、race、vet、gofmt、README 首验、当前平台构建以及三平台 release/checksum。
