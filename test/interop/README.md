# test/interop：Cairn 与原生 Agent CLI 的兼容性研究脚手架

本目录是 issue #8（原生 Agent CLI 兼容性研究 Spike）的可复跑脚手架。它只做
**取证**，不提供任何运行时集成能力：Cairn 的生产路径不导入本包。

## 两档怎么跑

| 档位 | 命令 | 依赖 | 用途 |
| --- | --- | --- | --- |
| 默认档 | `go test ./test/interop/ -count=1` | 无外部依赖，秒级 | 桩模型协议、矩阵模型与标注规则、harness 抽象、Cairn 侧外壳行为 |
| 标签档 | `go test ./test/interop/ -tags interop -count=1` | 需装 `codex` 与 `claude` | 完整能力矩阵、边界场景探测、受控对照实验 |

标签档在外部 CLI 缺失时**按列 skip 而不是失败**：该列在矩阵里被显式标为
"不可得"并写明原因，其余列照常测。当前实测版本：codex-cli 0.165.0、
Claude Code 2.1.312。

机器产物写入被忽略的 `.vidt/interop/`（`matrix.md`、`experiment.json`、
`experiment.md`），本地报告写在同样被忽略的 `docs/interop/`。

## 为什么标签档不进默认门禁

两个原因，都是实测出来的：

1. **外部 CLI 依赖**。完整矩阵与对照实验要真实拉起 `codex` / `claude`
   进程；机器上没装这两个 CLI 时，这些用例没有可测对象。
2. **进程负载**。标签档会连续拉起几十个外部 CLI 进程。实测把默认标签
   全量门禁从约 30 秒抬到约 233 秒，并且**显著提高了仓库既有的无关时序
   用例 `internal/app` 的 `TestSyncVersusAsyncDelegationOrdering` 的失败
   概率**——那个用例本身与本目录无关，但它被机器负载拖红了。

因此默认门禁只保留不依赖外部 CLI 的快用例；外部 CLI 相关的取证改为显式
标签，由需要复核兼容性时按需执行。

## 默认档覆盖什么

- **桩模型协议**：Chat Completions、Responses、Anthropic Messages 三种线
  协议的响应形态、用量块、步号推进不变量。
- **矩阵模型与标注规则**：按行取格、按 CLI 渲染、"实测/不可得"证据等级
  的强制规则（不可得必须写明原因，实测必须给出结论），以及能力与边界
  场景清单的覆盖口径。
- **harness 抽象**：受控进程执行器（退出码、stdout/stderr、超时回收、
  中断标记）、工作区快照与改动文件识别。
- **Cairn 侧外壳行为**：审批往返驱动（停在待审批态 → 取回绑定指纹 → 发起
  resume 放行 → 记录往返次数）、审批指纹提取规则、NDJSON 事件流与用量
  解析、运行标识提取。这部分用一份**假 cairn 可执行脚本**驱动，既覆盖
  驱动逻辑，又不必编译 Go 二进制，因此默认档保持秒级。

## 关键设计：桩模型而不是真实账号

为了让三侧外壳在 **零凭据、零外网** 的条件下跑同一组任务，本目录内置一个
本地桩模型服务（`stub.go`），按任务脚本确定性地回放工具调用序列：

- 同时提供 OpenAI Chat Completions、OpenAI Responses、Anthropic Messages
  三种线协议，桩模型报告的用量是确定性函数，可直接核对每个外壳是否如实
  转述提供方用量。
- Codex 通过 `model_providers` 指向桩模型；Claude Code 通过 settings 的
  `env` 块指向桩模型；Cairn 通过工作区 `.agent/config.yaml` 指向桩模型。
- 桩模型只使用占位密钥，**不读取、不记录、不落盘任何用户真实账号凭据**。

因为模型侧完全确定，对照实验比较的是 **Harness 行为**，不是模型能力。

## 不可绕过的边界

本目录固定三条边界，并在代码里体现：

1. 外部 CLI 的"完成"只记为 harness 观测，**不得**转写为 Cairn 的
   `verified`；验证可信只来自 Cairn 自己的 verifier 证据。
2. 外部工具的权限模型**不得**提升 Cairn Policy：探测只在临时目录内进行，
   外部 CLI 的授权决定不作为 Cairn 的授权输入。
3. 授权、账号与凭据**不得**从沙箱透传：桩模型使用固定占位串，隔离配置
   目录按次生成。

另外，本目录不引入默认后台进程、不提供通用插件宿主、不改变 Cairn 本地
Go Harness 的默认行为。

## 目录构成

| 文件 | 档位 | 职责 |
| --- | --- | --- |
| `doc.go` | 默认 | 包文档与三条边界的声明 |
| `matrix_model.go` | 默认 | 能力矩阵模型：能力行、边界场景、证据等级与渲染 |
| `stub.go` | 默认 | 本地桩模型服务与三种线协议回放 |
| `harness.go` | 默认 | 外壳抽象、受控进程执行器、工作区快照 |
| `harness_cairn.go` | 默认 | Cairn 原生外壳驱动与恢复入口 |
| `tasks.go` | 默认 | 受控任务集（8 个任务） |
| `experiment.go` | 默认 | 对照实验运行器与结果渲染 |
| `stub_test.go` | 默认 | 桩模型协议与步号推进测试 |
| `matrix_model_test.go` | 默认 | 矩阵渲染、标注规则、清单覆盖口径测试 |
| `harness_test.go` | 默认 | 进程执行器、工作区快照、Cairn 侧外壳行为测试 |
| `harness_codex.go` | `interop` | Codex 外壳驱动与恢复入口 |
| `harness_claude.go` | `interop` | Claude Code 外壳驱动与恢复入口 |
| `matrix_probe.go` | `interop` | 逐项能力探测与边界场景探测实现 |
| `matrix_test.go` | `interop` | 完整能力矩阵测试（按列 skip） |
| `experiment_test.go` | `interop` | 外壳可用性前置检查与受控对照实验 |
