package cli

import (
	"bytes"

	"context"
	"encoding/json"
	"github.com/fxbin/cairn/internal/config"
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

// TestProviderConfigureGeneratesVersionedConfigs 锁定配置生成：默认 version 1；
// 给出 --admission 生成 version 2 且 id 缺省归一 custom、文件可严格装载。
func TestProviderConfigureGeneratesVersionedConfigs(t *testing.T) {
	workspace := t.TempDir()
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	arguments := append([]string{"provider", "configure", "--base-url", "https://api.example.com/v1", "--model", "cfg-model", "--api-key-env", "AGENT_API_KEY", "--workspace", workspace}, offlineCatalogFlags()...)
	if exit := command.Run(context.Background(), arguments); exit != ExitOK {
		t.Fatalf("v1 exit = %d, stderr = %s", exit, stderr.String())
	}
	body, err := os.ReadFile(filepath.Join(workspace, ".agent", "config.yaml"))
	if err != nil || !strings.Contains(string(body), "version: 1") {
		t.Fatalf("v1 config missing: %v", err)
	}
	v2Workspace := t.TempDir()
	stdout.Reset()
	arguments = append([]string{"provider", "configure", "--base-url", "https://api.example.com/v1", "--model", "cfg-model", "--api-key-env", "AGENT_API_KEY", "--admission", "required", "--workspace", v2Workspace}, offlineCatalogFlags()...)
	if exit := command.Run(context.Background(), arguments); exit != ExitOK {
		t.Fatalf("v2 exit = %d, stderr = %s", exit, stderr.String())
	}
	body, err = os.ReadFile(filepath.Join(v2Workspace, ".agent", "config.yaml"))
	if err != nil || !strings.Contains(string(body), "version: 2") || !strings.Contains(string(body), "admission: required") || !strings.Contains(string(body), "id: custom") {
		t.Fatalf("v2 config missing identity fields: %v %s", err, string(body))
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
