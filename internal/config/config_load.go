// YAML 配置装载。
// Author: fxbin
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/contract"
	yaml "gopkg.in/yaml.v3"
)

func Load(path string, overrides Overrides) (Config, error) {
	config := Defaults()
	config.Profile = ProfileDefault
	recorder := &resolutionRecorder{}
	presetSource := "default"
	var providerOverlayFact providerOverlay
	recorder.record("profile", ProfileDefault, "default")
	recorder.record("provider.base_url", config.Provider.BaseURL, "default")
	recorder.record("provider.model", config.Provider.Model, "default")
	recorder.record("provider.api_key_env", config.Provider.APIKeyEnv, "default")
	recorder.record("provider.id", contract.ProviderIDCustom, "default")
	recorder.record("provider.admission", ProviderAdmissionRequired, "default")
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
			var probe profileProbe
			relaxed := yaml.NewDecoder(strings.NewReader(string(content)))
			if err := relaxed.Decode(&probe); err != nil {
				return Config{}, fmt.Errorf("parse config overlay: %w", err)
			}
			if probe.Version == nil {
				return Config{}, fmt.Errorf("config version is required and must be %d", CurrentVersion)
			}
			if *probe.Version != CurrentVersion {
				return Config{}, fmt.Errorf("unsupported config version %d", *probe.Version)
			}
			var modeMeta struct {
				ActiveMode string               `yaml:"active_mode"`
				Modes      map[string]yaml.Node `yaml:"modes"`
			}
			metaDecoder := yaml.NewDecoder(strings.NewReader(string(content)))
			if err := metaDecoder.Decode(&modeMeta); err != nil {
				return Config{}, fmt.Errorf("parse config modes: %w", err)
			}
			modeName := strings.TrimSpace(overrides.Mode)
			if modeName == "" {
				modeName = strings.TrimSpace(modeMeta.ActiveMode)
			}
			if modeName != "" {
				node, ok := modeMeta.Modes[modeName]
				if !ok {
					return Config{}, fmt.Errorf("unknown active mode %q", modeName)
				}
				if err := node.Decode(&config); err != nil {
					return Config{}, fmt.Errorf("apply mode %q: %w", modeName, err)
				}
				config.ActiveMode = modeName
				recorder.record("active_mode", modeName, "mode")
			}
			decoder := yaml.NewDecoder(strings.NewReader(string(content)))
			decoder.KnownFields(true)
			if err := decoder.Decode(&config); err != nil {
				return Config{}, fmt.Errorf("parse config: %w", err)
			}
			if strings.TrimSpace(overrides.Mode) != "" {
				config.ActiveMode = strings.TrimSpace(overrides.Mode)
				recorder.record("active_mode", config.ActiveMode, "cli")
			}
			providerOverlayFact = probe.Provider
			// 命名 Profile 的预算预设仅填充 YAML 缺省字段，显式值按优先级
			// 保留并标记来源。
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
			if probe.Provider.ID != nil {
				recorder.record("provider.id", config.Provider.ID, "yaml")
			}
			if probe.Provider.Admission != nil {
				recorder.record("provider.admission", config.Provider.Admission, "yaml")
			}
			applyProfilePreset(&config, &probe.Budget, preset, profile, recorder)
		}
	}
	recordProviderGenerationResolution(config.Provider.Generation, recorder)
	if err := resolveProviderConnection(&config, providerOverlayFact, recorder); err != nil {
		return Config{}, err
	}
	if err := normalizePreset(&config, recorder, presetSource); err != nil {
		return Config{}, err
	}
	if err := applyEnvironment(&config, recorder); err != nil {
		return Config{}, err
	}
	applyOverrides(&config, overrides, recorder)
	if err := ApplyPermissionMode(&config.Policy); err != nil {
		return Config{}, err
	}
	if err := resolvePromptFile(&config); err != nil {
		return Config{}, err
	}
	// 语义观测抽样比例未声明（0）时按默认值归一，使配置视图与生效行为一致。
	config.Telemetry.SamplePercent = config.Telemetry.EffectiveSamplePercent()
	if err := Validate(config); err != nil {
		return Config{}, err
	}
	config.Resolution = recorder.entries
	return config, nil
}

// applyProfilePreset 将命名 Profile 的预算预设填充到 YAML 缺省的预算字段；
// 显式设置的字段保留 YAML 值并把来源标记为 yaml。

// resolvePromptFile 展开 system_prompt_file 为 Resolved；内联 SystemPrompt 优先。
func resolvePromptFile(config *Config) error {
	config.Prompt.Resolved = strings.TrimSpace(config.Prompt.SystemPrompt)
	if config.Prompt.Resolved != "" {
		return nil
	}
	path := strings.TrimSpace(config.Prompt.SystemPromptFile)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read system_prompt_file: %w", err)
	}
	if !utf8.Valid(raw) {
		return errors.New("system_prompt_file must be UTF-8 text")
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return errors.New("system_prompt_file is empty")
	}
	config.Prompt.Resolved = text
	return nil
}
