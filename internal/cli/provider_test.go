package cli

import (
	"bytes"

	"context"
	"encoding/json"
	"github.com/fxbin/cairn/internal/config"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/provider"
)

// offlineCatalogFlags 返回指向立即拒绝端点的目录旗标，保证向导语义离线
// 降级而不等待网络超时。
func offlineCatalogFlags() []string {
	return []string{"--catalog-url", "http://127.0.0.1:1", "--catalog-cache", filepath.Join(os.TempDir(), "cairn-test-no-catalog.json")}
}

// TestProviderListOutputsRegistry 锁定注册表展示：闭合成员经 NDJSON 输出。
func TestProviderListOutputsRegistry(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"provider", "list", "--json"}); exit != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", exit, stderr.String())
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["provider_id"] != "custom" || entry["protocol_id"] != "openai-chat-completions" {
		t.Fatalf("registry entry = %v", entry)
	}
}

// TestProviderConfigureGeneratesVersion2Configs 锁定配置生成：默认生成 v2
// required；显式 optional 仍保持 v2，旧 --provider-id 参数不再受理。
func TestProviderConfigureGeneratesVersion2Configs(t *testing.T) {
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	arguments := append([]string{"provider", "configure", "--base-url", "https://api.example.com/v1", "--model", "cfg-model", "--api-key-env", "AGENT_API_KEY", "--workspace", workspace}, offlineCatalogFlags()...)
	if exit := command.Run(context.Background(), arguments); exit != ExitOK {
		t.Fatalf("default exit = %d, stderr = %s", exit, stderr.String())
	}
	body, err := os.ReadFile(filepath.Join(workspace, ".agent", "config.yaml"))
	if err != nil || !strings.Contains(string(body), "version: 2") || !strings.Contains(string(body), "admission: required") || !strings.Contains(string(body), "id: custom") {
		t.Fatalf("default v2 config missing identity fields: %v %s", err, string(body))
	}
	optionalWorkspace := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	arguments = append([]string{"provider", "configure", "--base-url", "https://api.example.com/v1", "--model", "cfg-model", "--api-key-env", "AGENT_API_KEY", "--admission", "optional", "--workspace", optionalWorkspace}, offlineCatalogFlags()...)
	if exit := command.Run(context.Background(), arguments); exit != ExitOK {
		t.Fatalf("optional exit = %d, stderr = %s", exit, stderr.String())
	}
	body, err = os.ReadFile(filepath.Join(optionalWorkspace, ".agent", "config.yaml"))
	if err != nil || !strings.Contains(string(body), "version: 2") || !strings.Contains(string(body), "admission: optional") {
		t.Fatalf("optional v2 config missing: %v %s", err, string(body))
	}
	stderr.Reset()
	arguments = append([]string{"provider", "configure", "--base-url", "https://api.example.com/v1", "--model", "cfg-model", "--api-key-env", "AGENT_API_KEY", "--provider-id", "custom", "--workspace", t.TempDir()}, offlineCatalogFlags()...)
	if exit := command.Run(context.Background(), arguments); exit != ExitUsage || !strings.Contains(stderr.String(), "flag provided but not defined") {
		t.Fatalf("removed --provider-id must be rejected: exit=%d stderr=%q", exit, stderr.String())
	}
}

// TestProviderAdmitStatusClosedLoop 锁定准入闭环：在档证据 → provider admit
// 记录 admitted 决定 → provider status 展示链头且 required 门放行。
func TestProviderAdmitStatusClosedLoop(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	configPath := filepath.Join(workspace, ".agent", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "version: 2\nprovider:\n  base_url: https://api.example.com/v1\n  model: loop-model\n  api_key_env: LOOP_KEY\n  admission: required\n  capability:\n    tool_calls: true\n    streaming_usage: true\n    stop_reasons: [stop]\n"
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	workspaceHash, err := cliWorkspaceHash(workspace, loadConfigOrDie(t, configPath))
	if err != nil {
		t.Fatal(err)
	}
	evidenceStore, err := provider.OpenEvidenceStore(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := provider.ResolveSelection(provider.SelectionFacts{Model: "loop-model", BaseURL: "https://api.example.com/v1"})
	if err != nil {
		t.Fatal(err)
	}
	evidence := provider.ProviderProbeEvidence{
		SchemaVersion: 1, Selection: selection, ProbeContractVersion: provider.CurrentProbeContractVersion,
		Observations: provider.EvidenceObservations{
			UsageStreamed: provider.ObservationObservedTrue, StopReasonsState: provider.StateObserved, StopReasons: []string{"stop"},
			ToolRoundtrip: provider.RoundtripSkipped, ModelRevisionState: provider.StateNotObserved,
		},
		Outcome: provider.EvidenceOutcomeCompleted, ProbedAt: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
	}
	evidenceFingerprint, err := evidenceStore.Append(&evidence)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"provider", "status", "--workspace", workspace, "--config", configPath, "--state-dir", stateDir, "--json"})
	if exit != ExitFailed {
		t.Fatalf("unadmitted required binding must exit failed, got %d", exit)
	}
	if !strings.Contains(stdout.String(), "未准入") && !strings.Contains(stdout.String(), "allowed\":false") {
		t.Fatalf("status must report unadmitted: %s", stdout.String())
	}
	stdout.Reset()
	exit = command.Run(context.Background(), []string{"provider", "admit", "--evidence", evidenceFingerprint, "--decision", "admitted", "--reason", "probe accepted", "--workspace", workspace, "--config", configPath, "--state-dir", stateDir})
	if exit != ExitOK {
		t.Fatalf("admit exit = %d, stderr = %s", exit, stderr.String())
	}
	stdout.Reset()
	exit = command.Run(context.Background(), []string{"provider", "status", "--workspace", workspace, "--config", configPath, "--state-dir", stateDir, "--json"})
	if exit != ExitOK {
		t.Fatalf("admitted status exit = %d, stderr = %s stdout = %s", exit, stderr.String(), stdout.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout.String())), &report); err != nil {
		t.Fatal(err)
	}
	if report["allowed"] != true || report["head_decision"] != "admitted" {
		t.Fatalf("closed loop status = %v", report)
	}
}

// loadConfigOrDie 为测试装载配置；装载失败即测试失败。
func loadConfigOrDie(t *testing.T, configPath string) config.Config {
	t.Helper()
	configuration, err := config.Load(configPath, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	return configuration
}

// TestProviderPresetSaveRemoveClosedLoop 锁定命名预设闭环：
// presets save 写入；同名拒绝；overwrite 允许替换；remove 删除；
// 激活中的预设拒绝删除。
func TestProviderPresetSaveRemoveClosedLoop(t *testing.T) {
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	save := []string{"provider", "presets", "save", "fast", "--base-url", "https://api.example.com/v1", "--model", "m1", "--api-key-env", "AGENT_API_KEY", "--workspace", workspace}
	if exit := command.Run(context.Background(), save); exit != ExitOK {
		t.Fatalf("save exit = %d, stderr = %s", exit, stderr.String())
	}
	body, err := os.ReadFile(filepath.Join(workspace, ".agent", "config.yaml"))
	if err != nil || !strings.Contains(string(body), "fast:") {
		t.Fatalf("preset missing: %v %s", err, string(body))
	}

	stderr.Reset()
	if exit := command.Run(context.Background(), save); exit != ExitFailed || !strings.Contains(stderr.String(), "already exists") {
		t.Fatalf("duplicate save must fail: exit=%d stderr=%q", exit, stderr.String())
	}

	stderr.Reset()
	overwrite := append(append([]string{}, save...), "--overwrite")
	if exit := command.Run(context.Background(), overwrite); exit != ExitOK {
		t.Fatalf("overwrite exit = %d, stderr = %s", exit, stderr.String())
	}

	stdout.Reset()
	if exit := command.Run(context.Background(), []string{"provider", "presets", "--workspace", workspace}); exit != ExitOK || !strings.Contains(stdout.String(), "fast") {
		t.Fatalf("list: exit=%d out=%q", exit, stdout.String())
	}

	// 激活后拒绝删除。
	stderr.Reset()
	if exit := command.Run(context.Background(), []string{"provider", "use", "fast", "--workspace", workspace}); exit != ExitOK {
		t.Fatalf("use exit = %d, stderr = %s", exit, stderr.String())
	}
	stderr.Reset()
	if exit := command.Run(context.Background(), []string{"provider", "presets", "remove", "fast", "--workspace", workspace}); exit != ExitFailed || !strings.Contains(stderr.String(), "active") {
		t.Fatalf("remove active must fail: exit=%d stderr=%q", exit, stderr.String())
	}

	// 删除未激活预设。
	stderr.Reset()
	if exit := command.Run(context.Background(), []string{"provider", "presets", "save", "slow", "--base-url", "https://api.example.com/v1", "--model", "m2", "--api-key-env", "AGENT_API_KEY", "--workspace", workspace}); exit != ExitOK {
		t.Fatalf("save slow exit = %d, stderr = %s", exit, stderr.String())
	}
	stderr.Reset()
	if exit := command.Run(context.Background(), []string{"provider", "presets", "remove", "slow", "--workspace", workspace}); exit != ExitOK {
		t.Fatalf("remove slow exit = %d, stderr = %s", exit, stderr.String())
	}
	body, _ = os.ReadFile(filepath.Join(workspace, ".agent", "config.yaml"))
	if strings.Contains(string(body), "slow:") {
		t.Fatalf("slow preset should be gone: %s", string(body))
	}
}

// TestProviderConfigureSavePreset 锁定 configure --save-preset 在新建配置时写入预设。
func TestProviderConfigureSavePreset(t *testing.T) {
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	args := append([]string{"provider", "configure", "--base-url", "https://api.example.com/v1", "--model", "cfg-model", "--api-key-env", "AGENT_API_KEY", "--save-preset", "default", "--workspace", workspace}, offlineCatalogFlags()...)
	if exit := command.Run(context.Background(), args); exit != ExitOK {
		t.Fatalf("configure exit = %d, stderr = %s", exit, stderr.String())
	}
	body, err := os.ReadFile(filepath.Join(workspace, ".agent", "config.yaml"))
	if err != nil || !strings.Contains(string(body), "default:") || !strings.Contains(string(body), "cfg-model") {
		t.Fatalf("preset not saved via configure: %v %s", err, string(body))
	}
}

// TestProviderAdmitStatusClosedLoop 锁定准入闭环：在档证据 → provider admit
// 记录 admitted 决定 → provider status 展示链头且 required 门放行。

// TestProviderModelsFilters 锁定模型目录筛选：tool-call/reasoning 仅保留支持项。
func TestProviderModelsFilters(t *testing.T) {
	catalog := `{"x":{"models":{"a":{"name":"A","tool_call":true,"reasoning":false,"limit":{"context":1000,"output":100},"cost":{"input":1,"output":2}},"b":{"name":"B","tool_call":false,"reasoning":true,"limit":{"context":2000,"output":200},"cost":{"input":3,"output":4}}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(catalog))
	}))
	defer server.Close()
	cache := filepath.Join(t.TempDir(), "catalog.json")
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	args := []string{"provider", "models", "--catalog-url", server.URL, "--catalog-cache", cache, "--tool-call", "--json"}
	if exit := command.Run(context.Background(), args); exit != ExitOK {
		t.Fatalf("tool-call filter exit = %d stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "A") || strings.Contains(stdout.String(), `"name":"B"`) {
		t.Fatalf("tool-call filter wrong: %s", stdout.String())
	}
	stdout.Reset()
	args = []string{"provider", "models", "--catalog-url", server.URL, "--catalog-cache", cache, "--reasoning", "--json"}
	if exit := command.Run(context.Background(), args); exit != ExitOK {
		t.Fatalf("reasoning filter exit = %d stderr=%s", exit, stderr.String())
	}
	if !strings.Contains(stdout.String(), "B") {
		t.Fatalf("reasoning filter wrong: %s", stdout.String())
	}
}
