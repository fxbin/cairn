# internal/capabilities/workflow/
> L2 | Parent: ../../../AGENTS.md

显式工作流能力。模板修订和事件链独立于 Agent Run。
App 仅通过显式命令连接运行；Scheduler 尚未接线。

## 成员

AGENTS.md: 模块职责地图与成员入口。
definition.go: 冻结线性节点、验收合同、制品依赖与返工区间。
definition_test.go: 回归验收合同快照、审查边界和版本化回放。
events.go: 冻结事件编码、哈希链与顺序投影。独立于运行 Journal。
control.go: 重放暂停、继续及冻结策略容许的重试。尝试耗尽或放弃对账后禁止重派。
instance.go: 版本化尝试、运行锚点及原子人工门。检查审查节点顺序。
events_test.go: 顺序执行、未决对账、非法事件与链损坏回归。
schedule.go: 内存调度合同与触发记录。无运行接线，不执行宿主 cron。
schedule_test.go: 触发、错过策略、运行缺失、并发与所有者回归。
step.go: 步骤状态机与恢复位置。不确定副作用先对账。
step_test.go: 步骤迁移、取消、恢复与重复身份回归。
storage/: 能力内的持久化叶子入口。

[PROTOCOL]: Update this header on change, then check AGENTS.md
