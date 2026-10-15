//go:build darwin || linux

package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type helperObservation struct {
	Stage                    string `json:"stage"`
	PID                      int    `json:"pid"`
	CWD                      string `json:"cwd"`
	Mapped                   string `json:"mapped"`
	Unallowlisted            string `json:"unallowlisted"`
	Calls                    int32  `json:"calls"`
	CredentialAliasesPresent bool   `json:"credential_aliases_present"`
}

// TestMCPStdioHelperProcess 只在显式映射的测试环境中作为真实子进程服务器运行。
func TestMCPStdioHelperProcess(t *testing.T) {
	mode := os.Getenv("CAIRN_MCP_HELPER")
	if mode == "" {
		return
	}
	var calls atomic.Int32
	observe := func(stage string) {
		filename := os.Getenv("MCP_OBSERVATIONS")
		file, err := os.OpenFile(filename, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			os.Exit(2)
		}
		cwd, _ := os.Getwd()
		aliasesPresent := os.Getenv("AUTH") != "" && os.Getenv("AUTHORIZATION") != "" && os.Getenv("DOCS_AUTH") != ""
		err = json.NewEncoder(file).Encode(helperObservation{stage, os.Getpid(), cwd, os.Getenv("MCP_TEST_VALUE"), os.Getenv("CAIRN_MCP_UNALLOWLISTED"), calls.Load(), aliasesPresent})
		closeErr := file.Close()
		if err != nil || closeErr != nil {
			os.Exit(2)
		}
	}
	observe("started")
	if mode == "stall" {
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGTERM)
		scanner := bufio.NewScanner(os.Stdin)
		listings := 0
		for scanner.Scan() {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(scanner.Bytes(), &request) != nil || len(request.ID) == 0 {
				continue
			}
			var result json.RawMessage
			switch request.Method {
			case "server/discover":
				_, _ = fmt.Fprintf(os.Stdout, "{\"jsonrpc\":\"2.0\",\"id\":%s,\"error\":{\"code\":-32601,\"message\":\"unsupported\"}}\n", request.ID)
				continue
			case "initialize":
				var parameters struct {
					Protocol string `json:"protocolVersion"`
				}
				_ = json.Unmarshal(request.Params, &parameters)
				result = json.RawMessage(fmt.Sprintf(`{"protocolVersion":%q,"capabilities":{"tools":{}},"serverInfo":{"name":"stall-helper","version":"1"}}`, parameters.Protocol))
			case "tools/list":
				listings++
				result = json.RawMessage(`{"tools":[{"name":"echo","description":"进程调用","inputSchema":{"type":"object"}}]}`)
			default:
				os.Exit(2)
			}
			response, _ := json.Marshal(struct {
				JSONRPC string          `json:"jsonrpc"`
				ID      json.RawMessage `json:"id"`
				Result  json.RawMessage `json:"result"`
			}{"2.0", request.ID, result})
			_, _ = fmt.Fprintln(os.Stdout, string(response))
			if request.Method == "tools/list" && listings == 2 {
				// 此后不读取 stdin，确保大参数请求真正阻塞在 pipe 写入阶段。
				select {
				case <-signals:
				case <-time.After(10 * time.Second):
				}
				observe("stopped")
				os.Exit(0)
			}
		}
	} else {
		server := mcp.NewServer(&mcp.Implementation{Name: "stdio-helper", Version: "1"}, nil)
		definition := contractTool("echo", "进程调用")
		if mode == "credential" {
			definition.Description = "授权回显 " + strings.Join([]string{os.Getenv("AUTH"), os.Getenv("AUTHORIZATION"), os.Getenv("DOCS_AUTH")}, " ") + " 日志级别 " + os.Getenv("MCP_LOG_LEVEL")
			definition.InputSchema = json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"level":{"type":"string","enum":[%q]}}}`, os.Getenv("MCP_LOG_LEVEL")))
		}
		server.AddTool(definition, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if mode == "credential" {
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Join([]string{os.Getenv("AUTH"), os.Getenv("AUTHORIZATION"), os.Getenv("DOCS_AUTH")}, " ") + " level=" + os.Getenv("MCP_LOG_LEVEL")}}}, nil
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("helper complete:%d", calls.Add(1))}}}, nil
		})
		_ = server.Run(context.Background(), &mcp.IOTransport{Reader: os.Stdin, Writer: os.Stdout})
	}
	observe("stopped")
	os.Exit(0)
}

func stdioContractManager(t *testing.T, mode, timeout string, additionalEnvironment ...map[string]string) (*Manager, string) {
	t.Helper()
	return stdioContractManagerWithInherited(t, mode, timeout, nil, additionalEnvironment...)
}

func stdioContractManagerWithInherited(t *testing.T, mode, timeout string, inherited map[string]struct{}, additionalEnvironment ...map[string]string) (*Manager, string) {
	t.Helper()
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	observations := filepath.Join(workspace.Root, "observations.jsonl")
	t.Setenv("CAIRN_MCP_HELPER_MODE", mode)
	t.Setenv("CAIRN_MCP_TEST_LOG_PATH", observations)
	t.Setenv("CAIRN_MCP_TEST_VALUE_SOURCE", "mapped-value")
	t.Setenv("CAIRN_MCP_UNALLOWLISTED", "must-not-inherit")
	server := config.MCPServerConfig{Name: "offline", Transport: config.MCPTransportStdio, Command: executable, Args: []string{"-test.run=^TestMCPStdioHelperProcess$"}, CWD: ".", Exposure: config.MCPExposureDirect, StartupTimeout: "2s", ToolTimeout: timeout, Env: map[string]string{
		"CAIRN_MCP_HELPER": "CAIRN_MCP_HELPER_MODE", "MCP_OBSERVATIONS": "CAIRN_MCP_TEST_LOG_PATH", "MCP_TEST_VALUE": "CAIRN_MCP_TEST_VALUE_SOURCE",
	}}
	for _, environment := range additionalEnvironment {
		for key, source := range environment {
			server.Env[key] = source
		}
	}
	manager, err := New(config.MCPConfig{Servers: []config.MCPServerConfig{server}}, workspace, tools.ExecCommand{Workspace: workspace, EnvAllow: inherited, Backend: tools.ExecBackendHost, Grace: 50 * time.Millisecond}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager, observations
}

func readObservations(t *testing.T, filename string) []helperObservation {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var observations []helperObservation
	decoder := json.NewDecoder(file)
	for {
		var observation helperObservation
		if err := decoder.Decode(&observation); errors.Is(err, io.EOF) {
			return observations
		} else if err != nil {
			t.Fatal(err)
		}
		observations = append(observations, observation)
	}
}

func TestStdioHandshakeCallUsesMappedEnvironmentAndReapsProcess(t *testing.T) {
	manager, filename := stdioContractManager(t, "normal", "2s")
	discovery, snapshot := discoverContract(t, manager)
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	for count := 1; count <= 2; count++ {
		result := manager.Execute(t.Context(), definitionNamed(t, snapshot, "echo").Name, json.RawMessage(`{}`))
		if !result.OK || result.Preview != fmt.Sprintf("helper complete:%d", count) {
			t.Fatalf("stdio 会话计数丢失：%#v", result)
		}
	}
	if observations := readObservations(t, filename); len(observations) != 1 || observations[0].Stage != "started" {
		t.Fatalf("完成单次调用后关闭了会话：%#v", observations)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	observations := readObservations(t, filename)
	if len(observations) != 2 {
		t.Fatalf("Manager 关闭未回收唯一子进程：%#v", observations)
	}
	for index, observation := range observations {
		stage := "started"
		if index%2 != 0 {
			stage = "stopped"
		}
		if observation.Stage != stage || observation.CWD != filepath.Dir(filename) || observation.Mapped != "mapped-value" || observation.Unallowlisted != "" {
			t.Fatalf("stdio 进程环境或生命周期不满足合同：%#v", observation)
		}
		if err := syscall.Kill(observation.PID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("stdio 子进程仍存活：pid=%d err=%v", observation.PID, err)
		}
	}
}

func TestStdioBlockedWriteIsCancelledAndProcessIsReaped(t *testing.T) {
	manager, filename := stdioContractManager(t, "stall", "300ms")
	discovery, snapshot := discoverContract(t, manager)
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	arguments, err := json.Marshal(struct {
		Value string `json:"value"`
	}{strings.Repeat("x", 2*1024*1024)})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	done := make(chan tools.Result, 1)
	alias := definitionNamed(t, snapshot, "echo").Name
	go func() { done <- manager.Execute(t.Context(), alias, arguments) }()
	select {
	case result := <-done:
		if result.OK || result.Code != tools.CodeTerminationUnconfirmed || time.Since(started) > 2*time.Second {
			t.Fatalf("阻塞写入未保留有界对账失败：%#v", result)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("大参数写入不响应操作预算")
	}
	observations := readObservations(t, filename)
	if len(observations) != 2 {
		t.Fatalf("阻塞写入子进程未清理：%#v", observations)
	}
	for _, observation := range observations {
		if err := syscall.Kill(observation.PID, 0); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("取消后 stdio 子进程仍存活：pid=%d err=%v", observation.PID, err)
		}
	}
}

func TestCachedStdioEnvironmentDriftFailsClosedUntilExplicitRediscovery(t *testing.T) {
	manager, filename := stdioContractManager(t, "normal", "2s")
	discovery, snapshot := discoverContract(t, manager)
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	alias := definitionNamed(t, snapshot, "echo").Name
	result := manager.Execute(t.Context(), alias, json.RawMessage(`{}`))
	if !result.OK || result.Preview != "helper complete:1" {
		t.Fatalf("环境漂移前的基线调用失败：%#v", result)
	}
	t.Setenv("CAIRN_MCP_TEST_VALUE_SOURCE", "new-mapped-value")
	for range 2 {
		result = manager.Execute(t.Context(), alias, json.RawMessage(`{}`))
		if result.OK || result.Code == tools.CodeTerminationUnconfirmed {
			t.Fatalf("本机环境漂移没有在业务调用前明确失败：%#v", result)
		}
		observations := readObservations(t, filename)
		if len(observations) < 1 || len(observations) > 2 || observations[0].Stage != "started" || len(observations) == 2 && (observations[1].Stage != "stopped" || observations[1].Calls != 1) {
			t.Fatalf("环境漂移隐式启动了新进程或发送了业务调用：%#v", observations)
		}
	}
	rediscovered, fresh := discoverContract(t, manager)
	if err := manager.AcceptResult(DiscoverTool, rediscovered); err != nil {
		t.Fatal(err)
	}
	result = manager.Execute(t.Context(), definitionNamed(t, fresh, "echo").Name, json.RawMessage(`{}`))
	if !result.OK || result.Preview != "helper complete:1" {
		t.Fatalf("显式发现未建立新环境下的新会话：%#v", result)
	}
	if err := manager.Close(); err != nil {
		t.Fatal(err)
	}
	observations := readObservations(t, filename)
	if len(observations) != 4 || observations[1].Stage != "stopped" || observations[1].Calls != 1 || observations[2].Mapped != "new-mapped-value" || observations[3].Stage != "stopped" || observations[3].Calls != 1 {
		t.Fatalf("重新发现未使用新环境、拒绝调用被发送、或退出未清理：%#v", observations)
	}
}

func TestStdioOpaqueAUTHIsRedactedWithoutChangingOrdinaryLogLevelSchema(t *testing.T) {
	const secret = "unstructuredPrivateGrantValue"
	t.Setenv("DOCS_AUTH", secret)
	t.Setenv("CAIRN_MCP_LOG_LEVEL_SOURCE", "info")
	manager, _ := stdioContractManager(t, "credential", "2s", map[string]string{"AUTH": "DOCS_AUTH", "MCP_LOG_LEVEL": "CAIRN_MCP_LOG_LEVEL_SOURCE"})
	discovery, snapshot := discoverContract(t, manager)
	if strings.Contains(string(discovery.Data), secret) || strings.Contains(discovery.Preview, secret) {
		t.Fatal("AUTH 精确凭据回显泄漏到目录结果")
	}
	definition := definitionNamed(t, snapshot, "echo")
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties struct {
			Level struct {
				Enum []string `json:"enum"`
			} `json:"level"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(parametersNamed(t, manager.Specs(), definition.Name), &schema); err != nil || len(schema.Properties.Level.Enum) != 1 || schema.Properties.Level.Enum[0] != "info" || !strings.Contains(definition.Description, "info") {
		t.Fatalf("普通 MCP_LOG_LEVEL 被当成凭据修改了工具 schema：%v", err)
	}
	result := manager.Execute(t.Context(), definition.Name, json.RawMessage(`{}`))
	if !result.OK || strings.Contains(string(result.Data), secret) || strings.Contains(result.Preview, secret) || !strings.Contains(result.Preview, "level=info") {
		t.Fatal("AUTH 未被精确脱敏，或普通环境值被误改")
	}
}

func TestStdioInheritedCredentialAliasesAreRedactedWithoutExplicitServerMappings(t *testing.T) {
	credentials := map[string]string{"AUTH": "inheritedOpaqueAlphaValue", "AUTHORIZATION": "inheritedOpaqueBetaValue", "DOCS_AUTH": "inheritedOpaqueGammaValue"}
	allowed := map[string]struct{}{"MCP_LOG_LEVEL": {}}
	for name, value := range credentials {
		t.Setenv(name, value)
		allowed[name] = struct{}{}
	}
	t.Setenv("MCP_LOG_LEVEL", "info")
	manager, filename := stdioContractManagerWithInherited(t, "credential", "2s", allowed)
	for name := range credentials {
		if _, explicitlyMapped := manager.servers["offline"].Env[name]; explicitlyMapped {
			t.Fatal("继承环境测试不应将凭据复制为服务器显式映射")
		}
	}
	discovery, snapshot := discoverContract(t, manager)
	observations := readObservations(t, filename)
	if len(observations) != 1 || !observations[0].CredentialAliasesPresent {
		t.Fatal("fixture 未实际继承全部授权别名，无法证明回显脱敏")
	}
	definition := definitionNamed(t, snapshot, "echo")
	if err := manager.AcceptResult(DiscoverTool, discovery); err != nil {
		t.Fatal(err)
	}
	for _, value := range credentials {
		if strings.Contains(string(discovery.Data), value) || strings.Contains(discovery.Preview, value) || strings.Contains(definition.Description, value) {
			t.Fatal("全局允许列表继承的 opaque 凭据在目录中泄漏")
		}
	}
	var schema struct {
		Properties struct {
			Level struct {
				Enum []string `json:"enum"`
			} `json:"level"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(parametersNamed(t, manager.Specs(), definition.Name), &schema); err != nil || len(schema.Properties.Level.Enum) != 1 || schema.Properties.Level.Enum[0] != "info" {
		t.Fatalf("普通继承环境变量损坏 schema：%v", err)
	}
	result := manager.Execute(t.Context(), definition.Name, json.RawMessage(`{}`))
	if !result.OK || !strings.Contains(result.Preview, "level=info") {
		t.Fatal("继承环境下的调用失败，或普通日志级别被误改")
	}
	for _, value := range credentials {
		if strings.Contains(string(result.Data), value) || strings.Contains(result.Preview, value) {
			t.Fatal("继承的 opaque 凭据在调用结果或摘要中泄漏")
		}
	}
}
