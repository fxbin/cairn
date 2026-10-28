// 配置严格校验。
// Author: fxbin
package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/credential"
	"github.com/fxbin/cairn/internal/provider"
)

func Validate(config Config) error {
	if err := validateSkills(config.Skills, config.Preset); err != nil {
		return err
	}
	if err := validateMCP(config.MCP, config.Preset); err != nil {
		return err
	}
	if config.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d", config.Version)
	}
	if config.Provider.BaseURL == "" || config.Provider.Model == "" {
		return fmt.Errorf("config version 2 requires provider.base_url and provider.model")
	}
	if config.Provider.Admission != ProviderAdmissionOptional && config.Provider.Admission != ProviderAdmissionRequired {
		return fmt.Errorf("provider.admission must be %s or %s in config version 2", ProviderAdmissionOptional, ProviderAdmissionRequired)
	}
	if config.Provider.ID != "" {
		// 完整注册表校验（形状 + 静态成员）：未知成员失败关闭，没有隐式
		// 回退到 custom。
		if _, err := provider.Lookup(config.Provider.ID); err != nil {
			return fmt.Errorf("provider.id: %w", err)
		}
	}
	if config.Profile != "" && config.Profile != ProfileDefault && config.Profile != ProfileComplex {
		return fmt.Errorf("profile must be %s or %s", ProfileDefault, ProfileComplex)
	}
	switch config.Preset {
	case "", PresetCoding, PresetReadOnly, PresetBenchmark:
	default:
		return fmt.Errorf("unknown preset %q (supported: %s, %s, %s)", config.Preset, PresetCoding, PresetReadOnly, PresetBenchmark)
	}
	if config.Preset == PresetReadOnly {
		if config.Policy.Write != "deny" || config.Policy.Exec != "deny" || len(config.Verification.Commands) != 0 {
			return fmt.Errorf("read-only preset requires deny policies and no verification commands")
		}
		// read-only 面不注册委派工具，异步委派开关在其上只会制造可达的失败
		// 提议，配置层直接失败关闭。
		if config.Tools.DelegateAsync {
			return fmt.Errorf("tools.delegate_async is not available with the %s preset", PresetReadOnly)
		}
	}
	if config.Provider.APIKeyEnv == "" {
		return fmt.Errorf("provider.api_key_env is required")
	}
	if err := providerAPIKeyEnv(config.Provider.APIKeyEnv); err != nil {
		return fmt.Errorf("provider.api_key_env: %w", err)
	}
	if config.Provider.BaseURL != "" {
		if err := validateProviderBaseURL(config.Provider.BaseURL); err != nil {
			return err
		}
	}
	if capability := config.Provider.Capability; capability != nil {
		if !capability.ToolCalls || !capability.StreamingUsage {
			return fmt.Errorf("provider.capability requires tool_calls and streaming_usage to be true")
		}
		if capability.ContextWindowTokens < 0 || capability.MaxOutputTokens < 0 {
			return fmt.Errorf("provider.capability token limits cannot be negative")
		}
		for _, reason := range capability.StopReasons {
			if reason == "" {
				return fmt.Errorf("provider.capability stop reasons cannot be empty strings")
			}
		}
	}
	if err := validateProviderGeneration(config.Provider.Generation, config.Provider.Capability); err != nil {
		return err
	}
	if config.Provider.RequestTimeout == "" {
		return fmt.Errorf("provider.request_timeout is required")
	}
	if _, err := time.ParseDuration(config.Provider.RequestTimeout); err != nil {
		return fmt.Errorf("provider.request_timeout: %w", err)
	}
	if config.Workspace.MaxReadBytes < 1 || config.Workspace.MaxSearchFiles < 1 || config.Workspace.MaxSearchBytes < 1 || config.Budget.MaxTurns < 1 || config.Budget.MaxToolCalls < 1 || config.Budget.MaxInputTokens < 1 || config.Budget.MaxOutputTokens < 1 {
		return fmt.Errorf("workspace limits and budget limits must be positive")
	}
	if config.Context.DecisionCheckpoint != nil {
		if config.Context.DecisionCheckpoint.MinTurns < 0 {
			return errors.New("context.decision_checkpoint.min_turns must not be negative")
		}
	}
	if config.Context.MaxPromptTokens < 1 || config.Context.MaxSummaryBytes < 1 {
		return fmt.Errorf("context limits must be positive")
	}
	if config.Context.InventoryBudgetPercent < 1 || config.Context.InventoryBudgetPercent > 99 {
		return fmt.Errorf("context.inventory_budget_percent must be within 1 and 99")
	}
	if config.Context.ThresholdReservePercent < 1 || config.Context.ThresholdReservePercent > 99 {
		return fmt.Errorf("context.threshold_reserve_percent must be within 1 and 99")
	}
	if _, err := config.WallTime(); err != nil {
		return fmt.Errorf("budget.max_wall_time: %w", err)
	}
	if config.Budget.MaxCostUSD < 0 {
		return fmt.Errorf("budget.max_cost_usd cannot be negative")
	}
	if config.Pricing.InputTokenUSD < 0 || config.Pricing.OutputTokenUSD < 0 {
		return fmt.Errorf("pricing token rates cannot be negative")
	}
	if config.Output.MaxPreviewBytes < 1 || config.Output.MaxArtifactBytes < 1 {
		return fmt.Errorf("output limits must be positive")
	}
	if _, err := config.TerminateGrace(); err != nil {
		return fmt.Errorf("cancellation.terminate_grace: %w", err)
	}
	if err := validateSandbox(config.Sandbox); err != nil {
		return err
	}
	if config.Budget.MaxCostUSD > 0 && (config.Pricing.InputTokenUSD <= 0 || config.Pricing.OutputTokenUSD <= 0) {
		return fmt.Errorf("budget.max_cost_usd requires explicit positive pricing token rates")
	}
	if !validPolicyEffect(config.Policy.Write) || !validPolicyEffect(config.Policy.Exec) {
		return fmt.Errorf("policy.write and policy.exec must be allow, deny, or ask")
	}
	allowedEnvironment := make(map[string]struct{}, len(config.Policy.EnvAllow))
	for _, key := range config.Policy.EnvAllow {
		if !validEnvironmentKey(key) {
			return fmt.Errorf("policy.env_allow contains invalid environment variable %q", key)
		}
		if credential.Key(key) {
			return fmt.Errorf("policy.env_allow cannot expose secret-like environment variable %q", key)
		}
		allowedEnvironment[key] = struct{}{}
	}
	for index, command := range config.Verification.Commands {
		if len(command.Argv) == 0 || command.Argv[0] == "" {
			return fmt.Errorf("verification.commands[%d].argv is required", index)
		}
		if command.Timeout != "" {
			if _, err := time.ParseDuration(command.Timeout); err != nil {
				return fmt.Errorf("verification.commands[%d].timeout: %w", index, err)
			}
		}
		for key := range command.Env {
			if _, allowed := allowedEnvironment[key]; !allowed {
				return fmt.Errorf("verification.commands[%d].env %q is not in policy.env_allow", index, key)
			}
		}
	}
	return nil
}

// 生成参数枚举取值来自 contract 包的单一事实源；此处只保留引用别名以维持
// 校验函数的可读性，任何取值变更都必须发生在合同层。
const (
	GenerationToolChoiceAuto = contract.GenerationToolChoiceAuto
	ReasoningEffortOff       = contract.ReasoningEffortOff
	ReasoningEffortLow       = contract.ReasoningEffortLow
	ReasoningEffortMedium    = contract.ReasoningEffortMedium
	ReasoningEffortHigh      = contract.ReasoningEffortHigh
)

// 生成参数的 Milli 边界引用合同层常量：温度千分位上限与 top_p 千分位区间，
// 指纹禁浮点。
const (
	generationTemperatureMaxMilli = contract.GenerationTempMaxMilli
	generationTopPMinMilli        = contract.GenerationTopPMinMilli
	generationTopPMaxMilli        = contract.GenerationTopPMaxMilli
)

// validateProviderGeneration 校验生成参数块的闭合取值与边界：Milli 整数必须
// 落在合同区间，枚举字段只接受声明的常量；显式的输出上限不得超过能力卡声明
// 的提供方上限（双方都声明时对照），防止生成配置越过已知模型边界。任何非法
// 值都在 Load 阶段失败关闭，而不是发送请求后才暴露。
func validateProviderGeneration(generation *ProviderGenerationConfig, capability *ProviderCapabilityConfig) error {
	if generation == nil {
		return nil
	}
	if generation.MaxOutputTokens != nil {
		if *generation.MaxOutputTokens <= 0 {
			return fmt.Errorf("provider.generation.max_output_tokens %d must be a positive integer", *generation.MaxOutputTokens)
		}
		if capability != nil && capability.MaxOutputTokens > 0 && *generation.MaxOutputTokens > capability.MaxOutputTokens {
			return fmt.Errorf("provider.generation.max_output_tokens %d exceeds declared provider capability limit %d", *generation.MaxOutputTokens, capability.MaxOutputTokens)
		}
	}
	if generation.TemperatureMilli != nil && (*generation.TemperatureMilli < 0 || *generation.TemperatureMilli > generationTemperatureMaxMilli) {
		return fmt.Errorf("provider.generation.temperature_milli %d outside supported range [0,%d]", *generation.TemperatureMilli, generationTemperatureMaxMilli)
	}
	if generation.TopPMilli != nil && (*generation.TopPMilli < generationTopPMinMilli || *generation.TopPMilli > generationTopPMaxMilli) {
		return fmt.Errorf("provider.generation.top_p_milli %d outside supported range [%d,%d]", *generation.TopPMilli, generationTopPMinMilli, generationTopPMaxMilli)
	}
	switch generation.ReasoningEffort {
	case "", ReasoningEffortOff, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh:
	default:
		return fmt.Errorf("unknown provider.generation.reasoning_effort %q (supported: %s, %s, %s, %s)", generation.ReasoningEffort, ReasoningEffortOff, ReasoningEffortLow, ReasoningEffortMedium, ReasoningEffortHigh)
	}
	switch generation.ToolChoice {
	case "", GenerationToolChoiceAuto:
	default:
		return fmt.Errorf("unknown provider.generation.tool_choice %q (supported: %s)", generation.ToolChoice, GenerationToolChoiceAuto)
	}
	return nil
}

// validateProviderBaseURL 拒绝携带 userinfo、query 或 fragment 的 Provider 基地址：
// 这些成分可能内嵌凭据，会随冻结配置制品、Resolution 与诊断输出落盘，绕开
// "密钥只能来自环境变量"的合同。空值不在此处强制必填，由请求层兜底校验。
func validateProviderBaseURL(baseURL string) error {
	parsed, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("provider.base_url must be an absolute http(s) URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("provider.base_url scheme must be http or https")
	}
	if parsed.User != nil {
		return fmt.Errorf("provider.base_url must not embed userinfo credentials")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" || parsed.RawFragment != "" {
		return fmt.Errorf("provider.base_url must not carry query or fragment")
	}
	return nil
}

// validateSandbox 验证执行后端配置，避免 Docker 参数将镜像名称解释成额外选项，
// 并为容器进程数设置有限上限。
func validateSandbox(sandbox SandboxConfig) error {
	if sandbox.MaxPIDs < 1 || sandbox.MaxPIDs > 1024 {
		return fmt.Errorf("sandbox.max_pids must be between 1 and 1024")
	}
	switch sandbox.Backend {
	case "host":
		if sandbox.DockerImage != "" {
			return fmt.Errorf("sandbox.docker_image is only valid when sandbox.backend is docker")
		}
		return nil
	case "docker":
		if sandbox.DockerImage == "" {
			return fmt.Errorf("sandbox.docker_image is required when sandbox.backend is docker")
		}
		if sandbox.DockerImage != strings.TrimSpace(sandbox.DockerImage) || strings.HasPrefix(sandbox.DockerImage, "-") || strings.ContainsAny(sandbox.DockerImage, " \t\r\n\x00") {
			return fmt.Errorf("sandbox.docker_image is invalid")
		}
		return nil
	default:
		return fmt.Errorf("sandbox.backend must be host or docker")
	}
}

// validPolicyEffect 判断策略效果是否属于 allow、deny 或 ask。
func validPolicyEffect(value string) bool {
	return value == "allow" || value == "deny" || value == "ask"
}

// validEnvironmentKey 验证环境变量名称的可移植字符集。
func validEnvironmentKey(key string) bool {
	if key == "" {
		return false
	}
	for index, character := range key {
		if !(character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' || index > 0 && character >= '0' && character <= '9') {
			return false
		}
	}
	return true
}

// providerAPIKeyEnv 拒绝把 provider.api_key_env 配成非法变量名；校验只覆盖
// 确定性语法（可移植字符集），不做密钥形状猜测——语法合法的值一律放行，
// 实际凭据治理依靠轮换、环境注入与禁止落盘完成。错误文本不回显输入值，
// 防止误填的真实密钥随错误输出扩散。
func providerAPIKeyEnv(name string) error {
	if !validEnvironmentKey(name) {
		return errors.New("api_key_env must be a portable environment variable name: letters, digits and underscores, not starting with a digit")
	}
	return nil
}

// RequestTimeout 解析模型请求超时配置。
