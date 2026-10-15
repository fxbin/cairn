package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

const testMCPName = "mcp__docs__lookup"

// loopMCPCatalog 模拟外部目录端口，独立记录执行与落盘后激活的顺序。
type loopMCPCatalog struct {
	active    bool
	ask       bool
	uncertain bool
	calls     []string
	accepted  int
}

func (catalog *loopMCPCatalog) Specs() []ports.ToolSpec {
	specs := []ports.ToolSpec{{Name: "mcp_discover", Parameters: json.RawMessage(`{"type":"object"}`)}}
	if catalog.active {
		specs = append(specs, ports.ToolSpec{Name: testMCPName, Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	return specs
}
func (catalog *loopMCPCatalog) DeclarationFingerprint(name string) (string, bool) {
	return "schema-v1", catalog.active && name == testMCPName
}
func (catalog *loopMCPCatalog) Proposal(name string, raw json.RawMessage) (tools.MCPProposal, bool, error) {
	if name != "mcp_discover" && (name != testMCPName || !catalog.active) {
		return tools.MCPProposal{}, true, errors.New("unavailable")
	}
	effect := "allow"
	if catalog.ask {
		effect = "ask"
	}
	return tools.MCPProposal{Server: "docs", Identity: "identity", ToolName: name, Effect: effect, Arguments: raw, Summary: "MCP test operation"}, true, nil
}
func (catalog *loopMCPCatalog) Execute(_ context.Context, name string, _ json.RawMessage) tools.Result {
	catalog.calls = append(catalog.calls, name)
	if catalog.uncertain && name == testMCPName {
		return tools.Failure(tools.CodeTerminationUnconfirmed, "uncertain external outcome")
	}
	return tools.Success(struct{}{}, "done", 0, "", false)
}
func (catalog *loopMCPCatalog) SideEffect(string) tools.SideEffectClass {
	return tools.SideEffectProcess
}
func (catalog *loopMCPCatalog) AcceptResult(name string, result tools.Result) error {
	if name == "mcp_discover" && result.OK {
		catalog.active = true
		catalog.accepted++
	}
	return nil
}
func (catalog *loopMCPCatalog) ModelResult(_ string, result tools.Result) tools.Result { return result }
func (catalog *loopMCPCatalog) Close() error                                           { return nil }

// newMCPLoopTest 装配真实 Loop、Policy 和 Journal，只有外部传输使用窄端口替身。
func newMCPLoopTest(t *testing.T, catalog tools.MCPCatalog, provider ports.Provider, journal *memory.Journal) *Loop {
	t.Helper()
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistryWithMCP(catalog)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate.MCPRegistry = registry
	return &Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 4}
}

// TestMCPLoadInSameResponseCannotInvokeUndeclaredTool 发现可为下一轮加载工具，不能改变当前响应的授权声明。
func TestMCPLoadInSameResponseCannotInvokeUndeclaredTool(t *testing.T) {
	catalog := &loopMCPCatalog{}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "discover", Name: "mcp_discover", Arguments: json.RawMessage(`{"server":"docs"}`)}}, {Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "early", Name: testMCPName, Arguments: json.RawMessage(`{}`)}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "next", Name: testMCPName, Arguments: json.RawMessage(`{}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "finished"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "discover tools")
	loop := newMCPLoopTest(t, catalog, provider, journal)
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "discover tools", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if len(catalog.calls) != 2 || catalog.calls[0] != "mcp_discover" || catalog.calls[1] != testMCPName {
		t.Fatalf("undeclared call executed: %v", catalog.calls)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !containsEventPayload(events, domain.EventToolResultAppended, `"mcp_not_declared"`) || countEvent(events, domain.EventOperationStarted) != 2 {
		t.Fatal("undeclared operation was not durably rejected")
	}
}

// TestMCPApprovalPrecedesConnectionAndColdResumeRestoresCatalog 冷恢复经审批后只执行一次，并重建后续声明。
func TestMCPApprovalPrecedesConnectionAndColdResumeRestoresCatalog(t *testing.T) {
	catalog := &loopMCPCatalog{ask: true}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "discover", Name: "mcp_discover", Arguments: json.RawMessage(`{"server":"docs"}`)}}, {Kind: ports.Done}}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "approved discovery")
	loop := newMCPLoopTest(t, catalog, provider, journal)
	input := RunInput{RunID: runID, Task: "approved discovery", WorkspaceHash: "workspace"}
	_, err := loop.Run(context.Background(), input)
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) || len(catalog.calls) != 0 {
		t.Fatalf("connection happened before approval: %v %v", catalog.calls, err)
	}
	nextCatalog := &loopMCPCatalog{ask: true}
	nextProvider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}}}}
	nextLoop := newMCPLoopTest(t, nextCatalog, nextProvider, journal)
	if _, err := nextLoop.Resume(context.Background(), input, ports.Approval{Approve: true, Fingerprint: waiting.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if len(nextCatalog.calls) != 1 || !nextCatalog.active {
		t.Fatalf("cold resume did not persist and activate discovery: %#v", nextCatalog)
	}
	replayCatalog := &loopMCPCatalog{}
	replayLoop := newMCPLoopTest(t, replayCatalog, &scriptedProvider{}, journal)
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := replayLoop.restoreMCPResults(events); err != nil || !replayCatalog.active || len(replayCatalog.calls) != 0 {
		t.Fatalf("restore reissued external discovery: %v %#v", err, replayCatalog)
	}
}

// TestMCPUncertainOutcomeRemainsStartedAndDoesNotRetry 未确认调用保持待对账，普通恢复不能盲重试。
func TestMCPUncertainOutcomeRemainsStartedAndDoesNotRetry(t *testing.T) {
	catalog := &loopMCPCatalog{uncertain: true}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "discover", Name: "mcp_discover", Arguments: json.RawMessage(`{"server":"docs"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "external", Name: testMCPName, Arguments: json.RawMessage(`{}`)}}, {Kind: ports.Done}}}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "external uncertain")
	loop := newMCPLoopTest(t, catalog, provider, journal)
	input := RunInput{RunID: runID, Task: "external uncertain", WorkspaceHash: "workspace"}
	if _, err := loop.Run(context.Background(), input); !errors.Is(err, ports.ErrWaitingReconcile) {
		t.Fatalf("error=%v", err)
	}
	if _, err := loop.Resume(context.Background(), input, ports.Approval{}); !errors.Is(err, ports.ErrWaitingReconcile) {
		t.Fatalf("resume error=%v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.calls) != 2 || projection.PendingOperation == nil || countEvent(events, domain.EventToolResultAppended) != 1 {
		t.Fatal("uncertain call retried or falsely closed")
	}
}

func (catalog *loopMCPCatalog) ResetProjection() { catalog.active = false }

func (catalog *loopMCPCatalog) NeedsQuiescence() bool { return false }

func (catalog *loopMCPCatalog) ExecuteWithIdentity(ctx context.Context, name string, raw json.RawMessage, _ string) tools.Result {
	return catalog.Execute(ctx, name, raw)
}
