package openaicompat

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// Client 将 OpenAI-compatible Chat Completions SSE 响应适配为 Provider 端口。
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

// ProviderError 保留提供方失败分类、HTTP 状态、重试信息与原始原因。
type ProviderError struct {
	Kind       string
	StatusCode int
	Retryable  bool
	RetryAfter time.Duration
	Cause      error
}

// Error 返回包含提供方分类和可选 HTTP 状态的错误文本。
func (err *ProviderError) Error() string {
	if err.StatusCode != 0 {
		return fmt.Sprintf("provider %s (%d): %v", err.Kind, err.StatusCode, err.Cause)
	}
	return fmt.Sprintf("provider %s: %v", err.Kind, err.Cause)
}

// Unwrap 暴露底层 HTTP、传输或解析错误。
func (err *ProviderError) Unwrap() error { return err.Cause }

// Stream 以最多三次无语义输出的可重试请求转发模型流事件。
func (client Client) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if emit == nil {
		return errors.New("provider emit callback is required")
	}
	endpoint, err := chatCompletionsURL(client.BaseURL)
	if err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	body, err := json.Marshal(chatRequest{Model: request.Model, Messages: toOpenAIMessages(request.Messages), Tools: toOpenAITools(request.Tools), Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}})
	if err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	for attempt := 0; attempt < 3; attempt++ {
		semantic, err := client.streamOnce(ctx, endpoint, body, emit)
		if err == nil {
			return nil
		}
		var providerError *ProviderError
		if semantic || ctx.Err() != nil || attempt == 2 || !errors.As(err, &providerError) || !providerError.Retryable {
			return err
		}
		delay := providerError.RetryAfter
		if delay <= 0 {
			delay = time.Duration(attempt+1) * 100 * time.Millisecond
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("provider retry loop exited unexpectedly")
}

// streamOnce 发起一次 SSE 请求，并报告是否已收到不可安全重试的语义输出。
func (client Client) streamOnce(ctx context.Context, endpoint string, body []byte, emit func(ports.ModelEvent) error) (bool, error) {
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, &ProviderError{Kind: "invalid_request", Cause: err}
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	if client.APIKey != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+client.APIKey)
	}
	httpClient := client.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	response, err := httpClient.Do(httpRequest)
	if err != nil {
		return false, &ProviderError{Kind: "transport", Retryable: true, Cause: err}
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		preview, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		return false, &ProviderError{Kind: statusKind(response.StatusCode), StatusCode: response.StatusCode, Retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500, RetryAfter: retryAfter(response.Header.Get("Retry-After"), time.Now()), Cause: fmt.Errorf("%s", strings.TrimSpace(string(preview)))}
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream") {
		return false, &ProviderError{Kind: "invalid_response", StatusCode: response.StatusCode, Cause: fmt.Errorf("expected text/event-stream, got %q", response.Header.Get("Content-Type"))}
	}
	semantic := false
	err = decodeStream(ctx, response.Body, func(event ports.ModelEvent) error {
		if event.Kind != ports.Done {
			semantic = true
		}
		return emit(event)
	})
	return semantic, err
}

// retryAfter 解析 Retry-After 的秒数或 HTTP 日期，无法解析时返回零。
func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return time.Until(when)
	}
	return 0
}

// chatCompletionsURL 验证绝对 HTTP(S) 基地址并拼接 Chat Completions 路径。
func chatCompletionsURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL must be an absolute HTTP URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported base URL scheme %q", parsed.Scheme)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	return parsed.String(), nil
}

// decodeStream 解析 SSE data 帧，累计工具调用片段并按端口事件顺序转发。
func decodeStream(ctx context.Context, source io.Reader, emit func(ports.ModelEvent) error) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var dataLines []string
	toolCalls := map[int]*toolCallAccumulator{}
	done := false
	emitEvent := func(event ports.ModelEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(event)
	}
	flush := func() error {
		if len(dataLines) == 0 {
			return nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if data == "[DONE]" {
			if done {
				return &ProviderError{Kind: "invalid_response", Cause: errors.New("received duplicate done marker")}
			}
			orderedCalls, err := orderedToolCalls(toolCalls)
			if err != nil {
				return &ProviderError{Kind: "invalid_response", Cause: err}
			}
			for _, call := range orderedCalls {
				if !json.Valid(call.Arguments) {
					return &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("tool call %q has invalid JSON arguments", call.ID)}
				}
				copy := ports.ToolCall{ID: call.ID, Name: call.Name, Arguments: append(json.RawMessage(nil), call.Arguments...)}
				if err := emitEvent(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &copy}); err != nil {
					return err
				}
			}
			done = true
			return emitEvent(ports.ModelEvent{Kind: ports.Done})
		}
		var chunk chatChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("decode SSE JSON: %w", err)}
		}
		if chunk.Usage != nil {
			usage := ports.Usage{PromptTokens: chunk.Usage.PromptTokens, CompletionTokens: chunk.Usage.CompletionTokens, TotalTokens: chunk.Usage.TotalTokens}
			if err := emitEvent(ports.ModelEvent{Kind: ports.UsageEvent, Usage: &usage}); err != nil {
				return err
			}
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				if err := emitEvent(ports.ModelEvent{Kind: ports.TextDelta, Text: choice.Delta.Content}); err != nil {
					return err
				}
			}
			for _, fragment := range choice.Delta.ToolCalls {
				accumulator := toolCalls[fragment.Index]
				if accumulator == nil {
					accumulator = &toolCallAccumulator{Index: fragment.Index}
					toolCalls[fragment.Index] = accumulator
				}
				accumulator.ID += fragment.ID
				accumulator.Name += fragment.Function.Name
				accumulator.Arguments = append(accumulator.Arguments, fragment.Function.Arguments...)
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			if err := flush(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		}
	}
	if err := scanner.Err(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &ProviderError{Kind: "transport", Retryable: true, Cause: err}
	}
	if err := flush(); err != nil {
		return err
	}
	if !done {
		return &ProviderError{Kind: "invalid_response", Cause: errors.New("stream ended before [DONE]")}
	}
	return nil
}

// chatRequest 是 OpenAI-compatible Chat Completions 的流式请求载荷。
type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	Tools         []openAITool    `json:"tools,omitempty"`
	Stream        bool            `json:"stream"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

// openAIMessage 是提供方 API 中的单条会话消息。
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

// openAIToolCall 是提供方 API 采用 function 类型封装的工具调用。
type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

// openAIFunctionCall 保存工具调用的名称与 JSON 字符串实参。
type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// streamOptions 请求流响应额外返回 token 使用量。
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// openAITool 是提供方 API 的 function 工具包装。
type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

// openAIFunction 描述提供方 API 中可调用函数的 schema。
type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// toOpenAITools 将 Cairn 固定工具定义转换为提供方请求格式。
func toOpenAITools(specs []ports.ToolSpec) []openAITool {
	tools := make([]openAITool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, openAITool{Type: "function", Function: openAIFunction{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters}})
	}
	return tools
}

// toOpenAIMessages 将端口消息转换为提供方 API 消息格式。
func toOpenAIMessages(messages []ports.Message) []openAIMessage {
	converted := make([]openAIMessage, 0, len(messages))
	for _, message := range messages {
		item := openAIMessage{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			item.ToolCalls = append(item.ToolCalls, openAIToolCall{ID: call.ID, Type: "function", Function: openAIFunctionCall{Name: call.Name, Arguments: string(call.Arguments)}})
		}
		converted = append(converted, item)
	}
	return converted
}

// chatChunk 是 SSE data 帧中可包含增量内容和用量的响应片段。
type chatChunk struct {
	Choices []choice      `json:"choices"`
	Usage   *usagePayload `json:"usage"`
}

// choice 封装提供方返回的一个增量候选项。
type choice struct {
	Delta delta `json:"delta"`
}

// delta 保存候选项新增的文本和工具调用碎片。
type delta struct {
	Content   string             `json:"content"`
	ToolCalls []toolCallFragment `json:"tool_calls"`
}

// toolCallFragment 是流式工具调用在单帧内的局部字段。
type toolCallFragment struct {
	Index    int              `json:"index"`
	ID       string           `json:"id"`
	Function functionFragment `json:"function"`
}

// functionFragment 是流式函数名称和实参字符串的局部字段。
type functionFragment struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// usagePayload 是提供方返回的 token 用量字段。
type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// toolCallAccumulator 按索引拼接跨 SSE 帧到达的工具调用片段。
type toolCallAccumulator struct {
	Index     int
	ID        string
	Name      string
	Arguments []byte
}

// orderedToolCalls 校验完整工具调用并按提供方索引恢复稳定顺序。
func orderedToolCalls(calls map[int]*toolCallAccumulator) ([]*toolCallAccumulator, error) {
	ordered := make([]*toolCallAccumulator, 0, len(calls))
	for _, call := range calls {
		if call.ID == "" || call.Name == "" {
			return nil, fmt.Errorf("tool call at index %d is missing an id or function name", call.Index)
		}
		ordered = append(ordered, call)
	}
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].Index < ordered[right].Index })
	return ordered, nil
}

// statusKind 将 HTTP 状态码归类为稳定的提供方错误种类。
func statusKind(statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return "auth"
	case statusCode == http.StatusTooManyRequests:
		return "rate_limit"
	case statusCode >= 500:
		return "server"
	default:
		return "invalid_request"
	}
}
