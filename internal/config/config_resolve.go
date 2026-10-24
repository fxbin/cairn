// 配置解析：Provider/预算覆盖、Profile 与环境/CLI 合成。
// Author: fxbin
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

func resolveProviderConnection(cfg *Config, overlay providerOverlay, recorder *resolutionRecorder) error {
	if overlay.Protocol != nil {
		cfg.Provider.Protocol = *overlay.Protocol
	}
	if cfg.Provider.Protocol == "" {
		cfg.Provider.Protocol = ProtocolOpenAIChatCompletions
	}
	if _, supported := supportedProviderProtocols[cfg.Provider.Protocol]; !supported {
		return fmt.Errorf("provider.protocol %q is unsupported (supported: %s)", cfg.Provider.Protocol, supportedProviderProtocolList())
	}
	for name, preset := range cfg.Provider.Presets {
		if strings.TrimSpace(name) == "" {
			return errors.New("provider.presets name must not be empty")
		}
		if strings.TrimSpace(name) == "use" || strings.TrimSpace(name) == "presets" {
			return fmt.Errorf("provider.presets name %q is reserved", name)
		}
		protocol := preset.Protocol
		if protocol == "" {
			protocol = ProtocolOpenAIChatCompletions
		}
		if _, supported := supportedProviderProtocols[protocol]; !supported {
			return fmt.Errorf("provider.presets.%s.protocol %q is unsupported (supported: %s)", name, protocol, supportedProviderProtocolList())
		}
		if preset.BaseURL == "" || preset.Model == "" || preset.APIKeyEnv == "" {
			return fmt.Errorf("provider.presets.%s requires base_url, model and api_key_env", name)
		}
	}
	if overlay.Use == nil || *overlay.Use == "" {
		if overlay.Protocol == nil {
			recorder.record("provider.protocol", cfg.Provider.Protocol, "default")
		}
		return nil
	}
	cfg.Provider.Use = *overlay.Use
	preset, exists := cfg.Provider.Presets[cfg.Provider.Use]
	if !exists {
		return fmt.Errorf("provider.use %q does not match any provider.presets entry", cfg.Provider.Use)
	}
	// use 与内联连接字段互斥：避免出现“看似生效实则被预设覆盖”的歧义。
	if overlay.hasInlineConnection() {
		return errors.New("provider.use is mutually exclusive with inline provider connection fields")
	}
	protocol := preset.Protocol
	if protocol == "" {
		protocol = ProtocolOpenAIChatCompletions
	}
	requestTimeout := preset.RequestTimeout
	if requestTimeout == "" {
		requestTimeout = "120s"
	}
	admission := preset.Admission
	if admission == "" {
		admission = ProviderAdmissionRequired
	}
	providerID := preset.ID
	if providerID == "" {
		providerID = contract.ProviderIDCustom
	}
	cfg.Provider.Protocol = protocol
	cfg.Provider.BaseURL = preset.BaseURL
	cfg.Provider.Model = preset.Model
	cfg.Provider.APIKeyEnv = preset.APIKeyEnv
	cfg.Provider.RequestTimeout = requestTimeout
	cfg.Provider.ID = providerID
	cfg.Provider.Admission = admission
	cfg.Provider.Capability = preset.Capability
	cfg.Provider.Generation = preset.Generation
	recorder.record("provider.use", cfg.Provider.Use, "yaml")
	recorder.record("provider.protocol", cfg.Provider.Protocol, "preset:"+cfg.Provider.Use)
	recorder.record("provider.base_url", cfg.Provider.BaseURL, "preset:"+cfg.Provider.Use)
	recorder.record("provider.model", cfg.Provider.Model, "preset:"+cfg.Provider.Use)
	recorder.record("provider.api_key_env", cfg.Provider.APIKeyEnv, "preset:"+cfg.Provider.Use)
	return nil
}

// normalizePreset 归一化并应用静态 Preset 的组合语义：read-only 强制写入与
// 执行策略为 deny 并记录来源（Preset 即组合，显式 allow 配置被组合覆盖且在
// Resolution 中可见）；benchmark 复用 coding 工具集，其冻结配置、Docker 与
// 隔离约束由 benchmark runner 自行强制，不在通用配置层重复。presetSource 由
// YAML Presence 探针提供，从而避免 Defaults 预填 coding 后误报为 yaml。返回校验错误。
func normalizePreset(cfg *Config, recorder *resolutionRecorder, presetSource string) error {
	switch cfg.Preset {
	case "":
		cfg.Preset = PresetCoding
		recorder.record("preset", PresetCoding, presetSource)
		return nil
	case PresetCoding, PresetBenchmark:
		recorder.record("preset", cfg.Preset, presetSource)
		return nil
	case PresetReadOnly:
		recorder.record("preset", PresetReadOnly, presetSource)
		if cfg.Policy.Write != "deny" {
			cfg.Policy.Write = "deny"
			recorder.record("policy.write", "deny", "preset:read-only")
		}
		if cfg.Policy.Exec != "deny" {
			cfg.Policy.Exec = "deny"
			recorder.record("policy.exec", "deny", "preset:read-only")
		}
		if len(cfg.Verification.Commands) != 0 {
			return errors.New("read-only preset forbids verification commands")
		}
		return nil
	default:
		return fmt.Errorf("unknown preset %q (supported: %s, %s, %s)", cfg.Preset, PresetCoding, PresetReadOnly, PresetBenchmark)
	}
}

// ApplyPreset 在已装载配置上应用静态 Preset 组合语义并重新校验。
// 桌面/GUI 切换模式必须走此入口，禁止局部改写 Policy 模拟只读。
func ApplyPreset(cfg *Config, preset string) error {
	if cfg == nil {
		return errors.New("config is required")
	}
	cfg.Preset = strings.TrimSpace(preset)
	recorder := &resolutionRecorder{}
	if err := normalizePreset(cfg, recorder, "runtime:preset"); err != nil {
		return err
	}
	return Validate(*cfg)
}

// budgetOverlay 以指针字段探测 YAML 中哪些预算与 Provider 关键字段被显式设置，
// 用于区分"字段缺省（可被命名 Profile 预设填充）"与"用户显式覆盖（优先于 Profile）"。
type budgetOverlay struct {
	MaxTurns        *int     `yaml:"max_turns"`
	MaxToolCalls    *int     `yaml:"max_tool_calls"`
	MaxInputTokens  *int     `yaml:"max_input_tokens"`
	MaxOutputTokens *int     `yaml:"max_output_tokens"`
	MaxWallTime     *string  `yaml:"max_wall_time"`
	MaxCostUSD      *float64 `yaml:"max_cost_usd"`
}

// providerOverlay 探测 Provider 关键字段的显式设置，用于来源记录。
type providerOverlay struct {
	BaseURL        *string `yaml:"base_url"`
	Model          *string `yaml:"model"`
	APIKeyEnv      *string `yaml:"api_key_env"`
	RequestTimeout *string `yaml:"request_timeout"`
	ID             *string `yaml:"id"`
	Admission      *string `yaml:"admission"`
	Use            *string `yaml:"use"`
	Protocol       *string `yaml:"protocol"`
}

// hasInlineConnection 报告 YAML 是否显式声明了内联连接字段；用于 provider.use
// 与内联字段互斥判定，不受 Defaults 预填影响。
func (overlay providerOverlay) hasInlineConnection() bool {
	return overlay.BaseURL != nil || overlay.Model != nil || overlay.APIKeyEnv != nil ||
		overlay.RequestTimeout != nil || overlay.ID != nil || overlay.Admission != nil
}

// profileProbe 是宽松解码的目标：确认配置版本显式在场，并提取 Profile 名称
// 与关键字段的显式设置。
type profileProbe struct {
	Version  *int            `yaml:"version"`
	Profile  string          `yaml:"profile"`
	Preset   *string         `yaml:"preset"`
	Budget   budgetOverlay   `yaml:"budget"`
	Provider providerOverlay `yaml:"provider"`
}

// profileBudget 返回命名 Profile 的预算预设；complex 档在默认档基础上按
// 2026-08-15 G0-R gate 实验与 8/13 验证参数非等比放大：轮次 2×（40→80）、
// 工具调用 1.6×（100→160）、输入 token 2.8×（500k→1.4M）。显式 YAML、环境
// 或 CLI 值仍按固定优先级覆盖预设。
func profileBudget(profile string) (BudgetConfig, error) {
	switch profile {
	case "", ProfileDefault:
		return Defaults().Budget, nil
	case ProfileComplex:
		budget := Defaults().Budget
		budget.MaxTurns = ComplexMaxTurns
		budget.MaxToolCalls = 160
		budget.MaxInputTokens = 1_400_000
		return budget, nil
	default:
		return BudgetConfig{}, fmt.Errorf("unknown execution profile %q (supported: %s, %s)", profile, ProfileDefault, ProfileComplex)
	}
}

// resolutionRecorder 构建 Resolved Execution Plan；同一字段再次记录时更新来源。
type resolutionRecorder struct {
	entries []ResolutionEntry
}

// record 追加或更新一个字段的最新值与来源。
func (recorder *resolutionRecorder) record(field, value, source string) {
	for index := range recorder.entries {
		if recorder.entries[index].Field == field {
			recorder.entries[index].Value = value
			recorder.entries[index].Source = source
			return
		}
	}
	recorder.entries = append(recorder.entries, ResolutionEntry{Field: field, Value: value, Source: source})
}

// WithPresetResolution 返回一份仅更新 Resolved Execution Plan 中 preset 记录的
// 配置副本。它不改变运行时 Preset、策略或配置指纹，供 benchmark runner 在强制
// benchmark Preset 后保留冻结配置的解释一致性。
func applyProfilePreset(config *Config, explicit *budgetOverlay, preset BudgetConfig, profile string, recorder *resolutionRecorder) {
	if explicit.MaxTurns != nil {
		recorder.record("budget.max_turns", strconv.Itoa(config.Budget.MaxTurns), "yaml")
	} else {
		config.Budget.MaxTurns = preset.MaxTurns
		recorder.record("budget.max_turns", strconv.Itoa(config.Budget.MaxTurns), budgetSourceLabel(profile))
	}
	if explicit.MaxToolCalls != nil {
		recorder.record("budget.max_tool_calls", strconv.Itoa(config.Budget.MaxToolCalls), "yaml")
	} else {
		config.Budget.MaxToolCalls = preset.MaxToolCalls
		recorder.record("budget.max_tool_calls", strconv.Itoa(config.Budget.MaxToolCalls), budgetSourceLabel(profile))
	}
	if explicit.MaxInputTokens != nil {
		recorder.record("budget.max_input_tokens", strconv.Itoa(config.Budget.MaxInputTokens), "yaml")
	} else {
		config.Budget.MaxInputTokens = preset.MaxInputTokens
		recorder.record("budget.max_input_tokens", strconv.Itoa(config.Budget.MaxInputTokens), budgetSourceLabel(profile))
	}
	if explicit.MaxOutputTokens != nil {
		recorder.record("budget.max_output_tokens", strconv.Itoa(config.Budget.MaxOutputTokens), "yaml")
	} else {
		config.Budget.MaxOutputTokens = preset.MaxOutputTokens
		recorder.record("budget.max_output_tokens", strconv.Itoa(config.Budget.MaxOutputTokens), budgetSourceLabel(profile))
	}
	if explicit.MaxWallTime != nil {
		recorder.record("budget.max_wall_time", config.Budget.MaxWallTime, "yaml")
	} else {
		config.Budget.MaxWallTime = preset.MaxWallTime
		recorder.record("budget.max_wall_time", config.Budget.MaxWallTime, budgetSourceLabel(profile))
	}
	if explicit.MaxCostUSD != nil {
		recorder.record("budget.max_cost_usd", strconv.FormatFloat(config.Budget.MaxCostUSD, 'f', -1, 64), "yaml")
	} else {
		config.Budget.MaxCostUSD = preset.MaxCostUSD
		recorder.record("budget.max_cost_usd", strconv.FormatFloat(config.Budget.MaxCostUSD, 'f', -1, 64), budgetSourceLabel(profile))
	}
}

// Validate 验证配置版本、限额、时长、策略与验证命令的安全约束。
func recordProviderGenerationResolution(generation *ProviderGenerationConfig, recorder *resolutionRecorder) {
	if generation == nil {
		for _, field := range generationResolutionFields {
			recorder.record(field, resolutionUnsupportedValue, "default")
		}
		return
	}
	recordExplicitGeneration(generation, recorder)
}

// generationResolutionFields 是生成参数在 Resolved Execution Plan 中的字段名，
// 与 ProviderGenerationConfig 的 YAML 键一一对应。
var generationResolutionFields = []string{
	"provider.generation.max_output_tokens",
	"provider.generation.temperature_milli",
	"provider.generation.top_p_milli",
	"provider.generation.reasoning_effort",
	"provider.generation.tool_choice",
}

// recordExplicitGeneration 记录已声明生成参数块的逐字段两态：显式值保留
// yaml 来源，块内缺省字段仍记为 unsupported。
func recordExplicitGeneration(generation *ProviderGenerationConfig, recorder *resolutionRecorder) {
	valueOf := []struct {
		field string
		set   bool
		value string
	}{
		{"provider.generation.max_output_tokens", generation.MaxOutputTokens != nil, itoaOrEmpty(generation.MaxOutputTokens)},
		{"provider.generation.temperature_milli", generation.TemperatureMilli != nil, itoaOrEmpty(generation.TemperatureMilli)},
		{"provider.generation.top_p_milli", generation.TopPMilli != nil, itoaOrEmpty(generation.TopPMilli)},
		{"provider.generation.reasoning_effort", generation.ReasoningEffort != "", generation.ReasoningEffort},
		{"provider.generation.tool_choice", generation.ToolChoice != "", generation.ToolChoice},
	}
	for _, item := range valueOf {
		if item.set {
			recorder.record(item.field, item.value, "yaml")
			continue
		}
		recorder.record(item.field, resolutionUnsupportedValue, "default")
	}
}

// itoaOrEmpty 把可选 Milli 整数转换为文本；nil 返回空串由调用方判空。
func itoaOrEmpty(value *int) string {
	if value == nil {
		return ""
	}
	return strconv.Itoa(*value)
}

// fingerprintView 显式列出参与 v2 冻结指纹的字段：只覆盖影响执行语义、
// 审批授权与预算终止的配置。展示与运维类字段（request_timeout、
// terminate_grace、output 限额）被刻意排除；新增执行语义字段时必须同步
// 加入此结构并补充指纹变化测试。
func applyEnvironment(config *Config, recorder *resolutionRecorder) error {
	if value := os.Getenv("AGENT_BASE_URL"); value != "" {
		config.Provider.BaseURL = value
		recorder.record("provider.base_url", value, "env")
	}
	if value := os.Getenv("AGENT_MODEL"); value != "" {
		config.Provider.Model = value
		recorder.record("provider.model", value, "env")
	}
	if value := os.Getenv("AGENT_API_KEY_ENV"); value != "" {
		config.Provider.APIKeyEnv = value
		recorder.record("provider.api_key_env", value, "env")
	}
	integerOverrides := []struct {
		name   string
		target *int
	}{
		{"AGENT_MAX_TURNS", &config.Budget.MaxTurns},
		{"AGENT_MAX_TOOL_CALLS", &config.Budget.MaxToolCalls},
		{"AGENT_MAX_INPUT_TOKENS", &config.Budget.MaxInputTokens},
		{"AGENT_MAX_OUTPUT_TOKENS", &config.Budget.MaxOutputTokens},
	}
	for _, override := range integerOverrides {
		value := os.Getenv(override.name)
		if value == "" {
			continue
		}
		limit, err := strconv.Atoi(value)
		if err != nil {
			return fmt.Errorf("%s: %w", override.name, err)
		}
		*override.target = limit
		recorder.record(envSourceField[override.name], strconv.Itoa(limit), "env")
	}
	if value := os.Getenv("AGENT_MAX_WALL_TIME"); value != "" {
		config.Budget.MaxWallTime = value
		recorder.record("budget.max_wall_time", value, "env")
	}
	if value := os.Getenv("AGENT_MAX_COST_USD"); value != "" {
		limit, err := strconv.ParseFloat(value, 64)
		if err != nil {
			return fmt.Errorf("AGENT_MAX_COST_USD: %w", err)
		}
		config.Budget.MaxCostUSD = limit
		recorder.record("budget.max_cost_usd", strconv.FormatFloat(limit, 'f', -1, 64), "env")
	}
	return nil
}

// envSourceField 将 AGENT_* 环境变量名映射到 Resolved Plan 的字段名。
var envSourceField = map[string]string{
	"AGENT_MAX_TURNS":         "budget.max_turns",
	"AGENT_MAX_TOOL_CALLS":    "budget.max_tool_calls",
	"AGENT_MAX_INPUT_TOKENS":  "budget.max_input_tokens",
	"AGENT_MAX_OUTPUT_TOKENS": "budget.max_output_tokens",
}

// applyOverrides 应用已经由 CLI 解析完成的显式非零覆盖项并记录来源。
func applyOverrides(config *Config, overrides Overrides, recorder *resolutionRecorder) {
	if overrides.BaseURL != "" {
		config.Provider.BaseURL = overrides.BaseURL
		recorder.record("provider.base_url", overrides.BaseURL, "cli")
	}
	if overrides.Model != "" {
		config.Provider.Model = overrides.Model
		recorder.record("provider.model", overrides.Model, "cli")
	}
	if overrides.APIKeyEnv != "" {
		config.Provider.APIKeyEnv = overrides.APIKeyEnv
		recorder.record("provider.api_key_env", overrides.APIKeyEnv, "cli")
	}
	if overrides.MaxTurns > 0 {
		config.Budget.MaxTurns = overrides.MaxTurns
		recorder.record("budget.max_turns", strconv.Itoa(overrides.MaxTurns), "cli")
	}
	if overrides.MaxToolCalls > 0 {
		config.Budget.MaxToolCalls = overrides.MaxToolCalls
		recorder.record("budget.max_tool_calls", strconv.Itoa(overrides.MaxToolCalls), "cli")
	}
	if overrides.MaxInputTokens > 0 {
		config.Budget.MaxInputTokens = overrides.MaxInputTokens
		recorder.record("budget.max_input_tokens", strconv.Itoa(overrides.MaxInputTokens), "cli")
	}
	if overrides.MaxOutputTokens > 0 {
		config.Budget.MaxOutputTokens = overrides.MaxOutputTokens
		recorder.record("budget.max_output_tokens", strconv.Itoa(overrides.MaxOutputTokens), "cli")
	}
	if overrides.MaxWallTime != "" {
		config.Budget.MaxWallTime = overrides.MaxWallTime
		recorder.record("budget.max_wall_time", overrides.MaxWallTime, "cli")
	}
	if overrides.MaxCostUSD > 0 {
		config.Budget.MaxCostUSD = overrides.MaxCostUSD
		recorder.record("budget.max_cost_usd", strconv.FormatFloat(overrides.MaxCostUSD, 'f', -1, 64), "cli")
	}
}
