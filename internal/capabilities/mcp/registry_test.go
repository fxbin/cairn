// [INPUT]: 真实固定注册表与只记录本地动作的目录替身。
// [OUTPUT]: 验证模型投影优先级、名称边界和同源目录状态。
// [POS]: MCP 提供器组合回归，基础 Runtime 不持有协议夹具。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package mcp_test

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/capabilities/mcp"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

type plainTool struct{ name string }

func (tool plainTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: tool.name, Parameters: json.RawMessage(`{"type":"object"}`)}
}
func (tool plainTool) Execute(context.Context, json.RawMessage) contract.ToolResult {
	return contract.ToolFailure("native", "native execution")
}

type projectedTool struct {
	plainTool
	calls int
}

func (tool *projectedTool) ModelResult(result contract.ToolResult) contract.ToolResult {
	tool.calls++
	result.Preview = "native-model-view"
	return result
}

type localCatalog struct {
	mcp.Catalog
	active bool
	calls  []string
	bound  string
}

func (catalog *localCatalog) Specs() []ports.ToolSpec {
	specs := []ports.ToolSpec{{Name: "mcp_discover", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if catalog.active {
		specs = append(specs, ports.ToolSpec{Name: "mcp__docs__lookup", Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	return specs
}
func (catalog *localCatalog) ModelResult(name string, result contract.ToolResult) contract.ToolResult {
	catalog.calls = append(catalog.calls, name)
	result.Preview = "catalog-model-view"
	return result
}
func (catalog *localCatalog) Execute(_ context.Context, name string, _ json.RawMessage) contract.ToolResult {
	return contract.ToolFailure("catalog", name)
}
func (catalog *localCatalog) ExecuteWithIdentity(_ context.Context, name string, _ json.RawMessage, binding string) contract.ToolResult {
	catalog.bound = binding
	return contract.ToolFailure("bound", name)
}
func (catalog *localCatalog) SideEffect(string) contract.SideEffectClass {
	return contract.SideEffectProcess
}
func (catalog *localCatalog) AcceptResult(_ string, result contract.ToolResult) error {
	if result.OK {
		catalog.active = true
	}
	return nil
}
func (catalog *localCatalog) ResetProjection()      { catalog.active = false }
func (catalog *localCatalog) NeedsQuiescence() bool { return catalog.active }
func (catalog *localCatalog) DeclarationFingerprint(name string) (string, bool) {
	return "schema-v1", catalog.active && name == "mcp__docs__lookup"
}

// TestRegistryModelResultUsesRegisteredToolAndPreservesMCPFallback 保留迁移前模型视图的完整优先级与证据断言。
func TestRegistryModelResultUsesRegisteredToolAndPreservesMCPFallback(t *testing.T) {
	projected := &projectedTool{plainTool: plainTool{name: "native-projection"}}
	static, err := tools.NewRegistry(projected, plainTool{name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &localCatalog{}
	registry, err := mcp.NewRegistry(static, catalog)
	if err != nil {
		t.Fatal(err)
	}
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
	for _, name := range []string{"plain", "unknown"} {
		if got := static.ModelResult(name, original); !reflect.DeepEqual(got, original) {
			t.Fatalf("unprojected result changed for %s", name)
		}
	}
}

// TestRegistryRejectsMissingSourcesAndCollisions 在组合时拒绝缺失来源及固定名称冲突。
func TestRegistryRejectsMissingSourcesAndCollisions(t *testing.T) {
	empty, err := tools.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mcp.NewRegistry(nil, &localCatalog{}); err == nil {
		t.Fatal("missing static source accepted")
	}
	if _, err := mcp.NewRegistry(empty, nil); err == nil {
		t.Fatal("missing catalog accepted")
	}
	for _, name := range []string{"mcp__collision", contract.MCPQuiesceTool, "mcp_discover"} {
		static, err := tools.NewRegistry(plainTool{name: name})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := mcp.NewRegistry(static, &localCatalog{}); err == nil {
			t.Fatalf("collision accepted: %s", name)
		}
	}
}

// TestRegistrySharesCatalogStateWithoutParallelAdmission 保留同一目录的激活、重置、身份和释放元数据。
func TestRegistrySharesCatalogStateWithoutParallelAdmission(t *testing.T) {
	static, err := tools.NewRegistry(plainTool{name: "plain"})
	if err != nil {
		t.Fatal(err)
	}
	catalog := &localCatalog{}
	registry, err := mcp.NewRegistry(static, catalog)
	if err != nil {
		t.Fatal(err)
	}
	if !registry.RequiresRequestScope() || !registry.RequiresAuthorizedExecution() || !reflect.DeepEqual(registry.RequiredLifecycleTools(), []string{contract.MCPQuiesceTool}) {
		t.Fatal("capability wiring requirements lost")
	}
	for _, name := range []string{"mcp_discover", "tool_search", "mcp__unknown", contract.MCPQuiesceTool} {
		if !registry.RequiresResultObserver(name) {
			t.Fatalf("namespace lost observer requirement: %s", name)
		}
	}
	if registry.RequiresResultObserver("unknown") || registry.RequiresResultObserver("plain") {
		t.Fatal("ordinary tool requires capability projection")
	}
	before, err := registry.Fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := canonicaljson.Fingerprint("cairn-tool-registry-v1", struct {
		Tools []ports.ToolSpec `json:"tools"`
	}{Tools: registry.Specs()})
	if err != nil || expected != before {
		t.Fatal("frozen registry fingerprint domain changed")
	}
	receipt := contract.ToolSuccess(struct{}{}, "accepted", 0, "", false)
	if err := registry.AcceptMCPResult("mcp_discover", receipt); err != nil {
		t.Fatal(err)
	}
	if !catalog.active || !registry.NeedsQuiescence() || !reflect.DeepEqual(registry.Names(), []string{"plain", "mcp_discover", "mcp__docs__lookup"}) {
		t.Fatal("same catalog not activated")
	}
	if _, found := registry.MCPDeclarationFingerprint("mcp__docs__lookup"); !found {
		t.Fatal("activated declaration lost")
	}
	after, err := registry.Fingerprint()
	if err != nil || after == before {
		t.Fatal("activated tools do not change frozen identity")
	}
	if !reflect.DeepEqual(registry.ReadonlyToolNames(), map[string]bool{"plain": true}) {
		t.Fatal("dynamic catalog admitted into parallel readonly set")
	}
	if got := registry.Execute(context.Background(), "plain", json.RawMessage(`{}`)); got.Code != "native" {
		t.Fatal("native execution bypassed")
	}
	if got := registry.Execute(context.Background(), "mcp__docs__lookup", json.RawMessage(`{}`)); got.Code != "catalog" {
		t.Fatal("catalog execution bypassed")
	}
	if got := registry.ExecuteWithBinding(context.Background(), "mcp__docs__lookup", json.RawMessage(`{}`), "authorized-binding"); got.Code != "bound" || catalog.bound != "authorized-binding" {
		t.Fatal("execution binding lost")
	}
	registry.ResetMCPProjection()
	reset, err := registry.Fingerprint()
	if err != nil || reset != before || catalog.active || registry.NeedsQuiescence() {
		t.Fatal("replay reset did not restore initial catalog view")
	}
}
