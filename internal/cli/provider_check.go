// 本文件实现 Provider 合同探测命令。
//
// Author: fxbin
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/adapters/provider/openaicompat"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/provider"
	"github.com/fxbin/cairn/internal/tools"
)

// providerCheckResult 是合同探测的闭合结论输出：无消息正文、无密钥、无明文
// 端点地址，可安全进入本地记录与人工复核。
type providerCheckResult struct {
	Result                string                     `json:"result"`
	ProviderKind          string                     `json:"provider_kind"`
	EndpointIdentity      string                     `json:"endpoint_identity"`
	Model                 string                     `json:"model"`
	ModelRevision         string                     `json:"model_revision"`
	CapabilityFingerprint string                     `json:"capability_fingerprint"`
	Generation            *providerGenerationSummary `json:"generation"`
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

// providerGenerationSummary 是生成参数的人类可读两态投影。
type providerGenerationSummary struct {
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	TemperatureMilli *int   `json:"temperature_milli,omitempty"`
	TopPMilli        *int   `json:"top_p_milli,omitempty"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	ToolChoice       string `json:"tool_choice,omitempty"`
}

// generationUnsupportedText 是摘要中表达全块未声明的固定文本。
const generationUnsupportedText = "unsupported"

// toolRoundtripSkipped 与相关常量构成工具回路检查结论的闭合词表：skipped=
// 未启用探测；ok=两程全通（工具调用、tool result 回喂、最终回答）；
// not_invoked=模型未调用工具；failed=第一程失败；second_leg_failed=第一程
// 成功但第二程失败或无最终回答。
const (
	toolRoundtripSkipped         = "skipped"
	toolRoundtripOK              = "ok"
	toolRoundtripNotInvoked      = "not_invoked"
	toolRoundtripFailed          = "failed"
	toolRoundtripSecondLegFailed = "second_leg_failed"
)

// probeResultPass 等常量构成探测结论的闭合词表。
const (
	probeResultPass         = "pass"
	probeResultFail         = "fail"
	probeResultInconclusive = "inconclusive"
)

// contractProbeToolResult 是探测工具回喂给第二程的固定 tool result 内容。
const contractProbeToolResult = `{"value":7}`

// checkProvider 对当前配置解析出的 OpenAI-compatible 提供方执行一次真实模型
// 合同探测：验证 usage 流、声明集合内的 stop reason、显式生成参数被端点接
// 受；--probe-tools 额外验证工具调用回路。探测只发最小提示词，不接触任务
// 正文，不写入任何事件或制品；付费外呼由用户显式发起本命令触发。
func (command Command) checkProvider(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("check-provider", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	configPath := flags.String("config", "", "configuration path")
	stateDir := flags.String("state-dir", "", "override the ignored Cairn state directory")
	jsonOutput := flags.Bool("json", false, "write JSON to stdout")
	probeTools := flags.Bool("probe-tools", false, "also verify the tool call round trip with a no-op probe tool")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "check-provider takes no positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	result := command.runProviderProbe(ctx, configuration, *probeTools)
	if fingerprint, err := persistProbeEvidence(*workspace, *stateDir, configuration, result); err != nil {
		fmt.Fprintln(command.Stderr, "evidence persistence failed:", err)
	} else {
		result.EvidenceFingerprint = fingerprint
	}
	if *jsonOutput {
		writeJSON(command.Stdout, result)
	} else {
		command.writeProviderCheck(result)
	}
	switch result.Result {
	case probeResultPass:
		return ExitOK
	default:
		return ExitFailed
	}
}

// runProviderProbe 执行探测主流程并始终返回完整结果结构：配置与能力卡不满足
// 合同时直接产出 fail 结论，而不是依赖错误路径拼装输出。
func (command Command) runProviderProbe(ctx context.Context, configuration config.Config, probeTools bool) providerCheckResult {
	result := providerCheckResult{Result: probeResultFail, ProviderKind: contract.ProviderKindOpenAICompatible}
	if configuration.Provider.BaseURL == "" || configuration.Provider.Model == "" {
		result.Error = "provider.base_url and provider.model must be configured"
		return result
	}
	digest, err := contract.BaseURLIdentityDigest(configuration.Provider.BaseURL)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	profile, err := contract.ResolveProviderRequestProfile(contract.ProviderKindOpenAICompatible, contract.ProviderRequestDeclaration{
		BaseURL: configuration.Provider.BaseURL, Model: configuration.Provider.Model,
		Capability: config.ProviderCapabilityDeclaration(configuration),
		Generation: config.ProviderGenerationDeclaration(configuration),
	})
	if err != nil {
		result.Error = err.Error()
		return result
	}
	card, err := contract.ResolveProviderCard(contract.ProviderKindOpenAICompatible, configuration.Provider.Model, config.ProviderCapabilityDeclaration(configuration))
	if err != nil {
		result.Error = err.Error()
		return result
	}
	result.EndpointIdentity = digest
	result.Model = profile.Model
	result.ModelRevision = profile.ProviderModelRevision
	result.CapabilityFingerprint = profile.CapabilityFingerprint
	result.Generation = summarizeGeneration(profile.GenerationOptions)
	client := openaicompat.Client{
		BaseURL: configuration.Provider.BaseURL, APIKey: os.Getenv(configuration.Provider.APIKeyEnv),
		HTTPClient: &http.Client{Timeout: probeTimeout(configuration)},
		Requirements: openaicompat.Requirements{
			RequireUsage:       card.StreamingUsage,
			AllowedStopReasons: append([]string(nil), card.StopReasons...),
		},
	}
	if generation := configuration.Provider.Generation; generation != nil {
		client.Generation = &openaicompat.GenerationOptions{
			TemperatureMilli: generation.TemperatureMilli,
			TopPMilli:        generation.TopPMilli, ReasoningEffort: generation.ReasoningEffort, ToolChoice: generation.ToolChoice,
		}
		if generation.MaxOutputTokens != nil {
			client.Generation.MaxOutputTokens = *generation.MaxOutputTokens
		}
	}
	request := ports.ModelRequest{Model: configuration.Provider.Model}
	promptText := "Reply with the single word: ok."
	if probeTools {
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
		captureProbeErrorClass(&result, eventsErr)
		if probeTools {
			result.ToolRoundtrip = toolRoundtripFailed
		} else {
			result.ToolRoundtrip = toolRoundtripSkipped
		}
		return result
	}
	result.StopReason = stopReason
	if !probeTools {
		result.ToolRoundtrip = toolRoundtripSkipped
		if !textSeen {
			result.Error = "provider produced no observable output"
			result.Result = probeResultFail
			return result
		}
		result.Result = probeResultPass
		return result
	}
	if len(toolCalls) == 0 {
		result.ToolRoundtrip = toolRoundtripNotInvoked
		if !textSeen {
			// 既无文本也无工具调用的空响应说明端点异常，属于失败而不是
			// 可判性存疑；只有输出了普通文本但未按要求调用工具才是
			// inconclusive。
			result.Error = "provider produced no observable output"
			result.Result = probeResultFail
			return result
		}
		result.Result = probeResultInconclusive
		return result
	}
	// 两程合同：第一程的每个工具调用都必须回喂 tool result；回路可用性以
	// 第二程能否产出最终回答判定，单程收到工具调用不足以判定 ok。
	secondLegMessages := append(append([]ports.Message(nil), request.Messages...), ports.Message{Role: "assistant", ToolCalls: toolCalls})
	for _, call := range toolCalls {
		secondLegMessages = append(secondLegMessages, ports.Message{Role: "tool", ToolCallID: call.ID, Content: contractProbeToolResult})
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
		captureProbeErrorClass(&result, secondErr)
		result.ToolRoundtrip = toolRoundtripSecondLegFailed
		result.Result = probeResultFail
		return result
	}
	// 最终回答的三要件：存在最终文本、没有再次请求工具、不以 tool_calls 终
	// 止。先说几句话再继续调工具的响应没有完成回路，不得判 ok。
	secondLegComplete := secondTextSeen && secondToolCalls == 0 && secondStopReason != contract.StopReasonToolCalls
	if !secondLegComplete {
		result.Error = "second leg did not produce a final answer"
		result.ToolRoundtrip = toolRoundtripSecondLegFailed
		result.Result = probeResultFail
		return result
	}
	result.StopReason = secondStopReason
	result.ToolRoundtrip = toolRoundtripOK
	result.Result = probeResultPass
	return result
}

// probeTimeout 返回探测专用超时：优先复用请求超时配置，保证与生产一致的
// 合同环境；未配置时使用保守的整次探测上限。
func probeTimeout(configuration config.Config) time.Duration {
	timeout, err := configuration.RequestTimeout()
	if err != nil || timeout <= 0 {
		return 120 * time.Second
	}
	return timeout
}

// summarizeGeneration 把合同生成参数投影为摘要；nil 表示全块未声明。
func summarizeGeneration(options contract.ProviderGenerationProfile) *providerGenerationSummary {
	summary := &providerGenerationSummary{
		MaxOutputTokens: options.MaxOutputTokens, TemperatureMilli: options.TemperatureMilli,
		TopPMilli: options.TopPMilli, ReasoningEffort: options.ReasoningEffort, ToolChoice: options.ToolChoice,
	}
	return summary
}

// writeProviderCheck 输出人类可读的探测报告；所有取值均为非敏感闭合字段。
func (command Command) writeProviderCheck(result providerCheckResult) {
	fmt.Fprintf(command.Stdout, "provider kind: %s\n", result.ProviderKind)
	fmt.Fprintf(command.Stdout, "endpoint identity: %s\n", result.EndpointIdentity)
	fmt.Fprintf(command.Stdout, "model: %s (revision: %s)\n", result.Model, result.ModelRevision)
	fmt.Fprintf(command.Stdout, "capability fingerprint: %s\n", result.CapabilityFingerprint)
	if summary := result.Generation; summary != nil {
		parts := make([]string, 0, 5)
		if summary.MaxOutputTokens > 0 {
			parts = append(parts, fmt.Sprintf("max_output_tokens=%d", summary.MaxOutputTokens))
		} else {
			parts = append(parts, fmt.Sprintf("max_output_tokens=%s", generationUnsupportedText))
		}
		for name, value := range map[string]*int{"temperature_milli": summary.TemperatureMilli, "top_p_milli": summary.TopPMilli} {
			if value != nil {
				parts = append(parts, fmt.Sprintf("%s=%d", name, *value))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", name, generationUnsupportedText))
		}
		for _, item := range []struct {
			name  string
			value string
		}{{"reasoning_effort", summary.ReasoningEffort}, {"tool_choice", summary.ToolChoice}} {
			if item.value == "" {
				parts = append(parts, fmt.Sprintf("%s=%s", item.name, generationUnsupportedText))
				continue
			}
			parts = append(parts, fmt.Sprintf("%s=%s", item.name, item.value))
		}
		fmt.Fprintf(command.Stdout, "generation: %s\n", strings.Join(parts, ", "))
	} else {
		fmt.Fprintf(command.Stdout, "generation: %s\n", generationUnsupportedText)
	}
	fmt.Fprintf(command.Stdout, "usage stream: %t\n", result.UsageStreamed)
	fmt.Fprintf(command.Stdout, "stop reason: %s\n", stopReasonOrUnknown(result.StopReason))
	fmt.Fprintf(command.Stdout, "tool roundtrip: %s\n", result.ToolRoundtrip)
	if result.Error != "" {
		fmt.Fprintf(command.Stdout, "error: %s\n", result.Error)
	}
	fmt.Fprintf(command.Stdout, "result: %s\n", result.Result)
}

// stopReasonOrUnknown 把空 stop reason 收敛为展示用的 unknown 标记。
func stopReasonOrUnknown(reason string) string {
	if reason == "" {
		return "unknown"
	}
	return reason
}

// captureProbeErrorClass 从探测错误中提取稳定分类与状态码：仅接受
// ProviderError 的闭合词表，其他错误不猜测类别，保持证据的脱敏合同。
func captureProbeErrorClass(result *providerCheckResult, err error) {
	var providerError *openaicompat.ProviderError
	if !errors.As(err, &providerError) {
		return
	}
	result.probeErrorClass = providerError.ErrorClass()
	result.probeStatusCode = providerError.StatusCode
}

// persistProbeEvidence 把一次真实执行过的探测观测落盘为内容寻址证据：
// 工作区哈希与 app 装配使用同一规范化算法（符号链接解析后的绝对路径摘要），
// 保证证据分区与准入门的绑定一致。落盘失败只报告不改变探测结论；未执行
// 网络探测的早期失败没有观测语义，直接跳过。
func persistProbeEvidence(workspacePath, stateDir string, configuration config.Config, result providerCheckResult) (string, error) {
	if !result.probed {
		return "", nil
	}
	root := workspacePath
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = current
	}
	workspace, err := tools.NewWorkspace(root, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(workspace.Root))
	workspaceHash := "sha256:" + hex.EncodeToString(digest[:])
	if stateDir == "" {
		stateDir = app.DefaultStateDir()
	}
	store, err := provider.OpenEvidenceStore(stateDir, workspaceHash)
	if err != nil {
		return "", err
	}
	selection, err := provider.ResolveSelection(provider.SelectionFacts{
		ProviderID: configuration.Provider.ID,
		Model:      configuration.Provider.Model,
		BaseURL:    configuration.Provider.BaseURL,
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
	} else if result.Result != probeResultFail {
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
	if result.Result == probeResultFail {
		outcome = provider.EvidenceOutcomeFailed
	}
	evidence := provider.ProviderProbeEvidence{
		SchemaVersion: 1, Selection: selection, ProbeContractVersion: provider.CurrentProbeContractVersion,
		Observations: observations, Outcome: outcome,
		ErrorClass: result.probeErrorClass, StatusCode: result.probeStatusCode,
		ProbedAt: time.Now().UTC(),
	}
	return store.Append(&evidence)
}
