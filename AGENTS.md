# Cairn 开发指南

## 项目与入口

- Cairn 是 Go 1.26.4 实现的事件溯源编码 Agent V0，模块为 `github.com/fxbin/cairn`。
- 可执行入口是 `cmd/cairn/main.go`；它处理 `SIGINT`/`SIGTERM`，第一次信号交由 Agent 持久化取消，第二次以 130 退出。
- CLI 实现在 `internal/cli/`，命令为 `run`、`resume`、`inspect`、`doctor`、`help`。`--json` 时 stdout 只能输出 NDJSON。
- 默认配置路径为 `<workspace>/.agent/config.yaml`。API key 只从 `provider.api_key_env` 指定的环境变量读取，不能写入 YAML 或事件。

## 模块地图

- `internal/domain/`：事件、闭合 payload schema、哈希链与 Run 投影/replay。
- `internal/ports/`：Provider 与 Journal 的端口；Domain 不依赖 CLI、HTTP、SQLite 或文件系统。
- `internal/adapters/`：SQLite/内存 Journal，以及 OpenAI-compatible 流式 Chat Completions Provider。
- `internal/tools/`：固定工具注册表；只读工具为 `list_files`、`read_file`、`search_text`，副作用工具为 `apply_patch`、`exec_command`。
- `internal/policy/`：写入、命令执行的 allow/deny/ask 决策与审批指纹。
- `internal/artifact/`：内容寻址、JSON 脱敏、权限和制品引用完整性。
- `internal/agent/`：串行单写入者 Loop，负责模型调用、审批、工具、预算、取消、恢复与验证。
- `internal/app/`：装配 Provider、Journal、工具、Policy、Artifact Store 和工作区锁，提供运行、恢复、对账、中止、检查、诊断用例。
- `internal/lock/`：工作区排他锁；当前 Unix 实现面向 Darwin/Linux。
- `test/integration/`、`test/e2e/`、`test/fault/`：分别覆盖真实 HTTP/SQLite 集成、四个离线 fixture 和故障边界。

## 开发约束

- 修改 EventType、payload 字段或状态迁移时，同时更新 `internal/domain/event.go` 的 `payloadSchemas`、`internal/domain/state.go` 的 replay 规则及相关测试。事件 payload 会被规范化并纳入哈希链。
- 所有副作用都必须遵循：提议 → Policy 决策 → 必要时审批 → `operation_started` → 结果或对账 → 关闭 Turn。不得在持久化授权前执行 patch 或命令。
- 工具必须由 `tools.NewRegistry` 显式注册；V0 不支持动态工具发现、插件、MCP、daemon/RPC 或 Team 实体。
- 所有工作区路径通过 `tools.Workspace.Resolve`。不得绕过工作区边界或跟随逃出工作区的符号链接。
- `apply_patch` 是确定性的单文件操作：更新/删除绑定已有 SHA-256，写入采用临时文件、同步与原子重命名。
- `exec_command` 只接受 argv，不经 shell；它受工作区、环境白名单、输出/超时上限和进程组取消保护。
- `workspace.ignore` 只能限制工具视图，不能缩小 verifier 的非敏感、非 volatile 工作区观察范围。
- 新配置字段必须同步更新 `Config`、默认值、严格 YAML 校验、fingerprint 与测试。写入和命令的默认策略都是 `ask`。
- 对已 started 但未记录终态的 workspace/process Operation，使用 `cairn resume --resolve` 对账；不得盲目重试。`doctor` 必须保持只读。

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
- 根目录 `cairn`、`.DS_Store`、测试产物和编辑器临时文件也已被 `.gitignore` 排除。
- 新提交使用：`<type>(<scope>): <中文说明>`。一个提交只表达一个可审计意图。
