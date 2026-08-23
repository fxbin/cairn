package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/ports"
)

// modelResponsePayload 是写入 model_response_finished 事件的响应内容；
// usage_estimated 标记 usage 为字节估算兜底而非提供方回报值。
type modelResponsePayload struct {
	Text           string           `json:"text"`
	ToolCalls      []ports.ToolCall `json:"tool_calls,omitempty"`
	Usage          *ports.Usage     `json:"usage,omitempty"`
	UsageEstimated bool             `json:"usage_estimated,omitempty"`
	StopReason     string           `json:"stop_reason,omitempty"`
}

// modelRequestPayload 是写入模型请求事件的可审计提供方信息。
type modelRequestPayload struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// modelRequestFailedPayload 保存一次没有完成模型响应的可回放分类；畸形工具参数只记录
// 固定错误文本，绝不把提供方原始参数写入事件。
type modelRequestFailedPayload struct {
	Error      string `json:"error"`
	ErrorClass string `json:"error_class,omitempty"`
	Retryable  bool   `json:"retryable,omitempty"`
}

// collectedResponse 汇集一次流式模型响应的文本、工具调用与 token 用量。
type collectedResponse struct {
	Text       string
	ToolCalls  []ports.ToolCall
	Usage      *ports.Usage
	StopReason string
}

var errSensitiveToolArguments = errors.New("model tool arguments contain a secret-like value")

// callModel 调用 Provider 并严格收集一个以 Done 结束的有效流式响应。
func (loop *Loop) callModel(ctx context.Context, messages []ports.Message) (collectedResponse, error) {
	var response collectedResponse
	done := false
	err := loop.Provider.Stream(ctx, ports.ModelRequest{Model: loop.Model, Messages: messages, Tools: loop.Tools.Specs()}, func(event ports.ModelEvent) error {
		if done {
			return errors.New("provider emitted event after done")
		}
		switch event.Kind {
		case ports.TextDelta:
			response.Text += event.Text
		case ports.ToolCallEvent:
			if event.ToolCall == nil || event.ToolCall.ID == "" || event.ToolCall.Name == "" {
				return errors.New("provider emitted invalid tool call")
			}
			if !json.Valid(event.ToolCall.Arguments) {
				return ports.ErrMalformedToolCallArguments
			}
			if !isObjectArguments(event.ToolCall.Arguments) {
				return ports.ErrMalformedToolCallArguments
			}
			if containsSensitiveToolCall(*event.ToolCall) {
				return errSensitiveToolArguments
			}
			response.ToolCalls = append(response.ToolCalls, *event.ToolCall)
		case ports.UsageEvent:
			if event.Usage == nil || response.Usage != nil {
				return errors.New("provider emitted invalid usage event")
			}
			usage := *event.Usage
			response.Usage = &usage
		case ports.Done:
			response.StopReason = event.StopReason
			done = true
		default:
			return fmt.Errorf("unknown provider event %q", event.Kind)
		}
		return nil
	})
	if err != nil {
		return collectedResponse{}, err
	}
	if !done {
		return collectedResponse{}, errors.New("provider returned without done")
	}
	if loop.RequireProviderUsage && response.Usage == nil {
		return collectedResponse{}, &ProviderCapabilityMismatchError{Requirement: "streaming usage", Observed: "missing"}
	}
	if len(loop.AllowedProviderStopReasons) != 0 {
		if response.StopReason == "" {
			return collectedResponse{}, &ProviderCapabilityMismatchError{Requirement: "finish reason", Observed: "missing"}
		}
		if !containsString(loop.AllowedProviderStopReasons, response.StopReason) {
			return collectedResponse{}, &ProviderCapabilityMismatchError{Requirement: "declared finish reason", Observed: response.StopReason}
		}
	}
	return response, nil
}

// containsString 判断稳定能力集合是否包含一个精确值；停止原因来自协议枚举，
// 不做大小写或模糊归一化，避免模型端返回未知值时被静默接受。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

// containsSensitiveToolCall 在模型响应尚未进入事件或上下文前拒绝可能包含凭据的
// 工具调用事实。除通用 JSON 脱敏与工具身份检查外，exec_command 的结构化 argv
// 规则（短口令标志、URL userinfo、查询串凭据键、Authorization 头、裸环境赋值）
// 统一由 sensitive.go 提供，不依赖面向长文本的脱敏阈值。
func containsSensitiveToolCall(call ports.ToolCall) bool {
	if sensitiveToolIdentity(call.Name, call.ID) {
		return true
	}
	if containsSecretJSON(call.Arguments) {
		return true
	}
	if call.Name != "exec_command" {
		return false
	}
	var fields struct {
		Argv []string `json:"argv"`
	}
	if json.Unmarshal(call.Arguments, &fields) != nil {
		return false
	}
	return sensitiveExecArgv(fields.Argv)
}

// prompt 返回显式系统提示词，或提供安全工具使用的默认提示词。
func (loop *Loop) prompt() string {
	if strings.TrimSpace(loop.SystemPrompt) != "" {
		return loop.SystemPrompt
	}
	return "You are Cairn, a careful coding agent. Use the supplied tools for repository facts. Start from task-scoped paths; avoid broad or repeated listings and rereading unchanged content. Treat a search_text line number only as a locator: read_file.offset is a byte offset, not a line number. Request bounded file slices; do not request the configured maximum merely to inspect source. Once you have enough evidence, implement the smallest allowed fix, then let the fixed verification decide completion. When the task-specific verification command passes after your relevant change, immediately return a final answer: do not add unrequested tests, refactors, or edge cases. Do not rerun an unchanged failed command: inspect the implicated code and patch first. Before update/delete, use read_file and copy its exact sha256 into apply_patch.expected_sha256; do not invoke shell hash programs. If a patch is rejected, read fresh state and repair it before rerunning tests. Return a concise final answer only when the task is answered."
}
