package tools

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// TestWorkspaceReadBoundedFilePreservesBytesAndLimit 直接读取普通文件时保持
// 原始字节与显式容量边界，不把工作区其他工具的默认读取额度混入该合同。
func TestWorkspaceReadBoundedFilePreservesBytesAndLimit(t *testing.T) {
	root := t.TempDir()
	content := []byte{0, 255, 1}
	if err := os.WriteFile(filepath.Join(root, "binary.dat"), content, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "empty.txt"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := workspace.ReadBoundedFile("binary.dat", int64(len(content)))
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("raw bounded read=%v err=%v", got, err)
	}
	if got, err = workspace.ReadBoundedFile("empty.txt", 0); err != nil || len(got) != 0 {
		t.Fatalf("empty bounded read=%v err=%v", got, err)
	}
	if got, err = workspace.ReadBoundedFile("binary.dat", int64(len(content)-1)); err == nil || err.Error() != "skill file is not a bounded regular file" || got != nil {
		t.Fatalf("oversized read=%v err=%v", got, err)
	}
}

// TestWorkspaceReadBoundedFilePreservesPathBoundary 保留显式忽略、敏感路径、
// 越界链接和目录拒绝；工作区内普通文件别名仍按原语义解析后读取。
func TestWorkspaceReadBoundedFilePreservesPathBoundary(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	for _, directory := range []string{"blocked", "ordinary"} {
		if err := os.Mkdir(filepath.Join(root, directory), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"visible.txt", ".env", "blocked/hidden.txt", "ordinary/inside.txt"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte("bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "outside.txt"), []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []struct{ name, target string }{
		{"inside-link", "ordinary/inside.txt"},
		{"outside-link", filepath.Join(outside, "outside.txt")},
		{"sensitive-alias", ".env"},
		{"ignored-alias", "blocked/hidden.txt"},
	} {
		if err := os.Symlink(link.target, filepath.Join(root, link.name)); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := NewWorkspace(root, []string{"blocked"}, 64, 100, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"visible.txt", "inside-link"} {
		content, err := workspace.ReadBoundedFile(path, 64)
		if err != nil || string(content) != "bytes" {
			t.Fatalf("allowed %q: content=%q err=%v", path, content, err)
		}
	}
	for _, test := range []struct{ path, message string }{
		{"../outside.txt", "skill path is not allowed"},
		{filepath.Join(outside, "outside.txt"), "skill path is not allowed"},
		{"outside-link", "skill path is not allowed"},
		{".env", "skill path is not allowed"},
		{"sensitive-alias", "skill path is not allowed"},
		{"blocked/hidden.txt", "skill path is not allowed"},
		{"ignored-alias", "skill path is not allowed"},
		{"ordinary", "skill file is not a bounded regular file"},
		{"missing.txt", "skill file is not a bounded regular file"},
	} {
		t.Run(test.path, func(t *testing.T) {
			content, err := workspace.ReadBoundedFile(test.path, 64)
			if err == nil || err.Error() != test.message || content != nil {
				t.Fatalf("content=%q err=%v want=%s", content, err, test.message)
			}
		})
	}
}

func TestWorkspaceReadBoundedFileRejectsUnavailableRoot(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, nil, 64, 100, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(root); err != nil {
		t.Fatal(err)
	}
	content, err := workspace.ReadBoundedFile("file.txt", 64)
	if err == nil || err.Error() != "open skill root failed" || content != nil {
		t.Fatalf("content=%q err=%v", content, err)
	}
}
