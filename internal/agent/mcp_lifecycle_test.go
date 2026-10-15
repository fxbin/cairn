package agent

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

type lifecycleCatalog struct {
	loopMCPCatalog
	live         bool
	cleanupFails bool
	cleanupDelay time.Duration
}

func (catalog *lifecycleCatalog) NeedsQuiescence() bool { return catalog.live }
func (catalog *lifecycleCatalog) Proposal(name string, raw json.RawMessage) (tools.MCPProposal, bool, error) {
	if name == tools.MCPQuiesceTool {
		return tools.MCPProposal{ToolName: name, Effect: "allow", Local: true, Summary: "close owned MCP resources", Arguments: raw}, true, nil
	}
	return catalog.loopMCPCatalog.Proposal(name, raw)
}
func (catalog *lifecycleCatalog) Execute(ctx context.Context, name string, raw json.RawMessage) tools.Result {
	if name == tools.MCPQuiesceTool {
		catalog.calls = append(catalog.calls, name)
		if catalog.cleanupDelay > 0 {
			time.Sleep(catalog.cleanupDelay)
		}
		if catalog.cleanupFails {
			return tools.Failure(tools.CodeTerminationUnconfirmed, "cleanup unconfirmed")
		}
		catalog.live = false
		return tools.Success(struct{}{}, "closed", 0, "", false)
	}
	catalog.live = true
	return catalog.loopMCPCatalog.Execute(ctx, name, raw)
}

// TestMCPQuiescesBeforeVerificationAndPreservesReconciliation 清理必须先于验证，失败不能伪装为运行完成。
func TestMCPQuiescesBeforeVerificationAndPreservesReconciliation(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "uncertain"}[fail], func(t *testing.T) {
			catalog := &lifecycleCatalog{cleanupFails: fail}
			provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
				{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "discover", Name: "mcp_discover", Arguments: json.RawMessage(`{"server":"docs"}`)}}, {Kind: ports.Done}},
				{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}},
			}}
			journal := memory.New()
			runID := domain.NewRunID()
			appendInitialRunningEvents(t, journal, runID, "MCP lifecycle")
			loop := newMCPLoopTest(t, catalog, provider, journal)
			loop.MaxToolCalls = 1
			_, runErr := loop.Run(context.Background(), RunInput{RunID: runID, Task: "MCP lifecycle", WorkspaceHash: "workspace"})
			events, err := journal.Load(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := domain.Replay(events)
			if err != nil {
				t.Fatal(err)
			}
			if fail {
				if !errors.Is(runErr, ports.ErrWaitingReconcile) || projection.PendingOperation == nil || projection.PendingOperation.ToolName != tools.MCPQuiesceTool || countEvent(events, domain.EventRunCompleted) != 0 || countEvent(events, domain.EventVerificationStarted) != 0 {
					t.Fatalf("cleanup failure was finalized: %#v %v", projection, runErr)
				}
			} else {
				if runErr != nil || catalog.live || projection.State != domain.RunCompleted || loop.startedOperations != 1 {
					t.Fatalf("cleanup counted as business call or failed: live=%v calls=%d error=%v", catalog.live, loop.startedOperations, runErr)
				}
				closed := false
				for _, event := range events {
					if event.Type == domain.EventOperationSucceeded && isMCPLifecyclePayload(event.Payload) {
						closed = true
					}
					if event.Type == domain.EventVerificationStarted && !closed {
						t.Fatal("verification started with live sessions")
					}
				}
				cold := newMCPLoopTest(t, &loopMCPCatalog{}, &scriptedProvider{}, journal)
				cold.MaxToolCalls = 2
				if err := cold.ensureToolBudget(context.Background(), runID); err != nil || cold.startedOperations != 1 {
					t.Fatalf("cold budget counted cleanup: calls=%d err=%v", cold.startedOperations, err)
				}
			}
		})
	}
}

type approvalMCPProvider struct {
	catalog  *lifecycleCatalog
	requests int
}

func (provider *approvalMCPProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.requests++
	call := ports.ToolCall{ID: "discover", Name: "mcp_discover", Arguments: json.RawMessage(`{"server":"docs"}`)}
	if provider.requests > 1 {
		provider.catalog.ask = true
		call = ports.ToolCall{ID: "lookup", Name: testMCPName, Arguments: json.RawMessage(`{"id":12}`)}
	}
	if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done})
}

// TestMCPQuiescesBeforeApprovalProposal 审批等待前确认清理，最后提议仍是用户真正需要审批的工具。
func TestMCPQuiescesBeforeApprovalProposal(t *testing.T) {
	catalog := &lifecycleCatalog{}
	provider := &approvalMCPProvider{catalog: catalog}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "approve after cleanup")
	loop := newMCPLoopTest(t, catalog, provider, journal)
	_, runErr := loop.Run(context.Background(), RunInput{RunID: runID, Task: "approve after cleanup", WorkspaceHash: "workspace"})
	var waiting *ports.WaitingApprovalError
	if !errors.As(runErr, &waiting) || catalog.live {
		t.Fatalf("approval paused with live sessions: %v live=%v", runErr, catalog.live)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if projection.PendingApproval == nil {
		t.Fatal("missing pending approval")
	}
	call, found := proposedToolCall(events, projection.PendingApproval.OperationID)
	if !found || call.Name != testMCPName || string(call.Arguments) != `{"id":12}` {
		t.Fatalf("wrong proposal in approval: %#v", call)
	}
	if len(catalog.calls) != 2 || catalog.calls[1] != tools.MCPQuiesceTool {
		t.Fatalf("unexpected external execution: %v", catalog.calls)
	}
}

// TestMCPRestoreRequiresSuccessfulMatchingReceipt 失败终态和不匹配回执均不能激活外部目录。
func TestMCPRestoreRequiresSuccessfulMatchingReceipt(t *testing.T) {
	for _, terminal := range []domain.EventType{domain.EventOperationFailed, domain.EventOperationSucceeded} {
		catalog := &loopMCPCatalog{}
		loop := newMCPLoopTest(t, catalog, &scriptedProvider{}, memory.New())
		operationID := domain.NewOperationID()
		receipt := tools.Success(struct {
			Count int `json:"count"`
		}{1}, "discovered", 0, "", false)
		appended := receipt
		if terminal == domain.EventOperationSucceeded {
			appended.Data = json.RawMessage(`{"count":2}`)
		}
		events := []domain.Event{
			{Type: domain.EventToolCallProposed, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: "discover", ToolName: "mcp_discover", Arguments: json.RawMessage(`{"server":"docs"}`)})},
			{Type: terminal, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "mcp_discover", Result: receipt})},
			{Type: domain.EventToolResultAppended, OperationID: &operationID, Payload: mustJSON(appended)},
		}
		if err := loop.restoreMCPResults(events); err == nil || catalog.active || catalog.accepted != 0 {
			t.Fatal("invalid receipt granted catalog state")
		}
	}
}

// TestMCPLifecycleSuccessCannotResetBusinessFailureGuard 清理成功不能掩盖连续业务失败。
func TestMCPLifecycleSuccessCannotResetBusinessFailureGuard(t *testing.T) {
	var events []domain.Event
	for index := 0; index < consecutiveOperationFailureLimit; index++ {
		failed, cleanup := domain.NewOperationID(), domain.NewOperationID()
		events = append(events,
			domain.Event{Type: domain.EventPolicyDecided, OperationID: &failed, Payload: json.RawMessage(`{"fingerprint":"same"}`)},
			domain.Event{Type: domain.EventOperationFailed, OperationID: &failed},
			domain.Event{Type: domain.EventToolCallProposed, OperationID: &cleanup, Payload: mustJSON(toolCallPayload{ToolName: tools.MCPQuiesceTool})},
			domain.Event{Type: domain.EventOperationSucceeded, OperationID: &cleanup},
		)
	}
	if repeatedOperationFailureGuard(events) != loopGuardRepeatedOperationFailure {
		t.Fatal("lifecycle cleanup reset business failure guard")
	}
}

// TestMCPLifecycleIsInvisibleToModelAndProgress 内部回收不伪造模型工具结果或业务进展。
func TestMCPLifecycleIsInvisibleToModelAndProgress(t *testing.T) {
	turnID, operationID := domain.NewTurnID(), domain.NewOperationID()
	events := []domain.Event{
		{Seq: 1, Type: domain.EventTurnOpened, TurnID: &turnID},
		{Seq: 2, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: "cairn-mcp-close-test", ToolName: tools.MCPQuiesceTool, Arguments: json.RawMessage(`{}`)})},
		{Seq: 3, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"fingerprint":"cleanup"}`)},
		{Seq: 4, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(tools.Success(struct{}{}, "closed", 0, "", false))},
	}
	loop := newMCPLoopTest(t, &loopMCPCatalog{}, &scriptedProvider{}, memory.New())
	messages, err := loop.messagesFromEvents(events, "task")
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range messages {
		if message.Role == "tool" {
			t.Fatal("internal cleanup created an unpaired model result")
		}
	}
	if openedTurnCount(events) != 0 || operationSignatureForTurn(events, turnID) != "" || progressFingerprint("workspace", events) != progressFingerprint("workspace", nil) {
		t.Fatal("lifecycle cleanup created business progress or consumed model turns")
	}
}

func (catalog *lifecycleCatalog) ExecuteWithIdentity(ctx context.Context, name string, raw json.RawMessage, _ string) tools.Result {
	return catalog.Execute(ctx, name, raw)
}

// TestMCPSlowCleanupCancellationUsesFreshCommitWindow 清理超过普通提交窗口后仍需写入确认终态。
func TestMCPSlowCleanupCancellationUsesFreshCommitWindow(t *testing.T) {
	catalog := &lifecycleCatalog{live: true, cleanupDelay: 6 * time.Second}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "slow cleanup cancellation")
	loop := newMCPLoopTest(t, catalog, &scriptedProvider{}, journal)
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, runErr := loop.persistCancellation(ctx, &projection, RunInput{RunID: runID, Task: "slow cleanup cancellation", WorkspaceHash: "workspace"}, nil)
	if !errors.Is(runErr, context.Canceled) || projection.State != domain.RunCancelled || catalog.live {
		t.Fatalf("confirmed cleanup failed to commit cancellation: state=%s err=%v", projection.State, runErr)
	}
}
