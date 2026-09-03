package contract

import (
	"errors"
	"fmt"
	"sort"
)

// providerCardSchemaV1 是 Provider Capability Card 的当前产品合同版本。
const providerCardSchemaV1 = 1

// ProviderCardSourceConfig 表示卡来自用户显式配置声明；能力模型不进编译期
// 代码，配置是唯一来源。
const ProviderCardSourceConfig = "config"

// providerCardProtocolChatCompletions 限定当前唯一生产线协议。
const providerCardProtocolChatCompletions = "openai-chat-completions"

// StopReasonContentFilter 等常量构成 OpenAI Chat Completions 线协议
// finish_reason 的闭合词表单一来源；生产代码比较或列举终止原因时必须引用
// 此处常量，不得散落字面量造成词表漂移。
const (
	StopReasonContentFilter = "content_filter"
	StopReasonLength        = "length"
	StopReasonStop          = "stop"
	StopReasonToolCalls     = "tool_calls"
)

// chatCompletionStopReasons 是 OpenAI Chat Completions 线协议定义的
// finish_reason 集合；它描述协议兼容性，不承诺具体模型会全部产生。
var chatCompletionStopReasons = []string{StopReasonContentFilter, StopReasonLength, StopReasonStop, StopReasonToolCalls}

// ProviderCapabilityDeclaration 是用户经 provider.capability 配置块给出的
// 显式能力声明；由 config 层完成严格 YAML 校验后传入解析。
type ProviderCapabilityDeclaration struct {
	ToolCalls           bool
	StreamingUsage      bool
	ContextWindowTokens int
	MaxOutputTokens     int
	StopReasons         []string
}

// ProviderCapabilityCard 是版本化的 Provider 能力合同：协议、工具调用、流式
// usage、上下文/输出上限与停止原因。上限为 0 表示未声明（不伪造数值），
// 供 doctor/manifest 展示与预算对照；它不是运行时自动协商的结果。
type ProviderCapabilityCard struct {
	SchemaVersion       int      `json:"schema_version"`
	ProviderKind        string   `json:"provider_kind"`
	Model               string   `json:"model"`
	Protocol            string   `json:"protocol"`
	ToolCalls           bool     `json:"tool_calls"`
	StreamingUsage      bool     `json:"streaming_usage"`
	ContextWindowTokens int      `json:"context_window_tokens,omitempty"`
	MaxOutputTokens     int      `json:"max_output_tokens,omitempty"`
	StopReasons         []string `json:"stop_reasons"`
	Source              string   `json:"source"`
}

// ResolveProviderCard 按固定优先级解析能力卡：显式配置声明 > 失败关闭。
// Cairn 唯一生产路径依赖工具调用与流式 usage，声明任一为假即拒绝启动；
// 模型能力不由编译期目录承载，未声明一律要求显式配置，不做静默降级或
// 网络目录查询。
func ResolveProviderCard(kind, model string, declared *ProviderCapabilityDeclaration) (ProviderCapabilityCard, error) {
	if kind != ProviderKindOpenAICompatible {
		return ProviderCapabilityCard{}, fmt.Errorf("provider kind %q has no capability contract", kind)
	}
	if model == "" {
		return ProviderCapabilityCard{}, errors.New("provider model is required to resolve a capability card")
	}
	if declared != nil {
		if !declared.ToolCalls || !declared.StreamingUsage {
			return ProviderCapabilityCard{}, errors.New("provider.capability must declare tool_calls and streaming_usage as true: the openai_compatible execution path requires both")
		}
		if declared.ContextWindowTokens < 0 || declared.MaxOutputTokens < 0 {
			return ProviderCapabilityCard{}, errors.New("provider.capability token limits must not be negative")
		}
		card := ProviderCapabilityCard{
			SchemaVersion: providerCardSchemaV1, ProviderKind: kind, Model: model,
			Protocol:  providerCardProtocolChatCompletions,
			ToolCalls: declared.ToolCalls, StreamingUsage: declared.StreamingUsage,
			ContextWindowTokens: declared.ContextWindowTokens, MaxOutputTokens: declared.MaxOutputTokens,
			StopReasons: normalizedStopReasons(declared.StopReasons), Source: ProviderCardSourceConfig,
		}
		return card, nil
	}
	return ProviderCapabilityCard{}, fmt.Errorf("provider capability unknown for model %q: declare provider.capability with tool_calls and streaming_usage in the workspace config", model)
}

// ValidateBudget 用已声明上限对照执行预算：估算输入上限不得超过声明上下文
// 窗口，输出预算不得超过声明输出上限；未声明（0）的上限不做对照。
func (card ProviderCapabilityCard) ValidateBudget(maxPromptTokens, maxOutputTokens int) error {
	if card.ContextWindowTokens > 0 && maxPromptTokens > card.ContextWindowTokens {
		return fmt.Errorf("context.max_prompt_tokens %d exceeds declared provider context window %d", maxPromptTokens, card.ContextWindowTokens)
	}
	if card.MaxOutputTokens > 0 && maxOutputTokens > card.MaxOutputTokens {
		return fmt.Errorf("budget.max_output_tokens %d exceeds declared provider output limit %d", maxOutputTokens, card.MaxOutputTokens)
	}
	return nil
}

// normalizedStopReasons 规范化声明的停止原因集合；空声明回退到协议标准集合，
// 去重并按字典序稳定，保证相同声明得到相同卡。
func normalizedStopReasons(reasons []string) []string {
	if len(reasons) == 0 {
		return append([]string(nil), chatCompletionStopReasons...)
	}
	unique := make(map[string]struct{}, len(reasons))
	for _, reason := range reasons {
		if reason == "" {
			continue
		}
		unique[reason] = struct{}{}
	}
	sorted := make([]string, 0, len(unique))
	for reason := range unique {
		sorted = append(sorted, reason)
	}
	sort.Strings(sorted)
	return sorted
}
