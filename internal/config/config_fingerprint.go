// v4 冻结配置指纹。
// Author: fxbin
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/provider"
)

type fingerprintView struct {
	Version      int                          `json:"version"`
	Profile      string                       `json:"profile"`
	Preset       string                       `json:"preset"`
	Provider     providerSelectionFingerprint `json:"provider"`
	Workspace    WorkspaceConfig              `json:"workspace"`
	Policy       PolicyConfig                 `json:"policy"`
	Verification VerificationConfig           `json:"verification"`
	Budget       BudgetConfig                 `json:"budget"`
	Context      ContextConfig                `json:"context"`
	Pricing      PricingConfig                `json:"pricing"`
	Sandbox      SandboxConfig                `json:"sandbox"`
	MCP          *MCPConfig                   `json:"mcp,omitempty"`
	Skills       *SkillsConfig                `json:"skills,omitempty"`
	Tools        *ToolsConfig                 `json:"tools,omitempty"`
	Hooks        *[]HookConfig                `json:"hooks,omitempty"`
	Prompt       *PromptConfig                `json:"prompt,omitempty"`
	Guard        *GuardConfig                 `json:"guard,omitempty"`
	ActiveMode   string                       `json:"active_mode,omitempty"`
}

// providerCapabilityFingerprint 是进入指纹的能力声明投影。
type providerCapabilityFingerprint struct {
	ToolCalls           bool     `json:"tool_calls"`
	StreamingUsage      bool     `json:"streaming_usage"`
	ContextWindowTokens int      `json:"context_window_tokens"`
	MaxOutputTokens     int      `json:"max_output_tokens"`
	StopReasons         []string `json:"stop_reasons"`
}

// providerGenerationFingerprint 是进入指纹的生成参数投影：未声明字段经
// omitempty 缺席表达 unsupported，显式值与缺席在字节层面可区分；块未声明时
// 整个投影为 nil 且键整体缺席，保证既有配置的指纹逐位不变。
type providerGenerationFingerprint struct {
	MaxOutputTokens  int    `json:"max_output_tokens,omitempty"`
	TemperatureMilli *int   `json:"temperature_milli,omitempty"`
	TopPMilli        *int   `json:"top_p_milli,omitempty"`
	ReasoningEffort  string `json:"reasoning_effort,omitempty"`
	ToolChoice       string `json:"tool_choice,omitempty"`
}

// Fingerprint 生成可写入事件日志的 v2 冻结非敏感配置摘要。Provider 段使用
// ProviderSelection 身份投影；endpoint_digest 是规范化身份，不是保密手段。
// 落盘值只有 "sha256:<hex>"，禁止从中推断算法版本。
func (config Config) Fingerprint() string {
	profile := config.Profile
	if profile == "" {
		profile = ProfileDefault
	}
	view := fingerprintView{
		Version:      config.Version,
		Profile:      profile,
		Preset:       config.Preset,
		Provider:     providerSelectionFingerprintFor(config),
		Workspace:    config.Workspace,
		Policy:       config.Policy,
		Verification: config.Verification,
		Budget:       config.Budget,
		Context:      config.Context,
		Pricing:      config.Pricing,
		Sandbox:      config.Sandbox,
		MCP:          mcpFingerprint(config.MCP),
		Skills:       skillsFingerprint(config.Skills),
		Tools:        toolsFingerprint(config.Tools),
		Hooks:        hooksFingerprint(config.Hooks),
		Prompt:       promptFingerprint(config.Prompt),
		Guard:        config.Guard,
		ActiveMode:   config.ActiveMode,
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(append([]byte("cairn-config-fingerprint-v4\x00"), encoded...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// toolsFingerprint 仅在存在 opt-in 工具时进入指纹，保证默认配置指纹不变。
func toolsFingerprint(tools ToolsConfig) *ToolsConfig {
	if !tools.Web && !tools.ExecSession && !tools.Browser && !tools.RunTests && !tools.LspDiagnostics {
		return nil
	}
	copied := tools
	return &copied
}

// promptFingerprint 仅在自定义系统提示时进入指纹。
func promptFingerprint(prompt PromptConfig) *PromptConfig {
	resolved := strings.TrimSpace(prompt.Resolved)
	if resolved == "" {
		resolved = strings.TrimSpace(prompt.SystemPrompt)
	}
	if resolved == "" && strings.TrimSpace(prompt.SystemPromptFile) == "" {
		return nil
	}
	copied := PromptConfig{SystemPrompt: resolved}
	return &copied
}

// hooksFingerprint 仅在配置了钩子时进入指纹。
func hooksFingerprint(rules []HookConfig) *[]HookConfig {
	if len(rules) == 0 {
		return nil
	}
	copied := append([]HookConfig(nil), rules...)
	return &copied
}

// providerSelectionFingerprint 是 v2 指纹的 provider 段：身份字段（provider_id、
// protocol_id、model_ref、endpoint_digest、api_key_env、admission）永远在场，
// 缺省 provider_id 归一为显式 "custom"，禁止 omitempty 制造同语义不同指纹；
// 能力与生成参数块保留指针两态语义。
type providerSelectionFingerprint struct {
	ProviderID     string                         `json:"provider_id"`
	ProtocolID     string                         `json:"protocol_id"`
	Model          string                         `json:"model"`
	ModelRef       string                         `json:"model_ref"`
	EndpointDigest string                         `json:"endpoint_digest"`
	APIKeyEnv      string                         `json:"api_key_env"`
	Admission      string                         `json:"admission"`
	Capability     *providerCapabilityFingerprint `json:"capability,omitempty"`
	Generation     *providerGenerationFingerprint `json:"generation,omitempty"`
}

// providerSelectionFingerprintFor 组装 v2 的 provider 身份段：身份字段复用
// 静态注册表与 Selection 派生函数，保证指纹、manifest 投影与准入绑定使用
// 同一身份事实来源。调用前提是配置已通过 v2 校验（base_url 与
// admission 必在场）；派生失败属于防御路径，直接 panic 失败关闭。
func providerSelectionFingerprintFor(config Config) providerSelectionFingerprint {
	selection, err := provider.ResolveSelection(provider.SelectionFacts{
		ProviderID: config.Provider.ID,
		Model:      config.Provider.Model,
		BaseURL:    config.Provider.BaseURL,
		Protocol:   config.Provider.Protocol,
	})
	if err != nil {
		panic(fmt.Sprintf("config v2 must derive a provider selection: %v", err))
	}
	view := providerSelectionFingerprint{
		ProviderID:     selection.ProviderID,
		ProtocolID:     selection.ProtocolID,
		Model:          config.Provider.Model,
		ModelRef:       selection.ModelRef,
		EndpointDigest: selection.EndpointDigest,
		APIKeyEnv:      config.Provider.APIKeyEnv,
		Admission:      config.Provider.Admission,
	}
	if capability := config.Provider.Capability; capability != nil {
		view.Capability = &providerCapabilityFingerprint{
			ToolCalls: capability.ToolCalls, StreamingUsage: capability.StreamingUsage,
			ContextWindowTokens: capability.ContextWindowTokens, MaxOutputTokens: capability.MaxOutputTokens,
			StopReasons: capability.StopReasons,
		}
	}
	if generation := config.Provider.Generation; generation != nil {
		view.Generation = &providerGenerationFingerprint{
			TemperatureMilli: generation.TemperatureMilli,
			TopPMilli:        generation.TopPMilli, ReasoningEffort: generation.ReasoningEffort, ToolChoice: generation.ToolChoice,
		}
		if generation.MaxOutputTokens != nil {
			view.Generation.MaxOutputTokens = *generation.MaxOutputTokens
		}
	}
	return view
}

// DefaultPath 返回工作区内的默认配置文件路径。
