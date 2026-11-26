//go:build darwin || linux

// sandbox_temp_exec_test.go 锁定自定义临时目录在真实 OS 沙箱下的行为：子进程继承的
// TMPDIR 位于系统临时根之外时可以写，其他工作区外位置仍然不可写；过宽的 TMPDIR，以及
// 只读档下覆盖工作区的 TMPDIR，让命令在启动前失败关闭。
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// platformFixedTempRootsForTest 是本平台的固定临时根，只用于判断检出位置是否让用例空转。
func platformFixedTempRootsForTest() []string {
	if runtime.GOOS == "darwin" {
		return []string{"/private/tmp", "/tmp", "/var/folders", "/private/var/folders"}
	}
	return []string{"/tmp", "/var/tmp"}
}

// requireCheckoutOutsideTempRoots 在检出位于系统临时根之下时跳过用例：那里构造不出
// 固定根之外的目录，继续跑只会让断言空转通过。
func requireCheckoutOutsideTempRoots(t *testing.T) {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	if pathWithinAny(canonical, platformFixedTempRootsForTest()) || pathWithinAny(canonical, sandboxUnbindableRootsForTest()) {
		t.Skip("检出位于系统临时根或沙箱保留、自建路径之下，无法构造可放行的固定根之外的临时目录")
	}
}

// sandboxUnbindableRootsForTest 是本平台沙箱保留或自建、无法构造临时根场景的路径：Linux
// 的 bubblewrap 保留 /workspace 并自建 /dev，开发容器常把检出放在 /workspace 之下。
func sandboxUnbindableRootsForTest() []string {
	if runtime.GOOS == "linux" {
		return []string{"/workspace", "/dev", "/proc", "/sys"}
	}
	return nil
}

// newScratchDirOutsideTempRoots 在包目录下建立一个位于系统临时根之外的目录，返回绝对
// 路径，测试结束时清理。与 newNonTempBoundaryFixture 同一做法：这是测试里稳定落在临时
// 根之外、又不需要触碰用户其他目录的位置。
func newScratchDirOutsideTempRoots(t *testing.T, pattern string) string {
	t.Helper()
	requireCheckoutOutsideTempRoots(t)
	path, err := os.MkdirTemp(".", pattern)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(path) })
	// TMPDIR 必须是绝对路径：沙箱命令换到工作目录执行，相对的临时目录在那里解析不到。
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return absolute
}

// execBoundaryArgv 把 argv 编码成 exec_command 的参数。
func execBoundaryArgv(t *testing.T, argv ...string) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(struct {
		Argv []string `json:"argv"`
	}{Argv: argv})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestExecCommandAllowsWriteToInheritedCustomTempDir 锁定本次修复的目标行为：环境白名单
// 把位于系统临时根之外的 TMPDIR 传给子进程后，子进程能写进去；同一层级下的其他目录
// 不因此变得可写。
func TestExecCommandAllowsWriteToInheritedCustomTempDir(t *testing.T) {
	if !OSSandboxAvailable() {
		t.Skip("当前平台没有 OS 沙箱，跳过自定义临时目录验证")
	}
	fixture := newExecBoundaryFixture(t)
	custom := newScratchDirOutsideTempRoots(t, "custom-tmp-")
	other := newScratchDirOutsideTempRoots(t, "other-dir-")
	t.Setenv("TMPDIR", custom)
	tool := ExecCommand{
		Workspace: fixture.Workspace, HostConfined: true, HostWrite: true,
		EnvAllow: map[string]struct{}{"TMPDIR": {}},
	}

	result := tool.Execute(context.Background(), execBoundaryArgv(t, "/bin/sh", "-c", `printf ok > "$TMPDIR/probe.txt"`))
	if !result.OK {
		var output execCommandOutput
		_ = json.Unmarshal(result.Data, &output)
		t.Fatalf("继承自定义 TMPDIR 的写入被沙箱拒绝: code=%s stderr=%s", result.Code, output.Stderr)
	}
	content, err := os.ReadFile(filepath.Join(custom, "probe.txt"))
	if err != nil || string(content) != "ok" {
		t.Fatalf("自定义临时目录里的文件缺失或内容不符: %q %v", content, err)
	}

	escaped := filepath.Join(other, "escaped.txt")
	result = tool.Execute(context.Background(), execBoundaryArgv(t, "/bin/sh", "-c", "printf escaped > '"+escaped+"'"))
	if result.OK {
		t.Fatalf("临时目录之外的越界写被报告为成功: %#v", result)
	}
	if result.Code != "nonzero_exit" {
		t.Fatalf("失败语义不稳定: code=%s preview=%s", result.Code, result.Preview)
	}
	output := execCommandOutputOf(t, result)
	stderr := strings.ToLower(output.Stderr)
	if !strings.Contains(stderr, "not permitted") && !strings.Contains(stderr, "read-only file system") {
		t.Fatalf("失败没有给出拒绝证据: stderr=%s", output.Stderr)
	}
	if _, err := os.Stat(escaped); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("临时目录之外的目录被写入: %v", err)
	}
}

// TestExecCommandFailsClosedForTooBroadTempDir 锁定过宽的 TMPDIR 让命令在启动前失败：结果
// 带稳定错误码与可读原因，命令本身没有执行。
func TestExecCommandFailsClosedForTooBroadTempDir(t *testing.T) {
	if !OSSandboxAvailable() {
		t.Skip("当前平台没有 OS 沙箱，跳过过宽临时目录验证")
	}
	fixture := newExecBoundaryFixture(t)
	t.Setenv("TMPDIR", "/")
	assertCommandRefusedBeforeStart(t, fixture.Workspace, true, "too broad")
}

// TestExecCommandGovernsCoveringTempDirByWriteMode 锁定覆盖工作区的 TMPDIR 在真实沙箱下按
// 写入档位区分：只读档在启动前失败，可写档照常执行并经继承的 TMPDIR 写到工作区之外。
func TestExecCommandGovernsCoveringTempDirByWriteMode(t *testing.T) {
	if !OSSandboxAvailable() {
		t.Skip("当前平台没有 OS 沙箱，跳过覆盖工作区的临时目录验证")
	}
	requireCheckoutOutsideTempRoots(t)
	fixture := newNonTempBoundaryFixture(t)
	parent, err := filepath.Abs(fixture.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", parent)
	assertCommandRefusedBeforeStart(t, fixture.Workspace, false, "covers the workspace")
	workspace, err := filepath.Abs(fixture.Workspace.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TMPDIR", workspace)
	assertCommandRefusedBeforeStart(t, fixture.Workspace, false, "covers the workspace")

	t.Setenv("TMPDIR", parent)
	tool := ExecCommand{
		Workspace: fixture.Workspace, HostConfined: true, HostWrite: true,
		EnvAllow: map[string]struct{}{"TMPDIR": {}},
	}
	result := tool.Execute(context.Background(), execBoundaryArgv(t, "/bin/sh", "-c", `printf ok > "$TMPDIR/parent-probe.txt"`))
	if !result.OK {
		t.Fatalf("可写档下覆盖工作区的临时目录不应阻止命令: code=%s preview=%s", result.Code, result.Preview)
	}
	if content, err := os.ReadFile(filepath.Join(parent, "parent-probe.txt")); err != nil || string(content) != "ok" {
		t.Fatalf("命令应能经继承的 TMPDIR 写入工作区的上级目录: %q %v", content, err)
	}
}

// assertCommandRefusedBeforeStart 断言收紧后的宿主命令没有执行，并给出稳定错误码与原因。
func assertCommandRefusedBeforeStart(t *testing.T, workspace Workspace, hostWrite bool, reason string) {
	t.Helper()
	tool := ExecCommand{Workspace: workspace, HostConfined: true, HostWrite: hostWrite}
	result := tool.Execute(context.Background(), execBoundaryArgv(t, "/bin/sh", "-c", "printf ran > ran.txt"))
	if result.OK {
		t.Fatalf("命令不应执行: %#v", result)
	}
	if result.Code != "sandbox_unavailable" {
		t.Fatalf("失败语义不稳定: code=%s preview=%s", result.Code, result.Preview)
	}
	if !strings.Contains(result.Preview, reason) {
		t.Fatalf("失败应说明 %q: %s", reason, result.Preview)
	}
	if _, err := os.Stat(filepath.Join(workspace.Root, "ran.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("命令在失败前被执行了: %v", err)
	}
}
