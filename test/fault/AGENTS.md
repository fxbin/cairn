# test/fault/
> L2 | Parent: ../../AGENTS.md

显式 faultinject 故障回归。不进入默认测试构建。

## 成员

AGENTS.md: 模块职责地图与成员入口。
fault_test.go: 运行 Journal 持久化边界故障回归。已 started 副作用先对账。
workflow_fault_test.go: 工作流悬挂副作用回归。恢复不重发或推进后续步骤。

[PROTOCOL]: Update this header on change, then check AGENTS.md
