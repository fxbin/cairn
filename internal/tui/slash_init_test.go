// slash_init_test 锁定 /init：新建骨架、已存在不覆盖、路径解析失败关闭。
//
// Author: fxbin
package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunInitCreatesAGENTSOnce 锁定首次创建与二次拒绝覆盖。
func TestRunInitCreatesAGENTSOnce(t *testing.T) {
	root := t.TempDir()
	result, err := runInitCommand(root)
	if err != nil || !result.Created || result.Existing {
		t.Fatalf("首次应创建: %+v %v", result, err)
	}
	path := filepath.Join(root, "AGENTS.md")
	raw, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(raw), "项目协作规约") {
		t.Fatalf("骨架内容不符: %v %q", err, raw)
	}
	second, err := runInitCommand(root)
	if err != nil || !second.Existing || second.Created {
		t.Fatalf("二次应标记已存在: %+v %v", second, err)
	}
	raw2, err := os.ReadFile(path)
	if err != nil || string(raw2) != string(raw) {
		t.Fatal("已存在文件不得被覆盖")
	}
}

// TestRunInitUsesCurrentDirectory 锁定空工作区回退到当前目录。
func TestRunInitUsesCurrentDirectory(t *testing.T) {
	root := t.TempDir()
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	result, err := runInitCommand("")
	if err != nil || !result.Created {
		t.Fatalf("空工作区应在当前目录创建: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, "AGENTS.md")); err != nil {
		t.Fatalf("AGENTS.md 未创建: %v", err)
	}
}
