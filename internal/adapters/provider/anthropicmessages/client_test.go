// client_test 覆盖 Anthropic Messages 合同：文本流、工具参数增量拼接、工具
// 结果往返、usage、停止原因、错误分类与思考块失败关闭。
//
// Author: fxbin
package anthropicmessages

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/ports"
)

// collectEvents 收集端口事件供断言。
func collectEvents(t *testing.T) (*[]ports.ModelEvent, func(ports.ModelEvent) error) {
	t.Helper()
	events := &[]ports.ModelEvent{}
	return events, func(event ports.ModelEvent) error {
		*events = append(*events, event)
		return nil
	}
}

// sseServer 构造返回固定 SSE 帧的测试服务。
func sseServer(t *testing.T, status int, body string, header map[string]string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("anthropic-version") != ProtocolVersion {
			t.Errorf("anthropic-version = %q", request.Header.Get("anthropic-version"))
		}
		for key, value := range header {
			writer.Header().Set(key, value)
		}
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestStreamTextAndUsage 锁定文本流、message_delta usage 与 end_turn 终态。
func TestStreamTextAndUsage(t *testing.T) {
	frames := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":" world"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":0}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	server := sseServer(t, http.StatusOK, frames, nil)
	events, emit := collectEvents(t)
	client := Client{BaseURL: server.URL, APIKey: "k", Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"end_turn", "tool_use", "max_tokens"}}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "claude-sonnet-5", Messages: []ports.Message{{Role: "user", Content: "hi"}}}, emit); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var usage *ports.Usage
	stop := ""
	for _, event := range *events {
		switch event.Kind {
		case ports.TextDelta:
			text.WriteString(event.Text)
		case ports.UsageEvent:
			usage = event.Usage
		case ports.Done:
			stop = event.StopReason
		}
	}
	if text.String() != "Hello world" || stop != "end_turn" {
		t.Fatalf("text=%q stop=%q", text.String(), stop)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %#v", usage)
	}
}

// TestStreamAssemblesToolCallsFromPartialJSON 锁定工具参数增量拼接为完整 JSON。
func TestStreamAssemblesToolCallsFromPartialJSON(t *testing.T) {
	frames := strings.Join([]string{
		"event: message_start",
		`data: {"type":"message_start","message":{"usage":{"input_tokens":3,"output_tokens":0}}}`,
		"",
		"event: content_block_start",
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"apply_patch","input":{}}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"a.go\","}}`,
		"",
		"event: content_block_delta",
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"mode\":\"create\"}"}}`,
		"",
		"event: content_block_stop",
		`data: {"type":"content_block_stop","index":1}`,
		"",
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"input_tokens":3,"output_tokens":7}}`,
		"",
		"event: message_stop",
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")
	server := sseServer(t, http.StatusOK, frames, nil)
	events, emit := collectEvents(t)
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "claude-sonnet-5"}, emit); err != nil {
		t.Fatal(err)
	}
	var call *ports.ToolCall
	stop := ""
	for _, event := range *events {
		if event.Kind == ports.ToolCallEvent {
			call = event.ToolCall
		}
		if event.Kind == ports.Done {
			stop = event.StopReason
		}
	}
	if call == nil || call.ID != "call_1" || call.Name != "apply_patch" {
		t.Fatalf("call = %#v", call)
	}
	if string(call.Arguments) != `{"path":"a.go","mode":"create"}` {
		t.Fatalf("arguments = %s", call.Arguments)
	}
	if stop != "tool_use" {
		t.Fatalf("stop = %q", stop)
	}
}

// TestToolResultRoundTrip 锁定工具结果消息映射为 tool_result 块。
func TestToolResultRoundTrip(t *testing.T) {
	var captured []anthropicMessage
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload messageRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		captured = payload.Messages
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL}
	_ = client.Stream(context.Background(), ports.ModelRequest{
		Model: "claude-sonnet-5",
		Messages: []ports.Message{
			{Role: "user", Content: "do it"},
			{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}},
			{Role: "user", ToolCallID: "call_1", Content: "file body"},
		},
	}, func(ports.ModelEvent) error { return nil })
	if len(captured) != 3 {
		t.Fatalf("messages = %#v", captured)
	}
	last := captured[2]
	if len(last.Content) != 1 || last.Content[0].Type != "tool_result" || last.Content[0].ToolUseID != "call_1" || last.Content[0].Content != "file body" {
		t.Fatalf("tool result = %#v", last.Content)
	}
	assistant := captured[1]
	if len(assistant.Content) != 1 || assistant.Content[0].Type != "tool_use" || assistant.Content[0].ID != "call_1" {
		t.Fatalf("assistant = %#v", assistant.Content)
	}
}

// TestClassifiesHTTPErrors 锁定错误分类与可重试性。
func TestClassifiesHTTPErrors(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		body      string
		wantKind  string
		retryable bool
	}{
		{name: "限流", status: http.StatusTooManyRequests, body: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, wantKind: "rate_limited", retryable: true},
		{name: "过载", status: http.StatusServiceUnavailable, body: `{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`, wantKind: "overloaded", retryable: true},
		{name: "认证", status: http.StatusUnauthorized, body: `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, wantKind: "authentication"},
		{name: "请求非法", status: http.StatusBadRequest, body: `{"type":"error","error":{"type":"invalid_request_error","message":"bad"}}`, wantKind: "invalid_request"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := sseServer(t, test.status, test.body, nil)
			client := Client{BaseURL: server.URL}
			err := client.Stream(context.Background(), ports.ModelRequest{Model: "m"}, func(ports.ModelEvent) error { return nil })
			var providerError *ProviderError
			if !errors.As(err, &providerError) {
				t.Fatalf("err = %v", err)
			}
			if providerError.Kind != test.wantKind || providerError.IsRetryable() != test.retryable {
				t.Fatalf("kind=%q retryable=%v", providerError.Kind, providerError.IsRetryable())
			}
		})
	}
}

// TestThinkingBlockFailsClosed 锁定未启用连续性时思考块失败关闭，不静默丢弃。
func TestThinkingBlockFailsClosed(t *testing.T) {
	frames := strings.Join([]string{
		"event: content_block_start",
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","signature":"sig","thinking":"hidden"}}`,
		"",
	}, "\n")
	server := sseServer(t, http.StatusOK, frames, nil)
	client := Client{BaseURL: server.URL}
	err := client.Stream(context.Background(), ports.ModelRequest{Model: "m"}, func(ports.ModelEvent) error { return nil })
	var providerError *ProviderError
	if !errors.As(err, &providerError) || providerError.Kind != "invalid_response" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(err.Error(), "continuity") {
		t.Fatalf("err = %v", err)
	}
}

// TestStreamContractRequiresUsage 锁定声明 streaming_usage 后缺 usage 失败关闭。
func TestStreamContractRequiresUsage(t *testing.T) {
	frames := "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	server := sseServer(t, http.StatusOK, frames, nil)
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"end_turn"}}}
	err := client.Stream(context.Background(), ports.ModelRequest{Model: "m"}, func(ports.ModelEvent) error { return nil })
	var providerError *ProviderError
	if !errors.As(err, &providerError) || providerError.Kind != "invalid_response" {
		t.Fatalf("err = %v", err)
	}
}
