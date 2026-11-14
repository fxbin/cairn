// tool_capabilities.go 将可选工具能力与最小工具端口分离。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

// mcpToolCapability 是外部工具目录、授权身份和资源生命周期的可选端口。
// 实现方须提供完整契约，避免出现仅支持执行而不支持结果核验的半接线状态。
type mcpToolCapability interface {
	ExecuteWithMCPIdentity(context.Context, string, json.RawMessage, string) contract.ToolResult
	AcceptMCPResult(string, contract.ToolResult) error
	MCPNeedsQuiescence() bool
	MCPDeclarationFingerprint(string) (string, bool)
	ResetMCPProjection()
}

// skillContextCapability 是技能激活与上下文恢复的可选端口。
type skillContextCapability interface {
	PrepareSkills(string)
	AcceptSkillResult(string, contract.ToolResult) error
	ActiveSkills() []contract.SkillContent
}

// mcpCapability 仅在注册表实现完整扩展契约时返回有效端口。
func (loop *Loop) mcpCapability() mcpToolCapability {
	capability, _ := loop.Tools.(mcpToolCapability)
	return capability
}

// skillCapability 仅在注册表实现完整扩展契约时返回有效端口。
func (loop *Loop) skillCapability() skillContextCapability {
	capability, _ := loop.Tools.(skillContextCapability)
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

// resetMCPProjection 在可用时清空外部目录的派生投影。
func (loop *Loop) resetMCPProjection() {
	if capability := loop.mcpCapability(); capability != nil {
		capability.ResetMCPProjection()
	}
}

// mcpDeclarationFingerprint 获取外部工具声明的当前身份。
func (loop *Loop) mcpDeclarationFingerprint(name string) (string, bool) {
	if capability := loop.mcpCapability(); capability != nil {
		return capability.MCPDeclarationFingerprint(name)
	}
	return "", false
}

// acceptMCPResult 只允许具备完整扩展端口的注册表接纳目录结果。
// 缺少端口时拒绝成功目录回执，不能静默遗失可恢复的外部状态。
func (loop *Loop) acceptMCPResult(name string, result contract.ToolResult) error {
	if capability := loop.mcpCapability(); capability != nil {
		return capability.AcceptMCPResult(name, result)
	}
	if result.OK && (name == "mcp_discover" || name == "tool_search") {
		return errors.New("MCP capability is required to restore tool catalog")
	}
	return nil
}

// prepareSkillContext 在具备技能能力时装载当前任务的可见性条件。
func (loop *Loop) prepareSkillContext(task string) {
	if capability := loop.skillCapability(); capability != nil {
		capability.PrepareSkills(task)
	}
}

// acceptSkillResult 仅允许完整技能能力接纳成功的激活回执。
func (loop *Loop) acceptSkillResult(name string, result contract.ToolResult) error {
	if capability := loop.skillCapability(); capability != nil {
		return capability.AcceptSkillResult(name, result)
	}
	if name == "skill" && result.OK {
		return errors.New("skill capability is required to restore activated skills")
	}
	return nil
}

// activeSkillContents 只返回已由扩展能力确认激活的历史正文。
func (loop *Loop) activeSkillContents() []contract.SkillContent {
	if capability := loop.skillCapability(); capability != nil {
		return capability.ActiveSkills()
	}
	return nil
}

// acceptCapabilityResults 在操作结果持久化后统一交给已装配的扩展端口核验。
func (loop *Loop) acceptCapabilityResults(name string, result contract.ToolResult) error {
	if err := loop.acceptMCPResult(name, result); err != nil {
		return err
	}
	return loop.acceptSkillResult(name, result)
}
