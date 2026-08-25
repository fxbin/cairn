# Cairn 开发指南

## 项目与入口

- Cairn 是 Go 1.26.4 实现的事件溯源编码 Agent V0，模块为 `github.com/fxbin/cairn`。
- 可执行入口是 `cmd/cairn/main.go`；它处理 `SIGINT`/`SIGTERM`，第一次信号交由 Agent 持久化取消，第二次以 130 退出。
- CLI 实现在 `internal/cli/`，命令为 `run`、`resume`、`inspect`、`doctor`、`benchmark run`、`help`。`--json` 时 stdout 只能输出 NDJSON。
- 默认配置路径为 `<workspace>/.agent/config.yaml`。API key 只从 `provider.api_key_env` 指定的环境变量读取，不能写入 YAML 或事件。

## 模块地图

- `internal/domain/`：事件、闭合 payload schema、哈希链与 Run 投影/replay。
- `internal/contract/`：普通 Run 与 benchmark 共用的版本化产品合同（TaskSpec、RunManifest、Result 与统一失败分类）；全部为事件与冻结配置制品的只读投影，不得成为第二事实源或携带密钥、任务正文、完整路径或 verifier 内容。
- `internal/ports/`：Provider 与 Journal 的端口；Domain 不依赖 CLI、HTTP、SQLite 或文件系统。
- `internal/adapters/`：SQLite/内存 Journal，以及 OpenAI-compatible 流式 Chat Completions Provider。
- `internal/tools/`：固定工具注册表；只读工具为 `list_files`、`read_file`、`search_text`，副作用工具为 `apply_patch`、`exec_command`。
- `internal/policy/`：写入、命令执行的 allow/deny/ask 决策与审批指纹。
- `internal/artifact/`：内容寻址、JSON 脱敏、权限和制品引用完整性。
- `internal/agent/`：串行单写入者 Loop，负责模型调用、审批、工具、预算、取消、恢复与验证。
- `internal/app/`：装配 Provider、Journal、工具、Policy、Artifact Store 和工作区锁，提供运行、恢复、对账、中止、检查、诊断用例。
- `internal/lock/`：工作区排他锁；当前 Unix 实现面向 Darwin/Linux。
- `internal/benchmark/`：本地任务合同解析、独立 clone runner 与非敏感结果 schema；不处理模型训练或调参数据。
- `benchmarks/`：个人本地评测任务、verifier、冻结配置、结果与一次性工作区；整个目录必须忽略且不得进入版本库。
- `test/integration/`、`test/e2e/`、`test/fault/`：分别覆盖真实 HTTP/SQLite 集成、四个离线 fixture 和故障边界；`test/contract/` 锁定普通 Run 与 benchmark 的统一合同投影一致性。

## 核心理念与协作流程

- **简洁至上**：以 KISS 为默认原则。优先采用标准库、现有边界和最小可验证改动；不得为尚未证实的扩展点、故障场景或抽象层预埋框架。安全边界、数据完整性和明确的产品合同不是“过度防御”，不得因简化而削弱。
- **第一性原理与事实优先**：先区分可观察事实、明确假设和待验证未知项。结论必须能指向代码、测试、事件或制品证据；发现用户前提、既有实现或设计存在错误时，应直接说明事实、影响和可选修正，不迎合错误前提。
- **先调研，后实现**：任何设计或编码前，先阅读适用的 `AGENTS.md`、相关调用链、配置/合同和已有验证。疑点优先以只读检查消除；只有会实质改变范围、安全性、兼容性或外部状态的未知项才请求用户决策，并在执行时显式记录所采用的假设。
- **结构化推进**：中等及以上复杂度或高风险改动遵循“构思方案 → 提请审核 → 分解任务 → 实施 → 独立复核 → 交付”的顺序。目标明确且可逆的小改动可直接实施，但仍须在交付中说明方案、证据和剩余风险。
- **中文协作**：面向用户的回复、计划、任务清单、代码注释和提交说明均使用中文。不得输出内部思维链；应以简洁的中文给出可审查的事实、理由、假设与结论。

## 操作时间与审计

- 每个会读取、运行、修改或影响仓库/运行环境的工程操作前，先获取本机当前时间，并以 `yyyy-MM-dd HH:mm:ss.SSS` 格式记录到 Git 忽略的 `.vidt/operation-log.jsonl`。记录至少包含时间、操作类别、目标范围、是否改变状态和结果；不得写入 prompt、源码正文、路径细节、密钥或其他敏感内容。日志写入自身不再递归记录。
- 运行时的外部副作用在持久化授权之前同样必须取得时间，并按既有 Event Journal 合同记录可审计时间。若现有事件 schema 使用规范化时间格式，不得为展示格式擅自改变其 schema；展示/运维日志另行使用上述毫秒格式。
- 时间必须来自系统时钟或可注入 `Clock`，测试不得依赖未冻结的真实时间；禁止伪造、回填或省略会影响因果排查的操作时间。

## Go 代码与测试规范

- `cmd/` 与 `internal/` 的生产 Go 代码中，每个 `type`、顶层函数和方法都必须在声明紧邻位置写中文文档注释，说明职责、关键输入/输出、状态变化及必要的安全约束；测试函数和显而易见的匿名字段除外。注释应准确具体，不机械复述标识符。
- 注释一律置于独立行；禁止行尾注释。新增生产 Go 文件在 package 文档中标注 `Author: fxbin`；不为每个符号重复添加作者标签，提交作者和 Git 历史是其余归属事实源。
- Go 不使用 Lombok 或 MapStruct：不得生成机械的 getter/setter；优先用构造函数、显式方法和字段可见性保持不变量。对象/DTO/领域模型转换必须使用显式、可测试的转换函数，禁止反射式映射、`map[string]any` 中转或 JSON 往返转换。
- 禁止未命名的业务魔法值。预算、TTL、状态码、大小、超时、schema 版本和策略枚举必须以有语义的常量、类型或配置字段表达；语言结构所必需且语义显然的 `0`、`1`、切片边界等可保留。
- 交付前必须对新增或修改的代码、说明和配置做内部审查，检查正确性、完整性、错误路径、并发/资源释放、事实边界、兼容性与本文件约束。
- 未经用户明确授权，禁止新增、删除或修改任何单元、集成、端到端、故障或契约测试用例；可以运行既有测试。若生产改动必然要求调整既有测试或新增覆盖，先说明原因、影响和最小测试范围，等待授权后再改动测试。

## 开发约束

- 修改 EventType、payload 字段或状态迁移时，同时更新 `internal/domain/event.go` 的 `payloadSchemas`、`internal/domain/state.go` 的 replay 规则及相关测试。事件 payload 会被规范化并纳入哈希链。
- 所有副作用都必须遵循：提议 → Policy 决策 → 必要时审批 → `operation_started` → 结果或对账 → 关闭 Turn。不得在持久化授权前执行 patch 或命令。
- 工具必须由 `tools.NewRegistry` 显式注册；V0 不支持动态工具发现、插件、MCP、daemon/RPC 或 Team 实体。
- 所有工作区路径通过 `tools.Workspace.Resolve`。不得绕过工作区边界或跟随逃出工作区的符号链接。
- `apply_patch` 是确定性的单文件操作：更新/删除绑定已有 SHA-256，写入采用临时文件、同步与原子重命名。
- `exec_command` 只接受 argv，不经 shell；它受工作区、环境白名单、输出/超时上限和进程组取消保护。
- `sandbox.backend` 可选 `host` 或 `docker`；使用 Docker 时必须指定本地已有镜像，并以无网络、只读根文件系统、最小能力、有限 PID 和唯一工作区挂载失败关闭执行。不得在 Docker 不可用时回退到宿主机。
- `cairn benchmark run` 必须通过 `--local-config` 显式读取 Git 忽略的本机评测配置；配置必须分别给出任务合同、冻结 Agent 配置、私有 verifier、结果、clone 工作区和 state 目录，CLI 不得从 manifest 位置推导这些目录。每次尝试必须有新的 clone、任务专属 Docker 镜像、固定 verifier 和冻结配置。基线克隆走本地优先的对象库（`<workspaces>/_mirror`）：基线已在本地镜像时不发起网络请求，缺失才镜像克隆或增量 fetch；每次尝试的工作区仍从镜像全新克隆并校验基线提交，对象内容完整性由 git 哈希保证。verifier patch 只能从忽略的本地目录注入，结果中只记录其摘要。
- `workspace.ignore` 只能限制工具视图，不能缩小 verifier 的非敏感、非 volatile 工作区观察范围。
- 新配置字段必须同步更新 `Config`、默认值、严格 YAML 校验与测试；影响执行、审批或预算语义的字段还必须加入 `internal/config` 的 `fingerprintView` 白名单并补充指纹变化测试（运维/展示类字段不得加入，避免在途 run 漂移）。写入和命令的默认策略都是 `ask`。
- Execution Profile 是版本化产品合同：`profile: default|complex`（空=default；default=40 轮，complex=80 轮并按 gate 实验非等比放大：工具 1.6×、输入 2.8×）。解析优先级固定为 安全默认值 → 命名 Profile → YAML 显式值 → 环境覆盖 → CLI 显式值；任意更高预算值必须显式出现并进入指纹。`Config.Resolution` 是只读的 Resolved Execution Plan（字段→值→来源），随冻结配置制品持久化，不得进入 YAML 解码或配置指纹。
- 静态 Preset 是编译期已知组件的能力组合：`preset: coding|read-only|benchmark`（空=coding）。read-only 强制 write/exec=deny、禁止验证命令并只注册三件只读工具（工具子集由 `readOnlySafeTools` 单一来源提供，Boot 装配与 doctor 清单共用）；benchmark 复用 coding 工具集，其冻结配置、Docker 与隔离约束由 benchmark runner 强制。Preset 进入配置指纹；未知 Preset 失败关闭；禁止目录扫描、自注册或动态加载。
- 对已 started 但未记录终态的 workspace/process Operation，使用 `cairn resume --resolve` 对账；不得盲目重试。`doctor` 必须保持只读。停在 `created`/`cancelling` 的非终态 run 只能经 `resume --abort-run` 补写终态解锁工作区。
- `test/fault` 与 `test/e2e` 分别以 `-tags=faultinject`、`-tags=e2e` 门控，默认 `go test ./...` 不包含它们；两条验证命令必须显式执行。
- `cmd/` 与 `internal/` 的生产 Go 代码中，每个 `type`、顶层函数和方法都必须在声明紧邻位置写简明中文注释，说明职责以及关键输入/输出、状态变化或安全约束；测试函数和显而易见的匿名字段除外。

## 高风险文件

- `internal/domain/event.go`、`internal/domain/state.go`：schema、哈希链、回放状态机。
- `internal/agent/loop.go`：执行顺序、审批、恢复、取消、预算和验证。
- `internal/policy/gate.go`、`internal/tools/workspace.go`：审批绑定、路径与验证观察边界。
- `internal/tools/apply_patch.go`、`internal/tools/exec_command_unix.go`：真实副作用边界。
- `internal/app/service.go`：workspace hash、制品/SQLite 路径、锁、配置漂移和对账。

## 验证

对修改过的 Go 文件运行 `gofmt -w <files>`，并按风险执行：

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/cairn
go test ./internal/adapters/provider/openaicompat -run Contract -count=1
go test ./test/fault -tags=faultinject -count=1
go test ./test/e2e -tags=e2e -count=1
```

## Git 与本地文件

- `docs/` 和 `.vidt/` 是本地规划/流程产物，必须保持忽略且不提交。
- `benchmarks/` 的任务合同、配置、verifier、结果和工作区必须保持忽略，不得包含在提交、发布包或示例中。`--local-config` 以及它在 Cairn 仓库内引用的所有资产会在运行前检查：已跟踪或未被 `.gitignore` 覆盖即失败。任务集只用于个人测评，不得作为模型训练语料或公开 holdout。
- runner 的 clone 与 bootstrap 不能继承模型密钥、代理凭据或用户 HOME；如需代理，只能显式传入无凭据的 loopback HTTP 地址。
- 根目录 `cairn`、`.DS_Store`、测试产物和编辑器临时文件也已被 `.gitignore` 排除。
- 新提交使用：`<type>(<scope>): <中文说明>`。一个提交只表达一个可审计意图。提交信息（标题与正文）不得出现 v2、V1.3 之类版本号标记，也不得引用具体模型名/模型版本与 A/B 回测数字；版本、模型与评测事实分别由代码、事件 schema、制品、benchmarks 本地目录与 `.vidt` 决策日志承载，提交说明只描述变更意图与语义结果。
