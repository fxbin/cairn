package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/telemetry"
)

// telemetryTaskMarker 是测试任务的唯一标记；它绝不允许出现在任何导出内容里。
const telemetryTaskMarker = "semantic-telemetry-task-marker"

// telemetryReceiver 是本地语义观测接收器替身：按路径记录收到的请求体。
type telemetryReceiver struct {
	server   *httptest.Server
	mutex    sync.Mutex
	received map[string][]byte
}

// newTelemetryReceiver 启动本地接收器；测试结束时自动关闭。
func newTelemetryReceiver(t *testing.T) *telemetryReceiver {
	t.Helper()
	receiver := &telemetryReceiver{received: make(map[string][]byte)}
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
func (receiver *telemetryReceiver) body(path string) []byte {
	receiver.mutex.Lock()
	defer receiver.mutex.Unlock()
	return append([]byte(nil), receiver.received[path]...)
}

// runSpanNames 解析 trace 请求体，返回其中的跨度名集合。
func runSpanNames(t *testing.T, body []byte) map[string]string {
	t.Helper()
	var decoded struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct {
					TraceID string `json:"traceId"`
					Name    string `json:"name"`
				} `json:"spans"`
			} `json:"scopeSpans"`
		} `json:"resourceSpans"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode trace request: %v", err)
	}
	names := make(map[string]string)
	for _, resourceSpan := range decoded.ResourceSpans {
		for _, scopeSpan := range resourceSpan.ScopeSpans {
			for _, span := range scopeSpan.Spans {
				names[span.Name] = span.TraceID
			}
		}
	}
	return names
}

// TestServiceRunExportsSemanticTraceToLocalReceiver 锁定端到端导出：显式开启
// 语义观测后，一次真实运行必须把关联 Trace 送达本地接收器，且导出内容不含
// 任务正文。
func TestServiceRunExportsSemanticTraceToLocalReceiver(t *testing.T) {
	receiver := newTelemetryReceiver(t)
	t.Setenv(telemetry.EnvEndpoint, receiver.server.URL)
	configuration := withTestProvider(config.Defaults())
	configuration.Telemetry.Enabled = true
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: telemetryTaskMarker, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "done" {
		t.Fatalf("result = %#v", result)
	}
	body := receiver.body("/v1/traces")
	if len(body) == 0 {
		t.Fatal("local receiver did not receive a trace request")
	}
	if strings.Contains(string(body), telemetryTaskMarker) {
		t.Fatal("exported trace must not carry the task body")
	}
	names := runSpanNames(t, body)
	traceID, found := names[telemetry.SpanNameRun]
	if !found {
		t.Fatalf("exported trace carries no run span: %+v", names)
	}
	if traceID != telemetry.TraceID(result.ID) {
		t.Fatalf("trace id = %q, want %q", traceID, telemetry.TraceID(result.ID))
	}
	if len(receiver.body("/v1/metrics")) == 0 {
		t.Fatal("local receiver did not receive a metrics request")
	}
}

// TestServiceRunSurvivesUnreachableTelemetryReceiver 锁定失败开放合同：观测
// 系统故障不得改变运行终态、结果或事件日志。
func TestServiceRunSurvivesUnreachableTelemetryReceiver(t *testing.T) {
	t.Setenv(telemetry.EnvEndpoint, "http://127.0.0.1:1")
	configuration := withTestProvider(config.Defaults())
	configuration.Telemetry.Enabled = true
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: telemetryTaskMarker, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatalf("an unreachable telemetry receiver must not break the run: %v", err)
	}
	if result.Response != "done" {
		t.Fatalf("result = %#v", result)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, result.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("run state = %q, want completed", inspected.Projection.State)
	}
}

// TestServiceRunWithoutTelemetryKeepsZeroExport 锁定默认关闭合同：未显式开启
// 时不建立任何导出连接，运行路径与既有行为完全一致。
func TestServiceRunWithoutTelemetryKeepsZeroExport(t *testing.T) {
	receiver := newTelemetryReceiver(t)
	t.Setenv(telemetry.EnvEndpoint, receiver.server.URL)
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	if _, err := service.Run(context.Background(), RunOptions{Task: telemetryTaskMarker, Workspace: workspace, StateDir: stateDir, Config: configuration}); err != nil {
		t.Fatal(err)
	}
	if len(receiver.body("/v1/traces")) != 0 || len(receiver.body("/v1/metrics")) != 0 {
		t.Fatal("a disabled telemetry plane must not export anything")
	}
}

// TestDoctorDistinguishesSemanticTelemetryStatus 锁定诊断如实性：语义观测导出
// 面的状态必须区分 disabled、sdk_disabled、no_signal、instrumented 与
// config_error，且 instrumented 只表示接线就绪，不宣称采集成功。
func TestDoctorDistinguishesSemanticTelemetryStatus(t *testing.T) {
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	disabled := withTestProvider(config.Defaults())
	report, err := service.Doctor(context.Background(), workspace, stateDir, disabled)
	if err != nil {
		t.Fatal(err)
	}
	if report.Telemetry.Semantic != TelemetrySemanticDisabled {
		t.Fatalf("semantic status = %q, want disabled", report.Telemetry.Semantic)
	}
	t.Setenv(telemetry.EnvEndpoint, "http://127.0.0.1:4318")
	enabled := withTestProvider(config.Defaults())
	enabled.Telemetry.Enabled = true
	report, err = service.Doctor(context.Background(), workspace, stateDir, enabled)
	if err != nil {
		t.Fatal(err)
	}
	if report.Telemetry.Semantic != TelemetrySemanticInstrumented {
		t.Fatalf("semantic status = %q, want instrumented", report.Telemetry.Semantic)
	}
	if report.Telemetry.SemanticSignals != "traces,metrics" {
		t.Fatalf("semantic signals = %q", report.Telemetry.SemanticSignals)
	}
	// 被 OTel 全局关闭开关禁用必须与"未启用"和"配置不合法"分开报告。
	t.Setenv(telemetry.EnvSDKDisabled, "true")
	report, err = service.Doctor(context.Background(), workspace, stateDir, enabled)
	if err != nil {
		t.Fatal(err)
	}
	if report.Telemetry.Semantic != TelemetrySemanticSDKDisabled {
		t.Fatalf("semantic status = %q, want sdk_disabled", report.Telemetry.Semantic)
	}
	if report.Telemetry.SemanticReason != telemetry.ReasonSDKDisabled {
		t.Fatalf("semantic reason = %q", report.Telemetry.SemanticReason)
	}
	t.Setenv(telemetry.EnvSDKDisabled, "")
	t.Setenv(telemetry.EnvEndpoint, "not-a-url")
	report, err = service.Doctor(context.Background(), workspace, stateDir, enabled)
	if err != nil {
		t.Fatal(err)
	}
	if report.Telemetry.Semantic != TelemetrySemanticConfigError {
		t.Fatalf("semantic status = %q, want config_error", report.Telemetry.Semantic)
	}
	if report.Telemetry.SemanticReason != telemetry.ReasonInvalidTracesEndpoint {
		t.Fatalf("semantic reason = %q", report.Telemetry.SemanticReason)
	}
}

// TestServiceRunWithGlobalSDKDisableExportsNothing 锁定全局关闭合同：OTEL 全局
// 关闭开关为真时，语义面不得发出任何请求，运行照常完成。
func TestServiceRunWithGlobalSDKDisableExportsNothing(t *testing.T) {
	receiver := newTelemetryReceiver(t)
	t.Setenv(telemetry.EnvEndpoint, receiver.server.URL)
	t.Setenv(telemetry.EnvSDKDisabled, "true")
	configuration := withTestProvider(config.Defaults())
	configuration.Telemetry.Enabled = true
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{Task: telemetryTaskMarker, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatalf("a globally disabled telemetry plane must not break the run: %v", err)
	}
	if result.Response != "done" {
		t.Fatalf("result = %#v", result)
	}
	if len(receiver.body("/v1/traces")) != 0 || len(receiver.body("/v1/metrics")) != 0 {
		t.Fatal("a globally disabled telemetry plane must not export anything")
	}
}

// TestServiceRunWithTracesExporterNoneSendsNoTrace 锁定 exporter 选择语义：
// OTEL_TRACES_EXPORTER=none 时不得有 trace 请求，指标信号照常发送。
func TestServiceRunWithTracesExporterNoneSendsNoTrace(t *testing.T) {
	receiver := newTelemetryReceiver(t)
	t.Setenv(telemetry.EnvEndpoint, receiver.server.URL)
	t.Setenv(telemetry.EnvTracesExporter, "none")
	configuration := withTestProvider(config.Defaults())
	configuration.Telemetry.Enabled = true
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	if _, err := service.Run(context.Background(), RunOptions{Task: telemetryTaskMarker, Workspace: workspace, StateDir: stateDir, Config: configuration}); err != nil {
		t.Fatal(err)
	}
	if len(receiver.body("/v1/traces")) != 0 {
		t.Fatal("OTEL_TRACES_EXPORTER=none must not produce a trace request")
	}
	if len(receiver.body("/v1/metrics")) == 0 {
		t.Fatal("the metrics signal must still be delivered")
	}
}

// TestDoctorReportsExportFailuresFromPersistedStats 锁定导出故障的可观测性：
// 接收器不可用时的失败计数必须落盘并在 doctor 的语义面可见。
func TestDoctorReportsExportFailuresFromPersistedStats(t *testing.T) {
	t.Setenv(telemetry.EnvEndpoint, "http://127.0.0.1:1")
	configuration := withTestProvider(config.Defaults())
	configuration.Telemetry.Enabled = true
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	if _, err := service.Run(context.Background(), RunOptions{Task: telemetryTaskMarker, Workspace: workspace, StateDir: stateDir, Config: configuration}); err != nil {
		t.Fatal(err)
	}
	report, err := service.Doctor(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if report.Telemetry.Semantic != TelemetrySemanticInstrumented {
		t.Fatalf("semantic status = %q", report.Telemetry.Semantic)
	}
	if report.Telemetry.SemanticExports == nil {
		t.Fatal("doctor must report the recorded export counts")
	}
	if report.Telemetry.SemanticExports.Failed == 0 {
		t.Fatalf("export failures must be visible in doctor: %+v", report.Telemetry.SemanticExports)
	}
	if report.Telemetry.SemanticExports.Exported != 0 {
		t.Fatalf("no export can succeed against a closed port: %+v", report.Telemetry.SemanticExports)
	}
}

// TestSemanticTelemetryDecisionAgreesBetweenRuntimeAndDoctor 锁定运行路径与
// doctor 的判定同源：同一组环境变量下，运行时是否装配导出器与 doctor 报告
// 的语义面状态必须一致，不会出现"doctor 说可用、运行却不导出"的分叉。
func TestSemanticTelemetryDecisionAgreesBetweenRuntimeAndDoctor(t *testing.T) {
	cases := []struct {
		name        string
		enabled     bool
		environment map[string]string
		wantStatus  string
	}{
		{name: "未启用", enabled: false, wantStatus: telemetry.ExportStatusDisabled},
		{name: "全局关闭", enabled: true, environment: map[string]string{telemetry.EnvSDKDisabled: "true"}, wantStatus: telemetry.ExportStatusSDKDisabled},
		{name: "端点非法", enabled: true, environment: map[string]string{telemetry.EnvEndpoint: "not-a-url"}, wantStatus: telemetry.ExportStatusConfigError},
		{name: "协议不受支持", enabled: true, environment: map[string]string{telemetry.EnvProtocol: "grpc"}, wantStatus: telemetry.ExportStatusConfigError},
		{name: "exporter 取值非法", enabled: true, environment: map[string]string{telemetry.EnvTracesExporter: "OTLP"}, wantStatus: telemetry.ExportStatusConfigError},
		{name: "两个信号都被关闭", enabled: true, environment: map[string]string{telemetry.EnvTracesExporter: "none", telemetry.EnvMetricsExporter: "none"}, wantStatus: telemetry.ExportStatusNoSignal},
		{name: "接线就绪", enabled: true, environment: map[string]string{telemetry.EnvEndpoint: "http://127.0.0.1:4318"}, wantStatus: telemetry.ExportStatusInstrumented},
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(telemetry.EnvSDKDisabled, "")
			t.Setenv(telemetry.EnvEndpoint, "")
			t.Setenv(telemetry.EnvTracesExporter, "")
			t.Setenv(telemetry.EnvMetricsExporter, "")
			t.Setenv(telemetry.EnvProtocol, "")
			t.Setenv(telemetry.EnvTracesProtocol, "")
			for name, value := range testCase.environment {
				t.Setenv(name, value)
			}
			configuration := withTestProvider(config.Defaults())
			configuration.Telemetry.Enabled = testCase.enabled
			workspace, stateDir := t.TempDir(), t.TempDir()
			report, err := service.Doctor(context.Background(), workspace, stateDir, configuration)
			if err != nil {
				t.Fatal(err)
			}
			if report.Telemetry.Semantic != testCase.wantStatus {
				t.Fatalf("doctor semantic status = %q, want %q", report.Telemetry.Semantic, testCase.wantStatus)
			}
			runtime, plan, err := newTelemetryRuntime(configuration)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Status != report.Telemetry.Semantic {
				t.Fatalf("runtime plan status %q diverges from doctor status %q", plan.Status, report.Telemetry.Semantic)
			}
			wantRuntime := testCase.wantStatus == telemetry.ExportStatusInstrumented
			if (runtime != nil) != wantRuntime {
				t.Fatalf("runtime assembled = %t, want %t for status %q", runtime != nil, wantRuntime, plan.Status)
			}
			if runtime != nil {
				_ = runtime.Close()
			}
		})
	}
}
