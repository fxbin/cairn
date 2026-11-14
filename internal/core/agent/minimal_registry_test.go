package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/policy"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// basicOnlyRegistry 是没有 Skill/MCP 方法的 Mock 工具执行端口。
type basicOnlyRegistry struct{ calls int }

func (registry *basicOnlyRegistry) Specs() []ports.ToolSpec {
	return []ports.ToolSpec{{Name: "read_file", Parameters: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}`)}}
}
func (registry *basicOnlyRegistry) Execute(_ context.Context, name string, _ json.RawMessage) contract.ToolResult {
	registry.calls++
	if name != "read_file" {
		return contract.ToolFailure("unexpected_tool", "unexpected mock tool")
	}
	return contract.ToolSuccess(struct {
		Content string `json:"content"`
	}{Content: "seed"}, "seed", 4, "", false)
}
func (registry *basicOnlyRegistry) SideEffect(string) contract.SideEffectClass {
	return contract.SideEffectNone
}
func (registry *basicOnlyRegistry) ModelResult(_ string, result contract.ToolResult) contract.ToolResult {
	return result
}
func (registry *basicOnlyRegistry) ReadonlyToolNames() map[string]bool {
	return map[string]bool{"read_file": true}
}

// TestLoopRunsWithBasicToolRegistry 验证 Core 可以消费不含高级方法的工具端口，
// 完成工具结果反馈和终态持久化，同时对缺失能力的成功回执保持失败关闭。
func TestLoopRunsWithBasicToolRegistry(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry := &basicOnlyRegistry{}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "read input")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"input.txt"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}},
	}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "test", MaxTurns: 3, MaxToolCalls: 5}
	if loop.mcpCapability() != nil || len(loop.ResultObservers) != 0 {
		t.Fatal("basic registry unexpectedly exposes an advanced capability")
	}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "read input", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "done" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if registry.calls != 1 || countEvent(events, domain.EventOperationStarted) != 1 || countEvent(events, domain.EventRunCompleted) != 1 {
		t.Fatal("basic execution did not persist exactly one tool operation and completion")
	}
	success := contract.ToolSuccess(struct{}{}, "receipt", 0, "", false)
	if loop.observeToolResult("mcp_discover", success) == nil || loop.observeToolResult("skill", success) == nil {
		t.Fatal("missing capability silently accepted an advanced success receipt")
	}
}

func (*basicOnlyRegistry) RequiresResultObserver(name string) bool { return name != "read_file" }
