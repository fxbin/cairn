// provider_presets_test 覆盖已命名连接预设的配置解析与激活：use 命中预设、
// 与内联字段互斥、未知协议失败关闭。
//
// Author: fxbin
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writePresetsConfig 写入含预设的配置文件。
func writePresetsConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestProviderUseSelectsNamedPreset 锁定 provider.use 投影预设为有效连接，
// 且指纹随预设内容变化。
func TestProviderUseSelectsNamedPreset(t *testing.T) {
	body := `version: 2
provider:
  use: work
  presets:
    work:
      base_url: https://work.example.com/v1
      model: work-model
      api_key_env: WORK_KEY
      capability:
        tool_calls: true
        streaming_usage: true
    personal:
      base_url: https://personal.example.com/v1
      model: personal-model
      api_key_env: PERSONAL_KEY
      capability:
        tool_calls: true
        streaming_usage: true
`
	configuration, err := Load(writePresetsConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Provider.Model != "work-model" || configuration.Provider.BaseURL != "https://work.example.com/v1" || configuration.Provider.APIKeyEnv != "WORK_KEY" {
		t.Fatalf("effective provider = %+v", configuration.Provider)
	}
	if configuration.Provider.Protocol != ProtocolOpenAIChatCompletions {
		t.Fatalf("protocol = %q", configuration.Provider.Protocol)
	}
	other := strings.Replace(body, "use: work", "use: personal", 1)
	second, err := Load(writePresetsConfig(t, other), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if second.Provider.Model != "personal-model" {
		t.Fatalf("switched provider = %+v", second.Provider)
	}
	if configuration.Fingerprint() == second.Fingerprint() {
		t.Fatal("different active presets must produce different fingerprints")
	}
}

// TestProviderUseRejectsInvalidSelection 锁定 use 悬空、与内联字段冲突、
// 未知协议全部失败关闭。
func TestProviderUseRejectsInvalidSelection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "use 悬空",
			body: "version: 2\nprovider:\n  use: missing\n  presets:\n    work:\n      base_url: https://x.test/v1\n      model: m\n      api_key_env: K\n",
			want: "does not match",
		},
		{
			name: "use 与内联冲突",
			body: "version: 2\nprovider:\n  use: work\n  base_url: https://inline.test/v1\n  presets:\n    work:\n      base_url: https://x.test/v1\n      model: m\n      api_key_env: K\n",
			want: "mutually exclusive",
		},
		{
			name: "未知协议",
			body: "version: 2\nprovider:\n  protocol: gemini-native\n  base_url: https://x.test/v1\n  model: m\n  api_key_env: K\n  request_timeout: 30s\n  admission: optional\n",
			want: "unsupported",
		},
		{
			name: "预设未知协议",
			body: "version: 2\nprovider:\n  use: work\n  presets:\n    work:\n      protocol: gemini-native\n      base_url: https://x.test/v1\n      model: m\n      api_key_env: K\n",
			want: "unsupported",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(writePresetsConfig(t, test.body), Overrides{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

// TestLegacyInlineProviderStillLoads 锁定未使用预设时单 provider 内联块零变化。
func TestLegacyInlineProviderStillLoads(t *testing.T) {
	body := "version: 2\nprovider:\n  base_url: https://legacy.test/v1\n  model: legacy-model\n  api_key_env: LEGACY_KEY\n  request_timeout: 45s\n  admission: optional\n  capability:\n    tool_calls: true\n    streaming_usage: true\n"
	configuration, err := Load(writePresetsConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Provider.Model != "legacy-model" || configuration.Provider.Use != "" || configuration.Provider.Protocol != ProtocolOpenAIChatCompletions {
		t.Fatalf("legacy provider = %+v", configuration.Provider)
	}
}

// TestAnthropicMessagesProtocolAccepted 锁定 anthropic-messages 为受支持协议。
func TestAnthropicMessagesProtocolAccepted(t *testing.T) {
	body := "version: 2\nprovider:\n  protocol: anthropic-messages\n  base_url: https://api.anthropic.com\n  model: claude-sonnet-5\n  api_key_env: ANTHROPIC_KEY\n  request_timeout: 60s\n  admission: optional\n  capability:\n    tool_calls: true\n    streaming_usage: true\n    stop_reasons: [end_turn, tool_use, max_tokens]\n"
	configuration, err := Load(writePresetsConfig(t, body), Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Provider.Protocol != ProtocolAnthropicMessages {
		t.Fatalf("protocol = %q", configuration.Provider.Protocol)
	}
}
