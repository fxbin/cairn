package openaicompat

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/ports"
)

func TestStreamAccumulatesToolCallsAndEmitsDoneLast(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"\"}}]}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"main.go\\\"}\"}}]}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var events []ports.ModelEvent
	client := Client{BaseURL: server.URL}
	err := client.Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(event ports.ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 || events[0].Kind != ports.TextDelta || events[1].Kind != ports.UsageEvent || events[2].Kind != ports.ToolCallEvent || events[3].Kind != ports.Done {
		t.Fatalf("unexpected event order: %#v", events)
	}
	if got := string(events[2].ToolCall.Arguments); got != `{"path":"main.go"}` {
		t.Fatalf("tool args = %s", got)
	}
}

func TestStreamReturnsCallbackErrorAndStops(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	want := errors.New("stop")
	err := (Client{BaseURL: server.URL}).Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(event ports.ModelEvent) error { return want })
	if !errors.Is(err, want) {
		t.Fatalf("expected callback error, got %v", err)
	}
}

func TestContractRetriesTransientFailureBeforeAnySemanticEvent(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if calls == 1 {
			writer.WriteHeader(http.StatusServiceUnavailable)
			_, _ = writer.Write([]byte("temporarily unavailable"))
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	var events []ports.ModelEvent
	err := (Client{BaseURL: server.URL}).Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(event ports.ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil || calls != 2 || len(events) != 2 || events[1].Kind != ports.Done {
		t.Fatalf("err=%v calls=%d events=%#v", err, calls, events)
	}
}

func TestContractDoesNotRetryAfterSemanticEvent(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	}))
	defer server.Close()
	err := (Client{BaseURL: server.URL}).Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil })
	if err == nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestChatCompletionsURL(t *testing.T) {
	endpoint, err := chatCompletionsURL("https://example.test/v1/")
	if err != nil {
		t.Fatal(err)
	}
	if endpoint != "https://example.test/v1/chat/completions" {
		t.Fatalf("endpoint = %s", endpoint)
	}
	if _, err := chatCompletionsURL("not-a-url"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected invalid URL, got %v", err)
	}
}
