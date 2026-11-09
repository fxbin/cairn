//go:build darwin

// ossandbox_darwin.go 将 host 收紧映射为 Seatbelt。
// Author: fxbin
package tools

import (
	"os"
)

// OSSandboxAvailable 表示 macOS Seatbelt 是否可用。
func OSSandboxAvailable() bool { return SeatbeltAvailable() }

// HostConfinementName 返回当前平台 OS 约束名。
func HostConfinementName() string { return "host+seatbelt" }

// ConfineArgv 用 sandbox-exec profile 包装 argv；cleanup 删除临时 profile。
func ConfineArgv(options SeatbeltOptions, argv []string) (wrapped []string, cleanup func() error, err error) {
	if !SeatbeltAvailable() {
		return nil, nil, errOSSandboxUnavailable
	}
	profile, err := SeatbeltProfile(options)
	if err != nil {
		return nil, nil, err
	}
	file, err := os.CreateTemp("", "cairn-seatbelt-*.sb")
	if err != nil {
		return nil, nil, err
	}
	path := file.Name()
	if _, err := file.WriteString(profile); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, nil, err
	}
	wrapped, err = WrapSeatbelt(path, argv)
	if err != nil {
		_ = os.Remove(path)
		return nil, nil, err
	}
	return wrapped, func() error { return os.Remove(path) }, nil
}

// newSandboxTermination 表示 macOS 不需要包装层取消投递：sandbox-exec 以 exec
// 方式直接运行目标命令，cairn 的直接子进程就是 workload，取消沿用进程组信号。
func newSandboxTermination() (*sandboxTermination, error) { return nil, nil }
