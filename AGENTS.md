# Cairn 开发指南

本文件只维护长期有效的协作规则、架构边界和代码入口。版本与依赖见 `go.mod`，命令参数见 CLI 帮助，默认值与兼容行为见实现和测试；不在这里维护阶段进度、实验数字或验收结论。

## 开始工作

- 使用中文沟通、编写代码注释与提交说明。先检查 `git status`、适用指南、相关调用链和既有测试；保留未提交改动，只处理本次范围。
- 以事实为准：区分代码已实现、离线验证、真实运行和发布验收。文档或记忆与当前代码冲突时，明确指出并核对，不沿用旧结论。
- 简洁优先：复用标准库和现有边界，不为未经证实的需求引入框架；不得以简化为由削弱安全、数据完整性或产品合同。
- 明确且可逆的小改动直接实施。中等以上复杂度或高风险改动先提出可审核方案，再分解实施、独立复核和交付；已有授权无需重复询问。
- 未经明确授权，不新增、删除或修改测试用例；可以运行既有测试。确需调整测试时，先说明原因与最小范围并取得授权。

## 项目导航

Cairn 是 Go 实现的事件溯源编码 Agent，模块为 `github.com/fxbin/cairn`。

| 入口 | 职责 |
| --- | --- |
| `cmd/cairn/main.go`、`internal/cli/` | 信号处理、CLI 分发；裸命令进入交互体验，运行、恢复、诊断、Provider、记忆和评测命令以帮助为准 |
| `internal/tui/` | 配置向导、任务输入、会话与事件展示、审批及补丁差异查看；业务动作调用应用层 |
| `internal/app/` | `boot.go` 装配与资源生命周期；`service.go` 运行/恢复/对账；`session.go` 前情；`rewind.go` 工作区回退 |
| `internal/agent/` | 串行单写入者 Loop；模型、工具、审批、预算、压缩、取消及验证 |
| `internal/domain/`、`internal/contract/` | 事件 schema、哈希链与 replay；任务、清单、结果和失败分类的只读合同投影 |
| `internal/config/`、`internal/credential/` | 配置解析、指纹与来源；环境变量和本机凭据文件装载 |
| `internal/provider/`、`internal/modelcatalog/`、`internal/ports/` | Provider 身份、证据与准入、模型目录建议、窄端口 |
| `internal/adapters/` | Journal、Memory 的 SQLite 存储及 OpenAI-compatible 协议适配 |
| `internal/tools/`、`internal/policy/`、`internal/artifact/`、`internal/lock/` | 工具和路径边界、授权、内容寻址与脱敏、工作区排他锁 |
| `internal/hooks/` | 工作区钩子：仅收紧 Policy，动作仅 `queue_followup`；不得授予权限 |
| `internal/memory/`、`internal/sandboxeval/`、`internal/benchmark/` | 可信记忆、受控演化、冻结评测与隔离执行 |
| `internal/decision/` | 项目决策账本的合同与状态机（ADR-0022）；**当前无运行时消费方，不得宣称已注入上下文** |
| `internal/workflow/` | 工作流步骤/调度**合同脚手架**；当前无运行时消费方，不得宣称已可调度 |

## 不可绕过的边界

- Event Journal 是运行事实源；TUI、记忆、遥测和合同投影不能替代它。Domain 不依赖 CLI、HTTP、SQLite 或文件系统；`--json` 的 stdout 仅输出 NDJSON。
- 副作用顺序固定：提议 → Policy → 必要时审批 → `operation_started` → 结果或对账 → 关闭 Turn。写入和命令默认 `ask`；回退同样走完整授权链。
- 内置工具由 `tools.NewRegistry` 静态注册；MCP 客户端由配置和 Composition Root 显式装配，发现目录及工具暴露状态从成功的 Journal 结果投影。外部调用保持完整授权链，目录/schema 漂移失败关闭，不自动重试结果不确定的调用。保持窄边界，不引入普通插件、daemon/RPC 或 Team 实体。
- Skill 是本地 `SKILL.md` 内容包，由配置显式启用并通过固定 `skill` 工具按需读取；正文激活只来自成功的 Journal 回执，压缩与冷恢复保留历史正文，资源读取绑定来源与摘要。技能的 `allowed-tools`、hooks、模型或子代理声明不能授予权限或执行代码；脚本仍走工作区、Policy、审批和沙箱边界。管理命令只写原生目录，跨客户端共享目录仅作显式只读来源。
- 用户全局 `AGENTS.md` 仅读取 `~/.cairn/AGENTS.md`，项目来源仅从显式工作区内有界发现；适用优先级为当前用户明确要求、就近项目规约、用户全局规约。正文是用户级指导，不能授予权限或自动执行命令；受控评测显式排除宿主用户规约。新运行将脱敏快照绑定到创建事件，压缩与冷恢复只读历史快照；旧运行缺少引用时不补读当前文件，引用损坏必须失败关闭。
- 工作区路径必须经过 `tools.Workspace.Resolve`，禁止符号链接逃逸。`workspace.ignore` 只限制工具视图，不得缩小 verifier 的非敏感、非 volatile 观察范围。
- `apply_patch` 保持确定性单文件操作，更新/删除绑定旧 SHA-256，写入使用临时文件、同步与原子重命名。`exec_command` 只接收 argv，不经过 shell，保留环境白名单、输出/超时限制和进程组取消。
- `exec_session` 仅在显式启用时注册：提供受控 tmux 会话（open/write/read/close），会话独立 ID、工作目录与环境白名单；建会话与敏感动词需授权，取消用 SIGHUP+进程组回收；Journal 记录会话生命周期与输入摘要，不记录按键原文；默认禁用，不继承 `exec_command` 审批豁免。
- Docker 使用本地已有镜像，无网络、只读根文件系统、最小能力、有限 PID 和唯一工作区挂载；不可用时失败关闭，禁止回退宿主机。
- 悬而未决的已 started 操作必须经 `resume --resolve` 对账，禁止盲目重试；停在 created/cancelling 的运行经 `resume --abort-run` 收尾。`doctor` 保持只读。
- 运行完成与验证可信分开；没有 verifier 证据不得宣称已验证。会话回退与文件回退语义分开，缺少无损 before 快照或工作区漂移时不能猜测旧内容，也不能将逐项回退宣称为整批原子事务。
- 记忆保留来源、失效与撤销关系，不能授予权限或决定运行终态。演化候选、晋升、沙箱激活和显式试运行保持隔离，不把试运行变为生产默认行为。

## 配置、凭据与合同变更

- 工作区配置为 `.agent/config.yaml`；用户根由 `internal/userdir` 统一解析为 `~/.cairn`；配置、凭据、全局规约、原生 Skills 和 MCP OAuth 都由此派生。用户资源统一从该根读写，不搜索或迁移其他系统配置目录。用户配置路径由 `config.UserConfigPath` 决定。保留安全默认值 → Profile → YAML → 环境 → CLI 的解析顺序；显式配置路径与用户级回退行为以解析器为准。
- 密钥只通过 `provider.api_key_env` 引用环境变量；本机 `keys.env` 使用 0600 权限，装载优先级为显式环境 → 工作区凭据 → 用户凭据。密钥不得进入 YAML、事件、制品、错误或提交。
- MCP 的环境与 Bearer/客户端密钥只引用环境变量；OAuth 令牌使用用户配置根下的独立私有存储、0600 原子文件和服务器身份绑定。登录由显式命令发起；read-only/benchmark 不启用外部 MCP，`doctor` 和 `mcp list` 不连接服务器。
- 新配置字段同步处理默认值、严格校验与测试。影响执行、权限、预算的字段加入 `fingerprintView`；展示/运维字段和 `Config.Resolution` 不进入指纹，Resolution 不参与 YAML 解码。
- Profile 的具体预算以 `internal/config` 为准；提高预算必须显式并进入指纹。Preset 只组合已知能力，未知值失败关闭；read-only 必须禁写、禁 exec、禁验证命令，工具子集由 `presetToolsWithWeb` 的只读分支统一提供。
- Provider 目录建议不等于能力验证；身份、协议、请求画像、探测证据和准入决定保持分离，变更时核对配置指纹与准入绑定。
- 修改事件或迁移时，联查 `domain/event.go` 的 `payloadSchemas`、`domain/state.go` 的 replay、写入方、读取投影和相关测试，保留旧事件兼容与哈希链完整性。
- 修改存储或恢复行为时，联查 CLI → App → Store → 完整性检查及错误路径；不得静默删除历史数据或放宽损坏检测。

## Go 规范与验证

- 生产代码的每个 type、顶层函数和方法紧邻写中文文档注释，说明职责及必要的输入输出、状态或安全约束；不机械复述名称。注释独立成行，禁止行尾注释；新生产文件的 package 文档标注 `Author: fxbin`。
- 使用显式类型、构造函数与转换函数；禁止机械 getter/setter、反射映射、`map[string]any` 中转或 JSON 往返转换对象。业务预算、TTL、大小、超时、状态码及版本使用有语义的常量或配置。
- 交付前自查正确性、错误路径、资源释放、并发、兼容性和本文约束；修改的 Go 文件运行 `gofmt`，所有改动检查 `git diff --check`。
- 纯文档改动检查事实、链接和 diff 即可。代码按影响范围运行既有测试；核心合同、跨模块或发布候选执行完整验证矩阵：

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/cairn
go test ./internal/adapters/provider/openaicompat -run Contract -count=1
go test ./test/fault -tags=faultinject -count=1
go test ./test/e2e -tags=e2e -count=1
```

默认测试不覆盖 fault/e2e 标签，涉及故障或端到端行为须显式运行。离线通过不能替代真实 Provider、TUI 交互或评测验收；交付说明实际运行的检查与未覆盖范围。

## 评测、本地文件与提交

- `docs/`、`.vidt/`、`benchmarks/`、`.agent/` 及构建/测试产物保持 Git 忽略，不提交、不打包；本地文档可以辅助决策，不能成为缺失时无法开发的唯一规范来源。
- benchmark 必须显式提供 `--local-config` 及各资产目录，不从 manifest 位置推导；配置和引用资产必须忽略且未跟踪。任务集仅供个人评测，不作为训练语料或公开 holdout。
- 每次评测使用新 clone、固定基线、任务专属 Docker 镜像、冻结配置与私有 verifier；镜像对象库本地优先，缺失才获取。verifier 仅从忽略目录注入，结果仅记录摘要，不泄露本地路径或正文。
- clone/bootstrap 不继承模型密钥、代理凭据或用户 HOME；代理仅允许显式无凭据 loopback HTTP 地址。
- 提交格式：`<type>(<scope>): <中文说明>`，一个提交一个意图，只暂存本次内容；提交说明不写版本标记、具体模型名/版本或 A/B 数字。

## 操作审计

- 每次读取、运行、修改或影响仓库/环境前，取得系统时间并写入忽略的 `.vidt/operation-log.jsonl`；时间格式 `yyyy-MM-dd HH:mm:ss.SSS`，记录类别、非敏感范围、是否改变状态及结果。日志自身不递归记录，不写 prompt、源码、路径细节或密钥。
- 运行时副作用的时间按 Event Journal 合同记录，不为展示格式改变 schema。时间来自系统时钟或可注入 Clock；测试使用冻结时钟，禁止伪造或回填操作时间。
