// Package config 定义显式 MCP 客户端的配置、默认值与执行身份。
// Author: fxbin
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

// MCP 的预算限制目录、协议响应和发现后进入模型的工具数量。
const (
	MCPTransportStdio            = "stdio"
	MCPTransportHTTP             = "streamable-http"
	MCPExposureDirect            = "direct"
	MCPExposureDeferred          = "deferred"
	MCPExposureHidden            = "hidden"
	DefaultMCPMaxTools           = 256
	DefaultMCPMaxCatalogBytes    = 512 * 1024
	DefaultMCPMaxResponseBytes   = 1024 * 1024
	DefaultMCPMaxSearchResults   = 8
	DefaultMCPStartupTimeout     = "15s"
	DefaultMCPToolTimeout        = "60s"
	MaxMCPServers                = 32
	MaxMCPTools                  = 1024
	MaxMCPSearchResults          = 32
	MaxMCPServerDescriptionBytes = 4096
	MaxMCPServerNameBytes        = 32
)

// MCPConfig 只装配明确声明的服务器；目录发现与外部调用仍需运行期授权。
type MCPConfig struct {
	Servers          []MCPServerConfig `yaml:"servers" json:"servers"`
	MaxTools         int               `yaml:"max_tools" json:"max_tools"`
	MaxCatalogBytes  int               `yaml:"max_catalog_bytes" json:"max_catalog_bytes"`
	MaxResponseBytes int               `yaml:"max_response_bytes" json:"max_response_bytes"`
	MaxSearchResults int               `yaml:"max_search_results" json:"max_search_results"`
}

// MCPServerConfig 保存连接身份与本地权限；环境和 Bearer 只能引用变量名称。
type MCPServerConfig struct {
	Name           string            `yaml:"name" json:"name"`
	Transport      string            `yaml:"transport" json:"transport"`
	Disabled       bool              `yaml:"disabled" json:"disabled"`
	Command        string            `yaml:"command,omitempty" json:"command,omitempty"`
	Args           []string          `yaml:"args,omitempty" json:"args,omitempty"`
	CWD            string            `yaml:"cwd,omitempty" json:"cwd,omitempty"`
	Env            map[string]string `yaml:"env,omitempty" json:"env,omitempty"`
	URL            string            `yaml:"url,omitempty" json:"url,omitempty"`
	BearerTokenEnv string            `yaml:"bearer_token_env,omitempty" json:"bearer_token_env,omitempty"`
	OAuth          *MCPOAuthConfig   `yaml:"oauth,omitempty" json:"oauth,omitempty"`
	Description    string            `yaml:"description,omitempty" json:"description,omitempty"`
	Exposure       string            `yaml:"exposure" json:"exposure"`
	ToolExposure   map[string]string `yaml:"tool_exposure,omitempty" json:"tool_exposure,omitempty"`
	AllowTools     []string          `yaml:"allow_tools,omitempty" json:"allow_tools,omitempty"`
	DenyTools      []string          `yaml:"deny_tools,omitempty" json:"deny_tools,omitempty"`
	ConnectPolicy  string            `yaml:"connect_policy" json:"connect_policy"`
	CallPolicy     string            `yaml:"call_policy" json:"call_policy"`
	StartupTimeout string            `yaml:"startup_timeout" json:"startup_timeout"`
	ToolTimeout    string            `yaml:"tool_timeout" json:"tool_timeout"`
}

// MCPOAuthConfig 声明注册与回调事实；令牌由独立的本机私有存储管理。
type MCPOAuthConfig struct {
	ClientID        string   `yaml:"client_id,omitempty" json:"client_id,omitempty"`
	Issuer          string   `yaml:"issuer,omitempty" json:"issuer,omitempty"`
	ClientSecretEnv string   `yaml:"client_secret_env,omitempty" json:"client_secret_env,omitempty"`
	CallbackPort    int      `yaml:"callback_port,omitempty" json:"callback_port,omitempty"`
	Scopes          []string `yaml:"scopes,omitempty" json:"scopes,omitempty"`
}

// Effective 填充有语义的默认值并复制可变集合，保证构造和指纹使用相同事实。
func (configuration MCPConfig) Effective() MCPConfig {
	if configuration.MaxTools == 0 {
		configuration.MaxTools = DefaultMCPMaxTools
	}
	if configuration.MaxCatalogBytes == 0 {
		configuration.MaxCatalogBytes = DefaultMCPMaxCatalogBytes
	}
	if configuration.MaxResponseBytes == 0 {
		configuration.MaxResponseBytes = DefaultMCPMaxResponseBytes
	}
	if configuration.MaxSearchResults == 0 {
		configuration.MaxSearchResults = DefaultMCPMaxSearchResults
	}
	servers := make([]MCPServerConfig, len(configuration.Servers))
	for i, server := range configuration.Servers {
		if server.Exposure == "" {
			server.Exposure = MCPExposureDeferred
		}
		if server.ConnectPolicy == "" {
			server.ConnectPolicy = "ask"
		}
		if server.CallPolicy == "" {
			server.CallPolicy = "ask"
		}
		if server.StartupTimeout == "" {
			server.StartupTimeout = DefaultMCPStartupTimeout
		}
		if server.ToolTimeout == "" {
			server.ToolTimeout = DefaultMCPToolTimeout
		}
		if server.CWD == "" && server.Transport == MCPTransportStdio {
			server.CWD = "."
		}
		server.Args = append([]string(nil), server.Args...)
		server.AllowTools = append([]string(nil), server.AllowTools...)
		server.DenyTools = append([]string(nil), server.DenyTools...)
		server.Env = cloneMCPStrings(server.Env)
		server.ToolExposure = cloneMCPStrings(server.ToolExposure)
		if server.OAuth != nil {
			oauth := *server.OAuth
			oauth.Scopes = append([]string(nil), oauth.Scopes...)
			server.OAuth = &oauth
		}
		servers[i] = server
	}
	configuration.Servers = servers
	return configuration
}

// cloneMCPStrings 复制配置字典，避免运行期目录持有调用方可变引用。
func cloneMCPStrings(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

// Fingerprint 将已归一化服务器身份、权限和暴露配置绑定到发现和审批快照。
func (server MCPServerConfig) Fingerprint() string {
	value := (MCPConfig{Servers: []MCPServerConfig{server}}).Effective().Servers[0]
	digest, err := canonicaljson.Fingerprint("cairn-mcp-server-v1", value)
	if err != nil {
		panic(err)
	}
	return digest
}

// mcpFingerprint 保持未配置 MCP 的旧指纹逐位不变。
func mcpFingerprint(configuration *MCPConfig) *MCPConfig {
	if configuration == nil {
		return nil
	}
	value := configuration.Effective()
	return &value
}

// validateMCP 在任何外部连接之前校验身份、权限、预算和凭据引用。
func validateMCP(configuration *MCPConfig, preset string) error {
	if configuration == nil {
		return nil
	}
	value := configuration.Effective()
	if len(value.Servers) > MaxMCPServers {
		return errors.New("mcp server count exceeds its limit")
	}
	if value.MaxTools < 1 || value.MaxTools > MaxMCPTools || value.MaxCatalogBytes < 1024 || value.MaxCatalogBytes > 4*DefaultMCPMaxCatalogBytes || value.MaxResponseBytes < 1024 || value.MaxResponseBytes > 4*DefaultMCPMaxResponseBytes || value.MaxSearchResults < 1 || value.MaxSearchResults > MaxMCPSearchResults {
		return errors.New("mcp limits are outside their allowed range")
	}
	names := make(map[string]bool)
	for _, server := range value.Servers {
		if len(server.Description) > MaxMCPServerDescriptionBytes {
			return errors.New("mcp server description exceeds its limit")
		}
		if !validMCPName(server.Name) || names[strings.ReplaceAll(server.Name, "-", "_")] {
			return errors.New("mcp server names must be valid and unique after normalization")
		}
		names[strings.ReplaceAll(server.Name, "-", "_")] = true
		if !server.Disabled && (preset == PresetReadOnly || preset == PresetBenchmark) {
			return errors.New("read-only and benchmark presets forbid enabled MCP servers")
		}
		if !validMCPExposure(server.Exposure) || !validPolicyEffect(server.ConnectPolicy) || !validPolicyEffect(server.CallPolicy) {
			return errors.New("invalid mcp exposure or policy")
		}
		for _, timeout := range []string{server.StartupTimeout, server.ToolTimeout} {
			duration, err := time.ParseDuration(timeout)
			if err != nil || duration <= 0 || duration > time.Hour {
				return errors.New("mcp timeouts must be positive and at most one hour")
			}
		}
		for _, pattern := range append(append([]string(nil), server.AllowTools...), server.DenyTools...) {
			if _, err := path.Match(pattern, "probe"); err != nil || pattern == "" {
				return errors.New("invalid mcp tool pattern")
			}
		}
		for name, exposure := range server.ToolExposure {
			if name == "" || !validMCPExposure(exposure) {
				return errors.New("invalid mcp tool exposure override")
			}
		}
		switch server.Transport {
		case MCPTransportStdio:
			if server.Command == "" || strings.ContainsAny(server.Command, "\r\n\x00") || server.URL != "" || server.BearerTokenEnv != "" || server.OAuth != nil {
				return errors.New("mcp stdio requires one command and forbids HTTP authentication fields")
			}
			for _, arg := range server.Args {
				if strings.ContainsRune(arg, '\x00') {
					return errors.New("mcp argv cannot contain NUL")
				}
			}
			for key, source := range server.Env {
				if !validEnvironmentKey(key) || !validEnvironmentKey(source) {
					return errors.New("mcp env must map portable variable names to variable references")
				}
			}
		case MCPTransportHTTP:
			if err := ValidateMCPURL(server.URL); err != nil {
				return err
			}
			if server.Command != "" || len(server.Args) != 0 || len(server.Env) != 0 || server.CWD != "" {
				return errors.New("mcp HTTP forbids stdio fields")
			}
			if server.BearerTokenEnv != "" && (!validEnvironmentKey(server.BearerTokenEnv) || server.OAuth != nil) {
				return errors.New("mcp bearer token must reference an environment variable and cannot coexist with OAuth")
			}
			if server.OAuth != nil {
				oauth := server.OAuth
				if oauth.CallbackPort < 0 || oauth.CallbackPort > 65535 || oauth.ClientSecretEnv != "" && (!validEnvironmentKey(oauth.ClientSecretEnv) || oauth.ClientID == "") {
					return errors.New("invalid mcp OAuth registration or callback port")
				}
				if oauth.ClientSecretEnv != "" && oauth.Issuer == "" {
					return errors.New("mcp confidential OAuth clients require an explicit issuer")
				}
				if oauth.Issuer != "" {
					if oauth.ClientID == "" {
						return errors.New("mcp explicit OAuth issuer requires a preregistered client_id")
					}
					if err := ValidateMCPURL(oauth.Issuer); err != nil {
						return errors.New("invalid mcp OAuth issuer")
					}
				}
			}
		default:
			return fmt.Errorf("mcp transport must be %s or %s", MCPTransportStdio, MCPTransportHTTP)
		}
	}
	return nil
}

// ValidateMCPConfig 为直接构造适配器的调用方提供与配置加载相同的校验。
func ValidateMCPConfig(configuration MCPConfig) error {
	return validateMCP(&configuration, PresetCoding)
}

// ValidateMCPURL 接受 HTTPS 或字面 loopback HTTP，拒绝 URL 中的凭据和查询串。
func ValidateMCPURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("mcp URL must be an absolute endpoint without credentials, query or fragment")
	}
	if parsed.Scheme == "https" {
		return nil
	}
	if parsed.Scheme == "http" {
		address := net.ParseIP(parsed.Hostname())
		if parsed.Hostname() == "localhost" || address != nil && address.IsLoopback() {
			return nil
		}
	}
	return errors.New("mcp URL requires HTTPS except on loopback")
}

// validMCPName 限定可移植的工具命名空间，禁止名称被解释为路径或操作参数。
func validMCPName(value string) bool {
	if len(value) == 0 || len(value) > MaxMCPServerNameBytes {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// validMCPExposure 只接受已实现的直接、延迟和隐藏模式。
func validMCPExposure(value string) bool {
	return value == MCPExposureDirect || value == MCPExposureDeferred || value == MCPExposureHidden
}
