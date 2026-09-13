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

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// Client 将 OpenAI-compatible Chat Completions SSE 响应适配为 Provider 端口。
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// Generation 携带显式生成参数；nil 表示整块未声明（全部 unsupported），
	// 请求载荷不会出现任何采样字段。
	Generation *GenerationOptions
	// Requirements 是由能力卡推导的响应合同要求：声明 streaming_usage 的
	// 提供方若不返回 usage、或 Done 携带未声明的 stop reason，都属于响应
	// 合同漂移，必须在 Provider 层失败关闭。
	Requirements Requirements
}

// GenerationOptions 是进入 Chat Completions 载荷的显式生成参数：仅显式值会
// 被序列化，指针与空串表达 unsupported；Milli 整数是配置与指纹的唯一形态，
// 浮点换算只发生在本层 wire 序列化边界。
type GenerationOptions struct {
	MaxOutputTokens  int
	TemperatureMilli *int
	TopPMilli        *int
	ReasoningEffort  string
	ToolChoice       string
}

// Requirements 是 Provider 层的响应闭合合同。
type Requirements struct {
	// RequireUsage 要求每次成功完成的流必须携带 usage 统计。
	RequireUsage bool
	// AllowedStopReasons 为空时不做 stop reason 校验；非空时 Done 必须携带
	// 集合内的原因且不允许缺失。
	AllowedStopReasons []string
}

// generationWireTemperatureDivisor 把 Milli 整数换算为线协议浮点温度/top_p。
const generationWireTemperatureDivisor = 1000.0

// applyGeneration 把显式生成参数写入线协议载荷：Milli 换算为浮点只在此处发
// 生；reasoning_effort=off 表示明确声明不使用推理档，线协议省略该字段；
// unsupported 字段一律不出现在载荷中。
func applyGeneration(target *chatRequest, options *GenerationOptions) error {
	if options == nil {
		return nil
	}
	if options.MaxOutputTokens > 0 {
		tokens := options.MaxOutputTokens
		target.MaxTokens = &tokens
	}
	if options.TemperatureMilli != nil {
		if err := validateRange("temperature_milli", *options.TemperatureMilli, 0, contract.GenerationTempMaxMilli); err != nil {
			return err
		}
		value := float64(*options.TemperatureMilli) / generationWireTemperatureDivisor
		target.Temperature = &value
	}
	if options.TopPMilli != nil {
		if err := validateRange("top_p_milli", *options.TopPMilli, contract.GenerationTopPMinMilli, contract.GenerationTopPMaxMilli); err != nil {
			return err
		}
		value := float64(*options.TopPMilli) / generationWireTemperatureDivisor
		target.TopP = &value
	}
	switch options.ReasoningEffort {
	case "":
	case contract.ReasoningEffortOff:
		// off 属于显式决策而非缺省：指纹与 Resolution 记录 off，线协议无
		// 对应取值，故省略字段本身即该决策的表达形式。
	case contract.ReasoningEffortLow, contract.ReasoningEffortMedium, contract.ReasoningEffortHigh:
		target.ReasoningEffort = options.ReasoningEffort
	default:
		return fmt.Errorf("unknown reasoning_effort %q", options.ReasoningEffort)
	}
	switch options.ToolChoice {
	case "":
	case contract.GenerationToolChoiceAuto:
		target.ToolChoice = options.ToolChoice
	default:
		return fmt.Errorf("unknown tool_choice %q", options.ToolChoice)
	}
	return nil
}

// validateRange 报告 Milli 参数越界；适配器在发起请求前拒绝越界输入，
// 与 config 与 contract 层构成三重失败关闭。
func validateRange(name string, value, minimum, maximum int) error {
	if value < minimum || value > maximum {
		return fmt.Errorf("%s %d outside supported range [%d,%d]", name, value, minimum, maximum)
	}
	return nil
}

// validateUsageAccounting 校验提供方回报的用量自洽：计数非负、total 在提供
// 时必须等于 prompt 与 completion 之和；total 缺席（0）不构成不一致，因为
// 部分按量端点不回填汇总值。校验在协议边界失败关闭（invalid_response），
// Agent Loop 保留同一规则作为纵深防御；错误文本只定性、不回显数值。
func validateUsageAccounting(usage *ports.Usage) error {
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.TotalTokens < 0 {
		return errors.New("provider usage contains a negative token count")
	}
	if usage.TotalTokens != 0 && usage.TotalTokens != usage.PromptTokens+usage.CompletionTokens {
		return errors.New("provider usage total does not equal prompt plus completion")
	}
	return nil
}

// checkStreamContract 在一次流成功完成后核对响应闭合合同：usage 缺失或
// stop reason 缺失/未声明都按 invalid_response 失败关闭，不做重试。
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

// Stream 以最多三次无语义输出的可重试请求转发模型流事件；显式生成参数在
// 构造载荷阶段即被校验，越界或未知枚举在发起网络请求前失败关闭。
func (client Client) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if emit == nil {
		return errors.New("provider emit callback is required")
	}
	endpoint, err := chatCompletionsURL(client.BaseURL)
	if err != nil {
		return &ProviderError{Kind: "invalid_request", Cause: err}
	}
	payload := chatRequest{Model: request.Model, Messages: toOpenAIMessages(request.Messages), Tools: toOpenAITools(request.Tools), Stream: true, StreamOptions: &streamOptions{IncludeUsage: true}}
	if err := applyGeneration(&payload, client.Generation); err != nil {
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

// maxRetryAfterDelay 限制服务端 Retry-After 可施加的单次休眠上限；更大值
// 只会来自异常或恶意的服务端，实际等待仍由取消与墙钟预算兜底。
const maxRetryAfterDelay = 30 * time.Second

// retryDelay 计算单次重试等待：尊重服务端 Retry-After 但设上限，缺省时按
// 尝试次数线性退避。
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
		return false, &ProviderError{Kind: statusKind(response.StatusCode), StatusCode: response.StatusCode, Retryable: response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500, RetryAfter: retryAfter(response.Header.Get("Retry-After"), time.Now()), Cause: errors.New("HTTP request rejected")}
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

// chatCompletionsURL 验证绝对 HTTP(S) 基地址并拼接 Chat Completions 路径；
// 携带 userinfo、query 或 fragment 的地址被拒绝，防止内嵌凭据随请求与
// 日志输出绕开"密钥只能来自环境变量"的合同。
func chatCompletionsURL(baseURL string) (string, error) {
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
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/chat/completions"
	return parsed.String(), nil
}

// decodeStream 解析 SSE data 帧，累计工具调用片段并按端口事件顺序转发。
func decodeStream(ctx context.Context, source io.Reader, emit func(ports.ModelEvent) error) error {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var dataLines []string
	toolCalls := map[int]*toolCallAccumulator{}
	stopReason := ""
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
					return &ProviderError{Kind: "invalid_response", Cause: ports.ErrMalformedToolCallArguments}
				}
				copy := ports.ToolCall{ID: call.ID, Name: call.Name, Arguments: append(json.RawMessage(nil), call.Arguments...)}
				if err := emitEvent(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &copy}); err != nil {
					return err
				}
			}
			done = true
			return emitEvent(ports.ModelEvent{Kind: ports.Done, StopReason: stopReason})
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
			if choice.FinishReason != "" {
				if stopReason != "" && stopReason != choice.FinishReason {
					return &ProviderError{Kind: "invalid_response", Cause: errors.New("received conflicting finish reasons")}
				}
				stopReason = choice.FinishReason
			}
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

// chatRequest 是 OpenAI-compatible Chat Completions 的流式请求载荷；生成参
// 数字段全部为指针/条件赋值，unsupported 时键整体缺席，显式零值（如
// temperature=0）仍会被序列化。
type chatRequest struct {
	Model           string          `json:"model"`
	Messages        []openAIMessage `json:"messages"`
	Tools           []openAITool    `json:"tools,omitempty"`
	Stream          bool            `json:"stream"`
	StreamOptions   *streamOptions  `json:"stream_options,omitempty"`
	MaxTokens       *int            `json:"max_tokens,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	ReasoningEffort string          `json:"reasoning_effort,omitempty"`
	ToolChoice      string          `json:"tool_choice,omitempty"`
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
	Delta        delta  `json:"delta"`
	FinishReason string `json:"finish_reason"`
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

// statusKind 将 HTTP 状态码归类为稳定的提供方错误种类；429 必须复用统一
// 失败词表的 rate_limited，禁止在此自造别名造成词表漂移。
func statusKind(statusCode int) string {
	switch {
	case statusCode == http.StatusUnauthorized || statusCode == http.StatusForbidden:
		return "auth"
	case statusCode == http.StatusTooManyRequests:
		return contract.FailureRateLimited
	case statusCode >= 500:
		return "server"
	default:
		return "invalid_request"
	}
}
