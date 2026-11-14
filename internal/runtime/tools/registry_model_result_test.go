package tools

import (
	"reflect"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// nativeModelResultTool 是已静态注册且能投影结果的普通工具，不依赖任何能力包。
type nativeModelResultTool struct {
	staticSpecTool
	calls int
}

func (tool *nativeModelResultTool) ModelResult(result contract.ToolResult) contract.ToolResult {
	tool.calls++
	result.Preview = "native-model-view"
	return result
}

// modelResultCatalog 只观察本测试消费的目录结果方法，其他生命周期不被调用。
type modelResultCatalog struct {
	MCPCatalog
	calls []string
}

func (catalog *modelResultCatalog) ModelResult(name string, result contract.ToolResult) contract.ToolResult {
	catalog.calls = append(catalog.calls, name)
	result.Preview = "catalog-model-view"
	return result
}

// TestRegistryModelResultUsesRegisteredToolAndPreservesMCPFallback 已注册工具的
// 窄方法优先投影；未提供投影和未知名称保持原 MCP fallback 或原始结果行为。
func TestRegistryModelResultUsesRegisteredToolAndPreservesMCPFallback(t *testing.T) {
	projected := &nativeModelResultTool{staticSpecTool: staticSpecTool{spec: ports.ToolSpec{Name: "native-projection"}}}
	registry, err := NewRegistry(projected, staticSpecTool{spec: ports.ToolSpec{Name: "plain"}})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &modelResultCatalog{}
	registry.mcp = catalog
	original := contract.ToolSuccess(struct {
		Body string `json:"body"`
	}{Body: "complete journal evidence"}, "original", 25, "identity", false)
	got := registry.ModelResult("native-projection", original)
	if got.Preview != "native-model-view" || projected.calls != 1 || len(catalog.calls) != 0 {
		t.Fatalf("native projection bypassed: result=%+v calls=%d catalog=%v", got, projected.calls, catalog.calls)
	}
	expected := original
	expected.Preview = "native-model-view"
	if !reflect.DeepEqual(got, expected) || original.Preview != "original" {
		t.Fatal("model projection changed full receipt fields")
	}
	for _, name := range []string{"plain", "unknown"} {
		if got := registry.ModelResult(name, original); got.Preview != "catalog-model-view" {
			t.Fatalf("MCP fallback lost for %s: %+v", name, got)
		}
	}
	if !reflect.DeepEqual(catalog.calls, []string{"plain", "unknown"}) {
		t.Fatalf("unexpected MCP routing: %v", catalog.calls)
	}
	registry.mcp = nil
	for _, name := range []string{"plain", "unknown"} {
		if got := registry.ModelResult(name, original); !reflect.DeepEqual(got, original) {
			t.Fatalf("unprojected result changed for %s", name)
		}
	}
}
