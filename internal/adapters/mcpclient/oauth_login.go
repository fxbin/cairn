// Package mcpclient 通过显式 CLI 登录完成 MCP OAuth 授权码与 PKCE 流程。
// Author: fxbin
package mcpclient

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

// Login 使用官方 SDK 验证授权服务器、state、issuer 与 PKCE，令牌只写本机私有文件。
func Login(ctx context.Context, directory string, server config.MCPServerConfig, output io.Writer) error {
	if server.Disabled || server.Transport != config.MCPTransportHTTP || server.OAuth == nil {
		return errors.New("MCP server must enable HTTP OAuth before login")
	}
	lease, err := credentialLock(ctx, directory, server)
	if err != nil {
		return err
	}
	defer lease.Release()
	listener, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(server.OAuth.CallbackPort)))
	if err != nil {
		return errors.New("listen for MCP OAuth callback failed")
	}
	defer listener.Close()
	callbackURL := "http://" + listener.Addr().String() + "/callback"
	callback := &authorizationCallback{host: listener.Addr().String(), result: make(chan auth.AuthorizationResult, 1)}
	httpServer := &http.Server{Handler: callback, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = httpServer.Serve(listener) }()
	defer func() {
		shutdownContext, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownContext)
	}()
	client, err := newHTTPClient(server, config.DefaultMCPMaxResponseBytes)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	persisted := false
	configuration := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL: callbackURL, Client: client, RequestRefreshToken: true,
		AuthorizationCodeFetcher: func(flowContext context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
			parsed, err := url.Parse(args.URL)
			if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" || parsed.User != nil {
				return nil, errors.New("invalid MCP authorization URL")
			}
			callback.mu.Lock()
			callback.state = parsed.Query().Get("state")
			callback.mu.Unlock()
			if _, err := fmt.Fprintln(output, "请在浏览器完成 MCP 登录：\n"+args.URL); err != nil {
				return nil, errors.New("write MCP authorization URL failed")
			}
			select {
			case <-flowContext.Done():
				return nil, flowContext.Err()
			case result := <-callback.result:
				return &result, nil
			}
		},
		ScopeFilter: func(discovered []string) []string {
			if len(server.OAuth.Scopes) != 0 {
				return append([]string(nil), server.OAuth.Scopes...)
			}
			return discovered
		},
		NewTokenSource: func(flowContext context.Context, oauthConfiguration *oauth2.Config, token *oauth2.Token) (oauth2.TokenSource, error) {
			if oauthConfiguration.ClientID == "" || token == nil || token.AccessToken == "" || config.ValidateMCPURL(oauthConfiguration.Endpoint.TokenURL) != nil {
				return nil, errors.New("MCP OAuth returned invalid credentials")
			}
			storedClientSecret := oauthConfiguration.ClientSecret
			if server.OAuth.ClientSecretEnv != "" {
				storedClientSecret = ""
			}
			record := tokenRecord{Version: catalogVersion, Identity: credentialIdentity(server), ClientID: oauthConfiguration.ClientID, ClientSecret: storedClientSecret, AuthURL: oauthConfiguration.Endpoint.AuthURL, TokenURL: oauthConfiguration.Endpoint.TokenURL, AuthStyle: oauthConfiguration.Endpoint.AuthStyle, Scopes: append([]string(nil), oauthConfiguration.Scopes...), Token: token}
			if err := writeTokenRecord(directory, server, record); err != nil {
				return nil, err
			}
			persisted = true
			return oauthConfiguration.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, client), token), nil
		},
	}
	if server.OAuth.ClientID != "" {
		credentials := &oauthex.ClientCredentials{ClientID: server.OAuth.ClientID, Issuer: server.OAuth.Issuer}
		if server.OAuth.ClientSecretEnv != "" {
			secret, found := os.LookupEnv(server.OAuth.ClientSecretEnv)
			if !found || secret == "" {
				return errors.New("MCP OAuth client secret reference is unavailable")
			}
			credentials.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: secret}
		}
		configuration.PreregisteredClient = credentials
	} else {
		configuration.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{RedirectURIs: []string{callbackURL}, ClientName: "Cairn", TokenEndpointAuthMethod: "none", GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"}, ApplicationType: "native"}}
	}
	handler, err := auth.NewAuthorizationCodeHandler(configuration)
	if err != nil {
		return errors.New("configure MCP OAuth failed")
	}
	clientSDK := mcp.NewClient(&mcp.Implementation{Name: "cairn-login", Version: "mcp-client-v1"}, &mcp.ClientOptions{MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
	session, connectErr := clientSDK.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: client, OAuthHandler: handler, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: config.DefaultMCPMaxResponseBytes}, nil)
	if session != nil {
		_ = session.Close()
	}
	if !persisted {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("MCP OAuth login did not produce credentials")
	}
	if connectErr != nil {
		return errors.New("MCP OAuth credentials were saved but server negotiation failed")
	}
	return nil
}

// authorizationCallback 只接收正确 loopback host/path/state 的授权结果。
type authorizationCallback struct {
	mu     sync.RWMutex
	state  string
	host   string
	result chan auth.AuthorizationResult
}

// ServeHTTP 拒绝伪造和重复参数，不把授权码或错误详情返回到页面。
func (callback *authorizationCallback) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
	query := request.URL.Query()
	callback.mu.RLock()
	expected := callback.state
	callback.mu.RUnlock()
	if request.Method != http.MethodGet || request.Host != callback.host || request.URL.Path != "/callback" || expected == "" || len(query["state"]) != 1 || len(query["code"]) != 1 || len(query["iss"]) > 1 || query.Get("code") == "" || subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(expected)) != 1 {
		http.Error(writer, "无效的授权回调。", http.StatusBadRequest)
		return
	}
	result := auth.AuthorizationResult{Code: query.Get("code"), State: query.Get("state"), Iss: query.Get("iss")}
	select {
	case callback.result <- result:
		fmt.Fprintln(writer, "登录回调已接收，可返回终端。")
	default:
		http.Error(writer, "授权回调已经接收。", http.StatusConflict)
	}
}
