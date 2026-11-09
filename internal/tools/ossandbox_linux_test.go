//go:build linux

// ossandbox_linux_test.go 锁定 bwrap 挂载参数合同：宿主根条目逐一只读映射、
// 临时目录根可写、工作区挂载后整体重挂只读。该参数曾导致 ubuntu-26.04
// runner 上 execvp /bin/sh 返回 ENOENT（沙箱根缺少 /lib64，动态链接的 dash
// 找不到 PT_INTERP），因此用参数层测试固化「根级条目必须完整映射」。
// Author: fxbin
package tools

import (
	"os"
	"strings"
	"testing"
)

// mountPairs 把扁平参数还原为 (源, 目标) 对，非绑定参数忽略。
func mountPairs(args []string) [][2]string {
	pairs := make([][2]string, 0, len(args)/3)
	for index := 0; index+2 < len(args); index++ {
		if args[index] != "--ro-bind" && args[index] != "--bind" {
			continue
		}
		pairs = append(pairs, [2]string{args[index+1], args[index+2]})
	}
	return pairs
}

// hasMount 报告参数中是否含指定源到同路径目标的绑定。
func hasMount(args []string, path string) bool {
	for _, pair := range mountPairs(args) {
		if pair[0] == path && pair[1] == path {
			return true
		}
	}
	return false
}

// fakeDirEntry 是 sandboxRootEntries 测试用的最小目录项。
type fakeDirEntry string

// Name 返回目录项名称。
func (entry fakeDirEntry) Name() string { return string(entry) }

// IsDir 报告该目录项是否为目录；测试不区分类型。
func (entry fakeDirEntry) IsDir() bool { return false }

// Type 返回目录项类型位；测试不区分类型。
func (entry fakeDirEntry) Type() os.FileMode { return 0 }

// Info 返回目录项元数据；测试不需要。
func (entry fakeDirEntry) Info() (os.FileInfo, error) { return nil, nil }

// TestSandboxRootEntriesSkipsSpecialEntries 锁定特殊条目由专用挂载或写规则
// 处理，普通条目按序全部进入只读映射。
func TestSandboxRootEntriesSkipsSpecialEntries(t *testing.T) {
	entries := []os.DirEntry{fakeDirEntry("bin"), fakeDirEntry("dev"), fakeDirEntry("lib64"), fakeDirEntry("proc"), fakeDirEntry("tmp"), fakeDirEntry("usr")}
	names, err := sandboxRootEntries(func(string) ([]os.DirEntry, error) { return entries, nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "bin,lib64,usr" {
		t.Fatalf("unexpected root entries: %v", names)
	}
}

// TestSandboxRootEntriesFailsClosed 锁定枚举失败必须失败关闭，不退化成可写
// 或空映射的沙箱。
func TestSandboxRootEntriesFailsClosed(t *testing.T) {
	if _, err := sandboxRootEntries(func(string) ([]os.DirEntry, error) {
		return nil, os.ErrPermission
	}); err == nil {
		t.Fatal("root enumeration failure must fail closed")
	}
}

// TestSandboxMountArgsBindsEveryRootEntryReadOnly 锁定根级条目完整只读映射：
// 缺少 /lib64 这类根级条目会让动态链接的解释器在执行期 ENOENT。
func TestSandboxMountArgsBindsEveryRootEntryReadOnly(t *testing.T) {
	entries := []string{"bin", "lib", "lib64", "sbin", "usr"}
	args := sandboxMountArgs(entries, func(string) bool { return false })
	for _, name := range entries {
		if !hasMount(args, "/"+name) {
			t.Fatalf("root entry %s missing from %v", name, args)
		}
	}
	if strings.Join(args[:3], " ") != "--ro-bind /bin /bin" {
		t.Fatalf("root entries must be read-only binds: %v", args[:3])
	}
}

// TestSandboxMountArgsBindsExistingTempRootsWritable 锁定临时目录根按存在性
// 改为可写绑定；缺失时跳过，避免 bwrap 因绑定源不存在而启动失败。
func TestSandboxMountArgsBindsExistingTempRootsWritable(t *testing.T) {
	args := sandboxMountArgs(nil, func(path string) bool { return path == "/tmp" })
	index := -1
	for offset := 0; offset+2 < len(args); offset++ {
		if args[offset] == "--bind" && args[offset+1] == "/tmp" {
			index = offset
		}
	}
	if index < 0 {
		t.Fatalf("existing temp root must be writable: %v", args)
	}
	if hasMount(args, "/var/tmp") {
		t.Fatalf("missing temp root must be skipped: %v", args)
	}
}

// TestConfineArgvRemountsRootReadOnly 锁定沙箱根在建立工作区挂载点后整体重挂
// 只读，且不再用 tmpfs 覆盖 /tmp：越界写得到 EROFS，工作区外只读引用仍可见。
func TestConfineArgvRemountsRootReadOnly(t *testing.T) {
	workspaceRoot := t.TempDir()
	argv, cleanup, err := ConfineArgv(SeatbeltOptions{WorkspaceRoot: workspaceRoot, WriteMode: SeatbeltWriteWorkspace}, []string{"/bin/sh", "-c", "true"})
	if cleanup != nil {
		defer func() { _ = cleanup() }()
	}
	if err != nil {
		if !strings.Contains(err.Error(), "bubblewrap") {
			t.Fatalf("unexpected error: %v", err)
		}
		t.Skip("bubblewrap unavailable on this host")
	}
	joined := strings.Join(argv, " ")
	workspaceIndex := strings.Index(joined, "--bind "+workspaceRoot+" /workspace")
	rootRemountIndex := strings.Index(joined, "--remount-ro / ")
	if rootRemountIndex < 0 {
		t.Fatalf("sandbox root must be remounted read-only: %v", argv)
	}
	if workspaceIndex < 0 || workspaceIndex > rootRemountIndex {
		t.Fatalf("root remount must follow workspace mount: %v", argv)
	}
	for _, forbidden := range []string{"--tmpfs", "--ro-bind / ", "--bind / /"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("unexpected mount form %q: %v", forbidden, argv)
		}
	}
	writable := 0
	for offset := 0; offset+2 < len(argv); offset++ {
		if argv[offset] == "--bind" {
			writable++
		}
	}
	tempRoots := 0
	for _, root := range sandboxWritableTempRoots {
		if strings.Contains(joined, "--bind "+root+" "+root) {
			tempRoots++
		}
	}
	if writable != tempRoots+1 {
		t.Fatalf("writable binds must be temp roots plus workspace: %v", argv)
	}
}
