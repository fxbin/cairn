// Package agent 从已持久化的成功工具证据恢复 MCP 目录和暴露投影。
// Author: fxbin
package agent

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// mcpSuccessReceipt 只读取成功终态中的显式工具结果，不能由失败回执授予目录状态。
type mcpSuccessReceipt struct {
	ToolName string       `json:"tool_name"`
	Result   tools.Result `json:"result"`
}

// restoreMCPResults 保留 Journal 的成功回执边界，不连接或重发任何外部调用。
func (loop *Loop) restoreMCPResults(events []domain.Event) error {
	loop.Tools.ResetMCPProjection()
	loop.mcpModelDeclarations = nil
	proposals := make(map[domain.OperationID]string)
	succeeded := make(map[domain.OperationID]mcpSuccessReceipt)
	for _, event := range events {
		if event.Type == domain.EventModelRequestStarted {
			loop.captureMCPDeclarations(loop.Tools.Specs())
			continue
		}
		if event.OperationID == nil {
			continue
		}
		switch event.Type {
		case domain.EventToolCallProposed:
			var proposal toolCallPayload
			if json.Unmarshal(event.Payload, &proposal) == nil {
				proposals[*event.OperationID] = proposal.ToolName
			}
		case domain.EventOperationSucceeded:
			var payload mcpSuccessReceipt
			if json.Unmarshal(event.Payload, &payload) == nil {
				succeeded[*event.OperationID] = payload
			}
		case domain.EventToolResultAppended:
			name := proposals[*event.OperationID]
			if name != "mcp_discover" && name != "tool_search" {
				continue
			}
			var result tools.Result
			if json.Unmarshal(event.Payload, &result) != nil {
				return errors.New("decode persisted MCP result failed")
			}
			if !result.OK || result.Code != "ok" {
				continue
			}
			receipt, found := succeeded[*event.OperationID]
			if !found || receipt.ToolName != name || !receipt.Result.OK || receipt.Result.Code != "ok" {
				return errors.New("persisted MCP catalog has no successful operation receipt")
			}
			receiptDigest, receiptErr := canonicaljson.Fingerprint("cairn-mcp-result-v1", receipt.Result)
			resultDigest, resultErr := canonicaljson.Fingerprint("cairn-mcp-result-v1", result)
			if receiptErr != nil || resultErr != nil || receiptDigest != resultDigest {
				return errors.New("persisted MCP result does not match its operation receipt")
			}
			if err := loop.Tools.AcceptMCPResult(name, result); err != nil {
				return err
			}
		}
	}
	return nil
}

// captureMCPDeclarations 冻结本次模型请求看见的 MCP 声明，加载只影响后续请求。
func (loop *Loop) captureMCPDeclarations(specs []ports.ToolSpec) {
	loop.mcpModelDeclarations = make(map[string]string)
	for _, spec := range specs {
		if strings.HasPrefix(spec.Name, "mcp__") {
			digest, found := loop.Tools.MCPDeclarationFingerprint(spec.Name)
			if found {
				loop.mcpModelDeclarations[spec.Name] = digest
			}
		}
	}
}

// bindMCPDeclaration 阻止模型在同一响应内使用新加载或已漂移的工具 schema。
// 无模型请求的显式脚本计划仍使用自身授权合同。
func (loop *Loop) bindMCPDeclaration(name string, decision policy.Decision) policy.Decision {
	if !strings.HasPrefix(name, "mcp__") || loop.mcpModelDeclarations == nil {
		return decision
	}
	expected, declared := loop.mcpModelDeclarations[name]
	matched := false
	if declared {
		digest, found := loop.Tools.MCPDeclarationFingerprint(name)
		matched = found && digest == expected
	}
	if !matched {
		decision.Effect = policy.Deny
		decision.RuleID = "mcp_declaration_changed"
		decision.FailureCode = "mcp_not_declared"
		decision.Summary = "MCP tool was not declared with this schema in the model request; discover or search it, then call it in a subsequent request"
	}
	return decision
}

// modelToolResultJSON 为目录发现生成精简视图，普通工具保持既有回放内容。
func (loop *Loop) modelToolResultJSON(name string, raw json.RawMessage) (string, error) {
	if name != "mcp_discover" && name != "tool_search" {
		return string(raw), nil
	}
	var result tools.Result
	if json.Unmarshal(raw, &result) != nil {
		return "", errors.New("decode persisted MCP result failed")
	}
	encoded, err := json.Marshal(loop.Tools.ModelResult(name, result))
	return string(encoded), err
}
