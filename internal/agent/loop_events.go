package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

const malformedToolArgumentsErrorClass = "malformed_tool_arguments"

const malformedToolArgumentsFeedback = "Cairn runtime: the previous tool call was rejected because its arguments were malformed. Do not repeat it. Reissue at most one tool call with valid JSON arguments and no secrets."

// EventSink 接收 Loop 已成功追加到日志的领域事件。
type EventSink func(domain.Event)

// append 以当前投影序号追加事件、应用投影并向外部 sink 广播。
// 家族外部追加（子完成回写、跨 run 消息）与本 Loop 并发时序号会冲突：
// 冲突属确定性可消除——先重载尾部把外部事实应用进投影，再重验重试一次；
// 外部事件使草稿非法或二次追加仍冲突时失败关闭，不无限重试。
func (loop *Loop) append(ctx context.Context, projection *domain.RunProjection, draft domain.EventDraft) error {
	if err := loop.appendOnce(ctx, projection, draft); err != nil {
		if !errors.Is(err, ports.ErrJournalConflict) {
			return err
		}
		if resyncErr := loop.resyncProjection(ctx, projection); resyncErr != nil {
			return fmt.Errorf("%w; resync after conflict failed: %v", err, resyncErr)
		}
		return loop.appendOnce(ctx, projection, draft)
	}
	return nil
}

// appendOnce 执行一次校验、CAS 追加、投影应用与 sink 广播。
func (loop *Loop) appendOnce(ctx context.Context, projection *domain.RunProjection, draft domain.EventDraft) error {
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
	if event.Type == domain.EventOperationStarted && !isMCPLifecyclePayload(event.Payload) {
		loop.startedOperations++
	}
	if loop.OnEvent != nil {
		loop.OnEvent(event)
	}
	return nil
}

// messagesFromEvents 将已持久化事实重建为下一次模型请求所需的对话上下文；
// 创建事件声明的前情引用必须可解析且可完整读取，否则失败关闭。
func (loop *Loop) messagesFromEvents(events []domain.Event, fallbackTask string) ([]ports.Message, error) {
	return loop.rebuildMessagesFromEvents(events, fallbackTask, false)
}

// rebuildMessagesFromEvents 共用持久化消息投影；压缩重建额外保留纯文本响应，
// 防止最终说明遭验证失败后丢失决策。普通恢复沿用不重复注入最终说明的行为。
func (loop *Loop) rebuildMessagesFromEvents(events []domain.Event, fallbackTask string, includeTextOnlyResponses bool) ([]ports.Message, error) {
	if err := loop.restoreProjectInstructions(events); err != nil {
		return nil, err
	}
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
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("decode run_created for message reconstruction: %w", err)
		}
		if payload.Task != "" {
			task = payload.Task
		}
		if len(payload.PriorContextRef) > 0 {
			var ref artifact.Ref
			if err := json.Unmarshal(payload.PriorContextRef, &ref); err != nil {
				return nil, fmt.Errorf("decode prior context ref: %w", err)
			}
			loaded, err := loop.loadPriorContext(&ref)
			if err != nil {
				return nil, fmt.Errorf("load prior context: %w", err)
			}
			if loop.PriorContext == "" && loop.PriorContextRef == nil {
				priorContext = loaded
				priorRef = &ref
				loop.PriorContext = loaded
				loop.PriorContextRef = priorRef
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
			if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" && payload.ToolName != tools.MCPQuiesceTool {
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
	messages := loop.contextMessages(task, priorContext, checkpoint)
	// 旧摘要可能丢失已触发的检查点正文。依据切点之前的 Journal 事实补齐消息
	// 投影，不写第二个反馈事件；后续压缩与请求预算会计算这条固定提醒。
	if throughSeq > 0 && !strings.Contains(checkpoint, decisionCheckpointSummary) {
		for _, event := range events {
			if event.Seq > throughSeq || event.Type != domain.EventVerificationFeedbackAppended {
				continue
			}
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && strings.HasPrefix(payload.Text, decisionCheckpointPrefix) {
				messages = append(messages, ports.Message{Role: "user", Content: decisionCheckpointSummary})
				break
			}
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
			if json.Unmarshal(event.Payload, &payload) != nil || len(payload.ToolCalls) == 0 && !includeTextOnlyResponses {
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
				message := ports.Message{Role: "assistant", Content: payload.Text, ToolCalls: payload.ToolCalls}
				if payload.ProtocolBlocksRef != nil && loop.Artifacts != nil {
					blocks, err := loop.loadProtocolBlocks(payload.ProtocolBlocksRef)
					if err != nil {
						return nil, err
					}
					message.ProtocolBlocks = blocks
				}
				messages = append(messages, message)
			}
		case domain.EventToolCallProposed:
			if event.OperationID == nil {
				continue
			}
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" && payload.ToolName != tools.MCPQuiesceTool {
				proposals[*event.OperationID] = ports.ToolCall{ID: payload.CallID, Name: payload.ToolName, Arguments: payload.Arguments}
			}
		case domain.EventToolResultAppended:
			if event.OperationID == nil {
				continue
			}
			if call, exists := proposals[*event.OperationID]; exists {
				content, err := loop.modelToolResultJSON(call.Name, event.Payload)
				if err != nil {
					return nil, err
				}
				messages = append(messages, ports.Message{Role: "tool", ToolCallID: call.ID, Content: content})
			}
		case domain.EventVerificationFeedbackAppended:
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Text != "" {
				messages = append(messages, ports.Message{Role: "user", Content: payload.Text})
			}
		case domain.EventSteeringInputConsumed:
			// 已生效 steering 按事件顺序重建为用户补充指导或异步子终态摘要；
			// 正文从受保护制品严格解引用，缺失或损坏失败关闭。
			var payload struct {
				InputID    string          `json:"input_id"`
				ContentRef json.RawMessage `json:"content_ref"`
				Outcome    string          `json:"outcome"`
				Kind       string          `json:"kind"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				continue
			}
			if payload.Outcome != "applied" {
				continue
			}
			if len(payload.ContentRef) == 0 {
				return nil, fmt.Errorf("steering input %s missing content ref", payload.InputID)
			}
			var ref artifact.Ref
			if err := json.Unmarshal(payload.ContentRef, &ref); err != nil {
				return nil, fmt.Errorf("steering input %s content ref: %w", payload.InputID, err)
			}
			content, err := loop.loadPriorContext(&ref)
			if err != nil {
				return nil, fmt.Errorf("steering input %s content: %w", payload.InputID, err)
			}
			prefix := "[用户补充当前任务指导]\n"
			if payload.Kind == steeringKindSubagentResult {
				prefix = asyncChildResultMessagePrefix
			}
			messages = append(messages, ports.Message{Role: "user", Content: prefix + content})
		}
	}
	return messages, nil
}

// sessionContextPrefix 标识会话前情消息，并用于压缩时精确提取而不重复注入。
const sessionContextPrefix = "以下本会话前情供背景参考，由上一任务的事件投影生成：\n"

// sessionContextBlock 把前情文本包装为标注来源的会话前情块；任务原文始
// 终是独立的最新用户消息，前情只提供背景不改变任务语义。
func sessionContextBlock(prior string) string {
	return sessionContextPrefix + prior
}

// loadProtocolBlocks 从受保护制品读取连续性块；引用存在即属于可回放合同，
// 摘要或格式错误必须失败关闭，不得静默丢弃签名。
func (loop *Loop) loadProtocolBlocks(ref *artifact.Ref) ([]ports.ProtocolBlock, error) {
	if ref == nil {
		return nil, nil
	}
	content, err := loop.Artifacts.Open(*ref)
	if err != nil {
		return nil, fmt.Errorf("load protocol blocks: %w", err)
	}
	var blocks []ports.ProtocolBlock
	if err := json.Unmarshal(content, &blocks); err != nil {
		return nil, fmt.Errorf("decode protocol blocks: %w", err)
	}
	return blocks, nil
}

// loadPriorContext 从制品库严格读取并校验前情卡；引用存在即属于可回放合同，
// 任一格式、存在性或摘要错误都必须由调用方传播。
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
