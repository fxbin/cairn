package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestMCPConfigurationDefaultsAndFingerprint 锁定显式配置的安全默认值与权限身份。
func TestMCPConfigurationDefaultsAndFingerprint(t *testing.T) {
	configuration := validDefaults()
	configuration.MCP = &MCPConfig{Servers: []MCPServerConfig{{Name: "docs", Transport: MCPTransportStdio, Command: "server", Env: map[string]string{"AUTH": "DOCS_AUTH"}}}}
	if err := Validate(configuration); err != nil {
		t.Fatal(err)
	}
	effective := configuration.MCP.Effective()
	server := effective.Servers[0]
	if server.Exposure != MCPExposureDeferred || server.ConnectPolicy != "ask" || server.CallPolicy != "ask" || server.CWD != "." || effective.MaxTools != DefaultMCPMaxTools {
		t.Fatalf("unsafe defaults: %#v", effective)
	}
	before := configuration.Fingerprint()
	configuration.MCP = &effective
	if configuration.Fingerprint() != before {
		t.Fatal("explicit defaults changed the fingerprint")
	}
	effective.Servers[0].Env["AUTH"] = "OTHER_AUTH"
	if configuration.Fingerprint() == before {
		t.Fatal("credential source changed without invalidating execution identity")
	}

}

// TestMCPConfigurationEffectiveCopiesMutableFields 避免运行目录被配置调用方事后改写。
func TestMCPConfigurationEffectiveCopiesMutableFields(t *testing.T) {
	original := MCPConfig{Servers: []MCPServerConfig{{Name: "docs", Args: []string{"first"}, Env: map[string]string{"KEY": "SOURCE"}, ToolExposure: map[string]string{"lookup": "direct"}, OAuth: &MCPOAuthConfig{Scopes: []string{"read"}}}}}
	copy := original.Effective()
	original.Servers[0].Args[0] = "changed"
	original.Servers[0].Env["KEY"] = "OTHER"
	original.Servers[0].ToolExposure["lookup"] = "hidden"
	original.Servers[0].OAuth.Scopes[0] = "write"
	if copy.Servers[0].Args[0] != "first" || copy.Servers[0].Env["KEY"] != "SOURCE" || copy.Servers[0].ToolExposure["lookup"] != "direct" || copy.Servers[0].OAuth.Scopes[0] != "read" {
		t.Fatal("effective configuration retained mutable aliases")
	}
}

// TestMCPConfigurationFailsClosed 约束传输身份、隔离 preset 和认证组合。
func TestMCPConfigurationFailsClosed(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"read-only", func(c *Config) { c.Preset = PresetReadOnly }},
		{"benchmark", func(c *Config) { c.Preset = PresetBenchmark }},
		{"namespace collision", func(c *Config) {
			c.MCP.Servers = append(c.MCP.Servers, MCPServerConfig{Name: "docs_api", Transport: MCPTransportHTTP, URL: "https://docs.test/mcp"})
			c.MCP.Servers[0].Name = "docs-api"
		}},
		{"userinfo", func(c *Config) { c.MCP.Servers[0].URL = "https://user:password@docs.test/mcp" }},
		{"query credential", func(c *Config) { c.MCP.Servers[0].URL = "https://docs.test/mcp?token=hidden" }},
		{"cleartext remote", func(c *Config) { c.MCP.Servers[0].URL = "http://docs.test/mcp" }},
		{"mixed transport", func(c *Config) { c.MCP.Servers[0].Command = "server" }},
		{"mixed authentication", func(c *Config) { c.MCP.Servers[0].BearerTokenEnv = "KEY"; c.MCP.Servers[0].OAuth = &MCPOAuthConfig{} }},
		{"inline environment", func(c *Config) {
			c.MCP.Servers[0].Transport = MCPTransportStdio
			c.MCP.Servers[0].URL = ""
			c.MCP.Servers[0].Command = "server"
			c.MCP.Servers[0].Env = map[string]string{"KEY": "literal value"}
		}},
		{"invalid policy", func(c *Config) { c.MCP.Servers[0].CallPolicy = "auto" }},
		{"invalid timeout", func(c *Config) { c.MCP.Servers[0].ToolTimeout = "0s" }},
		{"DCR explicit issuer", func(c *Config) { c.MCP.Servers[0].OAuth = &MCPOAuthConfig{Issuer: "https://issuer.test"} }},
		{"invalid exposure", func(c *Config) { c.MCP.Servers[0].Exposure = "unknown" }},
		{"invalid glob", func(c *Config) { c.MCP.Servers[0].DenyTools = []string{"["} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := validDefaults()
			c.MCP = &MCPConfig{Servers: []MCPServerConfig{{Name: "docs", Transport: MCPTransportHTTP, URL: "https://docs.test/mcp"}}}
			test.mutate(&c)
			if validateMCP(c.MCP, c.Preset) == nil {
				t.Fatal("unsafe MCP configuration accepted")
			}
		})
	}
	for _, endpoint := range []string{"https://docs.test/mcp", "http://127.0.0.1:9000/mcp", "http://[::1]:9000/mcp"} {
		if err := ValidateMCPURL(endpoint); err != nil {
			t.Fatal(err)
		}
	}
}

// TestMCPLoadRejectsUnknownFieldsAndKeepsAbsentContract 保留严格 YAML 和未配置 MCP 的冻结合同。
func TestMCPLoadRejectsUnknownFieldsAndKeepsAbsentContract(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "config.yaml")
	base := "version: 2\nprovider:\n" + validProviderYAML
	if err := os.WriteFile(filename, []byte(base+"mcp:\n  servers:\n    - name: docs\n      transport: streamable-http\n      url: https://docs.test/mcp\n      secret: forbidden\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filename, Overrides{}); err == nil {
		t.Fatal("unknown MCP field accepted")
	}
	c := validDefaults()
	encoded, err := json.Marshal(fingerprintView{MCP: mcpFingerprint(c.MCP)})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"mcp"`) {
		t.Fatal("absent MCP changed the preexisting fingerprint document")
	}
}
