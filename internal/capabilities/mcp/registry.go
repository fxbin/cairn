// [INPUT]: 显式静态工具端口和同一 MCP 目录实例。
// [OUTPUT]: 提供组合规格、授权执行、历史投影与生命周期元数据。
// [POS]: MCP 能力包装基础工具，不反向依赖 Runtime 或 Core。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package mcp 组合固定工具和受审计的 MCP 目录，不隐式发现客户端。
// Author: fxbin
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// StaticRegistry 是目录组合实际消费的固定工具端口；不会创建外部连接。
type StaticRegistry interface {
	Specs() []ports.ToolSpec
	Names() []string
	Execute(context.Context, string, json.RawMessage) contract.ToolResult
	SideEffect(string) contract.SideEffectClass
	ReadonlyToolNames() map[string]bool
	RequiresResultObserver(string) bool
	ProjectModelResult(string, contract.ToolResult) (contract.ToolResult, bool)
}

// Catalog 由 Composition Root 显式提供；成功结果落盘后才能激活新目录。
type Catalog interface {
	Specs() []ports.ToolSpec
	DeclarationFingerprint(string) (string, bool)
	Execute(context.Context, string, json.RawMessage) contract.ToolResult
	ExecuteWithIdentity(context.Context, string, json.RawMessage, string) contract.ToolResult
	Proposal(string, json.RawMessage) (contract.MCPProposal, bool, error)
	SideEffect(string) contract.SideEffectClass
	AcceptResult(string, contract.ToolResult) error
	ResetProjection()
	NeedsQuiescence() bool
	ModelResult(string, contract.ToolResult) contract.ToolResult
	Close() error
}

// Registry 将固定工具和目录组合为本次运行的同一能力实例。
// 静态名称所有权在构造时冻结，动态目录只由成功回执更新。
type Registry struct {
	static  StaticRegistry
	catalog Catalog
	native  map[string]bool
}

// NewRegistry 拒绝缺失依赖和名称碰撞，构造期间不连接服务器。
func NewRegistry(static StaticRegistry, catalog Catalog) (*Registry, error) {
	if static == nil || catalog == nil {
		return nil, errors.New("MCP tool provider requires a static registry and catalog")
	}
	registry := &Registry{static: static, catalog: catalog, native: make(map[string]bool)}
	for _, name := range static.Names() {
		if strings.HasPrefix(name, "mcp__") || name == contract.MCPQuiesceTool {
			return nil, errors.New("native tool name collides with MCP namespace")
		}
		registry.native[name] = true
	}
	for _, spec := range catalog.Specs() {
		if registry.native[spec.Name] {
			return nil, errors.New("native tool name collides with MCP helper")
		}
	}
	return registry, nil
}

// Specs 以原注册顺序追加当前已激活目录，保留模型声明和指纹输入。
func (registry *Registry) Specs() []ports.ToolSpec {
	specs := append([]ports.ToolSpec{}, registry.static.Specs()...)
	return append(specs, registry.catalog.Specs()...)
}

// Names 只返回模型可见名称，内部释放工具不进入该列表。
func (registry *Registry) Names() []string {
	names := append([]string{}, registry.static.Names()...)
	for _, spec := range registry.catalog.Specs() {
		names = append(names, spec.Name)
	}
	return names
}

// toolRegistryFingerprintPrefix 保留既有组合工具面的版本化指纹域。
const toolRegistryFingerprintPrefix = "cairn-tool-registry-v1"

// Fingerprint 使用原 Tools 字段与规范编码，防止迁移改变冻结身份。
func (registry *Registry) Fingerprint() (string, error) {
	return canonicaljson.Fingerprint(toolRegistryFingerprintPrefix, struct {
		Tools []ports.ToolSpec `json:"tools"`
	}{Tools: registry.Specs()})
}

// Execute 先按冻结名称选择固定工具，其余调用交给同一目录。
func (registry *Registry) Execute(ctx context.Context, name string, raw json.RawMessage) contract.ToolResult {
	if registry.native[name] {
		return registry.static.Execute(ctx, name, raw)
	}
	return registry.catalog.Execute(ctx, name, raw)
}

// SideEffect 保留固定工具与外部调用原有副作用分类。
func (registry *Registry) SideEffect(name string) contract.SideEffectClass {
	if registry.native[name] {
		return registry.static.SideEffect(name)
	}
	return registry.catalog.SideEffect(name)
}

// ReadonlyToolNames 仅透传固定工具集合，不授予外部工具并行资格。
func (registry *Registry) ReadonlyToolNames() map[string]bool {
	return registry.static.ReadonlyToolNames()
}

// RequiresResultObserver 对目录保留命名空间失败关闭，固定工具沿用通用元数据。
func (registry *Registry) RequiresResultObserver(name string) bool {
	if registry.native[name] {
		return registry.static.RequiresResultObserver(name)
	}
	return name == "mcp_discover" || name == "tool_search" || name == contract.MCPQuiesceTool || strings.HasPrefix(name, "mcp__")
}

// ModelResult 优先使用固定工具认领的投影，其余保留目录原有模型视图。
func (registry *Registry) ModelResult(name string, result contract.ToolResult) contract.ToolResult {
	if projected, claimed := registry.static.ProjectModelResult(name, result); claimed {
		return projected
	}
	return registry.catalog.ModelResult(name, result)
}

// MCPDeclarationFingerprint 只投影当前可见工具的定义身份，不解析环境或建立连接。
func (registry *Registry) MCPDeclarationFingerprint(name string) (string, bool) {
	if registry.catalog == nil {
		return "", false
	}
	return registry.catalog.DeclarationFingerprint(name)
}

// Proposal 提供能力侧的本地授权投影；不连接外部服务器。
func (registry *Registry) Proposal(name string, arguments json.RawMessage) (contract.MCPProposal, bool, error) {
	if registry == nil || registry.catalog == nil {
		return contract.MCPProposal{}, false, nil
	}
	return registry.catalog.Proposal(name, arguments)
}

// AcceptMCPResult 仅在相应工具结果已落盘后激活目录或模型可见工具。
func (registry *Registry) AcceptMCPResult(name string, result contract.ToolResult) error {
	if registry.catalog == nil {
		return nil
	}
	return registry.catalog.AcceptResult(name, result)
}

// ResetMCPProjection 在冷回放前清空派生目录，防止已有加载状态污染历史模型声明。
func (registry *Registry) ResetMCPProjection() {
	if registry.catalog != nil {
		registry.catalog.ResetProjection()
	}
}

// NeedsQuiescence 只查询本次 Boot 是否仍持有外部会话，不产生副作用。
func (registry *Registry) NeedsQuiescence() bool {
	return registry.catalog != nil && registry.catalog.NeedsQuiescence()
}

// ExecuteWithBinding 将策略已授权的认证摘要传到传输，普通内置工具保持原有执行入口。
func (registry *Registry) ExecuteWithBinding(ctx context.Context, name string, raw json.RawMessage, identity string) contract.ToolResult {
	if identity != "" && registry.catalog == nil {
		return contract.ToolFailure("authorized_execution_unavailable", "authorized tool execution is not configured")
	}
	if identity != "" && registry.catalog != nil {
		return registry.catalog.ExecuteWithIdentity(ctx, name, raw, identity)
	}
	return registry.Execute(ctx, name, raw)
}

// RequiresRequestScope 始终要求绑定模型请求声明，空指针包装也不能降级。
func (*Registry) RequiresRequestScope() bool { return true }

// RequiresAuthorizedExecution 始终要求授权绑定端口，不以缺失目录关闭安全要求。
func (*Registry) RequiresAuthorizedExecution() bool { return true }

// RequiredLifecycleTools 声明外部资源必须具备同名释放参与者；查询不建立连接。
func (*Registry) RequiredLifecycleTools() []string {
	return []string{contract.MCPQuiesceTool}
}
