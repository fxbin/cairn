// Package mcpclient 保存绑定服务器身份的本机 OAuth 凭据并串行刷新。
// Author: fxbin
package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/userdir"
	"golang.org/x/oauth2"
)

// OAuth 私有记录有独立大小与锁等待预算，避免凭据解析或争用无限增长。
const (
	maxCredentialBytes       = 64 * 1024
	credentialLockRetryDelay = 50 * time.Millisecond
)

// tokenRecord 只落盘在 0600 文件中，任何用户可见输出都不得序列化本类型。
type tokenRecord struct {
	Version      int              `json:"version"`
	Identity     string           `json:"identity"`
	ClientID     string           `json:"client_id"`
	ClientSecret string           `json:"client_secret,omitempty"`
	AuthURL      string           `json:"auth_url"`
	TokenURL     string           `json:"token_url"`
	Scopes       []string         `json:"scopes,omitempty"`
	AuthStyle    oauth2.AuthStyle `json:"auth_style"`
	Token        *oauth2.Token    `json:"token"`
}

// CredentialDirectory 返回 ~/.cairn/mcp-auth，令牌与工作区存储保持隔离。
func CredentialDirectory() (string, error) {
	return userdir.Path("mcp-auth")
}

// credentialIdentity 把令牌绑定到服务器名、URL 和 OAuth 客户端身份，阻止地址替换。
func credentialIdentity(server config.MCPServerConfig) string {
	clientID, secretEnv, issuer := "", "", ""
	var scopes []string
	if server.OAuth != nil {
		clientID = server.OAuth.ClientID
		secretEnv = server.OAuth.ClientSecretEnv
		issuer = server.OAuth.Issuer
		scopes = append([]string(nil), server.OAuth.Scopes...)
		sort.Strings(scopes)
	}
	encoded, _ := json.Marshal(struct {
		Name      string
		URL       string
		ClientID  string
		SecretEnv string
		Issuer    string
		Scopes    []string
	}{server.Name, server.URL, clientID, secretEnv, issuer, scopes})
	digest := sha256.Sum256(append([]byte("cairn-mcp-oauth-v1\x00"), encoded...))
	return hex.EncodeToString(digest[:])
}

// credentialPath 使用不可控路径字符之外的固定哈希文件名，目录本身必须是私有真目录。
func credentialPath(directory string, server config.MCPServerConfig) string {
	return filepath.Join(directory, credentialIdentity(server)+".json")
}

// secureCredentialDirectory 校验私有目录，拒绝符号链接和宽松权限。
func secureCredentialDirectory(directory string) error {
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return errors.New("create MCP credential directory failed")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o700 {
		return errors.New("MCP credential directory must be a private real directory")
	}
	return nil
}

// readTokenRecord 只接受私有常规文件和相符身份，不回显解析错误或凭据正文。
func readTokenRecord(directory string, server config.MCPServerConfig) (tokenRecord, error) {
	directoryInfo, directoryErr := os.Lstat(directory)
	if directoryErr != nil || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 {
		return tokenRecord{}, ErrAuthRequired
	}
	filename := credentialPath(directory, server)
	info, err := os.Lstat(filename)
	if err != nil {
		return tokenRecord{}, ErrAuthRequired
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 || info.Size() > maxCredentialBytes {
		return tokenRecord{}, errors.New("MCP credential file must be a private regular file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return tokenRecord{}, ErrAuthRequired
	}
	defer file.Close()
	var record tokenRecord
	decoder := json.NewDecoder(io.LimitReader(file, maxCredentialBytes+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&record) != nil || record.Version != catalogVersion || record.Identity != credentialIdentity(server) || record.Token == nil || record.Token.AccessToken == "" {
		return tokenRecord{}, ErrAuthRequired
	}
	var extra json.RawMessage
	if decoder.Decode(&extra) != io.EOF {
		return tokenRecord{}, ErrAuthRequired
	}
	return record, nil
}

// writeTokenRecord 通过同步的 0600 临时文件和原子替换保存令牌，调用方必须持有锁。
func writeTokenRecord(directory string, server config.MCPServerConfig, record tokenRecord) error {
	if err := secureCredentialDirectory(directory); err != nil {
		return err
	}
	filename := credentialPath(directory, server)
	if info, err := os.Lstat(filename); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0o600) {
		return errors.New("MCP credential target is not a private regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return errors.New("inspect MCP credential target failed")
	}
	encoded, err := json.Marshal(record)
	if err != nil || len(encoded) > maxCredentialBytes {
		return errors.New("encode MCP credentials failed")
	}
	file, err := os.CreateTemp(directory, ".oauth-")
	if err != nil {
		return errors.New("create MCP credential temporary file failed")
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err = file.Chmod(0o600); err == nil {
		_, err = file.Write(encoded)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		return errors.New("write MCP credentials failed")
	}
	if err = os.Rename(temporary, filename); err != nil {
		return errors.New("replace MCP credentials failed")
	}
	dir, err := os.Open(directory)
	if err != nil {
		return errors.New("open MCP credential directory failed")
	}
	defer dir.Close()
	if dir.Sync() != nil {
		return errors.New("sync MCP credential directory failed")
	}
	return nil
}

// credentialLock 复用文件锁串行化登录、刷新和退出，等待受到调用上下文约束。
func credentialLock(ctx context.Context, directory string, server config.MCPServerConfig) (*lock.Lock, error) {
	if err := secureCredentialDirectory(directory); err != nil {
		return nil, err
	}
	filename := credentialPath(directory, server) + ".lock"
	if info, err := os.Lstat(filename); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0o600) {
		return nil, errors.New("MCP credential lock must be a private regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, errors.New("inspect MCP credential lock failed")
	}
	for {
		lease, err := lock.Acquire(filename)
		if err == nil {
			return lease, nil
		}
		if !errors.Is(err, lock.ErrHeld) {
			return nil, errors.New("lock MCP credentials failed")
		}
		timer := time.NewTimer(credentialLockRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// storedAuthorization 在正常 Agent 调用中只读取/刷新已有凭据；交互登录由 CLI 显式发起。
type storedAuthorization struct {
	lastIdentity     string
	contextMu        sync.RWMutex
	operationContext context.Context
	secrets          *secretFilter
	server           config.MCPServerConfig
	directory        string
	client           *http.Client
}

// TokenSource 将刷新绑定到当前请求的预算和原始服务器身份。
func (authorization *storedAuthorization) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	authorization.contextMu.RLock()
	if authorization.operationContext != nil {
		ctx = authorization.operationContext
	}
	authorization.contextMu.RUnlock()
	return &storedTokenSource{ctx: ctx, authorization: authorization}, nil
}

// Authorize 拒绝把未登录或 scope 变化自动转换为浏览器授权和业务请求重试。
func (authorization *storedAuthorization) Authorize(_ context.Context, _ *http.Request, response *http.Response) error {
	if response != nil && response.Body != nil {
		_ = response.Body.Close()
	}
	return ErrAuthRequired
}

// storedTokenSource 在私有文件锁内读取最新刷新令牌，避免并发旋转造成凭据丢失。
type storedTokenSource struct {
	ctx           context.Context
	authorization *storedAuthorization
}

// Token 在需要时刷新并原子保存新令牌；错误只暴露重新登录分类。
func (source *storedTokenSource) Token() (*oauth2.Token, error) {
	authorization := source.authorization
	lease, err := credentialLock(source.ctx, authorization.directory, authorization.server)
	if err != nil {
		return nil, ErrAuthRequired
	}
	defer lease.Release()
	record, err := readTokenRecord(authorization.directory, authorization.server)
	if err != nil {
		return nil, ErrAuthRequired
	}
	clientSecret := record.ClientSecret
	if authorization.server.OAuth != nil && authorization.server.OAuth.ClientSecretEnv != "" {
		var found bool
		clientSecret, found = os.LookupEnv(authorization.server.OAuth.ClientSecretEnv)
		if !found || clientSecret == "" {
			return nil, ErrAuthRequired
		}
		record.ClientSecret = ""
	}
	currentIdentity, _ := (authenticationBinding{ClientID: record.ClientID, ClientSecret: clientSecret, AccessToken: record.Token.AccessToken, RefreshToken: record.Token.RefreshToken, Available: true}).digest()
	authorization.contextMu.RLock()
	expected := authorization.lastIdentity
	authorization.contextMu.RUnlock()
	if expected != "" && expected != currentIdentity {
		return nil, ErrAuthRequired
	}
	authorization.observeIdentity(record, clientSecret)
	if authorization.secrets != nil {
		authorization.secrets.add(clientSecret, record.Token.AccessToken, record.Token.RefreshToken)
	}
	if record.Token.Valid() {
		return record.Token, nil
	}
	if record.Token.RefreshToken == "" {
		return nil, ErrAuthRequired
	}
	if config.ValidateMCPURL(record.TokenURL) != nil {
		return nil, ErrAuthRequired
	}

	client := *authorization.client
	client.Transport = &resourceTokenTransport{base: client.Transport, tokenURL: record.TokenURL, resource: authorization.server.URL}
	refreshContext := context.WithValue(source.ctx, oauth2.HTTPClient, &client)
	configuration := oauth2.Config{ClientID: record.ClientID, ClientSecret: clientSecret, Scopes: record.Scopes, Endpoint: oauth2.Endpoint{AuthURL: record.AuthURL, TokenURL: record.TokenURL, AuthStyle: record.AuthStyle}}
	token, err := configuration.TokenSource(refreshContext, record.Token).Token()
	if err != nil || token == nil || token.AccessToken == "" {
		return nil, ErrAuthRequired
	}
	if authorization.secrets != nil {
		authorization.secrets.add(token.AccessToken, token.RefreshToken)
	}
	record.Token = token
	authorization.observeIdentity(record, clientSecret)
	if writeTokenRecord(authorization.directory, authorization.server, record) != nil {
		return nil, ErrAuthRequired
	}
	return token, nil
}

// resourceTokenTransport 为重新构造的刷新请求保留 MCP resource 绑定，其他请求不变。
type resourceTokenTransport struct {
	base     http.RoundTripper
	tokenURL string
	resource string
}

// RoundTrip 仅修改指定 token endpoint 的表单，禁止凭据通过重定向传播。
func (transport *resourceTokenTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method != http.MethodPost || request.URL.String() != transport.tokenURL {
		return transport.base.RoundTrip(request)
	}
	content, err := io.ReadAll(io.LimitReader(request.Body, maxCredentialBytes+1))
	if err != nil || len(content) > maxCredentialBytes {
		return nil, ErrAuthRequired
	}
	_ = request.Body.Close()
	form, err := url.ParseQuery(string(content))
	if err != nil {
		return nil, ErrAuthRequired
	}
	form.Set("resource", transport.resource)
	copyRequest := request.Clone(request.Context())
	copyRequest.Body = io.NopCloser(strings.NewReader(form.Encode()))
	copyRequest.ContentLength = int64(len(form.Encode()))
	return transport.base.RoundTrip(copyRequest)
}

// HasCredentials 返回非敏感登录状态，不连接或刷新服务端。
func HasCredentials(directory string, server config.MCPServerConfig) bool {
	_, err := readTokenRecord(directory, server)
	return err == nil
}

// Logout 只删除指定身份的私有令牌文件，保留配置和其他服务器凭据。
func Logout(ctx context.Context, directory string, server config.MCPServerConfig) error {
	lease, err := credentialLock(ctx, directory, server)
	if err != nil {
		return err
	}
	defer lease.Release()
	filename := credentialPath(directory, server)
	if info, err := os.Lstat(filename); err == nil && (!info.Mode().IsRegular() || info.Mode().Perm() != 0o600) {
		return errors.New("MCP credential target is not a private regular file")
	}
	if err := os.Remove(filename); err != nil && !os.IsNotExist(err) {
		return errors.New("remove MCP credentials failed")
	}
	return nil
}

// setOperationContext 在协商完成后切换预算，刷新读取上下文时保持并发安全。
func (authorization *storedAuthorization) setOperationContext(ctx context.Context) {
	authorization.contextMu.Lock()
	defer authorization.contextMu.Unlock()
	authorization.operationContext = ctx
}

// observeIdentity 保存本次请求实际读取或刷新的授权身份，避免正常旋转被误当作账号替换。
func (authorization *storedAuthorization) observeIdentity(record tokenRecord, clientSecret string) {
	identity, _ := (authenticationBinding{ClientID: record.ClientID, ClientSecret: clientSecret, AccessToken: record.Token.AccessToken, RefreshToken: record.Token.RefreshToken, Available: true}).digest()
	authorization.contextMu.Lock()
	defer authorization.contextMu.Unlock()
	authorization.lastIdentity = identity
}
