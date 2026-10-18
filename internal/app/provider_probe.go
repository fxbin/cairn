// provider_probe.go 把 Provider 合同探测的适配器装配与证据落盘收口到应用层，
// CLI/TUI 只消费非敏感结论，不再直接构造协议客户端。
//
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/fxbin/cairn/internal/adapters/provider/openaicompat"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/provider"
)

// 探测结论与工具回路的闭合词表；对外稳定，供证据与命令输出共用。
const (
	ProviderProbePass         = "pass"
	ProviderProbeFail         = "fail"
	ProviderProbeInconclusive = "inconclusive"

	ToolRoundtripSkipped         = "skipped"
	ToolRoundtripOK              = "ok"
	ToolRoundtripNotInvoked      = "not_invoked"
	ToolRoundtripFailed          = "failed"
	ToolRoundtripSecondLegFailed = "second_leg_failed"
)

// providerProbeToolResult 是探测工具回喂给第二程的固定 tool result 内容。
const providerProbeToolResult = `{"value":7}`

// ProviderGenerationSummary 是生成参数的人类可读两态投影。
type ProviderGenerationSummary struct {
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	TemperatureMilli *int   `json:"temperature_milli,omitempty"`
	TopPMilli        *int   `json:"top_p_milli,omitempty"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	ToolChoice       string `json:"tool_choice,omitempty"`
}

// ProviderProbeResult 是合同探测的闭合结论：无消息正文、无密钥、无明文端点，
// 可安全进入本地记录与人工复核。
type ProviderProbeResult struct {
	Result                string                     `json:"result"`
	ProviderKind          string                     `json:"provider_kind"`
	EndpointIdentity      string                     `json:"endpoint_identity"`
	Model                 string                     `json:"model"`
	ModelRevision         string                     `json:"model_revision"`
	CapabilityFingerprint string                     `json:"capability_fingerprint"`
	Generation            *ProviderGenerationSummary `json:"generation"`
	UsageStreamed         bool                       `json:"usage_streamed"`
	StopReason            string                     `json:"stop_reason"`
	ToolRoundtrip         string                     `json:"tool_roundtrip"`
	Error                 string                     `json:"error,omitempty"`
	EvidenceFingerprint   string                     `json:"evidence_fingerprint,omitempty"`

	// probed 记录网络探测是否真实执行过：仅执行过的探测才有观测事实可落
	// 盘为证据；早期配置失败没有观测语义，不进入证据存储。
	probed          bool
	probeErrorClass string
	probeStatusCode int
}

// CheckProviderOptions 描述一次只读合同探测：ProbeTools 额外验证工具调用回路。
type CheckProviderOptions struct {
	Config     config.Config
	Workspace  string
	StateDir   string
	ProbeTools bool
}

// CheckProvider 对配置解析出的提供方执行真实合同探测，并在执行过网络探测时
// 将观测落盘为内容寻址证据。装配与协议细节不出应用层；落盘失败只报告在
// Evidence 错误里，不改变探测结论。
func (service Service) CheckProvider(ctx context.Context, options CheckProviderOptions) (ProviderProbeResult, error) {
	configuration := options.Config
	result := ProviderProbeResult{Result: ProviderProbeFail, ProviderKind: contract.ProviderKindOpenAICompatible}
	if configuration.Provider.BaseURL == "" || configuration.Provider.Model == "" {
		result.Error = "provider.base_url and provider.model must be configured"
		return result, nil
	}
	digest, err := contract.BaseURLIdentityDigest(configuration.Provider.BaseURL)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	profile, err := contract.ResolveProviderRequestProfile(contract.ProviderKindOpenAICompatible, contract.ProviderRequestDeclaration{
		BaseURL:    configuration.Provider.BaseURL,
		Model:      configuration.Provider.Model,
		Capability: config.ProviderCapabilityDeclaration(configuration),
		Generation: config.ProviderGenerationDeclaration(configuration),
	})
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}
	if _, err := contract.ResolveProviderCard(contract.ProviderKindOpenAICompatible, configuration.Provider.Model, config.ProviderCapabilityDeclaration(configuration)); err != nil {
		result.Error = err.Error()
		return result, nil
	}
	result.EndpointIdentity = digest
	result.Model = profile.Model
	result.ModelRevision = profile.ProviderModelRevision
	result.CapabilityFingerprint = profile.CapabilityFingerprint
	result.Generation = summarizeProviderGeneration(profile.GenerationOptions)

	client, err := service.provider(configuration)
	if err != nil {
		result.Error = err.Error()
		return result, nil
	}

	request := ports.ModelRequest{Model: configuration.Provider.Model}
	promptText := "Reply with the single word: ok."
	if options.ProbeTools {
		request.Tools = []ports.ToolSpec{{
			Name:        "contract_probe",
			Description: "No-op probe tool used by Cairn to verify the function-call round trip.",
			Parameters:  json.RawMessage(`{"type":"object","properties":{"value":{"type":"integer"}},"required":["value"]}`),
		}}
		promptText = "Call the contract_probe tool once with value set to 7, then reply with the single word: ok."
	}
	request.Messages = []ports.Message{{Role: "user", Content: promptText}}

	// 网络探测自此开始执行：后续结论（含失败）都携带可落盘的观测事实。
	result.probed = true
	textSeen := false
	var toolCalls []ports.ToolCall
	var stopReason string
	eventsErr := client.Stream(ctx, request, func(event ports.ModelEvent) error {
		switch event.Kind {
		case ports.TextDelta:
			textSeen = true
		case ports.ToolCallEvent:
			if event.ToolCall != nil {
				toolCalls = append(toolCalls, *event.ToolCall)
			}
		case ports.UsageEvent:
			result.UsageStreamed = true
		case ports.Done:
			stopReason = event.StopReason
		}
		return nil
	})
	if eventsErr != nil {
		result.Error = eventsErr.Error()
		captureProviderProbeErrorClass(&result, eventsErr)
		if options.ProbeTools {
			result.ToolRoundtrip = ToolRoundtripFailed
		} else {
			result.ToolRoundtrip = ToolRoundtripSkipped
		}
		return service.finishProviderProbe(options, result)
	}
	result.StopReason = stopReason
	if !options.ProbeTools {
		result.ToolRoundtrip = ToolRoundtripSkipped
		if !textSeen {
			result.Error = "provider produced no observable output"
			result.Result = ProviderProbeFail
			return service.finishProviderProbe(options, result)
		}
		result.Result = ProviderProbePass
		return service.finishProviderProbe(options, result)
	}
	if len(toolCalls) == 0 {
		result.ToolRoundtrip = ToolRoundtripNotInvoked
		if !textSeen {
			result.Error = "provider produced no observable output"
			result.Result = ProviderProbeFail
			return service.finishProviderProbe(options, result)
		}
		result.Result = ProviderProbeInconclusive
		return service.finishProviderProbe(options, result)
	}
	secondLegMessages := append(append([]ports.Message(nil), request.Messages...), ports.Message{Role: "assistant", ToolCalls: toolCalls})
	for _, call := range toolCalls {
		secondLegMessages = append(secondLegMessages, ports.Message{Role: "tool", ToolCallID: call.ID, Content: providerProbeToolResult})
	}
	secondLeg := request
	secondLeg.Messages = secondLegMessages
	secondTextSeen := false
	secondToolCalls := 0
	secondStopReason := ""
	secondErr := client.Stream(ctx, secondLeg, func(event ports.ModelEvent) error {
		switch event.Kind {
		case ports.TextDelta:
			secondTextSeen = true
		case ports.ToolCallEvent:
			secondToolCalls++
		case ports.UsageEvent:
			result.UsageStreamed = true
		case ports.Done:
			if event.StopReason != "" {
				secondStopReason = event.StopReason
			}
		}
		return nil
	})
	if secondErr != nil {
		result.Error = secondErr.Error()
		captureProviderProbeErrorClass(&result, secondErr)
		result.ToolRoundtrip = ToolRoundtripSecondLegFailed
		result.Result = ProviderProbeFail
		return service.finishProviderProbe(options, result)
	}
	secondLegComplete := secondTextSeen && secondToolCalls == 0 && secondStopReason != contract.StopReasonToolCalls
	if !secondLegComplete {
		result.Error = "second leg did not produce a final answer"
		result.ToolRoundtrip = ToolRoundtripSecondLegFailed
		result.Result = ProviderProbeFail
		return service.finishProviderProbe(options, result)
	}
	result.StopReason = secondStopReason
	result.ToolRoundtrip = ToolRoundtripOK
	result.Result = ProviderProbePass
	return service.finishProviderProbe(options, result)
}

// finishProviderProbe 在真实探测后落盘证据；未探测或落盘失败不改写探测结论，
// 落盘失败以 error 返回供命令层单独报告。
func (service Service) finishProviderProbe(options CheckProviderOptions, result ProviderProbeResult) (ProviderProbeResult, error) {
	if !result.probed {
		return result, nil
	}
	fingerprint, err := service.persistProviderProbeEvidence(options, result)
	if err != nil {
		return result, err
	}
	result.EvidenceFingerprint = fingerprint
	return result, nil
}

// summarizeProviderGeneration 把合同生成参数投影为摘要；零值块表示未声明字段。
func summarizeProviderGeneration(options contract.ProviderGenerationProfile) *ProviderGenerationSummary {
	return &ProviderGenerationSummary{
		MaxOutputTokens:  options.MaxOutputTokens,
		TemperatureMilli: options.TemperatureMilli,
		TopPMilli:        options.TopPMilli,
		ReasoningEffort:  options.ReasoningEffort,
		ToolChoice:       options.ToolChoice,
	}
}

// captureProviderProbeErrorClass 提取稳定分类与状态码：仅接受 ProviderError
// 的闭合词表，其他错误不猜测类别，保持证据脱敏合同。
func captureProviderProbeErrorClass(result *ProviderProbeResult, err error) {
	var providerError *openaicompat.ProviderError
	if !errors.As(err, &providerError) {
		return
	}
	result.probeErrorClass = providerError.ErrorClass()
	result.probeStatusCode = providerError.StatusCode
}

// persistProviderProbeEvidence 把真实探测观测落盘为内容寻址证据；工作区哈希
// 与 assembleBoot 同一规范化算法，保证证据分区与准入门绑定一致。
func (service Service) persistProviderProbeEvidence(options CheckProviderOptions, result ProviderProbeResult) (string, error) {
	configuration := options.Config
	_, workspaceHash, err := workspaceFor(options.Workspace, configuration)
	if err != nil {
		return "", err
	}
	stateDir := options.StateDir
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	store, err := provider.OpenEvidenceStore(stateDir, workspaceHash)
	if err != nil {
		return "", err
	}
	selection, err := provider.ResolveSelection(provider.SelectionFacts{
		ProviderID: configuration.Provider.ID,
		Model:      configuration.Provider.Model,
		BaseURL:    configuration.Provider.BaseURL,
		Protocol:   configuration.Provider.Protocol,
	})
	if err != nil {
		return "", err
	}
	observations := provider.EvidenceObservations{
		UsageStreamed:      provider.ObservationNotObserved,
		StopReasonsState:   provider.StateNotObserved,
		ToolRoundtrip:      result.ToolRoundtrip,
		ModelRevisionState: provider.StateNotObserved,
	}
	if result.UsageStreamed {
		observations.UsageStreamed = provider.ObservationObservedTrue
	} else if result.Result != ProviderProbeFail {
		observations.UsageStreamed = provider.ObservationObservedFalse
	}
	if result.StopReason != "" {
		observations.StopReasonsState = provider.StateObserved
		observations.StopReasons = []string{result.StopReason}
	}
	if result.ModelRevision != "" {
		observations.ModelRevisionState = provider.StateObserved
		observations.ModelRevision = result.ModelRevision
	}
	outcome := provider.EvidenceOutcomeCompleted
	if result.Result == ProviderProbeFail {
		outcome = provider.EvidenceOutcomeFailed
	}
	evidence := provider.ProviderProbeEvidence{
		SchemaVersion:        1,
		Selection:            selection,
		ProbeContractVersion: provider.CurrentProbeContractVersion,
		Observations:         observations,
		Outcome:              outcome,
		ErrorClass:           result.probeErrorClass,
		StatusCode:           result.probeStatusCode,
		ProbedAt:             time.Now().UTC(),
	}
	return store.Append(&evidence)
}
