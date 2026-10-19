// Package geminigen 将 Google Gemini GenerateContent API（官方文档
// ai.google.dev/api/generate-content，2026-10-18 冻结）的流式响应适配为
// ports.Provider。独立线协议：contents/parts（text/functionCall/functionResponse）
// 与 Chat Completions、Messages、Responses 结构均不相同。首版覆盖文本流、
// 工具调用标识与参数、工具结果、多轮连续性、usage 与错误分类。无真实凭据
// 时仅离线验证，不虚报真实兼容。
//
// Author: fxbin
package geminigen

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
	"sync/atomic"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// geminiStreamCounter 是进程内流级唯一计数器，配合时间戳生成工具调用 ID 前缀。
var geminiStreamCounter int64

// Client 将 Gemini streamGenerateContent SSE 响应适配为 Provider 端口。
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// MaxOutputTokens 为可选输出上限；0 表示不发送。
	MaxOutputTokens int
	// TemperatureMilli/TopPMilli 为显式采样参数；nil 表示不发送。
	TemperatureMilli *int
	TopPMilli        *int
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
	endpoint, err := streamURL(client.BaseURL, request.Model)
	if err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	payload := generateRequest{
		Contents: toGeminiContents(request.Messages),
		Tools:    toGeminiTools(request.Tools),
	}
	if system := extractGeminiSystem(request.Messages); system != "" {
		payload.SystemInstruction = &content{Role: "system", Parts: []part{{Text: system}}}
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
func applyGeneration(target *generateRequest, client Client) error {
	if client.MaxOutputTokens > 0 || client.TemperatureMilli != nil || client.TopPMilli != nil {
		target.GenerationConfig = &generationConfig{}
		if client.MaxOutputTokens > 0 {
			target.GenerationConfig.MaxOutputTokens = client.MaxOutputTokens
		}
		if client.TemperatureMilli != nil {
			value := float64(*client.TemperatureMilli) / 1000.0
			target.GenerationConfig.Temperature = &value
		}
		if client.TopPMilli != nil {
			value := float64(*client.TopPMilli) / 1000.0
			target.GenerationConfig.TopP = &value
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
		return &ProviderError{Kind: "invalid_response", Cause: errors.New("provider stream ended without a finish reason")}
	}
	for _, allowed := range client.Requirements.AllowedStopReasons {
		if allowed == stopReason {
			return nil
		}
	}
	return &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("undeclared finish reason %q", stopReason)}
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

// streamURL 校验 base URL 并构造 streamGenerateContent SSE 端点。
func streamURL(baseURL, model string) (string, error) {
	if strings.TrimSpace(model) == "" {
		return "", errors.New("model is required")
	}
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
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/v1beta/models/" + url.PathEscape(model) + ":streamGenerateContent"
	query := parsed.Query()
	query.Set("alt", "sse")
	parsed.RawQuery = query.Encode()
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
		httpRequest.Header.Set("x-goog-api-key", client.APIKey)
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
	kind, retryable := geminiErrorKind(response.StatusCode, payload)
	retryAfter := parseRetryAfter(response.Header.Get("Retry-After"))
	return &ProviderError{
		Kind: kind, StatusCode: response.StatusCode, Retryable: retryable, RetryAfter: retryAfter,
		Cause: fmt.Errorf("gemini generateContent API error: %s", strings.TrimSpace(string(payload))),
	}
}

// geminiErrorKind 把状态码与错误体映射为失败分类。
func geminiErrorKind(statusCode int, payload []byte) (string, bool) {
	var envelope struct {
		Error struct {
			Code    int    `json:"code"`
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(payload, &envelope)
	switch {
	case statusCode == http.StatusTooManyRequests || envelope.Error.Status == "RESOURCE_EXHAUSTED":
		return "rate_limited", true
	case statusCode >= 500 || envelope.Error.Status == "UNAVAILABLE" || envelope.Error.Status == "INTERNAL":
		return "api_error", true
	case statusCode == http.StatusUnauthorized || envelope.Error.Status == "UNAUTHENTICATED":
		return "authentication", false
	case statusCode == http.StatusForbidden || envelope.Error.Status == "PERMISSION_DENIED":
		return "permission", false
	case statusCode == http.StatusRequestTimeout || statusCode == http.StatusGatewayTimeout:
		return "transport", true
	case envelope.Error.Status == "INVALID_ARGUMENT" || envelope.Error.Status == "FAILED_PRECONDITION":
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

// generateRequest 是 Gemini GenerateContent 请求载荷（独立线协议）。
type generateRequest struct {
	Contents          []content         `json:"contents"`
	Tools             []tool            `json:"tools,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
	SystemInstruction *content          `json:"systemInstruction,omitempty"`
}

// generationConfig 是显式生成参数。
type generationConfig struct {
	MaxOutputTokens int      `json:"maxOutputTokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"topP,omitempty"`
}

// content 是 role + parts 的对话单元。
type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

// part 是内容块判别联合的最小闭合集合。
type part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

// functionCall 是模型请求的工具调用。
type functionCall struct {
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

// functionResponse 是工具结果回执。
type functionResponse struct {
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

// tool 是工具声明容器。
type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

// functionDeclaration 是单个函数工具声明。
type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// toGeminiTools 转换工具声明。
func toGeminiTools(specs []ports.ToolSpec) []tool {
	if len(specs) == 0 {
		return nil
	}
	declarations := make([]functionDeclaration, 0, len(specs))
	for _, spec := range specs {
		schema := spec.Parameters
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		declarations = append(declarations, functionDeclaration{Name: spec.Name, Description: spec.Description, Parameters: schema})
	}
	return []tool{{FunctionDeclarations: declarations}}
}

// toGeminiContents 把端口消息转换为 Gemini contents：system 提取为
// systemInstruction；assistant → model；工具回执以 user 角色携带
// functionResponse，name 取前序 functionCall 的函数名（不是 call ID）。
func toGeminiContents(messages []ports.Message) []content {
	contents := make([]content, 0, len(messages))
	// 建立 call ID → function name 关联，供工具回执映射。
	callNames := map[string]string{}
	for _, message := range messages {
		for _, call := range message.ToolCalls {
			callNames[call.ID] = call.Name
		}
	}
	for _, message := range messages {
		if message.Role == "system" {
			continue
		}
		parts := make([]part, 0, 2+len(message.ToolCalls))
		role := message.Role
		switch {
		case message.ToolCallID != "" || role == "tool":
			name := callNames[message.ToolCallID]
			if name == "" {
				name = message.ToolCallID
			}
			response := json.RawMessage(`{}`)
			if message.Content != "" {
				if json.Valid([]byte(message.Content)) {
					response = json.RawMessage(message.Content)
				} else {
					encoded, _ := json.Marshal(message.Content)
					response = encoded
				}
			}
			parts = append(parts, part{FunctionResponse: &functionResponse{Name: name, Response: response}})
			role = "user"
		default:
			if message.Content != "" {
				parts = append(parts, part{Text: message.Content})
			}
		}
		for _, call := range message.ToolCalls {
			args := call.Arguments
			if len(args) == 0 {
				args = json.RawMessage(`{}`)
			}
			parts = append(parts, part{FunctionCall: &functionCall{Name: call.Name, Args: args}})
		}
		switch role {
		case "assistant", "model":
			role = "model"
		case "user", "":
			role = "user"
		default:
			role = "user"
		}
		if len(parts) == 0 {
			continue
		}
		contents = append(contents, content{Role: role, Parts: parts})
	}
	return contents
}

// extractGeminiSystem 提取 system 提示供 systemInstruction 使用。
func extractGeminiSystem(messages []ports.Message) string {
	for _, message := range messages {
		if message.Role == "system" && message.Content != "" {
			return message.Content
		}
	}
	return ""
}

// streamChunk 是 SSE data 帧中的 GenerateContentResponse 片段。
type streamChunk struct {
	Candidates []struct {
		Content struct {
			Role  string `json:"role"`
			Parts []part `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
		TotalTokenCount      int `json:"totalTokenCount"`
	} `json:"usageMetadata"`
}

// toolCallAccumulator 累积一个 functionCall。
type toolCallAccumulator struct {
	id   string
	name string
	args json.RawMessage
}

// decodeStream 解析 Gemini SSE 事件，拼接工具调用并转发端口事件。
// 工具调用 ID 含流级唯一前缀，避免跨轮 call_0 冲突导致函数名映射覆盖。
func decodeStream(ctx context.Context, source io.Reader, emit func(ports.ModelEvent) error) (bool, error) {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	// 流级唯一前缀：纳秒 + 进程内原子计数，保证跨轮调用 ID 不冲突。
	streamPrefix := fmt.Sprintf("g%d", atomic.AddInt64(&geminiStreamCounter, 1))
	var dataLines []string
	toolCalls := []*toolCallAccumulator{}
	toolIndex := 0
	stopReason := ""
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
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return semantic, &ProviderError{Kind: "invalid_response", Cause: fmt.Errorf("decode gemini stream chunk: %w", err)}
		}
		semantic = true
		for _, candidate := range chunk.Candidates {
			for _, block := range candidate.Content.Parts {
				if block.Text != "" {
					if err := emitEvent(ports.ModelEvent{Kind: ports.TextDelta, Text: block.Text}); err != nil {
						return semantic, err
					}
				}
				if block.FunctionCall != nil {
					args := block.FunctionCall.Args
					if len(args) == 0 {
						args = json.RawMessage(`{}`)
					}
					toolCalls = append(toolCalls, &toolCallAccumulator{
						id:   fmt.Sprintf("call_%s_%d", streamPrefix, toolIndex),
						name: block.FunctionCall.Name,
						args: args,
					})
					toolIndex++
				}
			}
			if candidate.FinishReason != "" {
				stopReason = mapFinishReason(candidate.FinishReason)
			}
		}
		if chunk.UsageMetadata != nil {
			usage := &ports.Usage{
				PromptTokens:     chunk.UsageMetadata.PromptTokenCount,
				CompletionTokens: chunk.UsageMetadata.CandidatesTokenCount,
				TotalTokens:      chunk.UsageMetadata.TotalTokenCount,
			}
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
		}
	}
	if _, err := flush(); err != nil {
		return semantic, err
	}
	if err := scanner.Err(); err != nil {
		return semantic, classifyTransportError(err)
	}
	if !semantic {
		return semantic, &ProviderError{Kind: "invalid_response", Cause: errors.New("gemini stream produced no candidates")}
	}
	for _, item := range toolCalls {
		call := ports.ToolCall{ID: item.id, Name: item.name, Arguments: item.args}
		if err := emitEvent(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
			return semantic, err
		}
	}
	if err := emitEvent(ports.ModelEvent{Kind: ports.Done, StopReason: stopReason}); err != nil {
		return semantic, err
	}
	return semantic, nil
}

// mapFinishReason 把 Gemini finishReason 映射为端口合同词汇（保留原词）。
func mapFinishReason(reason string) string {
	return reason
}
