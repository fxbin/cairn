//go:build linux

// ossandbox_linux_test.go 锁定 bwrap 挂载参数合同：基础目录固定挂载，根级
// 加载器别名目录按宿主存在性补齐。该参数曾导致 ubuntu-26.04 runner 上
// execvp /bin/sh 返回 ENOENT（沙箱根缺少 /lib64，动态链接的 dash 找不到
// PT_INTERP），因此用参数层测试固化「/lib 与 /lib64 必须同时出现」。
// Author: fxbin
package tools

import (
	"strings"
	"testing"
)

// mountPairs 把扁平参数还原为 (源, 目标) 对，非挂载参数忽略。
func mountPairs(args []string) [][2]string {
	pairs := make([][2]string, 0, len(args)/3)
	for index := 0; index+2 < len(args); index++ {
		if args[index] != "--ro-bind" {
			continue
		}
		pairs = append(pairs, [2]string{args[index+1], args[index+2]})
	}
	return pairs
}

// hasMount 报告参数中是否含指定源到同路径目标的只读挂载。
func hasMount(args []string, path string) bool {
	for _, pair := range mountPairs(args) {
		if pair[0] == path && pair[1] == path {
			return true
		}
	}
	return false
}

// TestSandboxMountArgsKeepsBaseDirectories 锁定基础目录固定挂载，不会因为
// 加载器别名是否存在于宿主而改变。
func TestSandboxMountArgsKeepsBaseDirectories(t *testing.T) {
	args := sandboxMountArgs(func(string) bool { return false })
	for _, path := range []string{"/usr", "/bin", "/sbin", "/lib", "/etc"} {
		if !hasMount(args, path) {
			t.Fatalf("base mount %s missing from %v", path, args)
		}
	}
	if len(mountPairs(args)) != 5 {
		t.Fatalf("unexpected extra mounts: %v", args)
	}
}

// TestSandboxMountArgsAddsLoaderAliasWhenPresent 锁定宿主存在 /lib64 时必须
// 补齐该根级别名：dash 的 PT_INTERP 是 /lib64/ld-linux-x86-64.so.2，沙箱根是
// 空 tmpfs，缺少该挂载会让 execvp 返回 ENOENT。
func TestSandboxMountArgsAddsLoaderAliasWhenPresent(t *testing.T) {
	args := sandboxMountArgs(func(path string) bool { return path == "/lib64" })
	if !hasMount(args, "/lib64") {
		t.Fatalf("loader alias mount missing: %v", args)
	}
	if !hasMount(args, "/lib") {
		t.Fatalf("loader alias requires /lib mount to resolve its relative symlink: %v", args)
	}
}

// TestSandboxMountArgsSkipsMissingLoaderAlias 锁定别名缺失时跳过：精简镜像
// 没有 /lib64，无条件挂载会让 bwrap 因绑定源不存在而启动失败。
func TestSandboxMountArgsSkipsMissingLoaderAlias(t *testing.T) {
	args := sandboxMountArgs(func(string) bool { return false })
	for _, path := range sandboxLoaderAliasPaths {
		if hasMount(args, path) {
			t.Fatalf("missing loader alias %s must not be mounted: %v", path, args)
		}
	}
}

// TestConfineArgvIncludesLoaderAliasMounts 锁定 ConfineArgv 与参数构造一致：
// 挂载段出现在 --proc 之前，工作区仍是唯一可写绑定。
func TestConfineArgvIncludesLoaderAliasMounts(t *testing.T) {
	argv, cleanup, err := ConfineArgv(SeatbeltOptions{WorkspaceRoot: t.TempDir()}, []string{"/bin/sh", "-c", "true"})
	if cleanup != nil {
		defer func() { _ = cleanup() }()
	}
	if err != nil {
		if !strings.Contains(err.Error(), "bwrap") {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Skip("bubblewrap unavailable on this host")
	}
	joined := strings.Join(argv, " ")
	procIndex := strings.Index(joined, "--proc /proc")
	usrIndex := strings.Index(joined, "--ro-bind /usr /usr")
	if usrIndex < 0 || procIndex < 0 || usrIndex > procIndex {
		t.Fatalf("mount arguments must precede --proc: %v", argv)
	}
	writable := 0
	for index := 0; index+2 < len(argv); index++ {
		if argv[index] == "--bind" {
			writable++
		}
	}
	if writable != 1 {
		t.Fatalf("workspace must stay the only writable bind: %v", argv)
	}
}
