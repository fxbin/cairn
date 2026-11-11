package tools

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gitReadTestWorkspace 创建临时 git 仓库并返回根目录。
func gitReadTestWorkspace(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		command := exec.Command("git", args...)
		command.Dir = root
		command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
		if out, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init")
	run("config", "user.email", "test@example.com")
	run("config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "a.txt")
	run("commit", "-m", "init")
	return root
}

// TestGitStatusReportsCleanThenDirty 覆盖干净与脏工作区两种状态。
func TestGitStatusReportsCleanThenDirty(t *testing.T) {
	root := gitReadTestWorkspace(t)
	workspace, err := NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tool := GitStatus{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if !result.OK {
		t.Fatalf("clean status should succeed: %s", result.Preview)
	}
	var status gitStatusResult
	if err := json.Unmarshal(result.Data, &status); err != nil {
		t.Fatal(err)
	}
	if !status.Clean || len(status.Entries) != 0 {
		t.Fatalf("expected clean status, got %+v", status)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result = tool.Execute(context.Background(), json.RawMessage(`{}`))
	if !result.OK {
		t.Fatalf("dirty status should succeed: %s", result.Preview)
	}
	if err := json.Unmarshal(result.Data, &status); err != nil {
		t.Fatal(err)
	}
	if status.Clean || len(status.Entries) == 0 {
		t.Fatalf("expected dirty status, got %+v", status)
	}
}

// TestGitDiffUnstagedAndStaged 锁定 staged 过滤与统一 diff 输出。
func TestGitDiffUnstagedAndStaged(t *testing.T) {
	root := gitReadTestWorkspace(t)
	workspace, err := NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello\nworld\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := GitDiff{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{}`))
	if !result.OK {
		t.Fatalf("unstaged diff should succeed: %s", result.Preview)
	}
	var diff gitDiffResult
	if err := json.Unmarshal(result.Data, &diff); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diff.Diff, "world") {
		t.Fatalf("expected unstaged diff to contain change, got %q", diff.Diff)
	}
	result = tool.Execute(context.Background(), json.RawMessage(`{"staged":true}`))
	if !result.OK {
		t.Fatalf("staged diff should succeed: %s", result.Preview)
	}
	if err := json.Unmarshal(result.Data, &diff); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(diff.Diff) != "" {
		t.Fatalf("expected empty staged diff, got %q", diff.Diff)
	}
}

// TestGitDiffRejectsEscapePaths 保证路径参数不能带 .. 逃逸。
func TestGitDiffRejectsEscapePaths(t *testing.T) {
	root := gitReadTestWorkspace(t)
	workspace, err := NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tool := GitDiff{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{"paths":["../secret"]}`))
	if result.OK || result.Code != "invalid_arguments" {
		t.Fatalf("expected invalid_arguments, got %+v", result)
	}
}
