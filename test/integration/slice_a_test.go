package integration_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// withTestProvider 为测试配置补齐 V1.4 能力卡所需的模型与显式声明。
func withTestProvider(configuration config.Config) config.Config {
	if configuration.Provider.Model == "" {
		configuration.Provider.Model = "test-model"
	}
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	return configuration
}
func TestSliceARealHTTPProviderSQLiteAndReadTools(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("request = %s %s", request.Method, request.URL.Path)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read_file\",\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]}}]}\n\n"))
		} else {
			_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"The README explains the project.\"}}]}\n\n"))
		}
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("Cairn is an event-sourced coding agent.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.BaseURL = server.URL + "/v1"
	configuration.Provider.Model = "test-model"
	stateDir := t.TempDir()
	result, err := (app.Service{}).Run(context.Background(), app.RunOptions{Task: "Explain the README", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	if result.Response != "The README explains the project." || requests.Load() != 2 {
		t.Fatalf("result = %#v, requests = %d", result, requests.Load())
	}
	inspected, err := (app.Service{}).Inspect(context.Background(), workspace, stateDir, configuration, result.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if inspected.Projection.State != domain.RunCompleted || len(inspected.Events) < 10 {
		t.Fatalf("inspect = %#v", inspected)
	}
}
