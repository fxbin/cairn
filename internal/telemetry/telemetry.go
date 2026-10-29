// Package telemetry 把已成功持久化的领域事件投影为可选的语义观测信号。
//
// 观测面只消费已经 append 成功的事件：实时采集通过事件 sink 接入，离线重建
// 直接读取 Journal 事件序列，两者共用同一聚合器，因此相同事件序列总是得到
// 一致的语义统计。观测永远不能反向决定 Run 终态、权限或可信等级——Event
// Journal 仍是唯一运行事实源，采集丢失只意味着少一份观测数据。
//
// 隐私边界：观测只输出受限枚举、计数与派生标识，绝不携带任务正文、Prompt、
// 模型回复、工具结果、验证证据、工作区绝对路径或任何密钥。
//
// Author: fxbin
package telemetry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// 观测面允许出现在维度标签与跨度属性中的取值集合。任何其他取值都归一到
// unknown，保证指标维度基数有界；run 标识只作为关联属性，不进入维度。
const (
	// OutcomeRunning 表示运行尚未到达终态。
	OutcomeRunning = "running"
	// OutcomeCompleted 表示运行以成功终态结束。
	OutcomeCompleted = "completed"
	// OutcomeFailed 表示运行以失败终态结束。
	OutcomeFailed = "failed"
	// OutcomeCancelled 表示运行以取消终态结束。
	OutcomeCancelled = "cancelled"
	// OutcomeUnknown 表示无法归类的终态或非终态观测。
	OutcomeUnknown = "unknown"

	// ToolClassBuiltin 表示内置工具。
	ToolClassBuiltin = "builtin"
	// ToolClassMCP 表示 MCP 目录工具。
	ToolClassMCP = "mcp"
	// ToolClassUnknown 表示工具类别不可判定。
	ToolClassUnknown = "unknown"

	// UsageSourceEstimated 表示 token 计数来自本地估算兜底。
	UsageSourceEstimated = "estimated"
	// UsageSourceProvider 表示 token 计数来自 Provider 回执。
	UsageSourceProvider = "provider"

	// SideEffectNone 表示只读操作。
	SideEffectNone = "none"
	// SideEffectWorkspace 表示工作区写入操作。
	SideEffectWorkspace = "workspace"
	// SideEffectProcess 表示进程执行操作。
	SideEffectProcess = "process"
	// SideEffectUnknown 表示副作用类别不可判定。
	SideEffectUnknown = "unknown"
)

// 语义跨度的种类与名称；它们只描述结构，不携带业务内容。
const (
	// ScopeKindRun 是运行级跨度。
	ScopeKindRun = "run"
	// ScopeKindTurn 是轮次级跨度。
	ScopeKindTurn = "turn"
	// ScopeKindOperation 是工具操作级跨度。
	ScopeKindOperation = "operation"
	// ScopeKindVerification 是验证级跨度。
	ScopeKindVerification = "verification"

	// SpanNameRun 是运行级跨度名。
	SpanNameRun = "cairn.run"
	// SpanNameTurn 是轮次级跨度名。
	SpanNameTurn = "cairn.turn"
	// SpanNameOperation 是工具操作级跨度名。
	SpanNameOperation = "cairn.operation"
	// SpanNameVerification 是验证级跨度名。
	SpanNameVerification = "cairn.verification"
)

// 语义度量的稳定名称；导出端按这些名字聚合，不写入任何高基数字段。
const (
	// MetricRunDurationMS 是运行墙钟时长（毫秒）。
	MetricRunDurationMS = "cairn.run.duration_ms"
	// MetricRunTurns 是开启的模型轮次数量。
	MetricRunTurns = "cairn.run.turns"
	// MetricRunOperations 是实际开始执行的工具操作数量。
	MetricRunOperations = "cairn.run.operations"
	// MetricRunOperationFailures 是失败的工具操作数量。
	MetricRunOperationFailures = "cairn.run.operation_failures"
	// MetricRunVerificationPassed 是验证通过次数。
	MetricRunVerificationPassed = "cairn.run.verification.passed"
	// MetricRunVerificationFailed 是验证失败次数。
	MetricRunVerificationFailed = "cairn.run.verification.failed"
	// MetricRunCancellations 是取消请求次数。
	MetricRunCancellations = "cairn.run.cancellations"
	// MetricRunResumes 是恢复次数。
	MetricRunResumes = "cairn.run.resumes"
	// MetricRunReconciliations 是人工对账次数。
	MetricRunReconciliations = "cairn.run.reconciliations"
	// MetricRunDuplicateAttempts 是重复尝试次数。
	MetricRunDuplicateAttempts = "cairn.run.duplicate_attempts"
	// MetricRunRetriedRequests 是模型请求重试次数。
	MetricRunRetriedRequests = "cairn.run.retried_requests"
	// MetricRunInputTokens 是输入 token 消耗，按用量来源分维度。
	MetricRunInputTokens = "cairn.run.tokens.input"
	// MetricRunOutputTokens 是输出 token 消耗，按用量来源分维度。
	MetricRunOutputTokens = "cairn.run.tokens.output"
	// MetricBudgetTurnsUsed 是已消耗的轮次预算。
	MetricBudgetTurnsUsed = "cairn.run.budget.turns_used"
	// MetricBudgetToolCallsUsed 是已消耗的工具调用预算。
	MetricBudgetToolCallsUsed = "cairn.run.budget.tool_calls_used"
	// MetricBudgetInputTokensUsed 是已消耗的输入 token 预算。
	MetricBudgetInputTokensUsed = "cairn.run.budget.input_tokens_used"
	// MetricBudgetOutputTokensUsed 是已消耗的输出 token 预算。
	MetricBudgetOutputTokensUsed = "cairn.run.budget.output_tokens_used"
	// MetricBudgetWallTimeMS 是已消耗的墙钟预算（毫秒）。
	MetricBudgetWallTimeMS = "cairn.run.budget.wall_time_ms"
)

// Sink 接收已经成功追加到事件日志的领域事件。
type Sink interface {
	Observe(event domain.Event)
}

// Attribute 是观测属性或指标标签的受限键值对；取值只来自闭合枚举或派生计数。
type Attribute struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Limits 是冻结配置中的预算上限投影；观测面只用它换算预算消耗比例。
type Limits struct {
	MaxTurns        int   `json:"max_turns"`
	MaxToolCalls    int   `json:"max_tool_calls"`
	MaxInputTokens  int   `json:"max_input_tokens"`
	MaxOutputTokens int   `json:"max_output_tokens"`
	MaxWallTimeMS   int64 `json:"max_wall_time_ms"`
}

// RunMetrics 汇总一次运行的语义计数；全部字段只来自已持久化事件。
type RunMetrics struct {
	Outcome               string `json:"outcome"`
	Turns                 int    `json:"turns"`
	Operations            int    `json:"operations"`
	OperationFailures     int    `json:"operation_failures"`
	OperationCancelled    int    `json:"operation_cancelled"`
	VerifierPassed        int    `json:"verifier_passed"`
	VerifierFailed        int    `json:"verifier_failed"`
	ModelFailures         int    `json:"model_failures"`
	CancelRequests        int    `json:"cancel_requests"`
	Resumes               int    `json:"resumes"`
	Reconciliations       int    `json:"reconciliations"`
	DuplicateAttempts     int    `json:"duplicate_attempts"`
	RetriedRequests       int    `json:"retried_requests"`
	ContextCompactions    int    `json:"context_compactions"`
	EstimatedInputTokens  int64  `json:"estimated_input_tokens"`
	EstimatedOutputTokens int64  `json:"estimated_output_tokens"`
	ProviderInputTokens   int64  `json:"provider_input_tokens"`
	ProviderOutputTokens  int64  `json:"provider_output_tokens"`
	DurationMS            int64  `json:"duration_ms"`
}

// BudgetUsage 汇总一次运行的预算消耗与上限；上限来自冻结配置，消耗来自事件。
type BudgetUsage struct {
	MaxTurns         int   `json:"max_turns"`
	TurnsUsed        int   `json:"turns_used"`
	MaxToolCalls     int   `json:"max_tool_calls"`
	ToolCallsUsed    int   `json:"tool_calls_used"`
	MaxInputTokens   int   `json:"max_input_tokens"`
	InputTokensUsed  int64 `json:"input_tokens_used"`
	MaxOutputTokens  int   `json:"max_output_tokens"`
	OutputTokensUsed int64 `json:"output_tokens_used"`
	MaxWallTimeMS    int64 `json:"max_wall_time_ms"`
	WallTimeMS       int64 `json:"wall_time_ms"`
}

// Span 是一次语义观测跨度；属性只含受限枚举与计数，绝不携带正文或路径。
type Span struct {
	Name       string      `json:"name"`
	Kind       string      `json:"kind"`
	TraceID    string      `json:"trace_id"`
	SpanID     string      `json:"span_id"`
	ParentID   string      `json:"parent_span_id,omitempty"`
	Start      time.Time   `json:"start_time"`
	End        time.Time   `json:"end_time"`
	Attributes []Attribute `json:"attributes,omitempty"`
}

// Snapshot 是一次运行的语义观测快照：聚合指标、预算消耗与关联跨度。
type Snapshot struct {
	RunID   domain.RunID `json:"run_id"`
	TraceID string       `json:"trace_id"`
	Metrics RunMetrics   `json:"metrics"`
	Budget  BudgetUsage  `json:"budget"`
	Spans   []Span       `json:"spans"`
}

// Metric 是一条语义度量；维度标签只允许受限枚举键值。
type Metric struct {
	Name   string      `json:"name"`
	Value  int64       `json:"value"`
	Labels []Attribute `json:"labels,omitempty"`
}

// MetricPoints 导出快照的语义度量。维度只包含受限枚举，run 标识不进入维度，
// 只作为跨度关联属性，避免高基数维度。
func (snapshot Snapshot) MetricPoints() []Metric {
	outcome := snapshot.Metrics.Outcome
	if outcome == "" {
		outcome = OutcomeUnknown
	}
	points := []Metric{
		{Name: MetricRunDurationMS, Value: snapshot.Metrics.DurationMS, Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunTurns, Value: int64(snapshot.Metrics.Turns), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunOperations, Value: int64(snapshot.Metrics.Operations), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunOperationFailures, Value: int64(snapshot.Metrics.OperationFailures), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunVerificationPassed, Value: int64(snapshot.Metrics.VerifierPassed), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunVerificationFailed, Value: int64(snapshot.Metrics.VerifierFailed), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunCancellations, Value: int64(snapshot.Metrics.CancelRequests), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunResumes, Value: int64(snapshot.Metrics.Resumes), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunReconciliations, Value: int64(snapshot.Metrics.Reconciliations), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunDuplicateAttempts, Value: int64(snapshot.Metrics.DuplicateAttempts), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunRetriedRequests, Value: int64(snapshot.Metrics.RetriedRequests), Labels: []Attribute{{Key: "outcome", Value: outcome}}},
		{Name: MetricRunInputTokens, Value: snapshot.Metrics.EstimatedInputTokens, Labels: []Attribute{{Key: "usage_source", Value: UsageSourceEstimated}}},
		{Name: MetricRunInputTokens, Value: snapshot.Metrics.ProviderInputTokens, Labels: []Attribute{{Key: "usage_source", Value: UsageSourceProvider}}},
		{Name: MetricRunOutputTokens, Value: snapshot.Metrics.EstimatedOutputTokens, Labels: []Attribute{{Key: "usage_source", Value: UsageSourceEstimated}}},
		{Name: MetricRunOutputTokens, Value: snapshot.Metrics.ProviderOutputTokens, Labels: []Attribute{{Key: "usage_source", Value: UsageSourceProvider}}},
		{Name: MetricBudgetTurnsUsed, Value: int64(snapshot.Budget.TurnsUsed)},
		{Name: MetricBudgetToolCallsUsed, Value: int64(snapshot.Budget.ToolCallsUsed)},
		{Name: MetricBudgetInputTokensUsed, Value: snapshot.Budget.InputTokensUsed},
		{Name: MetricBudgetOutputTokensUsed, Value: snapshot.Budget.OutputTokensUsed},
		{Name: MetricBudgetWallTimeMS, Value: snapshot.Budget.WallTimeMS},
	}
	return points
}

// Aggregator 按事件顺序派生语义统计与关联跨度。同一事件序列总是得到同一结果，
// 因此实时逐条观察与离线批量重建可以互相对账。
type Aggregator struct {
	mu            sync.Mutex
	limits        Limits
	samplePercent int
	runs          map[domain.RunID]*runAggregate
}

// NewAggregator 创建聚合器；samplePercent 是按运行标识稳定抽样的百分比，
// 取值 1-100，100 表示不抽样。
func NewAggregator(limits Limits, samplePercent int) *Aggregator {
	if samplePercent <= 0 || samplePercent > 100 {
		samplePercent = 100
	}
	return &Aggregator{limits: limits, samplePercent: samplePercent, runs: make(map[domain.RunID]*runAggregate)}
}

// Observe 消费一个已成功持久化的事件；未抽样的运行被直接忽略。
func (aggregator *Aggregator) Observe(event domain.Event) {
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	run, known := aggregator.runs[event.RunID]
	if !known {
		sampled := aggregator.sample(event.RunID)
		run = &runAggregate{
			runID:         event.RunID,
			sampled:       sampled,
			limits:        aggregator.limits,
			turns:         make(map[domain.TurnID]*openScope),
			operations:    make(map[domain.OperationID]*openScope),
			verifications: make(map[string]*openScope),
			explorations:  make(map[string]int),
		}
		aggregator.runs[event.RunID] = run
	}
	if !run.sampled {
		return
	}
	run.observe(event)
}

// Snapshot 返回指定运行的语义快照；运行未被抽样或尚无事件时返回 false。
func (aggregator *Aggregator) Snapshot(runID domain.RunID) (Snapshot, bool) {
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	run, known := aggregator.runs[runID]
	if !known || !run.sampled {
		return Snapshot{}, false
	}
	return run.snapshot(), true
}

// RunIDs 返回已观察到的运行标识，按字典序排列，保证输出顺序稳定。
func (aggregator *Aggregator) RunIDs() []domain.RunID {
	aggregator.mu.Lock()
	defer aggregator.mu.Unlock()
	ids := make([]domain.RunID, 0, len(aggregator.runs))
	for id := range aggregator.runs {
		ids = append(ids, id)
	}
	sortRunIDs(ids)
	return ids
}

// sample 按运行标识的稳定哈希决定是否抽样；同一运行在实时采集与离线重建中
// 得到相同判定，抽样不会让两条路径产生分歧。
func (aggregator *Aggregator) sample(runID domain.RunID) bool {
	if aggregator.samplePercent >= 100 {
		return true
	}
	digest := fnv.New32a()
	_, _ = digest.Write([]byte(runID))
	return int(digest.Sum32()%100) < aggregator.samplePercent
}

// runAggregate 保存单个运行的聚合中间状态，只保存计数与受限标识。
type runAggregate struct {
	runID         domain.RunID
	sampled       bool
	limits        Limits
	metrics       RunMetrics
	createdAt     time.Time
	lastEventAt   time.Time
	terminalAt    time.Time
	terminal      bool
	runSpanID     string
	runSpanStart  time.Time
	turns         map[domain.TurnID]*openScope
	operations    map[domain.OperationID]*openScope
	verifications map[string]*openScope
	explorations  map[string]int
	spans         []Span
	spanSequence  int
}

// openScope 记录一个尚未闭合的观测区间。
type openScope struct {
	spanID     string
	start      time.Time
	parentID   string
	toolClass  string
	sideEffect string
}

// observe 把一个事件折叠进聚合状态；这里只读取受限的计数与枚举字段。
func (run *runAggregate) observe(event domain.Event) {
	if !event.OccurredAt.IsZero() {
		run.lastEventAt = event.OccurredAt
	}
	switch event.Type {
	case domain.EventRunCreated:
		run.createdAt = event.OccurredAt
		run.runSpanStart = event.OccurredAt
		run.runSpanID = run.newSpanID(ScopeKindRun, "run")
	case domain.EventTurnOpened:
		run.metrics.Turns++
		scope := &openScope{spanID: run.newSpanID(ScopeKindTurn, turnKeyOf(event.TurnID)), start: event.OccurredAt, parentID: run.runSpanID}
		if event.TurnID != nil {
			run.turns[*event.TurnID] = scope
		}
	case domain.EventTurnClosed:
		run.closeTurn(event)
	case domain.EventOperationStarted:
		run.metrics.Operations++
		toolName, sideEffect := operationPayloadOf(event.Payload)
		scope := &openScope{
			spanID:     run.newSpanID(ScopeKindOperation, operationKeyOf(event.OperationID)),
			start:      event.OccurredAt,
			parentID:   run.parentForTurn(event.TurnID),
			toolClass:  toolClass(toolName),
			sideEffect: sideEffectClass(sideEffect),
		}
		if event.OperationID != nil {
			run.operations[*event.OperationID] = scope
		}
	case domain.EventOperationSucceeded:
		run.closeOperation(event, "succeeded")
	case domain.EventOperationFailed:
		run.metrics.OperationFailures++
		run.closeOperation(event, "failed")
	case domain.EventOperationCancelled:
		run.metrics.OperationCancelled++
		run.closeOperation(event, "cancelled")
	case domain.EventOperationReconciled:
		run.metrics.Reconciliations++
		run.closeOperation(event, "reconciled")
	case domain.EventVerificationStarted:
		run.openVerification(event)
	case domain.EventVerificationFinished:
		if verificationOK(event.Payload) {
			run.metrics.VerifierPassed++
		} else {
			run.metrics.VerifierFailed++
		}
		run.closeVerification(event)
	case domain.EventModelResponseFinished:
		run.observeUsage(event.Payload)
	case domain.EventModelRequestStarted:
		if retryIndexOf(event.Payload) > 0 {
			run.metrics.RetriedRequests++
		}
	case domain.EventModelRequestFailed:
		run.metrics.ModelFailures++
	case domain.EventContextCompacted:
		run.metrics.ContextCompactions++
	case domain.EventRunCancelRequested:
		run.metrics.CancelRequests++
	case domain.EventRunResumed:
		run.metrics.Resumes++
	case domain.EventRunCompleted:
		run.terminate(OutcomeCompleted, event.OccurredAt)
	case domain.EventRunFailed:
		run.terminate(OutcomeFailed, event.OccurredAt)
	case domain.EventRunCancelled:
		run.terminate(OutcomeCancelled, event.OccurredAt)
	}
}

// closeTurn 关闭轮次跨度并统计重复尝试：相同操作签名与进展指纹的 continue
// 关轮第二次及以后各计一次重复尝试。
func (run *runAggregate) closeTurn(event domain.Event) {
	outcome := OutcomeUnknown
	signature, progress := turnClosedOf(event.Payload)
	var payload struct {
		Outcome string `json:"outcome"`
	}
	if json.Unmarshal(event.Payload, &payload) == nil {
		outcome = turnOutcome(payload.Outcome)
	}
	if outcome == "continue" {
		key := signature + "\x00" + progress
		run.explorations[key]++
		if run.explorations[key] > 1 {
			run.metrics.DuplicateAttempts++
		}
	}
	if event.TurnID == nil {
		return
	}
	scope, known := run.turns[*event.TurnID]
	if !known {
		return
	}
	delete(run.turns, *event.TurnID)
	run.appendSpan(Span{
		Name:       SpanNameTurn,
		Kind:       ScopeKindTurn,
		TraceID:    TraceID(run.runID),
		SpanID:     scope.spanID,
		ParentID:   scope.parentID,
		Start:      scope.start,
		End:        event.OccurredAt,
		Attributes: []Attribute{{Key: "outcome", Value: outcome}},
	})
}

// closeOperation 关闭工具操作跨度；操作在恢复或对账场景下可能没有可观测的
// 开始事件，此时只统计计数，不伪造零时长跨度。
func (run *runAggregate) closeOperation(event domain.Event, outcome string) {
	if event.OperationID == nil {
		return
	}
	scope, known := run.operations[*event.OperationID]
	if !known {
		return
	}
	delete(run.operations, *event.OperationID)
	run.appendSpan(Span{
		Name:     SpanNameOperation,
		Kind:     ScopeKindOperation,
		TraceID:  TraceID(run.runID),
		SpanID:   scope.spanID,
		ParentID: scope.parentID,
		Start:    scope.start,
		End:      event.OccurredAt,
		Attributes: []Attribute{
			{Key: "outcome", Value: outcome},
			{Key: "tool_class", Value: scope.toolClass},
			{Key: "side_effect", Value: scope.sideEffect},
		},
	})
}

// openVerification 开启验证跨度；验证标识缺失时按序号定位，保证同一运行的
// 多次验证不会互相覆盖。
func (run *runAggregate) openVerification(event domain.Event) {
	key := verificationIDOf(event.Payload)
	if key == "" {
		key = "seq:" + strconv.FormatUint(event.Seq, 10)
	}
	scope := &openScope{spanID: run.newSpanID(ScopeKindVerification, key), start: event.OccurredAt, parentID: run.parentForTurn(event.TurnID)}
	run.verifications[key] = scope
}

// closeVerification 关闭验证跨度并记录通过或失败。
func (run *runAggregate) closeVerification(event domain.Event) {
	key := verificationIDOf(event.Payload)
	if key == "" {
		key = "seq:" + strconv.FormatUint(event.Seq, 10)
	}
	scope, known := run.verifications[key]
	if !known {
		return
	}
	delete(run.verifications, key)
	outcome := "failed"
	if verificationOK(event.Payload) {
		outcome = "passed"
	}
	run.appendSpan(Span{
		Name:       SpanNameVerification,
		Kind:       ScopeKindVerification,
		TraceID:    TraceID(run.runID),
		SpanID:     scope.spanID,
		ParentID:   scope.parentID,
		Start:      scope.start,
		End:        event.OccurredAt,
		Attributes: []Attribute{{Key: "outcome", Value: outcome}},
	})
}

// observeUsage 把 token 计数按来源分桶：估算兜底与 Provider 回执严格区分，
// 观测面不得把两者混算。
func (run *runAggregate) observeUsage(payload json.RawMessage) {
	var decoded struct {
		Usage *struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
		UsageEstimated bool `json:"usage_estimated"`
	}
	if json.Unmarshal(payload, &decoded) != nil || decoded.Usage == nil {
		return
	}
	if decoded.UsageEstimated {
		run.metrics.EstimatedInputTokens += decoded.Usage.PromptTokens
		run.metrics.EstimatedOutputTokens += decoded.Usage.CompletionTokens
		return
	}
	run.metrics.ProviderInputTokens += decoded.Usage.PromptTokens
	run.metrics.ProviderOutputTokens += decoded.Usage.CompletionTokens
}

// terminate 记录运行终态并闭合仍然打开的观测区间。
func (run *runAggregate) terminate(outcome string, at time.Time) {
	if run.terminal {
		return
	}
	run.terminal = true
	run.terminalAt = at
	run.metrics.Outcome = outcome
}

// parentForTurn 返回轮次跨度标识；轮次不可用时回退到运行跨度。
func (run *runAggregate) parentForTurn(turnID *domain.TurnID) string {
	if turnID == nil {
		return run.runSpanID
	}
	if scope, known := run.turns[*turnID]; known {
		return scope.spanID
	}
	return run.runSpanID
}

// newSpanID 生成运行内唯一的跨度标识；它由运行标识、种类、键与序号派生，
// 因此实时采集与离线重建得到同一组标识。
func (run *runAggregate) newSpanID(kind, key string) string {
	run.spanSequence++
	digest := sha256.Sum256([]byte("cairn-telemetry-span-v1\x00" + string(run.runID) + "\x00" + kind + "\x00" + key + "\x00" + strconv.Itoa(run.spanSequence)))
	return hex.EncodeToString(digest[:8])
}

// appendSpan 追加一个已闭合跨度。
func (run *runAggregate) appendSpan(span Span) {
	run.spans = append(run.spans, span)
}

// snapshot 汇总当前聚合状态；仍然打开的区间按最后事件时刻闭合，使实时快照
// 与离线重建在相同事件序列下产生一致结果。
func (run *runAggregate) snapshot() Snapshot {
	metrics := run.metrics
	if metrics.Outcome == "" {
		metrics.Outcome = OutcomeRunning
	}
	if !run.createdAt.IsZero() && !run.terminalAt.IsZero() {
		metrics.DurationMS = run.terminalAt.Sub(run.createdAt).Milliseconds()
	}
	if metrics.DurationMS < 0 {
		metrics.DurationMS = 0
	}
	spans := make([]Span, 0, len(run.spans)+1)
	spans = append(spans, run.spans...)
	if run.runSpanID != "" {
		end := run.terminalAt
		if end.IsZero() {
			end = run.lastEventAt
		}
		spans = append(spans, Span{
			Name:       SpanNameRun,
			Kind:       ScopeKindRun,
			TraceID:    TraceID(run.runID),
			SpanID:     run.runSpanID,
			Start:      run.runSpanStart,
			End:        end,
			Attributes: []Attribute{{Key: "outcome", Value: metrics.Outcome}},
		})
	}
	inputTokens := metrics.EstimatedInputTokens + metrics.ProviderInputTokens
	outputTokens := metrics.EstimatedOutputTokens + metrics.ProviderOutputTokens
	budget := BudgetUsage{
		MaxTurns:         run.limits.MaxTurns,
		TurnsUsed:        metrics.Turns,
		MaxToolCalls:     run.limits.MaxToolCalls,
		ToolCallsUsed:    metrics.Operations,
		MaxInputTokens:   run.limits.MaxInputTokens,
		InputTokensUsed:  inputTokens,
		MaxOutputTokens:  run.limits.MaxOutputTokens,
		OutputTokensUsed: outputTokens,
		MaxWallTimeMS:    run.limits.MaxWallTimeMS,
		WallTimeMS:       metrics.DurationMS,
	}
	return Snapshot{
		RunID:   run.runID,
		TraceID: TraceID(run.runID),
		Metrics: metrics,
		Budget:  budget,
		Spans:   spans,
	}
}

// TraceID 由运行标识派生稳定的追踪标识；它只用于关联，不进入任何指标维度。
func TraceID(runID domain.RunID) string {
	digest := sha256.Sum256([]byte("cairn-telemetry-trace-v1\x00" + string(runID)))
	return hex.EncodeToString(digest[:16])
}

// operationPayloadOf 只读取工具名与副作用类别两个受限字段。
func operationPayloadOf(payload json.RawMessage) (string, string) {
	var decoded struct {
		ToolName        string `json:"tool_name"`
		SideEffectClass string `json:"side_effect_class"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return "", ""
	}
	return decoded.ToolName, decoded.SideEffectClass
}

// turnClosedOf 只读取关轮的操作签名与进展指纹两个派生标识。
func turnClosedOf(payload json.RawMessage) (string, string) {
	var decoded struct {
		OperationSignature  string `json:"operation_signature"`
		ProgressFingerprint string `json:"progress_fingerprint"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return "", ""
	}
	return decoded.OperationSignature, decoded.ProgressFingerprint
}

// verificationIDOf 只读取验证标识。
func verificationIDOf(payload json.RawMessage) string {
	var decoded struct {
		VerificationID string `json:"verification_id"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return ""
	}
	return decoded.VerificationID
}

// verificationOK 只读取验证结论布尔值。
func verificationOK(payload json.RawMessage) bool {
	var decoded struct {
		OK bool `json:"ok"`
	}
	if json.Unmarshal(payload, &decoded) != nil {
		return false
	}
	return decoded.OK
}

// retryIndexOf 只读取模型请求重试序号。
func retryIndexOf(payload json.RawMessage) int {
	var decoded struct {
		RetryIndex *int `json:"retry_index"`
	}
	if json.Unmarshal(payload, &decoded) != nil || decoded.RetryIndex == nil {
		return 0
	}
	return *decoded.RetryIndex
}

// toolClass 把工具名归一到有界类别；MCP 目录工具名由外部服务器提供，
// 基数不受控，因此只导出类别而不导出原名。
func toolClass(name string) string {
	if name == "" {
		return ToolClassUnknown
	}
	if strings.HasPrefix(name, "mcp__") || name == "mcp_close" {
		return ToolClassMCP
	}
	return ToolClassBuiltin
}

// turnKeyOf 返回轮次标识的稳定键；标识缺失时使用空键，聚合器据此退化为
// 不登记轮次区间而不是伪造标识。
func turnKeyOf(turnID *domain.TurnID) string {
	if turnID == nil {
		return ""
	}
	return string(*turnID)
}

// operationKeyOf 返回操作标识的稳定键；标识缺失时返回空键。
func operationKeyOf(operationID *domain.OperationID) string {
	if operationID == nil {
		return ""
	}
	return string(*operationID)
}

// sideEffectClass 把副作用类别归一到有界取值。
func sideEffectClass(value string) string {
	switch value {
	case SideEffectNone, SideEffectWorkspace, SideEffectProcess:
		return value
	default:
		return SideEffectUnknown
	}
}

// turnOutcome 把关轮结论归一到有界取值。
func turnOutcome(value string) string {
	switch value {
	case "final", "continue", "failed", "cancelled":
		return value
	default:
		return OutcomeUnknown
	}
}

// sortRunIDs 按字典序排列运行标识，保证导出顺序稳定。
func sortRunIDs(ids []domain.RunID) {
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
}
