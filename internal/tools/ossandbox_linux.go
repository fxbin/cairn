//go:build linux

// ossandbox_linux.go 将 host 收紧映射为 bubblewrap。
// Author: fxbin
package tools

import (
	"fmt"
	"os"
	"os/exec"
)

// OSSandboxExecutable 是 Linux bubblewrap 入口。
const OSSandboxExecutable = "bwrap"

// sandboxLoaderAliasPaths 是根级动态加载器别名目录。沙箱根是空 tmpfs，只有
// 显式挂载的路径存在；内核执行动态链接程序时按 ELF PT_INTERP 直接打开解释器，
// 缺失时 execvp 返回 ENOENT，bwrap 报 "execvp /bin/sh: No such file or directory"。
// ubuntu-26.04 runner 上 /usr/bin/dash 的 PT_INTERP 是 /lib64/ld-linux-x86-64.so.2，
// 而 /lib64 只是指向 usr/lib64 的符号链接：必须同时挂载 /lib 与该别名，别名里的
// 相对符号链接 ../lib/x86_64-linux-gnu/... 才能在 /lib 内解析。实测只挂其中一个
// 仍然失败，两者同时挂载后 exec 成功。宿主不存在该路径时跳过，避免 bwrap 因
// 绑定源缺失而失败（精简镜像没有 /lib64）。
var sandboxLoaderAliasPaths = []string{"/lib32", "/lib64", "/libx32"}

// sandboxMountArgs 构造沙箱根的只读挂载参数：固定挂载基础目录，并在宿主存在时
// 补齐根级加载器别名目录。pathExists 由调用方注入，便于参数层测试。
func sandboxMountArgs(pathExists func(string) bool) []string {
	args := []string{
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/sbin", "/sbin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/etc", "/etc",
	}
	for _, path := range sandboxLoaderAliasPaths {
		if pathExists(path) {
			args = append(args, "--ro-bind", path, path)
		}
	}
	return args
}

// hostPathExists 报告宿主文件中该路径是否存在（符号链接按自身计）。
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
	args := []string{
		"--unshare-all",
		"--die-with-parent",
	}
	args = append(args, sandboxMountArgs(hostPathExists)...)
	args = append(args,
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	)
	if options.AllowNetwork {
		args = append(args, "--share-net")
	}
	args = append(args, "--bind", options.WorkspaceRoot, "/workspace")
	if options.WriteMode == SeatbeltWriteNone {
		args = append(args, "--remount-ro", "/workspace")
	}
	args = append(args, "--chdir", "/workspace", "--")
	wrapped = append([]string{binary}, args...)
	return append(wrapped, argv...), nil, nil
}
