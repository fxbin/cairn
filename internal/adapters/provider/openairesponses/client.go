// Package openairesponses 将 OpenAI Responses API（官方文档 2026-10-18 冻结）
// 的 SSE 流适配为 ports.Provider。与 Chat Completions 结构独立：请求使用
// input items（message / function_call / function_call_output），响应使用
// response.* 事件与 item/call 标识。首版覆盖文本流、工具参数增量、工具回执、
// 多轮连续性、usage 与终态。
//
// Author: fxbin
package openairesponses

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// Client 将 OpenAI Responses SSE 响应适配为 Provider 端口。
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// MaxOutputTokens 为可选输出上限；0 表示不发送。
	MaxOutputTokens int
	// TemperatureMilli/TopPMilli 为显式采样参数；nil 表示不发送。
	TemperatureMilli *int
	TopPMilli        *int
	// ToolChoice 为 auto|none|required 之一；空串表示不发送。
	ToolChoice string
	// Instructions 是可选顶层指令（developer/system）。
	Instructions string
	// Requirements 是由能力卡推导的响应合同要求。
	Requirements Requirements
}

// Requirements 是 Provider 层的响应闭合合同。
type Requirements struct {
	RequireUsage       bool
	AllowedStopReasons []string
}

// ProviderError 保留提供方失败分类、HTTP 状态、重试信息与安全的底层原因。
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

// ErrorClass 返回提供方失败分类。
func (err *ProviderError) ErrorClass() string { return err.Kind }

// IsRetryable 报告失败是否属于瞬态可重试类别。
func (err *ProviderError) IsRetryable() bool { return err.Retryable }

// Stream 以最多三次无语义输出的可重试请求转发模型流事件。
func (client Client) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if emit == nil {
		return errors.New("provider emit callback is required")
	}
	endpoint, err := responsesURL(client.BaseURL)
	if err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	payload := responseRequest{
		Model:  request.Model,
		Input:  toResponseInput(request.Messages),
		Tools:  toResponseTools(request.Tools),
		Stream: true,
	}
	if client.Instructions != "" {
		payload.Instructions = client.Instructions
	}
	if client.MaxOutputTokens > 0 {
		payload.MaxOutputTokens = client.MaxOutputTokens
	}
	if err := applyGeneration(&payload, client); err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	usageSeen := false
	stopReason := ""
	wrapped := func(event ports.ModelEvent) error {
		switch event.Kind {
		case ports.UsageEvent:
			usageSeen = true
			if event.Usage != nil {
				if err := validateUsageAccounting(event.Usage); err != nil {
					return &ProviderError{Kind: "invalid_response", Cause: err}
				}
			}
		case ports.Done:
			stopReason = event.StopReason
		}
		return emit(event)
	}
	for attempt := 0; attempt < 3; attempt++ {
		semantic, err := client.streamOnce(ctx, endpoint, body, wrapped)
		if err == nil {
			if err := client.checkStreamContract(usageSeen, stopReason); err != nil {
				return err
			}
			return nil
		}
		var providerError *ProviderError
		if semantic || ctx.Err() != nil || attempt == 2 || !errors.As(err, &providerError) || !providerError.Retryable {
			return err
		}
		timer := time.NewTimer(retryDelay(providerError.RetryAfter, attempt))
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return errors.New("provider retry loop exited unexpectedly")
}

// applyGeneration 写入显式采样参数。
func applyGeneration(target *responseRequest, client Client) error {
	if client.TemperatureMilli != nil {
		value := float64(*client.TemperatureMilli) / 1000.0
		target.Temperature = &value
	}
	if client.TopPMilli != nil {
		value := float64(*client.TopPMilli) / 1000.0
		target.TopP = &value
	}
	if client.ToolChoice != "" {
		switch client.ToolChoice {
		case "auto", "none", "required":
			target.ToolChoice = client.ToolChoice
		default:
			return fmt.Errorf("unsupported tool_choice %q", client.ToolChoice)
		}
	}
	return nil
}

// checkStreamContract 在一次流成功完成后核对响应闭合合同。
func (client Client) checkStreamContract(usageSeen bool, stopReason string) error {
	if client.Requirements.RequireUsage && !usageSeen {
		return &ProviderError{Kind: "invalid_response", Cause: errors.New("provider stream ended without usage")}
	}
	if len(client.Requirements.AllowedStopReasons) == 0 {
		return nil
	}
	if stopReason == "" {
		return &ProviderError{Kind: "invalid_response", Cause: errors.New("provider stream ended without a stop reason")}
	}
	for _, allowed := range client.Requirements.AllowedStopReasons {
		if allowed == stopReason {
			return nil
		}
	}
	return &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("undeclared stop reason %q", stopReason)}
}

// validateUsageAccounting 校验 usage 非负且合计一致。
func validateUsageAccounting(usage *ports.Usage) error {
	if usage == nil {
		return errors.New("provider usage is required")
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return errors.New("provider usage contains a negative token count")
	}
	if usage.TotalTokens != 0 && usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		return errors.New("provider usage total does not equal prompt plus completion")
	}
	return nil
}

// responsesURL 校验 base URL 并追加 /v1/responses 路径。
func responsesURL(baseURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("base URL must be an absolute HTTP URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("unsupported base URL scheme %q", parsed.Scheme)
	}
	if parsed.User != nil {
		return "", fmt.Errorf("base URL must not embed userinfo credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return "", fmt.Errorf("base URL must not carry query or fragment")
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/responses"
	return parsed.String(), nil
}

// maxRetryAfterDelay 限制服务端 Retry-After 可施加的单次休眠上限。
const maxRetryAfterDelay = 30 * time.Second

// retryDelay 计算单次重试等待。
func retryDelay(after time.Duration, attempt int) time.Duration {
	if after > maxRetryAfterDelay {
		return maxRetryAfterDelay
	}
	if after > 0 {
		return after
	}
	return time.Duration(attempt+1) * 100 * time.Millisecond
}

// streamOnce 发起一次 SSE 请求。
func (client Client) streamOnce(ctx context.Context, endpoint string, body []byte, emit func(ports.ModelEvent) error) (bool, error) {
	requestContext, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
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
		return false, classifyTransportError(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, classifyHTTPError(response)
	}
	return decodeStream(ctx, response.Body, emit)
}

// classifyTransportError 把传输层错误统一归入可重试 transport。
func classifyTransportError(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return &ProviderError{Kind: "transport", Retryable: true, Cause: err}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return &ProviderError{Kind: "transport", Retryable: true, Cause: err}
	}
	return &ProviderError{Kind: "transport", Retryable: true, Cause: err}
}

// classifyHTTPError 按 HTTP 状态与错误体分类。
func classifyHTTPError(response *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	kind, retryable := openAIErrorKind(response.StatusCode, payload)
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
	return &ProviderError{
		Kind: kind, StatusCode: response.StatusCode, Retryable: retryable, RetryAfter: retryAfter,
		Cause: fmt.Errorf("openai responses API error: %s", strings.TrimSpace(string(payload))),
	}
}

// openAIErrorKind 把状态码与错误体映射为失败分类。
func openAIErrorKind(statusCode int, payload []byte) (string, bool) {
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	switch {
	case statusCode == http.StatusTooManyRequests || envelope.Error.Code == "rate_limit_exceeded":
		return "rate_limited", true
	case statusCode >= 500:
		return "api_error", true
	case statusCode == http.StatusUnauthorized:
		return "authentication", false
	case statusCode == http.StatusForbidden:
		return "permission", false
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		return "transport", true
	case envelope.Error.Type == "invalid_request_error":
		return "invalid_request", false
	default:
		if statusCode >= 400 {
			return "invalid_request", false
		}
		return "api_error", true
	}
}

// parseRetryAfter 解析 Retry-After 秒数。
func parseRetryAfter(value string) time.Duration {
	if value == "" {
		return 0
	}
	if seconds, err := time.ParseDuration(value + "s"); err == nil && seconds > 0 {
		return seconds
	}
	return 0
}

// responseRequest 是 Responses API 请求载荷（与 Chat Completions 结构独立）。
type responseRequest struct {
	Model           string          `json:"model"`
	Input           []responseInput `json:"input"`
	Tools           []responseTool  `json:"tools,omitempty"`
	Stream          bool            `json:"stream"`
	Instructions    string          `json:"instructions,omitempty"`
	MaxOutputTokens int             `json:"max_output_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	ToolChoice      string          `json:"tool_choice,omitempty"`
}

// responseInput 是输入项判别联合的最小闭合集合。
type responseInput struct {
	Type      string `json:"type"`
	Role      string `json:"role,omitempty"`
	Content   string `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
}

// responseTool 是 function 工具声明。
type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toResponseTools 转换工具声明。
func toResponseTools(specs []ports.ToolSpec) []responseTool {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]responseTool, 0, len(specs))
	for _, spec := range specs {
		schema := spec.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, responseTool{Type: "function", Name: spec.Name, Description: spec.Description, Parameters: schema})
	}
	return tools
}

// toResponseInput 把端口消息转换为 Responses input items：message /
// function_call / function_call_output，与 Chat Completions 的 messages 结构独立。
func toResponseInput(messages []ports.Message) []responseInput {
	items := make([]responseInput, 0, len(messages)*2)
	for _, message := range messages {
		role := message.Role
		if role == "tool" {
			// 工具回执映射为 function_call_output。
			items = append(items, responseInput{Type: "function_call_output", CallID: message.ToolCallID, Output: message.Content})
			continue
		}
		if message.ToolCallID != "" {
			items = append(items, responseInput{Type: "function_call_output", CallID: message.ToolCallID, Output: message.Content})
		}
		if message.Content != "" {
			if role == "" {
				role = "user"
			}
			items = append(items, responseInput{Type: "message", Role: role, Content: message.Content})
		}
		for _, call := range message.ToolCalls {
			args := string(call.Arguments)
			if args == "" {
				args = "{}"
			}
			items = append(items, responseInput{Type: "function_call", CallID: call.ID, Name: call.Name, Arguments: args})
		}
	}
	return items
}

// streamEvent 是 SSE data 帧的判别联合。
type streamEvent struct {
	Type        string          `json:"type"`
	ItemID      string          `json:"item_id,omitempty"`
	OutputIndex int             `json:"output_index,omitempty"`
	Delta       string          `json:"delta,omitempty"`
	Item        json.RawMessage `json:"item,omitempty"`
	Response    json.RawMessage `json:"response,omitempty"`
}

// toolCallAccumulator 累积一个 function_call 的增量参数。
type toolCallAccumulator struct {
	id     string // item id (fc_...)
	callID string // call id (call_...) 供工具调用与回执
	name   string
	frames []string
}

// orderedToolCalls 按 id 排序拼接完整工具调用。
func orderedToolCalls(accumulated map[string]*toolCallAccumulator) ([]ports.ToolCall, error) {
	ids := make([]string, 0, len(accumulated))
	for id := range accumulated {
		ids = append(ids, id)
	}
	// 保持创建顺序：用 map 插入顺序在 Go 中不稳定，改用 slice 由调用方维护。
	calls := make([]ports.ToolCall, 0, len(ids))
	for _, id := range ids {
		item := accumulated[id]
		raw := strings.Join(item.frames, "")
		if strings.TrimSpace(raw) == "" {
			raw = "{}"
		}
		if !json.Valid([]byte(raw)) {
			return nil, ports.ErrMalformedToolCallArguments
		}
		calls = append(calls, ports.ToolCall{ID: item.id, Name: item.name, Arguments: json.RawMessage(raw)})
	}
	return calls, nil
}

// decodeStream 解析 Responses SSE 事件，拼接工具参数并转发端口事件。
func decodeStream(ctx context.Context, source io.Reader, emit func(ports.ModelEvent) error) (bool, error) {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var dataLines []string
	toolCalls := map[string]*toolCallAccumulator{}
	toolOrder := []string{}
	stopReason := ""
	done := false
	semantic := false
	emitEvent := func(event ports.ModelEvent) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return emit(event)
	}
	flush := func() (bool, error) {
		if len(dataLines) == 0 {
			return semantic, nil
		}
		data := strings.Join(dataLines, "\n")
		dataLines = nil
		if done {
			return semantic, nil
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return semantic, &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("decode responses stream event: %w", err)}
		}
		switch event.Type {
		case "response.created", "response.in_progress":
			semantic = true
		case "response.output_item.added":
			semantic = true
			var item struct {
				Type   string `json:"type"`
				ID     string `json:"id"`
				CallID string `json:"call_id"`
				Name   string `json:"name"`
			}
			_ = json.Unmarshal(event.Item, &item)
			if item.Type == "function_call" {
				// 官方流：item.id=fc_…、call_id=call_…、delta.item_id=fc_…
				// 以 item.id 为增量键，call_id 单独保留供工具调用标识。
				if _, exists := toolCalls[item.ID]; !exists {
					toolCalls[item.ID] = &toolCallAccumulator{id: item.ID, callID: item.CallID, name: item.Name}
					toolOrder = append(toolOrder, item.ID)
				}
			}
		case "response.output_text.delta":
			semantic = true
			if err := emitEvent(ports.ModelEvent{Kind: ports.TextDelta, Text: event.Delta}); err != nil {
				return semantic, err
			}
		case "response.function_call_arguments.delta":
			semantic = true
			target := toolCalls[event.ItemID]
			if target == nil {
				return semantic, &ProviderError{Kind: "invalid_response", Cause: errors.New("function_call_arguments.delta without a function_call item")}
			}
			target.frames = append(target.frames, event.Delta)
		case "response.output_item.done":
			semantic = true
		case "response.completed":
			semantic = true
			var payload struct {
				Status            string `json:"status"`
				IncompleteDetails *struct {
					Reason string `json:"reason"`
				} `json:"incomplete_details"`
				Usage *struct {
					InputTokens  int `json:"input_tokens"`
					OutputTokens int `json:"output_tokens"`
					TotalTokens  int `json:"total_tokens"`
				} `json:"usage"`
			}
			_ = json.Unmarshal(event.Response, &payload)
			if payload.Usage != nil {
				usage := &ports.Usage{PromptTokens: payload.Usage.InputTokens, CompletionTokens: payload.Usage.OutputTokens, TotalTokens: payload.Usage.TotalTokens}
				if usage.TotalTokens == 0 {
					usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				}
				if err := validateUsageAccounting(usage); err != nil {
					return semantic, &ProviderError{Kind: "invalid_response", Cause: err}
				}
				if err := emitEvent(ports.ModelEvent{Kind: ports.UsageEvent, Usage: usage}); err != nil {
					return semantic, err
				}
			}
			switch {
			case payload.Status == "completed":
				stopReason = "completed"
			case payload.Status == "incomplete" && payload.IncompleteDetails != nil:
				stopReason = mapIncompleteReason(payload.IncompleteDetails.Reason)
			case payload.Status == "failed":
				return semantic, &ProviderError{Kind: "api_error", Cause: errors.New("response failed")}
			default:
				stopReason = payload.Status
			}
			calls := make([]ports.ToolCall, 0, len(toolOrder))
			for _, id := range toolOrder {
				item := toolCalls[id]
				raw := strings.Join(item.frames, "")
				if strings.TrimSpace(raw) == "" {
					raw = "{}"
				}
				if !json.Valid([]byte(raw)) {
					return semantic, &ProviderError{Kind: "invalid_response", Cause: ports.ErrMalformedToolCallArguments}
				}
				callID := item.callID
				if callID == "" {
					callID = item.id
				}
				calls = append(calls, ports.ToolCall{ID: callID, Name: item.name, Arguments: json.RawMessage(raw)})
			}
			for _, call := range calls {
				call := call
				if err := emitEvent(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
					return semantic, err
				}
			}
			if err := emitEvent(ports.ModelEvent{Kind: ports.Done, StopReason: stopReason}); err != nil {
				return semantic, err
			}
			done = true
			return semantic, nil
		case "response.failed", "response.incomplete", "error":
			semantic = true
			return semantic, &ProviderError{Kind: "api_error", Cause: errors.New("response stream error: " + event.Type)}
		case "response.output_text.done", "response.function_call_arguments.done", "response.content_part.added", "response.content_part.done":
			// 中间态事件忽略。
		default:
			// 未知事件类型忽略，保持前向兼容。
		}
		return semantic, nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data:") {
			dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			continue
		}
		if strings.TrimSpace(line) == "" {
			if _, err := flush(); err != nil {
				return semantic, err
			}
			if done {
				return semantic, nil
			}
		}
	}
	if _, err := flush(); err != nil {
		return semantic, err
	}
	if err := scanner.Err(); err != nil {
		return semantic, classifyTransportError(err)
	}
	if !done {
		return semantic, &ProviderError{Kind: "invalid_response", Cause: errors.New("responses stream ended before response.completed")}
	}
	return semantic, nil
}

// mapIncompleteReason 把 incomplete_details.reason 映射为 stop reason 词表。
func mapIncompleteReason(reason string) string {
	switch reason {
	case "max_output_tokens", "max_messages", "content_filter", "steered":
		return reason
	default:
		return reason
	}
}
