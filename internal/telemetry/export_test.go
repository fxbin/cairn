package telemetry

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// otlpReceiver 是一个本地 OTLP 接收器替身：它按路径记录收到的请求体，
// 用于验证导出面确实把语义观测送达本地接收器。
type otlpReceiver struct {
	server   *httptest.Server
	mutex    sync.Mutex
	received map[string][]byte
}

// newOTLPReceiver 启动本地接收器；测试结束时自动关闭。
func newOTLPReceiver(t *testing.T) *otlpReceiver {
	t.Helper()
	receiver := &otlpReceiver{received: make(map[string][]byte)}
	receiver.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}
		receiver.mutex.Lock()
		receiver.received[request.URL.Path] = body
		receiver.mutex.Unlock()
		writer.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(receiver.server.Close)
	return receiver
}

// body 返回指定路径收到的请求体副本。
func (receiver *otlpReceiver) body(path string) []byte {
	receiver.mutex.Lock()
	defer receiver.mutex.Unlock()
	return append([]byte(nil), receiver.received[path]...)
}

// receiverPlan 构造指向本地接收器的接线判定：端点换成本地接收器，其余判定
// 沿用真实解析结果。
func receiverPlan(t *testing.T, receiver *otlpReceiver) Plan {
	t.Helper()
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusInstrumented {
		t.Fatalf("plan status = %q, want instrumented", plan.Status)
	}
	plan.Config.TracesEndpoint = receiver.server.URL + "/v1/traces"
	plan.Config.MetricsEndpoint = receiver.server.URL + "/v1/metrics"
	return plan
}

// TestOTLPExporterDeliversTraceToLocalReceiver 锁定真实导出：开启后本地接收器
// 必须收到一条结构合法的运行 Trace 与一组语义度量，追踪标识使用十六进制。
func TestOTLPExporterDeliversTraceToLocalReceiver(t *testing.T) {
	receiver := newOTLPReceiver(t)
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	exporter, err := NewOTLPExporter(receiverPlan(t, receiver))
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Export(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	traceBody := receiver.body("/v1/traces")
	if len(traceBody) == 0 {
		t.Fatal("local receiver did not receive a trace request")
	}
	var decoded struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID           string `json:"traceId"`
					SpanID            string `json:"spanId"`
					ParentSpanID      string `json:"parentSpanId"`
					Name              string `json:"name"`
					StartTimeUnixNano string `json:"startTimeUnixNano"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(traceBody, &decoded); err != nil {
		t.Fatalf("decode trace request: %v", err)
	}
	if len(decoded.ResourceSpans) != 1 || len(decoded.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("trace request shape = %+v", decoded)
	}
	spans := decoded.ResourceSpans[0].ScopeSpans[0].Spans
	if len(spans) == 0 {
		t.Fatal("trace request carries no span")
	}
	found := false
	for _, span := range spans {
		if span.Name != SpanNameRun {
			continue
		}
		found = true
		if len(span.TraceID) != 32 || strings.Trim(span.TraceID, "0123456789abcdef") != "" {
			t.Fatalf("trace id must be 32 hex characters, got %q", span.TraceID)
		}
		if len(span.SpanID) != 16 || strings.Trim(span.SpanID, "0123456789abcdef") != "" {
			t.Fatalf("span id must be 16 hex characters, got %q", span.SpanID)
		}
		if span.ParentSpanID != "" {
			t.Fatalf("root span must not carry a parent, got %q", span.ParentSpanID)
		}
		if span.StartTimeUnixNano == "" {
			t.Fatal("span must carry a start timestamp")
		}
	}
	if !found {
		t.Fatal("trace request carries no run span")
	}
	metricsBody := receiver.body("/v1/metrics")
	if len(metricsBody) == 0 {
		t.Fatal("local receiver did not receive a metrics request")
	}
	var metricsDecoded struct {
		ResourceMetrics []struct {
			ScopeMetrics []struct {
				Metrics []struct {
					Name string `json:"name"`
				} `json:"metrics"`
			} `json:"scopeMetrics"`
		} `json:"resourceMetrics"`
	}
	if err := json.Unmarshal(metricsBody, &metricsDecoded); err != nil {
		t.Fatalf("decode metrics request: %v", err)
	}
	if len(metricsDecoded.ResourceMetrics) != 1 || len(metricsDecoded.ResourceMetrics[0].ScopeMetrics) != 1 {
		t.Fatalf("metrics request shape = %+v", metricsDecoded)
	}
	names := map[string]bool{}
	for _, metric := range metricsDecoded.ResourceMetrics[0].ScopeMetrics[0].Metrics {
		names[metric.Name] = true
	}
	for _, expected := range []string{MetricRunTurns, MetricRunDurationMS, MetricRunInputTokens, MetricBudgetTurnsUsed, MetricBudgetMaxTurns} {
		if !names[expected] {
			t.Fatalf("metrics request misses %q, got %+v", expected, names)
		}
	}
}

// TestOTLPExportNeverCarriesSensitivePayloads 锁定线上隐私边界：含敏感正文的
// 事件序列导出后，接收端收到的字节里不得出现任何敏感标记。
func TestOTLPExportNeverCarriesSensitivePayloads(t *testing.T) {
	receiver := newOTLPReceiver(t)
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	exporter, err := NewOTLPExporter(receiverPlan(t, receiver))
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Export(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/traces", "/v1/metrics"} {
		body := string(receiver.body(path))
		if len(body) == 0 {
			t.Fatalf("receiver did not receive %s", path)
		}
		for _, marker := range sensitiveMarkers {
			if strings.Contains(body, marker) {
				t.Fatalf("%s leaked sensitive content %q", path, marker)
			}
		}
	}
}

// TestResolvePlanHonorsGlobalSDKDisable 锁定全局关闭合同：OTEL_SDK_DISABLED
// 为真时语义面不得导出任何数据，判定状态如实报告为被全局关闭。
func TestResolvePlanHonorsGlobalSDKDisable(t *testing.T) {
	t.Setenv(EnvSDKDisabled, "true")
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusSDKDisabled {
		t.Fatalf("plan status = %q, want %q", plan.Status, ExportStatusSDKDisabled)
	}
	if plan.Reason != ReasonSDKDisabled {
		t.Fatalf("plan reason = %q", plan.Reason)
	}
	if len(plan.Signals) != 0 {
		t.Fatalf("a globally disabled plane must send no signal, got %+v", plan.Signals)
	}
	if _, err := NewOTLPExporter(plan); err == nil {
		t.Fatal("a globally disabled plane must not be exportable")
	}
}

// TestResolvePlanKeepsDisabledWhenNotEnabled 锁定配置门：未显式启用时判定为
// disabled，不解析端点也不产生任何信号。
func TestResolvePlanKeepsDisabledWhenNotEnabled(t *testing.T) {
	t.Setenv(EnvSDKDisabled, "true")
	plan := ResolvePlan(false)
	if plan.Status != ExportStatusDisabled {
		t.Fatalf("plan status = %q, want %q", plan.Status, ExportStatusDisabled)
	}
	if plan.Reason != ReasonNotEnabled {
		t.Fatalf("plan reason = %q", plan.Reason)
	}
}

// TestResolvePlanHonorsExporterNone 锁定 exporter 选择语义：none 表示对应
// 信号不导出；两个信号都为 none 时没有任何数据外发。
func TestResolvePlanHonorsExporterNone(t *testing.T) {
	t.Setenv(EnvTracesExporter, "none")
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusInstrumented {
		t.Fatalf("plan status = %q", plan.Status)
	}
	if len(plan.Signals) != 1 || plan.Signals[0] != SignalMetrics {
		t.Fatalf("signals = %+v, want metrics only", plan.Signals)
	}
	if plan.Config.TracesEnabled {
		t.Fatal("traces must be disabled by OTEL_TRACES_EXPORTER=none")
	}
	t.Setenv(EnvMetricsExporter, "none")
	plan = ResolvePlan(true)
	if plan.Status != ExportStatusNoSignal {
		t.Fatalf("plan status = %q, want %q", plan.Status, ExportStatusNoSignal)
	}
	if plan.Reason != ReasonNoSignal {
		t.Fatalf("plan reason = %q", plan.Reason)
	}
	if len(plan.Signals) != 0 {
		t.Fatalf("signals = %+v, want none", plan.Signals)
	}
}

// TestOTLPExporterSkipsDisabledSignals 锁定信号开关的实际效果：被关闭的信号
// 不发出任何请求，另一个信号照常送达。
func TestOTLPExporterSkipsDisabledSignals(t *testing.T) {
	receiver := newOTLPReceiver(t)
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	plan := receiverPlan(t, receiver)
	plan.Config.TracesEnabled = false
	plan.Signals = []string{SignalMetrics}
	exporter, err := NewOTLPExporter(plan)
	if err != nil {
		t.Fatal(err)
	}
	if err := exporter.Export(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if len(receiver.body("/v1/traces")) != 0 {
		t.Fatal("a disabled trace signal must not reach the receiver")
	}
	if len(receiver.body("/v1/metrics")) == 0 {
		t.Fatal("the enabled metrics signal must still be delivered")
	}
}

// TestResolvePlanRejectsUnsupportedExporter 锁定 exporter 取值的严格性：不受
// 支持的名称失败关闭，不被静默当作默认。
func TestResolvePlanRejectsUnsupportedExporter(t *testing.T) {
	t.Setenv(EnvTracesExporter, "OTLP")
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusConfigError {
		t.Fatalf("plan status = %q, want %q", plan.Status, ExportStatusConfigError)
	}
	if plan.Reason != ReasonInvalidExporter {
		t.Fatalf("plan reason = %q", plan.Reason)
	}
	t.Setenv(EnvTracesExporter, "otlp")
	t.Setenv(EnvMetricsExporter, "prometheus")
	plan = ResolvePlan(true)
	if plan.Status != ExportStatusInstrumented {
		t.Fatalf("plan status = %q", plan.Status)
	}
	if len(plan.Signals) != 1 || plan.Signals[0] != SignalTraces {
		t.Fatalf("signals = %+v, want traces only", plan.Signals)
	}
}

// TestResolvePlanRejectsUnsupportedProtocol 锁定传输协议边界：语义面只实现
// OTLP/HTTP+JSON，显式声明 gRPC 时失败关闭而不是静默用错协议。
func TestResolvePlanRejectsUnsupportedProtocol(t *testing.T) {
	t.Setenv(EnvProtocol, "grpc")
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusConfigError {
		t.Fatalf("plan status = %q, want %q", plan.Status, ExportStatusConfigError)
	}
	if plan.Reason != ReasonUnsupportedProtocol {
		t.Fatalf("plan reason = %q", plan.Reason)
	}
	t.Setenv(EnvProtocol, ProtocolHTTPJSON)
	plan = ResolvePlan(true)
	if plan.Status != ExportStatusInstrumented {
		t.Fatalf("plan status = %q", plan.Status)
	}
}

// TestResolvePlanAttributesInvalidEndpointToItsVariable 锁定端点错误的归因：
// trace 端点与指标端点的错误分别归到各自的变量，不得互相错挂。
func TestResolvePlanAttributesInvalidEndpointToItsVariable(t *testing.T) {
	t.Setenv(EnvTracesEndpoint, "not-a-url")
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusConfigError {
		t.Fatalf("plan status = %q", plan.Status)
	}
	if plan.Reason != ReasonInvalidTracesEndpoint {
		t.Fatalf("plan reason = %q, want %q", plan.Reason, ReasonInvalidTracesEndpoint)
	}
	t.Setenv(EnvTracesEndpoint, "http://127.0.0.1:4318/v1/traces")
	t.Setenv(EnvMetricsEndpoint, "not-a-url")
	plan = ResolvePlan(true)
	if plan.Reason != ReasonInvalidMetricsEndpoint {
		t.Fatalf("plan reason = %q, want %q", plan.Reason, ReasonInvalidMetricsEndpoint)
	}
}

// TestResolvePlanDerivesEndpoints 锁定端点推导：只给基础端点时导出面自行
// 拼接信号路径，缺省时回退到本地标准端口。
func TestResolvePlanDerivesEndpoints(t *testing.T) {
	t.Setenv(EnvTracesEndpoint, "")
	t.Setenv(EnvMetricsEndpoint, "")
	t.Setenv(EnvEndpoint, "http://127.0.0.1:4318/")
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusInstrumented {
		t.Fatalf("plan status = %q", plan.Status)
	}
	if plan.Config.TracesEndpoint != "http://127.0.0.1:4318/v1/traces" {
		t.Fatalf("traces endpoint = %q", plan.Config.TracesEndpoint)
	}
	if plan.Config.MetricsEndpoint != "http://127.0.0.1:4318/v1/metrics" {
		t.Fatalf("metrics endpoint = %q", plan.Config.MetricsEndpoint)
	}
	t.Setenv(EnvEndpoint, "")
	plan = ResolvePlan(true)
	if plan.Config.TracesEndpoint != DefaultOTLPTracesEndpoint || plan.Config.MetricsEndpoint != DefaultOTLPMetricsEndpoint {
		t.Fatalf("default endpoints = %+v", plan.Config)
	}
}

// TestResolvePlanRejectsMalformedHeaders 锁定请求头解析：格式错误失败关闭，
// 合法请求头只保留键值，不进入任何诊断输出。
func TestResolvePlanRejectsMalformedHeaders(t *testing.T) {
	t.Setenv(EnvHeaders, "broken-header")
	plan := ResolvePlan(true)
	if plan.Status != ExportStatusConfigError {
		t.Fatalf("plan status = %q", plan.Status)
	}
	if plan.Reason != ReasonInvalidHeaders {
		t.Fatalf("plan reason = %q", plan.Reason)
	}
	t.Setenv(EnvHeaders, "authorization=Bearer local-token")
	plan = ResolvePlan(true)
	if plan.Status != ExportStatusInstrumented {
		t.Fatalf("plan status = %q", plan.Status)
	}
	if len(plan.Config.Headers) != 1 || plan.Config.Headers[0].Name != "authorization" {
		t.Fatalf("headers = %+v", plan.Config.Headers)
	}
}

// TestAsyncExporterRecordsFailuresWithoutBlocking 锁定异步导出：接收器不可用
// 时导出失败只体现在统计里，投递本身永不阻塞调用方。
func TestAsyncExporterRecordsFailuresWithoutBlocking(t *testing.T) {
	exporter := NewAsyncExporter(&failingExporter{}, 4)
	defer exporter.Close()
	snapshot := Snapshot{RunID: "run-async", TraceID: TraceID("run-async")}
	if err := exporter.Export(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := exporter.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	stats := exporter.Stats()
	if stats.Failed != 1 || stats.Exported != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}

// TestAsyncExporterDropsWhenQueueIsFull 锁定背压策略：队列拥塞时丢弃并计数，
// 绝不阻塞运行路径。
func TestAsyncExporterDropsWhenQueueIsFull(t *testing.T) {
	blocker := &blockingExporter{release: make(chan struct{})}
	exporter := NewAsyncExporter(blocker, 1)
	defer func() {
		close(blocker.release)
		_ = exporter.Close()
	}()
	snapshot := Snapshot{RunID: "run-backpressure"}
	for index := 0; index < 8; index++ {
		_ = exporter.Export(context.Background(), snapshot)
	}
	if exporter.Stats().Dropped == 0 {
		t.Fatal("a saturated queue must drop snapshots instead of blocking")
	}
}

// blockingExporter 是阻塞式导出器替身，用于构造队列拥塞场景。
type blockingExporter struct {
	release chan struct{}
}

// Export 阻塞到释放信号到达为止。
func (exporter *blockingExporter) Export(ctx context.Context, _ Snapshot) error {
	select {
	case <-exporter.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
