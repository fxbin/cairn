//go:build linux

// ossandbox_linux.go 将 host 收紧映射为 bubblewrap：宿主根按只读视图整体
// 映射，写权限只放开工作区与临时目录根，与 macOS seatbelt 的
// 「默认拒绝写、读不收紧」保持同一合同。
// Author: fxbin
package tools

import (
	"fmt"
	"os"
	"os/exec"
)

// OSSandboxExecutable 是 Linux bubblewrap 入口。
const OSSandboxExecutable = "bwrap"

// sandboxRootSkipEntries 是不按普通只读绑定处理的根级条目：/dev 与 /proc 由
// bwrap 专用挂载重建，/sys 不进入沙箱，/tmp 由临时写规则改为可写绑定，
// /workspace 是工作区挂载点（随后单独绑定，避免与宿主同名目录冲突）。
var sandboxRootSkipEntries = []string{"dev", "proc", "sys", "tmp", "workspace"}

// sandboxWritableTempRoots 是始终可写的临时目录根：编译器缓存与验证子进程的
// 临时写落在这些根之下。与 macOS seatbelt 一致，临时写范围不得被写拒绝规则
// 覆盖，否则会连带打断依赖临时目录的构建与验证。
var sandboxWritableTempRoots = []string{"/tmp", "/var/tmp"}

// sandboxRootEntries 列出需要只读映射的宿主根级条目，按字典序返回，跳过
// sandboxRootSkipEntries。列不出根目录时失败关闭，不退化为可写或空沙箱。
func sandboxRootEntries(readDir func(string) ([]os.DirEntry, error)) ([]string, error) {
	entries, err := readDir("/")
	if err != nil {
		return nil, fmt.Errorf("bubblewrap sandbox cannot enumerate host root: %w", err)
	}
	skipped := make(map[string]struct{}, len(sandboxRootSkipEntries))
	for _, name := range sandboxRootSkipEntries {
		skipped[name] = struct{}{}
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if _, skip := skipped[entry.Name()]; skip {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// sandboxMountArgs 构造沙箱根的挂载参数：根级条目逐一只读绑定，使工作区外的
// 只读引用（依赖、缓存、构建产物目录）仍然可用；随后重建 /dev 与 /proc，并把
// 存在的临时目录根改为可写绑定。pathExists 由调用方注入，便于参数层测试。
//
// 这里必须逐条绑定宿主根条目而不是只绑定 /usr、/bin 等少数目录：沙箱根是空
// tmpfs，未绑定的路径在内核里不存在，动态链接程序会因 PT_INTERP
// （ubuntu-26.04 runner 上 /usr/bin/dash 为 /lib64/ld-linux-x86-64.so.2）缺失而
// execvp 返回 ENOENT，表现为 "bwrap: execvp /bin/sh: No such file or directory"。
func sandboxMountArgs(entries []string, pathExists func(string) bool) []string {
	args := make([]string, 0, len(entries)*3+len(sandboxWritableTempRoots)*3+4)
	for _, name := range entries {
		path := "/" + name
		args = append(args, "--ro-bind", path, path)
	}
	args = append(args, "--dev", "/dev", "--proc", "/proc")
	for _, root := range sandboxWritableTempRoots {
		if pathExists(root) {
			args = append(args, "--bind", root, root)
		}
	}
	return args
}

// hostPathExists 报告宿主该路径是否存在（符号链接按自身计）。
func hostPathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// OSSandboxAvailable 判断 bwrap 是否在 PATH。
func OSSandboxAvailable() bool {
	_, err := exec.LookPath(OSSandboxExecutable)
	return err == nil
}

// HostConfinementName 返回当前平台 OS 约束名。
func HostConfinementName() string { return "host+bwrap" }

// ConfineArgv 用 bubblewrap 包装 argv；无独立临时文件。
//
// 沙箱根的 tmpfs 保持可写直到工作区挂载点建立完成，随后整体重挂为只读：
// 工作区父目录因此不可写（越界写得到 EROFS），而工作区绑定与临时目录绑定是
// 独立挂载，写权限不受影响。
func ConfineArgv(options SeatbeltOptions, argv []string) (wrapped []string, cleanup func() error, err error) {
	if !OSSandboxAvailable() {
		return nil, nil, errOSSandboxUnavailable
	}
	if options.WorkspaceRoot == "" {
		return nil, nil, fmt.Errorf("workspace root is required")
	}
	binary, lookErr := exec.LookPath(OSSandboxExecutable)
	if lookErr != nil {
		return nil, nil, fmt.Errorf("bubblewrap is required for host confinement: %w", lookErr)
	}
	entries, entriesErr := sandboxRootEntries(os.ReadDir)
	if entriesErr != nil {
		return nil, nil, entriesErr
	}
	args := []string{
		"--unshare-all",
		"--die-with-parent",
	}
	args = append(args, sandboxMountArgs(entries, hostPathExists)...)
	if options.AllowNetwork {
		args = append(args, "--share-net")
	}
	args = append(args, "--bind", options.WorkspaceRoot, "/workspace")
	if options.WriteMode == SeatbeltWriteNone {
		args = append(args, "--remount-ro", "/workspace")
	}
	args = append(args, "--remount-ro", "/", "--chdir", "/workspace", "--")
	wrapped = append([]string{binary}, args...)
	return append(wrapped, argv...), nil, nil
}
