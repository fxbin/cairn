// delegate_async_test.go 锁定异步委派端到端合同：立即返回与父停靠、子运行
// 期间 steering 可消费（不被同步 Join 卡死）、单写入者门禁、取消传播与
// goroutine 回收、未启用配置失败关闭、重启恢复矩阵（非终态子失败关闭、
// 已完成子经恢复交付）以及同步与异步的编排顺序与预算对比。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// asyncChildResultPrefix 与 agent 层交付前缀保持一致，供消息断言。
const asyncChildResultPrefix = "[异步子任务完成报告]"

// scriptStep 是编排 Provider 单次模型调用的预设输出。
type scriptStep string

const (
	stepDelegateAsync scriptStep = "delegate_async"
	stepDelegateSync  scriptStep = "delegate_sync"
	stepExec          scriptStep = "exec"
	stepText          scriptStep = "text"
)

// orderEvent 记录编排时序中的单次模型调用。
type orderEvent struct {
	label     string
	timestamp time.Time
}

// orderLog 聚合并发模型调用的时序事实，供同步/异步对比断言。
type orderLog struct {
	mu     sync.Mutex
	events []orderEvent
}

// record 追加一次调用时序。
func (log *orderLog) record(label string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append(log.events, orderEvent{label: label, timestamp: time.Now()})
}

// nthIndex 返回指定标签第 nth 次出现（从 1 起）的序号；缺失返回 len+1。
func (log *orderLog) nthIndex(label string, nth int) int {
	log.mu.Lock()
	defer log.mu.Unlock()
	seen := 0
	for index, event := range log.events {
		if event.label == label {
			seen++
			if seen == nth {
				return index
			}
		}
	}
	return len(log.events) + 1
}

// count 返回指定标签的出现次数；缺失返回 0。
func (log *orderLog) count(label string) int {
	log.mu.Lock()
	defer log.mu.Unlock()
	count := 0
	for _, event := range log.events {
		if event.label == label {
			count++
		}
	}
	return count
}

// labels 返回时序标签视图，供失败信息阅读。
func (log *orderLog) labels() []string {
	log.mu.Lock()
	defer log.mu.Unlock()
	labels := make([]string, 0, len(log.events))
	for _, event := range log.events {
		labels = append(labels, event.label)
	}
	return labels
}

// providerCallBarrier 记录单次模型调用的进入与返回时刻：两个通道在对应时刻
// 关闭，等待者无论早晚都立即观察到关闭，不会错过已经发生的事实，也不依赖
// 墙钟轮询。
type providerCallBarrier struct {
	entered    chan struct{}
	returned   chan struct{}
	enterOnce  sync.Once
	returnOnce sync.Once
}

// newProviderCallBarrier 创建一组空的进入与返回信号。
func newProviderCallBarrier() *providerCallBarrier {
	return &providerCallBarrier{entered: make(chan struct{}), returned: make(chan struct{})}
}

// markEntered 标记该次模型调用已进入。
func (barrier *providerCallBarrier) markEntered() {
	barrier.enterOnce.Do(func() { close(barrier.entered) })
}

// markReturned 标记该次模型调用已返回。
func (barrier *providerCallBarrier) markReturned() {
	barrier.returnOnce.Do(func() { close(barrier.returned) })
}

// orchestratingProvider 是异步委派测试的可编排 Provider：按脚本输出
// delegate_task/exec 提议或文本，finalOnSummary 时仅在消息出现子结果摘要后
// 收尾；可选阻塞释放模拟被卡住的子运行。每次模型调用另有进入与返回两个
// 同步点，供用例按因果顺序等待。
type orchestratingProvider struct {
	label          string
	order          *orderLog
	script         []scriptStep
	blockUntil     <-chan struct{}
	finalOnSummary bool
	requests       []ports.ModelRequest
	firstEntered   chan struct{}
	enteredOnce    sync.Once
	calls          atomic.Int32
	barrierMu      sync.Mutex
	barriers       []*providerCallBarrier
}

// newOrchestratingProvider 创建已初始化进入信号的编排 Provider。
func newOrchestratingProvider(label string, order *orderLog) *orchestratingProvider {
	return &orchestratingProvider{label: label, order: order, firstEntered: make(chan struct{})}
}

// callBarrier 返回第 index 次（从 1 起）模型调用的进入与返回屏障，按需创建；
// 调用尚未发生时返回等待中的通道，已发生时返回已关闭的通道。
func (provider *orchestratingProvider) callBarrier(index int) *providerCallBarrier {
	provider.barrierMu.Lock()
	defer provider.barrierMu.Unlock()
	for len(provider.barriers) < index {
		provider.barriers = append(provider.barriers, newProviderCallBarrier())
	}
	return provider.barriers[index-1]
}

// Stream 按脚本编排输出，并记录时序与请求镜像。
func (provider *orchestratingProvider) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	callIndex := int(provider.calls.Add(1))
	barrier := provider.callBarrier(callIndex)
	defer func() {
		provider.order.record(provider.label + "-done")
		barrier.markReturned()
	}()
	provider.requests = append(provider.requests, request)
	provider.enteredOnce.Do(func() { close(provider.firstEntered) })
	provider.order.record(provider.label)
	barrier.markEntered()
	if provider.blockUntil != nil {
		select {
		case <-provider.blockUntil:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	step := stepText
	if callIndex <= len(provider.script) {
		step = provider.script[callIndex-1]
	}
	switch step {
	case stepDelegateAsync, stepDelegateSync:
		arguments := `{"task":"child work","max_turns":5}`
		if step == stepDelegateAsync {
			arguments = `{"task":"child work","async":true,"max_turns":5}`
		}
		return emitToolCall(emit, ports.ToolCall{ID: "delegate-1", Name: "delegate_task", Arguments: json.RawMessage(arguments)})
	case stepExec:
		return emitToolCall(emit, ports.ToolCall{ID: "exec-1", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["true"]}`)})
	default:
		if provider.finalOnSummary {
			for _, message := range request.Messages {
				if strings.Contains(message.Content, asyncChildResultPrefix) {
					if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "父收尾"}); err != nil {
						return err
					}
					return emitTestCompletion(emit)
				}
			}
		}
		if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "等待子任务"}); err != nil {
			return err
		}
		return emitTestCompletion(emit)
	}
}

// emitToolCall 输出一次工具提议并补齐能力卡要求的完成事件。
func emitToolCall(emit func(ports.ModelEvent) error, call ports.ToolCall) error {
	if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// parentCalls 返回已发生的模型调用数。
func (provider *orchestratingProvider) callCount() int {
	return int(provider.calls.Load())
}

// asyncTestConfig 返回启用异步委派的测试配置。
func asyncTestConfig() config.Config {
	configuration := delegateTestConfig()
	configuration.Tools.DelegateAsync = true
	return configuration
}

// splitParentChildProvider 首个 Provider 给父运行，其余给子运行。
func splitParentChildProvider(parent, child *orchestratingProvider) func(config.Config) (ports.Provider, error) {
	var calls int32
	return func(config.Config) (ports.Provider, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return parent, nil
		}
		return child, nil
	}
}

// journalReader 是测试轮询所需的最小 Journal 读取面。
type journalReader interface {
	Load(context.Context, domain.RunID) ([]domain.Event, error)
	RunIDs(context.Context) ([]domain.RunID, error)
}

// countEvents 统计指定类型且载荷包含子串的事件数。
func countEvents(events []domain.Event, eventType domain.EventType, payloadContains string) int {
	count := 0
	for _, event := range events {
		if event.Type != eventType {
			continue
		}
		if payloadContains == "" || strings.Contains(string(event.Payload), payloadContains) {
			count++
		}
	}
	return count
}

// pollJournal 轮询 Journal 直至条件满足或超时，返回满足条件时的事件切片。
func pollJournal(t *testing.T, journal journalReader, runID domain.RunID, condition func([]domain.Event) bool, description string) []domain.Event {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, err := journal.Load(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if condition(events) {
			return events
		}
		time.Sleep(10 * time.Millisecond)
	}
	if events, err := journal.Load(context.Background(), runID); err == nil {
		t.Fatalf("timed out waiting for %s in run %s; events=%s", description, runID, describeEvents(events))
	} else {
		t.Fatalf("timed out waiting for %s in run %s: %v", description, runID, err)
	}
	return nil
}

// describeEvents 输出事件类型序列，供超时诊断。
func describeEvents(events []domain.Event) string {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, string(event.Type))
	}
	return strings.Join(types, ",")
}

// findParentRunWithDelegation 定位含委派事实的父运行；尚未出现时返回缺席。
func findParentRunWithDelegation(t *testing.T, journal journalReader) (domain.RunID, bool) {
	t.Helper()
	runIDs, err := journal.RunIDs(context.Background())
	if err != nil {
		return "", false
	}
	for _, runID := range runIDs {
		events, err := journal.Load(context.Background(), runID)
		if err != nil {
			continue
		}
		if countEvents(events, domain.EventSubagentDelegated, "") > 0 {
			return runID, true
		}
	}
	return "", false
}

// parentRunWithDelegation 轮询直至含委派事实的父运行出现，超时失败。
func parentRunWithDelegation(t *testing.T, journal journalReader) domain.RunID {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runID, found := findParentRunWithDelegation(t, journal); found {
			return runID
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("parent run with delegation not found")
	return ""
}

// TestAsyncDelegateParksParentAndDeliversChildResult 锁定异步委派主链路：
// delegate_task(async) 立即返回，父停靠等待子终态，安全点把子摘要作为下轮
// 交付注入后续模型请求，父随后按既有流程收尾；交付事实恰好一对。
func TestAsyncDelegateParksParentAndDeliversChildResult(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := asyncTestConfig()
	order := &orderLog{}
	parent := newOrchestratingProvider("parent", order)
	parent.script = []scriptStep{stepDelegateAsync}
	parent.finalOnSummary = true
	child := newOrchestratingProvider("child", order)
	service := Service{NewProvider: splitParentChildProvider(parent, child)}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	if result.Response != "父收尾" {
		t.Fatalf("parent final response = %q", result.Response)
	}
	childID := onlyChildRun(t, inspected.Projection)
	if record := inspected.Projection.Children[childID]; record.State != "completed" || record.Outcome != "ok" {
		t.Fatalf("child record = %+v", record)
	}
	if got := countEvents(inspected.Events, domain.EventSteeringInputSubmitted, "subagent_result"); got != 1 {
		t.Fatalf("subagent_result submitted facts = %d, want 1", got)
	}
	if got := countEvents(inspected.Events, domain.EventSteeringInputConsumed, "subagent_result"); got != 1 {
		t.Fatalf("subagent_result consumed facts = %d, want 1", got)
	}
	deliveredToModel := false
	for _, request := range parent.requests[1:] {
		for _, message := range request.Messages {
			if strings.Contains(message.Content, asyncChildResultPrefix) {
				deliveredToModel = true
			}
		}
	}
	if !deliveredToModel {
		t.Fatal("child summary must reach a later model request")
	}
}

// TestAsyncDelegateAcceptsSteeringWhileChildRuns 锁定验收一：异步子运行期间父
// 可接受并消费 steering，不被同步 Join 卡死；消费事实先于子完成事实落盘。
func TestAsyncDelegateAcceptsSteeringWhileChildRuns(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := asyncTestConfig()
	order := &orderLog{}
	release := make(chan struct{})
	parent := newOrchestratingProvider("parent", order)
	parent.script = []scriptStep{stepDelegateAsync}
	parent.finalOnSummary = true
	child := newOrchestratingProvider("child", order)
	child.blockUntil = release
	service := Service{NewProvider: splitParentChildProvider(parent, child)}
	bridge := agent.NewSteeringBridge()
	type runOutcome struct {
		result RunResult
		err    error
	}
	outcome := make(chan runOutcome, 1)
	go func() {
		result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration, Steering: bridge})
		outcome <- runOutcome{result: result, err: err}
	}()
	<-child.firstEntered
	for parent.callCount() < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	if err := bridge.Submit("steer-async-1", "补充当前任务指导"); err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	parentRunID := parentRunWithDelegation(t, journal)
	var steeringEvents []domain.Event
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		events, loadErr := journal.Load(context.Background(), parentRunID)
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		if countEvents(events, domain.EventSteeringInputConsumed, "steer-async-1") > 0 {
			steeringEvents = events
			break
		}
		select {
		case finished := <-outcome:
			t.Fatalf("run finished early while steering was pending: err=%v events=%s", finished.err, describeEvents(events))
		default:
		}
		time.Sleep(10 * time.Millisecond)
	}
	if steeringEvents == nil {
		events, _ := journal.Load(context.Background(), parentRunID)
		t.Fatalf("timed out waiting for steering consumption; events=%s", describeEvents(events))
	}
	if countEvents(steeringEvents, domain.EventSubagentCompleted, "") > 0 {
		t.Fatal("steering must be consumable before the blocked child finishes")
	}
	close(release)
	finished := <-outcome
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	parentEvents, err := journal.Load(context.Background(), finished.result.ID)
	if err != nil {
		t.Fatal(err)
	}
	steeringSeq, completedSeq := uint64(0), uint64(0)
	for _, event := range parentEvents {
		switch {
		case event.Type == domain.EventSteeringInputConsumed && strings.Contains(string(event.Payload), "steer-async-1"):
			steeringSeq = event.Seq
		case event.Type == domain.EventSubagentCompleted:
			completedSeq = event.Seq
		}
	}
	if steeringSeq == 0 || completedSeq == 0 || steeringSeq > completedSeq {
		t.Fatalf("steering seq=%d must precede child completion seq=%d", steeringSeq, completedSeq)
	}
}

// TestAsyncDelegateRefusesParentWritesWhileChildRuns 锁定单写入者门禁：异步子
// 活跃期间父的副作用工具与再次委派都在操作启动前以专用失败码拒绝，只读
// 工具不受限，父继续停靠并在子完成后正常收尾。
func TestAsyncDelegateRefusesParentWritesWhileChildRuns(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := asyncTestConfig()
	order := &orderLog{}
	release := make(chan struct{})
	parent := newOrchestratingProvider("parent", order)
	parent.script = []scriptStep{stepDelegateAsync, stepExec, stepDelegateAsync}
	parent.finalOnSummary = true
	child := newOrchestratingProvider("child", order)
	child.blockUntil = release
	service := Service{NewProvider: splitParentChildProvider(parent, child)}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	type runOutcome struct {
		result RunResult
		err    error
	}
	outcome := make(chan runOutcome, 1)
	go func() {
		result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
		outcome <- runOutcome{result: result, err: err}
	}()
	parentRunID := parentRunWithDelegation(t, journal)
	refusedEvents := pollJournal(t, journal, parentRunID, func(events []domain.Event) bool {
		return countEvents(events, domain.EventOperationFailed, "async_child_writer_active") >= 2
	}, "two refused parent write attempts")
	if countEvents(refusedEvents, domain.EventOperationStarted, "exec_command") > 0 {
		t.Fatal("refused parent side-effect call must never start an operation")
	}
	close(release)
	finished := <-outcome
	if finished.err != nil {
		t.Fatal(finished.err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, finished.result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted || len(inspected.Projection.Children) != 1 {
		t.Fatalf("parent projection = %+v", inspected.Projection)
	}
}

// TestAsyncDelegateCancelPropagatesAndReclaimsRunner 锁定取消传播与回收：父
// 取消时停靠解除、子运行按取消收尾、父 Run 返回前等待派发 goroutine 退出，
// 两个 Journal 都保留完整取消证据。
func TestAsyncDelegateCancelPropagatesAndReclaimsRunner(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := asyncTestConfig()
	order := &orderLog{}
	release := make(chan struct{})
	parent := newOrchestratingProvider("parent", order)
	parent.script = []scriptStep{stepDelegateAsync}
	parent.finalOnSummary = true
	child := newOrchestratingProvider("child", order)
	child.blockUntil = release
	service := Service{NewProvider: splitParentChildProvider(parent, child)}
	ctx, cancel := context.WithCancel(context.Background())
	type runOutcome struct {
		result RunResult
		err    error
	}
	outcome := make(chan runOutcome, 1)
	go func() {
		result, err := service.Run(ctx, RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
		outcome <- runOutcome{result: result, err: err}
	}()
	<-child.firstEntered
	cancel()
	finished := <-outcome
	if !errors.Is(finished.err, context.Canceled) {
		t.Fatalf("cancelled parent run error = %v", finished.err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, finished.result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCancelled {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	childID := onlyChildRun(t, inspected.Projection)
	childConfiguration := configuration
	childConfiguration.Budget.MaxTurns = 5
	childInspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, childConfiguration, domain.RunID(childID), true)
	if err != nil {
		t.Fatal(err)
	}
	if childInspected.Projection.State != domain.RunCancelled {
		t.Fatalf("child state = %s", childInspected.Projection.State)
	}
	close(release)
}

// TestAsyncDelegateFailsClosedWhenConfigDisabled 锁定配置门禁：未启用
// tools.delegate_async 时 async 请求失败关闭，不产生子运行，父按既有流程收尾。
func TestAsyncDelegateFailsClosedWhenConfigDisabled(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := delegateTestConfig()
	order := &orderLog{}
	parent := newOrchestratingProvider("parent", order)
	parent.script = []scriptStep{stepDelegateAsync}
	child := newOrchestratingProvider("child", order)
	service := Service{NewProvider: splitParentChildProvider(parent, child)}
	result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	if len(inspected.Projection.Children) != 0 {
		t.Fatalf("disabled async delegation must not create children: %+v", inspected.Projection.Children)
	}
	if countEvents(inspected.Events, domain.EventToolResultAppended, "async_delegate_disabled") == 0 {
		t.Fatal("model must see the async_delegate_disabled failure result")
	}
}

// seedParentWithChild 手工构造带子事实的运行中父 Journal（重启遗留现场）。
func seedParentWithChild(t *testing.T, stateDir, workspaceHash string, configuration config.Config, childCompleted bool) domain.RunID {
	t.Helper()
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	configRef := putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
	parentRunID, childRunID := domain.NewRunID(), domain.NewRunID()
	created := domain.EventDraft{
		RunID: parentRunID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated,
		Payload: artifact.MustEncodeRedactedJSON(struct {
			domain.RunCreatedPayload
			ConfigRef artifact.Ref `json:"config_ref"`
		}{RunCreatedPayload: domain.RunCreatedPayload{Task: "parent task", Workspace: "/seeded", ConfigFingerprint: configuration.Fingerprint()}, ConfigRef: configRef}),
	}
	started := domain.EventDraft{RunID: parentRunID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted}
	delegated := domain.EventDraft{RunID: parentRunID, WorkspaceHash: workspaceHash, Type: domain.EventSubagentDelegated, Payload: artifact.MustEncodeRedactedJSON(DelegatedPayload{ChildRunID: string(childRunID), TaskDigest: TaskDigest("child work"), Budget: SubagentBudget{MaxTurns: 5}})}
	seed := []domain.EventDraft{created, started, delegated}
	if childCompleted {
		seed = append(seed, domain.EventDraft{RunID: parentRunID, WorkspaceHash: workspaceHash, Type: domain.EventSubagentCompleted, Payload: artifact.MustEncodeRedactedJSON(CompletedPayload{ChildRunID: string(childRunID), Outcome: "ok"})})
	}
	for index, draft := range seed {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
	return parentRunID
}

// TestResumeRejectsParentWithUnfinishedAsyncChild 锁定重启恢复矩阵：异步委派
// 启用时，非终态子未收束的父运行 resume 失败关闭并引导显式恢复；旧配置口径
// （开关关闭）不受新门禁影响。
func TestResumeRejectsParentWithUnfinishedAsyncChild(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := asyncTestConfig()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	parentRunID := seedParentWithChild(t, stateDir, hash, configuration, false)
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	_, err = service.Resume(context.Background(), ResumeOptions{RunID: parentRunID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err == nil || !strings.Contains(err.Error(), "have not finished") || !strings.Contains(err.Error(), "abort-run") {
		t.Fatalf("resume must fail closed with child guidance: %v", err)
	}
	// 旧配置口径（开关关闭）以其自身冻结指纹另建运行，不受新门禁影响，
	// 保持既有恢复语义可达。
	legacy := delegateTestConfig()
	_, legacyHash, hashErr := workspaceFor(workspace, legacy)
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	legacyParentRunID := seedParentWithChild(t, stateDir, legacyHash, legacy, false)
	if _, legacyErr := service.Resume(context.Background(), ResumeOptions{RunID: legacyParentRunID, Workspace: workspace, StateDir: stateDir, Config: legacy}); legacyErr != nil {
		t.Fatalf("legacy resume must keep existing semantics: %v", legacyErr)
	}
}

// TestResumeParentDeliversCompletedChildResult 锁定重启交付：子已完成的父运行
// 经恢复续跑时，安全点把未交付的子终态摘要注入下一次模型请求后收尾。
func TestResumeParentDeliversCompletedChildResult(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := asyncTestConfig()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	parentRunID := seedParentWithChild(t, stateDir, hash, configuration, true)
	order := &orderLog{}
	parent := newOrchestratingProvider("parent", order)
	parent.finalOnSummary = true
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return parent, nil }}
	result, err := service.Resume(context.Background(), ResumeOptions{RunID: parentRunID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("parent state = %s", inspected.Projection.State)
	}
	if got := countEvents(inspected.Events, domain.EventSteeringInputConsumed, "subagent_result"); got != 1 {
		t.Fatalf("resume must deliver the completed child summary exactly once, got %d", got)
	}
}

// delegationOrderingTimeout 是编排顺序用例等待因果同步点的上限：超时即判定
// 顺序约束被破坏，打印可诊断的模型调用顺序后失败，不做无限等待。
const delegationOrderingTimeout = 10 * time.Second

// delegationOrderingScenario 是一次委派编排场景的观测结果：模型调用顺序、
// 父运行事件与预算汇总，供同步与异步两种口径对比断言。
type delegationOrderingScenario struct {
	order  *orderLog
	events []domain.Event
	budget BudgetSummary
}

// runDelegationOrderingScenario 跑一次委派编排场景并返回可断言的观测结果。
// 同步口径下父在子 Run 完成后才进入下一步，异步口径下父在子被阻塞时推进到
// 下一次模型调用；两条路径都由 channel 同步点推进，子是否完成与父是否推进
// 之间不存在墙钟竞态。
func runDelegationOrderingScenario(t *testing.T, async bool) delegationOrderingScenario {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := asyncTestConfig()
	order := &orderLog{}
	release := make(chan struct{})
	parent := newOrchestratingProvider("parent", order)
	parent.finalOnSummary = true
	child := newOrchestratingProvider("child", order)
	child.blockUntil = release
	if async {
		parent.script = []scriptStep{stepDelegateAsync}
	} else {
		parent.script = []scriptStep{stepDelegateSync}
	}
	service := Service{NewProvider: splitParentChildProvider(parent, child)}
	type runOutcome struct {
		result RunResult
		err    error
	}
	outcome := make(chan runOutcome, 1)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
		outcome <- runOutcome{result: result, err: err}
	}()
	// 收尾：无条件释放子 Provider 并等待运行 goroutine 退出，既不留悬挂
	// 等待，也不让一个场景的残留影响另一个场景。
	var releaseOnce sync.Once
	releaseChild := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		releaseChild()
		select {
		case <-finished:
		case <-time.After(delegationOrderingTimeout):
			t.Errorf("delegation scenario did not finish within %s", delegationOrderingTimeout)
		}
	})
	// 子 Provider 进入首次模型调用后即被 release 阻塞：这是「子已开始、尚未
	// 完成」的因果同步点，与调度次序无关。
	waitForOrderingSignal(t, child.callBarrier(1).entered, "child first model call", order)
	if async {
		// 异步口径：子仍被阻塞时父必须已推进到下一次模型调用；若实现退化为
		// 同步等待子完成，这里的有界等待会超时失败。
		waitForOrderingSignal(t, parent.callBarrier(2).entered, "parent second model call while the child is blocked", order)
	}
	releaseChild()
	waitForOrderingSignal(t, finished, "parent run completion", order)
	completed := <-outcome
	if completed.err != nil {
		t.Fatalf("parent run failed: %v; model call order=%v", completed.err, order.labels())
	}
	inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, completed.result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	return delegationOrderingScenario{order: order, events: inspected.Events, budget: SummarizeBudget(inspected.Events)}
}

// waitForOrderingSignal 等待一个因果同步点；超时即失败并打印可诊断的模型
// 调用顺序，不做无限等待。
func waitForOrderingSignal(t *testing.T, signal <-chan struct{}, description string, order *orderLog) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(delegationOrderingTimeout):
		t.Fatalf("timed out after %s waiting for %s; model call order=%v", delegationOrderingTimeout, description, order.labels())
	}
}

// assertOrderBefore 断言 first 的第 nth 次出现早于 second 的第 mth 次出现；
// 任一标记缺失即失败，缺失不得当作通过。
func assertOrderBefore(t *testing.T, log *orderLog, first string, nth int, second string, mth int, message string) {
	t.Helper()
	if log.count(first) < nth || log.count(second) < mth {
		t.Fatalf("%s: missing order markers %s#%d or %s#%d in %v", message, first, nth, second, mth, log.labels())
	}
	firstIndex, secondIndex := log.nthIndex(first, nth), log.nthIndex(second, mth)
	if firstIndex > secondIndex {
		t.Fatalf("%s: %s#%d at %d must precede %s#%d at %d in %v", message, first, nth, firstIndex, second, mth, secondIndex, log.labels())
	}
}

// turnOpenedSeq 返回父运行第 nth 次开放轮次的事件序号；缺失返回 0。
func turnOpenedSeq(events []domain.Event, nth int) uint64 {
	seen := 0
	for _, event := range events {
		if event.Type != domain.EventTurnOpened {
			continue
		}
		seen++
		if seen == nth {
			return event.Seq
		}
	}
	return 0
}

// subagentCompletedSeq 返回父运行中首个子完成事实的事件序号；缺失返回 0。
func subagentCompletedSeq(events []domain.Event) uint64 {
	for _, event := range events {
		if event.Type == domain.EventSubagentCompleted {
			return event.Seq
		}
	}
	return 0
}

// TestSyncVersusAsyncDelegationOrdering 锁定编排顺序与预算口径对比：同步委派
// 在子 Run 完成事实落盘前父不进入下一步，异步委派在子仍被阻塞时父已推进到
// 下一次模型调用；两种口径的父事件预算汇总保持同构（单子、同委派额度）。
// 顺序由 channel 同步点与父 Journal 事件序号给出，不依赖 goroutine 抢占次序。
func TestSyncVersusAsyncDelegationOrdering(t *testing.T) {
	syncScenario := runDelegationOrderingScenario(t, false)
	syncCompletionSeq := subagentCompletedSeq(syncScenario.events)
	syncSecondTurnSeq := turnOpenedSeq(syncScenario.events, 2)
	if syncCompletionSeq == 0 || syncSecondTurnSeq == 0 {
		t.Fatalf("sync scenario must record one child completion and two parent turns: %s", describeEvents(syncScenario.events))
	}
	if syncCompletionSeq > syncSecondTurnSeq {
		t.Fatalf("sync delegation must keep the parent behind the child run: child completion seq=%d second turn seq=%d events=%s", syncCompletionSeq, syncSecondTurnSeq, describeEvents(syncScenario.events))
	}
	assertOrderBefore(t, syncScenario.order, "child", 1, "parent", 2, "sync delegation must block the parent behind the child")
	asyncScenario := runDelegationOrderingScenario(t, true)
	asyncCompletionSeq := subagentCompletedSeq(asyncScenario.events)
	asyncSecondTurnSeq := turnOpenedSeq(asyncScenario.events, 2)
	if asyncCompletionSeq == 0 || asyncSecondTurnSeq == 0 {
		t.Fatalf("async scenario must record one child completion and two parent turns: %s", describeEvents(asyncScenario.events))
	}
	if asyncSecondTurnSeq > asyncCompletionSeq {
		t.Fatalf("async parent must advance before the child run completes: second turn seq=%d child completion seq=%d events=%s", asyncSecondTurnSeq, asyncCompletionSeq, describeEvents(asyncScenario.events))
	}
	assertOrderBefore(t, asyncScenario.order, "parent", 2, "child-done", 1, "async parent must advance while the child is blocked")
	if syncScenario.budget.UniqueChildren != 1 || asyncScenario.budget.UniqueChildren != 1 || syncScenario.budget.DelegatedTurns != asyncScenario.budget.DelegatedTurns {
		t.Fatalf("budget summaries must stay comparable: sync=%+v async=%+v", syncScenario.budget, asyncScenario.budget)
	}
}
