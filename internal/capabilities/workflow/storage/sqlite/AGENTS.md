# internal/capabilities/workflow/storage/sqlite/
> L2 | Parent: ../../AGENTS.md

SQLite 保存工作流链与定义修订索引。两者分别比较各自链头。
App 显式选择可写或只读打开，并关闭连接。

## 成员

AGENTS.md: 模块职责地图与成员入口。
definition.go: 追加定义修订索引。记录正文摘要，校验预期修订。
store.go: 单连接 SQLite、迁移校验、CAS 追加与只读完整性检查。已观察取消先于解码，不代表内容损坏。
scan_cancellation_test.go: 真实查询和已观察取消窗口回归。字段损坏仍拒绝。
store_test.go: 持久化、只读不建库、链头冲突与损坏拒绝回归。

[PROTOCOL]: Update this header on change, then check AGENTS.md
