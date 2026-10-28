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

// orchestratingProvider 是异步委派测试的可编排 Provider：按脚本输出
// delegate_task/exec 提议或文本，finalOnSummary 时仅在消息出现子结果摘要后
// 收尾；可选阻塞释放与模拟工作时长模拟慢子运行。
type orchestratingProvider struct {
	label          string
	order          *orderLog
	script         []scriptStep
	blockUntil     <-chan struct{}
	workPause      time.Duration
	finalOnSummary bool
	requests       []ports.ModelRequest
	firstEntered   chan struct{}
	enteredOnce    sync.Once
	calls          atomic.Int32
}

// newOrchestratingProvider 创建已初始化进入信号的编排 Provider。
func newOrchestratingProvider(label string, order *orderLog) *orchestratingProvider {
	return &orchestratingProvider{label: label, order: order, firstEntered: make(chan struct{})}
}

// Stream 按脚本编排输出，并记录时序与请求镜像。
func (provider *orchestratingProvider) Stream(ctx context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	callIndex := int(provider.calls.Add(1))
	provider.requests = append(provider.requests, request)
	provider.enteredOnce.Do(func() { close(provider.firstEntered) })
	provider.order.record(provider.label)
	if provider.blockUntil != nil {
		select {
		case <-provider.blockUntil:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if provider.workPause > 0 {
		select {
		case <-time.After(provider.workPause):
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

// TestSyncVersusAsyncDelegationOrdering 锁定编排顺序与预算口径对比：同步委派
// 阻塞父的后续模型调用（子先于父第二次调用），异步委派在子未完成时父已推进
// 到停靠前的等待调用；两种口径的父事件预算汇总保持同构（单子、同委派额度）。
func TestSyncVersusAsyncDelegationOrdering(t *testing.T) {
	runScenario := func(async bool) (*orderLog, BudgetSummary) {
		workspace, stateDir := t.TempDir(), t.TempDir()
		configuration := asyncTestConfig()
		order := &orderLog{}
		release := make(chan struct{})
		step := stepDelegateSync
		if async {
			step = stepDelegateAsync
		}
		parent := newOrchestratingProvider("parent", order)
		parent.script = []scriptStep{step}
		parent.finalOnSummary = true
		var child *orchestratingProvider
		if async {
			child = newOrchestratingProvider("child", order)
			child.blockUntil = release
		} else {
			child = newOrchestratingProvider("child", order)
			child.workPause = 50 * time.Millisecond
		}
		service := Service{NewProvider: splitParentChildProvider(parent, child)}
		if async {
			go func() {
				<-child.firstEntered
				for parent.callCount() < 2 {
					time.Sleep(time.Millisecond)
				}
				close(release)
			}()
		}
		result, err := service.Run(context.Background(), RunOptions{Task: "parent task", Workspace: workspace, StateDir: stateDir, Config: configuration})
		if err != nil {
			t.Fatal(err)
		}
		inspected, err := (Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
		if err != nil {
			t.Fatal(err)
		}
		return order, SummarizeBudget(inspected.Events)
	}
	syncOrder, syncBudget := runScenario(false)
	if syncOrder.nthIndex("child", 1) > syncOrder.nthIndex("parent", 2) {
		t.Fatalf("sync delegation must block the parent behind the child: %v", syncOrder.labels())
	}
	asyncOrder, asyncBudget := runScenario(true)
	if asyncOrder.nthIndex("parent", 2) > asyncOrder.nthIndex("child", 1) {
		t.Fatalf("async parent must advance while the child is blocked: %v", asyncOrder.labels())
	}
	if syncBudget.UniqueChildren != 1 || asyncBudget.UniqueChildren != 1 || syncBudget.DelegatedTurns != asyncBudget.DelegatedTurns {
		t.Fatalf("budget summaries must stay comparable: sync=%+v async=%+v", syncBudget, asyncBudget)
	}
}
