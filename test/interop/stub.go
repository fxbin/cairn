package interop

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Step 是受控任务脚本中的一步。脚本是确定性的：同一个任务在任何 Harness
// 上都产生同一串工具调用，因此对照实验比较的是 Harness 行为而不是模型能力。
type Step struct {
	// Op 取 "exec"（执行 shell 脚本）或 "message"（只输出文本）。
	Op string `json:"op"`
	// Script 是 exec 步骤交给 shell 的脚本正文。
	Script string `json:"script,omitempty"`
	// Text 是 message 步骤输出的文本。
	Text string `json:"text,omitempty"`
	// StallMS 在本步响应前人为延迟毫秒数，用于制造"运行中"的可中断窗口。
	StallMS int `json:"stall_ms,omitempty"`
}

// TaskScript 是一次受控任务的确定性脚本。脚本随任务文本一起交给外部
// Harness，桩模型从对话中取回它并逐步回放。
type TaskScript struct {
	ID    string `json:"id"`
	Steps []Step `json:"steps"`
}

// 任务脚本在提示词中的包裹标记。桩模型只在对话文本中查找这对标记。
const (
	scriptMarkerOpen  = "<cairn-interop-task>"
	scriptMarkerClose = "</cairn-interop-task>"
)

// EncodeScript 把任务脚本渲染成可以随提示词投递的文本块。
func EncodeScript(script TaskScript) string {
	encoded, err := json.Marshal(script)
	if err != nil {
		return scriptMarkerOpen + scriptMarkerClose
	}
	return scriptMarkerOpen + "\n" + string(encoded) + "\n" + scriptMarkerClose
}

// ExtractScript 从一段自由文本中取回任务脚本；取不到时返回 false。
func ExtractScript(text string) (TaskScript, bool) {
	start := strings.Index(text, scriptMarkerOpen)
	if start < 0 {
		return TaskScript{}, false
	}
	rest := text[start+len(scriptMarkerOpen):]
	end := strings.Index(rest, scriptMarkerClose)
	if end < 0 {
		return TaskScript{}, false
	}
	var script TaskScript
	if err := json.Unmarshal([]byte(strings.TrimSpace(rest[:end])), &script); err != nil {
		return TaskScript{}, false
	}
	return script, true
}

// wireProtocol 是桩模型能回放的三种线协议；分别对应 Cairn 的三个适配器与
// 两个外部 CLI 的默认线协议。
type wireProtocol string

const (
	// protocolChatCompletions 对应 OpenAI Chat Completions 线协议，Cairn 原生
	// 默认走这一条。
	protocolChatCompletions wireProtocol = "chat-completions"
	// protocolResponses 对应 OpenAI Responses 线协议，Codex 的自定义端点
	// 要求这一条。
	protocolResponses wireProtocol = "responses"
	// protocolAnthropic 对应 Anthropic Messages 线协议，Claude Code 走这一条。
	protocolAnthropic wireProtocol = "anthropic-messages"
)

// StubRequest 是一次桩模型入站请求的留痕，用于事后核对某个 Harness 实际
// 发出的请求画像（协议、模型、可见工具、推进到第几步）。
type StubRequest struct {
	Protocol  wireProtocol
	Path      string
	Model     string
	Tools     []string
	StepIndex int
	HasScript bool
	Streamed  bool
}

// Stub 是本地桩模型服务。它在 127.0.0.1 上以随机端口提供三种线协议的
// 确定性回放，使 Cairn 原生、Codex 与 Claude Code 可以在零凭据、零外网的
// 条件下跑同一组任务。它不校验也不读取任何真实凭据。
type Stub struct {
	listener net.Listener
	server   *http.Server
	baseURL  string

	mutex    sync.Mutex
	requests []StubRequest
}

// StartStub 启动桩模型服务并返回句柄；调用方必须 Close 释放端口。
func StartStub() (*Stub, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen stub: %w", err)
	}
	stub := &Stub{listener: listener, baseURL: "http://" + listener.Addr().String()}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", stub.handleChatCompletions)
	mux.HandleFunc("/v1/responses", stub.handleResponses)
	mux.HandleFunc("/v1/messages", stub.handleAnthropicMessages)
	stub.server = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		_ = stub.server.Serve(listener)
	}()
	return stub, nil
}

// Close 关闭桩模型服务并释放端口。
func (stub *Stub) Close() error {
	if stub.server == nil {
		return nil
	}
	return stub.server.Close()
}

// BaseURL 返回桩模型服务的基地址（不含路径）。
func (stub *Stub) BaseURL() string {
	return stub.baseURL
}

// Requests 返回入站请求留痕的副本。
func (stub *Stub) Requests() []StubRequest {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	out := make([]StubRequest, len(stub.requests))
	copy(out, stub.requests)
	return out
}

// record 记录一次入站请求。
func (stub *Stub) record(request StubRequest) {
	stub.mutex.Lock()
	defer stub.mutex.Unlock()
	stub.requests = append(stub.requests, request)
}

// chatCompletionsRequest 是 Chat Completions 请求的最小投影。
type chatCompletionsRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

// responsesRequest 是 Responses API 请求的最小投影。
type responsesRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
	Input  []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"input"`
	Tools []struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"tools"`
}

// anthropicRequest 是 Anthropic Messages 请求的最小投影。system 与消息
// content 在不同轮次可能是字符串或块数组，因此按原始 JSON 保存后再用
// 容错解码提取，避免把协议形状变化误判成解码失败。
type anthropicRequest struct {
	Model    string          `json:"model"`
	Stream   bool            `json:"stream"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

// anthropicTextBlock 是一个可能带文本的块。
type anthropicTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// anthropicTexts 从一段可能是字符串、也可能是块数组的原始 JSON 中提取
// 所有文本片段。
func anthropicTexts(raw json.RawMessage) []string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil
		}
		return []string{text}
	}
	var blocks []anthropicTextBlock
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	var texts []string
	for _, block := range blocks {
		if block.Text != "" {
			texts = append(texts, block.Text)
		}
	}
	return texts
}

// anthropicToolResults 统计一段原始 JSON 中的 tool_result 块数量。
func anthropicToolResults(raw json.RawMessage) int {
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "[") {
		return 0
	}
	var blocks []struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return 0
	}
	count := 0
	for _, block := range blocks {
		if block.Type == "tool_result" {
			count++
		}
	}
	return count
}

// handleChatCompletions 回放 Chat Completions 协议。
func (stub *Stub) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "read body", http.StatusBadRequest)
		return
	}
	var decoded chatCompletionsRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		http.Error(writer, "decode body", http.StatusBadRequest)
		return
	}
	var texts []string
	toolResults := 0
	for _, message := range decoded.Messages {
		if message.Role == "tool" {
			toolResults++
		}
		if message.Role == "user" {
			texts = append(texts, message.Content)
		}
	}
	names := make([]string, 0, len(decoded.Tools))
	for _, tool := range decoded.Tools {
		names = append(names, tool.Function.Name)
	}
	script, hasScript := scriptFromTexts(texts)
	stub.record(StubRequest{Protocol: protocolChatCompletions, Path: request.URL.Path, Model: decoded.Model, Tools: names, StepIndex: toolResults, HasScript: hasScript, Streamed: decoded.Stream})

	if !decoded.Stream {
		stub.writeChatCompletionJSON(writer, decoded.Model, script, toolResults, hasScript)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	write := func(payload string) {
		_, _ = io.WriteString(writer, "data: "+payload+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	stub.playChatCompletions(write, decoded.Model, script, toolResults, hasScript)
}

// writeChatUsage 写出一段带确定性用量的 Chat Completions 用量块。
func (stub *Stub) writeChatUsage(write func(string), stepIndex int) {
	input := stubInputTokens(stepIndex)
	output := stubOutputTokens(stepIndex)
	write(fmt.Sprintf(`{"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		input, output, input+output))
}

// playChatCompletions 按当前步号回放一段 Chat Completions 流。
func (stub *Stub) playChatCompletions(write func(string), model string, script TaskScript, stepIndex int, hasScript bool) {
	if !hasScript {
		write(`{"choices":[{"delta":{"content":"INTEROP_NO_SCRIPT"},"finish_reason":"stop"}]}`)
		stub.writeChatUsage(write, stepIndex)
		write("[DONE]")
		return
	}
	if stepIndex >= len(script.Steps) {
		write(`{"choices":[{"delta":{"content":"INTEROP_DONE"},"finish_reason":"stop"}]}`)
		stub.writeChatUsage(write, stepIndex)
		write("[DONE]")
		return
	}
	step := script.Steps[stepIndex]
	if step.Op == "message" {
		write(fmt.Sprintf(`{"choices":[{"delta":{"content":%s},"finish_reason":"stop"}]}`, mustJSONString(step.Text)))
		stub.writeChatUsage(write, stepIndex)
		write("[DONE]")
		return
	}
	stall(step)
	arguments := mustJSONString(mustJSONString(map[string]any{"argv": []string{"bash", "-lc", step.Script}}))
	write(fmt.Sprintf(`{"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_interop_%d","function":{"name":"exec_command","arguments":%s}}]},"finish_reason":null}]}`, stepIndex, arguments))
	write(`{"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`)
	stub.writeChatUsage(write, stepIndex)
	write("[DONE]")
}

// writeChatCompletionJSON 回放非流式 Chat Completions 响应。
func (stub *Stub) writeChatCompletionJSON(writer http.ResponseWriter, model string, script TaskScript, stepIndex int, hasScript bool) {
	message := map[string]any{"role": "assistant", "content": "INTEROP_DONE"}
	finish := "stop"
	if hasScript && stepIndex < len(script.Steps) {
		step := script.Steps[stepIndex]
		stall(step)
		if step.Op == "exec" {
			message = map[string]any{
				"role":    "assistant",
				"content": nil,
				"tool_calls": []any{map[string]any{
					"id":   fmt.Sprintf("call_interop_%d", stepIndex),
					"type": "function",
					"function": map[string]any{
						"name":      "exec_command",
						"arguments": mustJSONString(mustJSONString(map[string]any{"argv": []string{"bash", "-lc", step.Script}})),
					},
				}},
			}
			finish = "tool_calls"
		}
	}
	payload := map[string]any{
		"id":      "chatcmpl-interop",
		"object":  "chat.completion",
		"created": 0,
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	}
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(payload)
}

// handleResponses 回放 Responses 协议。
func (stub *Stub) handleResponses(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "read body", http.StatusBadRequest)
		return
	}
	var decoded responsesRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		http.Error(writer, "decode body", http.StatusBadRequest)
		return
	}
	var texts []string
	toolResults := 0
	for _, item := range decoded.Input {
		if item.Type == "function_call_output" {
			toolResults++
		}
		if item.Type == "message" && item.Role == "user" {
			for _, part := range item.Content {
				texts = append(texts, part.Text)
			}
		}
	}
	names := make([]string, 0, len(decoded.Tools))
	for _, tool := range decoded.Tools {
		if tool.Name != "" {
			names = append(names, tool.Name)
		}
	}
	script, hasScript := scriptFromTexts(texts)
	stub.record(StubRequest{Protocol: protocolResponses, Path: request.URL.Path, Model: decoded.Model, Tools: names, StepIndex: toolResults, HasScript: hasScript, Streamed: decoded.Stream})

	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	writeEvent := func(name string, payload any) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = io.WriteString(writer, "event: "+name+"\ndata: "+string(encoded)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	stub.playResponses(writeEvent, decoded.Model, script, toolResults, hasScript)
}

// playResponses 按当前步号回放一段 Responses 流。
func (stub *Stub) playResponses(writeEvent func(string, any), model string, script TaskScript, stepIndex int, hasScript bool) {
	writeEvent("response.created", map[string]any{
		"type":     "response.created",
		"response": map[string]any{"id": "resp_interop", "object": "response", "status": "in_progress", "model": model},
	})
	writeEvent("response.in_progress", map[string]any{
		"type":     "response.in_progress",
		"response": map[string]any{"id": "resp_interop", "object": "response", "status": "in_progress", "model": model},
	})
	if !hasScript {
		stub.emitResponsesText(writeEvent, "INTEROP_NO_SCRIPT")
		stub.finishResponses(writeEvent, model, stepIndex)
		return
	}
	if stepIndex >= len(script.Steps) {
		stub.emitResponsesText(writeEvent, "INTEROP_DONE")
		stub.finishResponses(writeEvent, model, stepIndex)
		return
	}
	step := script.Steps[stepIndex]
	if step.Op == "message" {
		stub.emitResponsesText(writeEvent, step.Text)
		stub.finishResponses(writeEvent, model, stepIndex)
		return
	}
	stall(step)
	arguments := mustJSONString(map[string]any{"cmd": step.Script})
	callID := fmt.Sprintf("call_interop_%d", stepIndex)
	itemID := fmt.Sprintf("fc_interop_%d", stepIndex)
	writeEvent("response.output_item.added", map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"type": "function_call", "id": itemID, "call_id": callID,
			"name": "exec_command", "arguments": "", "status": "in_progress",
		},
	})
	writeEvent("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "output_index": 0,
		"item_id": itemID, "delta": arguments,
	})
	writeEvent("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "output_index": 0,
		"item_id": itemID, "arguments": arguments,
	})
	writeEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{
			"type": "function_call", "id": itemID, "call_id": callID,
			"name": "exec_command", "arguments": arguments, "status": "completed",
		},
	})
	stub.finishResponses(writeEvent, model, stepIndex)
}

// emitResponsesText 回放一段助手文本输出。
func (stub *Stub) emitResponsesText(writeEvent func(string, any), text string) {
	writeEvent("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "output_index": 0,
		"item": map[string]any{"type": "message", "id": "msg_interop", "role": "assistant", "status": "in_progress", "content": []any{}},
	})
	writeEvent("response.content_part.added", map[string]any{
		"type": "response.content_part.added", "output_index": 0, "item_id": "msg_interop",
		"part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}},
	})
	writeEvent("response.output_text.delta", map[string]any{
		"type": "response.output_text.delta", "output_index": 0, "item_id": "msg_interop", "delta": text,
	})
	writeEvent("response.output_text.done", map[string]any{
		"type": "response.output_text.done", "output_index": 0, "item_id": "msg_interop", "text": text,
	})
	writeEvent("response.content_part.done", map[string]any{
		"type": "response.content_part.done", "output_index": 0, "item_id": "msg_interop",
		"part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}},
	})
	writeEvent("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "output_index": 0,
		"item": map[string]any{"type": "message", "id": "msg_interop", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}},
	})
}

// finishResponses 结束一段 Responses 流并附上用量。
func (stub *Stub) finishResponses(writeEvent func(string, any), model string, stepIndex int) {
	input := stubInputTokens(stepIndex)
	output := stubOutputTokens(stepIndex)
	writeEvent("response.completed", map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id": "resp_interop", "object": "response", "status": "completed", "model": model,
			"usage": map[string]any{
				"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
			},
		},
	})
}

// handleAnthropicMessages 回放 Anthropic Messages 协议。
func (stub *Stub) handleAnthropicMessages(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, "read body", http.StatusBadRequest)
		return
	}
	var decoded anthropicRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		http.Error(writer, "decode body", http.StatusBadRequest)
		return
	}
	var texts []string
	texts = append(texts, anthropicTexts(decoded.System)...)
	toolResults := 0
	for _, message := range decoded.Messages {
		toolResults += anthropicToolResults(message.Content)
		if message.Role == "user" {
			texts = append(texts, anthropicTexts(message.Content)...)
		}
	}
	names := make([]string, 0, len(decoded.Tools))
	for _, tool := range decoded.Tools {
		names = append(names, tool.Name)
	}
	script, hasScript := scriptFromTexts(texts)
	stub.record(StubRequest{Protocol: protocolAnthropic, Path: request.URL.Path, Model: decoded.Model, Tools: names, StepIndex: toolResults, HasScript: hasScript, Streamed: decoded.Stream})

	if !decoded.Stream {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"msg_interop","type":"message","role":"assistant","model":"`+decoded.Model+`","content":[{"type":"text","text":"INTEROP_DONE"}],"stop_reason":"end_turn","usage":{"input_tokens":0,"output_tokens":0}}`)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.WriteHeader(http.StatusOK)
	flusher, _ := writer.(http.Flusher)
	writeEvent := func(name string, payload any) {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return
		}
		_, _ = io.WriteString(writer, "event: "+name+"\ndata: "+string(encoded)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}
	stub.playAnthropic(writeEvent, decoded.Model, script, toolResults, hasScript)
}

// playAnthropic 按当前步号回放一段 Anthropic Messages 流。
func (stub *Stub) playAnthropic(writeEvent func(string, any), model string, script TaskScript, stepIndex int, hasScript bool) {
	writeEvent("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_interop", "type": "message", "role": "assistant", "model": model,
			"content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": stubInputTokens(stepIndex), "output_tokens": 0},
		},
	})
	if !hasScript {
		stub.emitAnthropicText(writeEvent, "INTEROP_NO_SCRIPT")
		stub.finishAnthropic(writeEvent, "end_turn", stepIndex)
		return
	}
	if stepIndex >= len(script.Steps) {
		stub.emitAnthropicText(writeEvent, "INTEROP_DONE")
		stub.finishAnthropic(writeEvent, "end_turn", stepIndex)
		return
	}
	step := script.Steps[stepIndex]
	if step.Op == "message" {
		stub.emitAnthropicText(writeEvent, step.Text)
		stub.finishAnthropic(writeEvent, "end_turn", stepIndex)
		return
	}
	stall(step)
	arguments := mustJSONString(map[string]any{"command": step.Script, "description": "interop task step"})
	writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "tool_use", "id": "toolu_interop_" + fmt.Sprint(stepIndex), "name": "Bash", "input": map[string]any{}},
	})
	writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": arguments},
	})
	writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	stub.finishAnthropic(writeEvent, "tool_use", stepIndex)
}

// emitAnthropicText 回放一段助手文本输出。
func (stub *Stub) emitAnthropicText(writeEvent func(string, any), text string) {
	writeEvent("content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	writeEvent("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]any{"type": "text_delta", "text": text},
	})
	writeEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
}

// finishAnthropic 结束一段 Anthropic 流并附上停止原因。
func (stub *Stub) finishAnthropic(writeEvent func(string, any), stopReason string, stepIndex int) {
	writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": stubOutputTokens(stepIndex)},
	})
	writeEvent("message_stop", map[string]any{"type": "message_stop"})
}

// stubInputTokens 与 stubOutputTokens 是桩模型报告的确定性用量：用量随
// 步号线性增长，使"外壳是否如实转述提供方用量"成为可核对的量，而不是
// 全零的不可观测值。
func stubInputTokens(stepIndex int) int { return 1000 + 100*stepIndex }

// stubOutputTokens 返回第 stepIndex 步的确定性输出用量。
func stubOutputTokens(stepIndex int) int { return 120 + 10*stepIndex }

// scriptFromTexts 在候选文本中查找任务脚本；取不到时返回零值与 false。
func scriptFromTexts(texts []string) (TaskScript, bool) {
	for index := len(texts) - 1; index >= 0; index-- {
		if script, ok := ExtractScript(texts[index]); ok {
			return script, true
		}
	}
	return TaskScript{}, false
}

// stall 按脚本声明延迟本步响应，用于制造可中断窗口。
func stall(step Step) {
	if step.StallMS <= 0 {
		return
	}
	time.Sleep(time.Duration(step.StallMS) * time.Millisecond)
}

// mustJSONString 把任意值渲染为 JSON 文本；本包只用于构造桩响应体。
func mustJSONString(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}
