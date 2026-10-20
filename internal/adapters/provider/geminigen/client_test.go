// client_test 覆盖 Gemini GenerateContent 合同：文本流、工具调用、工具结果、
// usage 与 finishReason。无真实凭据，仅离线验证。
//
// Author: fxbin
package geminigen

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
		if !strings.Contains(request.URL.Path, ":streamGenerateContent") {
			t.Errorf("path = %q", request.URL.Path)
		}
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// TestStreamTextAndUsage 锁定文本流、usageMetadata 与 finishReason。
func TestStreamTextAndUsage(t *testing.T) {
	frames := strings.Join([]string{
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"Hello\"}]}}]}",
		"",
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\" world\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":10,\"candidatesTokenCount\":5,\"totalTokenCount\":15}}",
		"",
	}, "\n")
	server := sseServer(t, http.StatusOK, frames)
	events, emit := collectEvents(t)
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"STOP", "MAX_TOKENS"}}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "gemini-2.5-pro", Messages: []ports.Message{{Role: "user", Content: "hi"}}}, emit); err != nil {
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
	if text.String() != "Hello world" || stop != "STOP" {
		t.Fatalf("text=%q stop=%q", text.String(), stop)
	}
	if usage == nil || usage.PromptTokens != 10 || usage.CompletionTokens != 5 || usage.TotalTokens != 15 {
		t.Fatalf("usage = %#v", usage)
	}
}

// TestStreamAssemblesFunctionCalls 锁定 functionCall 标识与 args。
func TestStreamAssemblesFunctionCalls(t *testing.T) {
	frames := strings.Join([]string{
		"data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"functionCall\":{\"name\":\"apply_patch\",\"args\":{\"path\":\"a.go\",\"mode\":\"create\"}}}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":3,\"candidatesTokenCount\":7,\"totalTokenCount\":10}}",
		"",
	}, "\n")
	server := sseServer(t, http.StatusOK, frames)
	events, emit := collectEvents(t)
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "gemini-2.5-pro"}, emit); err != nil {
		t.Fatal(err)
	}
	var call *ports.ToolCall
	for _, event := range *events {
		if event.Kind == ports.ToolCallEvent {
			call = event.ToolCall
		}
	}
	if call == nil || call.Name != "apply_patch" {
		t.Fatalf("call = %#v", call)
	}
	if !strings.Contains(string(call.Arguments), `"path":"a.go"`) {
		t.Fatalf("arguments = %s", call.Arguments)
	}
}

// TestToolResultMapsToFunctionResponse 锁定工具结果映射为 functionResponse。
func TestToolResultMapsToFunctionResponse(t *testing.T) {
	var captured []content
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload generateRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		captured = payload.Contents
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n"))
	}))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL}
	_ = client.Stream(context.Background(), ports.ModelRequest{
		Model: "gemini-2.5-pro",
		Messages: []ports.Message{
			{Role: "user", Content: "do it"},
			{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "c1", Name: "read_file", Arguments: json.RawMessage(`{"path":"x"}`)}}},
			{Role: "tool", ToolCallID: "read_file", Content: `{"ok":true}`},
		},
	}, func(ports.ModelEvent) error { return nil })
	if len(captured) < 3 {
		t.Fatalf("contents = %#v", captured)
	}
	last := captured[len(captured)-1]
	if len(last.Parts) != 1 || last.Parts[0].FunctionResponse == nil || last.Parts[0].FunctionResponse.Name != "read_file" {
		t.Fatalf("functionResponse = %#v", last.Parts)
	}
}

// TestHTTPErrorClassification 锁定错误分类。
func TestHTTPErrorClassification(t *testing.T) {
	server := sseServer(t, http.StatusTooManyRequests, `{"error":{"code":429,"status":"RESOURCE_EXHAUSTED","message":"quota"}}`)
	client := Client{BaseURL: server.URL}
	err := client.Stream(context.Background(), ports.ModelRequest{Model: "m"}, func(ports.ModelEvent) error { return nil })
	var providerError *ProviderError
	if err == nil || !strings.Contains(err.Error(), "rate_limited") {
		t.Fatalf("err = %v", err)
	}
	_ = providerError
}

// TestToolResultMatchesDuplicateCallIDsByOrder 锁定旧历史重复 call ID（如跨轮
// call_0）按出现顺序 FIFO 匹配函数名，不被后者覆盖。
func TestToolResultMatchesDuplicateCallIDsByOrder(t *testing.T) {
	var captured []content
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload generateRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		captured = payload.Contents
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte("data: {\"candidates\":[{\"content\":{\"role\":\"model\",\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1,\"totalTokenCount\":2}}\n\n"))
	}))
	t.Cleanup(server.Close)
	client := Client{BaseURL: server.URL}
	// 旧历史：两轮都是 call_0，但函数名不同。
	_ = client.Stream(context.Background(), ports.ModelRequest{
		Model: "gemini-2.5-pro",
		Messages: []ports.Message{
			{Role: "user", Content: "task 1"},
			{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "call_0", Name: "apply_patch", Arguments: json.RawMessage(`{}`)}}},
			{Role: "tool", ToolCallID: "call_0", Content: `{"ok":true}`},
			{Role: "user", Content: "task 2"},
			{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "call_0", Name: "read_file", Arguments: json.RawMessage(`{}`)}}},
			{Role: "tool", ToolCallID: "call_0", Content: `{"ok":true}`},
		},
	}, func(ports.ModelEvent) error { return nil })
	var names []string
	for _, c := range captured {
		for _, p := range c.Parts {
			if p.FunctionResponse != nil {
				names = append(names, p.FunctionResponse.Name)
			}
		}
	}
	if len(names) != 2 || names[0] != "apply_patch" || names[1] != "read_file" {
		t.Fatalf("functionResponse names = %v, want [apply_patch read_file]", names)
	}
}
