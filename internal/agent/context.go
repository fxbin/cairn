package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

const contextCompactionStrategy = "deterministic_tool_output_checkpoint_v1"

const contextCheckpointPrefix = "此前的对话原文已被压缩为以下可审计检查点。系统提示和原始任务保持不变；需要细节时请重新读取工作区或调用工具。\n"

// compactContextIfNeeded 在安全恢复边界把过长对话替换为持久化检查点。
func (loop *Loop) compactContextIfNeeded(ctx context.Context, projection *domain.RunProjection, input RunInput, messages []ports.Message) ([]ports.Message, error) {
	if !loop.ContextAutoCompact || loop.ContextMaxPromptTokens <= 0 {
		return messages, nil
	}
	before := loop.estimatePromptTokens(messages)
	if before <= loop.contextCompactionThreshold() {
		return messages, nil
	}
	if projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil {
		return nil, errors.New("context compaction requires a closed turn without pending work")
	}
	maximumSummaryBytes := loop.compactionSummaryLimit(input.Task)
	if maximumSummaryBytes < 1 {
		return nil, fmt.Errorf("context limit %d cannot fit the persistent prompt and tool definitions", loop.ContextMaxPromptTokens)
	}
	summary := summarizeMessages(messages, maximumSummaryBytes)
	compacted := loop.contextMessages(input.Task, summary)
	after := loop.estimatePromptTokens(compacted)
	if after > loop.ContextMaxPromptTokens {
		return nil, fmt.Errorf("context checkpoint requires %d tokens, exceeding configured limit %d", after, loop.ContextMaxPromptTokens)
	}
	payload := domain.ContextCompactedPayload{
		ThroughSeq:            projection.LastSeq,
		Summary:               summary,
		EstimatedTokensBefore: before,
		EstimatedTokensAfter:  after,
		Strategy:              contextCompactionStrategy,
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventContextCompacted, Payload: mustJSON(payload)}); err != nil {
		return nil, err
	}
	return compacted, nil
}

// contextCompactionThreshold 在模型的名义上下文上限前保留 20% 余量，吸收
// 工具定义、JSON 编码与中英文 tokenizer 差异，避免临界请求消耗整轮输入预算。
func (loop *Loop) contextCompactionThreshold() int {
	reserve := loop.ContextMaxPromptTokens / 5
	threshold := loop.ContextMaxPromptTokens - reserve
	if threshold < 1 {
		return 1
	}
	return threshold
}

// contextMessages 重新注入固定提示、原始任务和一条替代旧历史的检查点消息。
func (loop *Loop) contextMessages(task, summary string) []ports.Message {
	messages := []ports.Message{{Role: "system", Content: loop.prompt()}, {Role: "user", Content: task}}
	if summary != "" {
		messages = append(messages, ports.Message{Role: "user", Content: contextCheckpointPrefix + summary})
	}
	return messages
}

// compactionSummaryLimit 在静态工具定义和固定消息之后，为检查点预留字节空间。
func (loop *Loop) compactionSummaryLimit(task string) int {
	limitBytes := loop.ContextMaxPromptTokens * 3
	persistent := loop.prompt() + task + contextCheckpointPrefix
	for _, spec := range loop.Tools.Specs() {
		persistent += spec.Name + spec.Description + string(spec.Parameters)
	}
	remaining := limitBytes - len(persistent)
	if remaining < 1 {
		return 0
	}
	if loop.ContextMaxSummaryBytes > 0 && remaining > loop.ContextMaxSummaryBytes {
		remaining = loop.ContextMaxSummaryBytes
	}
	return remaining
}

// estimatePromptTokens 用每两个 UTF-8 字节一个 token 的悲观近似估算下一次请求输入，
// 为源码、JSON 和中英文混合内容保留压缩余量，避免低估导致累计输入预算先被耗尽。
func (loop *Loop) estimatePromptTokens(messages []ports.Message) int {
	bytes := 0
	for _, message := range messages {
		bytes += len(message.Role) + len(message.Content) + len(message.ToolCallID)
		for _, call := range message.ToolCalls {
			bytes += len(call.ID) + len(call.Name) + len(call.Arguments)
		}
	}
	for _, spec := range loop.Tools.Specs() {
		bytes += len(spec.Name) + len(spec.Description) + len(spec.Parameters)
	}
	return (bytes + 1) / 2
}

// summarizeMessages 优先保留最近的工具结果、模型决策和验证反馈，不复制原始数据体。
func summarizeMessages(messages []ports.Message, maximumBytes int) string {
	if maximumBytes < 1 {
		return ""
	}
	toolNames := make(map[string]string)
	previousCheckpoint := ""
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			toolNames[call.ID] = call.Name
		}
		if message.Role == "user" && strings.HasPrefix(message.Content, contextCheckpointPrefix) {
			previousCheckpoint = strings.TrimPrefix(message.Content, contextCheckpointPrefix)
		}
	}
	entries := make([]string, 0, len(messages))
	used := 0
	preservedCheckpoint := ""
	if previousCheckpoint != "" {
		available := maximumBytes/2 - len("- 先前检查点：\n")
		if available > 0 {
			preservedCheckpoint = "- 先前检查点：" + clipText(previousCheckpoint, available) + "\n"
			used = len(preservedCheckpoint)
		}
	}
	for index := len(messages) - 1; index >= 2; index-- {
		if messages[index].Role == "user" && strings.HasPrefix(messages[index].Content, contextCheckpointPrefix) {
			continue
		}
		entry := summarizeMessage(messages[index], toolNames)
		if entry == "" {
			continue
		}
		entry = "- " + entry + "\n"
		if used+len(entry) > maximumBytes {
			remaining := maximumBytes - used
			if remaining > len("- …\n") {
				entries = append(entries, "- "+clipText(strings.TrimSpace(entry[2:]), remaining-len("- …\n"))+"…\n")
			}
			break
		}
		entries = append(entries, entry)
		used += len(entry)
	}
	if len(entries) == 0 {
		if preservedCheckpoint != "" {
			return string(artifact.Redact([]byte(preservedCheckpoint)))
		}
		return clipText("无可保留的历史事实；请以原始任务和当前工作区为准。", maximumBytes)
	}
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
	return string(artifact.Redact([]byte(preservedCheckpoint + strings.Join(entries, ""))))
}

// summarizeMessage 将单条历史消息归约为不含工具原始 data 字段的事实记录。
func summarizeMessage(message ports.Message, toolNames map[string]string) string {
	switch message.Role {
	case "tool":
		name := toolNames[message.ToolCallID]
		if name == "" {
			name = "unknown_tool"
		}
		return fmt.Sprintf("工具 %s（调用 %s）：%s", name, message.ToolCallID, summarizeToolResult(message.Content))
	case "assistant":
		parts := make([]string, 0, len(message.ToolCalls)+1)
		if text := strings.TrimSpace(message.Content); text != "" {
			parts = append(parts, "模型说明："+clipText(string(artifact.Redact([]byte(text))), 480))
		}
		for _, call := range message.ToolCalls {
			parts = append(parts, fmt.Sprintf("请求工具 %s（调用 %s）", call.Name, call.ID))
		}
		return strings.Join(parts, "；")
	case "user":
		text := strings.TrimSpace(message.Content)
		if text == "" {
			return ""
		}
		return "用户或验证反馈：" + clipText(string(artifact.Redact([]byte(text))), 480)
	default:
		return ""
	}
}

// summarizeToolResult 只保留工具结果的状态、预览、大小和摘要，刻意丢弃 data 原文。
func summarizeToolResult(content string) string {
	var result struct {
		OK        bool   `json:"ok"`
		Code      string `json:"code"`
		Preview   string `json:"preview"`
		Truncated bool   `json:"truncated"`
		Bytes     int64  `json:"bytes"`
		SHA256    string `json:"sha256"`
	}
	if err := json.Unmarshal(artifact.RedactJSON([]byte(content)), &result); err != nil {
		return "返回了无法解析的结构化结果：" + clipText(string(artifact.Redact([]byte(content))), 320)
	}
	parts := []string{fmt.Sprintf("ok=%t", result.OK), "code=" + result.Code}
	if result.Preview != "" {
		parts = append(parts, "preview="+clipText(result.Preview, 480))
	}
	if result.Bytes > 0 {
		parts = append(parts, fmt.Sprintf("bytes=%d", result.Bytes))
	}
	if result.SHA256 != "" {
		parts = append(parts, "sha256="+result.SHA256)
	}
	if result.Truncated {
		parts = append(parts, "truncated=true")
	}
	return strings.Join(parts, "，")
}

// clipText 在 UTF-8 字符边界截断文本，避免摘要因单条消息越过既定上限。
func clipText(value string, maximumBytes int) string {
	if maximumBytes < 1 {
		return ""
	}
	if len(value) <= maximumBytes {
		return value
	}
	if maximumBytes <= len("…") {
		return ""
	}
	limit := maximumBytes - len("…")
	clipped := value[:limit]
	for !utf8.ValidString(clipped) {
		clipped = clipped[:len(clipped)-1]
	}
	return clipped + "…"
}
