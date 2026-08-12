package ports

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrMalformedToolCallArguments 表示提供方返回的工具参数不是完整 JSON；它不携带原始参数，
// 以便调用方可以安全地持久化分类并要求模型重新生成。
var ErrMalformedToolCallArguments = errors.New("provider returned malformed tool call arguments")

// Provider 是将模型流式响应适配为领域事件的端口。
type Provider interface {
	Stream(ctx context.Context, request ModelRequest, emit func(ModelEvent) error) error
}

// ModelRequest 是发送给模型提供方的标准化请求。
type ModelRequest struct {
	Model    string
	Messages []Message
	Tools    []ToolSpec
}

// Message 表示模型对话中的一条消息或工具调用回执。
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

// ToolSpec 描述可供模型选择调用的固定工具及其 JSON schema。
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

// ToolCall 表示模型请求执行的一次具名工具调用。
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// Usage 保存提供方报告的本次模型调用 token 消耗。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ModelEventKind 区分模型流式响应中的语义事件。
type ModelEventKind string

const (
	TextDelta     ModelEventKind = "text_delta"
	ToolCallEvent ModelEventKind = "tool_call"
	UsageEvent    ModelEventKind = "usage"
	Done          ModelEventKind = "done"
)

// ModelEvent 是 Provider 向 Agent 流式发送的统一响应片段。
type ModelEvent struct {
	Kind     ModelEventKind
	Text     string
	ToolCall *ToolCall
	Usage    *Usage
}
