//go:build darwin || linux

// process_unix.go 在 Unix 平台以进程组管理受控子进程，保证子进程树一并回收。
//
// Author: fxbin
package interop

import (
	"os/exec"
	"syscall"
)

// interopProcessGuard 是 Unix 进程组句柄；Unix 用负 PID 作为组 ID，无需额外资源。
type interopProcessGuard struct{}

// interopPrepareProcess 配置独立进程组。
func interopPrepareProcess(command *exec.Cmd) (*interopProcessGuard, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &interopProcessGuard{}, nil
}

// interopAfterStart 在 Unix 无需登记。
func interopAfterStart(_ *exec.Cmd, _ *interopProcessGuard) {}

// interopSignal 向进程组投递信号，失败时回退单进程投递。
func interopSignal(command *exec.Cmd, _ *interopProcessGuard, phase string) {
	var signal syscall.Signal
	switch phase {
	case "int":
		signal = syscall.SIGINT
	case "term":
		signal = syscall.SIGTERM
	default:
		signal = syscall.SIGKILL
	}
	if err := syscall.Kill(-command.Process.Pid, signal); err != nil {
		_ = syscall.Kill(command.Process.Pid, signal)
	}
}

// interopReleaseProcess 在 Unix 无需释放资源。
func interopReleaseProcess(_ *interopProcessGuard) {}
