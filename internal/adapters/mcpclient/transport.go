// Package mcpclient 为受审计操作提供 SDK 会话、分段预算与可确认的关闭边界。
// Author: fxbin
package mcpclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSessionCleanupTimeout 限制结束会话时的认证与协议清理。
const mcpSessionCleanupTimeout = 5 * time.Second

// wireSession 由 Boot 持有，封装 SDK 会话和本地进程；关闭错误保留副作用不确定性。
type wireSession struct {
	initialIdentity  string
	httpTransport    *boundedHTTPTransport
	sdk              *mcp.ClientSession
	process          *tools.MCPProcess
	maxResponseBytes int
	secrets          *secretFilter
	stopProcessWatch func()
	authorization    *storedAuthorization
	closeOnce        sync.Once
	closeError       error
}

// ErrAuthRequired 只携带重新登录分类，不携带服务端响应或任何凭据。
var ErrAuthRequired = errors.New("MCP authentication is required; use cairn mcp login")

// errTerminationUnconfirmed 表示连接失败后的进程回收也没有得到确认。
var errTerminationUnconfirmed = errors.New("MCP connection termination is unconfirmed")

// connect 在独立启动预算内协商协议；构造器不设置服务端发起能力或自动重试。
func (manager *Manager) connect(ctx context.Context, server config.MCPServerConfig, identity string) (*wireSession, error) {
	startupDuration, _ := time.ParseDuration(server.StartupTimeout)
	startupContext, cancel := context.WithTimeout(ctx, startupDuration)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "cairn", Version: "mcp-client-v1"}, &mcp.ClientOptions{Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}})
	session := &wireSession{maxResponseBytes: manager.configuration.MaxResponseBytes, secrets: &secretFilter{}}
	var transport mcp.Transport
	var authorization *storedAuthorization
	switch server.Transport {
	case config.MCPTransportStdio:
		for key := range manager.exec.EnvAllow {
			if credentialEnvironment(key) {
				if value, found := os.LookupEnv(key); found {
					session.secrets.add(value)
				}
			}
		}
		environment := make(map[string]string, len(server.Env))
		for key, source := range server.Env {
			value, found := os.LookupEnv(source)
			if !found {
				return nil, errors.New("MCP environment reference is unavailable")
			}
			environment[key] = value
			if credentialEnvironment(key) || credentialEnvironment(source) {
				session.secrets.add(value)
			}
		}
		argv := append([]string{server.Command}, server.Args...)
		process, err := manager.exec.StartMCP(argv, server.CWD, environment)
		if err != nil {
			return nil, err
		}
		session.process = process
		session.stopProcessWatch = watchProcessContext(startupContext, process)
		transport = &mcp.IOTransport{Reader: process.Reader, Writer: process.Writer, MaxLineLength: manager.configuration.MaxResponseBytes}
	case config.MCPTransportHTTP:
		httpClient, err := newHTTPClient(server, manager.configuration.MaxResponseBytes)
		if err != nil {
			return nil, err
		}
		session.httpTransport = httpClient.Transport.(*boundedHTTPTransport)
		session.httpTransport.secrets = session.secrets
		if server.BearerTokenEnv != "" {
			session.httpTransport.expectedIdentity = identity
		}
		httpTransport := &mcp.StreamableClientTransport{Endpoint: server.URL, HTTPClient: httpClient, MaxRetries: -1, DisableStandaloneSSE: true, MaxEventSize: manager.configuration.MaxResponseBytes}
		if server.OAuth != nil {
			authorization = &storedAuthorization{lastIdentity: identity, server: server, directory: manager.authDirectory, client: httpClient, operationContext: startupContext, secrets: session.secrets}
			httpTransport.OAuthHandler = authorization
		}
		transport = httpTransport
	default:
		return nil, errors.New("unsupported MCP transport")
	}
	connected, err := client.Connect(startupContext, transport, nil)
	if err != nil {
		if session.stopProcessWatch != nil {
			session.stopProcessWatch()
		}
		if session.process != nil && session.process.Close() != nil {
			return nil, errTerminationUnconfirmed
		}
		return nil, err
	}
	if session.stopProcessWatch != nil {
		session.stopProcessWatch()
		session.stopProcessWatch = nil
	}
	if authorization != nil {
		authorization.setOperationContext(ctx)
		session.authorization = authorization
	}
	session.sdk = connected
	return session, nil
}

// close 先关闭协议连接，再确认本地进程回收；不会重发业务调用。
func (session *wireSession) close() error {
	session.closeOnce.Do(func() {
		cleanupContext, cancel := context.WithTimeout(context.Background(), mcpSessionCleanupTimeout)
		defer cancel()
		if session.authorization != nil {
			session.authorization.setOperationContext(cleanupContext)
		}
		if session.stopProcessWatch != nil {
			session.stopProcessWatch()
			session.stopProcessWatch = nil
		}
		if session.httpTransport != nil {
			session.httpTransport.cleanupMu.Lock()
			session.httpTransport.cleanupContext = cleanupContext
			session.httpTransport.cleanupMu.Unlock()
		}
		if session.sdk != nil {
			if session.sdk.Close() != nil {
				session.closeError = errors.New("MCP protocol session cleanup failed")
			}
		}
		if session.process != nil {
			session.closeError = errors.Join(session.closeError, session.process.Close())
		}
	})
	return session.closeError
}

// operationBudget 在每次串行操作开始时绑定 OAuth 和管道取消；结束操作不会销毁会话。
func (session *wireSession) operationBudget(ctx context.Context) func() {
	if session.authorization != nil {
		session.authorization.setOperationContext(ctx)
	}
	if session.process != nil {
		return watchProcessContext(ctx, session.process)
	}
	return func() {}
}

// connectionFailure 把认证、启动和回收失败转换为不含原始服务端内容的分类。
func connectionFailure(err error) tools.Result {
	if errors.Is(err, errTerminationUnconfirmed) {
		return tools.Failure(tools.CodeTerminationUnconfirmed, "MCP process termination could not be confirmed")
	}
	if errors.Is(err, ErrAuthRequired) {
		return tools.Failure("mcp_auth_required", ErrAuthRequired.Error())
	}
	return tools.Failure("mcp_connection_failed", "MCP connection or protocol negotiation failed")
}

// readCatalog 有界遍历工具页，将 SDK schema 序列化为不透明文档并生成冻结摘要。
func (manager *Manager) readCatalog(ctx context.Context, server config.MCPServerConfig, session *wireSession) (Snapshot, error) {
	snapshot := Snapshot{Version: catalogVersion, Server: server.Name, Identity: server.Fingerprint(), Protocol: session.sdk.InitializeResult().ProtocolVersion, Tools: make([]Definition, 0)}
	cursor := ""
	seen := make(map[string]bool)
	total := 0
	for page := 0; page <= manager.configuration.MaxTools; page++ {
		if seen[cursor] {
			return Snapshot{}, errors.New("MCP catalog cursor repeated")
		}
		seen[cursor] = true
		result, err := session.sdk.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return Snapshot{}, err
		}
		total += len(result.Tools)
		if total > manager.configuration.MaxTools {
			return Snapshot{}, errors.New("MCP catalog exceeds its tool limit")
		}
		for _, tool := range result.Tools {
			if tool == nil || tool.Name == "" {
				return Snapshot{}, errors.New("MCP tool name is missing")
			}
			if session.secrets.text(tool.Name) != tool.Name {
				return Snapshot{}, errors.New("MCP tool identity contains credentials")
			}
			if !allowedTool(server, tool.Name) {
				continue
			}
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil || len(schema) == 0 || schema[0] != '{' {
				return Snapshot{}, errors.New("MCP tool schema is not an object")
			}
			hints, err := json.Marshal(tool.Annotations)
			if err != nil {
				return Snapshot{}, err
			}
			outputSchema, err := json.Marshal(tool.OutputSchema)
			if err != nil {
				return Snapshot{}, err
			}
			sourceDigest, err := canonicaljson.Fingerprint("cairn-mcp-source-tool-v1", tool)
			if err != nil {
				return Snapshot{}, err
			}
			definition := Definition{SourceDigest: sourceDigest, OriginalName: tool.Name, Name: toolAlias(server.Name, tool.Name), Description: session.secrets.text(tool.Description), InputSchemaBase64: base64.StdEncoding.EncodeToString(session.secrets.json(schema)), OutputSchemaBase64: base64.StdEncoding.EncodeToString(session.secrets.json(outputSchema)), HintsBase64: base64.StdEncoding.EncodeToString(session.secrets.json(hints))}
			definition.Digest, err = definitionDigest(definition)
			if err != nil {
				return Snapshot{}, err
			}
			snapshot.Tools = append(snapshot.Tools, definition)
		}
		encoded, err := json.Marshal(snapshot)
		if err != nil || len(encoded) > manager.configuration.MaxCatalogBytes {
			return Snapshot{}, errors.New("MCP catalog exceeds its byte limit")
		}
		if result.NextCursor == "" {
			sort.Slice(snapshot.Tools, func(i, j int) bool { return snapshot.Tools[i].Name < snapshot.Tools[j].Name })
			snapshot.Digest, err = snapshotDigest(snapshot)
			if err != nil {
				return Snapshot{}, err
			}
			manager.mu.RLock()
			validationErr := manager.validateCatalog(snapshot)
			manager.mu.RUnlock()
			return snapshot, validationErr
		}
		cursor = result.NextCursor
	}
	return Snapshot{}, errors.New("MCP catalog exceeds its pagination limit")
}

// call 只发出一次业务请求，保留协议结构和明确 isError；中途输入请求不自动重发。
func (session *wireSession) call(ctx context.Context, name string, arguments json.RawMessage) (tools.Result, error) {
	result, err := session.sdk.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		return tools.Result{}, err
	}
	if result == nil || len(result.InputRequests) != 0 || result.RequestState != "" {
		return tools.Result{}, errors.New("MCP tool has no complete response")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return tools.Result{}, err
	}
	encoded = artifact.RedactJSON(session.secrets.json(encoded))
	if len(encoded) > session.maxResponseBytes {
		return tools.Result{}, errors.New("MCP result exceeds its byte limit")
	}
	preview := mcpPreview(result, session.secrets)
	digest := sha256.Sum256(encoded)
	code := "ok"
	if result.IsError {
		code = "mcp_tool_error"
	}
	return tools.Result{OK: !result.IsError, Code: code, Data: encoded, Preview: preview, Bytes: int64(len(encoded)), SHA256: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

// mcpPreview 仅取有界的已脱敏文本摘要，二进制内容保留在结果制品中。
func mcpPreview(result *mcp.CallToolResult, secrets *secretFilter) string {
	const previewBytes = 1024
	var parts []string
	for _, content := range result.Content {
		if text, ok := content.(*mcp.TextContent); ok {
			parts = append(parts, text.Text)
		}
	}
	preview := secrets.text(strings.Join(parts, "\n"))
	if len(preview) > previewBytes {
		preview = preview[:previewBytes]
		for !utf8.ValidString(preview) {
			preview = preview[:len(preview)-1]
		}
	}
	if preview == "" {
		preview = "MCP tool returned structured content"
	}
	return preview
}

// watchProcessContext 让阻塞的管道写入也响应预算和取消；停止观察时等待 goroutine 退出。
func watchProcessContext(ctx context.Context, process *tools.MCPProcess) func() {
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-ctx.Done():
			_ = process.Close()
		case <-stop:
		}
	}()
	return func() { close(stop); <-done }
}

// credentialDigest 返回活会话实际使用的身份；不能以已变化的本机环境替代旧进程环境。
func (session *wireSession) credentialDigest() string {
	if session.authorization != nil {
		session.authorization.contextMu.RLock()
		value := session.authorization.lastIdentity
		session.authorization.contextMu.RUnlock()
		if value != "" {
			return value
		}
	}
	if session.httpTransport != nil && session.httpTransport.bearerEnv != "" {
		session.httpTransport.identityMu.RLock()
		bearer := session.httpTransport.lastBearer
		session.httpTransport.identityMu.RUnlock()
		if bearer != "" {
			value, _ := (authenticationBinding{Bearer: bearer, Available: true}).digest()
			return value
		}
	}
	return session.initialIdentity
}
