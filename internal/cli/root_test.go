package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
)

type fakeProvider struct{}

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
	for _, required := range []string{"cairn run", "--max-tool-calls", "--max-input-tokens", "--max-output-tokens", "--max-wall-time", "--max-cost-usd", "cairn resume", "--note <text>", "--evidence <ref>", "--abort-run --reason", "cairn inspect", "cairn doctor", "cairn benchmark run", "--prepare-timeout", "--prepare-proxy", "0 completed", "8 state corrupt"} {
		if !strings.Contains(usage, required) {
			t.Fatalf("usage omitted %q: %s", required, usage)
		}
	}
}

func TestBenchmarkRunRequiresExplicitLocalManifest(t *testing.T) {
	var stdout, stderr bytes.Buffer
	command := Command{Stdout: &stdout, Stderr: &stderr}
	if exit := command.Run(context.Background(), []string{"benchmark", "run", "--prepare-only"}); exit != ExitUsage || !strings.Contains(stderr.String(), "requires --manifest") {
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
