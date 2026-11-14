// Package agent 从已持久化的成功工具证据恢复 MCP 目录和暴露投影。
// Author: fxbin
package agent

import (
	"errors"
	"strings"

	"encoding/json"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// captureMCPDeclarations 冻结本次模型请求看见的 MCP 声明，加载只影响后续请求。
func (loop *Loop) captureMCPDeclarations(specs []ports.ToolSpec) {
	loop.mcpModelDeclarations = make(map[string]string)
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "mcp__") {
			digest, found := loop.mcpDeclarationFingerprint(spec.Name)
			if found {
				loop.mcpModelDeclarations[spec.Name] = digest
			}
		}
	}
}

// bindMCPDeclaration 阻止模型在同一响应内使用新加载或已漂移的工具 schema。
// 无模型请求的显式脚本计划仍使用自身授权合同。
func (loop *Loop) bindMCPDeclaration(name string, decision AuthorizationDecision) AuthorizationDecision {
	if !strings.HasPrefix(name, "mcp__") || loop.mcpModelDeclarations == nil {
		return decision
	}
	expected, declared := loop.mcpModelDeclarations[name]
	matched := false
	if declared {
		digest, found := loop.mcpDeclarationFingerprint(name)
		matched = found && digest == expected
	}
	if !matched {
		decision.Effect = AuthorizationDeny
		decision.RuleID = "mcp_declaration_changed"
		decision.FailureCode = "mcp_not_declared"
		decision.Summary = "MCP tool was not declared with this schema in the model request; discover or search it, then call it in a subsequent request"
	}
	return decision
}

// modelToolResultJSON 为显式观察的工具生成模型视图，普通工具保持既有回放内容。
func (loop *Loop) modelToolResultJSON(name string, raw json.RawMessage) (string, error) {
	projected := false
	for _, observer := range loop.ResultObservers {
		if observer != nil && observer.HandlesToolResult(name) {
			projected = true
			break
		}
	}
	if !projected {
		return string(raw), nil
	}
	var result contract.ToolResult
	if json.Unmarshal(raw, &result) != nil {
		return "", errors.New("decode persisted projected tool result failed")
	}
	encoded, err := json.Marshal(loop.Tools.ModelResult(name, result))
	return string(encoded), err
}
