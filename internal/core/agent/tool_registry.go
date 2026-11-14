// tool_registry.go 定义单 Agent Loop 的最小工具端口，具体工具由外部实现。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// ToolRegistry 仅定义单 Agent Loop 必需的工具能力；高级能力通过可选端口接入。
type ToolRegistry interface {
	// Specs 返回模型可见的工具声明。
	Specs() []ports.ToolSpec
	// Execute 执行具名工具调用。
	Execute(ctx context.Context, name string, arguments json.RawMessage) contract.ToolResult
	// SideEffect 返回工具副作用的保守分类。
	SideEffect(name string) contract.SideEffectClass
	// ModelResult 将工具结果投影为模型可见结果。
	ModelResult(name string, result contract.ToolResult) contract.ToolResult
	// ReadonlyToolNames 返回静态工具的只读集合，供并行计划使用。
	ReadonlyToolNames() map[string]bool
}
