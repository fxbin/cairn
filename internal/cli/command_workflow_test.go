// 工作流子命令的 CLI 接线测试：真实存储只读渲染、NDJSON 输出与用法约束。
package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	workflowsqlite "github.com/fxbin/cairn/internal/adapters/workflow/sqlite"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/tools"
	"github.com/fxbin/cairn/internal/workflow"
)

// workspaceHashForCLI 以与应用层一致的方式计算工作区状态哈希。
func workspaceHashForCLI(t *testing.T, workspacePath string) string {
	t.Helper()
	configuration := config.Defaults()
	view, err := tools.NewWorkspace(workspacePath, configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
	if err != nil {
		t.Fatalf("create workspace view: %v", err)
	}
	digest := sha256.Sum256([]byte(view.Root))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// seedWorkflowForCLI 在临时工作区落一条真实工作流链供命令读取。
func seedWorkflowForCLI(t *testing.T, workspace, stateDir string) string {
	t.Helper()
	hash := workspaceHashForCLI(t, workspace)
	store, err := workflowsqlite.Open(filepath.Join(stateDir, "workspaces", hash, "workflow.db"))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	created, err := store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventRunCreated,
		Payload: encodeWorkflowPayload(t, workflow.RunCreatedPayload{
			Steps:               []workflow.StepSpec{{ID: "prepare", Index: 0, Kind: workflow.StepKindExecute}},
			PlanSHA256:          "sha256:plan",
			ConfigFingerprint:   "sha256:config",
			AuthorizationSource: "user-explicit",
		})})
	if err != nil {
		t.Fatalf("append created: %v", err)
	}
	if _, err := store.Append(context.Background(), created.Seq, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: hash, Type: workflow.EventStepStarted,
		Payload: encodeWorkflowPayload(t, workflow.StepStartedPayload{StepID: "prepare"})}); err != nil {
		t.Fatalf("append started: %v", err)
	}
	return string(id)
}

// encodeWorkflowPayload 编码负载并在失败时终止测试。
func encodeWorkflowPayload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return encoded
}

func TestWorkflowStatusRendersReplayedPlan(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	writeCapabilityConfig(t, workspace)
	id := seedWorkflowForCLI(t, workspace, stateDir)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := Command{Service: app.Service{}, Stdout: stdout, Stderr: stderr}.workflowCommand(context.Background(),
		[]string{"status", id, "--workspace", workspace, "--state-dir", stateDir})
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{id, "prepare", "[execute] running", "next step: 1. prepare"} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestWorkflowStatusJSONOutputsSingleLineView(t *testing.T) {
	workspace := t.TempDir()
	stateDir := t.TempDir()
	writeCapabilityConfig(t, workspace)
	id := seedWorkflowForCLI(t, workspace, stateDir)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := Command{Service: app.Service{}, Stdout: stdout, Stderr: stderr}.workflowCommand(context.Background(),
		[]string{"status", id, "--workspace", workspace, "--state-dir", stateDir, "--json"})
	if code != ExitOK {
		t.Fatalf("exit=%d stderr=%s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"workflow_id":"`+id) {
		t.Fatalf("json output=%q", stdout.String())
	}
}

func TestWorkflowCommandRejectsUnknownSubcommandAndMissingID(t *testing.T) {
	stderr := &bytes.Buffer{}
	if code := (Command{Service: app.Service{}, Stdout: &bytes.Buffer{}, Stderr: stderr}).workflowCommand(context.Background(), []string{"launch"}); code != ExitUsage {
		t.Fatalf("unknown subcommand exit=%d", code)
	}
	if code := (Command{Service: app.Service{}, Stdout: &bytes.Buffer{}, Stderr: stderr}).workflowCommand(context.Background(), []string{"status"}); code != ExitUsage {
		t.Fatalf("missing id exit=%d", code)
	}
}
