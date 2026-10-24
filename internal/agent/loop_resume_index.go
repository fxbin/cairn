// 恢复用事件索引与工具结果配对。
// Author: fxbin
package agent

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

func completedModelResponseForTurn(events []domain.Event, turnID domain.TurnID) (modelResponsePayload, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventModelResponseFinished || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload modelResponsePayload
		return payload, json.Unmarshal(event.Payload, &payload) == nil
	}
	return modelResponsePayload{}, false
}

// toolEventIndex 是一次事件遍历重建的工具提议/结果索引；恢复路径多个查询
// 共用同一套解析规则，避免各 helper 分叉扫描语义。
type toolEventIndex struct {
	callIDs   map[string]bool
	proposals map[domain.OperationID]toolCallPayload
	results   map[domain.OperationID]tools.Result
}

// buildToolEventIndex 扫描工具提议与工具结果事件；解析失败的单条载荷跳过，
// 与既有恢复路径保持一致（缺记录表现为“尚未完成”而不是硬错误）。
func buildToolEventIndex(events []domain.Event) toolEventIndex {
	index := toolEventIndex{
		callIDs:   make(map[string]bool),
		proposals: make(map[domain.OperationID]toolCallPayload),
		results:   make(map[domain.OperationID]tools.Result),
	}
	for _, event := range events {
		if event.OperationID == nil && event.Type != domain.EventToolCallProposed {
			continue
		}
		switch event.Type {
		case domain.EventToolCallProposed:
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.CallID != "" {
				index.callIDs[payload.CallID] = true
			}
			if event.OperationID != nil {
				index.proposals[*event.OperationID] = payload
			}
		case domain.EventToolResultAppended:
			if event.OperationID == nil {
				continue
			}
			var result tools.Result
			if json.Unmarshal(event.Payload, &result) == nil {
				index.results[*event.OperationID] = result
			}
		}
	}
	return index
}

// proposedToolCallIDs 返回已经持久化提议的模型工具调用 ID，用于恢复一条
// 部分执行的模型响应时避免重复提议或重复执行。
func proposedToolCallIDs(events []domain.Event) map[string]bool {
	return buildToolEventIndex(events).callIDs
}

// completedResponseToolResults 汇总一条模型响应中各调用是否都已有持久化结果，
// 并确认是否至少成功写入过一个补丁。它用于恢复路径，避免在部分工具批次中
// 提前验证或重复执行已有调用。
func completedResponseToolResults(events []domain.Event, response modelResponsePayload) (complete, successfulPatch, allSucceeded bool) {
	index := buildToolEventIndex(events)
	byCallID := make(map[string]tools.Result)
	for operationID, proposal := range index.proposals {
		if result, found := index.results[operationID]; found {
			byCallID[proposal.CallID] = result
		}
	}
	if len(response.ToolCalls) == 0 {
		return true, false, true
	}
	allSucceeded = true
	for _, call := range response.ToolCalls {
		result, found := byCallID[call.ID]
		if !found {
			return false, false, false
		}
		allSucceeded = allSucceeded && result.OK
		successfulPatch = successfulPatch || call.Name == "apply_patch" && result.OK
	}
	return true, successfulPatch, allSucceeded
}

// hasAssistantMessage 判断指定轮次是否已写入用户可见的助手消息。
func hasAssistantMessage(events []domain.Event, turnID domain.TurnID) bool {
	for _, event := range events {
		if event.Type == domain.EventAssistantMessageAppended && event.TurnID != nil && *event.TurnID == turnID {
			return true
		}
	}
	return false
}

// incompleteModelAttempt 查找请求已开始但未持久化终态的模型尝试。
func incompleteModelAttempt(events []domain.Event) (domain.AttemptID, domain.TurnID, bool) {
	pending := make(map[domain.AttemptID]domain.TurnID)
	for _, event := range events {
		switch event.Type {
		case domain.EventModelRequestStarted:
			if event.AttemptID != nil && event.TurnID != nil {
				pending[*event.AttemptID] = *event.TurnID
			}
		case domain.EventModelResponseFinished, domain.EventModelRequestFailed:
			if event.AttemptID != nil {
				delete(pending, *event.AttemptID)
			}
		}
	}
	var latest domain.AttemptID
	var turn domain.TurnID
	for attempt, turnID := range pending {
		if attemptOrderAfter(attempt, latest) {
			latest, turn = attempt, turnID
		}
	}
	return latest, turn, latest != ""
}

// attemptOrderAfter 判断 attempt a 是否晚于 b。ID 形如 前缀_UnixNano_熵：
// 先按时间戳数值比较（同纳秒或时钟回拨时字典序不可靠），时间戳解析失败时
// 回退字典序以保持确定性。
func attemptOrderAfter(a, b domain.AttemptID) bool {
	aTS, aOK := attemptTimestamp(a)
	bTS, bOK := attemptTimestamp(b)
	if aOK && bOK && aTS != bTS {
		return aTS > bTS
	}
	return string(a) > string(b)
}

// attemptTimestamp 提取 attempt ID 中第二段的 UnixNano 时间戳。
func attemptTimestamp(id domain.AttemptID) (int64, bool) {
	parts := strings.Split(string(id), "_")
	if len(parts) != 3 {
		return 0, false
	}
	value, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// proposedVerificationCommand 从事件历史恢复某操作对应的验证命令提议。
func proposedVerificationCommand(events []domain.Event, operationID domain.OperationID) (verificationProposalPayload, bool) {
	for _, event := range events {
		if event.Type != domain.EventVerificationCommandProposed || event.OperationID == nil || *event.OperationID != operationID {
			continue
		}
		var payload verificationProposalPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return verificationProposalPayload{}, false
		}
		return payload, payload.VerificationID != "" && payload.CommandIndex >= 0 && json.Valid(payload.Arguments)
	}
	return verificationProposalPayload{}, false
}

// runCancelRequestedPayload 保存取消请求决定的预期终态与失败维度。
type runCancelRequestedPayload struct {
	Cause            string `json:"cause"`
	ExpectedTerminal string `json:"expected_terminal,omitempty"`
	BudgetDimension  string `json:"budget_dimension,omitempty"`
}

// repairMissingCancellationTerminal 在普通恢复路径之前收束已持久化的取消事实；
// operation_cancelled 只用于补写取消序列，绝不重新执行对应工具。
