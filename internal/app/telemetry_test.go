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
// 面的状态必须区分 disabled、instrumented 与 config_error，且 instrumented
// 只表示接线就绪，不宣称采集成功。
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
	t.Setenv(telemetry.EnvEndpoint, "not-a-url")
	report, err = service.Doctor(context.Background(), workspace, stateDir, enabled)
	if err != nil {
		t.Fatal(err)
	}
	if report.Telemetry.Semantic != TelemetrySemanticConfigError {
		t.Fatalf("semantic status = %q, want config_error", report.Telemetry.Semantic)
	}
}
