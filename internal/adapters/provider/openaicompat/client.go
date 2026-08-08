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

type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

type ProviderError struct {
	Kind       string
	StatusCode int
	Retryable  bool
	RetryAfter time.Duration
	Cause      error
}

func (err *ProviderError) Error() string {
	if err.StatusCode != 0 {
		return fmt.Sprintf("provider %s (%d): %v", err.Kind, err.StatusCode, err.Cause)
	}
	return fmt.Sprintf("provider %s: %v", err.Kind, err.Cause)
}

func (err *ProviderError) Unwrap() error { return err.Cause }

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

func retryAfter(value string, now time.Time) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil && when.After(now) {
		return time.Until(when)
	}
	return 0
}

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

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	Tools         []openAITool    `json:"tools,omitempty"`
	Stream        bool            `json:"stream"`
	StreamOptions *streamOptions  `json:"stream_options,omitempty"`
}

type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIFunctionCall `json:"function"`
}

type openAIFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openAITool struct {
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

func toOpenAITools(specs []ports.ToolSpec) []openAITool {
	tools := make([]openAITool, 0, len(specs))
	for _, spec := range specs {
		tools = append(tools, openAITool{Type: "function", Function: openAIFunction{Name: spec.Name, Description: spec.Description, Parameters: spec.Parameters}})
	}
	return tools
}

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

type chatChunk struct {
	Choices []choice      `json:"choices"`
	Usage   *usagePayload `json:"usage"`
}

type choice struct {
	Delta delta `json:"delta"`
}

type delta struct {
	Content   string             `json:"content"`
	ToolCalls []toolCallFragment `json:"tool_calls"`
}

type toolCallFragment struct {
	Index    int              `json:"index"`
	ID       string           `json:"id"`
	Function functionFragment `json:"function"`
}

type functionFragment struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type usagePayload struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type toolCallAccumulator struct {
	Index     int
	ID        string
	Name      string
	Arguments []byte
}

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
