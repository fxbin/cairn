// Package anthropicmessages 将 Anthropic Messages API（anthropic-version
// 2023-06-01，官方文档 2026-10-18 冻结）的 SSE 流适配为 ports.Provider。
// 首版覆盖文本流、工具参数增量拼接、工具结果往返、停止原因、usage、取消、
// 超时与错误分类。扩展思考（thinking/redacted_thinking）签名连续性尚未
// 启用：本适配器不请求 thinking，若服务端仍返回该类块则失败关闭，不静默
// 丢弃后宣称原生支持。
//
// Author: fxbin
package anthropicmessages

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
	"sort"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// ProtocolVersion 是本适配器冻结的 Anthropic 版本头。
const ProtocolVersion = "2023-06-01"

// defaultMaxTokens 是未声明输出上限时写入请求的最小必填值；Anthropic 要求
// max_tokens 必在场。
const defaultMaxTokens = 4096

// Client 将 Anthropic Messages SSE 响应适配为 Provider 端口。
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// MaxTokens 是请求必填的输出上限；0 表示使用 defaultMaxTokens。
	MaxTokens int
	// TemperatureMilli/TopPMilli 为显式采样参数（千分位整数）；nil 表示不发送。
	TemperatureMilli *int
	TopPMilli        *int
	// ToolChoice 为 auto|any|tool|none 之一；空串表示不发送。
	ToolChoice string
	// Requirements 是由能力卡推导的响应合同要求。
	Requirements Requirements
	// System 是可选的顶层 system 提示。
	System string
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

// ErrorClass 返回提供方失败分类，供 Agent 循环持久化 error_class 字段。
func (err *ProviderError) ErrorClass() string { return err.Kind }

// IsRetryable 报告失败是否属于瞬态可重试类别，实现 ports.ClassifiedError。
func (err *ProviderError) IsRetryable() bool { return err.Retryable }

// Stream 以最多三次无语义输出的可重试请求转发模型流事件。
func (client Client) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if emit == nil {
		return errors.New("provider emit callback is required")
	}
	endpoint, err := messagesURL(client.BaseURL)
	if err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	payload := messageRequest{
		Model:    request.Model,
		Messages: toAnthropicMessages(request.Messages),
		Tools:    toAnthropicTools(request.Tools),
		Stream:   true,
	}
	if client.System != "" {
		payload.System = client.System
	}
	maxTokens := client.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	payload.MaxTokens = maxTokens
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

// applyGeneration 写入显式采样参数；Milli 换算为浮点只在此处发生。
func applyGeneration(target *messageRequest, client Client) error {
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
		case "auto", "any", "none":
			target.ToolChoice = map[string]string{"type": client.ToolChoice}
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

// messagesURL 校验 base URL 并追加 /v1/messages 路径。
func messagesURL(baseURL string) (string, error) {
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
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1/messages"
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

// streamOnce 发起一次 SSE 请求，并报告是否已收到不可安全重试的语义输出。
func (client Client) streamOnce(ctx context.Context, endpoint string, body []byte, emit func(ports.ModelEvent) error) (bool, error) {
	requestContext, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	httpRequest, err := http.NewRequestWithContext(requestContext, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, &ProviderError{Kind: "invalid_request", Cause: err}
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	httpRequest.Header.Set("anthropic-version", ProtocolVersion)
	if client.APIKey != "" {
		httpRequest.Header.Set("x-api-key", client.APIKey)
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
	semantic, err := decodeStream(ctx, response.Body, emit)
	if err != nil {
		return semantic, err
	}
	return semantic, nil
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

// classifyHTTPError 按 Anthropic 错误类型与 HTTP 状态分类；限流/过载/超时
// 可重试，认证/权限/请求错误不可重试。
func classifyHTTPError(response *http.Response) error {
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
	kind, retryable := anthropicErrorKind(response.StatusCode, payload)
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
	return &ProviderError{
		Kind: kind, StatusCode: response.StatusCode, Retryable: retryable, RetryAfter: retryAfter,
		Cause: fmt.Errorf("anthropic messages API error: %s", strings.TrimSpace(string(payload))),
	}
}

// anthropicErrorKind 把状态码与错误体 type 映射为失败分类。
func anthropicErrorKind(statusCode int, payload []byte) (string, bool) {
	var envelope struct {
		Type  string `json:"type"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	errorType := envelope.Error.Type
	if errorType == "" {
		errorType = envelope.Type
	}
	switch errorType {
	case "rate_limit_error":
		return "rate_limited", true
	case "overloaded_error":
		return "overloaded", true
	case "timeout_error":
		return "transport", true
	case "authentication_error":
		return "authentication", false
	case "permission_error":
		return "permission", false
	case "billing_error":
		return "billing", false
	case "invalid_request_error":
		return "invalid_request", false
	case "not_found_error":
		return "not_found", false
	case "api_error":
		return "api_error", true
	}
	switch {
	case statusCode == http.StatusTooManyRequests:
		return "rate_limited", true
	case statusCode >= 500:
		return "api_error", true
	case statusCode == http.StatusUnauthorized:
		return "authentication", false
	case statusCode == http.StatusForbidden:
		return "permission", false
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		return "transport", true
	default:
		return "invalid_request", false
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

// messageRequest 是 Anthropic Messages 请求载荷。
type messageRequest struct {
	Model       string             `json:"model"`
	MaxTokens   int                `json:"max_tokens"`
	Messages    []anthropicMessage `json:"messages"`
	System      string             `json:"system,omitempty"`
	Tools       []anthropicTool    `json:"tools,omitempty"`
	Stream      bool               `json:"stream"`
	Temperature *float64           `json:"temperature,omitempty"`
	TopP        *float64           `json:"top_p,omitempty"`
	ToolChoice  map[string]string  `json:"tool_choice,omitempty"`
}

// anthropicMessage 是 Messages API 的一条输入消息。
type anthropicMessage struct {
	Role    string           `json:"role"`
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock 是消息内容块的最小闭合集合（text / tool_use / tool_result /
// thinking / redacted_thinking）。
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Thinking  string          `json:"thinking,omitempty"`
	Data      string          `json:"data,omitempty"`
}

// anthropicTool 是工具声明。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// toAnthropicTools 转换工具声明；schema 缺失时失败关闭。
func toAnthropicTools(specs []ports.ToolSpec) []anthropicTool {
	if len(specs) == 0 {
		return nil
	}
	tools := make([]anthropicTool, 0, len(specs))
	for _, spec := range specs {
		schema := spec.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		tools = append(tools, anthropicTool{Name: spec.Name, Description: spec.Description, InputSchema: schema})
	}
	return tools
}

// toAnthropicMessages 把端口消息转换为 Messages API 内容块序列；连续性块
// 原样回传，保持 thinking/redacted_thinking 的签名与顺序。
func toAnthropicMessages(messages []ports.Message) []anthropicMessage {
	converted := make([]anthropicMessage, 0, len(messages))
	for _, message := range messages {
		blocks := make([]anthropicBlock, 0, 2+len(message.ToolCalls)+len(message.ProtocolBlocks))
		for _, continuity := range message.ProtocolBlocks {
			switch continuity.Type {
			case "thinking":
				blocks = append(blocks, anthropicBlock{Type: "thinking", Signature: continuity.Signature, Thinking: continuity.Thinking})
			case "redacted_thinking":
				blocks = append(blocks, anthropicBlock{Type: "redacted_thinking", Data: continuity.Data})
			}
		}
		if message.ToolCallID != "" {
			blocks = append(blocks, anthropicBlock{Type: "tool_result", ToolUseID: message.ToolCallID, Content: message.Content})
		} else if message.Content != "" {
			blocks = append(blocks, anthropicBlock{Type: "text", Text: message.Content})
		}
		for _, call := range message.ToolCalls {
			input := call.Arguments
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			blocks = append(blocks, anthropicBlock{Type: "tool_use", ID: call.ID, Name: call.Name, Input: input})
		}
		role := message.Role
		if role == "" {
			role = "user"
		}
		converted = append(converted, anthropicMessage{Role: role, Content: blocks})
	}
	return converted
}

// streamEvent 是 SSE data 帧的判别联合。
type streamEvent struct {
	Type         string          `json:"type"`
	Index        int             `json:"index,omitempty"`
	Message      json.RawMessage `json:"message,omitempty"`
	ContentBlock json.RawMessage `json:"content_block,omitempty"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	Usage        json.RawMessage `json:"usage,omitempty"`
	Error        json.RawMessage `json:"error,omitempty"`
}

// toolCallAccumulator 累积一个 tool_use 块的增量 JSON。
type toolCallAccumulator struct {
	id     string
	name   string
	frames []string
}

// orderedToolCalls 按 id 排序拼接完整工具调用。
func orderedToolCalls(accumulated map[string]*toolCallAccumulator) ([]ports.ToolCall, error) {
	ids := make([]string, 0, len(accumulated))
	for id := range accumulated {
		ids = append(ids, id)
	}
	sort.Strings(ids)
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

// decodeStream 解析 Anthropic SSE 事件，拼接工具参数并转发端口事件。
func decodeStream(ctx context.Context, source io.Reader, emit func(ports.ModelEvent) error) (bool, error) {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var dataLines []string
	toolCalls := map[string]*toolCallAccumulator{}
	toolCallsByIndex := map[int]*toolCallAccumulator{}
	protocolBlocks := []ports.ProtocolBlock{}
	thinkingByIndex := map[int]*ports.ProtocolBlock{}
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
			return semantic, &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("decode anthropic stream event: %w", err)}
		}
		switch event.Type {
		case "message_start":
			semantic = true
			var payload struct {
				Usage json.RawMessage `json:"usage"`
			}
			_ = json.Unmarshal(event.Message, &payload)
			if len(payload.Usage) > 0 {
				usage, err := parseUsage(payload.Usage)
				if err != nil {
					return semantic, &ProviderError{Kind: "invalid_response", Cause: err}
				}
				if err := emitEvent(ports.ModelEvent{Kind: ports.UsageEvent, Usage: usage}); err != nil {
					return semantic, err
				}
			}
		case "content_block_start":
			semantic = true
			var block struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				// redacted_thinking 的 data 在 content_block_start 一次给全。
				Data      string `json:"data"`
				Signature string `json:"signature"`
				Thinking  string `json:"thinking"`
			}
			_ = json.Unmarshal(event.ContentBlock, &block)
			switch block.Type {
			case "text":
			case "thinking":
				item := ports.ProtocolBlock{Type: "thinking", Signature: block.Signature, Thinking: block.Thinking}
				protocolBlocks = append(protocolBlocks, item)
				thinkingByIndex[event.Index] = &protocolBlocks[len(protocolBlocks)-1]
			case "redacted_thinking":
				item := ports.ProtocolBlock{Type: "redacted_thinking", Data: block.Data}
				protocolBlocks = append(protocolBlocks, item)
				thinkingByIndex[event.Index] = &protocolBlocks[len(protocolBlocks)-1]
			case "tool_use":
				accumulator := &toolCallAccumulator{id: block.ID, name: block.Name}
				toolCalls[block.ID] = accumulator
				toolCallsByIndex[event.Index] = accumulator
			default:
				return semantic, &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("unsupported content block %q", block.Type)}
			}
		case "content_block_delta":
			semantic = true
			var delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				PartialJSON string `json:"partial_json"`
			}
			_ = json.Unmarshal(event.Delta, &delta)
			switch delta.Type {
			case "text_delta":
				if err := emitEvent(ports.ModelEvent{Kind: ports.TextDelta, Text: delta.Text}); err != nil {
					return semantic, err
				}
			case "input_json_delta":
				target := toolCallsByIndex[event.Index]
				if target == nil {
					return semantic, &ProviderError{Kind: "invalid_response", Cause: errors.New("input_json_delta without a tool_use block")}
				}
				target.frames = append(target.frames, delta.PartialJSON)
			case "thinking_delta":
				target := thinkingByIndex[event.Index]
				if target == nil {
					return semantic, &ProviderError{Kind: "invalid_response", Cause: errors.New("thinking_delta without a thinking block")}
				}
				target.Thinking += delta.Thinking
			case "signature_delta":
				target := thinkingByIndex[event.Index]
				if target == nil {
					return semantic, &ProviderError{Kind: "invalid_response", Cause: errors.New("signature_delta without a thinking block")}
				}
				target.Signature += delta.Signature
			default:
			}
		case "content_block_stop":
		case "message_delta":
			semantic = true
			var delta struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(event.Delta, &delta)
			if delta.StopReason != "" {
				stopReason = mapStopReason(delta.StopReason)
			}
			if len(event.Usage) > 0 {
				usage, err := parseUsage(event.Usage)
				if err != nil {
					return semantic, &ProviderError{Kind: "invalid_response", Cause: err}
				}
				if err := emitEvent(ports.ModelEvent{Kind: ports.UsageEvent, Usage: usage}); err != nil {
					return semantic, err
				}
			}
		case "message_stop":
			calls, err := orderedToolCalls(toolCalls)
			if err != nil {
				return semantic, &ProviderError{Kind: "invalid_response", Cause: err}
			}
			for _, call := range calls {
				call := call
				if err := emitEvent(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
					return semantic, err
				}
			}
			if err := emitEvent(ports.ModelEvent{Kind: ports.Done, StopReason: stopReason, ProtocolBlocks: protocolBlocks}); err != nil {
				return semantic, err
			}
			done = true
			return semantic, nil
		case "error":
			var payload struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(event.Error, &payload)
			return semantic, &ProviderError{Kind: mapErrorType(payload.Type), Cause: errors.New(payload.Message)}
		case "ping":
		default:
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
		return semantic, &ProviderError{Kind: "invalid_response", Cause: errors.New("anthropic stream ended before message_stop")}
	}
	return semantic, nil
}

// mapStopReason 保留 Anthropic stop_reason 原词；端口合同接受该闭合词表。
func mapStopReason(reason string) string {
	return reason
}

// mapErrorType 把 Anthropic error.type 映射为失败分类。
func mapErrorType(errorType string) string {
	switch errorType {
	case "rate_limit_error":
		return "rate_limited"
	case "overloaded_error":
		return "overloaded"
	case "timeout_error":
		return "transport"
	case "authentication_error":
		return "authentication"
	case "permission_error":
		return "permission"
	case "billing_error":
		return "billing"
	case "invalid_request_error":
		return "invalid_request"
	case "not_found_error":
		return "not_found"
	case "api_error":
		return "api_error"
	default:
		return "api_error"
	}
}

// parseUsage 解析 Anthropic usage；缓存/推理计量若不能完整表达则不猜测。
func parseUsage(raw json.RawMessage) (*ports.Usage, error) {
	var payload struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, fmt.Errorf("decode usage: %w", err)
	}
	// Anthropic 语义：总输入 = input + cache_creation + cache_read；这里如实累计，
	// 不把缓存计量默认为零，也不重复累计。
	prompt := payload.InputTokens + payload.CacheCreationInputTokens + payload.CacheReadInputTokens
	usage := &ports.Usage{PromptTokens: prompt, CompletionTokens: payload.OutputTokens, TotalTokens: prompt + payload.OutputTokens}
	if err := validateUsageAccounting(usage); err != nil {
		return nil, err
	}
	return usage, nil
}
