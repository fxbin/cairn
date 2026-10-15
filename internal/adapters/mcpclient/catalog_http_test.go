package mcpclient

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	journalmemory "github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// rpcFixture 在真实 loopback HTTP 上提供可变目录与可观察的单次请求合同。
type rpcFixture struct {
	mu              sync.Mutex
	server          *httptest.Server
	tools           []*mcp.Tool
	methods         []string
	callMode        string
	callResult      json.RawMessage
	nextCursor      string
	arguments       json.RawMessage
	headers         []http.Header
	beforeListReply func()
	deleteStatus    int
}

func newRPCFixture(t *testing.T, definitions ...*mcp.Tool) *rpcFixture {
	t.Helper()
	fixture := &rpcFixture{tools: definitions, callResult: json.RawMessage(`{"content":[{"type":"text","text":"工具已完成"}],"structuredContent":{"count":1}}`)}
	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.serveHTTP))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (fixture *rpcFixture) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodDelete {
		fixture.mu.Lock()
		fixture.methods = append(fixture.methods, "http/DELETE")
		status := fixture.deleteStatus
		fixture.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		writer.WriteHeader(status)
		_, _ = fmt.Fprint(writer, "opaque-delete-response")
		return
	}
	if request.Method != http.MethodPost {
		writer.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var message struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if json.NewDecoder(request.Body).Decode(&message) != nil || message.JSONRPC != "2.0" {
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	fixture.mu.Lock()
	fixture.methods = append(fixture.methods, message.Method)
	fixture.headers = append(fixture.headers, request.Header.Clone())
	definitions := append([]*mcp.Tool(nil), fixture.tools...)
	mode, result, cursor := fixture.callMode, append(json.RawMessage(nil), fixture.callResult...), fixture.nextCursor
	beforeListReply := fixture.beforeListReply
	if message.Method == "tools/call" {
		fixture.arguments = append(json.RawMessage(nil), message.Params...)
	}
	fixture.mu.Unlock()
	if len(message.ID) == 0 {
		writer.WriteHeader(http.StatusAccepted)
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	switch message.Method {
	case "initialize":
		var parameters struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(message.Params, &parameters)
		writer.Header().Set("Mcp-Session-Id", "offline-session")
		result, _ = json.Marshal(struct {
			ProtocolVersion string              `json:"protocolVersion"`
			Capabilities    json.RawMessage     `json:"capabilities"`
			ServerInfo      *mcp.Implementation `json:"serverInfo"`
		}{parameters.ProtocolVersion, json.RawMessage(`{"tools":{}}`), &mcp.Implementation{Name: "离线合同服务器", Version: "1"}})
	case "tools/list":
		if beforeListReply != nil {
			beforeListReply()
		}
		result, _ = json.Marshal(struct {
			Tools      []*mcp.Tool `json:"tools"`
			NextCursor string      `json:"nextCursor,omitempty"`
		}{definitions, cursor})
	case "tools/call":
		switch mode {
		case "disconnect":
			connection, _, err := writer.(http.Hijacker).Hijack()
			if err == nil {
				_ = connection.Close()
			}
			return
		case "unavailable":
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		case "oversized":
			result, _ = json.Marshal(&mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", 2048)}}})
		}
	default:
		writer.WriteHeader(http.StatusBadRequest)
		return
	}
	_ = json.NewEncoder(writer).Encode(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  json.RawMessage `json:"result"`
	}{"2.0", message.ID, result})
}

func (fixture *rpcFixture) count(method string) int {
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	count := 0
	for _, received := range fixture.methods {
		if received == method {
			count++
		}
	}
	return count
}

func contractTool(name, description string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: description, InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)}
}

func contractManager(t *testing.T, server config.MCPServerConfig, maxResponseBytes int) *Manager {
	t.Helper()
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	configuration := config.MCPConfig{Servers: []config.MCPServerConfig{server}, MaxResponseBytes: maxResponseBytes}
	manager, err := New(configuration, workspace, tools.ExecCommand{Workspace: workspace, Backend: tools.ExecBackendHost, Grace: 50 * time.Millisecond}, filepath.Join(t.TempDir(), "auth"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func contractHTTPServer(fixture *rpcFixture) config.MCPServerConfig {
	return config.MCPServerConfig{Name: "offline", Transport: config.MCPTransportHTTP, URL: fixture.server.URL, StartupTimeout: "2s", ToolTimeout: "2s"}
}

func discoverContract(t *testing.T, manager *Manager) (tools.Result, Snapshot) {
	t.Helper()
	result := manager.Execute(t.Context(), DiscoverTool, json.RawMessage(`{"server":"offline"}`))
	if !result.OK || result.Code != "ok" {
		t.Fatalf("发现失败：%#v", result)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(result.Data, &snapshot); err != nil {
		t.Fatal(err)
	}
	return result, snapshot
}

func definitionNamed(t *testing.T, snapshot Snapshot, original string) Definition {
	t.Helper()
	for _, definition := range snapshot.Tools {
		if definition.OriginalName == original {
			return definition
		}
	}
	t.Fatalf("目录中缺少工具 %q", original)
	return Definition{}
}

func hasSpec(specs []ports.ToolSpec, name string) bool {
	for _, spec := range specs {
		if spec.Name == name {
			return true
		}
	}
	return false
}

func parametersNamed(t *testing.T, specs []ports.ToolSpec, name string) json.RawMessage {
	t.Helper()
	for _, spec := range specs {
		if spec.Name == name {
			return spec.Parameters
		}
	}
	t.Fatalf("模型声明缺少工具：%s", name)
	return nil
}

func validateCredentialNamedSchema(raw json.RawMessage) error {
	var schema struct {
		Type       string `json:"type"`
		Properties struct {
			APIKey struct {
				Type string `json:"type"`
			} `json:"api_key"`
		} `json:"properties"`
		Examples []struct {
			AccessToken string `json:"access_token"`
		} `json:"examples"`
	}
	if !json.Valid(raw) {
		return errors.New("模型工具 schema 不是有效 JSON")
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		return fmt.Errorf("凭据名称属性被破坏为非 schema 对象：%w", err)
	}
	if schema.Type != "object" || schema.Properties.APIKey.Type != "string" || len(schema.Examples) != 1 || schema.Examples[0].AccessToken == "" {
		return errors.New("schema 属性定义或示例结构未保留")
	}
	return nil
}

// schemaLoopProvider 只替代模型响应，发现、策略、传输与 Journal 全部经过真实生产边界。
type schemaLoopProvider struct {
	requests []ports.ModelRequest
	alias    string
}

func (provider *schemaLoopProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests = append(provider.requests, request)
	var call ports.ToolCall
	switch len(provider.requests) {
	case 1:
		call = ports.ToolCall{ID: "discover_schema", Name: DiscoverTool, Arguments: json.RawMessage(`{"server":"offline"}`)}
	case 2:
		for _, spec := range request.Tools {
			if strings.HasPrefix(spec.Name, "mcp__") {
				if err := validateCredentialNamedSchema(spec.Parameters); err != nil {
					return err
				}
				provider.alias = spec.Name
				break
			}
		}
		if provider.alias == "" {
			return errors.New("发现结果落盘后没有可供模型调用的工具")
		}
		call = ports.ToolCall{ID: "call_schema", Name: provider.alias, Arguments: json.RawMessage(`{}`)}
	case 3:
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "schema flow complete"}); err != nil {
			return err
		}
		return emit(ports.ModelEvent{Kind: ports.Done})
	default:
		return errors.New("schema 回归出现了额外模型轮次")
	}
	if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done})
}

func TestSchemaSurvivesRealLoopJournalPersistenceReplayAndMCPCall(t *testing.T) {
	const schemaDocument = `{"type":"object","properties":{"api_key":{"type":"string","description":"普通 JSON schema 属性定义"}},"examples":[{"access_token":"example-value"}]}`
	definition := &mcp.Tool{Name: "schema_probe", Description: "schema 持久化回归", InputSchema: json.RawMessage(schemaDocument), OutputSchema: json.RawMessage(schemaDocument)}
	fixture := newRPCFixture(t, definition)
	server := contractHTTPServer(fixture)
	server.Exposure, server.ConnectPolicy, server.CallPolicy = config.MCPExposureDirect, "allow", "allow"
	manager := contractManager(t, server, 0)
	registry, err := tools.NewRegistryWithMCP(manager)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: manager.workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate.MCPRegistry = registry
	journal := journalmemory.New()
	input := agent.RunInput{RunID: domain.NewRunID(), Task: "验证 MCP schema 的持久化与模型声明", WorkspaceHash: "schema-workspace"}
	created, err := json.Marshal(domain.RunCreatedPayload{Task: input.Task, Workspace: manager.workspace.Root})
	if err != nil {
		t.Fatal(err)
	}
	for index, initial := range []struct {
		kind    domain.EventType
		payload json.RawMessage
	}{{domain.EventRunCreated, created}, {domain.EventRunStarted, nil}} {
		draft := domain.EventDraft{EventID: fmt.Sprintf("evt_schema_boot_%d", index), RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, OccurredAt: time.Date(2026, 1, 1, 0, 0, index, 0, time.UTC), Type: initial.kind, Payload: initial.payload}
		if _, err := journal.Append(t.Context(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
	provider := &schemaLoopProvider{}
	loop := agent.Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "offline-schema-provider", MaxTurns: 4, MaxToolCalls: 4, VerificationWorkspace: manager.workspace.ObservationView(nil)}
	result, err := loop.Run(t.Context(), input)
	if err != nil || result.Response != "schema flow complete" || fixture.count("tools/call") != 1 {
		t.Fatalf("schema 没有通过真实运行链并完成业务调用：response=%s err=%v calls=%d", result.Response, err, fixture.count("tools/call"))
	}
	if len(provider.requests) != 3 || provider.alias == "" {
		t.Fatal("模型没有在发现后的下一轮接收并使用工具声明")
	}
	if err := validateCredentialNamedSchema(parametersNamed(t, provider.requests[1].Tools, provider.alias)); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(t.Context(), input.RunID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunCompleted || projection.PendingOperation != nil {
		t.Fatalf("完整 Journal 无法回放收束：state=%s err=%v", projection.State, err)
	}
	names := make(map[domain.OperationID]string)
	var persisted tools.Result
	businessSucceeded := false
	for _, event := range events {
		if event.OperationID == nil {
			continue
		}
		switch event.Type {
		case domain.EventToolCallProposed:
			var payload struct {
				ToolName string `json:"tool_name"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				t.Fatal(err)
			}
			names[*event.OperationID] = payload.ToolName
		case domain.EventToolResultAppended:
			if names[*event.OperationID] == DiscoverTool {
				if err := json.Unmarshal(event.Payload, &persisted); err != nil {
					t.Fatal(err)
				}
			}
		case domain.EventOperationSucceeded:
			if names[*event.OperationID] == provider.alias {
				var payload struct {
					Result tools.Result `json:"result"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					t.Fatal(err)
				}
				businessSucceeded = payload.Result.OK && payload.Result.Code == "ok"
			}
		}
	}
	if !persisted.OK || !businessSucceeded {
		t.Fatal("Journal 没有成功发现与业务调用的真实回执")
	}
	cold := contractManager(t, server, 0)
	connections := fixture.count("initialize")
	if err := cold.AcceptResult(DiscoverTool, persisted); err != nil {
		t.Fatalf("冷目录无法接受实际 Journal 中的发现结果：%v", err)
	}
	if fixture.count("initialize") != connections {
		t.Fatal("冷目录接受持久化结果时重发了发现")
	}
	if err := validateCredentialNamedSchema(parametersNamed(t, cold.Specs(), provider.alias)); err != nil {
		t.Fatal(err)
	}
	var frozen struct {
		Tools []struct {
			OriginalName       string `json:"original_name"`
			OutputSchemaBase64 string `json:"output_schema_base64"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(persisted.Data, &frozen); err != nil {
		t.Fatal(err)
	}
	encodedOutput := ""
	for _, frozenTool := range frozen.Tools {
		if frozenTool.OriginalName == "schema_probe" {
			encodedOutput = frozenTool.OutputSchemaBase64
		}
	}
	output, err := base64.StdEncoding.DecodeString(encodedOutput)
	if err != nil {
		t.Fatal(err)
	}
	if err := validateCredentialNamedSchema(output); err != nil {
		t.Fatalf("输出 schema 经过 Journal 后结构被破坏：%v", err)
	}
}

func TestHTTPInteroperatesWithOfficialSDKStatelessServer(t *testing.T) {
	serverSDK := mcp.NewServer(&mcp.Implementation{Name: "official-offline", Version: "1"}, nil)
	serverSDK.AddTool(contractTool("echo", "官方 SDK 工具"), func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments struct {
			Value string `json:"value"`
		}
		if err := json.Unmarshal(request.Params.Arguments, &arguments); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: arguments.Value}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return serverSDK }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()
	server := config.MCPServerConfig{Name: "offline", Transport: config.MCPTransportHTTP, URL: endpoint.URL, Exposure: config.MCPExposureDirect, StartupTimeout: "2s", ToolTimeout: "2s"}
	manager := contractManager(t, server, 0)
	defer manager.Close()
	discovery, snapshot := discoverContract(t, manager)
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "echo").Name, json.RawMessage(`{"value":"SDK 互操作"}`))
	if !result.OK || result.Preview != "SDK 互操作" {
		t.Fatalf("官方 SDK Stateless 互操作失败：%#v", result)
	}
}

func TestHTTPStatefulServerKeepsOneSessionForRepeatedCalls(t *testing.T) {
	serverSDK := mcp.NewServer(&mcp.Implementation{Name: "stateful-offline", Version: "1"}, nil)
	var mu sync.Mutex
	counts := make(map[*mcp.ServerSession]int)
	serverSDK.AddTool(contractTool("counter", "会话计数"), func(_ context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if request.Session == nil {
			return nil, errors.New("有状态 MCP 会话缺失")
		}
		mu.Lock()
		counts[request.Session]++
		count := counts[request.Session]
		mu.Unlock()
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprint(count)}}}, nil
	})
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return serverSDK }, &mcp.StreamableHTTPOptions{JSONResponse: true})
	endpoint := httptest.NewServer(handler)
	defer endpoint.Close()
	server := config.MCPServerConfig{Name: "offline", Transport: config.MCPTransportHTTP, URL: endpoint.URL, Exposure: config.MCPExposureDirect, StartupTimeout: "2s", ToolTimeout: "2s"}
	manager := contractManager(t, server, 0)
	defer manager.Close()
	discovery, snapshot := discoverContract(t, manager)
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	for count := 1; count <= 2; count++ {
		result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "counter").Name, json.RawMessage(`{}`))
		if !result.OK || result.Preview != fmt.Sprint(count) {
			t.Fatalf("远端会话状态未保留：第 %d 次调用 %#v", count, result)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(counts) != 1 {
		t.Fatalf("一次 Manager 生命周期产生多个业务会话：%d", len(counts))
	}
}

func TestHTTPDeleteFailureCannotBeReportedAsSuccessfulSessionClose(t *testing.T) {
	fixture := newRPCFixture(t, contractTool("echo", "HTTP 清理失败测试"))
	fixture.deleteStatus = http.StatusInternalServerError
	manager := contractManager(t, contractHTTPServer(fixture), 0)
	discoverContract(t, manager)
	err := manager.Close()
	if err == nil || fixture.count("http/DELETE") != 1 {
		t.Fatalf("远端 DELETE 失败被报告为成功关闭：err=%v delete=%d", err, fixture.count("http/DELETE"))
	}
	if strings.Contains(err.Error(), "opaque-delete-response") || strings.Contains(err.Error(), fixture.server.URL) {
		t.Fatal("关闭错误包含远端正文或地址细节")
	}
}

func TestHTTPDeleteUnsupportedReportsLocalClosureAndUnsupportedRemoteRelease(t *testing.T) {
	fixture := newRPCFixture(t, contractTool("echo", "HTTP 本地关闭测试"))
	fixture.deleteStatus = http.StatusMethodNotAllowed
	manager := contractManager(t, contractHTTPServer(fixture), 0)
	discoverContract(t, manager)
	result := manager.Execute(t.Context(), tools.MCPQuiesceTool, json.RawMessage(`{}`))
	if !result.OK || result.Code != "ok" || fixture.count("http/DELETE") != 1 || manager.NeedsQuiescence() {
		t.Fatalf("拒绝 DELETE 后未完成本地清理：result=%#v delete=%d", result, fixture.count("http/DELETE"))
	}
	var report struct {
		ClientSessionsClosed     int `json:"client_sessions_closed"`
		RemoteReleaseUnsupported int `json:"remote_release_unsupported"`
	}
	if err := json.Unmarshal(result.Data, &report); err != nil || report.ClientSessionsClosed != 1 || report.RemoteReleaseUnsupported != 1 {
		t.Fatalf("清理报告未区分本地关闭与远端不支持释放：%s err=%v", result.Data, err)
	}
}

func assertResultJournalContract(t *testing.T, result tools.Result) {
	t.Helper()
	turn, operation := domain.TurnID("turn_mcp_contract"), domain.OperationID("op_mcp_contract")
	projection := domain.RunProjection{ID: "run_mcp_contract", State: domain.RunRunning, CurrentTurn: &turn}
	terminal := domain.EventOperationSucceeded
	if !result.OK {
		terminal = domain.EventOperationFailed
	}
	terminalPayload, err := json.Marshal(struct {
		ToolName string       `json:"tool_name"`
		Result   tools.Result `json:"result"`
	}{"mcp_contract", result})
	if err != nil {
		t.Fatal(err)
	}
	resultPayload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		kind    domain.EventType
		payload json.RawMessage
	}{
		{domain.EventToolCallProposed, json.RawMessage(`{"call_id":"call_contract","tool_name":"mcp_contract","arguments":{}}`)},
		{domain.EventPolicyDecided, json.RawMessage(`{"effect":"allow","rule":"mcp_contract"}`)},
		{domain.EventOperationStarted, json.RawMessage(`{"tool_name":"mcp_contract","side_effect_class":"process"}`)},
		{terminal, terminalPayload},
		{domain.EventToolResultAppended, resultPayload},
	}
	for index, step := range steps {
		draft := domain.EventDraft{EventID: fmt.Sprintf("evt_mcp_contract_%d", index), RunID: projection.ID, OccurredAt: time.Date(2026, 1, 1, 0, 0, index, 0, time.UTC), Type: step.kind, TurnID: &turn, OperationID: &operation, Payload: step.payload}
		if err := projection.ValidateDraft(draft); err != nil {
			t.Fatalf("适配器结果无法按 Journal 合同收束 %s：%v", step.kind, err)
		}
		event, err := domain.NewEvent(draft, projection.LastSeq+1, domain.GenesisHash)
		if err != nil {
			t.Fatal(err)
		}
		if err := projection.Apply(event); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHTTPDiscoveryExposureRequiresAcceptedEvidenceAndReplaysLocally(t *testing.T) {
	fixture := newRPCFixture(t, contractTool("direct", "直接工具"), contractTool("deferred", "延迟工具"), contractTool("hidden", "隐藏工具"), contractTool("denied", "禁止工具"))
	server := contractHTTPServer(fixture)
	server.Exposure = config.MCPExposureDeferred
	server.ToolExposure = map[string]string{"direct": config.MCPExposureDirect, "hidden": config.MCPExposureHidden}
	server.DenyTools = []string{"denied"}
	manager := contractManager(t, server, 0)
	if len(manager.Specs()) != 2 || fixture.count("initialize") != 0 {
		t.Fatal("构造与声明工具不应主动连接")
	}
	if _, found, err := manager.Proposal(DiscoverTool, json.RawMessage(`{"server":"offline"}`)); !found || err != nil || fixture.count("initialize") != 0 {
		t.Fatalf("发现提议应保持本地：found=%v err=%v", found, err)
	}
	discovery, snapshot := discoverContract(t, manager)
	if len(snapshot.Tools) != 3 || len(manager.Specs()) != 2 {
		t.Fatal("发现结果未接受前不得激活，拒绝工具不应进入目录")
	}
	direct := definitionNamed(t, snapshot, "direct")
	deferred := definitionNamed(t, snapshot, "deferred")
	hidden := definitionNamed(t, snapshot, "hidden")
	if _, _, err := manager.Proposal(direct.Name, json.RawMessage(`{}`)); err == nil {
		t.Fatal("未接受目录的工具可被提议")
	}
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	if !hasSpec(manager.Specs(), direct.Name) || hasSpec(manager.Specs(), deferred.Name) || hasSpec(manager.Specs(), hidden.Name) {
		t.Fatal("direct/deferred/hidden 暴露模式未被遵守")
	}
	search := manager.Execute(t.Context(), SearchTool, json.RawMessage(`{"query":"工具","limit":8}`))
	if !search.OK || hasSpec(manager.Specs(), deferred.Name) {
		t.Fatal("搜索未接受前不得激活工具")
	}
	var exposure Exposure
	if err := json.Unmarshal(search.Data, &exposure); err != nil || len(exposure.Loaded) != 2 {
		t.Fatalf("搜索结果包含隐藏工具或遗漏可见工具：%v %s", err, search.Data)
	}
	modelDiscovery, modelSearch := manager.ModelResult(DiscoverTool, discovery), manager.ModelResult(SearchTool, search)
	if strings.Contains(string(modelDiscovery.Data), "input_schema") || strings.Contains(string(modelSearch.Data), "input_schema") {
		t.Fatal("模型摘要包含完整目录 schema")
	}
	if err := manager.AcceptResult(SearchTool, search); err != nil || !hasSpec(manager.Specs(), deferred.Name) {
		t.Fatalf("搜索证据未激活 deferred 工具：%v", err)
	}
	requests := fixture.count("initialize")
	replayed := contractManager(t, server, 0)
	if err := replayed.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	if err := replayed.AcceptResult(SearchTool, search); err != nil {
		t.Fatal(err)
	}
	if fixture.count("initialize") != requests || !hasSpec(replayed.Specs(), deferred.Name) || hasSpec(replayed.Specs(), hidden.Name) {
		t.Fatal("恢复应只投影已接受事实，无需重连")
	}
	fixture.mu.Lock()
	defer fixture.mu.Unlock()
	initialization := -1
	for index, method := range fixture.methods {
		if method == "initialize" {
			initialization = index
			break
		}
	}
	if initialization < 0 || len(fixture.methods) < initialization+3 || strings.Join(fixture.methods[initialization:initialization+3], ",") != "initialize,notifications/initialized,tools/list" || fixture.headers[initialization+2].Get("Mcp-Session-Id") != "offline-session" || fixture.headers[initialization+2].Get("Mcp-Protocol-Version") == "" {
		t.Fatalf("HTTP 握手或会话头不完整：%v", fixture.methods)
	}
}

func TestHTTPBearerEchoIsRedactedFromCatalogResultAndPreview(t *testing.T) {
	const secret = "irregularOfflineCredentialValue"
	t.Setenv("CAIRN_MCP_TEST_BEARER", secret)
	fixture := newRPCFixture(t, contractTool("echo", "描述回显 "+secret))
	fixture.callResult = json.RawMessage(`{"content":[{"type":"text","text":"` + secret + `"}],"structuredContent":{"echo":"` + secret + `"}}`)
	server := contractHTTPServer(fixture)
	server.BearerTokenEnv = "CAIRN_MCP_TEST_BEARER"
	server.Exposure = config.MCPExposureDirect
	manager := contractManager(t, server, 0)
	discovery, snapshot := discoverContract(t, manager)
	if strings.Contains(string(discovery.Data), secret) || strings.Contains(discovery.Preview, secret) {
		t.Fatal("Bearer 在目录或摘要中泄漏")
	}
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "echo").Name, json.RawMessage(`{}`))
	if !result.OK || strings.Contains(string(result.Data), secret) || strings.Contains(result.Preview, secret) {
		t.Fatal("无特征 Bearer 回显泄漏到结果或摘要")
	}
}

func TestBearerProposalBindsRealTokenValueWithoutConnectingOrExposingIt(t *testing.T) {
	const original, replacement = "firstOpaqueBearerValue", "secondOpaqueBearerValue"
	t.Setenv("CAIRN_MCP_TEST_BEARER", original)
	fixture := newRPCFixture(t, contractTool("echo", "本机身份测试"))
	server := contractHTTPServer(fixture)
	server.BearerTokenEnv = "CAIRN_MCP_TEST_BEARER"
	manager := contractManager(t, server, 0)
	before, found, err := manager.Proposal(DiscoverTool, json.RawMessage(`{"server":"offline"}`))
	if err != nil || !found || before.EnvironmentDigest == "" {
		t.Fatalf("首次提议未绑定本机认证身份：found=%v err=%v", found, err)
	}
	t.Setenv("CAIRN_MCP_TEST_BEARER", replacement)
	after, found, err := manager.Proposal(DiscoverTool, json.RawMessage(`{"server":"offline"}`))
	if err != nil || !found || after.EnvironmentDigest == before.EnvironmentDigest || after.Identity != before.Identity {
		t.Fatalf("同名环境变量的真实 Bearer 变化未影响审批摘要：found=%v err=%v", found, err)
	}
	for _, proposal := range []tools.MCPProposal{before, after} {
		encoded, err := json.Marshal(proposal)
		if err != nil || strings.Contains(string(encoded), original) || strings.Contains(string(encoded), replacement) {
			t.Fatal("认证审批提议泄漏实际 Bearer 值")
		}
	}
	if fixture.count("server/discover") != 0 || fixture.count("initialize") != 0 {
		t.Fatal("本机审批身份读取主动连接了 MCP")
	}
}

func changingAuthenticationContract(t *testing.T, kind string) (*Manager, *rpcFixture, func() error) {
	t.Helper()
	fixture := newRPCFixture(t, contractTool("echo", "认证竞态工具"))
	server := contractHTTPServer(fixture)
	server.Exposure = config.MCPExposureDirect
	if kind == "bearer" {
		t.Setenv("CAIRN_MCP_RACE_BEARER", "opaqueApprovedBearerA")
		server.BearerTokenEnv = "CAIRN_MCP_RACE_BEARER"
		manager := contractManager(t, server, 0)
		return manager, fixture, func() error { return os.Setenv("CAIRN_MCP_RACE_BEARER", "opaqueReplacedBearerB") }
	}
	if kind != "oauth" {
		t.Fatalf("未知认证测试模式：%s", kind)
	}
	server.OAuth = &config.MCPOAuthConfig{ClientID: "offline-client"}
	manager := contractManager(t, server, 0)
	record := oauthRecord(server)
	if err := writeTokenRecord(manager.authDirectory, server, record); err != nil {
		t.Fatal(err)
	}
	record.Token.AccessToken, record.Token.RefreshToken = "opaqueReplacedAccessB", "opaqueReplacedRefreshB"
	return manager, fixture, func() error {
		lease, err := credentialLock(t.Context(), manager.authDirectory, server)
		if err != nil {
			return err
		}
		defer lease.Release()
		return writeTokenRecord(manager.authDirectory, server, record)
	}
}

func TestCredentialsReplacedDuringCatalogPreflightPreventBusinessCall(t *testing.T) {
	for _, kind := range []string{"bearer", "oauth"} {
		t.Run(kind, func(t *testing.T) {
			manager, fixture, replace := changingAuthenticationContract(t, kind)
			discovery, snapshot := discoverContract(t, manager)
			if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
				t.Fatal(err)
			}
			mutation := make(chan error, 1)
			fixture.mu.Lock()
			fixture.beforeListReply = func() { mutation <- replace() }
			fixture.mu.Unlock()
			result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "echo").Name, json.RawMessage(`{}`))
			select {
			case err := <-mutation:
				if err != nil {
					t.Fatalf("fixture 未完成认证来源替换：%v", err)
				}
			default:
				t.Fatal("测试未到达目录预检返回阶段")
			}
			if result.OK || fixture.count("tools/call") != 0 {
				t.Fatalf("目录返回期间替换的 %s 凭据被采纳到业务调用：result=%#v calls=%d", kind, result, fixture.count("tools/call"))
			}
		})
	}
}

func TestDiscoveryWithApprovedIdentityRejectsPostApprovalCredentialReplacement(t *testing.T) {
	for _, kind := range []string{"bearer", "oauth"} {
		t.Run(kind, func(t *testing.T) {
			manager, fixture, replace := changingAuthenticationContract(t, kind)
			arguments := json.RawMessage(`{"server":"offline"}`)
			approved, found, err := manager.Proposal(DiscoverTool, arguments)
			if err != nil || !found || approved.EnvironmentDigest == "" {
				t.Fatalf("未取得审批时的认证摘要：found=%v err=%v", found, err)
			}
			if err := replace(); err != nil {
				t.Fatal(err)
			}
			result := manager.ExecuteWithIdentity(t.Context(), DiscoverTool, arguments, approved.EnvironmentDigest)
			if result.OK || fixture.count("server/discover") != 0 || fixture.count("initialize") != 0 || fixture.count("tools/list") != 0 {
				t.Fatalf("首次连接采用了审批后的 %s 身份：%#v", kind, result)
			}
		})
	}
}

func TestHTTPTargetAndCatalogDriftFailBeforeBusinessCall(t *testing.T) {
	for _, change := range []string{"input_schema", "output_schema", "description", "removed", "other_tool"} {
		t.Run(change, func(t *testing.T) {
			fixture := newRPCFixture(t, contractTool("echo", "原声明"))
			server := contractHTTPServer(fixture)
			server.Exposure = config.MCPExposureDirect
			manager := contractManager(t, server, 0)
			discovery, snapshot := discoverContract(t, manager)
			if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			replacement := contractTool("echo", "原声明")
			switch change {
			case "input_schema":
				replacement.InputSchema = json.RawMessage(`{"type":"object","required":["value"]}`)
			case "output_schema":
				replacement.OutputSchema = json.RawMessage(`{"type":"object","properties":{"id":{"type":"integer"}}}`)
			case "description":
				replacement.Description = "已更换声明"
			}
			fixture.tools = []*mcp.Tool{replacement}
			if change == "removed" {
				fixture.tools = []*mcp.Tool{}
			} else if change == "other_tool" {
				fixture.tools = append(fixture.tools, contractTool("new_tool", "目录增量"))
			}
			fixture.mu.Unlock()
			result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "echo").Name, json.RawMessage(`{}`))
			if result.OK || result.Code != "mcp_catalog_changed" || fixture.count("tools/call") != 0 {
				t.Fatalf("漂移应关闭业务调用：result=%#v calls=%d", result, fixture.count("tools/call"))
			}
		})
	}
}

func TestHTTPCallPreservesStructuredErrorAndNeverRetriesUncertainOutcome(t *testing.T) {
	for _, mode := range []string{"success", "tool_error", "disconnect", "unavailable", "oversized", "input_request"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newRPCFixture(t, contractTool("echo", "调用工具"))
			server := contractHTTPServer(fixture)
			server.Exposure = config.MCPExposureDirect
			manager := contractManager(t, server, 1024)
			discovery, snapshot := discoverContract(t, manager)
			if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
				t.Fatal(err)
			}
			fixture.mu.Lock()
			fixture.callMode = mode
			if mode == "tool_error" {
				fixture.callResult = json.RawMessage(`{"content":[{"type":"text","text":"工具拒绝请求"}],"isError":true}`)
			} else if mode == "input_request" {
				fixture.callResult = json.RawMessage(`{"content":[],"requestState":"pending"}`)
			}
			fixture.mu.Unlock()
			result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "echo").Name, json.RawMessage(`{"value":"一次调用"}`))
			if fixture.count("tools/call") != 1 {
				t.Fatalf("业务请求被重试：%d", fixture.count("tools/call"))
			}
			switch mode {
			case "success":
				if !result.OK || result.Code != "ok" || !strings.Contains(string(result.Data), `"structuredContent":{"count":1}`) || !strings.Contains(result.Preview, "工具已完成") || result.Bytes != int64(len(result.Data)) {
					t.Fatalf("结构化结果合同丢失：%#v", result)
				}
				assertResultJournalContract(t, result)
			case "tool_error":
				if result.OK || result.Code != "mcp_tool_error" || !strings.Contains(string(result.Data), `"isError":true`) {
					t.Fatalf("服务端明确错误被当作不确定结果：%#v", result)
				}
				assertResultJournalContract(t, result)
			default:
				if result.OK || result.Code != tools.CodeTerminationUnconfirmed {
					t.Fatalf("未知业务结果必须保留对账边界：%#v", result)
				}
			}
			fixture.mu.Lock()
			defer fixture.mu.Unlock()
			if !strings.Contains(string(fixture.arguments), `"name":"echo"`) || !strings.Contains(string(fixture.arguments), `"value":"一次调用"`) {
				t.Fatalf("工具别名或 arguments 未按协议转换：%s", fixture.arguments)
			}
		})
	}
}

func TestHTTPDiscoveryRejectsUnboundedOrAmbiguousCatalog(t *testing.T) {
	for _, mode := range []string{"repeated_cursor", "duplicate_name", "tool_limit", "response_limit"} {
		t.Run(mode, func(t *testing.T) {
			fixture := newRPCFixture(t, contractTool("echo", "目录工具"))
			configuration := config.MCPConfig{Servers: []config.MCPServerConfig{contractHTTPServer(fixture)}}
			switch mode {
			case "repeated_cursor":
				fixture.nextCursor = "same"
			case "duplicate_name":
				fixture.tools = append(fixture.tools, contractTool("echo", "重复名称"))
			case "tool_limit":
				configuration.MaxTools = 1
				fixture.tools = append(fixture.tools, contractTool("other", "第二工具"))
			case "response_limit":
				configuration.MaxResponseBytes = 1024
				fixture.tools = []*mcp.Tool{contractTool("echo", strings.Repeat("x", 2048))}
			}
			workspace, err := tools.NewWorkspace(t.TempDir(), nil, 0, 0, 0)
			if err != nil {
				t.Fatal(err)
			}
			manager, err := New(configuration, workspace, tools.ExecCommand{Workspace: workspace}, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = manager.Close() })
			result := manager.Execute(t.Context(), DiscoverTool, json.RawMessage(`{"server":"offline"}`))
			if result.OK || result.Code != "mcp_discovery_failed" || len(manager.Specs()) != 2 || fixture.count("tools/list") > 2 {
				t.Fatalf("目录异常未有界失败：%#v", result)
			}
		})
	}
}

func TestAcceptedEvidenceRejectsTamperingAndHiddenExposureAtomically(t *testing.T) {
	fixture := newRPCFixture(t, contractTool("deferred", "延迟工具"), contractTool("hidden", "隐藏工具"))
	server := contractHTTPServer(fixture)
	server.ToolExposure = map[string]string{"hidden": config.MCPExposureHidden}
	manager := contractManager(t, server, 0)
	discovery, snapshot := discoverContract(t, manager)
	corrupted := snapshot
	corrupted.Identity = strings.Repeat("0", 64)
	encoded, _ := json.Marshal(corrupted)
	if err := manager.AcceptResult(DiscoverTool, tools.Success(corrupted, "", 0, "", false)); err == nil || len(manager.Specs()) != 2 {
		t.Fatalf("不匹配身份的目录被接受：%s", encoded)
	}
	corrupted = snapshot
	corrupted.Digest = strings.Repeat("0", 64)
	if err := manager.AcceptResult(DiscoverTool, tools.Success(corrupted, "", 0, "", false)); err == nil {
		t.Fatal("篡改目录摘要被接受")
	}
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	deferred := definitionNamed(t, snapshot, "deferred")
	exposure := Exposure{Version: catalogVersion, Loaded: []LoadedTool{
		{Server: snapshot.Server, CatalogDigest: snapshot.Digest, Tool: deferred},
		{Server: snapshot.Server, CatalogDigest: snapshot.Digest, Tool: definitionNamed(t, snapshot, "hidden")},
	}}
	if err := manager.AcceptResult(SearchTool, tools.Success(exposure, "", 0, "", false)); err == nil || hasSpec(manager.Specs(), deferred.Name) {
		t.Fatal("非法加载证据导致部分激活")
	}
	if result := manager.Execute(context.Background(), "mcp__unknown", json.RawMessage(`{}`)); result.OK || result.Code != "invalid_mcp_call" || fixture.count("tools/call") != 0 {
		t.Fatalf("未知工具不应连接或调用：%s", fmt.Sprint(result))
	}
}
