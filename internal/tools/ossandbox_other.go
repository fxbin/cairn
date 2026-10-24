//go:build !darwin && !linux

// ossandbox_other.go 非 macOS/Linux 无 OS 级 host 约束。
// Author: fxbin
package tools

// OSSandboxAvailable 非桌面 Unix 目标恒为 false。
func OSSandboxAvailable() bool { return false }

// HostConfinementName 返回 unsupported。
func HostConfinementName() string { return "host-unconfined" }

// ConfineArgv 无法约束。
func ConfineArgv(options SeatbeltOptions, argv []string) (wrapped []string, cleanup func() error, err error) {
	return nil, nil, errOSSandboxUnavailable
}
