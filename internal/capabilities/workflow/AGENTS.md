# internal/capabilities/workflow/
> L2 | Parent: ../../../AGENTS.md

显式工作流能力。模板修订和事件链独立于 Agent Run。
App 仅通过显式命令连接运行；Scheduler 尚未接线。

## 成员

AGENTS.md: 模块职责地图与成员入口。
definition.go: 定义线性节点、前序制品依赖与返工区间。宿主保存时冻结修订。
events.go: 冻结事件编码、哈希链与顺序投影。独立于运行 Journal。
events_test.go: 顺序执行、未决对账、非法事件与链损坏回归。
schedule.go: 内存调度合同与触发记录。无运行接线，不执行宿主 cron。
schedule_test.go: 触发、错过策略、运行缺失、并发与所有者回归。
step.go: 步骤状态机与恢复位置。不确定副作用先对账。
step_test.go: 步骤迁移、取消、恢复与重复身份回归。
storage/: 能力内的持久化叶子入口。

[PROTOCOL]: Update this header on change, then check AGENTS.md
