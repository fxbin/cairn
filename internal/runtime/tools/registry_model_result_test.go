// [INPUT]: 普通静态工具与通用模型视图。
// [OUTPUT]: 验证投影认领和原始证据保留。
// [POS]: 基础注册表回归，不装配高级目录。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

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

// TestRegistryModelResultUsesRegisteredTool 验证已认领投影与原始回执分别保留。
func TestRegistryModelResultUsesRegisteredTool(t *testing.T) {
	projected := &nativeModelResultTool{staticSpecTool: staticSpecTool{spec: ports.ToolSpec{Name: "native-projection"}}}
	registry, err := NewRegistry(projected, staticSpecTool{spec: ports.ToolSpec{Name: "plain"}})
	if err != nil {
		t.Fatal(err)
	}
	original := contract.ToolSuccess(struct {
		Body string `json:"body"`
	}{Body: "complete journal evidence"}, "original", 25, "identity", false)
	got := registry.ModelResult("native-projection", original)
	expected := original
	expected.Preview = "native-model-view"
	if !reflect.DeepEqual(got, expected) || projected.calls != 1 || original.Preview != "original" {
		t.Fatal("model projection changed full receipt fields")
	}
	for _, name := range []string{"plain", "unknown"} {
		if got := registry.ModelResult(name, original); !reflect.DeepEqual(got, original) {
			t.Fatalf("unprojected result changed for %s", name)
		}
	}
}
