// Package tools 为显式 MCP 客户端提供目录与持久化结果的窄边界。
// Author: fxbin
package tools

import (
	"context"
	"errors"
	"strings"

	"encoding/json"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// MCPCatalog 由 Composition Root 显式提供；成功结果落盘后才能激活新目录。
type MCPCatalog interface {
	Specs() []ports.ToolSpec
	DeclarationFingerprint(string) (string, bool)
	Execute(context.Context, string, json.RawMessage) contract.ToolResult
	ExecuteWithIdentity(context.Context, string, json.RawMessage, string) contract.ToolResult
	Proposal(string, json.RawMessage) (MCPProposal, bool, error)
	SideEffect(string) contract.SideEffectClass
	AcceptResult(string, contract.ToolResult) error
	ResetProjection()
	NeedsQuiescence() bool
	ModelResult(string, contract.ToolResult) contract.ToolResult
	Close() error
}

// MCPDeclarationFingerprint 只投影当前可见工具的定义身份，不解析环境或建立连接。
func (registry *Registry) MCPDeclarationFingerprint(name string) (string, bool) {
	if registry.mcp == nil {
		return "", false
	}
	return registry.mcp.DeclarationFingerprint(name)
}

// NewRegistryWithMCP 保留原有内置工具并显式连接 MCP 目录，不提供通用插件入口。
func NewRegistryWithMCP(catalog MCPCatalog, available ...Tool) (*Registry, error) {
	registry, err := NewRegistry(available...)
	if err != nil {
		return nil, err
	}
	registry.mcp = catalog
	if catalog != nil {
		for name := range registry.tools {
			if strings.HasPrefix(name, "mcp__") || name == contract.MCPQuiesceTool {
				return nil, errors.New("native tool name collides with MCP namespace")
			}
		}
		for _, spec := range catalog.Specs() {
			if _, exists := registry.tools[spec.Name]; exists {
				return nil, errors.New("native tool name collides with MCP helper")
			}
		}
	}
	return registry, nil
}

// MCPProposal 返回 MCP 的本地权限投影，供同一个 Policy Gate 判定。
func (registry *Registry) MCPProposal(name string, arguments json.RawMessage) (MCPProposal, bool, error) {
	return registry.Proposal(name, arguments)
}

// Proposal 实现 contract.MCPProposalSource 端口；语义与 MCPProposal 相同。
func (registry *Registry) Proposal(name string, arguments json.RawMessage) (MCPProposal, bool, error) {
	if registry == nil || registry.mcp == nil {
		return MCPProposal{}, false, nil
	}
	return registry.mcp.Proposal(name, arguments)
}

// AcceptMCPResult 仅在相应工具结果已落盘后激活目录或模型可见工具。
func (registry *Registry) AcceptMCPResult(name string, result contract.ToolResult) error {
	if registry.mcp == nil {
		return nil
	}
	return registry.mcp.AcceptResult(name, result)
}

// ModelResult 从目录证据生成精简的模型视图，Journal 仍保留完整发现快照。
func (registry *Registry) ModelResult(name string, result contract.ToolResult) contract.ToolResult {
	if name == "skill" {
		return skillModelResult(result)
	}
	if registry.mcp == nil {
		return result
	}
	return registry.mcp.ModelResult(name, result)
}

// ResetMCPProjection 在冷回放前清空派生目录，防止已有加载状态污染历史模型声明。
func (registry *Registry) ResetMCPProjection() {
	if registry.mcp != nil {
		registry.mcp.ResetProjection()
	}
}

// MCPNeedsQuiescence 只查询本次 Boot 是否仍持有外部会话，不产生副作用。
func (registry *Registry) MCPNeedsQuiescence() bool {
	return registry.mcp != nil && registry.mcp.NeedsQuiescence()
}

// ExecuteWithMCPIdentity 将策略已授权的认证摘要传到传输，普通内置工具保持原有执行入口。
func (registry *Registry) ExecuteWithMCPIdentity(ctx context.Context, name string, raw json.RawMessage, identity string) contract.ToolResult {
	if identity != "" && registry.mcp != nil {
		return registry.mcp.ExecuteWithIdentity(ctx, name, raw, identity)
	}
	return registry.Execute(ctx, name, raw)
}
