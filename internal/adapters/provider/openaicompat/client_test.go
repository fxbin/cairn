package openaicompat

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

func TestStreamAccumulatesToolCallsAndEmitsDoneLast(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"\"}}]}}]}\n\n"))
		_, _ = writer.Write([]byte("data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"main.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}` + "\n\n"))
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
	if events[3].StopReason != "tool_calls" {
		t.Fatalf("done stop reason = %q", events[3].StopReason)
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

// doneBoundaryReader 在完整结束帧之后的任何额外读取上返回错误，使解码器
// 不能依赖 EOF 或网络定时来证明已经收到 DONE。
type doneBoundaryReader struct {
	source        *strings.Reader
	readAfterDone bool
}

// Read 提供预先冻结的完整帧，随后记录并拒绝越过响应结束边界的读取。
func (reader *doneBoundaryReader) Read(target []byte) (int, error) {
	if reader.source.Len() == 0 {
		reader.readAfterDone = true
		return 0, errors.New("unexpected read after complete DONE frame")
	}
	return reader.source.Read(target)
}

// TestDecodeStreamStopsAtCompleteDoneFrame 锁定完成帧内工具参数的校验和事件
// 顺序；完成回调成功后不再读取输入，结束帧后的字节不属于已结束的响应。
func TestDecodeStreamStopsAtCompleteDoneFrame(t *testing.T) {
	reader := &doneBoundaryReader{source: strings.NewReader(
		"data: " + `{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"read_file","arguments":"{\"path\":\"main.go\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n" +
			"data: [DONE]\n\n")}
	var events []ports.ModelEvent
	err := decodeStream(context.Background(), reader, func(event ports.ModelEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("complete DONE frame must finish without another read: %v", err)
	}
	if reader.readAfterDone {
		t.Fatal("decoder read beyond the complete DONE frame")
	}
	if len(events) != 3 || events[0].Kind != ports.UsageEvent || events[1].Kind != ports.ToolCallEvent || events[2].Kind != ports.Done {
		t.Fatalf("unexpected event order: %#v", events)
	}
	if string(events[1].ToolCall.Arguments) != `{"path":"main.go"}` || events[2].StopReason != "tool_calls" {
		t.Fatalf("completion lost tool arguments or finish reason: %#v", events)
	}
}

// TestDecodeStreamPreservesDoneCallbackError 确认结束回调失败仍是失败，不能
// 因为帧已标记 DONE 而把回调错误转换为成功。
func TestDecodeStreamPreservesDoneCallbackError(t *testing.T) {
	want := errors.New("completion callback failed")
	err := decodeStream(context.Background(), strings.NewReader("data: [DONE]\n\n"), func(event ports.ModelEvent) error {
		if event.Kind != ports.Done {
			t.Fatalf("unexpected event: %#v", event)
		}
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("completion callback error = %v, want %v", err, want)
	}
}

// TestDecodeStreamRejectsInvalidCompletion 确认 finish_reason 不替代 DONE，
// 且 DONE 到来前发现的冲突终止原因仍失败关闭。
func TestDecodeStreamRejectsInvalidCompletion(t *testing.T) {
	stop := "data: " + `{"choices":[{"delta":{},"finish_reason":"stop"}]}` + "\n\n"
	for _, test := range []struct {
		name   string
		stream string
		want   string
	}{
		{name: "finish reason without DONE", stream: stop, want: "stream ended before [DONE]"},
		{name: "conflicting reasons before DONE", stream: stop + "data: " + `{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\ndata: [DONE]\n\n", want: "conflicting finish reasons"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := decodeStream(context.Background(), strings.NewReader(test.stream), func(ports.ModelEvent) error { return nil })
			var failure *ProviderError
			if !errors.As(err, &failure) || failure.Kind != "invalid_response" || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("invalid completion = %v, want invalid_response containing %q", err, test.want)
			}
		})
	}
}

// TestStreamClosesResponseBodyAfterDone 验证服务端发送完成帧后仍保持响应开启
// 时，客户端立即成功并关闭响应体；通道握手区分 DONE 与服务端 EOF。
func TestStreamClosesResponseBodyAfterDone(t *testing.T) {
	doneWritten := make(chan struct{})
	bodyClosed := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: " + `{"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		writer.(http.Flusher).Flush()
		close(doneWritten)
		select {
		case <-request.Context().Done():
			close(bodyClosed)
		case <-releaseHandler:
		}
	}))
	defer func() {
		close(releaseHandler)
		server.Close()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := Client{BaseURL: server.URL, HTTPClient: server.Client(), Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"stop"}}}
	completed := make(chan error, 1)
	go func() {
		completed <- client.Stream(ctx, ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil })
	}()
	select {
	case <-doneWritten:
	case <-ctx.Done():
		t.Fatalf("server did not send completion frame: %v", ctx.Err())
	}
	select {
	case err := <-completed:
		if err != nil {
			t.Fatalf("completed response waited for EOF or failed: %v", err)
		}
	case <-ctx.Done():
		t.Fatalf("client still waited for server EOF after DONE: %v", ctx.Err())
	}
	select {
	case <-bodyClosed:
	case <-ctx.Done():
		t.Fatalf("completed response body was not closed: %v", ctx.Err())
	}
}

func TestContractRejectsMalformedToolArgumentsWithoutEchoingThem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"main.go\\\",\\\"token\\\":\\\"sk-not-for-journal\\\"\"}}]}}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	err := (Client{BaseURL: server.URL}).Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil })
	if !errors.Is(err, ports.ErrMalformedToolCallArguments) {
		t.Fatalf("expected malformed arguments error, got %v", err)
	}
	if strings.Contains(err.Error(), "sk-not-for-journal") {
		t.Fatalf("provider error leaked malformed arguments: %v", err)
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
	if _, err := chatCompletionsURL("https://user:secret@example.test/v1"); err == nil || !strings.Contains(err.Error(), "userinfo") {
		t.Fatalf("expected userinfo rejection, got %v", err)
	}
	if _, err := chatCompletionsURL("https://example.test/v1?token=secret"); err == nil || !strings.Contains(err.Error(), "query or fragment") {
		t.Fatalf("expected query rejection, got %v", err)
	}
	if _, err := chatCompletionsURL("https://example.test/v1#secret"); err == nil || !strings.Contains(err.Error(), "query or fragment") {
		t.Fatalf("expected fragment rejection, got %v", err)
	}
}

// TestContractExhaustsRetriesOnPersistentTransientFailure 验证持续可重试失败
// 在总尝试次数（3 次）耗尽后返回最后一次服务端错误，且不放大调用次数。
func TestContractExhaustsRetriesOnPersistentTransientFailure(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		writer.Header().Set("Retry-After", "0")
		writer.WriteHeader(http.StatusServiceUnavailable)
		_, _ = writer.Write([]byte("still unavailable"))
	}))
	defer server.Close()
	err := (Client{BaseURL: server.URL}).Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil })
	var providerError *ProviderError
	if err == nil || !errors.As(err, &providerError) || providerError.Kind != "server" || !providerError.Retryable {
		t.Fatalf("expected retryable server error, got %v", err)
	}
	if calls != 3 {
		t.Fatalf("expected exactly 3 attempts, got %d", calls)
	}
}

// TestRetryDelayCapsServerProvidedWait 锁定重试等待上限：异常或恶意服务端
// 的超大 Retry-After 不会让客户端长时间休眠。
func TestRetryDelayCapsServerProvidedWait(t *testing.T) {
	if got := retryDelay(time.Hour, 0); got != 30*time.Second {
		t.Fatalf("retryDelay(1h) = %v, want 30s", got)
	}
	if got := retryDelay(2*time.Second, 0); got != 2*time.Second {
		t.Fatalf("retryDelay(2s) = %v, want 2s", got)
	}
	if got := retryDelay(0, 1); got != 200*time.Millisecond {
		t.Fatalf("retryDelay(0, 1) = %v, want 200ms", got)
	}
}

// TestContractSerializesExplicitGenerationAndOmitsUnsupported 锁定生成参数的
// 线协议合同：显式值进入载荷（temperature=0 也不缺席），unsupported 字段整
// 键缺席，reasoning_effort=off 表达为字段省略；越界值在拨号前失败关闭。
func TestContractSerializesExplicitGenerationAndOmitsUnsupported(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		captured = body
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	temperature := 0
	client := Client{BaseURL: server.URL, Generation: &GenerationOptions{
		MaxOutputTokens: 4096, TemperatureMilli: &temperature, ToolChoice: "auto",
	}}
	if err := client.Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(captured, &payload); err != nil {
		t.Fatalf("decode captured payload: %v", err)
	}
	for key, want := range map[string]any{"max_tokens": float64(4096), "temperature": float64(0), "tool_choice": "auto"} {
		got, present := payload[key]
		if !present || got != want {
			t.Fatalf("payload[%q] = %v (present=%v), want %v", key, got, present, want)
		}
	}
	for _, absent := range []string{"top_p", "reasoning_effort"} {
		if _, present := payload[absent]; present {
			t.Fatalf("unsupported field %q must be omitted from the wire payload", absent)
		}
	}
	badTemperature := 2001
	rejecting := Client{BaseURL: server.URL, Generation: &GenerationOptions{TemperatureMilli: &badTemperature}}
	requestsBefore := len(payload)
	_ = requestsBefore
	err := rejecting.Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil })
	var providerError *ProviderError
	if !errors.As(err, &providerError) || providerError.Kind != "invalid_request" {
		t.Fatalf("out-of-contract generation must fail as invalid_request before dialing, got %v", err)
	}
}

// TestContractFailsClosedOnMissingUsage 验证声明流式 usage 的提供方在流结束
// 未回 usage 时按响应合同漂移失败关闭。
func TestContractFailsClosedOnMissingUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"stop"}}}
	err := client.Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil })
	if !strings.Contains(err.Error(), "without usage") {
		t.Fatalf("missing usage must fail closed, got %v", err)
	}
}

// TestContractFailsClosedOnUndeclaredStopReason 覆盖两种 stop reason 漂移：
// 声明集合外的取值与完全缺失的 finish reason 都必须在 Provider 层被拒。
func TestContractFailsClosedOnUndeclaredStopReason(t *testing.T) {
	writeStream := func(finish string) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			frame := "{\"choices\":[{\"delta\":{\"content\":\"x\"}"
			if finish != "" {
				frame += ",\"finish_reason\":\"" + finish + "\""
			}
			frame += "}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}"
			_, _ = writer.Write([]byte("data: " + frame + "\n\n"))
			_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		}
	}
	server := httptest.NewServer(writeStream("weird_stop"))
	defer server.Close()
	undeclared := Client{BaseURL: server.URL, Requirements: Requirements{AllowedStopReasons: []string{"stop", "length"}}}
	if err := undeclared.Stream(context.Background(), ports.ModelRequest{Model: "m"}, func(ports.ModelEvent) error { return nil }); err == nil || !strings.Contains(err.Error(), "undeclared finish reason") {
		t.Fatalf("undeclared reason must fail closed, got %v", err)
	}
	noFinishServer := httptest.NewServer(writeStream(""))
	defer noFinishServer.Close()
	missing := Client{BaseURL: noFinishServer.URL, Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"stop"}}}
	if err := missing.Stream(context.Background(), ports.ModelRequest{Model: "m"}, func(ports.ModelEvent) error { return nil }); err == nil || !strings.Contains(err.Error(), "without a finish reason") {
		t.Fatalf("missing finish reason must fail closed, got %v", err)
	}
}

// TestContractClassifiesRateLimitWithUnifiedVocabulary 锁定 429 的失败分类：
// 适配器必须复用统一失败词表的 rate_limited 并保持可重试，禁止自造
// rate_limit 别名造成词表漂移。
func TestContractClassifiesRateLimitWithUnifiedVocabulary(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.WriteHeader(http.StatusTooManyRequests)
		_, _ = writer.Write([]byte("rate limited"))
	}))
	defer server.Close()
	err := (Client{BaseURL: server.URL}).Stream(context.Background(), ports.ModelRequest{Model: "test"}, func(ports.ModelEvent) error { return nil })
	var providerError *ProviderError
	if err == nil || !errors.As(err, &providerError) {
		t.Fatalf("expected provider error, got %v", err)
	}
	if providerError.ErrorClass() != "rate_limited" {
		t.Fatalf("429 must classify as rate_limited, got %q", providerError.ErrorClass())
	}
	if !providerError.Retryable {
		t.Fatal("429 must remain retryable")
	}
}

// TestContractFailsClosedOnInconsistentUsage 锁定协议边界的用量自洽校验：
// 负数计数与 total 不等于分项之和的响应按 invalid_response 失败关闭且不可
// 重试；total 缺席（0）不构成不一致。
func TestContractFailsClosedOnInconsistentUsage(t *testing.T) {
	usageFrame := func(usage string) http.HandlerFunc {
		return func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}],\"usage\":" + usage + "}\n\n"))
			_, _ = writer.Write([]byte("data: [DONE]\n\n"))
		}
	}
	cases := []struct {
		name  string
		usage string
		want  string
	}{
		{"负数 token 计数", `{"prompt_tokens":-1,"completion_tokens":2,"total_tokens":1}`, "negative"},
		{"total 与分项不一致", `{"prompt_tokens":3,"completion_tokens":2,"total_tokens":7}`, "total does not equal"},
		{"total 缺席通过", `{"prompt_tokens":3,"completion_tokens":2}`, ""},
		{"分项一致通过", `{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}`, ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(usageFrame(testCase.usage))
			defer server.Close()
			client := Client{BaseURL: server.URL, Requirements: Requirements{RequireUsage: true, AllowedStopReasons: []string{"stop"}}}
			err := client.Stream(context.Background(), ports.ModelRequest{Model: "m"}, func(ports.ModelEvent) error { return nil })
			if testCase.want == "" {
				if err != nil {
					t.Fatalf("expected success, got %v", err)
				}
				return
			}
			var providerError *ProviderError
			if !errors.As(err, &providerError) || providerError.Kind != "invalid_response" || !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("expected non-retryable invalid_response containing %q, got %v", testCase.want, err)
			}
			if providerError.Retryable {
				t.Fatal("inconsistent usage must not be retried")
			}
		})
	}
}
