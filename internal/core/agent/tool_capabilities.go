// [INPUT]: 工具注册表的可选授权执行与资源端口。
// [OUTPUT]: 传递外部授权身份并查询会话占用。
// [POS]: Core 的 MCP 执行过渡适配，投影已独立。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// tool_capabilities.go 将可选工具能力与最小工具端口分离。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

// mcpToolCapability 是外部工具授权身份与资源生命周期的过渡端口。
// 实现方须提供完整契约，避免出现仅支持执行而不支持结果核验的半接线状态。
type mcpToolCapability interface {
	ExecuteWithMCPIdentity(context.Context, string, json.RawMessage, string) contract.ToolResult
	MCPNeedsQuiescence() bool
}

// mcpCapability 仅在注册表实现完整扩展契约时返回有效端口。
func (loop *Loop) mcpCapability() mcpToolCapability {
	capability, _ := loop.Tools.(mcpToolCapability)
	return capability
}

// executeToolWithIdentity 在存在外部工具能力时保留授权身份；缺少扩展时
// 仅允许执行普通工具，禁止把外部调用降级成未经身份约束的基础执行。
func (loop *Loop) executeToolWithIdentity(ctx context.Context, name string, arguments json.RawMessage, identity string) contract.ToolResult {
	if capability := loop.mcpCapability(); capability != nil {
		return capability.ExecuteWithMCPIdentity(ctx, name, arguments, identity)
	}
	if identity != "" || strings.HasPrefix(name, "mcp__") || name == contract.MCPQuiesceTool {
		return contract.ToolFailure("mcp_unavailable", "MCP capability is not configured")
	}
	return loop.Tools.Execute(ctx, name, arguments)
}

// mcpNeedsQuiescence 仅在外部工具能力实际装配时检查资源占用。
func (loop *Loop) mcpNeedsQuiescence() bool {
	capability := loop.mcpCapability()
	return capability != nil && capability.MCPNeedsQuiescence()
}
