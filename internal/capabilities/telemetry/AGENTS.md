# internal/capabilities/telemetry/
> L2 | Parent: ../../../AGENTS.md

可选语义观测能力。只消费成功持久化事件。
App显式装配并关闭资源；导出失败不改变运行。
OTLP scope属于外部身份，目录迁移不改其值。

## 成员

AGENTS.md: 能力责任边界与成员入口。
telemetry.go: 事件聚合、有限维度、快照与关联跨度。不携带原始正文。
runtime.go: 终态单次导出与统计回调。提供Flush和Close边界。
export.go: 唯一接线判定、OTLP编码与有界异步导出。默认关闭。
telemetry_test.go: 实时与离线一致性、采样及敏感正文过滤回归。
export_test.go: 本地接收报文、禁用零导出与队列失败隔离回归。

[PROTOCOL]: Update this header on change, then check AGENTS.md
