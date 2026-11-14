package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMinimalPresetLoadsWithoutExpandingPermissions 锁定显式最小组合的解析、
// 来源与安全默认值；预设变化必须进入冻结指纹。
func TestMinimalPresetLoadsWithoutExpandingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 2\npreset: minimal\nprovider:\n"+validProviderYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	entry, found := resolutionEntry(configuration.Resolution, "preset")
	if configuration.Preset != PresetMinimal || !found || entry.Value != PresetMinimal || entry.Source != "yaml" {
		t.Fatalf("preset=%q resolution=%+v found=%t", configuration.Preset, entry, found)
	}
	if configuration.Policy.Write != "ask" || configuration.Policy.Exec != "ask" {
		t.Fatalf("minimal expanded permissions: %+v", configuration.Policy)
	}
	coding := configuration
	coding.Preset = PresetCoding
	if coding.Fingerprint() == configuration.Fingerprint() {
		t.Fatal("minimal tool surface must change the execution fingerprint")
	}
	if err := ApplyPreset(&coding, PresetMinimal); err != nil {
		t.Fatal(err)
	}
	if coding.Fingerprint() != configuration.Fingerprint() {
		t.Fatal("loaded and programmatically applied minimal presets diverged")
	}
}

// TestMinimalPresetRejectsAdvancedCapabilities 拒绝矛盾能力声明，避免只隐藏
// 工具名称却仍构造对应资源或执行后台任务。
func TestMinimalPresetRejectsAdvancedCapabilities(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"skills", func(c *Config) { c.Skills = &SkillsConfig{Enabled: true} }},
		{"mcp", func(c *Config) { c.MCP = &MCPConfig{Servers: []MCPServerConfig{{Name: "docs"}}} }},
		{"web", func(c *Config) { c.Tools.Web = true }},
		{"session", func(c *Config) { c.Tools.ExecSession = true }},
		{"delegate", func(c *Config) { c.Tools.DelegateAsync = true }},
		{"browser", func(c *Config) { c.Tools.Browser = true }},
		{"run_tests", func(c *Config) { c.Tools.RunTests = true }},
		{"lsp", func(c *Config) { c.Tools.LspDiagnostics = true }},
		{"hooks", func(c *Config) { c.Hooks = []HookConfig{{ID: "rule", Enabled: true}} }},
		{"verification", func(c *Config) { c.Verification.Commands = []VerificationCommand{{Argv: []string{"true"}}} }},
		{"auto_verify", func(c *Config) { c.Verification.AutoAfterPatch = true }},
		{"telemetry", func(c *Config) { c.Telemetry.Enabled = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := validDefaults()
			configuration.Preset = PresetMinimal
			test.mutate(&configuration)
			if err := Validate(configuration); err == nil || !strings.Contains(err.Error(), "minimal preset forbids") {
				t.Fatalf("conflicting capability accepted: %v", err)
			}
		})
	}
	configuration := validDefaults()
	configuration.Preset = PresetMinimal
	configuration.Skills = &SkillsConfig{}
	configuration.MCP = &MCPConfig{Servers: []MCPServerConfig{{Name: "docs", Disabled: true, Transport: MCPTransportStdio, Command: "unused"}}}
	if err := Validate(configuration); err != nil {
		t.Fatalf("disabled declarations must remain valid: %v", err)
	}
}

// TestMinimalPresetPreservesLegacyFingerprints 对照修改前冻结的六组指纹，
// 确保新增预设没有改变旧预设或既有 opt-in 工具配置的恢复身份。
func TestMinimalPresetPreservesLegacyFingerprints(t *testing.T) {
	for _, test := range []struct {
		preset string
		web    bool
		want   string
	}{
		{PresetCoding, false, "sha256:ab4e699d0d51dd3cb30d59d62966a903cf023094e4500037d465f6a43f93e0be"},
		{PresetCoding, true, "sha256:485b11eab8a66a03e65bfc871aa295c3c447a54b873d55a6476f7b2f4d02bec5"},
		{PresetReadOnly, false, "sha256:6812fbb4942a54d64aec124a2bfdf7eaa51af78a14b403d2043ed797b5c0d307"},
		{PresetReadOnly, true, "sha256:8655c65ad20068d27e0ffbbbcd07c7e21be9ac403e1ec0a50ed49234bf5f2375"},
		{PresetBenchmark, false, "sha256:4c87b70f373f12bf02f371d6769d148cdbca9618ed274945fece42929c88a13c"},
		{PresetBenchmark, true, "sha256:e826c204a0e71144837a41a7baa5c43c7bb54e0474814f04791ecb0096304be0"},
	} {
		configuration := validDefaults()
		configuration.Preset, configuration.Tools.Web = test.preset, test.web
		if got := configuration.Fingerprint(); got != test.want {
			t.Fatalf("preset=%s web=%t fingerprint=%s want=%s", test.preset, test.web, got, test.want)
		}
	}
}
