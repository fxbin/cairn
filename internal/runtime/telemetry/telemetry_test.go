package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// errExportFailed 是导出失败的固定哨兵错误。
var errExportFailed = errors.New("export failed")

// sensitiveMarkers 是必须出现在事件负载里、但绝不允许出现在任何观测产物里的
// 敏感标记：任务正文、Prompt、原始工具结果、验证证据、伪造的本机路径与
// 密钥样式串。它们全部是合成哨兵，不代表任何真实本机信息。
var sensitiveMarkers = []string{
	"task-body-marker",
	"prompt-body-marker",
	"raw-tool-result-marker",
	"verifier-body-marker",
	"/nonexistent-workspace/secret.txt",
	"sk-marker-value",
}

// telemetryFixture 构造一段覆盖轮次、操作、验证、用量、取消、恢复与对账的
// 运行事件序列；负载里刻意塞入敏感正文，用于证明观测面不会导出它们。
func telemetryFixture(t *testing.T) []domain.Event {
	t.Helper()
	runID := domain.RunID("run-telemetry")
	firstTurn := domain.TurnID("turn-1")
	secondTurn := domain.TurnID("turn-2")
	failedOperation := domain.OperationID("op-failed")
	succeededOperation := domain.OperationID("op-succeeded")
	cancelledOperation := domain.OperationID("op-cancelled")
	base := time.Unix(1_700_000_000, 0).UTC()
	event := func(seq uint64, kind domain.EventType, turn *domain.TurnID, operation *domain.OperationID, payload string) domain.Event {
		return domain.Event{
			Seq:         seq,
			RunID:       runID,
			Type:        kind,
			TurnID:      turn,
			OperationID: operation,
			OccurredAt:  base.Add(time.Duration(seq) * time.Second),
			Payload:     json.RawMessage(payload),
		}
	}
	return []domain.Event{
		event(1, domain.EventRunCreated, nil, nil, `{"task":"task-body-marker","workspace":"/nonexistent-workspace/secret.txt"}`),
		event(2, domain.EventTurnOpened, &firstTurn, nil, `{"turn":1}`),
		event(3, domain.EventToolCallProposed, &firstTurn, &failedOperation, `{"call_id":"c1","tool_name":"mcp__docs__search","arguments":{"query":"prompt-body-marker"}}`),
		event(4, domain.EventOperationStarted, &firstTurn, &failedOperation, `{"tool_name":"mcp__docs__search","side_effect_class":"none"}`),
		event(5, domain.EventOperationFailed, &firstTurn, &failedOperation, `{"tool_name":"mcp__docs__search","result":{"preview":"raw-tool-result-marker"}}`),
		event(6, domain.EventModelRequestStarted, &firstTurn, nil, `{"provider":"test","model":"test-model","retry_index":1}`),
		event(7, domain.EventModelResponseFinished, &firstTurn, nil, `{"text":"prompt-body-marker","usage":{"prompt_tokens":120,"completion_tokens":30},"usage_estimated":true}`),
		event(8, domain.EventVerificationStarted, &firstTurn, nil, `{"verification_id":"v1"}`),
		event(9, domain.EventVerificationFinished, &firstTurn, nil, `{"verification_id":"v1","ok":false,"evidence_ref":"verifier-body-marker"}`),
		event(10, domain.EventTurnClosed, &firstTurn, nil, `{"outcome":"continue","operation_signature":"sig-a","progress_fingerprint":"fp-a"}`),
		event(11, domain.EventTurnOpened, &secondTurn, nil, `{"turn":2}`),
		event(12, domain.EventOperationStarted, &secondTurn, &succeededOperation, `{"tool_name":"read_file","side_effect_class":"none"}`),
		event(13, domain.EventOperationSucceeded, &secondTurn, &succeededOperation, `{"tool_name":"read_file","result":{"preview":"sk-marker-value"}}`),
		event(14, domain.EventModelResponseFinished, &secondTurn, nil, `{"text":"done","usage":{"prompt_tokens":40,"completion_tokens":10}}`),
		event(15, domain.EventVerificationStarted, &secondTurn, nil, `{"verification_id":"v2"}`),
		event(16, domain.EventVerificationFinished, &secondTurn, nil, `{"verification_id":"v2","ok":true}`),
		event(17, domain.EventTurnClosed, &secondTurn, nil, `{"outcome":"continue","operation_signature":"sig-a","progress_fingerprint":"fp-a"}`),
		event(18, domain.EventOperationCancelled, &secondTurn, &cancelledOperation, `{"tool_name":"exec_command","cause":"user_interrupt"}`),
		event(19, domain.EventModelRequestFailed, &secondTurn, nil, `{"error_class":"provider_timeout","retryable":true}`),
		event(20, domain.EventContextCompacted, &secondTurn, nil, `{"through_seq":9,"summary":"checkpoint","strategy":"summary"}`),
		event(21, domain.EventRunCancelRequested, &secondTurn, nil, `{"cause":"user_interrupt"}`),
		event(22, domain.EventRunResumed, nil, nil, `{"invocation":"invoke-1","from_seq":17,"workspace_observation":"sha256:obs"}`),
		event(23, domain.EventOperationReconciled, &secondTurn, &failedOperation, `{"verdict":"applied_success","actor":"human","note":"raw-tool-result-marker"}`),
		event(24, domain.EventRunCancelled, nil, nil, `{"cause":"user_interrupt"}`),
	}
}

// fixtureLimits 是测试用的冻结预算上限投影。
func fixtureLimits() Limits {
	return Limits{MaxTurns: 40, MaxToolCalls: 100, MaxInputTokens: 500_000, MaxOutputTokens: 100_000, MaxWallTimeMS: 1_800_000}
}

// TestAggregateMatchesLiveObservation 锁定实时逐条观察与离线批量重建的一致性：
// 同一事件序列必须得到逐字段相同的语义快照与度量，实时采集丢失不影响
// 从 Journal 重建的权威统计。
func TestAggregateMatchesLiveObservation(t *testing.T) {
	events := telemetryFixture(t)
	live := NewAggregator(fixtureLimits(), 100)
	for _, event := range events {
		live.Observe(event)
	}
	liveSnapshot, ok := live.Snapshot(events[0].RunID)
	if !ok {
		t.Fatal("live observation must produce a snapshot")
	}
	offlineSnapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	if !reflect.DeepEqual(liveSnapshot, offlineSnapshot) {
		t.Fatalf("live snapshot drifted from offline rebuild:\nlive=%+v\noffline=%+v", liveSnapshot, offlineSnapshot)
	}
	if !reflect.DeepEqual(liveSnapshot.MetricPoints(), offlineSnapshot.MetricPoints()) {
		t.Fatal("metric points drifted between live and offline paths")
	}
	if liveSnapshot.Metrics.Turns != 2 || liveSnapshot.Metrics.Operations != 2 {
		t.Fatalf("metrics = %+v", liveSnapshot.Metrics)
	}
	if liveSnapshot.Metrics.OperationFailures != 1 || liveSnapshot.Metrics.VerifierPassed != 1 || liveSnapshot.Metrics.VerifierFailed != 1 {
		t.Fatalf("counters = %+v", liveSnapshot.Metrics)
	}
	if liveSnapshot.Metrics.CancelRequests != 1 || liveSnapshot.Metrics.Resumes != 1 || liveSnapshot.Metrics.Reconciliations != 1 {
		t.Fatalf("control plane counters = %+v", liveSnapshot.Metrics)
	}
	if liveSnapshot.Metrics.DuplicateAttempts != 1 || liveSnapshot.Metrics.RetriedRequests != 1 {
		t.Fatalf("duplicate attempt counters = %+v", liveSnapshot.Metrics)
	}
	if liveSnapshot.Metrics.Outcome != OutcomeCancelled {
		t.Fatalf("outcome = %q", liveSnapshot.Metrics.Outcome)
	}
	if liveSnapshot.Metrics.DurationMS != 23_000 {
		t.Fatalf("duration = %d", liveSnapshot.Metrics.DurationMS)
	}
}

// TestUsageSeparatesEstimateFromProviderReceipt 锁定用量来源区分：估算兜底与
// Provider 回执必须分桶统计，绝不混算成同一个数字。
func TestUsageSeparatesEstimateFromProviderReceipt(t *testing.T) {
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	if snapshot.Metrics.EstimatedInputTokens != 120 || snapshot.Metrics.EstimatedOutputTokens != 30 {
		t.Fatalf("estimated usage = %+v", snapshot.Metrics)
	}
	if snapshot.Metrics.ProviderInputTokens != 40 || snapshot.Metrics.ProviderOutputTokens != 10 {
		t.Fatalf("provider usage = %+v", snapshot.Metrics)
	}
	if snapshot.Budget.InputTokensUsed != 160 || snapshot.Budget.OutputTokensUsed != 40 {
		t.Fatalf("budget usage = %+v", snapshot.Budget)
	}
	estimated := int64(0)
	provider := int64(0)
	for _, point := range snapshot.MetricPoints() {
		if point.Name != MetricRunInputTokens {
			continue
		}
		for _, label := range point.Labels {
			switch label.Value {
			case UsageSourceEstimated:
				estimated = point.Value
			case UsageSourceProvider:
				provider = point.Value
			}
		}
	}
	if estimated != 120 || provider != 40 {
		t.Fatalf("token metrics = estimated %d provider %d", estimated, provider)
	}
}

// TestSnapshotNeverCarriesSensitivePayloads 锁定隐私边界：含敏感正文的负载
// 不得出现在快照、度量标签或跨度属性中。
func TestSnapshotNeverCarriesSensitivePayloads(t *testing.T) {
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	body := string(encoded)
	for _, marker := range sensitiveMarkers {
		if strings.Contains(body, marker) {
			t.Fatalf("snapshot leaked sensitive content %q", marker)
		}
	}
	if strings.Contains(body, "mcp__docs__search") {
		t.Fatal("unbounded tool name must not reach the observation payload")
	}
	for _, span := range snapshot.Spans {
		for _, attribute := range span.Attributes {
			for _, marker := range sensitiveMarkers {
				if strings.Contains(attribute.Value, marker) {
					t.Fatalf("span attribute leaked %q", marker)
				}
			}
		}
	}
}

// TestMetricLabelsStayBounded 锁定基数边界：指标维度只允许受限枚举键值，
// 运行标识绝不进入维度。
func TestMetricLabelsStayBounded(t *testing.T) {
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	allowedKeys := map[string]bool{"outcome": true, "usage_source": true}
	allowedValues := map[string]bool{
		OutcomeRunning: true, OutcomeCompleted: true, OutcomeFailed: true,
		OutcomeCancelled: true, OutcomeUnknown: true,
		UsageSourceEstimated: true, UsageSourceProvider: true,
	}
	for _, point := range snapshot.MetricPoints() {
		for _, label := range point.Labels {
			if !allowedKeys[label.Key] {
				t.Fatalf("metric %q uses unbounded label key %q", point.Name, label.Key)
			}
			if !allowedValues[label.Value] {
				t.Fatalf("metric %q uses unbounded label value %q", point.Name, label.Value)
			}
			if label.Value == string(events[0].RunID) {
				t.Fatalf("metric %q must not carry the run id as a label", point.Name)
			}
		}
	}
}

// TestMetricPointsIncludeBudgetCeilings 锁定预算上限出口：采集端只拿到消耗
// 就算不出利用率，因此预算上限必须与消耗一起导出。
func TestMetricPointsIncludeBudgetCeilings(t *testing.T) {
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	values := map[string]int64{}
	for _, point := range snapshot.MetricPoints() {
		values[point.Name] = point.Value
	}
	expected := map[string]int64{
		MetricBudgetMaxTurns:        40,
		MetricBudgetMaxToolCalls:    100,
		MetricBudgetMaxInputTokens:  500_000,
		MetricBudgetMaxOutputTokens: 100_000,
		MetricBudgetMaxWallTimeMS:   1_800_000,
	}
	for name, want := range expected {
		got, exists := values[name]
		if !exists {
			t.Fatalf("metric %q is missing from the exported points", name)
		}
		if got != want {
			t.Fatalf("metric %q = %d, want %d", name, got, want)
		}
	}
}

// TestMetricPointsCoverAggregatedCounters 锁定度量出口完整性：issue 要求
// 观察到的每个聚合计数都必须有对应的度量，否则采集端看不到。
func TestMetricPointsCoverAggregatedCounters(t *testing.T) {
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	values := map[string]int64{}
	for _, point := range snapshot.MetricPoints() {
		values[point.Name] += point.Value
	}
	expected := map[string]int64{
		MetricRunDurationMS:             23_000,
		MetricRunTurns:                  2,
		MetricRunOperations:             2,
		MetricRunOperationFailures:      1,
		MetricRunOperationCancellations: 1,
		MetricRunModelFailures:          1,
		MetricRunContextCompactions:     1,
		MetricRunVerificationPassed:     1,
		MetricRunVerificationFailed:     1,
		MetricRunCancellations:          1,
		MetricRunResumes:                1,
		MetricRunReconciliations:        1,
		MetricRunDuplicateAttempts:      1,
		MetricRunRetriedRequests:        1,
		MetricRunInputTokens:            160,
		MetricRunOutputTokens:           40,
		MetricBudgetTurnsUsed:           2,
		MetricBudgetToolCallsUsed:       2,
		MetricBudgetInputTokensUsed:     160,
		MetricBudgetOutputTokensUsed:    40,
		MetricBudgetWallTimeMS:          23_000,
	}
	for name, want := range expected {
		got, exists := values[name]
		if !exists {
			t.Fatalf("metric %q is missing from the exported points", name)
		}
		if got != want {
			t.Fatalf("metric %q = %d, want %d", name, got, want)
		}
	}
}

// TestSpansCorrelateRunTurnOperationAndVerification 锁定 Trace 关联结构：运行、
// 轮次、操作与验证共享同一追踪标识，父子关系明确，跨度标识互不相同。
func TestSpansCorrelateRunTurnOperationAndVerification(t *testing.T) {
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	kinds := map[string]int{}
	spanIDs := map[string]bool{}
	runSpanID := ""
	for _, span := range snapshot.Spans {
		kinds[span.Kind]++
		if spanIDs[span.SpanID] {
			t.Fatalf("duplicate span id %q", span.SpanID)
		}
		spanIDs[span.SpanID] = true
		if span.TraceID != snapshot.TraceID {
			t.Fatalf("span %q drifted from trace id", span.Name)
		}
		if span.Kind == ScopeKindRun {
			runSpanID = span.SpanID
		}
	}
	if kinds[ScopeKindRun] != 1 || kinds[ScopeKindTurn] != 2 || kinds[ScopeKindOperation] != 2 || kinds[ScopeKindVerification] != 2 {
		t.Fatalf("span kinds = %+v", kinds)
	}
	if runSpanID == "" {
		t.Fatal("run span missing")
	}
	for _, span := range snapshot.Spans {
		if span.Kind == ScopeKindRun {
			continue
		}
		if span.ParentID == "" {
			t.Fatalf("span %q has no parent", span.Name)
		}
		if !spanIDs[span.ParentID] {
			t.Fatalf("span %q references an unknown parent", span.Name)
		}
	}
}

// TestSamplingDecisionIsSharedByLiveAndOfflinePaths 锁定抽样策略：抽样判定
// 由运行标识派生，实时与离线路径必须一致，且低比例抽样不会选中全部运行。
func TestSamplingDecisionIsSharedByLiveAndOfflinePaths(t *testing.T) {
	sampled := 0
	for index := 0; index < 64; index++ {
		runID := domain.RunID("run-sample-" + strconv.Itoa(index))
		events := []domain.Event{{
			Seq:        1,
			RunID:      runID,
			Type:       domain.EventRunCompleted,
			OccurredAt: time.Unix(1_700_000_000, 0).UTC(),
			Payload:    json.RawMessage(`{}`),
		}}
		live := NewAggregator(fixtureLimits(), 1)
		live.Observe(events[0])
		_, liveSampled := live.Snapshot(runID)
		_, offlineSampled := Aggregate(events, fixtureLimits(), 1)
		if liveSampled != offlineSampled {
			t.Fatalf("sampling diverged for %s", runID)
		}
		if liveSampled {
			sampled++
		}
	}
	if sampled == 64 {
		t.Fatal("one percent sampling must not select every run")
	}
}

// TestRuntimeExportFailureLeavesAggregationIntact 锁定失败开放合同：导出失败
// 只体现在导出器统计里，聚合结果与运行事实不受影响。
func TestRuntimeExportFailureLeavesAggregationIntact(t *testing.T) {
	events := telemetryFixture(t)
	exporter := &failingExporter{}
	runtime := NewRuntime(exporter, fixtureLimits(), 100)
	for _, event := range events {
		runtime.Observe(event)
	}
	snapshot, ok := runtime.Snapshot(events[0].RunID)
	if !ok {
		t.Fatal("runtime must expose the aggregated snapshot")
	}
	if snapshot.Metrics.Turns != 2 || snapshot.Metrics.Operations != 2 || snapshot.Metrics.Outcome != OutcomeCancelled {
		t.Fatalf("metrics after failed export = %+v", snapshot.Metrics)
	}
	if exporter.calls != 1 {
		t.Fatalf("exporter calls = %d, want exactly one export at terminal state", exporter.calls)
	}
}

// TestRuntimeDoesNotExportBeforeTerminalState 锁定导出时机：只有运行到达终态
// 才导出一次，运行过程中的中间快照不外发。
func TestRuntimeDoesNotExportBeforeTerminalState(t *testing.T) {
	events := telemetryFixture(t)
	exporter := &failingExporter{}
	runtime := NewRuntime(exporter, fixtureLimits(), 100)
	for _, event := range events[:len(events)-1] {
		runtime.Observe(event)
	}
	if exporter.calls != 0 {
		t.Fatalf("exporter calls before terminal state = %d", exporter.calls)
	}
	runtime.Observe(events[len(events)-1])
	if exporter.calls != 1 {
		t.Fatalf("exporter calls at terminal state = %d", exporter.calls)
	}
}

// failingExporter 是恒定失败的导出器替身，用于证明观测失败不改变聚合结果。
type failingExporter struct {
	calls int
}

// Export 记录调用次数并返回固定错误。
func (exporter *failingExporter) Export(_ context.Context, _ Snapshot) error {
	exporter.calls++
	return errExportFailed
}
