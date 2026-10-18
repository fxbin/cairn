// client_test 覆盖 OpenAI Responses 合同：文本流、工具参数增量、工具回执映射、
// usage 与终态。
//
// Author: fxbin
package openairesponses

import (
	"context"
	"encoding/json"
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
func sseServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", request.URL.Path)
		}
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestStreamTextAndUsage 锁定文本流、response.completed usage 与终态。
func TestStreamTextAndUsage(t *testing.T) {
	frames := strings.Join([]string{
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"Hello"}`,
		"",
		"event: response.output_text.delta",
		`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":" world"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
		"",
	}, "\n")
	server := sseServer(t, http.StatusOK, frames)
	events, emit := collectEvents(t)
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"completed", "tool_use"}}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "gpt-6", Messages: []ports.Message{{Role: "user", Content: "hi"}}}, emit); err != nil {
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
	if text.String() != "Hello world" || stop != "completed" {
		t.Fatalf("text=%q stop=%q", text.String(), stop)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %#v", usage)
	}
}

// TestStreamAssemblesToolCalls 锁定 function_call 参数增量拼接与 call_id 标识。
// 官方流：item.id=fc_…、call_id=call_…、delta.item_id=fc_…（两者不同）。
func TestStreamAssemblesToolCalls(t *testing.T) {
	frames := strings.Join([]string{
		"event: response.output_item.added",
		`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"apply_patch"}}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"path\":\"a.go\","}`,
		"",
		"event: response.function_call_arguments.delta",
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"mode\":\"create\"}"}`,
		"",
		"event: response.completed",
		`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":3,"output_tokens":7,"total_tokens":10}}}`,
		"",
	}, "\n")
	server := sseServer(t, http.StatusOK, frames)
	events, emit := collectEvents(t)
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "gpt-6"}, emit); err != nil {
		t.Fatal(err)
	}
	var call *ports.ToolCall
	for _, event := range *events {
		if event.Kind == ports.ToolCallEvent {
			call = event.ToolCall
		}
	}
	if call == nil || call.ID != "call_1" || call.Name != "apply_patch" {
		t.Fatalf("call = %#v", call)
	}
	if string(call.Arguments) != `{"path":"a.go","mode":"create"}` {
		t.Fatalf("arguments = %s", call.Arguments)
	}
}

// TestToolResultMapsToFunctionCallOutput 锁定工具回执映射为 function_call_output。
func TestToolResultMapsToFunctionCallOutput(t *testing.T) {
	var captured []responseInput
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload responseRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		captured = payload.Input
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	}))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL}
	_ = client.Stream(context.Background(), ports.ModelRequest{
		Model: "gpt-6",
		Messages: []ports.Message{
			{Role: "user", Content: "do it"},
			{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "call_1", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}},
			{Role: "tool", ToolCallID: "call_1", Content: "file body"},
		},
	}, func(ports.ModelEvent) error { return nil })
	if len(captured) != 3 {
		t.Fatalf("input = %#v", captured)
	}
	if captured[1].Type != "function_call" || captured[1].CallID != "call_1" {
		t.Fatalf("function_call = %#v", captured[1])
	}
	if captured[2].Type != "function_call_output" || captured[2].CallID != "call_1" || captured[2].Output != "file body" {
		t.Fatalf("function_call_output = %#v", captured[2])
	}
}

// TestIncompleteStopReasonMapped 锁定 incomplete_details.reason 映射为 stop reason。
func TestIncompleteStopReasonMapped(t *testing.T) {
	frames := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"
	server := sseServer(t, http.StatusOK, frames)
	events, emit := collectEvents(t)
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"max_output_tokens", "completed"}}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "m"}, emit); err != nil {
		t.Fatal(err)
	}
	stop := ""
	for _, event := range *events {
		if event.Kind == ports.Done {
			stop = event.StopReason
		}
	}
	if stop != "max_output_tokens" {
		t.Fatalf("stop = %q", stop)
	}
}
