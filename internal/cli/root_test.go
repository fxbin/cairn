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

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// withTestProvider 为 CLI 测试配置补齐能力卡所需的模型与显式声明。
func withTestProvider(configuration config.Config) config.Config {
	if configuration.Provider.BaseURL == "" {
		configuration.Provider.BaseURL = "https://provider.test/v1"
	}
	if configuration.Provider.Model == "" {
		configuration.Provider.Model = "test-model"
	}
	configuration.Provider.Admission = config.ProviderAdmissionOptional
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
	content := "version: 2\nprovider:\n  id: custom\n  admission: optional\n  base_url: https://provider.test/v1\n  model: test-model\n  capability:\n    tool_calls: true\n    streaming_usage: true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

type fakeProvider struct{}

func TestExitForUnconfirmedOperationRequiresReconciliation(t *testing.T) {
	err := &ports.WaitingReconcileError{OperationID: domain.OperationID("op-test"), Reason: "unconfirmed"}
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
			ID               string `json:"id"`
			TaskPromptDigest string `json:"task_prompt_digest"`
		} `json:"candidate"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(captureOutput.String())), &captured); err != nil || captured.Type != "memory_capture" || captured.Candidate.ID == "" || captured.Candidate.TaskPromptDigest == "" {
		t.Fatalf("capture output=%q err=%v", captureOutput.String(), err)
	}
	var explainOutput, explainErrors bytes.Buffer
	explainCommand := Command{Service: service, Stdout: &explainOutput, Stderr: &explainErrors}
	if exit := explainCommand.Run(context.Background(), []string{"memory", "explain", "--task-digest", captured.Candidate.TaskPromptDigest, "--precision", "trace", "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitOK || !json.Valid([]byte(strings.TrimSpace(explainOutput.String()))) || strings.Contains(explainOutput.String(), "cli-memory-secret-task-sentinel") {
		t.Fatalf("explain exit=%d stdout=%q stderr=%s", exit, explainOutput.String(), explainErrors.String())
	}
	var invalidOutput, invalidErrors bytes.Buffer
	invalidCommand := Command{Service: service, Stdout: &invalidOutput, Stderr: &invalidErrors}
	if exit := invalidCommand.Run(context.Background(), []string{"memory", "explain", "--failure-class", "unbounded", "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitUsage || strings.TrimSpace(invalidOutput.String()) != "" {
		t.Fatalf("invalid explain exit=%d stdout=%q stderr=%s", exit, invalidOutput.String(), invalidErrors.String())
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
	var explainOutput, explainErrors bytes.Buffer
	explainCommand := Command{Service: service, Stdout: &explainOutput, Stderr: &explainErrors}
	if exit := explainCommand.Run(context.Background(), []string{"memory", "explain", "--outcome", "failed", "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitCorrupt || !json.Valid([]byte(strings.TrimSpace(explainOutput.String()))) || !strings.Contains(explainOutput.String(), "source_invalid") {
		t.Fatalf("explain exit=%d stdout=%q stderr=%s", exit, explainOutput.String(), explainErrors.String())
	}
}

func TestBenchmarkRunRequiresExplicitLocalConfig(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"benchmark", "run", "--prepare-only"}); exit != ExitUsage || !strings.Contains(stderr.String(), "requires --local-config") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
}

// TestBenchmarkPrepareOnlyEnvironmentInvalidReturnsFailed 锁定 CLI 在环境资格失败时
// 仍输出完整报告，但不能把 prepare-only 请求误报为成功。
func TestBenchmarkPrepareOnlyEnvironmentInvalidReturnsFailed(t *testing.T) {
	assets := t.TempDir()
	manifestPath := filepath.Join(assets, "tasks.yaml")
	agentConfigPath := filepath.Join(assets, "agent.yaml")
	localConfigPath := filepath.Join(assets, "local.yaml")
	manifest := `version: 1
suite: cli-regression
intended_use: evaluation_only
task_count: 1
tasks:
  - id: CLI-001
    kind: natural
    repository:
      name: unavailable
      url: https://127.0.0.1:1/unavailable.git
      baseline_commit: 0123456789abcdef0123456789abcdef01234567
    language: go
    task_prompt: 修复测试任务
    scope:
      allowed_paths: [note.txt]
      prohibited_paths: [.git]
    verification:
      command:
        argv: ["true"]
        cwd: .
    runtime:
      docker_image: alpine:latest
execution_contract:
  attempts_per_task: 1
  agent_workspace: disposable_clone
  reset_between_attempts: true
  network_during_agent_run: denied
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentConfigPath, []byte("version: 2\nprovider:\n  id: custom\n  admission: optional\n  base_url: https://provider.test/v1\n  model: test-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	localConfig := "version: 1\npaths:\n" +
		"  manifest: " + manifestPath + "\n" +
		"  agent_config: " + agentConfigPath + "\n" +
		"  private_dir: " + filepath.Join(assets, "private") + "\n" +
		"  results_dir: " + filepath.Join(assets, "results") + "\n" +
		"  workspaces_dir: " + filepath.Join(assets, "workspaces") + "\n" +
		"  state_dir: " + filepath.Join(assets, "state") + "\n" +
		"execution:\n  prepare_timeout: 1ms\n"
	if err := os.WriteFile(localConfigPath, []byte(localConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"benchmark", "run", "--prepare-only", "--local-config", localConfigPath})
	if exit != ExitFailed {
		t.Fatalf("exit=%d want=%d stdout=%q stderr=%q", exit, ExitFailed, stdout.String(), stderr.String())
	}
	var report struct {
		Outcome string `json:"outcome"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &report); err != nil || report.Outcome != "environment_invalid" {
		t.Fatalf("report=%q outcome=%q err=%v", stdout.String(), report.Outcome, err)
	}
}

func TestResolveRequiresAuditableCLIArguments(t *testing.T) {
	workspace := t.TempDir()
	writeCapabilityConfig(t, workspace)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"resume", "--workspace", workspace, "--state-dir", t.TempDir(), "--resolve", "op-1=not_applied", "run-1"})
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
	if err := os.WriteFile(configPath, []byte("version: 2\nprovider:\n  base_url: https://provider.test/v1\n  model: test-model\n  api_key_env: CAIRN_CLI_DIAGNOSTIC_KEY\n  capability:\n    tool_calls: true\n    streaming_usage: true\n"), 0o600); err != nil {
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
	writeCapabilityConfig(t, workspace)
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
	// 零命令计划：completed 但不可信（ADR-0017）。
	if projected.Manifest.RunID != domain.RunID(seeded.RunID) || projected.State != "completed" || projected.Evidence.Trusted {
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

// TestCandidateEvaluateUsageRequiresFrozenAssets 锁定评测动词的用法合同：缺
// 少冻结资产或变体参数即拒绝并提示先创建候选，usage 词表包含 evaluate。
func TestCandidateEvaluateUsageRequiresFrozenAssets(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"memory", "candidate"}); exit != ExitUsage || !strings.Contains(stderr.String(), "evaluate") {
		t.Fatalf("candidate dispatch must mention evaluate: exit=%d stderr=%q", exit, stderr.String())
	}
	if exit := command.Run(context.Background(), []string{"memory", "candidate", "evaluate", "--candidate", "c-1"}); exit != ExitUsage || !strings.Contains(stderr.String(), "create the candidate first") {
		t.Fatalf("missing frozen assets must fail with creation hint: exit=%d stderr=%q", exit, stderr.String())
	}
}

// TestCandidateEvaluateFailsClosedForUnknownCandidate 验证对不存在候选的失败
// 关闭：完整提供本地资产后仍因候选缺失而退出，错误文本明确提示先用 create。
func TestCandidateEvaluateFailsClosedForUnknownCandidate(t *testing.T) {
	workspace := t.TempDir()
	writeCapabilityConfig(t, workspace)
	assets := t.TempDir()
	localConfigPath := filepath.Join(assets, "local.yaml")
	placeholderPaths := map[string]string{
		"paths.manifest":       filepath.Join(assets, "tasks.yaml"),
		"paths.agent_config":   filepath.Join(assets, "agent.yaml"),
		"paths.private_dir":    filepath.Join(assets, "private"),
		"paths.results_dir":    filepath.Join(assets, "results"),
		"paths.workspaces_dir": filepath.Join(assets, "workspaces"),
		"paths.state_dir":      filepath.Join(assets, "state"),
	}
	content := "version: 1\npaths:\n"
	for name, path := range placeholderPaths {
		content += "  " + name[strings.Index(name, ".")+1:] + ": " + path + "\n"
	}
	content += "execution:\n  prepare_timeout: 1m\n"
	if err := os.WriteFile(localConfigPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assets, "agent.yaml"), []byte("version: 2\nprovider:\n  id: custom\n  admission: optional\n  base_url: https://provider.test/v1\n  model: test-model\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	frozenFiles := map[string]string{
		"--eval-set": filepath.Join(assets, "eval-set.json"),
		"--lease":    filepath.Join(assets, "lease.json"),
		"--policy":   filepath.Join(assets, "policy.json"),
	}
	for _, path := range frozenFiles {
		if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	arguments := []string{"memory", "candidate", "evaluate",
		"--candidate", "does-not-exist", "--local-config", localConfigPath,
		"--workspace", workspace, "--state-dir", filepath.Join(t.TempDir(), "state"),
		"--max-prompt-tokens", "4096", "--max-summary-bytes", "8192",
		"--inventory-percent", "40", "--reserve-percent", "25"}
	for flag, path := range frozenFiles {
		arguments = append(arguments, flag, path)
	}
	exit := command.Run(context.Background(), arguments)
	if exit == ExitOK || !strings.Contains(stderr.String(), "create the candidate first") {
		t.Fatalf("unknown candidate must fail closed with a creation hint: exit=%d stderr=%q stdout=%q", exit, stderr.String(), stdout.String())
	}
}

// TestCandidatePromoteUsageRequiresClosedInputs 锁定晋升动词的用法合同：
// 缺少候选、算名、一次性审批 nonce、审批指纹或套件状态根即拒绝。
func TestCandidatePromoteUsageRequiresClosedInputs(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"memory", "candidate", "promote", "--candidate", "c-1"}); exit != ExitUsage || !strings.Contains(stderr.String(), "requires --candidate, --approver, --approval-nonce, --approval-fingerprint") {
		t.Fatalf("exit=%d stderr=%q", exit, stderr.String())
	}
	if exit := command.Run(context.Background(), []string{"memory", "promotion"}); exit != ExitUsage || !strings.Contains(stderr.String(), "activate, rollback or list") {
		t.Fatalf("promotion dispatch exit=%d stderr=%q", exit, stderr.String())
	}
}

// TestMedianInt64UsesLowerMiddle 锁定晋升账本对偶数样本使用下中位的合同。
func TestMedianInt64UsesLowerMiddle(t *testing.T) {
	if got := medianInt64([]int64{40, 10, 30, 20}); got != 20 {
		t.Fatalf("even sample median = %d, want lower middle 20", got)
	}
}

// TestPromotionActivateUsageRequiresVersion 锁定沙箱激活动词的用法合同：
// 必须显式携带版本号与状态根，缺省即拒绝。
func TestPromotionActivateUsageRequiresVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"memory", "promotion", "activate"}); exit != ExitUsage {
		t.Fatalf("activate without arguments must fail usage, exit=%d stderr=%q", exit, stderr.String())
	}
	if exit := command.Run(context.Background(), []string{"memory", "candidate", "promote", "--candidate", "c-1", "--approver", "fxbin", "--approval-fingerprint", "sha256:aa", "--suite-state-dir", "/tmp/x", "--max-prompt-tokens", "32000", "--max-summary-bytes", "16384", "--inventory-percent", "25", "--reserve-percent", "20", "--approval-nonce", "sha256:bb"}); exit == ExitOK {
		t.Fatalf("promote against an unknown store must not succeed, exit=%d", exit)
	}
}

// TestTrialActivationFlagFailsClosedWithoutPromotion 锁定试运行旗标的失败
// 关闭面：显式请求激活但目标库无激活版本时拒绝运行，不沉默回基线。
func TestTrialActivationFlagFailsClosedWithoutPromotion(t *testing.T) {
	workspace := t.TempDir()
	writeCapabilityConfig(t, workspace)
	var stdout, stderr bytes.Buffer
	command := Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return fakeProvider{}, nil }}, Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"run", "trial task", "--workspace", workspace, "--state-dir", t.TempDir(), "--trial-activation"})
	if exit == ExitOK || !strings.Contains(stderr.String(), "trial activation") {
		t.Fatalf("explicit trial activation without a promotion must fail closed: exit=%d stderr=%q", exit, stderr.String())
	}
}

// TestInitGeneratesLoadableMinimalConfig 锁定向导核心合同：目录预填能力卡、
// 生成的最小 YAML 可严格装载且能力字段保真。
func TestInitGeneratesLoadableMinimalConfig(t *testing.T) {
	original, _ := os.Getwd()
	tempWorkspace := t.TempDir()
	if err := os.Chdir(tempWorkspace); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"init",
		"--base-url", "https://provider.test/v4", "--model", "test-model-x", "--api-key-env", "TEST_KEY", "--catalog-url", "http://127.0.0.1:1/unreachable", "--catalog-cache", filepath.Join(t.TempDir(), "catalog.json")})
	if exit != ExitOK {
		t.Fatalf("init exit=%d stdout=%q stderr=%q", exit, stdout.String(), stderr.String())
	}
	loaded, err := config.Load(config.DefaultPath("."), config.Overrides{})
	if err != nil {
		t.Fatalf("generated config must load strictly: %v", err)
	}
	if loaded.Provider.BaseURL != "https://provider.test/v4" || loaded.Provider.Model != "test-model-x" || loaded.Provider.APIKeyEnv != "TEST_KEY" {
		t.Fatalf("provider fields drifted: %+v", loaded.Provider)
	}
	if loaded.Provider.Capability == nil || !loaded.Provider.Capability.ToolCalls || !loaded.Provider.Capability.StreamingUsage {
		t.Fatalf("capability missing or wrong: %+v", loaded.Provider.Capability)
	}
	if loaded.Provider.Capability.ContextWindowTokens != 0 {
		t.Fatal("catalog-miss model must leave context limit at zero")
	}
	if len(loaded.Provider.Capability.StopReasons) != 3 || loaded.Provider.Capability.StopReasons[0] != "stop" {
		t.Fatalf("stop reasons must be the protocol set: %v", loaded.Provider.Capability.StopReasons)
	}
}

// TestInitRefusesToOverwriteExistingConfig 锁定防覆盖：既有 config.yaml 存
// 在时向导拒绝执行。
func TestInitRefusesToOverwriteExistingConfig(t *testing.T) {
	original, _ := os.Getwd()
	tempWorkspace := t.TempDir()
	if err := os.Chdir(tempWorkspace); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(original)
	if err := os.MkdirAll(".agent", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config.DefaultPath("."), []byte("version: 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	exit := command.Run(context.Background(), []string{"init", "--base-url", "https://x.test", "--model", "m", "--api-key-env", "K", "--catalog-url", "http://127.0.0.1:1/unreachable", "--catalog-cache", filepath.Join(t.TempDir(), "catalog.json")})
	if exit == ExitOK || !strings.Contains(stderr.String(), "配置已存在") {
		t.Fatalf("must refuse overwrite by default: exit=%d stderr=%q", exit, stderr.String())
	}
}

// TestMemoryTelemetryUsageAndMissingStore 锁定遥测命令两面：用法缺省拒绝
// 与库缺失时失败关闭（ErrNotFound→非零退出，不产出伪造快照）。
func TestMemoryTelemetryUsageAndMissingStore(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"memory", "telemetry", "extra"}); exit != ExitUsage {
		t.Fatalf("positional args must fail usage: exit=%d", exit)
	}
	workspace := t.TempDir()
	writeCapabilityConfig(t, workspace)
	exit := command.Run(context.Background(), []string{"memory", "telemetry", "--workspace", workspace, "--state-dir", t.TempDir(), "--json"})
	if exit == ExitOK || !strings.Contains(stderr.String(), "memory telemetry failed") {
		t.Fatalf("missing store must fail closed: exit=%d stderr=%q", exit, stderr.String())
	}
}
