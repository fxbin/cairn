# internal/capabilities/task/sqlite/
> L2 | Parent: ../AGENTS.md

SQLite 持久化任务与分配。读事务返回同一版本的快照和分配历史。

AGENTS.md: Task SQLite 的模块地图。
store.go: 管理版本化 schema、私有连接及版本比较事务；分配只追加。

[PROTOCOL]: Update this header on change, then check AGENTS.md
