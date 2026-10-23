package tui

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func latestGit(t *testing.T, root string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", root}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %s %v", args, output, err)
	}
}

func TestLatestDiffDispatchIsAsyncAndRejectsForeignGitEnvironment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	latestGit(t, root, "init", "-q")
	if err := os.WriteFile(filepath.Join(root, "local.txt"), []byte("local"), 0600); err != nil {
		t.Fatal(err)
	}
	console := NewRunConsole(context.Background(), RunConsoleInput{Workspace: root})
	defer console.rootCancel()
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "foreign.git"))
	handled, command := console.runCommand("/diff")
	if !handled || command == nil {
		t.Fatal("/diff 必须返回后台查询命令，不能在 Update 中执行 Git")
	}
	model, _ := console.Update(command())
	console = model.(RunConsole)
	if text := strings.Join(console.lines, "\n"); !strings.Contains(text, "local.txt") || strings.Contains(text, "工作区干净") {
		t.Fatalf("查询被宿主 Git 环境重定向: %s", text)
	}
}

func TestLatestDiffIgnoresOldRequestAndFsmonitor(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git unavailable")
	}
	root := t.TempDir()
	latestGit(t, root, "init", "-q")
	script := filepath.Join(root, ".git", "review-fsmonitor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf invoked > .git/fsmonitor-invoked\nprintf '\\000'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	latestGit(t, root, "config", "core.fsmonitor", script)
	if err := os.WriteFile(filepath.Join(root, "old.txt"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	console := NewRunConsole(context.Background(), RunConsoleInput{Workspace: root})
	defer console.rootCancel()
	_, first := console.runCommand("/diff")
	if first == nil {
		t.Fatal("查询必须异步")
	}
	old := first()
	if _, err := os.Stat(filepath.Join(root, ".git", "fsmonitor-invoked")); !os.IsNotExist(err) {
		t.Fatalf("只读查询执行了 fsmonitor 回调: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	_, second := console.runCommand("/diff")
	if second == nil {
		t.Fatal("查询必须异步")
	}
	before := strings.Join(console.lines, "\n")
	model, _ := console.Update(old)
	console = model.(RunConsole)
	if strings.Join(console.lines, "\n") != before {
		t.Fatal("过期 Git 回执写入了界面")
	}
	model, _ = console.Update(second())
	console = model.(RunConsole)
	if text := strings.Join(console.lines, "\n"); !strings.Contains(text, "new.txt") {
		t.Fatalf("新查询未展示: %s", text)
	}
}
