package tui

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestLatestDiffOutputLimitAndSafeNULPaths(t *testing.T) {
	output := new(gitStatusOutput)
	_, err := io.Copy(output, io.LimitReader(strings.NewReader(strings.Repeat("x", gitStatusOutputLimit+1)), int64(gitStatusOutputLimit+1)))
	if !errors.Is(err, io.ErrShortBuffer) || output.buffer.Len() != gitStatusOutputLimit {
		t.Fatalf("读取超限: bytes=%d err=%v", output.buffer.Len(), err)
	}
	lines, err := gitStatusLines("R  new\n\x1b[31m.txt\x00old.txt\x00?? other.txt\x00")
	text := strings.Join(lines, "\n")
	if err != nil || !strings.Contains(text, "变更 2 项") || !strings.Contains(text, `\n\x1b[31m.txt`) || strings.Contains(text, "\x1b") {
		t.Fatalf("路径未安全展示或重命名计数错误: %q %v", text, err)
	}
	if _, err := gitStatusLines("R  missing-source\x00"); err == nil {
		t.Fatal("不完整记录不应伪装成有效列表")
	}
}

func TestLatestDiffCancelledContextReturnsFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	console := NewRunConsole(ctx, RunConsoleInput{Workspace: t.TempDir()})
	defer console.rootCancel()
	_, command := console.runCommand("/diff")
	cancel()
	result, ok := command().(gitStatusMsg)
	if !ok || result.err == nil || len(result.lines) != 0 {
		t.Fatalf("取消被误报为干净工作区: %+v", result)
	}
}
