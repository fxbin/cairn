package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/policy"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// projectingRegistryFixture 把普通读取声明为需要状态投影，验证 Core 无能力名称分支。
type projectingRegistryFixture struct{ basicOnlyRegistry }

func (*projectingRegistryFixture) RequiresResultObserver(name string) bool {
	return name == "read_file"
}

// resultObserverProbe 验证回调只能看见已经落盘的完整操作事实。
type resultObserverProbe struct {
	journal      ports.Journal
	runID        domain.RunID
	notified     int
	beginError   error
	replayError  error
	observeError error
}

func (*resultObserverProbe) HandlesToolResult(name string) bool     { return name == "read_file" }
func (probe *resultObserverProbe) BeginReplay([]domain.Event) error { return probe.beginError }
func (probe *resultObserverProbe) ReplayEvent(domain.Event) error   { return probe.replayError }
func (probe *resultObserverProbe) ObserveToolResult(_ string, _ contract.ToolResult) error {
	events, err := probe.journal.Load(context.Background(), probe.runID)
	if err != nil {
		return err
	}
	if countEvent(events, domain.EventOperationSucceeded) != probe.notified+1 || countEvent(events, domain.EventToolResultAppended) != probe.notified+1 {
		return errors.New("observer was called before complete result persistence")
	}
	probe.notified++
	return probe.observeError
}

// newResultObserverLoop 使用同一 Loop 驱动未知能力的工具、投影及 Journal。
func newResultObserverLoop(t *testing.T, parallel bool) (*Loop, *resultObserverProbe, *projectingRegistryFixture, RunInput) {
	t.Helper()
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	input := RunInput{RunID: domain.NewRunID(), Task: "project result", WorkspaceHash: "workspace"}
	appendInitialRunningEvents(t, journal, input.RunID, input.Task)
	calls := []ports.ModelEvent{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "first", Name: "read_file", Arguments: json.RawMessage(`{"path":"input.txt"}`)}}}
	if parallel {
		calls = append(calls, ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "second", Name: "read_file", Arguments: json.RawMessage(`{"path":"input.txt"}`)}})
	}
	calls = append(calls, ports.ModelEvent{Kind: ports.Done})
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{calls, {{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}}}}
	registry := &projectingRegistryFixture{}
	probe := &resultObserverProbe{journal: journal, runID: input.RunID}
	loop := &Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, ResultObservers: []ToolResultObserver{probe}}
	return loop, probe, registry, input
}

// TestResultObserverRequiresUniqueWiringBeforeExecution 缺失、重复或空观察器不得先执行状态工具。
func TestResultObserverRequiresUniqueWiringBeforeExecution(t *testing.T) {
	for _, entry := range []string{"run", "scripted", "resume", "scripted-resume"} {
		for _, variant := range []string{"missing", "duplicate", "nil"} {
			t.Run(entry+"/"+variant, func(t *testing.T) {
				loop, probe, registry, input := newResultObserverLoop(t, false)
				calls := []ports.ToolCall{{ID: "first", Name: "read_file", Arguments: json.RawMessage(`{"path":"input.txt"}`)}}
				approval := ports.Approval{}
				if entry == "resume" || entry == "scripted-resume" {
					loop.Gate = &secondDecisionFailure{Authorizer: loop.Gate, calls: 1, ask: true}
					var err error
					if entry == "resume" {
						_, err = loop.Run(context.Background(), input)
					} else {
						_, err = loop.RunScriptedToolPlan(context.Background(), input, ports.Approval{}, calls)
					}
					var waiting *ports.WaitingApprovalError
					if !errors.As(err, &waiting) {
						t.Fatalf("fixture did not pause: %v", err)
					}
					approval = ports.Approval{Approve: true, Fingerprint: waiting.Fingerprint}
				}
				switch variant {
				case "missing":
					loop.ResultObservers = nil
				case "duplicate":
					loop.ResultObservers = []ToolResultObserver{probe, probe}
				case "nil":
					loop.ResultObservers = []ToolResultObserver{nil}
				}
				var err error
				switch entry {
				case "run":
					_, err = loop.Run(context.Background(), input)
				case "resume":
					_, err = loop.Resume(context.Background(), input, approval)
				default:
					_, err = loop.RunScriptedToolPlan(context.Background(), input, approval, calls)
				}
				if err == nil {
					t.Fatal("invalid observer wiring ran")
				}
				events, loadErr := loop.Journal.Load(context.Background(), input.RunID)
				if loadErr != nil {
					t.Fatal(loadErr)
				}
				if registry.calls != 0 || countEvent(events, domain.EventOperationStarted) != 0 || countEvent(events, domain.EventRunCompleted) != 0 {
					t.Fatal("stateful operation executed before projection wiring was accepted")
				}
			})
		}
	}
}

// TestResultObserverSeesPersistedFactsAndPropagatesFailure 不得把未落盘结果交观察器或吞掉投影错误。
func TestResultObserverSeesPersistedFactsAndPropagatesFailure(t *testing.T) {
	for _, fails := range []bool{false, true} {
		loop, probe, registry, input := newResultObserverLoop(t, false)
		sentinel := errors.New("projection refused persisted result")
		if fails {
			probe.observeError = sentinel
		}
		result, err := loop.Run(context.Background(), input)
		if fails && !errors.Is(err, sentinel) {
			t.Fatalf("observer error lost: %v", err)
		}
		if !fails && (err != nil || result.Response != "done") {
			t.Fatalf("result=%+v err=%v", result, err)
		}
		if registry.calls != 1 || probe.notified != 1 {
			t.Fatal("tool or observer did not execute exactly once")
		}
		events, loadErr := loop.Journal.Load(context.Background(), input.RunID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if fails && countEvent(events, domain.EventRunCompleted) != 0 {
			t.Fatal("projection error produced false completion")
		}
	}
}

// secondDecisionFailure 在第二项并行准备阶段失败，第一项已 started 必须完整收尾。
type secondDecisionFailure struct {
	Authorizer
	calls   int
	failure error
	ask     bool
}

func (gate *secondDecisionFailure) Decide(name string, args json.RawMessage) (AuthorizationDecision, error) {
	gate.calls++
	if gate.calls >= 2 && gate.ask {
		decision, err := gate.Authorizer.Decide(name, args)
		if err != nil {
			return decision, err
		}
		decision.Effect = AuthorizationAsk
		return decision, nil
	}
	if gate.calls == 2 {
		return AuthorizationDecision{}, gate.failure
	}
	return gate.Authorizer.Decide(name, args)
}

// TestPreparedReadonlyObserverErrorIsJoined 保留准备失败和已启动项的投影错误，同时闭合已启动操作。
func TestPreparedReadonlyObserverErrorIsJoined(t *testing.T) {
	loop, probe, registry, input := newResultObserverLoop(t, true)
	prepareErr, projectionErr := errors.New("second decision failed"), errors.New("first projection failed")
	loop.Gate = &secondDecisionFailure{Authorizer: loop.Gate, failure: prepareErr}
	probe.observeError = projectionErr
	_, err := loop.Run(context.Background(), input)
	if !errors.Is(err, prepareErr) || !errors.Is(err, projectionErr) {
		t.Fatalf("joined error missing cause: %v", err)
	}
	events, loadErr := loop.Journal.Load(context.Background(), input.RunID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	projection, replayErr := domain.Replay(events)
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	if registry.calls != 1 || probe.notified != 1 || projection.PendingOperation != nil || countEvent(events, domain.EventOperationStarted) != countEvent(events, domain.EventOperationSucceeded) {
		t.Fatalf("prepared operation not closed: calls=%d notifications=%d projection=%+v", registry.calls, probe.notified, projection)
	}
}

// TestReplayObserverErrorsAndUnknownSuccessFailClosed 回放错误和未知成功事实都不能默默丢失，普通未知失败保持空投影。
func TestReplayObserverErrorsAndUnknownSuccessFailClosed(t *testing.T) {
	loop, probe, _, _ := newResultObserverLoop(t, false)
	sentinel := errors.New("replay unavailable")
	probe.beginError = sentinel
	if err := loop.restoreResultProjections(nil); !errors.Is(err, sentinel) {
		t.Fatalf("begin error lost: %v", err)
	}
	probe.beginError, probe.replayError = nil, sentinel
	if err := loop.restoreResultProjections([]domain.Event{{Type: domain.EventRunStarted}}); !errors.Is(err, sentinel) {
		t.Fatalf("replay error lost: %v", err)
	}
	registry, err := tools.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	loop.Tools, loop.ResultObservers = registry, nil
	operationID := domain.NewOperationID()
	events := []domain.Event{
		{Type: domain.EventToolCallProposed, OperationID: &operationID, Payload: mustJSON(toolCallPayload{ToolName: "unknown", Arguments: json.RawMessage(`{}`)})},
		{Type: domain.EventToolResultAppended, OperationID: &operationID, Payload: mustJSON(contract.ToolFailure("unknown_tool", "unknown tool"))},
	}
	if err := loop.restoreResultProjections(events); err != nil {
		t.Fatalf("unknown failure changed: %v", err)
	}
	if err := loop.observeToolResult("unknown", contract.ToolFailure("unknown_tool", "unknown tool")); err != nil {
		t.Fatal(err)
	}
	events[1].Payload = mustJSON(contract.ToolSuccess(struct{}{}, "unexpected success", 0, "", false))
	if err := loop.restoreResultProjections(events); err == nil {
		t.Fatal("unknown successful history silently lost its projection")
	}
	if err := loop.observeToolResult("unknown", contract.ToolSuccess(struct{}{}, "unexpected success", 0, "", false)); err == nil {
		t.Fatal("unknown live success silently lost its projection")
	}
}

// TestPreparedReadonlyKeepsApprovalAndObserverErrors 三调用组等待审批时仍须暴露已启动首项的投影失败。
func TestPreparedReadonlyKeepsApprovalAndObserverErrors(t *testing.T) {
	loop, probe, registry, input := newResultObserverLoop(t, true)
	provider := loop.Provider.(*scriptedProvider)
	calls := provider.scripts[0]
	provider.scripts[0] = append(calls[:len(calls)-1], ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "third", Name: "read_file", Arguments: json.RawMessage(`{"path":"input.txt"}`)}}, ports.ModelEvent{Kind: ports.Done})
	sentinel := errors.New("prepared projection failed during approval pause")
	probe.observeError = sentinel
	loop.Gate = &secondDecisionFailure{Authorizer: loop.Gate, ask: true}
	_, err := loop.Run(context.Background(), input)
	var waiting *ports.WaitingApprovalError
	if !errors.Is(err, sentinel) || !errors.As(err, &waiting) {
		t.Fatalf("lost approval or observer cause: %v", err)
	}
	events, loadErr := loop.Journal.Load(context.Background(), input.RunID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if registry.calls != 1 || probe.notified != 1 || countEvent(events, domain.EventOperationStarted) != 1 || countEvent(events, domain.EventOperationSucceeded) != 1 || countEvent(events, domain.EventRunCompleted) != 0 {
		t.Fatal("approval pause lost prepared operation or executed a later call")
	}
}
