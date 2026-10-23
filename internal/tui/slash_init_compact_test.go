// slash_init_compact_test 锁定 /init 写文件与 /compact 只读投影行为。
//
// Author: fxbin
package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// TestInitProjectInstructionsWritesSkeleton 锁定首次写入与存在时拒绝覆盖。
func TestInitProjectInstructionsWritesSkeleton(t *testing.T) {
	root := t.TempDir()
	console := NewRunConsole(t.Context(), RunConsoleInput{Workspace: root, Config: config.Config{}})
	handled, _ := console.initProjectInstructions("")
	if !handled {
		t.Fatal("/init 应被命令层消费")
	}
	target := filepath.Join(root, "AGENTS.md")
	raw, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(raw), "# 项目规约") {
		t.Fatalf("未写入骨架: %v %q", err, raw)
	}
	// 二次调用不覆盖
	if err := os.WriteFile(target, []byte("# keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	console.lines = nil
	console.initProjectInstructions("")
	if got, _ := os.ReadFile(target); string(got) != "# keep" {
		t.Fatalf("已存在时不应覆盖: %q", got)
	}
	console.lines = nil
	console.initProjectInstructions("--force")
	if got, _ := os.ReadFile(target); !strings.Contains(string(got), "# 项目规约") {
		t.Fatalf("--force 应覆盖: %q", got)
	}
}

// TestAppendCompactStatusProjectsUsageAndCheckpoint 锁定用量与检查点投影。
func TestAppendCompactStatusProjectsUsageAndCheckpoint(t *testing.T) {
	console := NewRunConsole(t.Context(), RunConsoleInput{Workspace: t.TempDir(), Config: config.Config{}})
	console.promptTokens, console.outputTokens = 120, 30
	console.appendCompactStatus()
	joined := strings.Join(console.lines, "\n")
	if !strings.Contains(joined, "上下文用量") || !strings.Contains(joined, "in 120 / out 30") {
		t.Fatalf("用量行缺失: %q", joined)
	}
	if !strings.Contains(joined, "尚无") {
		t.Fatalf("无检查点时应提示尚无: %q", joined)
	}
	console.applyEvent(domain.Event{
		Type:       domain.EventContextCompacted,
		OccurredAt: time.Unix(10, 0).UTC(),
		Payload:    []byte(`{"through_seq":7,"estimated_units_before":900,"estimated_units_after":200,"strategy":"deterministic_journal_checkpoint_v4","trigger":"prompt_estimate_over_threshold"}`),
	})
	console.lines = nil
	console.appendCompactStatus()
	joined = strings.Join(console.lines, "\n")
	if !strings.Contains(joined, "through_seq=7") || !strings.Contains(joined, "900 → 200") {
		t.Fatalf("检查点投影缺失: %q", joined)
	}
	_ = app.RunResult{}
}
