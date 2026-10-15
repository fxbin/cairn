// Package mcpclient 以显式配置、受审计发现快照和惰性连接接入外部 MCP 工具。
// Author: fxbin
package mcpclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// 目录证据版本和辅助工具名属于恢复与模型声明合同。
const (
	catalogVersion     = 1
	DiscoverTool       = "mcp_discover"
	SearchTool         = "tool_search"
	maxToolAliasLength = 64
)

// Definition 保留源工具身份与已脱敏的编码 schema，防止日志脱敏二次改写文档。
// HintsBase64 只提供协议提示，不参与权限判定。
type Definition struct {
	SourceDigest       string `json:"source_digest"`
	OriginalName       string `json:"original_name"`
	Name               string `json:"name"`
	Description        string `json:"description"`
	InputSchemaBase64  string `json:"input_schema_base64"`
	OutputSchemaBase64 string `json:"output_schema_base64,omitempty"`
	HintsBase64        string `json:"hints_base64,omitempty"`
	Digest             string `json:"digest"`
}

// Snapshot 将完整发现目录绑定到明确的服务器配置和协商协议版本。
type Snapshot struct {
	Version  int          `json:"version"`
	Server   string       `json:"server"`
	Identity string       `json:"identity"`
	Protocol string       `json:"protocol"`
	Tools    []Definition `json:"tools"`
	Digest   string       `json:"digest"`
}

// Exposure 是搜索结果的可回放加载集合；只能引用已经持久化的目录摘要。
type Exposure struct {
	Version int          `json:"version"`
	Loaded  []LoadedTool `json:"loaded"`
}

// LoadedTool 保留一次搜索所加载工具的目录身份和模型声明。
type LoadedTool struct {
	Server        string     `json:"server"`
	CatalogDigest string     `json:"catalog_digest"`
	Tool          Definition `json:"tool"`
}

// Manager 保存已审计目录与可见性投影；构造和 Specs 都不访问外部服务。
type Manager struct {
	configuration config.MCPConfig
	workspace     tools.Workspace
	exec          tools.ExecCommand
	authDirectory string
	mu            sync.RWMutex
	servers       map[string]config.MCPServerConfig
	catalogs      map[string]Snapshot
	active        map[string]bool
	sessions      map[string]*wireSession
	execution     chan struct{}
	closed        bool
}

// New 复制服务器配置并创建空目录，所有连接均由授权后的 Execute 发起。
func New(configuration config.MCPConfig, workspace tools.Workspace, command tools.ExecCommand, authDirectory string) (*Manager, error) {
	if err := config.ValidateMCPConfig(configuration); err != nil {
		return nil, err
	}
	configuration = configuration.Effective()
	manager := &Manager{configuration: configuration, workspace: workspace, exec: command, authDirectory: authDirectory, servers: make(map[string]config.MCPServerConfig), catalogs: make(map[string]Snapshot), active: make(map[string]bool), sessions: make(map[string]*wireSession), execution: make(chan struct{}, 1)}
	for _, server := range configuration.Servers {
		if !server.Disabled {
			manager.servers[server.Name] = server
		}
	}
	return manager, nil
}

// Close 回收当前 Boot 持有的所有 MCP 会话，不保存远端临时状态或自动重连。
func (manager *Manager) Close() error {
	manager.mu.Lock()
	manager.closed = true
	manager.mu.Unlock()
	_, err := manager.closeSessions()
	return err
}

// Specs 只声明发现/搜索入口及成功加载的工具，保持名称与顺序确定。
func (manager *Manager) Specs() []ports.ToolSpec {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if len(manager.servers) == 0 {
		return nil
	}
	specs := []ports.ToolSpec{
		{Name: DiscoverTool, Description: "Discover a configured MCP server before using its tools. This may start its approved local process. Configured servers: " + manager.serverSummary(), Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"required":["server"],"properties":{"server":{"type":"string"}}}`)},
		{Name: SearchTool, Description: "Search discovered MCP tools and load matching deferred tools for subsequent calls. Discover the server first if it is not in the catalog.", Parameters: json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["query"],"properties":{"query":{"type":"string","minLength":1},"server":{"type":"string"},"limit":{"type":"integer","minimum":1,"maximum":%d}}}`, manager.configuration.MaxSearchResults))},
	}
	names := make([]string, 0, len(manager.active))
	for name := range manager.active {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		_, definition, found := manager.lookup(name)
		if found {
			specs = append(specs, definition.declaration())
		}
	}
	return specs
}

// DeclarationFingerprint 将服务器与完整工具定义绑定为本次模型可见身份，不涉及凭据。
func (manager *Manager) DeclarationFingerprint(name string) (string, bool) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	server, definition, found := manager.lookup(name)
	if !found || !manager.active[name] {
		return "", false
	}
	digest, err := canonicaljson.Fingerprint("cairn-mcp-model-declaration-v1", struct {
		Identity   string
		Definition string
	}{manager.catalogs[server].Identity, definition.Digest})
	return digest, err == nil
}

// serverSummary 展开显式服务器的名称与简短用途，调用方持有目录读锁。
func (manager *Manager) serverSummary() string {
	names := make([]string, 0, len(manager.servers))
	for name := range manager.servers {
		names = append(names, name)
	}
	sort.Strings(names)
	rows := make([]string, 0, len(names))
	for _, name := range names {
		description := manager.servers[name].Description
		if len(description) > 240 {
			description = string([]rune(description)[:min(80, len([]rune(description)))])
		}
		rows = append(rows, name+" ("+description+")")
	}
	return strings.Join(rows, ", ")
}

// Proposal 只解析和绑定本地目录，不连接或刷新远端，也不信任工具注解。
func (manager *Manager) Proposal(name string, raw json.RawMessage) (tools.MCPProposal, bool, error) {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	if name == tools.MCPQuiesceTool {
		if string(bytes.TrimSpace(raw)) != "{}" {
			return tools.MCPProposal{}, true, errors.New("invalid MCP lifecycle arguments")
		}
		identity, err := canonicaljson.Fingerprint("cairn-mcp-close-v1", manager.catalogIdentities())
		return tools.MCPProposal{Identity: identity, ToolName: name, Arguments: raw, Effect: "allow", Local: true, Summary: "关闭本次运行持有的 MCP 会话"}, true, err
	}
	if name == DiscoverTool {
		var arguments struct {
			Server string `json:"server"`
		}
		if decode(raw, &arguments) != nil {
			return tools.MCPProposal{}, true, errors.New("invalid discovery arguments")
		}
		server, found := manager.servers[arguments.Server]
		if !found {
			return tools.MCPProposal{}, true, errors.New("unknown MCP server")
		}
		summary := "连接 MCP " + strconv.Quote(server.Name) + ": " + server.URL
		if server.Transport == config.MCPTransportStdio {
			argv, _ := json.Marshal(append([]string{server.Command}, server.Args...))
			summary = "启动 MCP " + strconv.Quote(server.Name) + ": argv=" + string(argv) + " cwd=" + strconv.Quote(server.CWD)
		}
		environmentDigest, err := manager.environmentDigest(server)
		if err != nil {
			return tools.MCPProposal{}, true, err
		}
		return tools.MCPProposal{Server: server.Name, Identity: server.Fingerprint(), EnvironmentDigest: environmentDigest, ToolName: name, Arguments: raw, Effect: server.ConnectPolicy, Stdio: server.Transport == config.MCPTransportStdio, Summary: summary}, true, nil
	}
	if name == SearchTool {
		var arguments searchArguments
		if decode(raw, &arguments) != nil || strings.TrimSpace(arguments.Query) == "" || arguments.Limit < 0 || arguments.Limit > manager.configuration.MaxSearchResults {
			return tools.MCPProposal{}, true, errors.New("invalid search arguments")
		}
		if arguments.Server != "" {
			if _, found := manager.servers[arguments.Server]; !found {
				return tools.MCPProposal{}, true, errors.New("unknown MCP server")
			}
		}
		identity, err := canonicaljson.Fingerprint("cairn-mcp-search-v1", manager.catalogIdentities())
		return tools.MCPProposal{Identity: identity, ToolName: name, Arguments: raw, Effect: "allow", Local: true, Summary: "search audited MCP tool catalog"}, true, err
	}
	serverName, definition, found := manager.lookup(name)
	if !found || !manager.active[name] {
		if strings.HasPrefix(name, "mcp__") {
			return tools.MCPProposal{}, true, errors.New("MCP tool is not loaded")
		}
		return tools.MCPProposal{}, false, nil
	}
	if !json.Valid(raw) || len(bytes.TrimSpace(raw)) == 0 || bytes.TrimSpace(raw)[0] != '{' {
		return tools.MCPProposal{}, true, errors.New("MCP arguments must be an object")
	}
	server := manager.servers[serverName]
	environmentDigest, err := manager.environmentDigest(server)
	if err != nil {
		return tools.MCPProposal{}, true, err
	}
	if live := manager.sessions[server.Name]; live != nil && live.credentialDigest() != environmentDigest {
		return tools.MCPProposal{}, true, errors.New("MCP live session identity changed; discover the server again")
	}
	effect := server.CallPolicy
	connectRequired := manager.sessions[server.Name] == nil
	if connectRequired {
		effect = combinedPolicy(effect, server.ConnectPolicy)
	}
	return tools.MCPProposal{Server: server.Name, Identity: server.Fingerprint(), EnvironmentDigest: environmentDigest, CatalogDigest: manager.catalogs[serverName].Digest, ToolDigest: definition.Digest, ToolName: name, Arguments: raw, Effect: effect, Stdio: server.Transport == config.MCPTransportStdio, Summary: "调用 MCP " + strconv.Quote(server.Name) + "/" + strconv.Quote(definition.OriginalName)}, true, nil
}

// environmentDigest 绑定实际环境或 HTTP 认证摘要；只读取本机事实，恢复审批检测身份漂移。
func (manager *Manager) environmentDigest(server config.MCPServerConfig) (string, error) {
	if server.Transport != config.MCPTransportStdio {
		return readAuthenticationBinding(manager.authDirectory, server).digest()
	}
	values := make(map[string]string)
	for key := range manager.exec.EnvAllow {
		if value, found := os.LookupEnv(key); found {
			values[key] = value
		}
	}
	for key, source := range server.Env {
		value, found := os.LookupEnv(source)
		if !found {
			return "", errors.New("MCP environment reference is unavailable")
		}
		values[key] = value
	}
	return canonicaljson.Fingerprint("cairn-mcp-environment-v1", values)
}

// catalogIdentities 对搜索所依据的已审计目录生成确定性的身份列表，调用方持锁。
func (manager *Manager) catalogIdentities() []string {
	identities := make([]string, 0, len(manager.catalogs))
	for name, snapshot := range manager.catalogs {
		identities = append(identities, name+":"+snapshot.Digest)
	}
	sort.Strings(identities)
	return identities
}

// SideEffect 把连接/外部调用保守归为进程副作用；纯目录搜索不会发出请求。
func (manager *Manager) SideEffect(name string) tools.SideEffectClass {
	if name == DiscoverTool || name == tools.MCPQuiesceTool || strings.HasPrefix(name, "mcp__") {
		return tools.SideEffectProcess
	}
	return tools.SideEffectNone
}

// Execute 使用已通过 Policy 的调用参数执行一次操作，工具调用不自动重试。
func (manager *Manager) Execute(ctx context.Context, name string, raw json.RawMessage) tools.Result {
	return manager.ExecuteWithIdentity(ctx, name, raw, "")
}

// ExecuteWithIdentity 将授权时的认证摘要冻结到本次执行，发现和业务调用均不能采纳替换身份。
func (manager *Manager) ExecuteWithIdentity(ctx context.Context, name string, raw json.RawMessage, identity string) tools.Result {
	select {
	case manager.execution <- struct{}{}:
		defer func() { <-manager.execution }()
	case <-ctx.Done():
		return tools.Failure("cancelled", "MCP operation was cancelled before execution")
	}
	proposal, found, err := manager.Proposal(name, raw)
	if err != nil || !found {
		return tools.Failure("invalid_mcp_call", "MCP tool is unavailable or its arguments are invalid")
	}
	if identity != "" && identity != proposal.EnvironmentDigest {
		return tools.Failure("mcp_authorization_changed", "MCP credentials changed after policy decision; obtain fresh authorization")
	}
	if name == SearchTool {
		return manager.search(raw)
	}
	if name == tools.MCPQuiesceTool {
		report, err := manager.closeSessions()
		if err != nil {
			return tools.Failure(tools.CodeTerminationUnconfirmed, "MCP session cleanup could not be confirmed; reconcile this operation")
		}
		return tools.Success(report, "MCP client sessions closed", 0, "", false)
	}
	manager.mu.RLock()
	server := manager.servers[proposal.Server]
	manager.mu.RUnlock()
	if name == DiscoverTool {
		return manager.discover(ctx, server, proposal.EnvironmentDigest)
	}
	return manager.call(ctx, server, name, raw, proposal.EnvironmentDigest)
}

// discover 在受审计操作内连接并读取有界目录；会话由 Boot 持有，目录需落盘后激活。
func (manager *Manager) discover(ctx context.Context, server config.MCPServerConfig, identity string) tools.Result {
	duration, _ := time.ParseDuration(server.ToolTimeout)
	operationContext, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	session, err := manager.session(operationContext, server, true, identity)
	if err != nil {
		return connectionFailure(err)
	}
	stopBudget := session.operationBudget(operationContext)
	defer stopBudget()
	snapshot, err := manager.readCatalog(operationContext, server, session)
	if err != nil {
		if manager.dropSession(server.Name, session) != nil {
			return tools.Failure(tools.CodeTerminationUnconfirmed, "MCP connection termination could not be confirmed")
		}
		return tools.Failure("mcp_discovery_failed", "MCP catalog could not be read within its limits")
	}
	return tools.Success(snapshot, fmt.Sprintf("discovered %d tools from MCP server %s", len(snapshot.Tools), server.Name), 0, snapshot.Digest, false)
}

// call 复用当前会话并检查完整目录身份后只发送一次 tools/call；未知结果保留对账边界。
func (manager *Manager) call(ctx context.Context, server config.MCPServerConfig, name string, raw json.RawMessage, identity string) tools.Result {
	manager.mu.RLock()
	serverName, expected, found := manager.lookup(name)
	expectedCatalog := manager.catalogs[serverName].Digest
	manager.mu.RUnlock()
	if !found {
		return tools.Failure("invalid_mcp_call", "MCP tool is not in the audited catalog")
	}
	duration, _ := time.ParseDuration(server.ToolTimeout)
	operationContext, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	session, err := manager.session(operationContext, server, false, identity)
	if err != nil {
		return connectionFailure(err)
	}
	stopBudget := session.operationBudget(operationContext)
	defer stopBudget()
	current, err := manager.readCatalog(operationContext, server, session)
	if err != nil {
		closeErr := manager.dropSession(server.Name, session)
		if closeErr != nil {
			return tools.Failure(tools.CodeTerminationUnconfirmed, "MCP connection termination could not be confirmed")
		}
		return tools.Failure("mcp_discovery_failed", "MCP preflight catalog could not be read")
	}
	matched := false
	for _, definition := range current.Tools {
		if definition.OriginalName == expected.OriginalName && definition.Digest == expected.Digest {
			matched = true
			break
		}
	}
	if !matched || current.Digest != expectedCatalog {
		closeErr := manager.dropSession(server.Name, session)
		if closeErr != nil {
			return tools.Failure(tools.CodeTerminationUnconfirmed, "MCP connection termination could not be confirmed")
		}
		return tools.Failure("mcp_catalog_changed", "MCP tool definition changed; discover the server again before calling it")
	}
	result, err := session.call(operationContext, expected.OriginalName, raw)
	if err != nil {
		_ = manager.dropSession(server.Name, session)
		return tools.Failure(tools.CodeTerminationUnconfirmed, "MCP tool outcome is uncertain; reconcile this operation before continuing")
	}
	return result
}

// AcceptResult 验证成功且已落盘的发现/搜索证据，恢复可见性而不重发请求。
func (manager *Manager) AcceptResult(name string, result tools.Result) error {
	if !result.OK || result.Code != "ok" || name != DiscoverTool && name != SearchTool {
		return nil
	}
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if name == DiscoverTool {
		var snapshot Snapshot
		if decode(result.Data, &snapshot) != nil {
			return errors.New("invalid persisted MCP catalog")
		}
		server, found := manager.servers[snapshot.Server]
		if !found || snapshot.Version != catalogVersion || snapshot.Identity != server.Fingerprint() {
			return errors.New("persisted MCP catalog identity does not match configuration")
		}
		digest, err := snapshotDigest(snapshot)
		if err != nil || digest != snapshot.Digest {
			return errors.New("persisted MCP catalog digest is invalid")
		}
		if err := manager.validateCatalog(snapshot); err != nil {
			return err
		}
		if previous, found := manager.catalogs[snapshot.Server]; found {
			for _, definition := range previous.Tools {
				delete(manager.active, definition.Name)
			}
		}
		manager.catalogs[snapshot.Server] = snapshot
		for _, definition := range snapshot.Tools {
			if toolExposure(server, definition.OriginalName) == config.MCPExposureDirect {
				manager.active[definition.Name] = true
			}
		}
		return nil
	}
	var exposure Exposure
	if decode(result.Data, &exposure) != nil || exposure.Version != catalogVersion || len(exposure.Loaded) > manager.configuration.MaxSearchResults {
		return errors.New("invalid persisted MCP exposure")
	}
	for _, loaded := range exposure.Loaded {
		snapshot, found := manager.catalogs[loaded.Server]
		server, definition, exists := manager.lookup(loaded.Tool.Name)
		if !found || !exists || server != loaded.Server || snapshot.Digest != loaded.CatalogDigest || definition.Digest != loaded.Tool.Digest || toolExposure(manager.servers[server], definition.OriginalName) == config.MCPExposureHidden {
			return errors.New("persisted MCP exposure does not match its audited catalog")
		}
	}
	for _, loaded := range exposure.Loaded {
		manager.active[loaded.Tool.Name] = true
	}
	return nil
}

// validateCatalog 验证每个源名称、稳定别名和定义摘要，并限制跨服务器目录总量。
func (manager *Manager) validateCatalog(snapshot Snapshot) error {
	count := len(snapshot.Tools)
	for name, prior := range manager.catalogs {
		if name != snapshot.Server {
			count += len(prior.Tools)
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || len(encoded) > manager.configuration.MaxCatalogBytes || count > manager.configuration.MaxTools {
		return errors.New("persisted MCP catalog exceeds configured limits")
	}
	names := make(map[string]bool)
	for _, definition := range snapshot.Tools {
		digest, err := definitionDigest(definition)
		schema, schemaErr := decodeSchemaDocument(definition.InputSchemaBase64)
		if err != nil || schemaErr != nil || len(schema) == 0 || schema[0] != '{' || !validOptionalSchemaDocument(definition.OutputSchemaBase64) || !validOptionalSchemaDocument(definition.HintsBase64) || digest != definition.Digest || !validSourceDigest(definition.SourceDigest) || definition.OriginalName == "" || definition.Name != toolAlias(snapshot.Server, definition.OriginalName) || names[definition.Name] || !allowedTool(manager.servers[snapshot.Server], definition.OriginalName) {
			return errors.New("persisted MCP tool definition is invalid")
		}
		names[definition.Name] = true
	}
	return nil
}

// lookup 在目录中查询别名，调用方必须持有读锁或写锁。
func (manager *Manager) lookup(name string) (string, Definition, bool) {
	for server, snapshot := range manager.catalogs {
		for _, definition := range snapshot.Tools {
			if definition.Name == name {
				return server, definition, true
			}
		}
	}
	return "", Definition{}, false
}

// ModelResult 使发现目录的完整 schema 保留在日志中，模型只看到摘要与可搜索提示。
func (manager *Manager) ModelResult(name string, result tools.Result) tools.Result {
	if name == SearchTool && result.OK && result.Code == "ok" {
		var exposure Exposure
		if json.Unmarshal(result.Data, &exposure) != nil {
			return result
		}
		type matchSummary struct {
			Server      string `json:"server"`
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		matches := make([]matchSummary, 0, len(exposure.Loaded))
		for _, loaded := range exposure.Loaded {
			matches = append(matches, matchSummary{Server: loaded.Server, Name: loaded.Tool.Name, Description: loaded.Tool.Description})
		}
		data, err := json.Marshal(struct {
			Matches    []matchSummary `json:"matches"`
			NextAction string         `json:"next_action"`
		}{matches, "matching tools are declared in the next model request"})
		if err == nil {
			result.Data = data
		}
		return result
	}
	if name != DiscoverTool || !result.OK {
		return result
	}
	var snapshot Snapshot
	if json.Unmarshal(result.Data, &snapshot) != nil {
		return result
	}
	data, err := json.Marshal(struct {
		Server     string `json:"server"`
		Tools      int    `json:"tools"`
		Digest     string `json:"catalog_digest"`
		NextAction string `json:"next_action"`
	}{snapshot.Server, len(snapshot.Tools), snapshot.Digest, "direct tools are declared in the next request; use tool_search to find deferred tools"})
	if err == nil {
		result.Data = data
	}
	return result
}

// searchArguments 限定纯目录搜索的输入，不能携带外部调用或任意脚本。
type searchArguments struct {
	Query  string `json:"query"`
	Server string `json:"server,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// rankedTool 保存确定性的匹配分数与目录身份。
type rankedTool struct {
	LoadedTool
	score int
}

// search 按名称、服务器用途和描述匹配延迟工具，返回待落盘的加载事实。
func (manager *Manager) search(raw json.RawMessage) tools.Result {
	var arguments searchArguments
	if decode(raw, &arguments) != nil {
		return tools.Failure("invalid_mcp_search", "invalid tool search")
	}
	limit := arguments.Limit
	if limit == 0 {
		limit = manager.configuration.MaxSearchResults
	}
	terms := strings.FieldsFunc(strings.ToLower(arguments.Query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '-' })
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	matches := make([]rankedTool, 0)
	for server, snapshot := range manager.catalogs {
		if arguments.Server != "" && arguments.Server != server {
			continue
		}
		for _, definition := range snapshot.Tools {
			if toolExposure(manager.servers[server], definition.OriginalName) == config.MCPExposureHidden {
				continue
			}
			name := strings.ToLower(definition.OriginalName + " " + definition.Name)
			text := strings.ToLower(definition.Description + " " + manager.servers[server].Description)
			score := 0
			for _, term := range terms {
				if strings.Contains(name, term) {
					score += 4
				}
				if strings.Contains(text, term) {
					score++
				}
			}
			if score > 0 {
				matches = append(matches, rankedTool{LoadedTool: LoadedTool{Server: server, CatalogDigest: snapshot.Digest, Tool: definition}, score: score})
			}
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].Tool.Name < matches[j].Tool.Name
	})
	exposure := Exposure{Version: catalogVersion, Loaded: make([]LoadedTool, 0, min(limit, len(matches)))}
	for _, match := range matches[:min(limit, len(matches))] {
		exposure.Loaded = append(exposure.Loaded, match.LoadedTool)
	}
	return tools.Success(exposure, fmt.Sprintf("loaded %d matching MCP tools", len(exposure.Loaded)), 0, "", false)
}

// allowedTool 应用显式允许/拒绝列表，拒绝列表优先。
func allowedTool(server config.MCPServerConfig, name string) bool {
	allowed := len(server.AllowTools) == 0
	for _, pattern := range server.AllowTools {
		match, _ := path.Match(pattern, name)
		allowed = allowed || match
	}
	for _, pattern := range server.DenyTools {
		match, _ := path.Match(pattern, name)
		if match {
			return false
		}
	}
	return allowed
}

// toolExposure 优先应用源工具名的显式覆盖，否则使用服务器模式。
func toolExposure(server config.MCPServerConfig, name string) string {
	if value, found := server.ToolExposure[name]; found {
		return value
	}
	return server.Exposure
}

// toolAlias 使用源名称哈希稳定隔离碰撞，限制模型接口可接受的名称长度。
func toolAlias(server, original string) string {
	var normalized strings.Builder
	for _, r := range original {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' {
			normalized.WriteRune(r)
		} else {
			normalized.WriteByte('_')
		}
	}
	sum := sha256.Sum256([]byte(original))
	suffix := "_" + hex.EncodeToString(sum[:4])
	prefix := "mcp__" + strings.ReplaceAll(server, "-", "_") + "__"
	name := normalized.String()
	length := maxToolAliasLength - len(prefix) - len(suffix)
	if len(name) > length {
		name = name[:length]
	}
	return prefix + name + suffix
}

// definitionDigest 对源名称、声明和提示字段生成独立的定义身份。
func definitionDigest(definition Definition) (string, error) {
	definition.Digest = ""
	return canonicaljson.Fingerprint("cairn-mcp-tool-v1", definition)
}

// snapshotDigest 对整份目录与服务器身份生成冻结摘要。
func snapshotDigest(snapshot Snapshot) (string, error) {
	snapshot.Digest = ""
	return canonicaljson.Fingerprint("cairn-mcp-catalog-v1", snapshot)
}

// decode 严格读取协议投影或辅助工具输入，拒绝未知字段和多份 JSON。
func decode(raw json.RawMessage, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

// declaration 将目录中的不透明 schema 文档显式转换为模型工具声明。
func (definition Definition) declaration() ports.ToolSpec {
	schema, _ := decodeSchemaDocument(definition.InputSchemaBase64)
	return ports.ToolSpec{Name: definition.Name, Description: definition.Description, Parameters: schema}
}

// decodeSchemaDocument 解码规范 Base64 中的 JSON 文档；目录接纳时验证，模型声明时复用。
func decodeSchemaDocument(encoded string) (json.RawMessage, error) {
	document, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || base64.StdEncoding.EncodeToString(document) != encoded || !json.Valid(document) {
		return nil, errors.New("invalid encoded MCP schema document")
	}
	return bytes.TrimSpace(document), nil
}

// validOptionalSchemaDocument 允许缺省、对象或 null 的输出 schema 与协议注解。
func validOptionalSchemaDocument(encoded string) bool {
	if encoded == "" {
		return true
	}
	document, err := decodeSchemaDocument(encoded)
	return err == nil && (document[0] == '{' || string(document) == "null")
}

// ResetProjection 丢弃派生的目录与暴露状态；外部配置和凭据身份保持不变。
func (manager *Manager) ResetProjection() {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	manager.catalogs = make(map[string]Snapshot)
	manager.active = make(map[string]bool)
}

// session 仅在授权后的操作内建立连接，后续操作复用同一个 Boot 的远端会话。
func (manager *Manager) session(ctx context.Context, server config.MCPServerConfig, rediscover bool, authorizedIdentity string) (*wireSession, error) {
	manager.mu.RLock()
	existing, closed := manager.sessions[server.Name], manager.closed
	manager.mu.RUnlock()
	if closed {
		return nil, errors.New("MCP manager is closed")
	}
	currentIdentity, err := manager.environmentDigest(server)
	if err != nil {
		return nil, err
	}
	if authorizedIdentity != "" && currentIdentity != authorizedIdentity {
		return nil, ErrAuthRequired
	}
	if existing != nil {
		if existing.credentialDigest() == currentIdentity {
			return existing, nil
		}
		if !rediscover {
			return nil, errors.New("MCP session credentials changed; discover the server again")
		}
		if manager.dropSession(server.Name, existing) != nil {
			return nil, errTerminationUnconfirmed
		}
	}
	connected, err := manager.connect(ctx, server, currentIdentity)
	if err != nil {
		return nil, err
	}
	manager.mu.Lock()
	if manager.closed {
		manager.mu.Unlock()
		_ = connected.close()
		return nil, errors.New("MCP manager is closed")
	}
	connected.initialIdentity = currentIdentity
	manager.sessions[server.Name] = connected
	manager.mu.Unlock()
	return connected, nil
}

// dropSession 移除失效连接并确认清理；调用失败不触发自动重连或重试。
func (manager *Manager) dropSession(name string, session *wireSession) error {
	manager.mu.Lock()
	if manager.sessions[name] == session {
		delete(manager.sessions, name)
	}
	manager.mu.Unlock()
	return session.close()
}

// combinedPolicy 为冷连接合并连接与调用权限，任何禁令优先，其次为审批。
func combinedPolicy(left, right string) string {
	if left == "deny" || right == "deny" {
		return "deny"
	}
	if left == "ask" || right == "ask" {
		return "ask"
	}
	return "allow"
}

// NeedsQuiescence 只查询受本次 Boot 管理的活会话数量，便于审批暂停和验证前收束。
func (manager *Manager) NeedsQuiescence() bool {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return len(manager.sessions) > 0
}

// sessionCleanupReport 区分已关闭的客户端会话与服务器不支持释放的远端会话。
type sessionCleanupReport struct {
	ClientSessionsClosed     int `json:"client_sessions_closed"`
	RemoteReleaseUnsupported int `json:"remote_release_unsupported"`
}

// sessionCloseOutcome 把并行关闭的确认结果交回串行聚合器，不携带服务器内容。
type sessionCloseOutcome struct {
	err                error
	releaseUnsupported bool
}

// closeSessions 回收外部资源并报告释放能力；保留目录，重连仍需独立权限判定。
func (manager *Manager) closeSessions() (sessionCleanupReport, error) {
	manager.mu.Lock()
	sessions := manager.sessions
	manager.sessions = make(map[string]*wireSession)
	manager.mu.Unlock()
	outcomes := make(chan sessionCloseOutcome, len(sessions))
	for _, session := range sessions {
		go func(current *wireSession) {
			outcome := sessionCloseOutcome{err: current.close()}
			if current.httpTransport != nil {
				current.httpTransport.cleanupMu.RLock()
				outcome.releaseUnsupported = current.httpTransport.remoteReleaseUnsupported
				current.httpTransport.cleanupMu.RUnlock()
			}
			outcomes <- outcome
		}(session)
	}
	var report sessionCleanupReport
	var failures []error
	for range sessions {
		outcome := <-outcomes
		if outcome.err != nil {
			failures = append(failures, outcome.err)
			continue
		}
		report.ClientSessionsClosed++
		if outcome.releaseUnsupported {
			report.RemoteReleaseUnsupported++
		}
	}
	return report, errors.Join(failures...)
}

// validSourceDigest 校验原始定义摘要的完整格式，不能用缺失或截断摘要替代远端身份。
func validSourceDigest(value string) bool {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value[len(prefix):])
	return err == nil && len(decoded) == sha256.Size
}
