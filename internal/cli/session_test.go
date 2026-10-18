// session_test 覆盖会话 CLI 入口的用法校验与只读投影：list/show/tree 不写
// 工作区，fork/continue 缺 --from 时失败关闭。
//
// Author: fxbin
package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestSessionCommandUsage 锁定子命令分派与必填参数校验。
func TestSessionCommandUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "无子命令", args: []string{"session"}, want: "session requires"},
		{name: "未知子命令", args: []string{"session", "nope"}, want: "session requires"},
		{name: "list 多余参数", args: []string{"session", "list", "extra"}, want: "does not accept positional"},
		{name: "show 缺 run", args: []string{"session", "show"}, want: "requires a run ID"},
		{name: "tree 多余参数", args: []string{"session", "tree", "extra"}, want: "does not accept positional"},
		{name: "fork 缺 from", args: []string{"session", "fork", "task"}, want: "requires --from"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
			code := Command{Stdout: stdout, Stderr: stderr}.Run(context.Background(), test.args)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d; stderr=%s", code, ExitUsage, stderr.String())
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr = %q, want substring %q", stderr.String(), test.want)
			}
		})
	}
}

// TestSessionListWithoutJournalIsEmpty 锁定无日志时 list 返回空而不创建状态。
func TestSessionListWithoutJournalIsEmpty(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := Command{Stdout: stdout, Stderr: stderr}.Run(context.Background(), []string{
		"session", "list", "--workspace", workspace, "--state-dir", stateDir,
	})
	if code != ExitOK {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want empty", stdout.String())
	}
}

// TestSessionTreeWithoutJournalIsEmpty 锁定无日志时 tree 输出暂无会话。
func TestSessionTreeWithoutJournalIsEmpty(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	writeCapabilityConfig(t, workspace)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := Command{Stdout: stdout, Stderr: stderr}.Run(context.Background(), []string{
		"session", "tree", "--workspace", workspace, "--state-dir", stateDir,
	})
	if code != ExitOK {
		t.Fatalf("exit = %d; stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "暂无会话") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}
