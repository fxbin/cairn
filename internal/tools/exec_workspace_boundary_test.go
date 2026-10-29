//go:build darwin || linux

// exec_workspace_boundary_test.go 锁定 exec_command 的工作区写边界：
// 写入工作区父目录必须被显式拒绝，合法的工作区内写入与工作区外只读
// 引用不受影响。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// execBoundaryFixture 是一次越界写实验的目录布局：工作区位于 root 之下，
// 工作区父目录就是 root，与受控对照实验的快照根一致。
type execBoundaryFixture struct {
	// Root 是工作区的父目录。
	Root string
	// Workspace 是工作区视图。
	Workspace Workspace
}

// newExecBoundaryFixture 在临时根下建立 root/ws 布局并返回可用的工作区。
func newExecBoundaryFixture(t *testing.T) execBoundaryFixture {
	t.Helper()
	root := t.TempDir()
	workspaceRoot := filepath.Join(root, "ws")
	if err := os.MkdirAll(workspaceRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(workspaceRoot, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	return execBoundaryFixture{Root: root, Workspace: workspace}
}

// execCommandOutputOf 从失败结果里取出结构化输出，断言失败语义而非只断言
// "文件没出现"。
func execCommandOutputOf(t *testing.T, result Result) execCommandOutput {
	t.Helper()
	var output execCommandOutput
	if err := json.Unmarshal(result.Data, &output); err != nil {
		t.Fatalf("失败结果没有有效结构化输出: %v, data=%s", err, result.Data)
	}
	return output
}

// TestExecCommandRejectsWriteToWorkspaceParent 锁定越界写必须被显式拒绝：
// 命令以稳定的非零退出码失败并给出拒绝证据，而不是"没写成功"却报告成功。
func TestExecCommandRejectsWriteToWorkspaceParent(t *testing.T) {
	if !OSSandboxAvailable() {
		t.Skip("当前平台没有 OS 沙箱，host 后端本就没有可强制的写边界")
	}
	fixture := newExecBoundaryFixture(t)
	tool := ExecCommand{Workspace: fixture.Workspace, HostConfined: true, HostWrite: true}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/sh","-c","printf escaped > ../escaped.txt"]}`))
	if result.OK {
		t.Fatalf("越界写被报告为成功: %#v", result)
	}
	if result.Code != "nonzero_exit" {
		t.Fatalf("失败语义不稳定: code=%s preview=%s", result.Code, result.Preview)
	}
	output := execCommandOutputOf(t, result)
	if output.ExitCode != execBoundaryDeniedExitCode {
		t.Fatalf("退出码不稳定: exit=%d stderr=%s", output.ExitCode, output.Stderr)
	}
	if !strings.Contains(strings.ToLower(output.Stderr), "not permitted") && !strings.Contains(strings.ToLower(output.Stderr), "no such file") {
		t.Fatalf("失败没有给出拒绝证据: stderr=%s", output.Stderr)
	}
	if _, err := os.Stat(filepath.Join(fixture.Root, "escaped.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("工作区父目录被写入: %v", err)
	}
}

// TestExecCommandAllowsWriteInsideWorkspace 确认收紧没有误伤工作区内的写入，
// 包括构建产物目录这类工作区内的子目录。
func TestExecCommandAllowsWriteInsideWorkspace(t *testing.T) {
	if !OSSandboxAvailable() {
		t.Skip("当前平台没有 OS 沙箱，跳过沙箱写边界验证")
	}
	fixture := newExecBoundaryFixture(t)
	tool := ExecCommand{Workspace: fixture.Workspace, HostConfined: true, HostWrite: true}
	result := tool.Execute(context.Background(), json.RawMessage(`{"argv":["/bin/sh","-c","mkdir -p dist && printf artifact > dist/out.bin && printf plain > created.txt"]}`))
	if !result.OK {
		t.Fatalf("工作区内写入被误伤: %#v", result)
	}
	for _, name := range []string{"created.txt", filepath.Join("dist", "out.bin")} {
		if _, err := os.Stat(filepath.Join(fixture.Workspace.Root, name)); err != nil {
			t.Fatalf("工作区内文件缺失 %s: %v", name, err)
		}
	}
}

// TestExecCommandAllowsReadOutsideWorkspace 确认收紧只作用于写：工作区外的
// 只读引用（依赖、构建产物目录）仍然可用。
func TestExecCommandAllowsReadOutsideWorkspace(t *testing.T) {
	if !OSSandboxAvailable() {
		t.Skip("当前平台没有 OS 沙箱，跳过沙箱写边界验证")
	}
	fixture := newExecBoundaryFixture(t)
	dependency := filepath.Join(fixture.Root, "dependency.txt")
	if err := os.WriteFile(dependency, []byte("outside-read-payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := ExecCommand{Workspace: fixture.Workspace, HostConfined: true, HostWrite: true}
	arguments, err := json.Marshal(ExecArguments{
		Argv:           []string{"/bin/sh", "-c", "cat " + dependency},
		TimeoutMS:      defaultExecTimeoutMS,
		MaxOutputBytes: defaultMaxOutputBytes,
	})
	if err != nil {
		t.Fatal(err)
	}
	result := tool.Execute(context.Background(), arguments)
	if !result.OK {
		t.Fatalf("工作区外只读引用被误伤: %#v", result)
	}
	if !strings.Contains(result.Preview, "outside-read-payload") {
		t.Fatalf("工作区外只读引用没有返回内容: %s", result.Preview)
	}
}

// execBoundaryDeniedExitCode 是重定向失败时 POSIX shell 的稳定退出码：沙箱
// 拒绝写入时，shell 以 1 退出并报告 "Operation not permitted"。
const execBoundaryDeniedExitCode = 1
