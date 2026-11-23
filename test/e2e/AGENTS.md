# test/e2e/
> L2 | Parent: ../../AGENTS.md

按 e2e 标签执行实际运行链。响应使用本地夹具。

AGENTS.md: 端到端验证入口与成员地图。
e2e_test.go: 验证工具、审批、修复与不确定副作用对账。
signal_test.go: 验证独立 CLI 进程收到信号时的持久收尾。
task_autonomy_test.go: 验证 Go 编码任务跨进程审批、返工与逐标准验收。

[PROTOCOL]: Update this header on change, then check AGENTS.md
