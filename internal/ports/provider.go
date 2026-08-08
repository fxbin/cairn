package ports

import (
	"context"
	"encoding/json"
)

type Provider interface {
	Stream(ctx context.Context, request ModelRequest, emit func(ModelEvent) error) error
}

type ModelRequest struct {
	Model    string
	Messages []Message
	Tools    []ToolSpec
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ModelEventKind string

const (
	TextDelta     ModelEventKind = "text_delta"
	ToolCallEvent ModelEventKind = "tool_call"
	UsageEvent    ModelEventKind = "usage"
	Done          ModelEventKind = "done"
)

type ModelEvent struct {
	Kind     ModelEventKind
	Text     string
	ToolCall *ToolCall
	Usage    *Usage
}
