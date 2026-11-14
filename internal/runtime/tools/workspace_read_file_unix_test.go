//go:build darwin || linux

package tools

import (
	"path/filepath"
	"syscall"
	"testing"
)

// TestWorkspaceReadBoundedFileRejectsFIFO 未连接写端的 FIFO 不能被当作普通
// 文件打开；必须在读取前拒绝，避免内容容量限制掩盖阻塞和特殊文件访问。
func TestWorkspaceReadBoundedFileRejectsFIFO(t *testing.T) {
	root := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 64, 100, 1024)
	if err != nil {
		t.Fatal(err)
	}
	content, err := workspace.ReadBoundedFile("pipe", 64)
	if err == nil || err.Error() != "skill file is not a bounded regular file" || content != nil {
		t.Fatalf("content=%q err=%v", content, err)
	}
}
