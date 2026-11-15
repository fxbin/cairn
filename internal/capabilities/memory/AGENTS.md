# internal/capabilities/memory/
> L2 | Parent: ../../../AGENTS.md

可信记忆的纯合同、受限召回工具与显式生命周期。
应用验证外部来源；记忆不授予权限或决定Run终态。

## 成员

AGENTS.md: 职责地图与完整成员入口。
consolidation.go: 确定性巩固计划与派生事实。
consolidation_test.go: 冻结输入、支持度与确定性巩固回归。
evaluation.go: 唯一评测绑定、状态和验证。
evaluation_test.go: 评测身份确定性与状态闭合回归。
evolution.go: 演化候选、来源资格与冷却合同。
evolution_gate.go: 受控评测、晋升、沙箱试运行与回退门。
evolution_test.go: 变体参数、来源与候选状态回归。
memory.go: 候选、证据定位、状态和有界保留合同。
promotion.go: 人工晋升决定、版本与显式激活合同。
promotion_gate_test.go: 晋升、沙箱、试运行与回退隔离回归。
recall.go: 有界计划和临时证据账本。
recall_test.go: 精确范围、精度、冲突与证据缺口回归。
recall_tool.go: MemoryRecall工具规格、校验和只读执行。
recall_tool_bind.go: MemoryExplainFunc实例回调合同。
storage/: 能力内的SQLite存储叶子。

[PROTOCOL]: Update this header on change, then check AGENTS.md
