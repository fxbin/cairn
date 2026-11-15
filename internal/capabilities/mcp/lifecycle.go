// [INPUT]: 本次组合根持有资源的只读占用源。
// [OUTPUT]: 提供原 MCP 释放提议与内部名称分类。
// [POS]: MCP 生命周期能力；实际审计执行留在 Core。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package mcp 提供本次 MCP 资源的固定释放提议。
// Author: fxbin
package mcp

import (
	"encoding/json"
	"github.com/fxbin/cairn/internal/contract"
)

// QuiescenceSource 只查询本次资源，不建立连接或触发释放。
type QuiescenceSource interface{ NeedsQuiescence() bool }

// Lifecycle 将同一资源源绑定到原 MCP 回收操作，提议在实例生命周期内不变。
type Lifecycle struct{ source QuiescenceSource }

// NewLifecycle 显式绑定本次资源源；缺失来源产出无效提议，由 Core 在执行前拒绝。
func NewLifecycle(source QuiescenceSource) *Lifecycle { return &Lifecycle{source: source} }

// NeedsQuiescence 只读取本次资源是否仍需释放，不触发传输。
func (lifecycle *Lifecycle) NeedsQuiescence() bool {
	return lifecycle != nil && lifecycle.source != nil && lifecycle.source.NeedsQuiescence()
}

// QuiescenceOperation 保留旧名称、调用前缀与错误说明；回放仅消费此固定分类。
func (lifecycle *Lifecycle) QuiescenceOperation() contract.LifecycleOperation {
	if lifecycle == nil || lifecycle.source == nil {
		return contract.LifecycleOperation{}
	}
	return contract.LifecycleOperation{
		ToolName: contract.MCPQuiesceTool, Arguments: json.RawMessage(`{}`), CallIDPrefix: "cairn-mcp-close-",
		PendingOperationReason: "MCP lifecycle cleanup requires the pending operation to be reconciled first",
		DeniedReason:           "MCP lifecycle cleanup must only release owned resources",
		UnconfirmedReason:      "MCP session cleanup could not be confirmed",
	}
}
