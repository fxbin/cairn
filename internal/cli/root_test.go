package cli

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// withTestProvider 为 CLI 测试配置补齐能力卡所需的模型与显式声明。
func withTestProvider(configuration config.Config) config.Config {
	if configuration.Provider.Model == "" {
		configuration.Provider.Model = "test-model"
	}
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	return configuration
}

// writeCapabilityConfig 在测试工作区写入仅含能力声明的缺省配置，使 CLI 的
// run 装配满足"能力卡只由显式配置产出"的失败关闭契约。
func writeCapabilityConfig(t *testing.T, workspace string) {
	t.Helper()
	path := config.DefaultPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "version: 1\nprovider:\n  capability:\n    tool_calls: true\n    streaming_usage: true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
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
	if err := emit(ports.ModelEvent{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done, StopReason: "stop"})
}

func TestRunWritesOnlyNDJSONToStdout(t *testing.T) {
	workspace := t.TempDir()
	writeCapabilityConfig(t, workspace)
	var stdout, stderr bytes.Buffer
	command := Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}, Stdout: &stdout, Stderr: &stderr}
	exitCode := command.Run(context.Background(), []string{"run", "answer this", "--workspace", workspace, "--state-dir", t.TempDir(), "--model", "glm-5.2", "--json"})
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
	for _, required := range []string{"cairn run", "--max-tool-calls", "--max-input-tokens", "--max-output-tokens", "--max-wall-time", "--max-cost-usd", "cairn resume", "--note <text>", "--evidence <ref>", "--abort-run --reason", "cairn inspect", "cairn evidence", "cairn doctor", "cairn memory capture", "memory revoke", "cairn benchmark run", "--local-config <path>", "--prepare-timeout", "--prepare-proxy", "0 completed", "8 state corrupt"} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omitted %q: %s", required, usage)
		}
	}
}

// TestMemoryCommandsUseOnlyNDJSONAndDoNotLeakTaskText 锁定四个 memory 命令的
// 公共 CLI 契约：capture 的来源是 Run ID，所有 JSON 输出均为脱敏结构化投影。
func TestMemoryCommandsUseOnlyNDJSONAndDoNotLeakTaskText(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}
	var runOutput, runErrors bytes.Buffer
	runCommand := Command{Service: service, Stdout: &runOutput, Stderr: &runErrors}
	if exit := runCommand.Run(context.Background(), []string{"run", "cli-memory-secret-task-sentinel", "--workspace", workspace, "--state-dir", stateDir, "--model", "glm-5.2", "--json"}); exit != ExitOK {
		t.Fatalf("seed run exit=%d stderr=%s", exit, runErrors.String())
	}
	var runResult struct {
		RunID string `json:"run_id"`
	}
	lines := strings.Split(strings.TrimSpace(runOutput.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &runResult); err != nil || runResult.RunID == "" {
		t.Fatalf("seed run result=%q err=%v", lines[len(lines)-1], err)
	}
	var captureOutput, captureErrors bytes.Buffer
	captureCommand := Command{Service: service, Stdout: &captureOutput, Stderr: &captureErrors}
	if exit := captureCommand.Run(context.Background(), []string{"memory", "capture", "--run", runResult.RunID, "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitOK {
		t.Fatalf("capture exit=%d stderr=%s", exit, captureErrors.String())
	}
	if strings.Contains(captureOutput.String(), "cli-memory-secret-task-sentinel") {
		t.Fatalf("capture output leaked task: %s", captureOutput.String())
	}
	var captured struct {
		Type      string `json:"type"`
		Candidate struct {
			ID string `json:"id"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(captureOutput.String())), &captured); err != nil || captured.Type != "memory_capture" || captured.Candidate.ID == "" {
		t.Fatalf("capture output=%q err=%v", captureOutput.String(), err)
	}
	var recallOutput, recallErrors bytes.Buffer
	recallCommand := Command{Service: service, Stdout: &recallOutput, Stderr: &recallErrors}
	if exit := recallCommand.Run(context.Background(), []string{"memory", "recall", "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitOK || !json.Valid([]byte(strings.TrimSpace(recallOutput.String()))) {
		t.Fatalf("recall exit=%d stdout=%q stderr=%s", exit, recallOutput.String(), recallErrors.String())
	}
	var inspectOutput, inspectErrors bytes.Buffer
	inspectCommand := Command{Service: service, Stdout: &inspectOutput, Stderr: &inspectErrors}
	if exit := inspectCommand.Run(context.Background(), []string{"memory", "inspect", captured.Candidate.ID, "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitOK || !json.Valid([]byte(strings.TrimSpace(inspectOutput.String()))) {
		t.Fatalf("inspect exit=%d stdout=%q stderr=%s", exit, inspectOutput.String(), inspectErrors.String())
	}
	var revokeOutput, revokeErrors bytes.Buffer
	revokeCommand := Command{Service: service, Stdout: &revokeOutput, Stderr: &revokeErrors}
	if exit := revokeCommand.Run(context.Background(), []string{"memory", "revoke", captured.Candidate.ID, "--reason", "human_request", "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitOK || !json.Valid([]byte(strings.TrimSpace(revokeOutput.String()))) {
		t.Fatalf("revoke exit=%d stdout=%q stderr=%s", exit, revokeOutput.String(), revokeErrors.String())
	}
}

// TestMemoryRecallReturnsCorruptExitForTamperedSource 锁定 CLI 不能把来源事件
// 损坏降级为空 recall 或普通失败；只读证据校验必须映射到稳定的 ExitCorrupt。
func TestMemoryRecallReturnsCorruptExitForTamperedSource(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}
	var runOutput, runErrors bytes.Buffer
	runCommand := Command{Service: service, Stdout: &runOutput, Stderr: &runErrors}
	if exit := runCommand.Run(context.Background(), []string{"run", "corrupt recall source", "--workspace", workspace, "--state-dir", stateDir, "--model", "glm-5.2", "--json"}); exit != ExitOK {
		t.Fatalf("seed run exit=%d stderr=%s", exit, runErrors.String())
	}
	var seeded struct {
		RunID string `json:"run_id"`
	}
	lines := strings.Split(strings.TrimSpace(runOutput.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &seeded); err != nil || seeded.RunID == "" {
		t.Fatalf("seed run=%q err=%v", lines[len(lines)-1], err)
	}
	var captureOutput, captureErrors bytes.Buffer
	captureCommand := Command{Service: service, Stdout: &captureOutput, Stderr: &captureErrors}
	if exit := captureCommand.Run(context.Background(), []string{"memory", "capture", "--run", seeded.RunID, "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitOK {
		t.Fatalf("capture exit=%d stderr=%s", exit, captureErrors.String())
	}
	workspaceEntries, err := os.ReadDir(filepath.Join(stateDir, "workspaces"))
	if err != nil || len(workspaceEntries) != 1 {
		t.Fatalf("memory workspace entries=%v err=%v", workspaceEntries, err)
	}
	database, err := sql.Open("sqlite", filepath.Join(stateDir, "workspaces", workspaceEntries[0].Name(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE events SET event_hash = ? WHERE run_id = ? AND seq = 1", "0000000000000000000000000000000000000000000000000000000000000000", seeded.RunID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	var recallOutput, recallErrors bytes.Buffer
	recallCommand := Command{Service: service, Stdout: &recallOutput, Stderr: &recallErrors}
	if exit := recallCommand.Run(context.Background(), []string{"memory", "recall", "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitCorrupt {
		t.Fatalf("recall exit=%d stdout=%q stderr=%s", exit, recallOutput.String(), recallErrors.String())
	}
	if strings.TrimSpace(recallOutput.String()) != "" {
		t.Fatalf("corrupt recall wrote candidate output: %q", recallOutput.String())
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
	writeCapabilityConfig(t, workspace)
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

// TestEvidenceCommandProjectsUnifiedResult 锁定 V1.5 统一证据视图：evidence
// 命令的 NDJSON 与人类视图来自同一 contract.Result 投影，内建目录模型可回放
// Provider 能力卡，输出不含任务正文。
func TestEvidenceCommandProjectsUnifiedResult(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}
	var runStdout bytes.Buffer
	runCommand := Command{Service: service, Stdout: &runStdout, Stderr: os.Stderr}
	if exit := runCommand.Run(context.Background(), []string{"run", "evidence probe", "--workspace", workspace, "--state-dir", stateDir, "--model", "glm-5.2", "--json"}); exit != ExitOK {
		t.Fatalf("seed run exit=%d stdout=%s", exit, runStdout.String())
	}
	var seeded struct {
		RunID string `json:"run_id"`
	}
	lines := strings.Split(strings.TrimSpace(runStdout.String()), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &seeded); err != nil || seeded.RunID == "" {
		t.Fatalf("seed result line=%s err=%v", lines[len(lines)-1], err)
	}

	var jsonOut bytes.Buffer
	jsonCommand := Command{Service: service, Stdout: &jsonOut, Stderr: os.Stderr}
	if exit := jsonCommand.Run(context.Background(), []string{"evidence", "--workspace", workspace, "--state-dir", stateDir, "--json", seeded.RunID}); exit != ExitOK {
		t.Fatalf("evidence json exit=%d", exit)
	}
	var projected contract.Result
	if err := json.Unmarshal([]byte(strings.TrimSpace(jsonOut.String())), &projected); err != nil {
		t.Fatalf("evidence json=%q err=%v", jsonOut.String(), err)
	}
	if projected.Manifest.RunID != domain.RunID(seeded.RunID) || projected.State != "completed" || !projected.Evidence.Trusted {
		t.Fatalf("projected=%#v", projected)
	}
	card := projected.Manifest.Provider.Card
	if card == nil || card.Source != contract.ProviderCardSourceConfig || card.Model != "glm-5.2" || !card.ToolCalls || !card.StreamingUsage {
		t.Fatalf("declared provider card = %#v", card)
	}
	if strings.Contains(jsonOut.String(), "evidence probe") {
		t.Fatalf("evidence leaked task prompt: %s", jsonOut.String())
	}

	var humanOut bytes.Buffer
	humanCommand := Command{Service: service, Stdout: &humanOut, Stderr: os.Stderr}
	if exit := humanCommand.Run(context.Background(), []string{"evidence", "--workspace", workspace, "--state-dir", stateDir, seeded.RunID}); exit != ExitOK {
		t.Fatalf("evidence human exit=%d", exit)
	}
	for _, required := range []string{"task: freeform (cli) prompt=sha256:", "outcome: completed", "completion evidence:", "metrics: turns"} {
		if !strings.Contains(humanOut.String(), required) {
			t.Fatalf("evidence human output missing %q: %s", required, humanOut.String())
		}
	}
	if strings.Contains(humanOut.String(), "evidence probe") {
		t.Fatalf("evidence human view leaked task prompt: %s", humanOut.String())
	}
}
