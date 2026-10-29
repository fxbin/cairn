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

// TestOTLPExporterDeliversTraceToLocalReceiver 锁定真实导出：开启后本地接收器
// 必须收到一条结构合法的运行 Trace 与一组语义度量，追踪标识使用十六进制。
func TestOTLPExporterDeliversTraceToLocalReceiver(t *testing.T) {
	receiver := newOTLPReceiver(t)
	events := telemetryFixture(t)
	snapshot, ok := Aggregate(events, fixtureLimits(), 100)
	if !ok {
		t.Fatal("offline rebuild must produce a snapshot")
	}
	exporter, err := NewOTLPExporter(OTLPConfig{
		TracesEndpoint:  receiver.server.URL + "/v1/traces",
		MetricsEndpoint: receiver.server.URL + "/v1/metrics",
	})
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
	for _, expected := range []string{MetricRunTurns, MetricRunDurationMS, MetricRunInputTokens, MetricBudgetTurnsUsed} {
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
	exporter, err := NewOTLPExporter(OTLPConfig{
		TracesEndpoint:  receiver.server.URL + "/v1/traces",
		MetricsEndpoint: receiver.server.URL + "/v1/metrics",
	})
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

// TestOTLPConfigFromEnvRejectsUnsupportedInput 锁定连接事实的失败关闭：非法
// 端点与非法请求头都不允许进入导出路径。
func TestOTLPConfigFromEnvRejectsUnsupportedInput(t *testing.T) {
	t.Setenv(EnvTracesEndpoint, "not-a-url")
	if _, err := OTLPConfigFromEnv(); err == nil {
		t.Fatal("invalid traces endpoint must fail closed")
	}
	t.Setenv(EnvTracesEndpoint, "ftp://127.0.0.1:4318/v1/traces")
	if _, err := OTLPConfigFromEnv(); err == nil {
		t.Fatal("unsupported scheme must fail closed")
	}
	t.Setenv(EnvTracesEndpoint, "http://127.0.0.1:4318/v1/traces")
	t.Setenv(EnvHeaders, "broken-header")
	if _, err := OTLPConfigFromEnv(); err == nil {
		t.Fatal("malformed headers must fail closed")
	}
	t.Setenv(EnvHeaders, "authorization=Bearer local-token")
	configuration, err := OTLPConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.TracesEndpoint != "http://127.0.0.1:4318/v1/traces" {
		t.Fatalf("traces endpoint = %q", configuration.TracesEndpoint)
	}
	if len(configuration.Headers) != 1 || configuration.Headers[0].Name != "authorization" {
		t.Fatalf("headers = %+v", configuration.Headers)
	}
}

// TestOTLPConfigFromEnvDerivesTracesEndpoint 锁定端点推导：只给基础端点时
// 导出面必须自行拼接 trace 路径，缺省时回退到本地标准端口。
func TestOTLPConfigFromEnvDerivesTracesEndpoint(t *testing.T) {
	t.Setenv(EnvTracesEndpoint, "")
	t.Setenv(EnvEndpoint, "http://127.0.0.1:4318/")
	configuration, err := OTLPConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.TracesEndpoint != "http://127.0.0.1:4318/v1/traces" {
		t.Fatalf("traces endpoint = %q", configuration.TracesEndpoint)
	}
	if configuration.MetricsEndpoint != "http://127.0.0.1:4318/v1/metrics" {
		t.Fatalf("metrics endpoint = %q", configuration.MetricsEndpoint)
	}
	t.Setenv(EnvEndpoint, "")
	configuration, err = OTLPConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if configuration.TracesEndpoint != DefaultOTLPTracesEndpoint || configuration.MetricsEndpoint != DefaultOTLPMetricsEndpoint {
		t.Fatalf("default endpoints = %+v", configuration)
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
