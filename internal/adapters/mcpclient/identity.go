// Package mcpclient 区分当前本机凭据与活会话实际使用的认证身份。
// Author: fxbin
package mcpclient

import (
	"os"
	"strings"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/credential"
)

// authenticationBinding 仅在内存中组合实际授权值；对外只输出不可逆摘要。
type authenticationBinding struct {
	Bearer       string
	ClientSecret string
	ClientID     string
	AccessToken  string
	RefreshToken string
	Available    bool
}

// digest 将账号凭据替换绑定到审批与活会话，不记录令牌或密钥正文。
func (binding authenticationBinding) digest() (string, error) {
	return canonicaljson.Fingerprint("cairn-mcp-authentication-v1", binding)
}

// readAuthenticationBinding 仅读取本机凭据事实；缺少凭据使用稳定状态，不连接或刷新。
func readAuthenticationBinding(directory string, server config.MCPServerConfig) authenticationBinding {
	binding := authenticationBinding{Available: true}
	if server.BearerTokenEnv != "" {
		binding.Bearer, binding.Available = os.LookupEnv(server.BearerTokenEnv)
	}
	if server.OAuth != nil {
		record, err := readTokenRecord(directory, server)
		binding.Available = err == nil
		if err == nil {
			binding.ClientID = record.ClientID
			binding.AccessToken = record.Token.AccessToken
			binding.RefreshToken = record.Token.RefreshToken
			binding.ClientSecret = record.ClientSecret
		}
		if server.OAuth.ClientSecretEnv != "" {
			value, found := os.LookupEnv(server.OAuth.ClientSecretEnv)
			binding.ClientSecret = value
			binding.Available = binding.Available && found
		}
	}
	return binding
}

// credentialEnvironment 复用通用凭据词汇，并明确识别 MCP 常见认证别名，保留普通运行环境。
func credentialEnvironment(name string) bool {
	name = strings.ToUpper(name)
	return credential.Key(name) || name == "AUTH" || name == "AUTHORIZATION" || name == "KEY" || strings.HasSuffix(name, "_AUTH")
}
