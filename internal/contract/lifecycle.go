// [INPUT]: 能力侧固定释放提议，不携带执行回调。
// [OUTPUT]: 提供内部资源释放的纯数据合同。
// [POS]: Core 与能力共享的审计操作输入，不改变事件 schema。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package contract 提供资源生命周期的纯释放提议。
// Author: fxbin
package contract

import "encoding/json"

// LifecycleOperation 绑定固定释放工具、原始参数和结果不确定时的说明。
// 查询与分类不执行操作；宿主负责完整授权、Journal 和对账链。
type LifecycleOperation struct {
	ToolName               string
	Arguments              json.RawMessage
	CallIDPrefix           string
	PendingOperationReason string
	DeniedReason           string
	UnconfirmedReason      string
}
