package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/ports"
)

// staticSpecTool 是返回固定规格的测试替身工具，执行路径不在本测试覆盖面。
type staticSpecTool struct {
	spec ports.ToolSpec
}

// Spec 返回构造时固化的工具定义。
func (tool staticSpecTool) Spec() ports.ToolSpec { return tool.spec }

// Execute 返回未使用占位结果。
func (tool staticSpecTool) Execute(_ context.Context, _ json.RawMessage) Result {
	return Failure("unused", "")
}

// TestRegistryFingerprintDeterministicAndSensitive 锁定注册表指纹合同：同一
// 注册序列恒同指纹，注册顺序、名称或参数 schema 任一变化都改变身份。
func TestRegistryFingerprintDeterministicAndSensitive(t *testing.T) {
	base := func() *Registry {
		registry, err := NewRegistry(
			staticSpecTool{spec: ports.ToolSpec{Name: "alpha", Description: "first", Parameters: []byte(`{"type":"object"}`)}},
			staticSpecTool{spec: ports.ToolSpec{Name: "beta", Description: "second", Parameters: []byte(`{"type":"object"}`)}},
		)
		if err != nil {
			t.Fatalf("registry fixture: %v", err)
		}
		return registry
	}
	first, err := base().Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	second, err := base().Fingerprint()
	if err != nil || first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("fingerprint must be deterministic sha256: %q vs %q (%v)", first, second, err)
	}
	reorderedRegistry, err := NewRegistry(
		staticSpecTool{spec: ports.ToolSpec{Name: "beta", Description: "second", Parameters: []byte(`{"type":"object"}`)}},
		staticSpecTool{spec: ports.ToolSpec{Name: "alpha", Description: "first", Parameters: []byte(`{"type":"object"}`)}},
	)
	if err != nil {
		t.Fatalf("reordered registry: %v", err)
	}
	reordered, err := reorderedRegistry.Fingerprint()
	if err != nil {
		t.Fatalf("reordered fingerprint: %v", err)
	}
	if reordered == first {
		t.Fatal("registration order drift must change the fingerprint")
	}
	driftedRegistry, err := NewRegistry(
		staticSpecTool{spec: ports.ToolSpec{Name: "alpha", Description: "changed description", Parameters: []byte(`{"type":"object"}`)}},
		staticSpecTool{spec: ports.ToolSpec{Name: "beta", Description: "second", Parameters: []byte(`{"type":"object"}`)}},
	)
	if err != nil {
		t.Fatalf("drifted registry: %v", err)
	}
	drifted, err := driftedRegistry.Fingerprint()
	if err != nil {
		t.Fatalf("drifted fingerprint: %v", err)
	}
	if drifted == first {
		t.Fatal("description drift must change the fingerprint")
	}
	schemaDriftedRegistry, err := NewRegistry(
		staticSpecTool{spec: ports.ToolSpec{Name: "alpha", Description: "first", Parameters: []byte(`{"type":"string"}`)}},
		staticSpecTool{spec: ports.ToolSpec{Name: "beta", Description: "second", Parameters: []byte(`{"type":"object"}`)}},
	)
	if err != nil {
		t.Fatalf("schema drifted registry: %v", err)
	}
	schemaDrifted, err := schemaDriftedRegistry.Fingerprint()
	if err != nil {
		t.Fatalf("schema drifted fingerprint: %v", err)
	}
	if schemaDrifted == first {
		t.Fatal("parameter schema drift must change the fingerprint")
	}
}
