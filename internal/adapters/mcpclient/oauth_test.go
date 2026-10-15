package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"
)

func oauthServerConfig(endpoint string) config.MCPServerConfig {
	return config.MCPServerConfig{Name: "offline", Transport: config.MCPTransportHTTP, URL: endpoint, OAuth: &config.MCPOAuthConfig{ClientID: "offline-client", Scopes: []string{"read", "write"}}}
}

func oauthRecord(server config.MCPServerConfig) tokenRecord {
	return tokenRecord{Version: catalogVersion, Identity: credentialIdentity(server), ClientID: server.OAuth.ClientID, TokenURL: server.URL + "/token", AuthStyle: oauth2.AuthStyleInParams, Token: &oauth2.Token{AccessToken: "offline-access", RefreshToken: "offline-refresh", TokenType: "Bearer", Expiry: time.Now().Add(time.Hour)}}
}

func TestOAuthPrivateStoreIdentityPermissionsAndStrictRecord(t *testing.T) {
	server := oauthServerConfig("http://127.0.0.1:12345/mcp")
	directory := filepath.Join(t.TempDir(), "auth")
	record := oauthRecord(server)
	if err := writeTokenRecord(directory, server, record); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(credentialPath(directory, server))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("令牌文件权限不是 0600：%v", err)
	}
	dirInfo, err := os.Stat(directory)
	if err != nil || dirInfo.Mode().Perm() != 0o700 || !HasCredentials(directory, server) {
		t.Fatal("令牌目录必须私有且可本地读取")
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 || strings.HasPrefix(entries[0].Name(), ".oauth-") {
		t.Fatal("原子写入遗留了临时令牌文件")
	}
	for _, change := range []string{"name", "endpoint", "client", "issuer", "scope"} {
		t.Run(change, func(t *testing.T) {
			changed := server
			oauth := *server.OAuth
			changed.OAuth = &oauth
			switch change {
			case "name":
				changed.Name = "other"
			case "endpoint":
				changed.URL = "http://127.0.0.1:12346/mcp"
			case "client":
				changed.OAuth.ClientID = "other-client"
			case "issuer":
				changed.OAuth.Issuer = "https://issuer.example.test"
			case "scope":
				changed.OAuth.Scopes = []string{"admin"}
			}
			if HasCredentials(directory, changed) {
				t.Fatal("服务器或 OAuth 身份变化复用了旧令牌")
			}
		})
	}
	for _, corruption := range []string{"directory_mode", "file_mode", "trailing_json", "identity", "symlink"} {
		t.Run(corruption, func(t *testing.T) {
			private := filepath.Join(t.TempDir(), "auth")
			if err := writeTokenRecord(private, server, record); err != nil {
				t.Fatal(err)
			}
			filename := credentialPath(private, server)
			switch corruption {
			case "directory_mode":
				if err := os.Chmod(private, 0o755); err != nil {
					t.Fatal(err)
				}
			case "file_mode":
				if err := os.Chmod(filename, 0o644); err != nil {
					t.Fatal(err)
				}
			case "trailing_json":
				file, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY, 0)
				if err != nil {
					t.Fatal(err)
				}
				_, err = io.WriteString(file, ` {}`)
				closeErr := file.Close()
				if err != nil || closeErr != nil {
					t.Fatal(errors.Join(err, closeErr))
				}
			case "identity":
				changed := record
				changed.Identity = strings.Repeat("0", 64)
				if err := writeTokenRecord(private, server, changed); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				target := filepath.Join(t.TempDir(), "target")
				if err := os.Rename(filename, target); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filename); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readTokenRecord(private, server); err == nil || HasCredentials(private, server) {
				t.Fatal("不可信的凭据存储被接受")
			}
		})
	}
	if err := Logout(context.Background(), directory, server); err != nil || HasCredentials(directory, server) {
		t.Fatalf("退出后凭据仍可读取：%v", err)
	}
}

func TestOAuthProposalBindsLocalGrantReplacementWithoutConnecting(t *testing.T) {
	for _, changedField := range []string{"access_token", "refresh_token"} {
		t.Run(changedField, func(t *testing.T) {
			fixture := newRPCFixture(t, contractTool("echo", "本机 grant 身份测试"))
			server := oauthServerConfig(fixture.server.URL)
			manager := contractManager(t, server, 0)
			record := oauthRecord(server)
			if err := writeTokenRecord(manager.authDirectory, server, record); err != nil {
				t.Fatal(err)
			}
			before, found, err := manager.Proposal(DiscoverTool, json.RawMessage(`{"server":"offline"}`))
			if err != nil || !found || before.EnvironmentDigest == "" {
				t.Fatalf("OAuth 提议未绑定本机 grant：found=%v err=%v", found, err)
			}
			const replacement = "replacementOpaqueOAuthGrant"
			if changedField == "access_token" {
				record.Token.AccessToken = replacement
			} else {
				record.Token.RefreshToken = replacement
			}
			if err := writeTokenRecord(manager.authDirectory, server, record); err != nil {
				t.Fatal(err)
			}
			after, found, err := manager.Proposal(DiscoverTool, json.RawMessage(`{"server":"offline"}`))
			if err != nil || !found || after.EnvironmentDigest == before.EnvironmentDigest || after.Identity != before.Identity {
				t.Fatalf("本机 %s 替换未影响审批摘要：found=%v err=%v", changedField, found, err)
			}
			encoded, err := json.Marshal(after)
			if err != nil || strings.Contains(string(encoded), replacement) || strings.Contains(string(encoded), record.Token.AccessToken) || strings.Contains(string(encoded), record.Token.RefreshToken) {
				t.Fatal("审批提议泄漏 OAuth grant 正文")
			}
			if fixture.count("server/discover") != 0 || fixture.count("initialize") != 0 {
				t.Fatal("本机 OAuth 审批身份读取连接了服务器")
			}
		})
	}
}

func TestOAuthRefreshIsResourceBoundAndPersistedOnlyOnce(t *testing.T) {
	var refreshes atomic.Int32
	type refreshRequest struct{ resource, refresh string }
	received := make(chan refreshRequest, 1)
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/token" || request.Method != http.MethodPost {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		refreshes.Add(1)
		_ = request.ParseForm()
		received <- refreshRequest{request.PostForm.Get("resource"), request.PostForm.Get("refresh_token")}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"offline-new-access","refresh_token":"offline-rotated-refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer endpoint.Close()
	server := oauthServerConfig(endpoint.URL)
	directory := filepath.Join(t.TempDir(), "auth")
	record := oauthRecord(server)
	record.Token.Expiry = time.Now().Add(-time.Hour)
	if err := writeTokenRecord(directory, server, record); err != nil {
		t.Fatal(err)
	}
	client, err := newHTTPClient(server, 4096)
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseIdleConnections()
	authorization := &storedAuthorization{server: server, directory: directory, client: client}
	source, err := authorization.TokenSource(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		token, err := source.Token()
		if err != nil || token.AccessToken != "offline-new-access" || token.RefreshToken != "offline-rotated-refresh" {
			t.Fatalf("刷新失败或旋转令牌丢失：%v", err)
		}
	}
	request := <-received
	if refreshes.Load() != 1 || request.resource != server.URL || request.refresh != "offline-refresh" {
		t.Fatal("刷新未绑定 resource 或重复发送")
	}
	saved, err := readTokenRecord(directory, server)
	if err != nil || saved.Token.RefreshToken != "offline-rotated-refresh" {
		t.Fatal("刷新令牌未原子保存")
	}
}

func TestReusedOAuthSessionRefreshUsesCurrentOperationContext(t *testing.T) {
	fixture := newRPCFixture(t, contractTool("echo", "认证后的调用"))
	var refreshes atomic.Int32
	endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/token" {
			refreshes.Add(1)
			_, _ = io.Copy(io.Discard, request.Body)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"access_token":"offline-reused-access","refresh_token":"offline-reused-refresh","token_type":"Bearer","expires_in":3600}`)
			return
		}
		fixture.serveHTTP(writer, request)
	}))
	defer endpoint.Close()
	server := oauthServerConfig(endpoint.URL + "/mcp")
	server.Exposure = config.MCPExposureDirect
	server.StartupTimeout, server.ToolTimeout = "2s", "2s"
	directory := filepath.Join(t.TempDir(), "auth")
	record := oauthRecord(server)
	record.TokenURL = endpoint.URL + "/token"
	if err := writeTokenRecord(directory, server, record); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	manager, err := New(config.MCPConfig{Servers: []config.MCPServerConfig{server}}, workspace, tools.ExecCommand{Workspace: workspace}, directory)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	discovery, snapshot := discoverContract(t, manager)
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	// 上一次发现预算已经关闭；下一次调用必须使用新的预算来刷新原会话。
	record.Token.Expiry = time.Now().Add(-time.Hour)
	if err := writeTokenRecord(directory, server, record); err != nil {
		t.Fatal(err)
	}
	result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "echo").Name, json.RawMessage(`{}`))
	if !result.OK || refreshes.Load() != 1 || fixture.count("initialize") != 1 {
		t.Fatalf("复用会话刷新使用了已关闭预算或建立了新会话：%#v refresh=%d initialize=%d", result, refreshes.Load(), fixture.count("initialize"))
	}
}

func TestOAuthRefreshAndLockWaitRespectOperationCancellation(t *testing.T) {
	for _, mode := range []string{"refresh", "lock"} {
		t.Run(mode, func(t *testing.T) {
			entered := make(chan struct{}, 1)
			release := make(chan struct{})
			endpoint := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				_, _ = io.Copy(io.Discard, request.Body)
				entered <- struct{}{}
				select {
				case <-request.Context().Done():
				case <-release:
				}
			}))
			defer endpoint.Close()
			defer close(release)
			server := oauthServerConfig(endpoint.URL)
			directory := filepath.Join(t.TempDir(), "auth")
			record := oauthRecord(server)
			record.Token.Expiry = time.Now().Add(-time.Hour)
			if err := writeTokenRecord(directory, server, record); err != nil {
				t.Fatal(err)
			}
			if mode == "lock" {
				lease, err := credentialLock(t.Context(), directory, server)
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
			}
			client, err := newHTTPClient(server, 4096)
			if err != nil {
				t.Fatal(err)
			}
			defer client.CloseIdleConnections()
			operation, cancel := context.WithCancel(t.Context())
			defer cancel()
			authorization := &storedAuthorization{server: server, directory: directory, client: client, operationContext: operation}
			// SDK 传入的上下文可能已与请求预算脱离，操作上下文仍必须生效。
			source, err := authorization.TokenSource(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { _, err := source.Token(); done <- err }()
			if mode == "refresh" {
				select {
				case <-entered:
				case <-time.After(2 * time.Second):
					t.Fatal("刷新未发送请求")
				}
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, ErrAuthRequired) {
					t.Fatalf("取消未返回认证失败分类：%v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("OAuth 刷新或锁等待不响应操作取消")
			}
		})
	}
}

func TestOAuthMissingCredentialsNeverStartsInteractiveLogin(t *testing.T) {
	fixture := newRPCFixture(t, contractTool("echo", "调用工具"))
	server := oauthServerConfig(fixture.server.URL)
	manager := contractManager(t, server, 0)
	result := manager.Execute(t.Context(), DiscoverTool, json.RawMessage(`{"server":"offline"}`))
	if result.OK || result.Code != "mcp_auth_required" || fixture.count("initialize") != 0 {
		t.Fatalf("未登录时自动建立连接或未分类：%#v", result)
	}
	if err := (&storedAuthorization{}).Authorize(t.Context(), nil, &http.Response{Body: http.NoBody}); !errors.Is(err, ErrAuthRequired) {
		t.Fatal("401 不应触发隐式授权")
	}
}

type authorizationURLWriter struct {
	output chan string
}

func (writer authorizationURLWriter) Write(content []byte) (int, error) {
	writer.output <- string(content)
	return len(content), nil
}

func TestExplicitOAuthLoginCompletesPKCEAndStoresPrivateCredentials(t *testing.T) {
	fixture := newRPCFixture(t, contractTool("echo", "登录后工具"))
	type exchangeExpectation struct{ challenge, callback, resource string }
	expectations := make(chan exchangeExpectation, 1)
	var exchanges atomic.Int32
	validExchange := make(chan bool, 1)
	mux := http.NewServeMux()
	endpoint := httptest.NewServer(mux)
	defer endpoint.Close()
	baseURL := endpoint.URL
	resource := baseURL + "/mcp"
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(writer http.ResponseWriter, request *http.Request) {
		origin := "http://" + request.Host
		auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{Resource: origin + "/mcp", AuthorizationServers: []string{origin}, ScopesSupported: []string{"read", "write"}}).ServeHTTP(writer, request)
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		origin := "http://" + request.Host
		_ = json.NewEncoder(writer).Encode(oauthex.AuthServerMeta{Issuer: origin, AuthorizationEndpoint: origin + "/authorize", TokenEndpoint: origin + "/token", ResponseTypesSupported: []string{"code"}, GrantTypesSupported: []string{"authorization_code", "refresh_token"}, TokenEndpointAuthMethodsSupported: []string{"none"}, CodeChallengeMethodsSupported: []string{"S256"}, AuthorizationResponseIssParameterSupported: true})
	})
	mux.HandleFunc("/token", func(writer http.ResponseWriter, request *http.Request) {
		exchanges.Add(1)
		_ = request.ParseForm()
		verifier := request.PostForm.Get("code_verifier")
		digest := sha256.Sum256([]byte(verifier))
		expected := <-expectations
		validExchange <- verifier != "" && base64.RawURLEncoding.EncodeToString(digest[:]) == expected.challenge && request.PostForm.Get("code") == "offline-code" && request.PostForm.Get("redirect_uri") == expected.callback && request.PostForm.Get("resource") == expected.resource
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"access_token":"offline-login-access","refresh_token":"offline-login-refresh","token_type":"Bearer","expires_in":3600}`)
	})
	mux.HandleFunc("/mcp", func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer offline-login-access" {
			writer.Header().Set("WWW-Authenticate", `Bearer resource_metadata="http://`+request.Host+`/.well-known/oauth-protected-resource/mcp"`)
			writer.WriteHeader(http.StatusUnauthorized)
			return
		}
		fixture.serveHTTP(writer, request)
	})
	server := oauthServerConfig(resource)
	server.OAuth.Issuer = baseURL
	directory := filepath.Join(t.TempDir(), "auth")
	output := make(chan string, 1)
	finished := make(chan error, 1)
	loginContext, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	go func() { finished <- Login(loginContext, directory, server, authorizationURLWriter{output: output}) }()
	var authorizationURL *url.URL
	select {
	case text := <-output:
		lines := strings.Split(strings.TrimSpace(text), "\n")
		var err error
		authorizationURL, err = url.Parse(lines[len(lines)-1])
		if err != nil {
			t.Fatal(err)
		}
	case err := <-finished:
		t.Fatalf("显式登录未产生授权 URL：%v", err)
	case <-loginContext.Done():
		t.Fatal("显式登录等待授权 URL 超时")
	}
	query := authorizationURL.Query()
	codeChallenge, callbackURL, state := query.Get("code_challenge"), query.Get("redirect_uri"), query.Get("state")
	expectations <- exchangeExpectation{codeChallenge, callbackURL, resource}
	if authorizationURL.Path != "/authorize" || query.Get("code_challenge_method") != "S256" || codeChallenge == "" || state == "" || query.Get("client_id") != server.OAuth.ClientID || query.Get("resource") != resource || !strings.Contains(query.Get("scope"), "read") {
		t.Fatal("授权请求未绑定 PKCE、客户端、scope 和 resource")
	}
	callback, err := url.Parse(callbackURL)
	if err != nil {
		t.Fatal(err)
	}
	callbackQuery := url.Values{"state": {"wrong-state"}, "code": {"offline-code"}, "iss": {baseURL}}
	callback.RawQuery = callbackQuery.Encode()
	response, err := http.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusBadRequest || HasCredentials(directory, server) {
		t.Fatal("错误 state 回调产生了凭据")
	}
	callbackQuery.Set("state", state)
	callback.RawQuery = callbackQuery.Encode()
	response, err = http.Get(callback.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("正确回调被拒绝：%d", response.StatusCode)
	}
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("显式登录失败：%v", err)
		}
	case <-loginContext.Done():
		fixture.mu.Lock()
		methods := append([]string(nil), fixture.methods...)
		fixture.mu.Unlock()
		t.Fatalf("显式登录未完成：exchanges=%d methods=%v", exchanges.Load(), methods)
	}
	if exchanges.Load() != 1 || !<-validExchange || !HasCredentials(directory, server) || fixture.count("initialize") != 1 {
		t.Fatal("授权交换、协议协商或私有凭据持久化不完整")
	}
	saved, err := readTokenRecord(directory, server)
	if err != nil || saved.Token.AccessToken != "offline-login-access" || saved.Token.RefreshToken != "offline-login-refresh" {
		t.Fatal("登录后的令牌未保存")
	}
}

func TestOAuthCallbackRejectsForgedHostPathAndDuplicateParameters(t *testing.T) {
	for _, raw := range []string{
		"http://evil.example/callback?state=expected&code=offline",
		"http://127.0.0.1:12345/other?state=expected&code=offline",
		"http://127.0.0.1:12345/callback?state=expected&state=expected&code=offline",
		"http://127.0.0.1:12345/callback?state=expected&code=offline&code=second",
	} {
		callback := &authorizationCallback{host: "127.0.0.1:12345", state: "expected", result: make(chan auth.AuthorizationResult, 1)}
		writer := httptest.NewRecorder()
		callback.ServeHTTP(writer, httptest.NewRequest(http.MethodGet, raw, nil))
		if writer.Code != http.StatusBadRequest || len(callback.result) != 0 || writer.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("伪造回调被接收：%s", raw)
		}
	}
}
