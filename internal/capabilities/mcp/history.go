// Package mcp 从成功且相互匹配的 Journal 回执恢复 MCP 目录投影，不执行外部调用。
// Author: fxbin
package mcp

import (
	"encoding/json"
	"errors"
	"strings"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// ProjectionSource 仅提供派生目录的清空和成功结果接纳，不暴露工具执行或连接入口。
type ProjectionSource interface {
	ResetMCPProjection()
	AcceptMCPResult(string, contract.ToolResult) error
}

// History 逐事件恢复历史目录；提议和成功回执只在当前一次回放中有效。
// 调用方按 Journal 顺序单写入，并在每个事件之后自行冻结模型请求声明。
type History struct {
	source    ProjectionSource
	proposals map[domain.OperationID]string
	succeeded map[domain.OperationID]successReceipt
}

// successReceipt 只读取操作成功终态中的显式结果，失败终态不能授予目录。
type successReceipt struct {
	ToolName string              `json:"tool_name"`
	Result   contract.ToolResult `json:"result"`
}

// proposalPayload 保留历史提议的解码形状，只用工具名关联结果，不执行参数。
type proposalPayload struct {
	CallID    string          `json:"call_id"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
}

// NewHistory 绑定显式提供的目录来源，不发现能力或建立外部连接。
func NewHistory(source ProjectionSource) *History {
	return &History{source: source, proposals: make(map[domain.OperationID]string), succeeded: make(map[domain.OperationID]successReceipt)}
}

// HandlesToolResult 声明本投影接纳的 MCP 帮助、动态工具及内部回收名称。
func (*History) HandlesToolResult(name string) bool {
	return name == "mcp_discover" || name == "tool_search" || strings.HasPrefix(name, "mcp__") || name == contract.MCPQuiesceTool
}

// ObserveToolResult 转发已持久化的实时回执；权限与持久化顺序由调用方维持。
// 缺少显式目录来源时失败关闭，不能静默丢弃已成功的能力结果。
func (history *History) ObserveToolResult(name string, result contract.ToolResult) error {
	if history == nil || history.source == nil {
		return errors.New("MCP history requires a projection source")
	}
	return history.source.AcceptMCPResult(name, result)
}

// BeginReplay 清空目录和上一次回放的临时证据，不提前消费事件或冻结模型声明。
// events 由调用方随后按原始顺序逐个传给 ReplayEvent。
func (history *History) BeginReplay(_ []domain.Event) error {
	if history == nil {
		return errors.New("MCP history requires a projection source")
	}
	history.proposals = make(map[domain.OperationID]string)
	history.succeeded = make(map[domain.OperationID]successReceipt)
	if history.source == nil {
		return errors.New("MCP history requires a projection source")
	}
	history.source.ResetMCPProjection()
	return nil
}

// ReplayEvent 仅由帮助工具的成功操作与一致结果回执恢复目录；沿用原指纹域。
// 动态调用及内部回收历史不授予目录，不要求新增回执；模型请求声明交给调用方处理。
func (history *History) ReplayEvent(event domain.Event) error {
	if history == nil || history.source == nil {
		return errors.New("MCP history requires a projection source")
	}
	if event.OperationID == nil {
		return nil
	}
	switch event.Type {
	case domain.EventToolCallProposed:
		var proposal proposalPayload
		if json.Unmarshal(event.Payload, &proposal) == nil {
			history.proposals[*event.OperationID] = proposal.ToolName
		}
	case domain.EventOperationSucceeded:
		var receipt successReceipt
		if json.Unmarshal(event.Payload, &receipt) == nil {
			history.succeeded[*event.OperationID] = receipt
		}
	case domain.EventToolResultAppended:
		name := history.proposals[*event.OperationID]
		if name != "mcp_discover" && name != "tool_search" {
			return nil
		}
		var result contract.ToolResult
		if json.Unmarshal(event.Payload, &result) != nil {
			return errors.New("decode persisted MCP result failed")
		}
		if !result.OK || result.Code != "ok" {
			return nil
		}
		receipt, found := history.succeeded[*event.OperationID]
		if !found || receipt.ToolName != name || !receipt.Result.OK || receipt.Result.Code != "ok" {
			return errors.New("persisted MCP catalog has no successful operation receipt")
		}
		receiptDigest, receiptErr := canonicaljson.Fingerprint("cairn-mcp-result-v1", receipt.Result)
		resultDigest, resultErr := canonicaljson.Fingerprint("cairn-mcp-result-v1", result)
		if receiptErr != nil || resultErr != nil || receiptDigest != resultDigest {
			return errors.New("persisted MCP result does not match its operation receipt")
		}
		return history.ObserveToolResult(name, result)
	}
	return nil
}
