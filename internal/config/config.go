// Package config loads Cairn's small, non-secret repository configuration.
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

type Config struct {
	Version      int                `yaml:"version"`
	Provider     ProviderConfig     `yaml:"provider"`
	Workspace    WorkspaceConfig    `yaml:"workspace"`
	Policy       PolicyConfig       `yaml:"policy"`
	Verification VerificationConfig `yaml:"verification"`
	Budget       BudgetConfig       `yaml:"budget"`
	Pricing      PricingConfig      `yaml:"pricing"`
	Output       OutputConfig       `yaml:"output"`
	Cancellation CancellationConfig `yaml:"cancellation"`
}

type ProviderConfig struct {
	BaseURL        string `yaml:"base_url"`
	Model          string `yaml:"model"`
	APIKeyEnv      string `yaml:"api_key_env"`
	RequestTimeout string `yaml:"request_timeout"`
}

type WorkspaceConfig struct {
	Ignore         []string `yaml:"ignore"`
	MaxReadBytes   int64    `yaml:"max_read_bytes"`
	MaxSearchFiles int      `yaml:"max_search_files"`
	MaxSearchBytes int64    `yaml:"max_search_bytes"`
}

type BudgetConfig struct {
	MaxTurns        int     `yaml:"max_turns"`
	MaxToolCalls    int     `yaml:"max_tool_calls"`
	MaxInputTokens  int     `yaml:"max_input_tokens"`
	MaxOutputTokens int     `yaml:"max_output_tokens"`
	MaxWallTime     string  `yaml:"max_wall_time"`
	MaxCostUSD      float64 `yaml:"max_cost_usd"`
}

// PricingConfig is intentionally opt-in. A cost budget is invalid until both
// reported-token prices are explicit, so Cairn never silently enforces a
// fictional cost estimate.
type PricingConfig struct {
	InputTokenUSD  float64 `yaml:"input_token_usd"`
	OutputTokenUSD float64 `yaml:"output_token_usd"`
}

type OutputConfig struct {
	MaxPreviewBytes  int `yaml:"max_preview_bytes"`
	MaxArtifactBytes int `yaml:"max_artifact_bytes"`
}

type CancellationConfig struct {
	TerminateGrace string `yaml:"terminate_grace"`
}

type PolicyConfig struct {
	Write          string   `yaml:"write"`
	Exec           string   `yaml:"exec"`
	ProtectedPaths []string `yaml:"protected_paths"`
	EnvAllow       []string `yaml:"env_allowlist"`
}

type VerificationConfig struct {
	VolatilePaths []string              `yaml:"volatile_paths"`
	Commands      []VerificationCommand `yaml:"commands"`
}

type VerificationCommand struct {
	Argv           []string          `yaml:"argv"`
	CWD            string            `yaml:"cwd"`
	Timeout        string            `yaml:"timeout"`
	MaxOutputBytes int               `yaml:"max_output_bytes"`
	Env            map[string]string `yaml:"env"`
}

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

func Defaults() Config {
	return Config{
		Version:   1,
		Provider:  ProviderConfig{APIKeyEnv: "AGENT_API_KEY", RequestTimeout: "120s"},
		Workspace: WorkspaceConfig{Ignore: []string{".git", ".agent", ".cairn", "node_modules", "vendor"}, MaxReadBytes: 1024 * 1024, MaxSearchFiles: 10_000, MaxSearchBytes: 64 * 1024 * 1024},
		Policy:    PolicyConfig{Write: "ask", Exec: "ask", ProtectedPaths: []string{".git", ".agent", ".cairn", ".env", ".env.*", "**/*credential*"}, EnvAllow: []string{"PATH", "LANG", "TERM", "TMPDIR", "GOFLAGS"}},
		Budget: BudgetConfig{
			MaxTurns: 30, MaxToolCalls: 100, MaxInputTokens: 500_000,
			MaxOutputTokens: 100_000, MaxWallTime: "30m",
		},
		Verification: VerificationConfig{VolatilePaths: []string{".cache", "tmp/test-results"}},
		Output:       OutputConfig{MaxPreviewBytes: 16 * 1024, MaxArtifactBytes: 10 * 1024 * 1024},
		Cancellation: CancellationConfig{TerminateGrace: "5s"},
	}
}

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

func validPolicyEffect(value string) bool {
	return value == "allow" || value == "deny" || value == "ask"
}

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

func sensitiveEnvironmentKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "password") || strings.Contains(key, "api_key") || strings.Contains(key, "credential")
}

func (config Config) RequestTimeout() (time.Duration, error) {
	return time.ParseDuration(config.Provider.RequestTimeout)
}

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

// Fingerprint identifies the frozen, non-secret configuration for a run.
func (config Config) Fingerprint() string {
	encoded, err := json.Marshal(config)
	if err != nil {
		panic(err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:])
}

func DefaultPath(workspace string) string { return filepath.Join(workspace, ".agent", "config.yaml") }

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
