// Package config 读取 Cairn 小型、不可包含密钥的仓库配置。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/credential"
	"gopkg.in/yaml.v3"
)

const (
	// DefaultMaxTurns 是普通任务的默认模型轮次硬上限。
	DefaultMaxTurns = 40
	// ComplexMaxTurns 是复杂任务建议显式选择的模型轮次档位；更高上限仍由场景配置决定。
	ComplexMaxTurns = 80
)

// Config 汇集一次 Cairn 运行的全部非敏感配置。
type Config struct {
	Version      int                `yaml:"version"`
	Profile      string             `yaml:"profile"`
	Preset       string             `yaml:"preset"`
	Provider     ProviderConfig     `yaml:"provider"`
	Workspace    WorkspaceConfig    `yaml:"workspace"`
	Policy       PolicyConfig       `yaml:"policy"`
	Verification VerificationConfig `yaml:"verification"`
	Budget       BudgetConfig       `yaml:"budget"`
	Context      ContextConfig      `yaml:"context"`
	Pricing      PricingConfig      `yaml:"pricing"`
	Output       OutputConfig       `yaml:"output"`
	Cancellation CancellationConfig `yaml:"cancellation"`
	Sandbox      SandboxConfig      `yaml:"sandbox"`
	// Resolution 是解析过程生成的非敏感 Resolved Execution Plan（字段→值→来源），
	// 只读派生事实：不参与 YAML 解码、不进入配置指纹（语义已由解析后的字段承载），
	// 随冻结配置制品持久化，供诊断与可访问性视图使用。
	Resolution []ResolutionEntry `yaml:"-" json:"resolution,omitempty"`
}

// ResolutionEntry 是 Resolved Execution Plan 的单条记录：关键字段的最终值与其
// 来源（default、yaml、profile:<name>、env 或 cli）。
type ResolutionEntry struct {
	Field  string `json:"field"`
	Value  string `json:"value"`
	Source string `json:"source"`
}

// ProfileDefault 与 ProfileComplex 是当前版本化的命名 Execution Profile；空值
// 等价 default。Profile 只预设预算字段，不自动推断任务复杂度。
const (
	ProfileDefault = "default"
	ProfileComplex = "complex"
)

// 静态 Preset 是编译期已知组件的能力组合：工具子集、策略模式与验证要求。
// 不存在目录扫描、自注册或动态加载；未知 Preset 在校验层失败关闭。
const (
	PresetCoding    = "coding"
	PresetReadOnly  = "read-only"
	PresetBenchmark = "benchmark"
)

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
	BaseURL   *string `yaml:"base_url"`
	Model     *string `yaml:"model"`
	APIKeyEnv *string `yaml:"api_key_env"`
}

// profileProbe 是第二轮宽松解码的目标：只提取 Profile 名称与显式字段设置。
type profileProbe struct {
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
func WithPresetResolution(configuration Config, preset, source string) Config {
	configuration.Resolution = append([]ResolutionEntry(nil), configuration.Resolution...)
	recorder := resolutionRecorder{entries: configuration.Resolution}
	recorder.record("preset", preset, source)
	configuration.Resolution = recorder.entries
	return configuration
}

// budgetSourceLabel 返回预算字段在某 Profile 下的来源标签。
func budgetSourceLabel(profile string) string {
	if profile == "" || profile == ProfileDefault {
		return "default"
	}
	return "profile:" + profile
}

// ProviderConfig 配置兼容 OpenAI API 的模型提供方。
type ProviderConfig struct {
	BaseURL        string `yaml:"base_url"`
	Model          string `yaml:"model"`
	APIKeyEnv      string `yaml:"api_key_env"`
	RequestTimeout string `yaml:"request_timeout"`
}

// WorkspaceConfig 限制模型工具可读取和检索的工作区范围。
type WorkspaceConfig struct {
	Ignore         []string `yaml:"ignore"`
	MaxReadBytes   int64    `yaml:"max_read_bytes"`
	MaxSearchFiles int      `yaml:"max_search_files"`
	MaxSearchBytes int64    `yaml:"max_search_bytes"`
}

// BudgetConfig 定义运行在轮次、工具、token、时间和成本上的硬上限。
type BudgetConfig struct {
	MaxTurns        int     `yaml:"max_turns"`
	MaxToolCalls    int     `yaml:"max_tool_calls"`
	MaxInputTokens  int     `yaml:"max_input_tokens"`
	MaxOutputTokens int     `yaml:"max_output_tokens"`
	MaxWallTime     string  `yaml:"max_wall_time"`
	MaxCostUSD      float64 `yaml:"max_cost_usd"`
}

// ContextConfig 限制单次模型请求的可见历史，并控制自动压缩生成的检查点体积。
type ContextConfig struct {
	AutoCompact     bool `yaml:"auto_compact"`
	MaxPromptTokens int  `yaml:"max_prompt_tokens"`
	MaxSummaryBytes int  `yaml:"max_summary_bytes"`
}

// PricingConfig 必须显式启用；只有输入与输出 token 价格都给出时，成本预算
// 才合法，Cairn 不会静默采用虚构的成本估计。
type PricingConfig struct {
	InputTokenUSD  float64 `yaml:"input_token_usd"`
	OutputTokenUSD float64 `yaml:"output_token_usd"`
}

// OutputConfig 限制事件预览和证据制品的体积。
type OutputConfig struct {
	MaxPreviewBytes  int `yaml:"max_preview_bytes"`
	MaxArtifactBytes int `yaml:"max_artifact_bytes"`
}

// CancellationConfig 配置终止子进程前的优雅等待时间。
type CancellationConfig struct {
	TerminateGrace string `yaml:"terminate_grace"`
}

// SandboxConfig 决定 exec_command 使用宿主机还是受限 Docker 容器；Docker
// 后端必须显式指定已存在的镜像，不能隐式回退到宿主机执行。
type SandboxConfig struct {
	Backend     string `yaml:"backend"`
	DockerImage string `yaml:"docker_image"`
	MaxPIDs     int    `yaml:"max_pids"`
}

// PolicyConfig 决定工作区写入、进程执行和环境变量暴露的默认边界。
type PolicyConfig struct {
	Write          string   `yaml:"write"`
	Exec           string   `yaml:"exec"`
	ProtectedPaths []string `yaml:"protected_paths"`
	EnvAllow       []string `yaml:"env_allowlist"`
}

// VerificationConfig 描述完成前必须执行的固定验收命令。
type VerificationConfig struct {
	VolatilePaths  []string              `yaml:"volatile_paths"`
	Commands       []VerificationCommand `yaml:"commands"`
	AutoAfterPatch bool                  `yaml:"auto_after_patch"`
}

// VerificationCommand 是独立于模型提议的受控验证命令。
type VerificationCommand struct {
	Argv           []string          `yaml:"argv"`
	CWD            string            `yaml:"cwd"`
	Timeout        string            `yaml:"timeout"`
	MaxOutputBytes int               `yaml:"max_output_bytes"`
	Env            map[string]string `yaml:"env"`
}

// Overrides 保存由环境变量或 CLI 提供的可控运行时覆盖项。
type Overrides struct {
	BaseURL         string
	Model           string
	APIKeyEnv       string
	MaxTurns        int
	MaxToolCalls    int
	MaxInputTokens  int
	MaxOutputTokens int
	MaxWallTime     string
	MaxCostUSD      float64
}

// Defaults 返回安全优先的 V0 默认配置，写入和进程执行都要求审批。
func Defaults() Config {
	return Config{
		Version:   1,
		Profile:   ProfileDefault,
		Preset:    PresetCoding,
		Provider:  ProviderConfig{APIKeyEnv: "AGENT_API_KEY", RequestTimeout: "120s"},
		Workspace: WorkspaceConfig{Ignore: []string{".git", ".agent", ".cairn", "node_modules", "vendor"}, MaxReadBytes: 1024 * 1024, MaxSearchFiles: 10_000, MaxSearchBytes: 64 * 1024 * 1024},
		Policy:    PolicyConfig{Write: "ask", Exec: "ask", ProtectedPaths: []string{".git", ".agent", ".cairn", ".env", ".env.*", "**/*credential*"}, EnvAllow: []string{"PATH", "LANG", "TERM", "TMPDIR", "GOFLAGS"}},
		Budget: BudgetConfig{
			MaxTurns: DefaultMaxTurns, MaxToolCalls: 100, MaxInputTokens: 500_000,
			MaxOutputTokens: 100_000, MaxWallTime: "30m",
		},
		Context:      ContextConfig{AutoCompact: true, MaxPromptTokens: 50_000, MaxSummaryBytes: 16 * 1024},
		Verification: VerificationConfig{VolatilePaths: []string{".cache", "tmp/test-results"}},
		Output:       OutputConfig{MaxPreviewBytes: 16 * 1024, MaxArtifactBytes: 10 * 1024 * 1024},
		Cancellation: CancellationConfig{TerminateGrace: "5s"},
		Sandbox:      SandboxConfig{Backend: "host", MaxPIDs: 64},
	}
}

// Load 按固定优先级合并配置：安全默认值 → 命名 Profile 预设 → YAML 显式值 →
// 环境覆盖 → CLI 显式值，并校验最终配置。解析过程为每个关键字段记录最终值与
// 来源，生成非敏感的 Resolved Execution Plan；恢复比较使用解析后的配置指纹，
// Profile 或字段漂移都会被既有指纹校验拒绝。
func Load(path string, overrides Overrides) (Config, error) {
	config := Defaults()
	config.Profile = ProfileDefault
	recorder := &resolutionRecorder{}
	presetSource := "default"
	recorder.record("profile", ProfileDefault, "default")
	recorder.record("provider.base_url", config.Provider.BaseURL, "default")
	recorder.record("provider.model", config.Provider.Model, "default")
	recorder.record("provider.api_key_env", config.Provider.APIKeyEnv, "default")
	recorder.record("budget.max_turns", strconv.Itoa(config.Budget.MaxTurns), "default")
	recorder.record("budget.max_tool_calls", strconv.Itoa(config.Budget.MaxToolCalls), "default")
	recorder.record("budget.max_input_tokens", strconv.Itoa(config.Budget.MaxInputTokens), "default")
	recorder.record("budget.max_output_tokens", strconv.Itoa(config.Budget.MaxOutputTokens), "default")
	recorder.record("budget.max_wall_time", config.Budget.MaxWallTime, "default")
	recorder.record("budget.max_cost_usd", strconv.FormatFloat(config.Budget.MaxCostUSD, 'f', -1, 64), "default")
	if path != "" {
		content, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return Config{}, fmt.Errorf("read config: %w", err)
			}
		} else {
			decoder := yaml.NewDecoder(strings.NewReader(string(content)))
			decoder.KnownFields(true)
			if err := decoder.Decode(&config); err != nil {
				return Config{}, fmt.Errorf("parse config: %w", err)
			}
			// 第二轮宽松解码只探测 Profile 名称与关键字段的显式设置：命名 Profile
			// 的预算预设仅填充 YAML 缺省字段，显式值按优先级保留并标记来源。
			var probe profileProbe
			relaxed := yaml.NewDecoder(strings.NewReader(string(content)))
			if err := relaxed.Decode(&probe); err != nil {
				return Config{}, fmt.Errorf("parse config profile overlay: %w", err)
			}
			if probe.Preset != nil {
				presetSource = "yaml"
			}
			profile := probe.Profile
			if profile == "" {
				profile = ProfileDefault
			}
			preset, err := profileBudget(profile)
			if err != nil {
				return Config{}, err
			}
			config.Profile = profile
			if probe.Profile != "" {
				recorder.record("profile", profile, "yaml")
			}
			if probe.Provider.BaseURL != nil {
				recorder.record("provider.base_url", config.Provider.BaseURL, "yaml")
			}
			if probe.Provider.Model != nil {
				recorder.record("provider.model", config.Provider.Model, "yaml")
			}
			if probe.Provider.APIKeyEnv != nil {
				recorder.record("provider.api_key_env", config.Provider.APIKeyEnv, "yaml")
			}
			applyProfilePreset(&config, &probe.Budget, preset, profile, recorder)
		}
	}
	if err := normalizePreset(&config, recorder, presetSource); err != nil {
		return Config{}, err
	}
	if err := applyEnvironment(&config, recorder); err != nil {
		return Config{}, err
	}
	applyOverrides(&config, overrides, recorder)
	if err := Validate(config); err != nil {
		return Config{}, err
	}
	config.Resolution = recorder.entries
	return config, nil
}

// applyProfilePreset 将命名 Profile 的预算预设填充到 YAML 缺省的预算字段；
// 显式设置的字段保留 YAML 值并把来源标记为 yaml。
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
func Validate(config Config) error {
	if config.Version != 1 {
		return fmt.Errorf("unsupported config version %d", config.Version)
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
	}
	if config.Provider.APIKeyEnv == "" {
		return fmt.Errorf("provider.api_key_env is required")
	}
	if config.Provider.BaseURL != "" {
		if err := validateProviderBaseURL(config.Provider.BaseURL); err != nil {
			return err
		}
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
	if config.Context.MaxPromptTokens < 1 || config.Context.MaxSummaryBytes < 1 {
		return fmt.Errorf("context limits must be positive")
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

// RequestTimeout 解析模型请求超时配置。
func (config Config) RequestTimeout() (time.Duration, error) {
	return time.ParseDuration(config.Provider.RequestTimeout)
}

// WallTime 解析并确保运行总时长预算为正数。
func (config Config) WallTime() (time.Duration, error) {
	duration, err := time.ParseDuration(config.Budget.MaxWallTime)
	if err != nil {
		return 0, err
	}
	if duration <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return duration, nil
}

// TerminateGrace 解析并确保进程终止宽限期为正数。
func (config Config) TerminateGrace() (time.Duration, error) {
	duration, err := time.ParseDuration(config.Cancellation.TerminateGrace)
	if err != nil {
		return 0, err
	}
	if duration <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return duration, nil
}

// fingerprintView 显式列出参与冻结指纹的字段：只覆盖影响执行语义、审批授权
// 与预算终止的配置。展示与运维类字段（request_timeout、terminate_grace、output
// 限额）被刻意排除，使后续版本新增非语义字段或调整展示参数不会令在途 run 的
// 指纹漂移；新增执行语义字段时必须同步加入此结构并补充指纹变化测试。
type fingerprintView struct {
	Version      int                 `json:"version"`
	Profile      string              `json:"profile"`
	Preset       string              `json:"preset"`
	Provider     providerFingerprint `json:"provider"`
	Workspace    WorkspaceConfig     `json:"workspace"`
	Policy       PolicyConfig        `json:"policy"`
	Verification VerificationConfig  `json:"verification"`
	Budget       BudgetConfig        `json:"budget"`
	Context      ContextConfig       `json:"context"`
	Pricing      PricingConfig       `json:"pricing"`
	Sandbox      SandboxConfig       `json:"sandbox"`
}

// providerFingerprint 只保留决定请求去向与身份的提供方字段；request_timeout 属
// 运维调参，不影响授权与回放语义，不参与指纹。
type providerFingerprint struct {
	BaseURL   string `json:"base_url"`
	Model     string `json:"model"`
	APIKeyEnv string `json:"api_key_env"`
}

// Fingerprint 生成可写入事件日志的冻结非敏感配置摘要；带版本前缀，指纹算法
// 本身演进时递增前缀使新旧摘要天然不可比。
func (config Config) Fingerprint() string {
	profile := config.Profile
	if profile == "" {
		profile = ProfileDefault
	}
	view := fingerprintView{
		Version:      config.Version,
		Profile:      profile,
		Preset:       config.Preset,
		Provider:     providerFingerprint{BaseURL: config.Provider.BaseURL, Model: config.Provider.Model, APIKeyEnv: config.Provider.APIKeyEnv},
		Workspace:    config.Workspace,
		Policy:       config.Policy,
		Verification: config.Verification,
		Budget:       config.Budget,
		Context:      config.Context,
		Pricing:      config.Pricing,
		Sandbox:      config.Sandbox,
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(append([]byte("cairn-config-fingerprint-v3\x00"), encoded...))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// DefaultPath 返回工作区内的默认配置文件路径。
func DefaultPath(workspace string) string { return filepath.Join(workspace, ".agent", "config.yaml") }

// applyEnvironment 将允许的 AGENT_* 环境变量覆写到配置中并记录来源；非法数值
// 视为配置错误而非静默忽略，与同名 CLI flag 的行为保持一致。
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
