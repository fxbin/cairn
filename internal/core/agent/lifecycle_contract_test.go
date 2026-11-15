// [INPUT]: 通用资源替身、真实审计链与 MCP 接线夹具。
// [OUTPUT]: 验证资源名称无关的释放及不完整接线拒绝。
// [POS]: Core 生命周期和授权执行端口回归。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

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
)

const fixtureReleaseTool = "release_owned_resource"

type lifecycleFixture struct {
	live bool
	plan contract.LifecycleOperation
}

func (fixture *lifecycleFixture) NeedsQuiescence() bool { return fixture.live }
func (fixture *lifecycleFixture) QuiescenceOperation() contract.LifecycleOperation {
	return fixture.plan
}
func newLifecycleFixture() *lifecycleFixture {
	return &lifecycleFixture{live: true, plan: contract.LifecycleOperation{ToolName: fixtureReleaseTool, Arguments: json.RawMessage(`{}`), CallIDPrefix: "release-", PendingOperationReason: "pending must reconcile", DeniedReason: "release denied", UnconfirmedReason: "release unconfirmed"}}
}

type lifecycleRegistryFixture struct {
	basicOnlyRegistry
	owner    *lifecycleFixture
	releases int
	binding  string
	failed   bool
}

func (registry *lifecycleRegistryFixture) Execute(ctx context.Context, name string, raw json.RawMessage) contract.ToolResult {
	if name == fixtureReleaseTool {
		registry.releases++
		if registry.failed {
			return contract.ToolFailure(contract.CodeTerminationUnconfirmed, "unconfirmed")
		}
		registry.owner.live = false
		return contract.ToolSuccess(struct{}{}, "released", 0, "", false)
	}
	return registry.basicOnlyRegistry.Execute(ctx, name, raw)
}
func (*lifecycleRegistryFixture) SideEffect(name string) contract.SideEffectClass {
	if name == fixtureReleaseTool {
		return contract.SideEffectProcess
	}
	return contract.SideEffectNone
}
func (*lifecycleRegistryFixture) RequiresResultObserver(name string) bool {
	return name == fixtureReleaseTool
}
func (*lifecycleRegistryFixture) RequiredLifecycleTools() []string {
	return []string{fixtureReleaseTool}
}

type lifecycleAuthorityFixture struct{ Authorizer }

func (gate lifecycleAuthorityFixture) Decide(name string, raw json.RawMessage) (AuthorizationDecision, error) {
	if name == fixtureReleaseTool {
		return AuthorizationDecision{Effect: AuthorizationAllow, Fingerprint: stableFingerprint("fixture-release"), ExecutionBinding: "release-binding"}, nil
	}
	return gate.Authorizer.Decide(name, raw)
}

type lifecycleObserverFixture struct {
	journal  ports.Journal
	runID    domain.RunID
	notified int
}

func (*lifecycleObserverFixture) HandlesToolResult(name string) bool {
	return name == fixtureReleaseTool
}
func (*lifecycleObserverFixture) BeginReplay([]domain.Event) error { return nil }
func (*lifecycleObserverFixture) ReplayEvent(domain.Event) error   { return nil }
func (observer *lifecycleObserverFixture) ObserveToolResult(_ string, _ contract.ToolResult) error {
	events, err := observer.journal.Load(context.Background(), observer.runID)
	if err != nil {
		return err
	}
	if len(events) < 2 || events[len(events)-1].Type != domain.EventToolResultAppended || events[len(events)-2].Type != domain.EventOperationSucceeded {
		return errors.New("release observed before complete persistence")
	}
	observer.notified++
	return nil
}

// TestGenericLifecycleKeepsAuditAndReconciliation 非协议资源同样经完整审计链，未确认时不得先完成运行。
func TestGenericLifecycleKeepsAuditAndReconciliation(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "uncertain"}[failed], func(t *testing.T) {
			loop, _, _, input := newResultObserverLoop(t, false)
			owner := newLifecycleFixture()
			registry := &lifecycleRegistryFixture{owner: owner, failed: failed}
			observer := &lifecycleObserverFixture{journal: loop.Journal, runID: input.RunID}
			loop.Tools = registry
			loop.Gate = lifecycleAuthorityFixture{loop.Gate}
			loop.ResultObservers = []ToolResultObserver{observer}
			loop.LifecycleParticipants = []LifecycleParticipant{owner}
			loop.MaxToolCalls = 1
			_, err := loop.Run(context.Background(), input)
			events, loadErr := loop.Journal.Load(context.Background(), input.RunID)
			if loadErr != nil {
				t.Fatal(loadErr)
			}
			projection, replayErr := domain.Replay(events)
			if replayErr != nil {
				t.Fatal(replayErr)
			}
			if registry.calls != 1 || registry.releases != 1 || loop.startedOperations != 1 || registry.binding != "release-binding" {
				t.Fatalf("business=%d releases=%d budget=%d err=%v", registry.calls, registry.releases, loop.startedOperations, err)
			}
			if failed {
				if !errors.Is(err, ports.ErrWaitingReconcile) || projection.PendingOperation == nil || projection.PendingOperation.ToolName != fixtureReleaseTool || countEvent(events, domain.EventRunCompleted) != 0 || countEvent(events, domain.EventVerificationStarted) != 0 || observer.notified != 0 {
					t.Fatalf("unconfirmed release finalized: %+v err=%v", projection, err)
				}
				if _, resumeErr := loop.Resume(context.Background(), input, ports.Approval{}); !errors.Is(resumeErr, ports.ErrWaitingReconcile) || registry.releases != 1 {
					t.Fatalf("uncertain release retried: count=%d err=%v", registry.releases, resumeErr)
				}
			} else if err != nil || owner.live || projection.State != domain.RunCompleted || observer.notified != 1 {
				t.Fatalf("release failed: %+v err=%v", projection, err)
			}
		})
	}
}

type hiddenExecutionFixture struct{ ToolRegistry }

func (hiddenExecutionFixture) RequiresAuthorizedExecution() bool { return true }
func (hiddenExecutionFixture) RequiredLifecycleTools() []string {
	return []string{contract.MCPQuiesceTool}
}

// TestLifecycleMissingWiringStopsBeforeProvider 缺失、重复或无效参与者不得请求模型或启动工具。
func TestLifecycleMissingWiringStopsBeforeProvider(t *testing.T) {
	for _, variant := range []string{"missing", "duplicate", "nil", "invalid", "execution", "observer", "visible", "readonly"} {
		t.Run(variant, func(t *testing.T) {
			provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}}}}
			journal := memory.New()
			input := RunInput{RunID: domain.NewRunID(), Task: "wiring", WorkspaceHash: "workspace"}
			appendInitialRunningEvents(t, journal, input.RunID, input.Task)
			catalog := &loopMCPCatalog{}
			loop := newMCPLoopTest(t, catalog, provider, journal)
			switch variant {
			case "missing":
				loop.LifecycleParticipants = nil
			case "duplicate":
				loop.LifecycleParticipants = append(loop.LifecycleParticipants, loop.LifecycleParticipants[0])
			case "nil":
				loop.LifecycleParticipants = []LifecycleParticipant{nil}
			case "invalid":
				loop.LifecycleParticipants = []LifecycleParticipant{&lifecycleFixture{}}
			case "observer":
				loop.ResultObservers = nil
			case "visible":
				loop.Tools = lifecycleRegistryView{ToolRegistry: loop.Tools, visible: true}
			case "readonly":
				loop.Tools = lifecycleRegistryView{ToolRegistry: loop.Tools, readonly: true}
			case "execution":
				loop.Tools = hiddenExecutionFixture{ToolRegistry: loop.Tools}
			}
			if _, err := loop.Run(context.Background(), input); err == nil {
				t.Fatal("incomplete lifecycle wiring ran")
			}
			events, err := journal.Load(context.Background(), input.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) != 0 || len(catalog.calls) != 0 || countEvent(events, domain.EventOperationStarted) != 0 {
				t.Fatal("execution preceded wiring gate")
			}
		})
	}
}

// TestExecutionBindingCannotFallBack 非空执行绑定必须原样交给授权执行端口，不能丢入基础工具。
func TestExecutionBindingCannotFallBack(t *testing.T) {
	registry := &basicOnlyRegistry{}
	loop := &Loop{Tools: registry}
	got := loop.executeToolWithBinding(context.Background(), "read_file", json.RawMessage(`{}`), "opaque-binding")
	if got.OK || registry.calls != 0 || got.Code != "authorized_execution_unavailable" {
		t.Fatal("bound execution fell back to basic registry")
	}
	executor := &bindingExecutorFixture{}
	loop.Tools = executor
	if result := loop.executeToolWithBinding(context.Background(), "read_file", json.RawMessage(`{}`), "opaque-binding"); !result.OK || executor.binding != "opaque-binding" {
		t.Fatal("execution binding lost")
	}
}

type bindingExecutorFixture struct {
	basicOnlyRegistry
	binding string
}

func (executor *bindingExecutorFixture) ExecuteWithBinding(ctx context.Context, name string, raw json.RawMessage, binding string) contract.ToolResult {
	executor.binding = binding
	return executor.basicOnlyRegistry.Execute(ctx, name, raw)
}

// lifecycleRegistryView 只改变夹具声明或副作用分类，验证不完整工具面无法释放资源。
type lifecycleRegistryView struct {
	ToolRegistry
	visible  bool
	readonly bool
}

func (view lifecycleRegistryView) Specs() []ports.ToolSpec {
	specs := append([]ports.ToolSpec(nil), view.ToolRegistry.Specs()...)
	if view.visible {
		specs = append(specs, ports.ToolSpec{Name: contract.MCPQuiesceTool, Parameters: json.RawMessage(`{"type":"object"}`)})
	}
	return specs
}
func (view lifecycleRegistryView) SideEffect(name string) contract.SideEffectClass {
	if view.readonly {
		return contract.SideEffectNone
	}
	return view.ToolRegistry.SideEffect(name)
}

// TestLifecycleModelCannotInvokeRelease 模型不能猜测内部释放名称绕过正常工具面。
func TestLifecycleModelCannotInvokeRelease(t *testing.T) {
	journal := memory.New()
	input := RunInput{RunID: domain.NewRunID(), Task: "model release", WorkspaceHash: "workspace"}
	appendInitialRunningEvents(t, journal, input.RunID, input.Task)
	catalog := &loopMCPCatalog{}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "hidden", Name: contract.MCPQuiesceTool, Arguments: json.RawMessage(`{}`)}}, {Kind: ports.Done}}}}
	loop := newMCPLoopTest(t, catalog, provider, journal)
	if _, err := loop.Run(context.Background(), input); err == nil {
		t.Fatal("model invoked internal release")
	}
	events, err := journal.Load(context.Background(), input.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.calls) != 0 || countEvent(events, domain.EventOperationStarted) != 0 {
		t.Fatal("hidden model call executed")
	}
}

// ExecuteWithBinding 记录真实释放执行接收的授权绑定，基础调用仍复用同一工具实例。
func (registry *lifecycleRegistryFixture) ExecuteWithBinding(ctx context.Context, name string, raw json.RawMessage, binding string) contract.ToolResult {
	if name == fixtureReleaseTool {
		registry.binding = binding
	}
	return registry.Execute(ctx, name, raw)
}
