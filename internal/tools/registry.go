// Package tools 管理 Cairn 的固定工具注册表，刻意不提供动态发现或插件加载入口。
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/fxbin/cairn/internal/ports"
)

// Result 是工具调用返回给 Agent 并可写入事件日志的标准结果。
type Result struct {
	OK        bool            `json:"ok"`
	Code      string          `json:"code"`
	Data      json.RawMessage `json:"data,omitempty"`
	Preview   string          `json:"preview,omitempty"`
	Truncated bool            `json:"truncated"`
	Bytes     int64           `json:"bytes"`
	SHA256    string          `json:"sha256,omitempty"`
}

// Tool 定义固定工具的模型声明和同步执行契约。
type Tool interface {
	Spec() ports.ToolSpec
	Execute(ctx context.Context, raw json.RawMessage) Result
}

// SideEffectClass 对工具可能造成的副作用进行保守分类。
type SideEffectClass string

const (
	SideEffectNone      SideEffectClass = "none"
	SideEffectWorkspace SideEffectClass = "workspace"
	SideEffectProcess   SideEffectClass = "process"
)

// SideEffectTool 由具有副作用分类的工具实现。
type SideEffectTool interface {
	SideEffect() SideEffectClass
}

// Registry 按注册顺序保存名称唯一的固定工具。
type Registry struct {
	tools map[string]Tool
	order []string
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
	return specs
}

// Execute 查找具名工具并执行；未知工具会返回结构化失败结果。
func (registry *Registry) Execute(ctx context.Context, name string, arguments json.RawMessage) Result {
	tool, exists := registry.tools[name]
	if !exists {
		return Failure("unknown_tool", "unknown tool: "+name)
	}
	return tool.Execute(ctx, arguments)
}

// SideEffect 返回具名工具的副作用分类，未知或未分类工具保守视为无副作用。
func (registry *Registry) SideEffect(name string) SideEffectClass {
	tool, exists := registry.tools[name]
	if !exists {
		return SideEffectNone
	}
	if classified, ok := tool.(SideEffectTool); ok {
		return classified.SideEffect()
	}
	return SideEffectNone
}

// Failure 创建不含原始数据的结构化工具失败结果。
func Failure(code, preview string) Result { return Result{Code: code, Preview: preview} }

// Success 将结果数据序列化为 JSON 并创建结构化成功结果。
func Success(data any, preview string, bytes int64, digest string, truncated bool) Result {
	encoded, err := json.Marshal(data)
	if err != nil {
		return Failure("internal", err.Error())
	}
	return Result{OK: true, Code: "ok", Data: encoded, Preview: preview, Bytes: bytes, SHA256: digest, Truncated: truncated}
}
