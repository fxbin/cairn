// Package app 提供 MCP 配置与本机认证用例，业务工具调用仍由 Agent 执行。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"io"

	"github.com/fxbin/cairn/internal/adapters/mcpclient"
	"github.com/fxbin/cairn/internal/config"
)

// MCPServerStatus 描述配置和私有凭据存在性，不宣称已连接或工具已验证。
type MCPServerStatus struct {
	Name               string `json:"name"`
	Transport          string `json:"transport"`
	Exposure           string `json:"exposure"`
	Disabled           bool   `json:"disabled"`
	Authentication     string `json:"authentication"`
	CredentialsPresent bool   `json:"credentials_present"`
	ConnectPolicy      string `json:"connect_policy"`
	CallPolicy         string `json:"call_policy"`
}

// MCPDeclaration 是能力清单中的纯配置投影，不含 URL、环境值或连接状态。
type MCPDeclaration struct {
	Name          string `json:"name"`
	Transport     string `json:"transport"`
	Exposure      string `json:"exposure"`
	Disabled      bool   `json:"disabled"`
	ConnectPolicy string `json:"connect_policy"`
	CallPolicy    string `json:"call_policy"`
}

// mcpDeclarations 供 Boot 与 doctor 共用同一非敏感 MCP 能力声明。
func mcpDeclarations(configuration *config.MCPConfig) []MCPDeclaration {
	if configuration == nil {
		return nil
	}
	declarations := make([]MCPDeclaration, 0, len(configuration.Servers))
	for _, server := range configuration.Effective().Servers {
		declarations = append(declarations, MCPDeclaration{Name: server.Name, Transport: server.Transport, Exposure: server.Exposure, Disabled: server.Disabled, ConnectPolicy: server.ConnectPolicy, CallPolicy: server.CallPolicy})
	}
	return declarations
}

// hasEnabledMCP 在纯装配阶段判断是否存在明确启用的 MCP 服务器。
func hasEnabledMCP(configuration *config.MCPConfig) bool {
	if configuration == nil {
		return false
	}
	for _, server := range configuration.Servers {
		if !server.Disabled {
			return true
		}
	}
	return false
}

// MCPStatus 返回非敏感配置与凭据存在性；不连接或启动服务器。
func (service Service) MCPStatus(configuration config.Config) ([]MCPServerStatus, error) {
	if err := config.Validate(configuration); err != nil {
		return nil, err
	}
	statuses := make([]MCPServerStatus, 0)
	if configuration.MCP == nil {
		return statuses, nil
	}
	directory, err := mcpclient.CredentialDirectory()
	if err != nil {
		return nil, errors.New("resolve MCP credential directory failed")
	}
	for _, server := range configuration.MCP.Effective().Servers {
		status := MCPServerStatus{Name: server.Name, Transport: server.Transport, Exposure: server.Exposure, Disabled: server.Disabled, Authentication: "none", ConnectPolicy: server.ConnectPolicy, CallPolicy: server.CallPolicy}
		if server.BearerTokenEnv != "" {
			status.Authentication = "bearer_env"
		}
		if server.OAuth != nil {
			status.Authentication = "oauth"
			status.CredentialsPresent = mcpclient.HasCredentials(directory, server)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// MCPLogin 只对明确配置的 HTTP OAuth 服务器发起用户登录，不调用业务工具。
func (service Service) MCPLogin(ctx context.Context, configuration config.Config, name string, output io.Writer) error {
	server, err := configuredMCPServer(configuration, name)
	if err != nil {
		return err
	}
	directory, err := mcpclient.CredentialDirectory()
	if err != nil {
		return errors.New("resolve MCP credential directory failed")
	}
	return mcpclient.Login(ctx, directory, server, output)
}

// MCPLogout 移除指定服务器身份的 OAuth 凭据，保留配置和其他凭据。
func (service Service) MCPLogout(ctx context.Context, configuration config.Config, name string) error {
	server, err := configuredMCPServer(configuration, name)
	if err != nil {
		return err
	}
	directory, err := mcpclient.CredentialDirectory()
	if err != nil {
		return errors.New("resolve MCP credential directory failed")
	}
	return mcpclient.Logout(ctx, directory, server)
}

// configuredMCPServer 严格匹配显式服务器名，不接受临时 URL 或动态命令。
func configuredMCPServer(configuration config.Config, name string) (config.MCPServerConfig, error) {
	if err := config.Validate(configuration); err != nil {
		return config.MCPServerConfig{}, err
	}
	if configuration.MCP != nil {
		for _, server := range configuration.MCP.Effective().Servers {
			if server.Name == name {
				return server, nil
			}
		}
	}
	return config.MCPServerConfig{}, errors.New("MCP server is not configured")
}
