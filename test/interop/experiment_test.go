//go:build interop

// 本文件属于 interop 标签档：对照实验需要真实外部 CLI 进程。
package interop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// moduleRoot 返回当前模块根目录。
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	return root
}

// buildHarnesses 装配三侧外壳：桩模型、Cairn 原生二进制、Codex 与 Claude
// Code 的隔离家目录。返回装配好的外壳与桩模型句柄。
func buildHarnesses(t *testing.T, ctx context.Context) ([]Harness, *Stub) {
	t.Helper()
	stub, err := StartStub()
	if err != nil {
		t.Fatalf("start stub: %v", err)
	}
	t.Cleanup(func() { _ = stub.Close() })

	root := t.TempDir()
	binary, err := BuildCairnBinary(ctx, moduleRoot(t), filepath.Join(root, "bin"))
	if err != nil {
		t.Fatalf("build cairn: %v", err)
	}

	harnesses := []Harness{
		NewCairnHarness(binary, filepath.Join(root, "cairn"), stub.BaseURL()),
	}
	codexHarness, err := NewCodexHarness(filepath.Join(root, "codex-home"), stub.BaseURL())
	if err != nil {
		t.Fatalf("setup codex harness: %v", err)
	}
	harnesses = append(harnesses, codexHarness)

	claudeHarness, err := NewClaudeHarness(filepath.Join(root, "claude-config"), stub.BaseURL())
	if err != nil {
		t.Fatalf("setup claude harness: %v", err)
	}
	harnesses = append(harnesses, claudeHarness)
	return harnesses, stub
}

// TestHarnessPlumbing 校验三侧外壳都能在零凭据条件下启动并完成一次最小
// 受控运行：这是对照实验的前置条件，不是产品结论。
func TestHarnessPlumbing(t *testing.T) {
	requireCLI(t, "codex", "codex")
	requireCLI(t, "claude", "claude")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	harnesses, stub := buildHarnesses(t, ctx)

	task := TaskSet()[0]
	for _, harness := range harnesses {
		harness := harness
		t.Run(harness.Name(), func(t *testing.T) {
			root := t.TempDir()
			workspace := filepath.Join(root, "ws")
			if err := os.MkdirAll(workspace, 0o755); err != nil {
				t.Fatalf("create workspace: %v", err)
			}
			if preparer, ok := harness.(interface{ WriteConfig(string) error }); ok {
				if err := preparer.WriteConfig(workspace); err != nil {
					t.Fatalf("write config: %v", err)
				}
			}
			result, err := harness.Run(ctx, HarnessRun{
				Workspace: workspace, SnapshotRoot: root,
				Task: task.Prompt(), Timeout: interopRunTimeout,
			})
			if err != nil {
				t.Fatalf("run harness: %v", err)
			}
			t.Logf("%s 退出码=%d 事件=%v 用量=%+v 补充=%v", harness.Name(), result.ExitCode, result.Events, result.Usage, result.Extra)
			if result.ExitCode != 0 {
				t.Errorf("%s 期望退出码 0，实际 %d；stderr=%s；结果行=%s", harness.Name(), result.ExitCode,
					truncate(result.Stderr, 600), truncate(lastResultLine(result.Stdout), 1200))
			}
		})
	}
	if len(stub.Requests()) == 0 {
		t.Errorf("桩模型没有收到任何请求，对照实验前提不成立")
	}
}

// TestControlledExperiment 在同一组任务上对照 Cairn 原生、Codex 与
// Claude Code 三侧外壳：成功率、工作区副作用、越界行为、审批往返、token
// 用量与墙钟耗时。结果写入忽略目录，供本地报告引用。
func TestControlledExperiment(t *testing.T) {
	requireCLI(t, "codex", "codex")
	requireCLI(t, "claude", "claude")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	harnesses, stub := buildHarnesses(t, ctx)
	tasks := TaskSet()
	if len(tasks) < 5 || len(tasks) > 10 {
		t.Fatalf("验收要求任务数落在 5 到 10 之间，当前 %d", len(tasks))
	}

	outcomes, err := RunExperiment(ctx, harnesses, tasks, ExperimentOptions{
		Root: t.TempDir(), Timeout: interopRunTimeout,
	})
	if err != nil {
		t.Fatalf("run experiment: %v", err)
	}

	t.Logf("对照实验结果:\n%s", RenderExperiments(outcomes))
	for _, line := range SummarizeByHarness(outcomes) {
		t.Logf("%s", line)
	}
	for _, outcome := range outcomes {
		t.Logf("任务 %s / 外壳 %s：退出码 %d，改动文件 %v，审批往返 %d，用量 %+v，说明 %v",
			outcome.TaskID, outcome.Harness, outcome.ExitCode, outcome.ChangedFiles,
			outcome.ApprovalRoundTrips, outcome.Usage, outcome.Notes)
	}

	writeExperimentArtifacts(t, outcomes, stub.Requests())
}

// writeExperimentArtifacts 把实验结果写入忽略目录，供本地报告引用。
func writeExperimentArtifacts(t *testing.T, outcomes []ExperimentOutcome, requests []StubRequest) {
	t.Helper()
	directory := filepath.Join(moduleRoot(t), ".vidt", "interop")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Logf("write experiment artifacts: %v", err)
		return
	}
	payload := map[string]any{
		"outcomes": outcomes,
		"requests": requests,
		"summary":  SummarizeByHarness(outcomes),
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Logf("encode experiment artifacts: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(directory, "experiment.json"), encoded, 0o644); err != nil {
		t.Logf("write experiment artifacts: %v", err)
		return
	}
	report := "# 对照实验原始结果\n\n```\n" + RenderExperiments(outcomes) + "\n```\n\n"
	for _, line := range SummarizeByHarness(outcomes) {
		report += "- " + line + "\n"
	}
	if err := os.WriteFile(filepath.Join(directory, "experiment.md"), []byte(report), 0o644); err != nil {
		t.Logf("write experiment artifacts: %v", err)
	}
}
