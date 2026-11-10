// tool_registry.go 定义 Core 对工具注册表的最小观察与执行端口；具体装配
// 由应用层注入实现，Core 不持有 tools 包的具体类型。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// ToolRegistry 是 Agent Loop 可见的工具面端口：固定工具与 MCP 目录的声明、
// 执行、副作用分类、MCP 投影回执与技能内容装载均经此窄接口收敛。
type ToolRegistry interface {
	// Specs 返回模型可见的全部工具声明。
	Specs() []ports.ToolSpec
	// Execute 执行具名工具调用。
	Execute(ctx context.Context, name string, arguments json.RawMessage) contract.ToolResult
	// ExecuteWithMCPIdentity 以授权回传的 MCP 身份执行外部工具调用。
	ExecuteWithMCPIdentity(ctx context.Context, name string, raw json.RawMessage, identity string) contract.ToolResult
	// SideEffect 返回具名工具的保守副作用分类。
	SideEffect(name string) contract.SideEffectClass
	// ModelResult 将工具结果投影为模型可见结果。
	ModelResult(name string, result contract.ToolResult) contract.ToolResult
	// AcceptMCPResult 校验并接受一次 MCP 目录投影回执。
	AcceptMCPResult(name string, result contract.ToolResult) error
	// AcceptSkillResult 校验并接受一次技能读取回执。
	AcceptSkillResult(name string, result contract.ToolResult) error
	// ActiveSkills 返回本次运行已激活的技能正文。
	ActiveSkills() []contract.SkillContent
	// PrepareSkills 按任务文本预装载匹配技能。
	PrepareSkills(task string)
	// MCPNeedsQuiescence 查询是否仍持有需回收的外部 MCP 会话。
	MCPNeedsQuiescence() bool
	// MCPDeclarationFingerprint 返回 MCP 工具声明的指纹及是否存在。
	MCPDeclarationFingerprint(name string) (string, bool)
	// ResetMCPProjection 在冷回放前清空派生 MCP 目录。
	ResetMCPProjection()
	// ReadonlyToolNames 返回固定工具中的只读工具名集合。
	ReadonlyToolNames() map[string]bool
}
