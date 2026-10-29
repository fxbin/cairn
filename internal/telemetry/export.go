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
	// EnvSDKDisabled 是 OTel 全局关闭开关；为真时任何观测导出都不得发生，
	// 语义面与 SDK 面共用它。
	EnvSDKDisabled = "OTEL_SDK_DISABLED"
	// EnvTracesExporter 是 trace 信号 exporter 选择的标准环境变量名。
	EnvTracesExporter = "OTEL_TRACES_EXPORTER"
	// EnvMetricsExporter 是 metrics 信号 exporter 选择的标准环境变量名。
	EnvMetricsExporter = "OTEL_METRICS_EXPORTER"
	// EnvProtocol 是 OTLP 传输协议的标准环境变量名。
	EnvProtocol = "OTEL_EXPORTER_OTLP_PROTOCOL"
	// EnvTracesProtocol 是 trace 信号传输协议的标准环境变量名。
	EnvTracesProtocol = "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL"
	// ProtocolHTTPJSON 是语义面唯一实现的 OTLP 传输协议。
	ProtocolHTTPJSON = "http/json"
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

// 语义导出面的接线状态取值。doctor 只报告这些状态，绝不据此宣称已经成功
// 采集到观测数据。
const (
	// ExportStatusDisabled 表示语义观测导出未被显式启用（默认状态）。
	ExportStatusDisabled = "disabled"
	// ExportStatusSDKDisabled 表示被 OTel 全局关闭开关禁用：此时不得导出
	// 任何观测数据。
	ExportStatusSDKDisabled = "sdk_disabled"
	// ExportStatusConfigError 表示显式启用但连接事实不受支持，导出失败关闭。
	ExportStatusConfigError = "config_error"
	// ExportStatusNoSignal 表示接线就绪，但两个信号都被 exporter 选择关闭，
	// 本次不会有任何数据外发。
	ExportStatusNoSignal = "no_signal"
	// ExportStatusInstrumented 表示导出接线就绪且至少一个信号会被发送。
	ExportStatusInstrumented = "instrumented"
)

// 语义导出面的稳定原因取值；只描述原因类别与出错的变量归属，不含端点、
// 请求头取值或任何本机路径。
const (
	// ReasonNotEnabled 表示调用方没有显式开启语义观测导出。
	ReasonNotEnabled = "not_enabled"
	// ReasonSDKDisabled 表示 OTEL_SDK_DISABLED 全局关闭了观测。
	ReasonSDKDisabled = "sdk_disabled"
	// ReasonInvalidExporter 表示 exporter 选择不受支持。
	ReasonInvalidExporter = "invalid_exporter"
	// ReasonUnsupportedProtocol 表示声明的传输协议不被语义面实现。
	ReasonUnsupportedProtocol = "unsupported_protocol"
	// ReasonInvalidTracesEndpoint 表示 trace 端点非法。
	ReasonInvalidTracesEndpoint = "invalid_traces_endpoint"
	// ReasonInvalidMetricsEndpoint 表示指标端点非法。
	ReasonInvalidMetricsEndpoint = "invalid_metrics_endpoint"
	// ReasonInvalidHeaders 表示请求头格式非法。
	ReasonInvalidHeaders = "invalid_headers"
	// ReasonNoSignal 表示两个信号都被 exporter 选择关闭。
	ReasonNoSignal = "no_signal"
)

// 语义导出面的信号名。
const (
	// SignalTraces 是 trace 信号。
	SignalTraces = "traces"
	// SignalMetrics 是 metrics 信号。
	SignalMetrics = "metrics"
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

// OTLPConfig 是语义导出面的连接事实：端点与请求头都来自标准 OTel 环境变量，
// 两个信号各自带开关。
type OTLPConfig struct {
	TracesEndpoint  string
	MetricsEndpoint string
	Headers         []Header
	Timeout         time.Duration
	// TracesEnabled 表示 trace 信号会被发送。
	TracesEnabled bool
	// MetricsEnabled 表示 metrics 信号会被发送。
	MetricsEnabled bool
}

// Plan 描述语义导出面的接线判定结果：状态、实际会发送的信号、连接事实与
// 稳定原因。运行路径与 doctor 共用它，保证两侧判定不会分叉。
type Plan struct {
	// Status 是接线状态，取 ExportStatus* 之一。
	Status string
	// Signals 列出实际会发送的信号；为空表示本次没有任何数据外发。
	Signals []string
	// Config 是接线合法时的连接事实。
	Config OTLPConfig
	// Reason 是稳定原因取值，说明状态为何如此。
	Reason string
}

// ResolvePlan 按启用开关与标准 OTel 环境变量判定语义导出接线。它是唯一的
// 判定入口：运行路径据此决定是否装配导出器，doctor 据此报告状态。
//
// 判定顺序：未启用 → disabled；OTEL_SDK_DISABLED 为真 → sdk_disabled（全局
// 关闭，任何数据都不得导出）；exporter 选择或传输协议不受支持 →
// config_error；端点或请求头非法 → config_error；两个信号都被关闭 →
// no_signal；其余 → instrumented。
func ResolvePlan(enabled bool) Plan {
	if !enabled {
		return Plan{Status: ExportStatusDisabled, Reason: ReasonNotEnabled}
	}
	if strings.EqualFold(os.Getenv(EnvSDKDisabled), "true") {
		return Plan{Status: ExportStatusSDKDisabled, Reason: ReasonSDKDisabled}
	}
	traces, tracesSupported := exporterEnabled(os.Getenv(EnvTracesExporter), false)
	metrics, metricsSupported := exporterEnabled(os.Getenv(EnvMetricsExporter), true)
	if !tracesSupported || !metricsSupported {
		return Plan{Status: ExportStatusConfigError, Reason: ReasonInvalidExporter}
	}
	if !protocolSupported(os.Getenv(EnvProtocol)) || !protocolSupported(os.Getenv(EnvTracesProtocol)) {
		return Plan{Status: ExportStatusConfigError, Reason: ReasonUnsupportedProtocol}
	}
	configuration, reason, err := resolveOTLPConfig()
	if err != nil {
		return Plan{Status: ExportStatusConfigError, Reason: reason}
	}
	configuration.TracesEnabled = traces
	configuration.MetricsEnabled = metrics
	plan := Plan{Status: ExportStatusInstrumented, Config: configuration}
	if traces {
		plan.Signals = append(plan.Signals, SignalTraces)
	}
	if metrics {
		plan.Signals = append(plan.Signals, SignalMetrics)
	}
	if len(plan.Signals) == 0 {
		plan.Status = ExportStatusNoSignal
		plan.Reason = ReasonNoSignal
	}
	return plan
}

// exporterEnabled 按上游 autoexport 的精确语义解析单个信号 exporter：空值
// 采用 otlp；none 表示该信号不导出；metrics 的 prometheus 由 SDK 面承担，
// 语义面不实现，按不导出处理；其它取值不受支持，失败关闭。名称严格区分
// 大小写且不裁剪，与 SDK 面保持同一口径。
func exporterEnabled(value string, allowPrometheus bool) (bool, bool) {
	switch value {
	case "":
		return true, true
	case "otlp":
		return true, true
	case "none":
		return false, true
	case "prometheus":
		return false, allowPrometheus
	default:
		return false, false
	}
}

// protocolSupported 判定语义面能否使用给定的 OTLP 传输协议。语义面只实现
// OTLP/HTTP+JSON，不引入 gRPC 依赖；显式声明其它协议时失败关闭，避免把
// JSON 发到 gRPC 端口造成静默失败。
func protocolSupported(value string) bool {
	return value == "" || value == ProtocolHTTPJSON
}

// resolveOTLPConfig 解析导出连接事实：端点与请求头只来自标准 OTel 环境变量，
// 没有显式端点时回退到本地标准端口；非法端点或请求头失败关闭，并给出
// 稳定原因与出错的变量归属。
func resolveOTLPConfig() (OTLPConfig, string, error) {
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
		return OTLPConfig{}, ReasonInvalidHeaders, err
	}
	configuration.Headers = headers
	if err := validateEndpoint(configuration.TracesEndpoint); err != nil {
		return OTLPConfig{}, ReasonInvalidTracesEndpoint, fmt.Errorf("%s: %w", EnvTracesEndpoint, err)
	}
	if err := validateEndpoint(configuration.MetricsEndpoint); err != nil {
		return OTLPConfig{}, ReasonInvalidMetricsEndpoint, fmt.Errorf("%s: %w", EnvMetricsEndpoint, err)
	}
	return configuration, "", nil
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

// NewOTLPExporter 按接线判定创建导出器。只有 instrumented 的判定才允许装配
// 导出器：被全局关闭、配置不合法或没有信号可发时一律拒绝，杜绝绕过判定
// 直接导出数据。
func NewOTLPExporter(plan Plan) (*OTLPExporter, error) {
	if plan.Status != ExportStatusInstrumented {
		return nil, fmt.Errorf("semantic telemetry export is %s, not instrumented", plan.Status)
	}
	if err := validateEndpoint(plan.Config.TracesEndpoint); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvTracesEndpoint, err)
	}
	if err := validateEndpoint(plan.Config.MetricsEndpoint); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvMetricsEndpoint, err)
	}
	timeout := plan.Config.Timeout
	if timeout <= 0 {
		timeout = DefaultExportTimeout
	}
	return &OTLPExporter{configuration: plan.Config, client: &http.Client{Timeout: timeout}}, nil
}

// Export 导出一个快照的跨度与度量；任一信号失败都返回错误，调用方只记录
// 失败事实，不改变运行终态。被 exporter 选择关闭的信号不会发出任何请求。
func (exporter *OTLPExporter) Export(ctx context.Context, snapshot Snapshot) error {
	if exporter.configuration.TracesEnabled && len(snapshot.Spans) > 0 {
		body, err := encodeTraceRequest(snapshot)
		if err != nil {
			return err
		}
		if err := exporter.post(ctx, exporter.configuration.TracesEndpoint, body); err != nil {
			return fmt.Errorf("export traces: %w", err)
		}
	}
	if exporter.configuration.MetricsEnabled {
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
