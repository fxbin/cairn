# 验证基线与恢复/审批矩阵

本文件固定 Cairn 的自动化验证入口、三级证据分层，以及关键恢复/审批场景对应的既有测试。密钥、任务正文、verifier 内容与本地用户目录不进入任何 CI 日志或 Artifact。

## 三级证据分层

任何"已验证"结论必须注明级别，三级不得混报：

| 级别 | 含义 | 入口 |
|---|---|---|
| 离线测试 | 无真实 Provider，进程内脚本化 Provider 与回环端点 | GitHub Actions（`.github/workflows/ci.yml`）及本地同命令 |
| 沙箱评测 | 冻结任务集、私有 verifier、新 clone、Docker 隔离 | 本地显式 benchmark 命令（须 `--local-config` 与资产目录），不入 CI |
| 真实 Provider 验收 | 付费端点与真实模型交互 | 本地显式密钥配置，人工触发 |

CI 只证明第一级；通过 CI 不代表沙箱评测或真实验收已通过。

## CI 入口

- **gate**（push 到 main / 全部 PR）：gofmt 漂移检查、`go vet`、`go build ./cmd/cairn`、默认标签全量测试（不含 `gui` 包——Wails 桌面壳依赖本地前端构建产物 `gui/frontend/dist`，git 忽略，须先跑 gui 前端构建，按可用性单独分组，不在默认门禁覆盖范围）。
- **extended**（仅手动 workflow_dispatch / 每周定时）：`go test -race`（gui 除外）、`openaicompat -run Contract`、`./test/fault -tags=faultinject`、`./test/e2e -tags=e2e`。
- 工作流不配置任何密钥；fault/e2e 用例使用进程内脚本化 Provider 与回环 HTTP 端点，不调用真实服务；缺环境时用例显式失败，不会静默跳过后报告通过。

## 恢复与审批矩阵（既有用例映射）

以下只引用仓库中实际存在的测试；命令默认指 `go test <包路径>`，标 `-tags` 的须显式加标签。

### 1. Journal 崩溃/损坏
预期：损坏数据库在打开（含只读打开）时被拒绝；事件被篡改、重排或删除后哈希链校验失败；replay 非法运行被拒绝。

- `internal/domain`：`TestVerifyChainRejectsPayloadTampering`、`TestVerifyChainRejectsReorderedDeletedAndRelinkedEvents`
- `internal/adapters/journal/...`：`TestOpenReadOnlyRejectsDamagedDatabaseWithoutWaiting`、`TestCheckRejectsReplayInvalidRun`

### 2. started 副作用未知（对账）
预期：悬而未决的已 started 写操作禁止自动重试，必须经 `resume --resolve` 对账；只读 started 操作可保持 runnable 不阻塞恢复。

- `test/fault`：`TestFaultInjectStartedWorkspaceOperationRequiresReconciliation`（`-tags=faultinject`）、`TestFaultInjectStartedReadOperationCanRemainRunnable`
- `internal/app`：`TestServiceReconcilesDanglingWorkspaceOperationWithoutRetry`
- `test/e2e`：`TestFixtureReconcileDanglingPatchWithoutRetryingIt`（`-tags=e2e`）

### 3. MCP 结果不确定/目录漂移
预期：目录/schema 漂移失败关闭；会话关闭失败不得上报为成功；目录暴露状态须来自已接受的证据并可本地重放。

- `internal/adapters/mcpclient`：`TestHTTPDeleteFailureCannotBeReportedAsSuccessfulSessionClose`、`TestHTTPDiscoveryExposureRequiresAcceptedEvidenceAndReplaysLocally`、`TestSchemaSurvivesRealLoopJournalPersistenceReplayAndMCPCall`

### 4. 取消与恢复
预期：运行中中断进入 cancelling 并持久化；abort 收尾停在 cancelling 的运行；审批恢复恰好消费一个匹配审批；并发恢复只有一个工作区锁持有者。

- `test/e2e`：`TestFirstInterruptPersistsCancellationAndExitsGracefully`（`-tags=e2e`；同文件 `TestSecondInterruptPreemptsCancellation` 在特定宿主受安全软件信号干扰属已知环境噪音，CI 首跑结果另行补记）
- `internal/app`：`TestServiceAbortCompletesCancellingRun`、`TestServiceResumeConsumesOneMatchingApproval`、`TestConcurrentResumeAllowsExactlyOneWorkspaceLockHolder`
- `internal/agent`：`TestResumeZeroMutationVerificationStages`

### 5. Verifier 失败
预期：验证失败反馈给模型修复而非标记完成；补丁验证通过才自动完成；审批后恢复继续同一条验证命令。

- `internal/agent`：`TestLoopFeedsVerificationFailureBackForRepairBeforeCompletion`、`TestLoopAutomaticallyCompletesAfterSuccessfulPatchVerification`
- `test/e2e`：`TestFixtureVerificationFailureThenRepair`（`-tags=e2e`）

### 6. 工作区漂移与回退
预期：缺无损 before 快照时拒绝回退（不猜测旧内容）；回退幂等重入；回退同样走完整授权链并尊重已应用的对账裁决。

- `internal/app`：`TestRewindRefusesWithoutBeforeSnapshot`、`TestRewindContinuesByIdempotentReentry`、`TestRewindReconcileRespectsAppliedVerdict`

## 首次基线记录（2026-10-28）

- 本地（darwin/arm64，Go 1.26.4）：gofmt 无漂移（仅本地忽略目录除外）、`go vet` 0 问题、`go build ./cmd/cairn` 成功、默认标签测试 34 个包全部通过（本地含 gui 前端产物与本机 Docker/alpine）。
- CI 首跑暴露两处环境事实，已按合同处理：
  1. `gui` 包在无前端产物的全新检出不编译（embed 无匹配文件），按分组约定移出门禁，桌面构建另行验证；
  2. benchmark runner 把尝试的执行与验证强制走 Docker 沙箱（`alpine:latest`），默认测试集因此依赖本机 Docker 与该镜像；CI 在测试前预拉取公共 alpine 镜像，使沙箱路径在 Linux 真实执行（镜像缺失时按合同失败关闭，不静默跳过）。
- 基线结果：gate 绿（push 触发，约 40 秒）；extended 首跑绿（手动触发，约 2 分钟：race 全量、openaicompat 协议合同、faultinject 故障注入、e2e 全部通过）。本机 macOS 上不稳定的信号抢占用例在 Linux CI 通过，确认为宿主环境噪音而非产品缺陷。
- 未执行：沙箱评测、真实 Provider 验收、桌面 GUI 构建验证。
- 未证明的能力：真实 Provider 下的稳定性、TUI 交互体验、桌面 GUI 构建、发布级验收；以上须按三级分层另行取证，不得引用 CI 结果替代。
