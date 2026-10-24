//go:build linux

// ossandbox_linux.go 将 host 收紧映射为 bubblewrap。
// Author: fxbin
package tools

import (
	"fmt"
	"os/exec"
)

// OSSandboxExecutable 是 Linux bubblewrap 入口。
const OSSandboxExecutable = "bwrap"

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
		"--ro-bind", "/usr", "/usr",
		"--ro-bind", "/bin", "/bin",
		"--ro-bind", "/sbin", "/sbin",
		"--ro-bind", "/lib", "/lib",
		"--ro-bind", "/etc", "/etc",
		"--proc", "/proc",
		"--dev", "/dev",
		"--tmpfs", "/tmp",
	}
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
