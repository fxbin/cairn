# internal/adapters/journal/sqlite/
> L2 | Parent: ../../../../AGENTS.md

SQLite 追加式 Journal 持久化。并发追加保持 CAS 和哈希链。
取消读取不构成内容损坏。只读入口不创建或迁移数据。

## 成员

AGENTS.md: Journal 职责地图与成员入口。
journal.go: Journal 实现受事务约束的追加与读取。行解码保留取消与损坏分类。
journal_open_dsn_test.go: 验证数据库路径含空格时的原样编码。
journal_test.go: 验证持久化重放、序号冲突及取消查询。
readonly_open_test.go: 验证只读连接锁等待和取消，不创建数据库。
scan_cancellation_test.go: 真实 SQLite 行验证取消分类及损坏拒绝。

[PROTOCOL]: Update this header on change, then check AGENTS.md
