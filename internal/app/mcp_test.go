// [INPUT]: Boot、MCP 配置与本地 HTTP 请求计数器。
// [OUTPUT]: 验证惰性构造及请求作用域配对。
// [POS]: 应用层外部能力装配回归。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package app

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/core/agent"
	"github.com/fxbin/cairn/internal/ports"
)

// TestMCPBootAndStatusAreLazy 构造与查询不能连接远端或提前暴露未发现工具。
func TestMCPBootAndStatusAreLazy(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	configuration := withTestProvider(config.Defaults())
	configuration.MCP = &config.MCPConfig{Servers: []config.MCPServerConfig{{Name: "docs", Transport: config.MCPTransportHTTP, URL: server.URL, Exposure: config.MCPExposureDirect}}}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	boot, err := service.assembleBoot(t.TempDir(), t.TempDir(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	if len(boot.Loop.ResultObservers) != 1 || !boot.Loop.ResultObservers[0].HandlesToolResult("mcp_discover") {
		t.Fatal("MCP historical result projection was not explicitly assembled")
	}
	if boot.MCP == nil {
		t.Fatal("MCP was not explicitly assembled")
	}
	if _, scoped := boot.Loop.Gate.(agent.RequestScope); !scoped || !boot.Registry.RequiresRequestScope() {
		t.Fatal("MCP authorizer lost its paired request scope")
	}
	specs := boot.Registry.Specs()
	found := false
	for _, spec := range specs {
		if spec.Name == "mcp_discover" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing discovery helper")
	}
	statuses, err := service.MCPStatus(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 0 || len(statuses) != 1 || statuses[0].Name != "docs" || statuses[0].CredentialsPresent {
		t.Fatalf("boot/status issued a connection or misrepresented credentials: %d %#v", requests.Load(), statuses)
	}
}
