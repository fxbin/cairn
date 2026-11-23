//go:build darwin || linux

// process_unix.go 提供 Unix 平台的进程组配置、整树信号与退出码解析。
// 进程组取消必须确认整树回收；这些函数只做 OS 层投递，确认由调用方的
// Wait 有界等待统一判定。
//
// Author: fxbin
package tools

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
)

// processGroup 保存 Unix 进程组取消所需的句柄；Unix 使用负 PID 作为组 ID，
// 无需持有额外 OS 资源。
type processGroup struct{}

// prepareProcessGroup 在进程启动前配置独立进程组。Unix 设置 Setpgid，使后续
// 可对 -pid 投递整组信号。
func prepareProcessGroup(command *exec.Cmd) (*processGroup, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return &processGroup{}, nil
}

// afterProcessStart 在进程启动后登记进程组；Unix 无需额外动作。
func afterProcessStart(_ *exec.Cmd, _ *processGroup) {}

// signalGroupTerm 向整个进程组投递 SIGTERM。
func signalGroupTerm(command *exec.Cmd, _ *processGroup) {
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
}

// signalGroupKill 向整个进程组投递 SIGKILL。
func signalGroupKill(command *exec.Cmd, _ *processGroup) {
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}

// releaseProcessGroup 释放进程组持有的资源；Unix 无资源可释放。
func releaseProcessGroup(_ *processGroup) {}

// processExitOutcome 从 Wait 错误解析退出码与终止信号。Unix 的 WaitStatus
// 可区分正常退出与被信号终止。
func processExitOutcome(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		return -1, ""
	}
	status, ok := exitError.Sys().(syscall.WaitStatus)
	if !ok {
		return exitError.ExitCode(), ""
	}
	if status.Signaled() {
		return -1, status.Signal().String()
	}
	return status.ExitStatus(), ""
}

// dockerUserArgs 返回以宿主用户身份运行容器的 argv 片段。Unix 必须显式指定
// uid:gid，避免 bind mount 产出 root 属主文件。
func dockerUserArgs() []string {
	return []string{"--user", strconv.Itoa(os.Getuid()) + ":" + strconv.Itoa(os.Getgid())}
}
