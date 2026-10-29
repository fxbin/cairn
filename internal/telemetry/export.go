// 本文件实现语义观测的导出面：只把受限的聚合结果与关联跨度送到接收器。
//
// Author: fxbin
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 导出端点与请求头只从标准 OTel 环境变量读取；配置面不携带端点与凭据。
const (
	// EnvTracesEndpoint 是 OTLP trace 端点的标准环境变量名。
	EnvTracesEndpoint = "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"
	// EnvMetricsEndpoint 是 OTLP 指标端点的标准环境变量名。
	EnvMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	// EnvEndpoint 是 OTLP 基础端点的标准环境变量名。
	EnvEndpoint = "OTEL_EXPORTER_OTLP_ENDPOINT"
	// EnvHeaders 是 OTLP 请求头的标准环境变量名。
	EnvHeaders = "OTEL_EXPORTER_OTLP_HEADERS"
	// DefaultOTLPTracesEndpoint 是没有显式端点时的本地 trace 接收地址。
	DefaultOTLPTracesEndpoint = "http://127.0.0.1:4318/v1/traces"
	// DefaultOTLPMetricsEndpoint 是没有显式端点时的本地指标接收地址。
	DefaultOTLPMetricsEndpoint = "http://127.0.0.1:4318/v1/metrics"
	// DefaultExportTimeout 是单次导出请求的默认超时。
	DefaultExportTimeout = 5 * time.Second
	// DefaultExportQueueSize 是异步导出队列的默认长度。
	DefaultExportQueueSize = 64
	// exportFlushPollInterval 是收束待导出快照时的轮询间隔。
	exportFlushPollInterval = 5 * time.Millisecond
	// exportMaxResponseBytes 是导出响应体的读取上限；超出即停止读取。
	exportMaxResponseBytes = 1 << 20
)

// Exporter 导出一个运行的语义快照。实现必须只输出快照中的受限字段，
// 绝不能把事件负载、Prompt、工具结果或本机路径写进导出内容。
type Exporter interface {
	Export(ctx context.Context, snapshot Snapshot) error
}

// Header 是一条导出请求头；取值只存在于内存，不写入日志、事件或报告。
type Header struct {
	Name  string
	Value string
}

// OTLPConfig 是语义导出面的连接事实：端点与请求头都来自标准 OTel 环境变量。
type OTLPConfig struct {
	TracesEndpoint  string
	MetricsEndpoint string
	Headers         []Header
	Timeout         time.Duration
}

// OTLPConfigFromEnv 读取标准 OTel 环境变量并给出导出连接事实。没有显式端点
// 时回退到本地标准端口；端点非法或请求头格式错误时失败关闭。
func OTLPConfigFromEnv() (OTLPConfig, error) {
	base := strings.TrimSpace(os.Getenv(EnvEndpoint))
	configuration := OTLPConfig{
		TracesEndpoint:  strings.TrimSpace(os.Getenv(EnvTracesEndpoint)),
		MetricsEndpoint: strings.TrimSpace(os.Getenv(EnvMetricsEndpoint)),
		Timeout:         DefaultExportTimeout,
	}
	if configuration.TracesEndpoint == "" && base != "" {
		configuration.TracesEndpoint = strings.TrimSuffix(base, "/") + "/v1/traces"
	}
	if configuration.TracesEndpoint == "" {
		configuration.TracesEndpoint = DefaultOTLPTracesEndpoint
	}
	if configuration.MetricsEndpoint == "" && base != "" {
		configuration.MetricsEndpoint = strings.TrimSuffix(base, "/") + "/v1/metrics"
	}
	if configuration.MetricsEndpoint == "" {
		configuration.MetricsEndpoint = DefaultOTLPMetricsEndpoint
	}
	headers, err := parseExportHeaders(os.Getenv(EnvHeaders))
	if err != nil {
		return OTLPConfig{}, err
	}
	configuration.Headers = headers
	if err := validateEndpoint(configuration.TracesEndpoint); err != nil {
		return OTLPConfig{}, fmt.Errorf("%s: %w", EnvTracesEndpoint, err)
	}
	if err := validateEndpoint(configuration.MetricsEndpoint); err != nil {
		return OTLPConfig{}, fmt.Errorf("%s: %w", EnvEndpoint, err)
	}
	return configuration, nil
}

// parseExportHeaders 解析标准 OTel 请求头串（逗号分隔的键值对）；空串表示
// 不带请求头，格式错误失败关闭。
func parseExportHeaders(raw string) ([]Header, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, nil
	}
	headers := make([]Header, 0, 4)
	for _, item := range strings.Split(trimmed, ",") {
		entry := strings.TrimSpace(item)
		if entry == "" {
			continue
		}
		name, value, found := strings.Cut(entry, "=")
		name = strings.TrimSpace(name)
		if !found || name == "" {
			return nil, fmt.Errorf("%s entry %q must be name=value", EnvHeaders, entry)
		}
		headers = append(headers, Header{Name: name, Value: strings.TrimSpace(value)})
	}
	return headers, nil
}

// validateEndpoint 校验导出端点：必须是带主机的 http 或 https 地址。
func validateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("endpoint must be a valid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("endpoint scheme must be http or https")
	}
	if parsed.Host == "" {
		return errors.New("endpoint must carry a host")
	}
	return nil
}

// OTLPExporter 用 OTLP/JSON 把语义跨度与度量导出到接收器。它只序列化快照中
// 的受限字段：追踪标识、跨度标识、计数与枚举标签，绝不序列化事件负载。
type OTLPExporter struct {
	configuration OTLPConfig
	client        *http.Client
}

// NewOTLPExporter 校验连接事实并创建导出器；非法端点失败关闭。
func NewOTLPExporter(configuration OTLPConfig) (*OTLPExporter, error) {
	if err := validateEndpoint(configuration.TracesEndpoint); err != nil {
		return nil, fmt.Errorf("traces endpoint: %w", err)
	}
	if err := validateEndpoint(configuration.MetricsEndpoint); err != nil {
		return nil, fmt.Errorf("metrics endpoint: %w", err)
	}
	timeout := configuration.Timeout
	if timeout <= 0 {
		timeout = DefaultExportTimeout
	}
	return &OTLPExporter{configuration: configuration, client: &http.Client{Timeout: timeout}}, nil
}

// Export 导出一个快照的跨度与度量；任一信号失败都返回错误，调用方只记录
// 失败事实，不改变运行终态。
func (exporter *OTLPExporter) Export(ctx context.Context, snapshot Snapshot) error {
	if len(snapshot.Spans) > 0 {
		body, err := encodeTraceRequest(snapshot)
		if err != nil {
			return err
		}
		if err := exporter.post(ctx, exporter.configuration.TracesEndpoint, body); err != nil {
			return fmt.Errorf("export traces: %w", err)
		}
	}
	points := snapshot.MetricPoints()
	if len(points) > 0 {
		body, err := encodeMetricRequest(snapshot, points)
		if err != nil {
			return err
		}
		if err := exporter.post(ctx, exporter.configuration.MetricsEndpoint, body); err != nil {
			return fmt.Errorf("export metrics: %w", err)
		}
	}
	return nil
}

// post 发送一次导出请求；响应体只用于判定成功与否，不进入任何日志。
func (exporter *OTLPExporter) post(ctx context.Context, endpoint string, body []byte) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	for _, header := range exporter.configuration.Headers {
		request.Header.Set(header.Name, header.Value)
	}
	response, err := exporter.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, exportMaxResponseBytes))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("receiver returned status %d", response.StatusCode)
	}
	return nil
}

// ExportStats 汇总异步导出的处理结果；它只用于诊断，不影响运行终态。
type ExportStats struct {
	Exported int `json:"exported"`
	Failed   int `json:"failed"`
	Dropped  int `json:"dropped"`
}

// AsyncExporter 把导出放到后台执行：队列有界，队列满时丢弃并计数，
// 导出失败只体现在统计里，绝不阻塞或改变运行终态。
type AsyncExporter struct {
	inner   Exporter
	queue   chan Snapshot
	closed  chan struct{}
	once    sync.Once
	mu      sync.Mutex
	pending int
	stats   ExportStats
	stopped bool
}

// NewAsyncExporter 创建带后台工作队列的导出器；队列长度非正时使用默认值。
func NewAsyncExporter(inner Exporter, queueSize int) *AsyncExporter {
	if queueSize <= 0 {
		queueSize = DefaultExportQueueSize
	}
	exporter := &AsyncExporter{inner: inner, queue: make(chan Snapshot, queueSize), closed: make(chan struct{})}
	go exporter.work()
	return exporter
}

// work 消费队列中的快照；单个快照失败只更新统计，不中断队列。
func (exporter *AsyncExporter) work() {
	for snapshot := range exporter.queue {
		exporter.deliver(snapshot)
	}
	close(exporter.closed)
}

// deliver 导出单个快照并更新统计。待处理计数在导出完成后才回落，
// 因此收束等待的是真正送达或真正失败，而不是刚出队。
func (exporter *AsyncExporter) deliver(snapshot Snapshot) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultExportTimeout)
	defer cancel()
	err := exporter.inner.Export(ctx, snapshot)
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if err != nil {
		exporter.stats.Failed++
	} else {
		exporter.stats.Exported++
	}
	exporter.pending--
}

// Export 投递一个快照；队列已满时丢弃并计数，投递永不阻塞运行路径。
// 导出器关闭后投递被拒绝，绝不向已关闭的队列写入。
func (exporter *AsyncExporter) Export(_ context.Context, snapshot Snapshot) error {
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	if exporter.stopped {
		return errors.New("exporter is closed")
	}
	exporter.pending++
	select {
	case exporter.queue <- snapshot:
		return nil
	default:
		exporter.pending--
		exporter.stats.Dropped++
		return errors.New("export queue is full")
	}
}

// Flush 等待已投递的快照全部处理完毕；上下文到期即返回超时错误，调用方
// 只需记录失败，不得据此改变运行结果。
func (exporter *AsyncExporter) Flush(ctx context.Context) error {
	for {
		exporter.mu.Lock()
		pending := exporter.pending
		exporter.mu.Unlock()
		if pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(exportFlushPollInterval):
		}
	}
}

// Stats 返回异步导出的处理统计。
func (exporter *AsyncExporter) Stats() ExportStats {
	exporter.mu.Lock()
	defer exporter.mu.Unlock()
	return exporter.stats
}

// Close 关闭队列并等待后台工作退出；重复调用安全。
func (exporter *AsyncExporter) Close() error {
	exporter.once.Do(func() {
		exporter.mu.Lock()
		exporter.stopped = true
		exporter.mu.Unlock()
		close(exporter.queue)
	})
	<-exporter.closed
	return nil
}

// 以下结构体是 OTLP/JSON 的最小编码形状：它们只承载受限字段，
// 事件负载没有任何进入导出内容的路径。

// otlpTraceRequest 是 OTLP trace 导出请求体。
type otlpTraceRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

// otlpResourceSpans 聚合一个资源下的全部作用域跨度。
type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

// otlpResource 描述产生观测的资源身份。
type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

// otlpScopeSpans 聚合一个作用域下的跨度集合。
type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

// otlpScope 描述产生观测的代码作用域。
type otlpScope struct {
	Name string `json:"name"`
}

// otlpSpan 是一个导出跨度；标识使用十六进制字符串，符合 OTLP/JSON 约定。
type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}

// otlpStatus 描述跨度结论；未成功也未失败时保持未设置。
type otlpStatus struct {
	Code int `json:"code"`
}

// otlpKeyValue 是一个导出属性键值对。
type otlpKeyValue struct {
	Key   string       `json:"key"`
	Value otlpAnyValue `json:"value"`
}

// otlpAnyValue 是导出属性的取值包装；观测属性只使用字符串值。
type otlpAnyValue struct {
	StringValue string `json:"stringValue,omitempty"`
}

// otlpMetricsRequest 是 OTLP 指标导出请求体。
type otlpMetricsRequest struct {
	ResourceMetrics []otlpResourceMetrics `json:"resourceMetrics"`
}

// otlpResourceMetrics 聚合一个资源下的全部作用域指标。
type otlpResourceMetrics struct {
	Resource     otlpResource       `json:"resource"`
	ScopeMetrics []otlpScopeMetrics `json:"scopeMetrics"`
}

// otlpScopeMetrics 聚合一个作用域下的指标集合。
type otlpScopeMetrics struct {
	Scope   otlpScope    `json:"scope"`
	Metrics []otlpMetric `json:"metrics"`
}

// otlpMetric 是一条导出指标。
type otlpMetric struct {
	Name  string    `json:"name"`
	Unit  string    `json:"unit,omitempty"`
	Gauge otlpGauge `json:"gauge"`
}

// otlpGauge 承载指标的当前值集合。
type otlpGauge struct {
	DataPoints []otlpNumberDataPoint `json:"dataPoints"`
}

// otlpNumberDataPoint 是一个指标数据点；整数值按 OTLP/JSON 约定编码为字符串。
type otlpNumberDataPoint struct {
	Attributes   []otlpKeyValue `json:"attributes,omitempty"`
	TimeUnixNano string         `json:"timeUnixNano"`
	AsInt        string         `json:"asInt"`
}

// otlpScopeName 是导出观测的作用域名。
const otlpScopeName = "github.com/fxbin/cairn/internal/telemetry"

// encodeTraceRequest 把快照编码为 OTLP/JSON trace 请求体。
func encodeTraceRequest(snapshot Snapshot) ([]byte, error) {
	spans := make([]otlpSpan, 0, len(snapshot.Spans))
	for _, span := range snapshot.Spans {
		spans = append(spans, otlpSpan{
			TraceID:           span.TraceID,
			SpanID:            span.SpanID,
			ParentSpanID:      span.ParentID,
			Name:              span.Name,
			Kind:              otlpSpanKindInternal,
			StartTimeUnixNano: strconv.FormatInt(span.Start.UnixNano(), 10),
			EndTimeUnixNano:   strconv.FormatInt(span.End.UnixNano(), 10),
			Attributes:        otlpAttributes(span.Attributes),
			Status:            otlpStatusFor(span),
		})
	}
	request := otlpTraceRequest{ResourceSpans: []otlpResourceSpans{{
		Resource:   otlpResource{Attributes: otlpResourceAttributes()},
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{Name: otlpScopeName}, Spans: spans}},
	}}}
	return json.Marshal(request)
}

// encodeMetricRequest 把快照的语义度量编码为 OTLP/JSON 指标请求体。
func encodeMetricRequest(snapshot Snapshot, points []Metric) ([]byte, error) {
	metrics := make([]otlpMetric, 0, len(points))
	for _, point := range points {
		metrics = append(metrics, otlpMetric{
			Name: point.Name,
			Unit: "1",
			Gauge: otlpGauge{DataPoints: []otlpNumberDataPoint{{
				Attributes:   otlpAttributes(point.Labels),
				TimeUnixNano: strconv.FormatInt(snapshotExportTime(snapshot).UnixNano(), 10),
				AsInt:        strconv.FormatInt(point.Value, 10),
			}}},
		})
	}
	request := otlpMetricsRequest{ResourceMetrics: []otlpResourceMetrics{{
		Resource:     otlpResource{Attributes: otlpResourceAttributes()},
		ScopeMetrics: []otlpScopeMetrics{{Scope: otlpScope{Name: otlpScopeName}, Metrics: metrics}},
	}}}
	return json.Marshal(request)
}

// snapshotExportTime 返回导出时刻：取运行跨度结束时刻，缺失时退回当前时刻。
func snapshotExportTime(snapshot Snapshot) time.Time {
	for _, span := range snapshot.Spans {
		if span.Kind == ScopeKindRun && !span.End.IsZero() {
			return span.End
		}
	}
	return time.Now().UTC()
}

// otlpResourceAttributes 给出资源级属性；它们是常量，不含任何运行内容。
func otlpResourceAttributes() []otlpKeyValue {
	return []otlpKeyValue{
		{Key: "service.name", Value: otlpAnyValue{StringValue: "cairn"}},
		{Key: "cairn.telemetry.source", Value: otlpAnyValue{StringValue: "journal"}},
	}
}

// otlpAttributes 把受限属性转换为导出属性。
func otlpAttributes(attributes []Attribute) []otlpKeyValue {
	if len(attributes) == 0 {
		return nil
	}
	converted := make([]otlpKeyValue, 0, len(attributes))
	for _, attribute := range attributes {
		converted = append(converted, otlpKeyValue{Key: attribute.Key, Value: otlpAnyValue{StringValue: attribute.Value}})
	}
	return converted
}

// otlpSpanKindInternal 是内部处理跨度的 OTLP 枚举值。
const otlpSpanKindInternal = 1

// otlpStatusFor 按跨度结论给出状态码：成功为 OK，失败为 ERROR，其余未设置。
func otlpStatusFor(span Span) otlpStatus {
	for _, attribute := range span.Attributes {
		if attribute.Key != "outcome" {
			continue
		}
		switch attribute.Value {
		case OutcomeCompleted, "passed", "succeeded":
			return otlpStatus{Code: 1}
		case OutcomeFailed:
			return otlpStatus{Code: 2}
		}
	}
	return otlpStatus{Code: 0}
}
