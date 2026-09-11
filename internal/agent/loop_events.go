package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

const malformedToolArgumentsErrorClass = "malformed_tool_arguments"

const malformedToolArgumentsFeedback = "Cairn runtime: the previous tool call was rejected because its arguments were malformed. Do not repeat it. Reissue at most one tool call with valid JSON arguments and no secrets."

// EventSink 接收 Loop 已成功追加到日志的领域事件。
type EventSink func(domain.Event)

// append 以当前投影序号追加事件、应用投影并向外部 sink 广播。
func (loop *Loop) append(ctx context.Context, projection *domain.RunProjection, draft domain.EventDraft) error {
	if err := projection.ValidateDraft(draft); err != nil {
		return fmt.Errorf("validate %s after sequence %d: %w", draft.Type, projection.LastSeq, err)
	}
	event, err := loop.Journal.Append(ctx, projection.LastSeq, draft)
	if err != nil {
		return fmt.Errorf("append %s after sequence %d: %w", draft.Type, projection.LastSeq, err)
	}
	if err := projection.Apply(event); err != nil {
		return fmt.Errorf("apply prevalidated %s after sequence %d: %w", event.Type, projection.LastSeq, err)
	}
	if event.Type == domain.EventOperationStarted {
		loop.startedOperations++
	}
	if loop.OnEvent != nil {
		loop.OnEvent(event)
	}
	return nil
}

// messagesFromEvents 将已持久化事实重建为下一次模型请求所需的对话上下文。
func (loop *Loop) messagesFromEvents(events []domain.Event, fallbackTask string) []ports.Message {
	task := fallbackTask
	priorContext := loop.PriorContext
	priorRef := loop.PriorContextRef
	checkpoint, throughSeq := latestContextCheckpoint(events)
	proposals := make(map[domain.OperationID]ports.ToolCall)
	completedCalls := make(map[string]bool)
	for _, event := range events {
		if event.Type != domain.EventRunCreated {
			continue
		}
		var payload domain.RunCreatedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Task != "" {
			task = payload.Task
			// resume 回放时链关系与制品引用来自创建事件；输入侧显式携带
			// 优先（新 run 场景），缺省时按事件回放，保持重放一致性。
			if loop.PriorContext == "" && loop.PriorContextRef == nil && len(payload.PriorContextRef) > 0 {
				var ref artifact.Ref
				if json.Unmarshal(payload.PriorContextRef, &ref) == nil {
					priorRef = &ref
					if loaded, err := loop.loadPriorContext(priorRef); err == nil {
						priorContext = loaded
					}
				}
			}
		}
	}
	for _, event := range events {
		if event.Seq <= throughSeq {
			continue
		}
		switch event.Type {
		case domain.EventToolCallProposed:
			if event.OperationID == nil {
				continue
			}
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" {
				proposals[*event.OperationID] = ports.ToolCall{ID: payload.CallID, Name: payload.ToolName, Arguments: payload.Arguments}
			}
		case domain.EventToolResultAppended:
			if event.OperationID == nil {
				continue
			}
			if call, exists := proposals[*event.OperationID]; exists {
				completedCalls[call.ID] = true
			}
		}
	}
	messages := loop.contextMessages(task, checkpoint)
	if priorContext != "" {
		// 前情块插在系统提示与任务原文之间：背景先于指令，任务语义不被
		// 前情改写（与压缩检查点的注入层级平行）。
		contextBlock := ports.Message{Role: "user", Content: sessionContextBlock(priorContext)}
		if len(messages) > 1 {
			messages = append([]ports.Message{messages[0], contextBlock}, messages[1:]...)
		} else {
			messages = append([]ports.Message{contextBlock}, messages...)
		}
	}
	for _, event := range events {
		if event.Seq <= throughSeq {
			continue
		}
		switch event.Type {
		case domain.EventModelRequestFailed:
			var payload modelRequestFailedPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ErrorClass == malformedToolArgumentsErrorClass && payload.Retryable {
				messages = append(messages, ports.Message{Role: "user", Content: malformedToolArgumentsFeedback})
			}
		case domain.EventModelResponseFinished:
			var payload modelResponsePayload
			if json.Unmarshal(event.Payload, &payload) != nil || len(payload.ToolCalls) == 0 {
				continue
			}
			complete := true
			for _, call := range payload.ToolCalls {
				if !completedCalls[call.ID] {
					complete = false
					break
				}
			}
			if complete {
				messages = append(messages, ports.Message{Role: "assistant", Content: payload.Text, ToolCalls: payload.ToolCalls})
			}
		case domain.EventToolCallProposed:
			if event.OperationID == nil {
				continue
			}
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" {
				proposals[*event.OperationID] = ports.ToolCall{ID: payload.CallID, Name: payload.ToolName, Arguments: payload.Arguments}
			}
		case domain.EventToolResultAppended:
			if event.OperationID == nil {
				continue
			}
			if call, exists := proposals[*event.OperationID]; exists {
				messages = append(messages, ports.Message{Role: "tool", ToolCallID: call.ID, Content: string(event.Payload)})
			}
		case domain.EventVerificationFeedbackAppended:
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Text != "" {
				messages = append(messages, ports.Message{Role: "user", Content: payload.Text})
			}
		}
	}
	return messages
}

// sessionContextBlock 把前情文本包装为标注来源的会话前情块；任务原文始
// 终是独立的最新用户消息，前情只提供背景不改变任务语义。
func sessionContextBlock(prior string) string {
	return "以下本会话前情供背景参考，由上一任务的事件投影生成：\n" + prior
}

// loadPriorContext 从制品库读取前情卡文本；读取失败由调用方按缺省降级处
// 理（不注入、不失败关闭——前情是增强而非合同必需）。
func (loop *Loop) loadPriorContext(ref *artifact.Ref) (string, error) {
	if loop.Artifacts == nil || ref == nil {
		return "", errors.New("artifact store unavailable")
	}
	content, err := loop.Artifacts.Open(*ref)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

// malformedToolArgumentsFailureCount 从持久化模型失败中统计畸形工具参数次数，
// 用于在恢复后保留“只给一次纠正机会”的边界。
func malformedToolArgumentsFailureCount(events []domain.Event) int {
	count := 0
	for _, event := range events {
		if event.Type != domain.EventModelRequestFailed {
			continue
		}
		var payload modelRequestFailedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ErrorClass == malformedToolArgumentsErrorClass {
			count++
		}
	}
	return count
}

// latestContextCheckpoint 返回最新有效检查点及其所替代历史的最后事件序号。
func latestContextCheckpoint(events []domain.Event) (string, uint64) {
	var summary string
	var throughSeq uint64
	for _, event := range events {
		if event.Type != domain.EventContextCompacted {
			continue
		}
		var payload domain.ContextCompactedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Summary != "" && payload.ThroughSeq < event.Seq {
			summary = payload.Summary
			throughSeq = payload.ThroughSeq
		}
	}
	return summary, throughSeq
}
