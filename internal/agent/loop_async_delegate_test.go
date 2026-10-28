// loop_async_delegate_test.go 锁定异步委派在 Agent Loop 层的停靠、交付与
// 单写入者门禁：park 等待子完成或 steering、无活跃信号失败关闭、子终态摘要
// 一次性持久化交付（resume 回放不重复注入）、停靠期间父副作用工具拒绝且
// 开关关闭时行为不变。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// asyncFixture 组装启用异步委派语义的最小 Loop 事实：已开启的运行、开放轮次
// 与可选的子事实，供停靠与交付路径直测。
type asyncFixture struct {
	journal *memory.Journal
	loop    *Loop
	runID   domain.RunID
}

// newAsyncFixture 建立运行事实并装配带制品库的 Loop。
func newAsyncFixture(t *testing.T) *asyncFixture {
	t.Helper()
	journal := memory.New()
	runID := domain.NewRunID()
	ctx := context.Background()
	seed := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "ws", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"parent","workspace":"/ws"}`)},
		{RunID: runID, WorkspaceHash: "ws", Type: domain.EventRunStarted},
	}
	for index, draft := range seed {
		if _, err := journal.Append(ctx, uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
	store, err := artifact.New(filepath.Join(t.TempDir(), "artifacts"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Journal: journal, Artifacts: store, Model: "fake", MaxTurns: 5}
	loop.steeringInbox = make(chan SteeringRequest, 8)
	loop.steeringConsumed = map[string]struct{}{}
	return &asyncFixture{journal: journal, loop: loop, runID: runID}
}

// appendEvent 追加一条父运行事件。
func (fixture *asyncFixture) appendEvent(t *testing.T, draft domain.EventDraft) {
	t.Helper()
	events, err := fixture.journal.Load(context.Background(), fixture.runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.journal.Append(context.Background(), uint64(len(events)), draft); err != nil {
		t.Fatal(err)
	}
}

// openTurn 追加开放轮次并返回轮次标识。
func (fixture *asyncFixture) openTurn(t *testing.T) domain.TurnID {
	t.Helper()
	turnID := domain.NewTurnID()
	fixture.appendEvent(t, domain.EventDraft{RunID: fixture.runID, WorkspaceHash: "ws", Type: domain.EventTurnOpened, TurnID: &turnID, Payload: json.RawMessage(`{"turn":1}`)})
	return turnID
}

// appendChildFact 向父运行追加一个委派或完成事实（外部写入路径）。
func (fixture *asyncFixture) appendChildFact(t *testing.T, childRunID string, completed bool, outcome string) {
	t.Helper()
	if completed {
		fixture.appendEvent(t, domain.EventDraft{RunID: fixture.runID, WorkspaceHash: "ws", Type: domain.EventSubagentCompleted, Payload: mustJSON(map[string]any{"child_run_id": childRunID, "outcome": outcome})})
		return
	}
	fixture.appendEvent(t, domain.EventDraft{RunID: fixture.runID, WorkspaceHash: "ws", Type: domain.EventSubagentDelegated, Payload: mustJSON(map[string]any{"child_run_id": childRunID})})
}

// seedChildRun 写入子运行的最小事实（创建、启动、完成与最终回复）。
func (fixture *asyncFixture) seedChildRun(t *testing.T, childRunID string, finalResponse string) {
	t.Helper()
	seed := []domain.EventDraft{
		{RunID: domain.RunID(childRunID), WorkspaceHash: "ws", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"child","workspace":"/ws"}`)},
		{RunID: domain.RunID(childRunID), WorkspaceHash: "ws", Type: domain.EventRunStarted},
		{RunID: domain.RunID(childRunID), WorkspaceHash: "ws", Type: domain.EventRunCompleted, Payload: mustJSON(map[string]any{"final_response": finalResponse})},
	}
	for index, draft := range seed {
		if _, err := fixture.journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
}

// projection 重放父运行投影。
func (fixture *asyncFixture) projection(t *testing.T) domain.RunProjection {
	t.Helper()
	events, err := fixture.journal.Load(context.Background(), fixture.runID)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	return projection
}

// assertContinueTurnClose 断言父运行存在 outcome=continue 的轮次关闭事实。
func assertContinueTurnClose(t *testing.T, fixture *asyncFixture) {
	t.Helper()
	events, _ := fixture.journal.Load(context.Background(), fixture.runID)
	for _, event := range events {
		if event.Type != domain.EventTurnClosed {
			continue
		}
		var payload domain.TurnClosedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Outcome == "continue" {
			return
		}
	}
	t.Fatal("expected a turn_closed fact with outcome continue")
}

// TestParkForPendingAsyncChildrenWakesOnChildSettle 锁定停靠唤醒：非终态子在
// 完成信号关闭后唤醒，停靠以 continue 关闭轮次并保留助手消息。等待停靠落盘
// 的 continue 轮次关闭事实后再收束子信号，保证时序确定。
func TestParkForPendingAsyncChildrenWakesOnChildSettle(t *testing.T) {
	fixture := newAsyncFixture(t)
	turnID := fixture.openTurn(t)
	childRunID := string(domain.NewRunID())
	fixture.appendChildFact(t, childRunID, false, "")
	fixture.loop.AsyncDelegateEnabled = true
	fixture.loop.AsyncChildren = NewAsyncChildSet()
	fixture.loop.AsyncChildren.Launch(childRunID)
	projection := fixture.projection(t)
	type parkOutcome struct {
		parked bool
		err    error
	}
	outcome := make(chan parkOutcome, 1)
	go func() {
		parked, err := fixture.loop.parkForPendingAsyncChildren(context.Background(), &projection, RunInput{RunID: fixture.runID, WorkspaceHash: "ws"}, turnID, "等待子任务")
		outcome <- parkOutcome{parked: parked, err: err}
	}()
	waitForEvent(t, fixture, domain.EventTurnClosed)
	// 真实时序：子 goroutine 先完成回写（subagent_completed）再关闭完成信号。
	fixture.appendChildFact(t, childRunID, true, "ok")
	fixture.loop.AsyncChildren.Settle(childRunID)
	result := <-outcome
	if result.err != nil || !result.parked {
		t.Fatalf("park = parked:%v err:%v", result.parked, result.err)
	}
	assertContinueTurnClose(t, fixture)
}

// waitForEvent 轮询父 Journal 直至出现指定事件类型，超时失败。
func waitForEvent(t *testing.T, fixture *asyncFixture, eventType domain.EventType) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		events, err := fixture.journal.Load(context.Background(), fixture.runID)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events {
			if event.Type == eventType {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", eventType)
}

// TestParkForPendingAsyncChildrenWakesOnSteering 锁定停靠期间 steering 可投递：
// 收件箱到达即唤醒并回到安全点，父运行不被同步 Join 卡死。
func TestParkForPendingAsyncChildrenWakesOnSteering(t *testing.T) {
	fixture := newAsyncFixture(t)
	turnID := fixture.openTurn(t)
	childRunID := string(domain.NewRunID())
	fixture.appendChildFact(t, childRunID, false, "")
	fixture.loop.AsyncDelegateEnabled = true
	fixture.loop.AsyncChildren = NewAsyncChildSet()
	fixture.loop.AsyncChildren.Launch(childRunID)
	if err := fixture.loop.SubmitSteering(SteeringRequest{InputID: "steer-1", Content: "补充指导"}); err != nil {
		t.Fatal(err)
	}
	projection := fixture.projection(t)
	parked, err := fixture.loop.parkForPendingAsyncChildren(context.Background(), &projection, RunInput{RunID: fixture.runID, WorkspaceHash: "ws"}, turnID, "等待子任务")
	if err != nil || !parked {
		t.Fatalf("park = parked:%v err:%v", parked, err)
	}
	fixture.loop.AsyncChildren.Settle(childRunID)
}

// TestParkForPendingAsyncChildrenFailsClosedWithoutLiveRunner 锁定失败关闭：
// 非终态子缺少本进程活跃信号（进程重启遗留或同步审批暂停子）时停靠失败关闭
// 并引导显式恢复，不轮询猜测子状态。
func TestParkForPendingAsyncChildrenFailsClosedWithoutLiveRunner(t *testing.T) {
	fixture := newAsyncFixture(t)
	turnID := fixture.openTurn(t)
	fixture.appendChildFact(t, string(domain.NewRunID()), false, "")
	fixture.loop.AsyncDelegateEnabled = true
	fixture.loop.AsyncChildren = NewAsyncChildSet()
	projection := fixture.projection(t)
	_, err := fixture.loop.parkForPendingAsyncChildren(context.Background(), &projection, RunInput{RunID: fixture.runID, WorkspaceHash: "ws"}, turnID, "等待子任务")
	if err == nil || !strings.Contains(err.Error(), "no live runner") {
		t.Fatalf("park must fail closed with recovery guidance: %v", err)
	}
}

// TestDeliverCompletedAsyncChildrenPersistsSingleDelivery 锁定安全点交付：
// 已完成子以 steering submitted/consumed 对持久化一次，重复调用不重复注入；
// 消费去重表从事件重建（resume 口径）后仍不重复，消息重建按 kind 注入子
// 结果前缀。
func TestDeliverCompletedAsyncChildrenPersistsSingleDelivery(t *testing.T) {
	fixture := newAsyncFixture(t)
	childRunID := string(domain.NewRunID())
	fixture.appendChildFact(t, childRunID, false, "")
	fixture.appendChildFact(t, childRunID, true, "ok")
	fixture.seedChildRun(t, childRunID, "子任务已完成全部工作")
	fixture.loop.AsyncDelegateEnabled = true
	input := RunInput{RunID: fixture.runID, WorkspaceHash: "ws"}
	projection := fixture.projection(t)
	messages, err := fixture.loop.deliverCompletedAsyncChildren(context.Background(), &projection, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || !strings.Contains(messages[0].Content, asyncChildResultMessagePrefix) || !strings.Contains(messages[0].Content, "子任务已完成全部工作") {
		t.Fatalf("delivered messages = %#v", messages)
	}
	second, err := fixture.loop.deliverCompletedAsyncChildren(context.Background(), &projection, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 0 {
		t.Fatalf("delivery must be idempotent, second = %#v", second)
	}
	events, _ := fixture.journal.Load(context.Background(), fixture.runID)
	submitted, consumed := 0, 0
	for _, event := range events {
		var payload struct {
			Kind string `json:"kind"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.Kind != steeringKindSubagentResult {
			continue
		}
		switch event.Type {
		case domain.EventSteeringInputSubmitted:
			submitted++
		case domain.EventSteeringInputConsumed:
			consumed++
		}
	}
	if submitted != 1 || consumed != 1 {
		t.Fatalf("delivery facts submitted=%d consumed=%d, want 1/1", submitted, consumed)
	}
	fixture.loop.steeringConsumed = steeringConsumedFromEvents(events)
	replayed, err := fixture.loop.deliverCompletedAsyncChildren(context.Background(), &projection, input, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(replayed) != 0 {
		t.Fatalf("replayed delivery must stay empty, got %#v", replayed)
	}
	rebuilt, err := fixture.loop.rebuildMessagesFromEvents(events, "parent", false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range rebuilt {
		if strings.Contains(message.Content, asyncChildResultMessagePrefix) {
			found = true
		}
	}
	if !found {
		t.Fatalf("rebuilt messages must carry the subagent result summary: %#v", rebuilt)
	}
}

// sideEffectStubTool 是带副作用的可执行测试工具。
type sideEffectStubTool struct {
	calls int
}

func (tool *sideEffectStubTool) Spec() ports.ToolSpec {
	return ports.ToolSpec{Name: "exec_command", Parameters: json.RawMessage(`{"type":"object"}`)}
}

func (tool *sideEffectStubTool) SideEffect() tools.SideEffectClass { return tools.SideEffectProcess }

func (tool *sideEffectStubTool) Execute(_ context.Context, _ json.RawMessage) tools.Result {
	tool.calls++
	return tools.Success(struct{}{}, "executed", 0, "", false)
}

// TestExecuteToolRefusesSideEffectWhileAsyncChildActive 锁定单写入者门禁：
// 异步子活跃期间父副作用类工具在操作启动前失败关闭（专用失败码、完整审计
// 链、不执行副作用），开关关闭时既有行为完全不变。
func TestExecuteToolRefusesSideEffectWhileAsyncChildActive(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	for _, enabled := range []bool{true, false} {
		fixture := newAsyncFixture(t)
		tool := &sideEffectStubTool{}
		registry, err := tools.NewRegistry(tool)
		if err != nil {
			t.Fatal(err)
		}
		fixture.loop.Tools = registry
		fixture.loop.Gate = gate
		fixture.loop.AsyncDelegateEnabled = enabled
		turnID := fixture.openTurn(t)
		fixture.appendChildFact(t, string(domain.NewRunID()), false, "")
		call := ports.ToolCall{ID: "exec-1", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["true"]}`)}
		projection := fixture.projection(t)
		result, err := fixture.loop.executeTool(context.Background(), &projection, RunInput{RunID: fixture.runID, WorkspaceHash: "ws"}, turnID, call)
		if err != nil {
			t.Fatal(err)
		}
		if enabled {
			if result.OK || result.Code != asyncChildWriterBlockedCode {
				t.Fatalf("side-effect call must fail closed while async child active: %+v", result)
			}
			if tool.calls != 0 {
				t.Fatal("refused side-effect tool must not execute")
			}
			continue
		}
		if !result.OK || tool.calls != 1 {
			t.Fatalf("disabled gate must keep existing behavior: %+v calls=%d", result, tool.calls)
		}
	}
}
