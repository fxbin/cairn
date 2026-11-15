# internal/capabilities/workflow/storage/sqlite/
> L2 | Parent: ../../AGENTS.md

SQLite 叶子存储完整工作流链。原 schema、哈希编码与 CAS 不变。
App 显式选择可写或只读打开，并关闭连接。

## 成员

AGENTS.md: 模块职责地图与成员入口。
store.go: 单连接 SQLite、迁移校验、CAS 追加与只读完整性检查。
store_test.go: 持久化、只读不建库、链头冲突与损坏拒绝回归。

[PROTOCOL]: Update this header on change, then check AGENTS.md
