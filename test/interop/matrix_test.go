//go:build interop

// 本文件属于 interop 标签档：完整能力矩阵需要真实外部 CLI 进程。
package interop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCapabilityMatrix 对三侧外壳执行能力矩阵与边界场景探测。外部 CLI
// 不在机器上时按列跳过：该列在矩阵里被显式标为"不可得"并写明原因，
// 而不是让整份矩阵失败或留空。
func TestCapabilityMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

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

	matrix := Matrix{}
	// Cairn 原生侧始终可测：它只需要本仓库的二进制与本地桩模型。
	matrix.AddAll(ProbeHarness(ctx, NewCairnHarness(binary, filepath.Join(root, "cairn"), stub.BaseURL()), stub).Observations...)

	// 外部 CLI 逐列处理：缺失时该列跳过并标注不可得。
	for _, column := range []struct {
		cli     string
		command string
		build   func(t *testing.T, root string, baseURL string) Harness
	}{
		{
			cli: "codex", command: "codex",
			build: func(t *testing.T, root string, baseURL string) Harness {
				t.Helper()
				harness, buildErr := NewCodexHarness(filepath.Join(root, "codex-home"), baseURL)
				if buildErr != nil {
					t.Fatalf("setup codex harness: %v", buildErr)
				}
				return harness
			},
		},
		{
			cli: "claude", command: "claude",
			build: func(t *testing.T, root string, baseURL string) Harness {
				t.Helper()
				harness, buildErr := NewClaudeHarness(filepath.Join(root, "claude-config"), baseURL)
				if buildErr != nil {
					t.Fatalf("setup claude harness: %v", buildErr)
				}
				return harness
			},
		},
	} {
		column := column
		t.Run(column.cli, func(t *testing.T) {
			requireCLI(t, column.cli, column.command)
			matrix.AddAll(ProbeHarness(ctx, column.build(t, root, stub.BaseURL()), stub).Observations...)
		})
		if _, err := lookPath(column.command); err != nil {
			matrix.AddAll(UnavailableObservations(column.cli, "外部 CLI 未安装，本机不可得")...)
			t.Logf("外部 CLI %s 不可用，该列标记为不可得", column.cli)
		}
	}

	rendered := matrix.Render([]string{"cairn-native", "codex", "claude"})
	t.Logf("兼容性矩阵:\n%s", rendered)

	for _, observation := range matrix.Observations {
		if observation.Status == StatusUnavailable && observation.Reason == "" {
			t.Errorf("矩阵格 %s/%s 标为不可得但没有写明原因", observation.CLI, observation.Row())
		}
		if observation.Status != StatusMeasured && observation.Status != StatusSurface {
			continue
		}
		if observation.Summary == "" {
			t.Errorf("矩阵格 %s/%s 标为 %s 但没有给出结论", observation.CLI, observation.Row(), observation.Status)
		}
	}

	writeMatrixArtifacts(t, matrix)
}

// writeMatrixArtifacts 把矩阵渲染结果写入忽略目录，供本地报告引用。
func writeMatrixArtifacts(t *testing.T, matrix Matrix) {
	t.Helper()
	directory := filepath.Join(moduleRoot(t), ".vidt", "interop")
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Logf("write matrix artifacts: %v", err)
		return
	}
	rendered := matrix.Render([]string{"cairn-native", "codex", "claude"})
	if err := os.WriteFile(filepath.Join(directory, "matrix.md"), []byte(rendered), 0o644); err != nil {
		t.Logf("write matrix artifacts: %v", err)
	}
}
