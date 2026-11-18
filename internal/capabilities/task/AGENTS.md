# internal/capabilities/task/
> L2 | Parent: ../../../AGENTS.md

Task 定义跨会话生命周期和分配身份。应用层连接独立的持久化和 Journal。

AGENTS.md: Task 的模块地图。
task.go: 冻结任务目标、验收条件、进度与证据引用；校验迁移及文本上界。
sqlite/: 以版本比较持久化任务事实，原子提交分配。

[PROTOCOL]: Update this header on change, then check AGENTS.md
