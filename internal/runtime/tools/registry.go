// Package tools 管理 Cairn 的内置工具与显式装配的 MCP 客户端目录。
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// Tool 定义固定工具的模型声明和同步执行契约。
type Tool interface {
	Spec() ports.ToolSpec
	Execute(ctx context.Context, raw json.RawMessage) contract.ToolResult
}

// SideEffectTool 由具有副作用分类的工具实现。
type SideEffectTool interface {
	SideEffect() contract.SideEffectClass
}

// Registry 按注册顺序保存名称唯一的固定工具。
type Registry struct {
	tools map[string]Tool
	order []string
	mcp   MCPCatalog
}

// NewRegistry 校验工具名称唯一性后创建不可动态扩展的注册表。
func NewRegistry(available ...Tool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]Tool, len(available))}
	for _, tool := range available {
		if tool == nil {
			return nil, fmt.Errorf("tool registry cannot contain nil")
		}
		name := tool.Spec().Name
		if name == "" {
			return nil, fmt.Errorf("tool registry cannot contain unnamed tool")
		}
		if _, exists := registry.tools[name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", name)
		}
		registry.tools[name] = tool
		registry.order = append(registry.order, name)
	}
	return registry, nil
}

// Specs 按注册顺序返回供模型选择的工具定义。
func (registry *Registry) Specs() []ports.ToolSpec {
	specs := make([]ports.ToolSpec, 0, len(registry.order))
	for _, name := range registry.order {
		specs = append(specs, registry.tools[name].Spec())
	}
	if registry.mcp != nil {
		specs = append(specs, registry.mcp.Specs()...)
	}
	return specs
}

// toolRegistryFingerprintPrefix 是固定工具注册表身份的版本化域前缀。
const toolRegistryFingerprintPrefix = "cairn-tool-registry-v1"

// Fingerprint 以版本化前缀对注册顺序与全部工具规格计算规范化指纹：名称、
// 描述与参数 schema 任一漂移都会改变身份。评测冻结合同用它钉住模型可见的
// 工具面，保证不同 Run 面对的工具集合逐位一致。
func (registry *Registry) Fingerprint() (string, error) {
	return canonicaljson.Fingerprint(toolRegistryFingerprintPrefix, struct {
		Tools []ports.ToolSpec `json:"tools"`
	}{Tools: registry.Specs()})
}

// Names 按注册顺序返回模型可见工具名（含 MCP 目录），供子代理收窄与诊断投影使用。
func (registry *Registry) Names() []string {
	names := make([]string, 0, len(registry.order))
	names = append(names, registry.order...)
	if registry.mcp != nil {
		for _, spec := range registry.mcp.Specs() {
			names = append(names, spec.Name)
		}
	}
	return names
}

// Execute 查找具名工具并执行；未知工具会返回结构化失败结果。
func (registry *Registry) Execute(ctx context.Context, name string, arguments json.RawMessage) contract.ToolResult {
	tool, exists := registry.tools[name]
	if !exists {
		if registry.mcp != nil {
			return registry.mcp.Execute(ctx, name, arguments)
		}
		return contract.ToolFailure("unknown_tool", "unknown tool: "+name)
	}
	return tool.Execute(ctx, arguments)
}

// SideEffect 返回具名工具的副作用分类，未知或未分类工具保守视为无副作用。
func (registry *Registry) SideEffect(name string) contract.SideEffectClass {
	tool, exists := registry.tools[name]
	if !exists {
		if registry.mcp != nil {
			return registry.mcp.SideEffect(name)
		}
		return contract.SideEffectNone
	}
	if classified, ok := tool.(SideEffectTool); ok {
		return classified.SideEffect()
	}
	return contract.SideEffectNone
}

// ReadonlyToolNames 返回固定工具中副作用为 none 的只读工具名集合。
func (registry *Registry) ReadonlyToolNames() map[string]bool {
	return ReadonlyToolNames(registry)
}
