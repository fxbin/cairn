# test/interop：Cairn 与原生 Agent CLI 的兼容性研究脚手架

本目录是 issue #8（原生 Agent CLI 兼容性研究 Spike）的可复跑脚手架。它只做
**取证**，不提供任何运行时集成能力：Cairn 的生产路径不导入本包。

## 它回答什么问题

1. 如果把 Codex / Claude Code 当作外层 Harness，Cairn 需要的八项能力
   （start / stream / cancel / resume / approval / artifact / usage / error）
   和四个边界场景（会话丢失、进程中断、运行期权限提示、文件修改事件）
   能不能被满足。
2. 同一组任务在 Cairn 原生、Codex、Claude Code 三侧执行时，成功率、
   工作区副作用、审批往返、token 用量与耗时差多少。
3. 外部 CLI 升级时，会打断哪些接口。

## 复跑方式

```bash
# 能力矩阵与边界场景探测
go test ./test/interop/ -run TestCapabilityMatrix -count=1 -v

# 受控对照实验（8 个任务 × 3 侧外壳）
go test ./test/interop/ -run TestControlledExperiment -count=1 -v

# 外壳可用性前置检查
go test ./test/interop/ -run TestHarnessPlumbing -count=1 -v
```

外部 CLI 不在机器上时，相关测试按格 **skip** 而不是失败：结论必须区分
"实测"与"不可得"。当前实测版本：codex-cli 0.165.0、Claude Code 2.1.312。

机器产物写入被忽略的 `.vidt/interop/`（`matrix.md`、`experiment.json`、
`experiment.md`），本地报告写在同样被忽略的 `docs/interop/`。

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

| 文件 | 职责 |
| --- | --- |
| `doc.go` | 包文档与三条边界的声明 |
| `matrix_model.go` | 能力矩阵模型：能力行、边界场景、证据等级与渲染 |
| `matrix_probe.go` | 逐项能力探测与边界场景探测实现 |
| `stub.go` | 本地桩模型服务与三种线协议回放 |
| `harness.go` | 外壳抽象、受控进程执行器、工作区快照 |
| `harness_cairn.go` / `harness_codex.go` / `harness_claude.go` | 三侧外壳驱动与恢复入口 |
| `tasks.go` | 受控任务集（8 个任务） |
| `experiment.go` | 对照实验运行器与结果渲染 |
| `matrix_test.go` / `experiment_test.go` | 测试入口 |
