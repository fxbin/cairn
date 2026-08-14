package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

func TestClassifyFailurePreservesLoopGuardReason(t *testing.T) {
	classification := classifyFailure(&agent.LoopGuardExceededError{Reason: "periodic_turn_cycle"})
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
	report, err := qualificationRunner().withNow(now).Run(context.Background(), RunOptions{
		Pack:           pack,
		Attempts:       1,
		Configuration:  config.Defaults(),
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
	runner := qualificationRunner()
	runner.Now = func() time.Time { return now }
	runner.CloneAtBaseline = func(_ context.Context, _ Task, _ string, _ string) error {
		cloneCalls++
		return errors.New("checkout transport unavailable")
	}
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, Attempts: 2, Configuration: config.Defaults(), ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t), PrepareOnly: true,
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
	runner := qualificationRunner()
	runner.CloneAtBaseline = func(_ context.Context, _ Task, _ string, _ string) error { return errors.New("offline") }
	configuration := config.Defaults()
	configuration.Provider.BaseURL = ""
	configuration.Provider.Model = ""
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
	runner := qualificationRunner()
	runner.Now = func() time.Time { return now }
	runner.CloneAtBaseline = func(ctx context.Context, task Task, workspace, proxy string) error {
		cloned = append(cloned, workspace)
		return cloneAtBaseline(ctx, task, workspace, proxy)
	}
	runner.Service = app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return terminalProvider{}, nil }}
	configuration := config.Defaults()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "test"
	configuration.Provider.APIKeyEnv = "CAIRN_BENCHMARK_TEST_KEY"
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
	runner := qualificationRunner()
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
	configuration := config.Defaults()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "test"
	configuration.Provider.APIKeyEnv = "CAIRN_BENCHMARK_TEST_KEY"
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
	runner := qualificationRunner()
	runner.CloneAtBaseline = func(_ context.Context, _ Task, _ string, _ string) error {
		return errors.New("git https://private.example/repo?token=credential-value failed")
	}
	report, err := runner.Run(context.Background(), RunOptions{
		Pack: pack, PrepareOnly: true, Configuration: config.Defaults(), ResultsRoot: filepath.Join(output, "results"), WorkspacesRoot: filepath.Join(output, "workspaces"), StateRoot: filepath.Join(output, "state"), PrivateRoot: filepath.Join(output, "private"), CairnRoot: repositoryRoot(t),
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
	base := config.Defaults()
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
	if !containsString(configuration.Policy.EnvAllow, "DATABASE_URL") {
		t.Fatalf("verification environment was not allowlisted: %#v", configuration.Policy.EnvAllow)
	}
	base.Policy.Exec = "ask"
	if _, err := configurationForTask(base, testPack("https://example.invalid/repository.git", "0123456789").Tasks[0]); err == nil {
		t.Fatal("expected unattended policy rejection")
	}
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
	base := config.Defaults()
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

func TestMetricsFromEventsUsesPersistedUsageOnly(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventTurnOpened},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"answer.txt"}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"answer.txt"}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"apply_patch","arguments":{"path":"answer.txt"}}`)},
		{Type: domain.EventContextCompacted},
		{Type: domain.EventVerificationStarted},
		{Type: domain.EventModelResponseFinished, Payload: json.RawMessage(`{"usage":{"prompt_tokens":5,"completion_tokens":7}}`)},
	}
	configuration := config.Defaults()
	configuration.Pricing = config.PricingConfig{InputTokenUSD: 0.1, OutputTokenUSD: 0.2}
	metrics := metricsFromEvents(events, configuration)
	if metrics.Turns != 1 || metrics.ToolCalls != 3 || metrics.InputTokens != 5 || metrics.OutputTokens != 7 || math.Abs(metrics.CostUSD-1.9) > 1e-9 || metrics.FirstPatchTurn != 1 || metrics.FirstVerificationTurn != 1 || metrics.ContextCompactions != 1 || metrics.RepeatedReadSearchCalls != 1 || metrics.ZeroPatchTurnsBudgetExhausted {
		t.Fatalf("metrics=%#v", metrics)
	}
}

func TestMetricsFromEventsClassifiesZeroPatchTurnsBudgetExhaustion(t *testing.T) {
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
			metrics := metricsFromEvents(test.events, config.Defaults())
			if metrics.ZeroPatchTurnsBudgetExhausted != test.want {
				t.Fatalf("ZeroPatchTurnsBudgetExhausted=%t want %t", metrics.ZeroPatchTurnsBudgetExhausted, test.want)
			}
		})
	}
}

func TestMetricsFromEventsCanonicalizesObjectArgumentsAndSkipsInvalidArguments(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"answer.txt","options":{"after":2,"before":1}}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"options":{"before":1,"after":2},"path":"answer.txt"}}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"search_text"}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"search_text","arguments":"not-an-object"}`)},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"search_text","arguments":["not-an-object"]}`)},
	}
	metrics := metricsFromEvents(events, config.Defaults())
	if metrics.ToolCalls != len(events) || metrics.RepeatedReadSearchCalls != 1 {
		t.Fatalf("metrics=%#v", metrics)
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
func qualificationRunner() Runner {
	return Runner{
		ApplyPrivateVerifier: func(_ context.Context, _ string, _ string, _ string) (string, error) {
			return "sha256:test-verifier", nil
		},
		RunBootstrap:         func(_ context.Context, _ string, _ string, _ []Command) error { return nil },
		DockerImageAvailable: func(_ context.Context, _ string) error { return nil },
	}
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
	return emit(ports.ModelEvent{Kind: ports.Done})
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
