package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

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
	exitCode := command.Run(context.Background(), []string{"run", "answer this", "--workspace", t.TempDir(), "--state-dir", t.TempDir(), "--json"})
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
	if exit := runCommand.Run(context.Background(), []string{"run", "seed journal", "--workspace", workspace, "--state-dir", stateDir, "--json"}); exit != ExitOK {
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
}
