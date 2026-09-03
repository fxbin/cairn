package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/provider"
	"github.com/fxbin/cairn/internal/tools"
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
	body := "provider:\n  base_url: " + baseURL + "\n  model: probe-model\n  api_key_env: CAIRN_PROBE_KEY\n  capability:\n    tool_calls: true\n    streaming_usage: true\n    stop_reasons: [stop, length, tool_calls]\n  generation:\n    temperature_milli: 600\n"
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

// probeToolCallFrame 输出第一程的 SSE 帧：模型对探测工具发起一次完整调用。
func probeToolCallFrame(writer http.ResponseWriter) {
	writer.Header().Set("Content-Type", "text/event-stream")
	_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"contract_probe\",\"arguments\":\"{\\\"value\\\":7}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n"))
	_, _ = writer.Write([]byte("data: [DONE]\n\n"))
}

// TestCheckProviderProbeToolsVerifiesSecondLeg 验证两程工具探测：第一程收到
// 工具调用后，探测必须回喂 assistant(tool_calls) 与 tool result 消息发起第
// 二程，并以第二程的最终回答判定 ok；单程收到工具调用不再构成 ok。
func TestCheckProviderProbeToolsVerifiesSecondLeg(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	requestCount := 0
	var secondLegMessages []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var payload struct {
			Messages []map[string]any `json:"messages"`
		}
		_ = json.Unmarshal(body, &payload)
		requestCount++
		if requestCount == 1 {
			probeToolCallFrame(writer)
			return
		}
		secondLegMessages = payload.Messages
		sseFrame(writer, "stop")
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--json", "--probe-tools"})
	if exit != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	if report["tool_roundtrip"] != "ok" || report["result"] != "pass" {
		t.Fatalf("two-leg probe mismatch: %v", report)
	}
	if len(secondLegMessages) < 3 {
		t.Fatalf("second leg must replay user, assistant and tool messages: %v", secondLegMessages)
	}
	tool := secondLegMessages[len(secondLegMessages)-1]
	if tool["role"] != "tool" || tool["tool_call_id"] != "call_1" {
		t.Fatalf("second leg must feed back the tool result keyed by call id: %v", tool)
	}
	assistant := secondLegMessages[len(secondLegMessages)-2]
	if assistant["role"] != "assistant" {
		t.Fatalf("second leg must carry the assistant tool_calls turn: %v", assistant)
	}
}

// TestCheckProviderProbeToolsFailsOnSecondLegFailure 验证第二程失败路径：第
// 一程成功而第二程被响应合同拒绝时，探测必须报告 second_leg_failed 并以失
// 败退出，不得把单程工具调用冒充为 ok。
func TestCheckProviderProbeToolsFailsOnSecondLegFailure(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		if requestCount == 1 {
			probeToolCallFrame(writer)
			return
		}
		sseFrame(writer, "weird_stop")
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--json", "--probe-tools"})
	if exit != ExitFailed {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	if report["tool_roundtrip"] != "second_leg_failed" || report["result"] != "fail" {
		t.Fatalf("second leg failure mismatch: %v", report)
	}
}

// TestCheckProviderProbeToolsFailsWhenSecondLegCallsToolAgain 验证第二程再
// 次请求工具不是 ok：模型先输出文本、随后再次调用工具并以 tool_calls 终止
// 时，回路未完成，必须报告 second_leg_failed 并以失败退出。
func TestCheckProviderProbeToolsFailsWhenSecondLegCallsToolAgain(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		writer.Header().Set("Content-Type", "text/event-stream")
		if requestCount == 1 {
			probeToolCallFrame(writer)
			return
		}
		// 第二程：先给文本，再发起一次新的工具调用并以 tool_calls 终止。
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"working\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_2\",\"function\":{\"name\":\"contract_probe\",\"arguments\":\"{\\\"value\\\":8}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--json", "--probe-tools"})
	if exit != ExitFailed {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	if report["tool_roundtrip"] != "second_leg_failed" || report["result"] != "fail" {
		t.Fatalf("second leg must not pass while it requests tools again: %v", report)
	}
}

// TestCheckProviderProbeToolsFailsOnEmptyFirstLeg 验证第一程空响应：既无文
// 本也无工具调用的空输出是端点异常，必须以失败退出，而不是降级为
// inconclusive。
func TestCheckProviderProbeToolsFailsOnEmptyFirstLeg(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":0,\"total_tokens\":3}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--json", "--probe-tools"})
	if exit != ExitFailed {
		t.Fatalf("empty first leg must exit non-zero, got %d", exit)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	if report["result"] != "fail" {
		t.Fatalf("empty first leg must fail, got: %v", report)
	}
}

// TestCheckProviderProbeToolsInconclusiveWhenToolNotInvoked 验证模型输出了
// 普通文本但未按要求调用工具时的可判性结论：报告 not_invoked 并以
// inconclusive 退出，而不是通过。
func TestCheckProviderProbeToolsInconclusiveWhenToolNotInvoked(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sseFrame(writer, "stop")
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--json", "--probe-tools"})
	if exit != ExitFailed {
		t.Fatalf("inconclusive probe must exit non-zero, got %d", exit)
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	if report["tool_roundtrip"] != "not_invoked" || report["result"] != "inconclusive" {
		t.Fatalf("not-invoked probe mismatch: %v", report)
	}
}

// TestCheckProviderPersistsProbeEvidence 验证探测观测落盘：真实执行过网络
// 探测后，JSON 结论携带 evidence_fingerprint，且证据文件在
// provider/evidence/ 下可按指纹读回；早期配置失败则不落证据。
func TestCheckProviderPersistsProbeEvidence(t *testing.T) {
	t.Setenv("CAIRN_PROBE_KEY", "sk-probe-sentinel")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		sseFrame(writer, "stop")
	}))
	defer server.Close()
	configPath := writeProbeConfig(t, server.URL)
	workspace := t.TempDir()
	stateDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"check-provider", "--config", configPath, "--workspace", workspace, "--state-dir", stateDir, "--json"})
	if exit != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := report["evidence_fingerprint"].(string)
	if !strings.HasPrefix(fingerprint, "sha256:") {
		t.Fatalf("probe must report an evidence fingerprint: %v", report)
	}
	store, err := provider.OpenEvidenceStore(stateDir, probeWorkspaceHash(t, workspace))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.Get(fingerprint)
	if err != nil {
		t.Fatalf("evidence must be readable back by fingerprint: %v", err)
	}
	if evidence.Outcome != provider.EvidenceOutcomeCompleted || evidence.Observations.UsageStreamed != provider.ObservationObservedTrue {
		t.Fatalf("unexpected evidence observations: %+v", evidence.Observations)
	}
	if evidence.Selection.ProviderID != contract.ProviderIDCustom {
		t.Fatalf("evidence selection provider = %q", evidence.Selection.ProviderID)
	}
}

// probeWorkspaceHash 复刻 app 装配的工作区哈希算法，供测试定位证据分区。
func probeWorkspaceHash(t *testing.T, workspace string) string {
	t.Helper()
	view, err := tools.NewWorkspace(workspace, nil, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(view.Root))
	return "sha256:" + hex.EncodeToString(digest[:])
}
