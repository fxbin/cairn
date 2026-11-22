# internal/capabilities/task/
> L2 | Parent: ../../../AGENTS.md

Task 定义跨会话生命周期和分配身份。应用层连接独立的持久化和 Journal。

AGENTS.md: Task 的模块地图。
task.go: 冻结目标、验收标准及完成证据。校验生命周期迁移。
workflow.go: 保存唯一实例的准备和确认事实。创建锚点由应用层核验。
sqlite/: 以版本比较持久化任务事实，原子提交分配。

[PROTOCOL]: Update this header on change, then check AGENTS.md
