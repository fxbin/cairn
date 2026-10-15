// Package mcpclient 限制 MCP 与 OAuth 网络请求的目标、凭据和响应大小。
// Author: fxbin
package mcpclient

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/config"
)

// MCP 网络预算限制连接、TLS 握手和响应头等待；响应体另受操作预算约束。
const (
	mcpNetworkDialTimeout    = 10 * time.Second
	mcpResponseHeaderTimeout = 15 * time.Second
)

// boundedHTTPTransport 只向配置原点发送 Bearer，并限制解压后的响应字节。
type boundedHTTPTransport struct {
	cleanupMu                sync.RWMutex
	cleanupContext           context.Context
	remoteReleaseUnsupported bool
	expectedIdentity         string
	identityMu               sync.RWMutex
	lastBearer               string
	base                     *http.Transport
	endpoint                 *url.URL
	bearerEnv                string
	maxBytes                 int
	secrets                  *secretFilter
}

// newHTTPClient 构造无代理继承、禁止重定向且有响应上限的 MCP/OAuth 客户端。
func newHTTPClient(server config.MCPServerConfig, maxBytes int) (*http.Client, error) {
	endpoint, err := url.Parse(server.URL)
	if err != nil || endpoint.Host == "" {
		return nil, errors.New("invalid MCP endpoint")
	}
	base := &http.Transport{ForceAttemptHTTP2: true, Proxy: nil, DialContext: mcpDialer(endpoint), TLSHandshakeTimeout: mcpNetworkDialTimeout, ResponseHeaderTimeout: mcpResponseHeaderTimeout, DisableKeepAlives: true}
	return &http.Client{Transport: &boundedHTTPTransport{base: base, endpoint: endpoint, bearerEnv: server.BearerTokenEnv, maxBytes: maxBytes}, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("MCP HTTP redirects are not allowed") }}, nil
}

// mcpDialer 允许显式端点和公开授权服务器，阻止元数据中的私网/loopback 跳转。
func mcpDialer(endpoint *url.URL) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("invalid MCP network target")
		}
		addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, errors.New("resolve MCP network target failed")
		}
		endpointIP := net.ParseIP(endpoint.Hostname())
		localEndpoint := endpoint.Hostname() == "localhost" || endpointIP != nil && endpointIP.IsLoopback()
		for _, ip := range addresses {
			parsed := net.IP(ip.AsSlice())
			if !strings.EqualFold(host, endpoint.Hostname()) && !(localEndpoint && parsed.IsLoopback()) && (!parsed.IsGlobalUnicast() || parsed.IsPrivate() || parsed.IsLoopback() || parsed.IsLinkLocalUnicast()) {
				return nil, errors.New("MCP authorization target is not public")
			}
		}
		dialer := net.Dialer{Timeout: mcpNetworkDialTimeout}
		for _, ip := range addresses {
			connection, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if err == nil {
				return connection, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
		}
		return nil, errors.New("connect MCP network target failed")
	}
}

// RoundTrip 在发送前验证协议与凭据原点，并在读取阶段施加统一响应上限。
func (transport *boundedHTTPTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.User != nil || request.URL.Fragment != "" || request.URL.Scheme != "https" && request.URL.Scheme != "http" {
		return nil, errors.New("invalid MCP HTTP target")
	}
	if request.URL.Scheme == "http" {
		ip := net.ParseIP(request.URL.Hostname())
		if request.URL.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
			return nil, errors.New("MCP HTTP requires HTTPS except on loopback")
		}
	}
	requestContext := request.Context()
	if request.Method == http.MethodDelete && sameOrigin(request.URL, transport.endpoint) {
		transport.cleanupMu.RLock()
		if transport.cleanupContext != nil {
			requestContext = transport.cleanupContext
		}
		transport.cleanupMu.RUnlock()
	}
	copyRequest := request.Clone(requestContext)
	copyRequest.Header = request.Header.Clone()
	if transport.bearerEnv != "" && sameOrigin(request.URL, transport.endpoint) {
		token, exists := os.LookupEnv(transport.bearerEnv)
		if !exists || token == "" || strings.ContainsAny(token, "\r\n") {
			return nil, ErrAuthRequired
		}
		transport.identityMu.Lock()
		currentIdentity, _ := (authenticationBinding{Bearer: token, Available: true}).digest()
		if transport.expectedIdentity != "" && transport.expectedIdentity != currentIdentity {
			transport.identityMu.Unlock()
			return nil, ErrAuthRequired
		}
		transport.expectedIdentity = currentIdentity
		transport.lastBearer = token
		transport.identityMu.Unlock()
		if transport.secrets != nil {
			transport.secrets.add(token)
		}
		copyRequest.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := transport.base.RoundTrip(copyRequest)
	if err != nil {
		return nil, errors.New("MCP HTTP request failed")
	}
	if request.Method == http.MethodDelete && sameOrigin(request.URL, transport.endpoint) {
		// SDK 不检查 DELETE 状态；这里确认释放或明确记录仅关闭本地会话。
		_ = response.Body.Close()
		response.Body = http.NoBody
		response.ContentLength = 0
		switch {
		case response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices:
		case response.StatusCode == http.StatusNotFound || response.StatusCode == http.StatusGone:
		case response.StatusCode == http.StatusMethodNotAllowed:
			transport.cleanupMu.Lock()
			transport.remoteReleaseUnsupported = true
			transport.cleanupMu.Unlock()
		default:
			return nil, errors.New("MCP HTTP session release was not confirmed")
		}
		return response, nil
	}
	if response.ContentLength > int64(transport.maxBytes) {
		_ = response.Body.Close()
		return nil, errors.New("MCP HTTP response exceeds its limit")
	}
	response.Body = &boundedResponseBody{source: response.Body, remaining: int64(transport.maxBytes)}
	return response, nil
}

// CloseIdleConnections 在单次操作结束后回收底层连接资源。
func (transport *boundedHTTPTransport) CloseIdleConnections() { transport.base.CloseIdleConnections() }

// sameOrigin 比较规范原点，避免凭据被转发到另一地址或端口。
func sameOrigin(left, right *url.URL) bool {
	return strings.EqualFold(left.Scheme, right.Scheme) && strings.EqualFold(left.Host, right.Host)
}

// boundedResponseBody 在超过字节上限时返回明确失败，不能把截断响应当作完整结果。
type boundedResponseBody struct {
	source    io.ReadCloser
	remaining int64
}

// Read 额外探测一个字节以区分刚好达到上限与实际超限。
func (body *boundedResponseBody) Read(buffer []byte) (int, error) {
	if int64(len(buffer)) > body.remaining+1 {
		buffer = buffer[:body.remaining+1]
	}
	n, err := body.source.Read(buffer)
	if int64(n) > body.remaining {
		accepted := int(body.remaining)
		body.remaining = 0
		_ = body.source.Close()
		return accepted, errors.New("MCP HTTP response exceeds its limit")
	}
	body.remaining -= int64(n)
	return n, err
}

// Close 回收响应体，供 SDK 的失败和正常退出路径共同使用。
func (body *boundedResponseBody) Close() error { return body.source.Close() }
