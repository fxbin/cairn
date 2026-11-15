# internal/capabilities/memory/storage/sqlite/
> L2 | Parent: ../../AGENTS.md

能力持久化叶子。事务、迁移和不可变来源关系只在本包闭合。
只读打开不迁移、不修复或改变文件权限。

## 成员

AGENTS.md: 职责地图与完整成员入口。
candidates.go: 候选捕获、召回、撤销与审计校验。
consolidation.go: 批量巩固、派生替代和来源级联撤销。
consolidation_fixture_test.go: 真实巩固与历史v2库夹具。
consolidation_migration_test.go: 历史v2拒绝、升级原子性与不变字节回归。
consolidation_read.go: 派生读取、资格、历史解释与读回。
consolidation_test.go: 事务、派生血缘、TTL和失败回滚回归。
consolidation_validate.go: 写入前重算和派生结构不变量校验。
evaluation.go: 唯一评测启动、结论提交与读回。
evaluation_migration_test.go: 评测迁移和注册表闭合形状回归。
evaluation_transaction_test.go: 唯一评测身份与事务生命周期回归。
evolution.go: 显式创建、拒绝、作废和到期。
evolution_fixture_test.go: 演化来源、冻结时钟与定向篡改夹具。
evolution_invariants.go: 演化状态、时间线和冷却一致性校验。
evolution_read.go: 演化来源、候选、冷却和精确读回。
evolution_readback_test.go: 演化写集读回失配的零残留回归。
evolution_test.go: 演化资格、冷却和终态一致性回归。
migration.go: 耐久性配置、迁移及不可变校验和。
promotion.go: 晋升版本、沙箱激活与回退事务。
promotion_test.go: 人工晋升、版本、激活及回退隔离回归。
store.go: 私有连接、只读打开与历史预检。
store_test.go: 捕获、只读边界、私有权限与来源损坏回归。
telemetry.go: 记忆侧聚合采集与只读快照。
testdata/: 原有历史v2数据库和不可变来源证明。

[PROTOCOL]: Update this header on change, then check AGENTS.md
