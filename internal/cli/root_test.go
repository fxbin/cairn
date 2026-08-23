package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// withTestProvider 为 CLI 测试配置补齐 V1.4 能力卡所需的模型与显式声明。
func withTestProvider(configuration config.Config) config.Config {
	if configuration.Provider.Model == "" {
		configuration.Provider.Model = "test-model"
	}
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	return configuration
}

type fakeProvider struct{}

func TestExitForUnconfirmedOperationRequiresReconciliation(t *testing.T) {
	err := &agent.WaitingReconcileError{OperationID: domain.OperationID("op-test"), Reason: "unconfirmed"}
	if exit := exitFor(err); exit != ExitReconcile {
		t.Fatalf("exit = %d, want %d", exit, ExitReconcile)
	}
}

func (fakeProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "answer"}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done})
}

func TestRunWritesOnlyNDJSONToStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}, Stdout: &stdout, Stderr: &stderr}
	exitCode := command.Run(context.Background(), []string{"run", "answer this", "--workspace", t.TempDir(), "--state-dir", t.TempDir(), "--model", "glm-5.2", "--json"})
	if exitCode != ExitOK {
		t.Fatalf("exit = %d, stderr = %s", exitCode, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	for _, line := range lines {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("non-NDJSON line %q: %v", line, err)
		}
	}
}

func TestHelpMatchesSupportedCommandsAndExitContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"help"}); exit != ExitOK {
		t.Fatalf("help exit=%d", exit)
	}
	usage := stderr.String()
	for _, required := range []string{"cairn run", "--max-tool-calls", "--max-input-tokens", "--max-output-tokens", "--max-wall-time", "--max-cost-usd", "cairn resume", "--note <text>", "--evidence <ref>", "--abort-run --reason", "cairn inspect", "cairn doctor", "cairn benchmark run", "--local-config <path>", "--prepare-timeout", "--prepare-proxy", "0 completed", "8 state corrupt"} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omitted %q: %s", required, usage)
		}
	}
}

func TestBenchmarkRunRequiresExplicitLocalConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"benchmark", "run", "--prepare-only"}); exit != ExitUsage || !strings.Contains(stderr.String(), "requires --local-config") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
}

func TestResolveRequiresAuditableCLIArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"resume", "--workspace", t.TempDir(), "--state-dir", t.TempDir(), "--resolve", "op-1=not_applied", "run-1"})
	if exit != ExitUsage || !strings.Contains(stderr.String(), "--resolve requires both --note and --evidence") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
}

func TestAbortRunRequiresReason(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"resume", "run_test", "--abort-run"}, &stdout, &stderr)
	if code != ExitUsage || !strings.Contains(stderr.String(), "--abort-run requires --reason") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// TestDoctorWritesOnlyNDJSONToStdout 锁定 doctor --json 的 stdout 纯 NDJSON 契约
// （AB-4：人类视图与机器视图共享同一事实，错误只走 stderr）。
func TestDoctorWritesOnlyNDJSONToStdout(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	// doctor 只读校验既有 journal；先用文本型 fakeProvider 跑一次 run 建立日志。
	var runStdout, runStderr bytes.Buffer
	runCommand := Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}, Stdout: &runStdout, Stderr: &runStderr}
	if exit := runCommand.Run(context.Background(), []string{"run", "seed journal", "--workspace", workspace, "--state-dir", stateDir, "--model", "glm-5.2", "--json"}); exit != ExitOK {
		t.Fatalf("seed run exit = %d, stderr = %s", exit, runStderr.String())
	}
	var output bytes.Buffer
	command := Command{Stdout: &output, Stderr: os.Stderr}
	code := command.Run(context.Background(), []string{"doctor", "--workspace", workspace, "--state-dir", stateDir, "--config", config.DefaultPath(workspace), "--json"})
	if code != 0 {
		t.Fatalf("doctor exit = %d, stderr shown above", code)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 1 || !json.Valid([]byte(lines[0])) {
		t.Fatalf("stdout must be a single NDJSON object, got %q", output.String())
	}
	var report map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &report); err != nil {
		t.Fatalf("decode doctor report: %v", err)
	}
	if _, exists := report["provider"]; !exists {
		t.Fatalf("doctor report missing provider section: %v", report)
	}
	if _, exists := report["build"]; !exists {
		t.Fatalf("doctor report missing build identity: %v", report)
	}
}

// TestHumanDiagnosticsExposeTheSameOperationalFacts 锁定人类视图不会丢失 JSON
// 中的关键诊断事实，且只显示密钥环境变量名而不显示其值。
func TestHumanDiagnosticsExposeTheSameOperationalFacts(t *testing.T) {
	t.Setenv("CAIRN_CLI_DIAGNOSTIC_KEY", "cli-secret-sentinel-value")
	workspace, stateDir := t.TempDir(), t.TempDir()
	configPath := config.DefaultPath(workspace)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("provider:\n  base_url: https://provider.test/v1\n  model: test-model\n  api_key_env: CAIRN_CLI_DIAGNOSTIC_KEY\n  capability:\n    tool_calls: true\n    streaming_usage: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var doctorOut, doctorErr bytes.Buffer
	command := Command{Stdout: &doctorOut, Stderr: &doctorErr}
	if code := command.Run(context.Background(), []string{"doctor", "--workspace", workspace, "--state-dir", stateDir, "--config", configPath}); code != ExitOK {
		t.Fatalf("doctor exit=%d stderr=%q", code, doctorErr.String())
	}
	for _, required := range []string{"ready:", "preset", "profile", "config fingerprint:", "build: revision=", "workspace:", "policy:", "journal:"} {
		if !strings.Contains(doctorOut.String(), required) {
			t.Fatalf("doctor human output missing %q: %s", required, doctorOut.String())
		}
	}
	if strings.Contains(doctorOut.String(), "cli-secret-sentinel-value") {
		t.Fatalf("doctor human output leaked credential value: %s", doctorOut.String())
	}
}

// TestHumanInspectExposesReplayedRunIdentity 锁定 inspect 的人类视图从同一回放
// 摘要呈现 Run 身份、工作区观察与验证进展，而不依赖重新执行模型或 verifier。
func TestHumanInspectExposesReplayedRunIdentity(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}
	result, err := service.Run(context.Background(), app.RunOptions{Task: "inspect identity", Workspace: workspace, StateDir: stateDir, Config: withTestProvider(config.Defaults())})
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Service: service, Stdout: &stdout, Stderr: &stderr}
	if code := command.Run(context.Background(), []string{"inspect", "--workspace", workspace, "--state-dir", stateDir, string(result.ID)}); code != ExitOK {
		t.Fatalf("inspect exit=%d stderr=%q", code, stderr.String())
	}
	for _, required := range []string{"config fingerprint:", "build: revision=", "task: freeform (cli) prompt=sha256:", "provider: openai_compatible", "preset: coding profile", "policy: write=", "workspace:", "workspace observation:", "changes:", "last verification:"} {
		if !strings.Contains(stdout.String(), required) {
			t.Fatalf("inspect human output missing %q: %s", required, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "inspect identity") {
		t.Fatalf("inspect human output leaked task prompt: %s", stdout.String())
	}
}
