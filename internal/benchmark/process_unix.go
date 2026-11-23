//go:build darwin || linux

// process_unix.go 在 Unix 平台用进程组管理评测子进程树。
//
// Author: fxbin
package benchmark

import (
	"os/exec"
	"syscall"
)

// benchmarkProcessGuard 是 Unix 进程组句柄。
type benchmarkProcessGuard struct{}

// benchmarkPrepareProcess 配置独立进程组。
func benchmarkPrepareProcess(command *exec.Cmd) (*benchmarkProcessGuard, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &benchmarkProcessGuard{}, nil
}

// benchmarkAfterStart 在 Unix 无需登记。
func benchmarkAfterStart(_ *exec.Cmd, _ *benchmarkProcessGuard) {}

// benchmarkSignal 向进程组投递信号，失败回退单进程。
func benchmarkSignal(command *exec.Cmd, _ *benchmarkProcessGuard, phase string) {
	signal := syscall.SIGKILL
	if phase == "term" {
		signal = syscall.SIGTERM
	}
	if err := syscall.Kill(-command.Process.Pid, signal); err != nil {
		_ = syscall.Kill(command.Process.Pid, signal)
	}
}

// benchmarkReleaseProcess 在 Unix 无需释放资源。
func benchmarkReleaseProcess(_ *benchmarkProcessGuard) {}
