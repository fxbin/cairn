package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// rendezvousProbe 是验证只读并行执行的探针工具：进入即宣告到达并等待对端
// 到达；串行执行时对端永远无法到达，超时返回失败暴露缺乏并行。
type rendezvousProbe struct {
	name    string
	arrived sync.Once
	signal  chan struct{}
	peer    chan struct{}
}

func (probe *rendezvousProbe) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: probe.name, Description: "readonly rendezvous probe", Parameters: json.RawMessage(`{"type":"object"}`)}
}

// SideEffect 声明为无副作用，可进只读并行组。
func (probe *rendezvousProbe) SideEffect() tools.SideEffectClass { return tools.SideEffectNone }

// Execute 宣告到达并等待对端；对端未在时限内到达即判定为串行执行。
func (probe *rendezvousProbe) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	probe.arrived.Do(func() { close(probe.signal) })
	select {
	case <-probe.peer:
		return tools.Success(json.RawMessage(`{}`), "peer rendezvous", 0, "", false)
	case <-time.After(5 * time.Second):
		return tools.Failure("serial_execution_detected", "peer probe did not arrive; readonly calls were executed serially")
	case <-ctx.Done():
		return tools.Failure("cancelled", "context cancelled before rendezvous")
	}
}

// TestLoopExecutesReadonlyBatchInParallel 用会合探针锁定同批只读调用的
// 真并发执行：两个探针互等对方进入，串行必然超时失败；并行则双双成功。
func TestLoopExecutesReadonlyBatchInParallel(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	signalA, signalB := make(chan struct{}), make(chan struct{})
	probeA := &rendezvousProbe{name: "list_files", signal: signalA, peer: signalB}
	probeB := &rendezvousProbe{name: "read_file", signal: signalB, peer: signalA}
	registry, err := tools.NewRegistry(probeA, probeB)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "probe-a", Name: "list_files", Arguments: json.RawMessage(`{}`)}},
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "probe-b", Name: "read_file", Arguments: json.RawMessage(`{}`)}},
			{Kind: ports.Done},
		},
		{{Kind: ports.TextDelta, Text: "probes finished"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "readonly batch parallelism")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "readonly batch parallelism", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "probes finished" {
		t.Fatalf("run=%#v err=%v", result, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, domain.EventOperationStarted) != 2 || countEvent(events, domain.EventOperationSucceeded) != 2 || countEvent(events, domain.EventOperationFailed) != 0 {
		t.Fatalf("readonly probes did not both succeed: started=%d succeeded=%d failed=%d",
			countEvent(events, domain.EventOperationStarted), countEvent(events, domain.EventOperationSucceeded), countEvent(events, domain.EventOperationFailed))
	}
}

// cancellingProbe 在执行时取消运行上下文并返回取消结果。
type cancellingProbe struct {
	name   string
	cancel context.CancelFunc
}

func (probe *cancellingProbe) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: probe.name, Description: "readonly cancelling probe", Parameters: json.RawMessage(`{"type":"object"}`)}
}

// SideEffect 声明为无副作用，可进只读并行组。
func (probe *cancellingProbe) SideEffect() tools.SideEffectClass { return tools.SideEffectNone }

// Execute 取消运行上下文并返回取消结果。
func (probe *cancellingProbe) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	probe.cancel()
	return tools.Failure("cancelled", "context cancelled by probe")
}

// waitForCancelProbe 等待取消发生后返回成功结果，用于保证批次末位调用
// 不携带错误、命中批次错误分支而非单调用分支。
type waitForCancelProbe struct{ name string }

func (probe *waitForCancelProbe) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: probe.name, Description: "readonly wait-for-cancel probe", Parameters: json.RawMessage(`{"type":"object"}`)}
}

// SideEffect 声明为无副作用，可进只读并行组。
func (probe *waitForCancelProbe) SideEffect() tools.SideEffectClass { return tools.SideEffectNone }

// Execute 等待上下文取消后立即成功，模拟一个无视取消的快速只读工具。
func (probe *waitForCancelProbe) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	select {
	case <-ctx.Done():
		return tools.Success(json.RawMessage(`{}`), "done", 0, "", false)
	case <-time.After(5 * time.Second):
		return tools.Failure("timeout", "context was not cancelled")
	}
}

// TestLoopCancelsReadonlyBatchWithoutDanglingOperations 锁定批次取消语义：
// 非末位只读调用取消时经批次分支持久化取消终态（而非失败终态），且同组
// 其余已 started 调用全部闭合，不留下悬空只读操作。
func TestLoopCancelsReadonlyBatchWithoutDanglingOperations(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	probeA := &cancellingProbe{name: "list_files", cancel: cancel}
	probeB := &waitForCancelProbe{name: "read_file"}
	registry, err := tools.NewRegistry(probeA, probeB)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "probe-a", Name: "list_files", Arguments: json.RawMessage(`{}`)}},
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "probe-b", Name: "read_file", Arguments: json.RawMessage(`{}`)}},
			{Kind: ports.Done},
		},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "readonly batch cancellation")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	_, err = loop.Run(ctx, RunInput{RunID: runID, Task: "readonly batch cancellation", WorkspaceHash: "workspace"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run err = %v, want context.Canceled", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, replayErr := domain.Replay(events)
	if replayErr != nil || projection.State != domain.RunCancelled {
		t.Fatalf("projection state=%s err=%v, want cancelled", projection.State, replayErr)
	}
	if dangling := projection.StartedOperations(); len(dangling) != 0 {
		t.Fatalf("dangling started readonly operations: %d", len(dangling))
	}
	// 取消与信号量获取同时就绪时 worker 可能把未执行调用短路为 cancelled，
	// 两种分布都合法；合同只要求：至少一条取消操作、无失败操作、无悬空。
	if countEvent(events, domain.EventOperationCancelled) < 1 || countEvent(events, domain.EventOperationFailed) != 0 {
		t.Fatalf("operation outcomes wrong: cancelled=%d failed=%d",
			countEvent(events, domain.EventOperationCancelled), countEvent(events, domain.EventOperationFailed))
	}
}

// externalAppendProbe 在执行时以外部写者身份向本 run Journal 追加一条
// steering 事实，模拟家族外部追加与 Loop 事件的确定性竞争。
type externalAppendProbe struct {
	name    string
	journal ports.Journal
	runID   domain.RunID
}

func (probe *externalAppendProbe) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: probe.name, Description: "readonly external-append probe", Parameters: json.RawMessage(`{"type":"object"}`)}
}

// SideEffect 声明为无副作用，可进只读并行组。
func (probe *externalAppendProbe) SideEffect() tools.SideEffectClass { return tools.SideEffectNone }

// Execute 以当前末序号 CAS 追加一条外部 steering 事实后成功返回。
func (probe *externalAppendProbe) Execute(ctx context.Context, raw json.RawMessage) tools.Result {
	events, err := probe.journal.Load(ctx, probe.runID)
	if err != nil {
		return tools.Failure("probe_failed", err.Error())
	}
	draft := domain.EventDraft{RunID: probe.runID, WorkspaceHash: "workspace", Type: domain.EventSteeringInputSubmitted, Payload: json.RawMessage(`{"payload_version":1,"input_id":"ext-1","kind":"steer"}`)}
	if _, err := probe.journal.Append(ctx, uint64(len(events)), draft); err != nil {
		return tools.Failure("probe_failed", err.Error())
	}
	return tools.Success(json.RawMessage(`{}`), "external event appended", 0, "", false)
}

// TestLoopAppendResyncsAfterExternalFamilyAppend 锁定 Loop 追加韧性：外部
// 家族追加（本测试以探针确定性制造）抬高序号后，Loop 的下一次追加经
// 冲突再同步重试成功，运行干净完成且外部事实保留在事件链中。
func TestLoopAppendResyncsAfterExternalFamilyAppend(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "external append resilience")
	probe := &externalAppendProbe{name: "read_file", journal: journal, runID: runID}
	registry, err := tools.NewRegistry(probe)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "probe-1", Name: "read_file", Arguments: json.RawMessage(`{}`)}},
			{Kind: ports.Done},
		},
		{{Kind: ports.TextDelta, Text: "resynced"}, {Kind: ports.Done}},
	}}
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3}
	result, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "external append resilience", WorkspaceHash: "workspace"})
	if err != nil || result.Response != "resynced" {
		t.Fatalf("run=%#v err=%v", result, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	external := false
	for _, event := range events {
		if event.Type == domain.EventSteeringInputSubmitted {
			external = true
		}
	}
	if !external {
		t.Fatalf("external steering fact missing from journal")
	}
	if _, err := domain.Replay(events); err != nil {
		t.Fatalf("journal no longer replays: %v", err)
	}
}
