package interop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCapabilityMatrix 对三侧外壳执行能力矩阵与边界场景探测。外部 CLI
// 不在机器上时按格跳过：矩阵的结论必须区分"实测"与"不可得"。
func TestCapabilityMatrix(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	harnesses, stub := buildHarnesses(t, ctx)

	matrix := ProbeMatrix(ctx, harnesses, stub)
	rendered := matrix.Render(matrix.SortedCLIs())
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
	rendered := matrix.Render(matrix.SortedCLIs())
	if err := os.WriteFile(filepath.Join(directory, "matrix.md"), []byte(rendered), 0o644); err != nil {
		t.Logf("write matrix artifacts: %v", err)
	}
}
