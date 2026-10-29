package interop

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// stubTestTimeout 是桩模型协议测试的单次请求时限。
const stubTestTimeout = 15 * time.Second

// postStub 向桩模型发一次请求并返回原始响应体。
func postStub(t *testing.T, stub *Stub, path string, payload any) string {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), stubTestTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, stub.BaseURL()+path, strings.NewReader(string(encoded)))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("post %s 状态码 %d，body=%s", path, response.StatusCode, truncate(string(body), 300))
	}
	return string(body)
}

// sseDataLines 提取 SSE 响应里的全部 data 行。
func sseDataLines(body string) []string {
	var lines []string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "data:") {
			lines = append(lines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	return lines
}

// sseEventNames 提取 SSE 响应里的全部事件名。
func sseEventNames(body string) []string {
	var names []string
	scanner := bufio.NewScanner(strings.NewReader(body))
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "event:") {
			names = append(names, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	return names
}

// TestStubScriptRoundTrip 锁定任务脚本的封装与取回：标记一旦被外部 CLI
// 原样透传，桩模型必须能原样取回，否则脚本步号无法推进。
func TestStubScriptRoundTrip(t *testing.T) {
	script := TaskScript{ID: "demo", Steps: []Step{
		{Op: "exec", Script: "printf 'x' > a.txt"},
		{Op: "message", Text: "done"},
	}}
	encoded := EncodeScript(script)
	if !strings.Contains(encoded, scriptMarkerOpen) || !strings.Contains(encoded, scriptMarkerClose) {
		t.Fatalf("封装结果缺少标记: %s", encoded)
	}
	decoded, ok := ExtractScript(encoded)
	if !ok {
		t.Fatalf("无法取回脚本: %s", encoded)
	}
	if decoded.ID != script.ID || len(decoded.Steps) != len(script.Steps) {
		t.Fatalf("取回的脚本与原脚本不一致: %#v", decoded)
	}
	if decoded.Steps[0].Script != script.Steps[0].Script {
		t.Fatalf("步内容不一致: %#v", decoded.Steps[0])
	}
	if _, ok := ExtractScript("没有标记的自由文本"); ok {
		t.Fatal("没有标记时不应取回脚本")
	}
}

// TestStubChatCompletionsProtocol 锁定 Chat Completions 线协议的桩响应形态：
// 工具调用以分片形式出现，usage 独立成块，结尾是 [DONE]。
func TestStubChatCompletionsProtocol(t *testing.T) {
	stub, err := StartStub()
	if err != nil {
		t.Fatalf("start stub: %v", err)
	}
	defer stub.Close()

	script := TaskScript{ID: "chat", Steps: []Step{{Op: "exec", Script: "printf 'hi' > a.txt"}}}
	body := postStub(t, stub, "/v1/chat/completions", map[string]any{
		"model":    "stub-model",
		"stream":   true,
		"messages": []any{map[string]any{"role": "user", "content": EncodeScript(script)}},
	})
	lines := sseDataLines(body)
	if len(lines) == 0 {
		t.Fatalf("没有 SSE 数据行: %s", body)
	}
	if lines[len(lines)-1] != "[DONE]" {
		t.Fatalf("流未以 [DONE] 结尾: %v", lines[len(lines)-1])
	}
	var sawToolCall, sawUsage bool
	for _, line := range lines {
		if strings.Contains(line, "\"tool_calls\"") {
			sawToolCall = true
		}
		if strings.Contains(line, "\"usage\"") {
			sawUsage = true
		}
	}
	if !sawToolCall {
		t.Fatalf("没有观察到工具调用分片: %v", lines)
	}
	if !sawUsage {
		t.Fatalf("没有观察到 usage 块: %v", lines)
	}
	requests := stub.Requests()
	if len(requests) != 1 {
		t.Fatalf("桩模型入站请求数 = %d，期望 1", len(requests))
	}
	if requests[0].Protocol != protocolChatCompletions || requests[0].StepIndex != 0 {
		t.Fatalf("入站请求留痕不符: %#v", requests[0])
	}
}

// TestStubResponsesProtocol 锁定 Responses 线协议的桩响应形态：函数调用
// 参数以增量事件出现，终态事件携带用量。
func TestStubResponsesProtocol(t *testing.T) {
	stub, err := StartStub()
	if err != nil {
		t.Fatalf("start stub: %v", err)
	}
	defer stub.Close()

	script := TaskScript{ID: "responses", Steps: []Step{{Op: "exec", Script: "printf 'hi' > a.txt"}}}
	body := postStub(t, stub, "/v1/responses", map[string]any{
		"model":  "stub-model",
		"stream": true,
		"input": []any{map[string]any{
			"type": "message", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": EncodeScript(script)}},
		}},
	})
	names := sseEventNames(body)
	for _, wanted := range []string{"response.created", "response.output_item.added", "response.function_call_arguments.delta", "response.completed"} {
		if !containsString(names, wanted) {
			t.Fatalf("缺少事件 %s，实际事件 %v", wanted, names)
		}
	}
	if !strings.Contains(body, fmt.Sprintf("%d", stubInputTokens(0))) {
		t.Fatalf("终态事件没有携带确定性用量: %s", truncate(body, 400))
	}
}

// TestStubAnthropicProtocol 锁定 Anthropic Messages 线协议的桩响应形态：
// 工具输入以 input_json_delta 增量出现，终态事件携带停止原因。
func TestStubAnthropicProtocol(t *testing.T) {
	stub, err := StartStub()
	if err != nil {
		t.Fatalf("start stub: %v", err)
	}
	defer stub.Close()

	script := TaskScript{ID: "anthropic", Steps: []Step{{Op: "exec", Script: "printf 'hi' > a.txt"}}}
	body := postStub(t, stub, "/v1/messages", map[string]any{
		"model":  "stub-model",
		"stream": true,
		"system": []any{map[string]any{"type": "text", "text": "系统提示"}},
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": EncodeScript(script)}},
		}},
	})
	names := sseEventNames(body)
	for _, wanted := range []string{"message_start", "content_block_start", "content_block_delta", "message_delta", "message_stop"} {
		if !containsString(names, wanted) {
			t.Fatalf("缺少事件 %s，实际事件 %v", wanted, names)
		}
	}
	if !strings.Contains(body, `"tool_use"`) {
		t.Fatalf("没有观察到 tool_use 内容块: %s", truncate(body, 400))
	}
}

// TestStubStepProgression 锁定步号推进：对话里出现一次工具结果后，桩模型
// 应当推进到下一步；三侧外壳的"任务是否走完"判定依赖这条不变量。
func TestStubStepProgression(t *testing.T) {
	stub, err := StartStub()
	if err != nil {
		t.Fatalf("start stub: %v", err)
	}
	defer stub.Close()

	script := TaskScript{ID: "progression", Steps: []Step{
		{Op: "exec", Script: "printf '1' > a.txt"},
	}}
	first := postStub(t, stub, "/v1/messages", map[string]any{
		"model":  "stub-model",
		"stream": true,
		"messages": []any{map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": EncodeScript(script)}},
		}},
	})
	if !strings.Contains(first, `"tool_use"`) {
		t.Fatalf("首轮应当给出工具调用: %s", truncate(first, 300))
	}
	second := postStub(t, stub, "/v1/messages", map[string]any{
		"model":  "stub-model",
		"stream": true,
		"messages": []any{
			map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": EncodeScript(script)}},
			},
			map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Bash", "input": map[string]any{}}},
			},
			map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"}},
			},
		},
	})
	if strings.Contains(second, `"tool_use"`) {
		t.Fatalf("末轮不应再给出工具调用: %s", truncate(second, 300))
	}
	requests := stub.Requests()
	if len(requests) != 2 {
		t.Fatalf("入站请求数 = %d，期望 2", len(requests))
	}
	if requests[0].StepIndex != 0 || requests[1].StepIndex != 1 {
		t.Fatalf("步号未按工具结果条数推进: %#v", requests)
	}
}

// containsString 判断字符串切片是否包含目标值。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
