package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// benchmarkTestConfig 返回满足 v2 Provider 合同的基准测试基线配置，
// 仅补齐 base_url/model 与准入声明，其余沿用安全默认值。
func benchmarkTestConfig() config.Config {
	configuration := config.Defaults()
	configuration.Provider.BaseURL = "https://provider.test/v1"
	configuration.Provider.Model = "test-model"
	configuration.Provider.Admission = config.ProviderAdmissionOptional
	return configuration
}

func TestClassifyFailurePreservesLoopGuardReason(t *testing.T) {
	classification := contract.ClassifyError(&ports.LoopGuardExceededError{Reason: "periodic_turn_cycle"})
	if classification != "loop_guard_periodic_turn_cycle" {
		t.Fatalf("classification = %q", classification)
	}
}

func TestRunnerPrepareOnlyCreatesDisposableCloneAndResult(t *testing.T) {
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	root := repositoryRoot(t)
	output := t.TempDir()
	privateRoot := filepath.Join(output, "private")
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	report, err := qualificationRunner(source).withNow(now).Run(context.Background(), RunOptions{
		Pack:           pack,
		Attempts:       1,
		Configuration:  benchmarkTestConfig(),
		ResultsRoot:    filepath.Join(output, "results"),
		WorkspacesRoot: filepath.Join(output, "workspaces"),
		StateRoot:      filepath.Join(output, "state"),
		PrivateRoot:    privateRoot,
		CairnRoot:      root,
		PrepareOnly:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "prepared" || !report.EnvironmentValid || len(report.Results) != 0 || len(report.Qualifications) != 1 || report.Qualifications[0].Outcome != "qualified" || report.Qualifications[0].SourceRevision != baseline {
		t.Fatalf("report=%#v", report)
	}
	resultPath := filepath.Join(output, "results", report.SuiteRunID, "LOCAL-001-qualification-01.json")
	if _, err := os.Stat(resultPath); err != nil {
		t.Fatalf("result artifact: %v", err)
	}
	if got := report.Qualifications[0].Workspace; !strings.Contains(got, "qualification") || got == source {
		t.Fatalf("workspace=%q source=%q", got, source)
	}
}

func TestRunnerQualificationFailureProducesEnvironmentInvalidWithoutAgentAttempts(t *testing.T) {
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	output := t.TempDir()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cloneCalls := 0
	runner := qualificationRunner(source)
	runner.Now = func() time.Time { return now }
	runner.CloneAtBaseline = func(_ context.Context, _ Task, _ string, _ string) error {
		cloneCalls++
		return errors.New("checkout transport unavailable")
	}
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, Attempts: 2, Configuration: benchmarkTestConfig(), ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t), PrepareOnly: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "environment_invalid" || report.EnvironmentValid || len(report.Results) != 0 || len(report.Qualifications) != 2 || cloneCalls != 2 || report.Passed() {
		t.Fatalf("report=%#v clone_calls=%d", report, cloneCalls)
	}
	for _, qualification := range report.Qualifications {
		if qualification.Outcome != "failed" || qualification.FailureClass != "qualification_prepare" || !strings.Contains(qualification.Workspace, string(filepath.Separator)+"qualification"+string(filepath.Separator)+"LOCAL-001"+string(filepath.Separator)+"attempt-") {
			t.Fatalf("qualification=%#v", qualification)
		}
	}
	resultContents, err := os.ReadFile(filepath.Join(output, "results", report.SuiteRunID, "suite-result.json"))
	if err != nil || !strings.Contains(string(resultContents), `"environment_invalid"`) {
		t.Fatalf("suite result=%q err=%v", resultContents, err)
	}
}

func TestRunnerEnvironmentInvalidSkipsProviderCredentialRequirement(t *testing.T) {
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	output := t.TempDir()
	runner := qualificationRunner(source)
	runner.CloneAtBaseline = func(_ context.Context, _ Task, _ string, _ string) error { return errors.New("offline") }
	configuration := benchmarkTestConfig()
	configuration.Provider.APIKeyEnv = "UNSET_BENCHMARK_KEY"
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, Configuration: configuration, ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t),
	})
	if err != nil || report.Outcome != "environment_invalid" || len(report.Results) != 0 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
}

func TestRunnerQualificationAndAgentAttemptsUseIndependentWorkspaces(t *testing.T) {
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	output := t.TempDir()
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	cloned := make([]string, 0, 2)
	runner := qualificationRunner(source)
	runner.Now = func() time.Time { return now }
	runner.CloneAtBaseline = func(ctx context.Context, task Task, workspace, proxy string) error {
		cloned = append(cloned, workspace)
		task.Repository.URL = source
		return cloneAtBaseline(ctx, task, workspace, proxy)
	}
	runner.Service = app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return terminalProvider{}, nil }}
	configuration := benchmarkTestConfig()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "test"
	configuration.Provider.APIKeyEnv = "CAIRN_BENCHMARK_TEST_KEY"
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	configuration.Policy.Write, configuration.Policy.Exec = "allow", "allow"
	t.Setenv(configuration.Provider.APIKeyEnv, "test-key")
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, Attempts: 1, Configuration: configuration, ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cloned) != 2 || len(report.Qualifications) != 1 || len(report.Results) != 1 || cloned[0] == cloned[1] || report.Qualifications[0].Workspace == report.Results[0].Workspace {
		t.Fatalf("cloned=%#v report=%#v", cloned, report)
	}
}

func TestRunnerFormalPreparationFailureInvalidatesSuiteBeforeProvider(t *testing.T) {
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	output := t.TempDir()
	runner := qualificationRunner(source)
	bootstrapCalls := 0
	providerCalls := 0
	runner.RunBootstrap = func(_ context.Context, _ string, _ string, _ []Command) error {
		bootstrapCalls++
		if bootstrapCalls == 2 {
			return errors.New("private command output: secret-token")
		}
		return nil
	}
	runner.Service = app.Service{NewProvider: func(config.Config) (ports.Provider, error) {
		providerCalls++
		return terminalProvider{}, nil
	}}
	configuration := benchmarkTestConfig()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "test"
	configuration.Provider.APIKeyEnv = "CAIRN_BENCHMARK_TEST_KEY"
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	configuration.Policy.Write, configuration.Policy.Exec = "allow", "allow"
	t.Setenv(configuration.Provider.APIKeyEnv, "test-key")
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, Configuration: configuration, ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Outcome != "environment_invalid" || report.EnvironmentValid || len(report.Results) != 0 || providerCalls != 0 || len(report.Qualifications) != 1 || len(report.Preparations) != 1 || report.Preparations[0].FailureClass != "preparation_bootstrap" {
		t.Fatalf("report=%#v provider_calls=%d", report, providerCalls)
	}
	if report.Preparations[0].Error != "bootstrap preparation failed" || strings.Contains(report.Preparations[0].Error, "secret-token") {
		t.Fatalf("preparation error leaked detail: %#v", report.Preparations[0])
	}
}

func TestQualificationFailureUsesOnlyRedactedStableError(t *testing.T) {
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	output := t.TempDir()
	runner := qualificationRunner(source)
	runner.CloneAtBaseline = func(_ context.Context, _ Task, _ string, _ string) error {
		return errors.New("git https://private.example/repo?token=credential-value failed")
	}
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, PrepareOnly: true, Configuration: benchmarkTestConfig(), ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t),
	})
	if err != nil || len(report.Qualifications) != 1 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	qualification := report.Qualifications[0]
	if qualification.Error != "source preparation failed" || strings.Contains(qualification.Error, "private.example") || strings.Contains(qualification.Error, "credential-value") {
		t.Fatalf("qualification error leaked detail: %#v", qualification)
	}
}

func TestTaskPackLoadsStrictlyFromCallerSuppliedLocalPath(t *testing.T) {
	contents := []byte("version: 1\nsuite: local-test\nintended_use: evaluation_only\ntask_count: 1\ntasks:\n  - id: LOCAL-001\n    kind: natural\n    repository:\n      name: local\n      url: https://example.invalid/repository.git\n      baseline_commit: 0123456789\n    language: go\n    task_prompt: repair\n    scope:\n      allowed_paths: [main.go]\n      prohibited_paths: [vendor/]\n    verification:\n      command:\n        argv: [go, test, ./...]\n        cwd: .\n    runtime:\n      docker_image: alpine:latest\nexecution_contract:\n  attempts_per_task: 1\n  agent_workspace: disposable_clone\n  reset_between_attempts: true\n  network_during_agent_run: denied\n  bootstrap_time_in_budget: false\n")
	path := filepath.Join(t.TempDir(), "tasks.yaml")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	pack, err := LoadTaskPack(path)
	if err != nil || pack.TaskCount != 1 || pack.Tasks[0].ID != "LOCAL-001" {
		t.Fatalf("pack=%#v err=%v", pack, err)
	}
}

// TestTaskPackRejectsPrivateOrLocalRepositoryLocations 锁定评测合同写入结果前的
// 来源边界：仅允许无凭据的 HTTP(S) 远端标识，避免本机路径或 URL 令牌进入 TaskSpec。
func TestTaskPackRejectsPrivateOrLocalRepositoryLocations(t *testing.T) {
	for _, repositoryURL := range []string{
		"/Users/example/private-repository",
		"file:///Users/example/private-repository",
		"https://user:token@example.invalid/repository.git",
		"https://example.invalid/repository.git?token=secret",
		"https://example.invalid/repository.git#private",
		"git@example.invalid:owner/repository.git",
	} {
		pack := testPack("https://example.invalid/repository.git", "0123456789")
		pack.Tasks[0].Repository.URL = repositoryURL
		if err := pack.Validate(); err == nil {
			t.Fatalf("repository URL %q was accepted", repositoryURL)
		}
	}
}

// TestTaskPackRejectsRepositoryNamesThatCanLeakLocalPaths 锁定来源显示名与 URL
// 共享同一空间边界，避免本地路径经 TaskSpec.RepositoryName 重新进入结果。
func TestTaskPackRejectsRepositoryNamesThatCanLeakLocalPaths(t *testing.T) {
	for _, name := range []string{"/Users/example/private-repository", "../private-repository", "owner/../private-repository", `C:\\Users\\example\\private-repository`} {
		pack := testPack("https://example.invalid/repository.git", "0123456789")
		pack.Tasks[0].Repository.Name = name
		if err := pack.Validate(); err == nil {
			t.Fatalf("repository name %q was accepted", name)
		}
	}
}

// TestBenchmarkResultJSONOmitsRuntimePaths 锁定所有可持久化 benchmark 结果都
// 不编码 runner 的 clone 或状态目录；这些路径只用于进程内执行编排。
func TestBenchmarkResultJSONOmitsRuntimePaths(t *testing.T) {
	report := SuiteResult{
		Qualifications: []QualificationResult{{Workspace: "/private/qualification"}},
		Preparations:   []PreparationResult{{Workspace: "/private/preparation", StateDir: "/private/preparation-state"}},
		Results:        []AttemptResult{{Workspace: "/private/attempt", StateDir: "/private/attempt-state"}},
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"/private/qualification", "/private/preparation", "/private/preparation-state", "/private/attempt", "/private/attempt-state", `"workspace"`, `"state_dir"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("benchmark JSON leaked runtime path field %q: %s", forbidden, encoded)
		}
	}
}

func TestTaskPromptExcludesReferenceOracle(t *testing.T) {
	task := testPack("https://example.invalid/repository.git", "0123456789").Tasks[0]
	prompt := task.AgentPrompt()
	for _, forbidden := range []string{task.Repository.ReferenceCommit, task.Repository.ReferenceSubject} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("agent prompt exposed reference metadata %q: %s", forbidden, prompt)
		}
	}
	if !strings.Contains(prompt, task.Scope.AllowedPaths[0]) || !strings.Contains(prompt, strings.Join(task.Verification.Command.Argv, " ")) {
		t.Fatalf("agent prompt omitted local task constraints: %s", prompt)
	}
}

func TestConfigurationForTaskRequiresDockerAndUnattendedPolicy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 2\npreset: coding\nprovider:\n  id: custom\n  admission: optional\n  base_url: https://provider.test/v1\n  model: test-model\n  api_key_env: K\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := config.Load(path, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	base.Policy.Write, base.Policy.Exec = "allow", "allow"
	task := testPack("https://example.invalid/repository.git", "0123456789").Tasks[0]
	task.Verification.Command.Env = map[string]string{"DATABASE_URL": "sqlite+aiosqlite:///:memory:"}
	task.Verification.VolatilePaths = []string{".pytest_cache"}
	configuration, err := configurationForTask(base, task)
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Sandbox.Backend != "docker" || configuration.Sandbox.DockerImage != "alpine:latest" || len(configuration.Verification.Commands) != 1 || configuration.Verification.Commands[0].Env["DATABASE_URL"] != "sqlite+aiosqlite:///:memory:" || !containsString(configuration.Verification.VolatilePaths, ".pytest_cache") {
		t.Fatalf("configuration=%#v", configuration)
	}
	if configuration.Preset != config.PresetBenchmark {
		t.Fatalf("benchmark attempt must freeze the benchmark preset, got %q", configuration.Preset)
	}
	presetEntry, found := benchmarkResolutionEntry(configuration.Resolution, "preset")
	if !found || presetEntry.Value != config.PresetBenchmark || presetEntry.Source != "benchmark:runner" {
		t.Fatalf("benchmark preset resolution=%+v found=%t", presetEntry, found)
	}
	if !containsString(configuration.Policy.EnvAllow, "DATABASE_URL") {
		t.Fatalf("verification environment was not allowlisted: %#v", configuration.Policy.EnvAllow)
	}
	base.Policy.Exec = "ask"
	if _, err := configurationForTask(base, testPack("https://example.invalid/repository.git", "0123456789").Tasks[0]); err == nil {
		t.Fatal("expected unattended policy rejection")
	}
}

// benchmarkResolutionEntry 从 benchmark 冻结配置的 Resolved Plan 查询单个字段，
// 供测试验证 runner 强制 Preset 后不会遗留 coding 的来源记录。
func benchmarkResolutionEntry(entries []config.ResolutionEntry, field string) (config.ResolutionEntry, bool) {
	for _, entry := range entries {
		if entry.Field == field {
			return entry, true
		}
	}
	return config.ResolutionEntry{}, false
}

func TestTaskVerificationVolatilePathsAreExactAndValidated(t *testing.T) {
	pack := testPack("https://example.invalid/repository.git", "0123456789")
	pack.Tasks[0].Verification.VolatilePaths = []string{".pytest_cache", "tmp/test-results"}
	if err := pack.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]string{{"."}, {"**"}, {"tmp/*"}, {".git"}, {".git/objects"}, {".agent/config.yaml"}, {".cairn/artifacts"}, {"tmp", "tmp"}, {"../escape"}} {
		pack.Tasks[0].Verification.VolatilePaths = invalid
		if err := pack.Validate(); err == nil {
			t.Fatalf("expected volatile paths %q to fail", invalid)
		}
	}
}

func TestConfigurationForTaskUsesOnlyDeclaredVolatilePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "backend", ".pytest_cache"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("before"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := benchmarkTestConfig()
	base.Provider.BaseURL = "https://provider.test/v1"
	base.Provider.Model = "test-model"
	base.Policy.Write, base.Policy.Exec = "allow", "allow"
	task := testPack("https://example.invalid/repository.git", "0123456789").Tasks[0]
	task.Verification.VolatilePaths = []string{"backend/.pytest_cache"}
	configuration, err := configurationForTask(base, task)
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Verification.VolatilePaths) != 1 || configuration.Verification.VolatilePaths[0] != "backend/.pytest_cache" {
		t.Fatalf("volatile paths=%#v", configuration.Verification.VolatilePaths)
	}
	workspace, err := tools.NewWorkspace(root, []string{"node_modules"}, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	verificationView := workspace.ObservationView(configuration.Verification.VolatilePaths)
	before, err := verificationView.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "backend", ".pytest_cache", "lastfailed"), []byte("volatile"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterVolatile, err := verificationView.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if afterVolatile != before {
		t.Fatal("declared volatile cache changed verifier observation")
	}
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("after"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterTracked, err := verificationView.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if afterTracked == afterVolatile {
		t.Fatal("undeclared workspace mutation did not change verifier observation")
	}
}

func TestCommandEnvironmentRejectsSecretLikeValues(t *testing.T) {
	for _, command := range []Command{
		{Argv: []string{"true"}, CWD: ".", Env: map[string]string{"MODEL_API_KEY": "value"}},
		{Argv: []string{"true"}, CWD: ".", Env: map[string]string{"DATABASE_URL": "sk-not-a-public-value"}},
		{Argv: []string{"true"}, CWD: ".", Env: map[string]string{"DATABASE_URL": "postgresql://user:password@localhost/cairn"}},
		{Argv: []string{"true"}, CWD: ".", Env: map[string]string{"DATABASE_URL": "line\nbreak"}},
	} {
		if err := validateCommand(command); err == nil {
			t.Fatalf("expected command environment rejection: %#v", command)
		}
	}
}

func TestApplyPrivateVerifierRecordsDigestWithoutPublicContent(t *testing.T) {
	workspace, _ := localGitRepository(t)
	privateRoot := filepath.Join(t.TempDir(), "private")
	verifierDirectory := filepath.Join(privateRoot, "verifiers")
	if err := os.MkdirAll(verifierDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/note.txt b/note.txt\nindex df967b9..48b3c1c 100644\n--- a/note.txt\n+++ b/note.txt\n@@ -1 +1,2 @@\n baseline\n+private verifier\n"
	if err := os.WriteFile(filepath.Join(verifierDirectory, "LOCAL-001.patch"), []byte(patch), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprint, err := applyPrivateVerifier(context.Background(), workspace, privateRoot, "LOCAL-001")
	if err != nil || !strings.HasPrefix(fingerprint, "sha256:") {
		t.Fatalf("fingerprint=%q err=%v", fingerprint, err)
	}
	contents, err := os.ReadFile(filepath.Join(workspace, "note.txt"))
	if err != nil || !strings.Contains(string(contents), "private verifier") {
		t.Fatalf("patched contents=%q err=%v", contents, err)
	}
}

func TestObservationsFromEventsDeriveBenchmarkOnlyMetrics(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventTurnOpened},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"answer.txt"}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"answer.txt"}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"apply_patch","arguments":{"path":"answer.txt"}}`)},
		{Type: domain.EventContextCompacted},
		{Type: domain.EventVerificationStarted},
		{Type: domain.EventModelResponseFinished, Payload: json.RawMessage(`{"usage":{"prompt_tokens":5,"completion_tokens":7}}`)},
	}
	observations := observationsFromEvents(events)
	if observations.FirstPatchTurn != 1 || observations.FirstVerificationTurn != 1 || observations.RepeatedReadSearchCalls != 1 || observations.ZeroPatchTurnsBudgetExhausted {
		t.Fatalf("observations=%#v", observations)
	}
}

func TestObservationsFromEventsClassifiesZeroPatchTurnsBudgetExhaustion(t *testing.T) {
	tests := []struct {
		name   string
		events []domain.Event
		want   bool
	}{
		{
			name: "turn budget exhaustion without patch proposal",
			events: []domain.Event{
				{Type: domain.EventRunFailed, Payload: json.RawMessage(`{"budget_dimension":"turns"}`)},
			},
			want: true,
		},
		{
			name: "turn budget exhaustion after patch proposal",
			events: []domain.Event{
				{Type: domain.EventTurnOpened},
				{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"apply_patch"}`)},
				{Type: domain.EventRunFailed, Payload: json.RawMessage(`{"budget_dimension":"turns"}`)},
			},
			want: false,
		},
		{
			name: "other budget dimension without patch proposal",
			events: []domain.Event{
				{Type: domain.EventRunFailed, Payload: json.RawMessage(`{"budget_dimension":"input_tokens"}`)},
			},
			want: false,
		},
		{
			name: "no run failure without patch proposal",
			events: []domain.Event{
				{Type: domain.EventTurnOpened},
				{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file"}`)},
			},
			want: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			observations := observationsFromEvents(test.events)
			if observations.ZeroPatchTurnsBudgetExhausted != test.want {
				t.Fatalf("ZeroPatchTurnsBudgetExhausted=%t want %t", observations.ZeroPatchTurnsBudgetExhausted, test.want)
			}
		})
	}
}

func TestObservationsCanonicalizeObjectArgumentsAndSkipInvalidArguments(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"answer.txt","options":{"after":2,"before":1}}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"options":{"before":1,"after":2},"path":"answer.txt"}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"search_text"}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"search_text","arguments":"not-an-object"}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"search_text","arguments":["not-an-object"]}`)},
	}
	observations := observationsFromEvents(events)
	if observations.RepeatedReadSearchCalls != 1 {
		t.Fatalf("observations=%#v", observations)
	}
}

// TestAttemptResultEmbedsUnifiedContract 锁定 benchmark 尝试与普通 Run 共用的
// 产品合同投影：任务身份、配置指纹、Provider、终态与完成证据都由
// contract.Result 携带，且不泄漏任务正文或本地路径。
func TestAttemptResultEmbedsUnifiedContract(t *testing.T) {
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	output := t.TempDir()
	runner := qualificationRunner(source)
	runner.Service = app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return terminalProvider{}, nil }}
	configuration := benchmarkTestConfig()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "test-model"
	configuration.Provider.APIKeyEnv = "CAIRN_BENCHMARK_TEST_KEY"
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	configuration.Policy.Write, configuration.Policy.Exec = "allow", "allow"
	t.Setenv(configuration.Provider.APIKeyEnv, "test-key")
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, Attempts: 1, Configuration: configuration, ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t),
	})
	if err != nil || len(report.Results) != 1 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	attempt := report.Results[0]
	if attempt.SchemaVersion != 2 || attempt.Run == nil {
		t.Fatalf("attempt schema=%d run=%#v", attempt.SchemaVersion, attempt.Run)
	}
	manifest := attempt.Run.Manifest
	if manifest.Task.Kind != contract.TaskKindBenchmark || manifest.Task.Benchmark == nil || manifest.Task.Benchmark.TaskID != "LOCAL-001" || manifest.Task.Benchmark.TaskPackFingerprint != pack.Fingerprint() || manifest.Task.Benchmark.VerifierFingerprint != "sha256:test-verifier" || manifest.Task.Benchmark.SourceRevision != baseline {
		t.Fatalf("task spec=%#v", manifest.Task)
	}
	if manifest.RunID == "" || manifest.WorkspaceHash == "" || manifest.ConfigFingerprint == "" || manifest.Build.Fingerprint == "" {
		t.Fatalf("manifest identity incomplete: %#v", manifest)
	}
	if manifest.Provider.Kind != contract.ProviderKindOpenAICompatible || manifest.Provider.BaseURL != "https://example.invalid/v1" || manifest.Provider.Model != "test-model" {
		t.Fatalf("provider identity=%#v", manifest.Provider)
	}
	if manifest.Preset != config.PresetBenchmark || manifest.Policy.Write != "allow" || manifest.Policy.Exec != "allow" {
		t.Fatalf("preset/policy identity=%#v", manifest)
	}
	if attempt.Run.State != string(domain.RunCompleted) || attempt.Run.FailureClass != "" || !attempt.Run.Evidence.Trusted {
		t.Fatalf("run outcome=%#v", attempt.Run)
	}
	// 统一合同与结果信封都不得携带任务正文、本地路径或密钥；Workspace/
	// StateDir 只在 runner 进程内用于编排，序列化结果中必须完全缺席。
	contractOnly, err := json.Marshal(struct {
		Task contract.TaskSpec `json:"task"`
		Run  *contract.Result  `json:"run"`
	}{Task: attempt.Task, Run: attempt.Run})
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"修复本地测试任务", attempt.Workspace, attempt.StateDir, "test-key"} {
		if strings.Contains(string(contractOnly), forbidden) {
			t.Fatalf("unified contract leaked %q", forbidden)
		}
	}
	if encoded, err := json.Marshal(attempt); err != nil {
		t.Fatal(err)
	} else if strings.Contains(string(encoded), "修复本地测试任务") || strings.Contains(string(encoded), "test-key") || strings.Contains(string(encoded), attempt.Workspace) || strings.Contains(string(encoded), attempt.StateDir) {
		t.Fatal("attempt envelope leaked task prompt, credential, or local path")
	}
}

// TestAttemptResultDecodesSchemaV1Files 锁定旧 schema 1 结果文件仍可解码：
// 升级不破坏既有本地结果的可读性，运行期字段自然为零值。
func TestAttemptResultDecodesSchemaV1Files(t *testing.T) {
	legacy := []byte(`{"schema_version":1,"task_id":"LOCAL-001","attempt":1,"outcome":"failed","failure_class":"budget_turns","workspace":"/tmp/w","state_dir":"/tmp/s","task_pack_fingerprint":"sha256:x","config_fingerprint":"sha256:y","provider_base_url":"https://old.invalid/v1","model":"old-model","run_id":"run-1","run_state":"failed","verification_succeeded":false,"doctor_ok":true,"metrics":{"turns":40,"tool_calls":10,"input_tokens":100,"output_tokens":20,"cost_usd":0.5}}`)
	var attempt AttemptResult
	if err := json.Unmarshal(legacy, &attempt); err != nil {
		t.Fatal(err)
	}
	if attempt.SchemaVersion != 1 || attempt.TaskID != "LOCAL-001" || attempt.Outcome != "failed" || attempt.FailureClass != "budget_turns" || !attempt.DoctorOK {
		t.Fatalf("decoded legacy attempt=%#v", attempt)
	}
	if attempt.Run != nil {
		t.Fatalf("legacy file must not invent a contract result: %#v", attempt.Run)
	}
}

func TestRunProcessOutputTerminatesTheWholeProcessGroupOnTimeout(t *testing.T) {
	timedContext, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := runProcessOutput(timedContext, t.TempDir(), []string{"/bin/sh", "-c", "sleep 30 & wait"})
	if err != context.DeadlineExceeded {
		t.Fatalf("error=%v", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("timeout left a child process running for %s", elapsed)
	}
}

func TestValidatePrepareProxyOnlyAllowsCredentialFreeLoopbackHTTP(t *testing.T) {
	for _, value := range []string{"", "http://127.0.0.1:7890", "http://localhost:7890", "http://[::1]:7890"} {
		if err := validatePrepareProxy(value); err != nil {
			t.Fatalf("validate %q: %v", value, err)
		}
	}
	for _, value := range []string{
		"https://127.0.0.1:7890",
		"http://user:password@127.0.0.1:7890",
		"http://example.com:7890",
		"http://127.0.0.1",
		"http://127.0.0.1:7890/path",
		"http://127.0.0.1:7890?token=value",
	} {
		if err := validatePrepareProxy(value); err == nil {
			t.Fatalf("expected proxy %q to be rejected", value)
		}
	}
}

func TestBootstrapDockerArgumentsAreFailClosedAndDoNotUseProxy(t *testing.T) {
	workspace := t.TempDir()
	arguments, err := bootstrapDockerArguments(workspace, "example.test/cairn-task:1", Command{Argv: []string{"tool", "--check"}, CWD: "."})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(arguments, " ")
	for _, required := range []string{"docker run --rm", "--pull never", "--network none", "--read-only", "--cap-drop ALL", "--security-opt no-new-privileges", "--pids-limit 64", "--mount type=bind,src=" + workspace + ",dst=/workspace", "--workdir /workspace", "--env HOME=/tmp", "--env TMPDIR=/tmp", "example.test/cairn-task:1 tool --check"} {
		if !strings.Contains(joined, required) {
			t.Fatalf("bootstrap arguments omitted %q: %q", required, joined)
		}
	}
	for _, forbidden := range []string{"HTTP_PROXY=", "HTTPS_PROXY=", "http_proxy=", "https_proxy="} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("bootstrap must not receive %q: %q", forbidden, joined)
		}
	}
	if _, err := bootstrapDockerArguments(workspace, "", Command{Argv: []string{"true"}, CWD: "."}); err == nil {
		t.Fatal("expected image validation failure")
	}
}

func TestRestrictedProcessEnvironmentDoesNotInheritModelKey(t *testing.T) {
	t.Setenv("DEEPSEEK_API_KEY", "benchmark-test-secret")
	environment := strings.Join(restrictedProcessEnvironment("http://127.0.0.1:7890"), "\n")
	if strings.Contains(environment, "DEEPSEEK_API_KEY") || strings.Contains(environment, "benchmark-test-secret") {
		t.Fatalf("restricted environment inherited model credential: %q", environment)
	}
	for _, required := range []string{"HTTP_PROXY=http://127.0.0.1:7890", "HTTPS_PROXY=http://127.0.0.1:7890", "HOME="} {
		if !strings.Contains(environment, required) {
			t.Fatalf("restricted environment omitted %q: %q", required, environment)
		}
	}
}

func testPack(repositoryURL, baseline string) TaskPack {
	if filepath.IsAbs(repositoryURL) {
		repositoryURL = "https://example.invalid/local-test-repository.git"
	}
	return TaskPack{
		Version: 1, Suite: "test-pack", IntendedUse: "evaluation_only", TaskCount: 1,
		Tasks: []Task{{
			ID:           "LOCAL-001",
			Kind:         "natural",
			Repository:   Repository{Name: "local", URL: repositoryURL, BaselineCommit: baseline, ReferenceCommit: "reference-only", ReferenceSubject: "reference subject"},
			Language:     "go",
			TaskPrompt:   "修复本地测试任务",
			Scope:        Scope{AllowedPaths: []string{"note.txt"}, ProhibitedPaths: []string{".git"}},
			Verification: Verification{Command: Command{Argv: []string{"true"}, CWD: "."}},
			Runtime:      Runtime{DockerImage: "alpine:latest"},
		}},
		Execution: ExecutionContract{AttemptsPerTask: 1, AgentWorkspace: "disposable_clone", ResetBetweenAttempts: true, NetworkDuringAgentRun: "denied"},
	}
}

// qualificationRunner 返回不依赖本机 Docker 或私有 verifier 文件的 runner；测试仍
// 使用真实 git clone，从而覆盖 qualification 与正式 clone 的工作区隔离。
func qualificationRunner(sources ...string) Runner {
	runner := Runner{
		ApplyPrivateVerifier: func(_ context.Context, _ string, _ string, _ string) (string, error) {
			return "sha256:test-verifier", nil
		},
		RunBootstrap:         func(_ context.Context, _ string, _ string, _ []Command) error { return nil },
		DockerImageAvailable: func(_ context.Context, _ string) error { return nil },
	}
	if len(sources) == 1 {
		source := sources[0]
		runner.CloneAtBaseline = func(ctx context.Context, task Task, workspace, proxy string) error {
			task.Repository.URL = source
			return cloneAtBaseline(ctx, task, workspace, proxy)
		}
	}
	return runner
}

// withNow 为测试 runner 固定时钟，避免结果目录和时间字段随系统时间变化。
func (runner Runner) withNow(now time.Time) Runner {
	runner.Now = func() time.Time { return now }
	return runner
}

// terminalProvider 仅返回最终文本，确保 runner 测试不会发起真实网络模型请求。
type terminalProvider struct{}

// Stream 生成一条无工具调用的最终响应，供独立工作区测试验证执行路径。
func (terminalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"}); err != nil {
		return err
	}
	if err := emit(ports.ModelEvent{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done, StopReason: "stop"})
}

func localGitRepository(t *testing.T) (string, string) {
	t.Helper()
	repository := t.TempDir()
	runGit(t, repository, "init")
	runGit(t, repository, "config", "user.email", "cairn@example.test")
	runGit(t, repository, "config", "user.name", "Cairn Test")
	if err := os.WriteFile(filepath.Join(repository, "note.txt"), []byte("baseline\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit(t, repository, "add", "note.txt")
	runGit(t, repository, "commit", "-m", "baseline")
	command := exec.Command("git", "rev-parse", "HEAD")
	command.Dir = repository
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	return repository, strings.TrimSpace(string(output))
}

func runGit(t *testing.T, directory string, arguments ...string) {
	t.Helper()
	command := exec.Command("git", arguments...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(arguments, " "), err, output)
	}
}

// containsString 判断测试断言所需的字符串是否存在于切片中。
func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test file")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

// TestRetryTransientRetriesThenSucceedsAndExhausts 锁定准备阶段重试语义：瞬态
// 错误按上限重试，成功即返回，取消类错误不重试。
func TestRetryTransientRetriesThenSucceedsAndExhausts(t *testing.T) {
	originalBackoff := prepareRetryBackoff
	prepareRetryBackoff = 0
	defer func() { prepareRetryBackoff = originalBackoff }()
	calls := 0
	err := retryTransient(func() error {
		calls++
		if calls < 3 {
			return errors.New("spurious network error")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	calls = 0
	err = retryTransient(func() error {
		calls++
		return context.Canceled
	})
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("cancelled must not retry: err=%v calls=%d", err, calls)
	}
	calls = 0
	err = retryTransient(func() error {
		calls++
		return errors.New("persistent")
	})
	if err == nil || calls != prepareRetryAttempts {
		t.Fatalf("exhaustion must reach attempt cap: err=%v calls=%d", err, calls)
	}
}
