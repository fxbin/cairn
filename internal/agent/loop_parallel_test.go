package agent

import (
	"context"
	"encoding/json"
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
