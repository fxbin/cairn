// Package config 读取 Cairn 小型、不可包含密钥的仓库配置。
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

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

// Load 合并默认值、严格 YAML、环境变量与显式覆盖项，并校验最终配置。
func Load(path string, overrides Overrides) (Config, error) {
	config := Defaults()
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
		}
	}
	applyEnvironment(&config)
	applyOverrides(&config, overrides)
	if err := Validate(config); err != nil {
		return Config{}, err
	}
	return config, nil
}

// Validate 验证配置版本、限额、时长、策略与验证命令的安全约束。
func Validate(config Config) error {
	if config.Version != 1 {
		return fmt.Errorf("unsupported config version %d", config.Version)
	}
	if config.Provider.APIKeyEnv == "" {
		return fmt.Errorf("provider.api_key_env is required")
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
		if sensitiveEnvironmentKey(key) {
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

// sensitiveEnvironmentKey 判断环境变量名是否暗示包含凭据。
func sensitiveEnvironmentKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "api_key") || strings.Contains(key, "credential")
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

// Fingerprint 生成可写入事件日志的冻结非敏感配置摘要。
func (config Config) Fingerprint() string {
	encoded, err := json.Marshal(config)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// DefaultPath 返回工作区内的默认配置文件路径。
func DefaultPath(workspace string) string { return filepath.Join(workspace, ".agent", "config.yaml") }

// applyEnvironment 将允许的 AGENT_* 环境变量覆写到配置中。
func applyEnvironment(config *Config) {
	if value := os.Getenv("AGENT_BASE_URL"); value != "" {
		config.Provider.BaseURL = value
	}
	if value := os.Getenv("AGENT_MODEL"); value != "" {
		config.Provider.Model = value
	}
	if value := os.Getenv("AGENT_API_KEY_ENV"); value != "" {
		config.Provider.APIKeyEnv = value
	}
	if value := os.Getenv("AGENT_MAX_TURNS"); value != "" {
		if turns, err := strconv.Atoi(value); err == nil {
			config.Budget.MaxTurns = turns
		}
	}
	if value := os.Getenv("AGENT_MAX_TOOL_CALLS"); value != "" {
		if limit, err := strconv.Atoi(value); err == nil {
			config.Budget.MaxToolCalls = limit
		}
	}
	if value := os.Getenv("AGENT_MAX_INPUT_TOKENS"); value != "" {
		if limit, err := strconv.Atoi(value); err == nil {
			config.Budget.MaxInputTokens = limit
		}
	}
	if value := os.Getenv("AGENT_MAX_OUTPUT_TOKENS"); value != "" {
		if limit, err := strconv.Atoi(value); err == nil {
			config.Budget.MaxOutputTokens = limit
		}
	}
	if value := os.Getenv("AGENT_MAX_WALL_TIME"); value != "" {
		config.Budget.MaxWallTime = value
	}
	if value := os.Getenv("AGENT_MAX_COST_USD"); value != "" {
		if limit, err := strconv.ParseFloat(value, 64); err == nil {
			config.Budget.MaxCostUSD = limit
		}
	}
}

// applyOverrides 应用已经由 CLI 解析完成的显式非零覆盖项。
func applyOverrides(config *Config, overrides Overrides) {
	if overrides.BaseURL != "" {
		config.Provider.BaseURL = overrides.BaseURL
	}
	if overrides.Model != "" {
		config.Provider.Model = overrides.Model
	}
	if overrides.APIKeyEnv != "" {
		config.Provider.APIKeyEnv = overrides.APIKeyEnv
	}
	if overrides.MaxTurns > 0 {
		config.Budget.MaxTurns = overrides.MaxTurns
	}
	if overrides.MaxToolCalls > 0 {
		config.Budget.MaxToolCalls = overrides.MaxToolCalls
	}
	if overrides.MaxInputTokens > 0 {
		config.Budget.MaxInputTokens = overrides.MaxInputTokens
	}
	if overrides.MaxOutputTokens > 0 {
		config.Budget.MaxOutputTokens = overrides.MaxOutputTokens
	}
	if overrides.MaxWallTime != "" {
		config.Budget.MaxWallTime = overrides.MaxWallTime
	}
	if overrides.MaxCostUSD > 0 {
		config.Budget.MaxCostUSD = overrides.MaxCostUSD
	}
}
