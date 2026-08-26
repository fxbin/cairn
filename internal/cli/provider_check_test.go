package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// sseFrame 输出一个带内容增量、可选 finish_reason 与 usage 的完整 SSE 帧。
func sseFrame(writer http.ResponseWriter, finish string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	frame := "{\"choices\":[{\"delta\":{\"content\":\"ok\"}"
	if finish != "" {
		frame += ",\"finish_reason\":\"" + finish + "\""
	}
	frame += "}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}"
	_, _ = writer.Write([]byte("data: " + frame + "\n\n"))
	_, _ = writer.Write([]byte("data: [DONE]\n\n"))
}

// writeProbeConfig 写入指向探测端点的最小工作区配置并返回状态目录无关路径。
func writeProbeConfig(t *testing.T, baseURL string) string {
	t.Helper()
	workspace := t.TempDir()
	path := filepath.Join(workspace, ".agent", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "provider:\n  base_url: " + baseURL + "\n  model: probe-model\n  api_key_env: CAIRN_PROBE_KEY\n  capability:\n    tool_calls: true\n    streaming_usage: true\n    stop_reasons: [stop, length]\n  generation:\n    temperature_milli: 600\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCheckProviderPassesAndReportsClosedContract 验证对合规端点的探测全绿：
// 退出码为零，NDJSON 单行报告携带身份摘要与两态生成参数摘要，且不泄露
// 明文端点地址或密钥环境变量的值。
func TestCheckProviderPassesAndReportsClosedContract(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if got := request.Header.Get("Authorization"); got != "Bearer sk-probe-sentinel" {
			t.Errorf("probe must authenticate via the configured key env")
		}
		sseFrame(writer, "stop")
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--json"})
	if exit != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	line := strings.TrimSpace(stdout.String())
	if !json.Valid([]byte(line)) || strings.Count(line, "\n") != 0 {
		t.Fatalf("stdout must be a single JSON line, got %q", line)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(line), &report); err != nil {
		t.Fatal(err)
	}
	if report["result"] != "pass" || report["usage_streamed"] != true || report["stop_reason"] != "stop" || report["tool_roundtrip"] != "skipped" {
		t.Fatalf("report mismatch: %v", report)
	}
	if identity, _ := report["endpoint_identity"].(string); !strings.HasPrefix(identity, "sha256:") {
		t.Fatalf("endpoint identity must be a digest, got %v", report["endpoint_identity"])
	}
	generation, _ := report["generation"].(map[string]any)
	_, hasTopP := generation["top_p_milli"]
	if generation == nil || generation["temperature_milli"] != float64(600) || hasTopP {
		t.Fatalf("generation summary mismatch: %v", generation)
	}
	if strings.Contains(line, server.URL) || strings.Contains(line, "http") || strings.Contains(line, "sk-probe-sentinel") {
		t.Fatal("report must not leak the plaintext endpoint or the key value")
	}
}

// TestCheckProviderFailsClosedOnUndeclaredStopReason 验证端点漂移探测失败：
// 未声明的 finish reason 使探测以失败退出并在报告中给出错误摘要。
func TestCheckProviderFailsClosedOnUndeclaredStopReason(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sseFrame(writer, "weird_stop")
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--json"})
	if exit != ExitFailed {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	if report["result"] != "fail" || report["error"] == "" {
		t.Fatalf("failure report mismatch: %v", report)
	}
}

// TestCheckProviderRequiresDeclaredCapability 验证能力卡缺失时探测直接产出
// 失败结论而不是启动网络调用。
func TestCheckProviderRequiresDeclaredCapability(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, ".agent", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "provider:\n  base_url: https://provider.test/v1\n  model: probe-model\n  api_key_env: CAIRN_PROBE_KEY\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", path, "--json"})
	if exit != ExitFailed {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	errorText, _ := report["error"].(string)
	if !strings.Contains(errorText, "capability") {
		t.Fatalf("expected a capability contract failure, got %v", report["error"])
	}
}

// TestCheckProviderRejectsPositionalArguments 锁定参数合同：位置参数属于用法
// 错误，防止把 run-id 等参数误传给探测命令。
func TestCheckProviderRejectsPositionalArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"check-provider", "extra"}); exit != ExitUsage {
		t.Fatalf("positional argument must be a usage error, got exit %d", exit)
	}
}
