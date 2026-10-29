// 工作流执行类子命令的 CLI 接线测试：run/resume/accept 的退出码、NDJSON 输出
// 与人工门提示，不接受任意脚本。
package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
)

// cliWorkflowProvider 是 CLI 用例的最小模型替身：只回报文本与完成信号。
type cliWorkflowProvider struct {
	calls int
}

// Stream 记录一次调用并返回最小完成流。
func (provider *cliWorkflowProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"}); err != nil {
		return err
	}
	if err := emit(ports.ModelEvent{Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}); err != nil {
		return err
	}
	return emit(ports.ModelEvent{Kind: ports.Done, StopReason: "stop"})
}

// writeWorkflowCapabilityConfig 写入带冻结验证命令的工作区配置；工作流创建
// 前置门要求配置中已有验证命令。
func writeWorkflowCapabilityConfig(t *testing.T, workspace string) {
	t.Helper()
	path := config.DefaultPath(workspace)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "version: 2\nprovider:\n  id: custom\n  admission: optional\n  base_url: https://provider.test/v1\n  model: test-model\n  capability:\n    tool_calls: true\n    streaming_usage: true\npolicy:\n  write: allow\n  exec: allow\nverification:\n  commands:\n    - argv:\n        - /usr/bin/true\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// workflowCommandFixture 准备一个可执行工作流命令的临时工作区。
func workflowCommandFixture(t *testing.T) (string, string, *cliWorkflowProvider) {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeWorkflowCapabilityConfig(t, workspace)
	provider := &cliWorkflowProvider{}
	return workspace, stateDir, provider
}

// TestWorkflowRunCommandRejectsMissingTask 锁定用法约束：缺任务正文时不进入执行路径。
func TestWorkflowRunCommandRejectsMissingTask(t *testing.T) {
	workspace, stateDir, provider := workflowCommandFixture(t)
	stderr := &bytes.Buffer{}
	code := (Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }},
		Stdout: &bytes.Buffer{}, Stderr: stderr}).workflowCommand(context.Background(),
		[]string{"run", "--workspace", workspace, "--state-dir", stateDir})
	if code != ExitUsage {
		t.Fatalf("exit = %d stderr = %s", code, stderr.String())
	}
	if provider.calls != 0 {
		t.Fatalf("provider calls = %d", provider.calls)
	}
}

// TestWorkflowRunCommandStopsAtAcceptGate 锁定执行类命令的默认收束：工作流停在
// 人工审批门并给出下一步提示，不自动通过。
func TestWorkflowRunCommandStopsAtAcceptGate(t *testing.T) {
	workspace, stateDir, provider := workflowCommandFixture(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := (Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }},
		Stdout: stdout, Stderr: stderr}).workflowCommand(context.Background(),
		[]string{"run", "answer the question", "--workspace", workspace, "--state-dir", stateDir})
	if code != ExitOK {
		t.Fatalf("exit = %d stderr = %s", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "next action:") || !strings.Contains(output, "workflow accept") {
		t.Fatalf("output = %q", output)
	}
	if !strings.Contains(output, "[accept] pending") {
		t.Fatalf("output = %q", output)
	}
}

// TestWorkflowRunCommandJSONOutputsSingleLineOutcome 锁定 --json 只输出单行 NDJSON。
func TestWorkflowRunCommandJSONOutputsSingleLineOutcome(t *testing.T) {
	workspace, stateDir, provider := workflowCommandFixture(t)
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	code := (Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }},
		Stdout: stdout, Stderr: stderr}).workflowCommand(context.Background(),
		[]string{"run", "answer the question", "--workspace", workspace, "--state-dir", stateDir, "--json"})
	if code != ExitOK {
		t.Fatalf("exit = %d stderr = %s", code, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], `"workflow_id":"`) {
		t.Fatalf("json output = %q", stdout.String())
	}
}

// TestWorkflowAcceptCommandRefusesWhenNotAtAcceptGate 锁定人工门失败关闭：目标
// 工作流不存在时命令如实失败，不自动创建或通过。
func TestWorkflowAcceptCommandRefusesWhenNotAtAcceptGate(t *testing.T) {
	workspace, stateDir, provider := workflowCommandFixture(t)
	stderr := &bytes.Buffer{}
	code := (Command{Service: app.Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }},
		Stdout: &bytes.Buffer{}, Stderr: stderr}).workflowCommand(context.Background(),
		[]string{"accept", "wf-missing", "--workspace", workspace, "--state-dir", stateDir})
	if code == ExitOK {
		t.Fatal("accept must fail for a missing workflow")
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

// TestWorkflowCommandRequiresSubcommand 锁定分派约束：缺子命令时只报用法错误。
func TestWorkflowCommandRequiresSubcommand(t *testing.T) {
	stderr := &bytes.Buffer{}
	code := (Command{Service: app.Service{}, Stdout: &bytes.Buffer{}, Stderr: stderr}).workflowCommand(context.Background(), nil)
	if code != ExitUsage {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stderr.String(), "status, run, resume, accept") {
		t.Fatalf("stderr = %q", stderr.String())
	}
}
