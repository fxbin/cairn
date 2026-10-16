// Package tools 为显式 MCP 客户端提供目录与持久化结果的窄边界。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/fxbin/cairn/internal/ports"
)

// MCPProposal 是不产生连接或调用的本地授权投影；身份来自已持久化目录。
type MCPProposal struct {
	Server            string          `json:"server"`
	Identity          string          `json:"identity"`
	EnvironmentDigest string          `json:"environment_digest,omitempty"`
	CatalogDigest     string          `json:"catalog_digest,omitempty"`
	ToolDigest        string          `json:"tool_digest,omitempty"`
	ToolName          string          `json:"tool_name"`
	Arguments         json.RawMessage `json:"arguments"`
	Effect            string          `json:"effect"`
	Stdio             bool            `json:"stdio"`
	Local             bool            `json:"local"`
	Summary           string          `json:"summary"`
}

// MCPCatalog 由 Composition Root 显式提供；成功结果落盘后才能激活新目录。
type MCPCatalog interface {
	Specs() []ports.ToolSpec
	DeclarationFingerprint(string) (string, bool)
	Execute(context.Context, string, json.RawMessage) Result
	ExecuteWithIdentity(context.Context, string, json.RawMessage, string) Result
	Proposal(string, json.RawMessage) (MCPProposal, bool, error)
	SideEffect(string) SideEffectClass
	AcceptResult(string, Result) error
	ResetProjection()
	NeedsQuiescence() bool
	ModelResult(string, Result) Result
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
			if strings.HasPrefix(name, "mcp__") || name == MCPQuiesceTool {
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
	if registry == nil || registry.mcp == nil {
		return MCPProposal{}, false, nil
	}
	return registry.mcp.Proposal(name, arguments)
}

// AcceptMCPResult 仅在相应工具结果已落盘后激活目录或模型可见工具。
func (registry *Registry) AcceptMCPResult(name string, result Result) error {
	if registry.mcp == nil {
		return nil
	}
	return registry.mcp.AcceptResult(name, result)
}

// ModelResult 从目录证据生成精简的模型视图，Journal 仍保留完整发现快照。
func (registry *Registry) ModelResult(name string, result Result) Result {
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

// MCPQuiesceTool 是运行期资源回收入口，不作为模型工具声明或通用命令暴露。
const MCPQuiesceTool = "mcp_close"

// MCPNeedsQuiescence 只查询本次 Boot 是否仍持有外部会话，不产生副作用。
func (registry *Registry) MCPNeedsQuiescence() bool {
	return registry.mcp != nil && registry.mcp.NeedsQuiescence()
}

// ExecuteWithMCPIdentity 将策略已授权的认证摘要传到传输，普通内置工具保持原有执行入口。
func (registry *Registry) ExecuteWithMCPIdentity(ctx context.Context, name string, raw json.RawMessage, identity string) Result {
	if identity != "" && registry.mcp != nil {
		return registry.mcp.ExecuteWithIdentity(ctx, name, raw, identity)
	}
	return registry.Execute(ctx, name, raw)
}
