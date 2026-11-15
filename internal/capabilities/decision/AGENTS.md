# internal/capabilities/decision/
> L2 | Parent: ../../../AGENTS.md

项目决策的受限合同与进程内账本。
证据只保留定位；外部解析器核对事实源。
当前无运行时消费方；纯投影不等于模型已接线。

## 成员

AGENTS.md: 能力边界与完整成员入口。
decision.go: 决策记录、快照复制、作用域指纹与状态迁移合同。
ledger.go: 候选、人工确认、撤销替代与独立限量查询快照。历史保留审计。
context.go: 过滤有效记录并生成有界独立投影。不自动注入模型。
evidence.go: 证据定位校验及外部解析报告。不复制证据正文。
text.go: 凭据、私有路径、标识与文本预算的拒绝规则。
decision_test.go: 正文隐私、指纹范围、TTL与状态拒绝回归。
ledger_test.go: 人工来源、主体隔离、历史、证据与并发回归。
ledger_snapshot_test.go: 账本出口、限量尾部与纯投影的引用隔离回归。
ab_eval_test.go: 合成事实下的信息与引用对照。不证明模型收益。

[PROTOCOL]: Update this header on change, then check AGENTS.md
