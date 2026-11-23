//go:build windows

// process_windows.go 提供 Windows 平台的进程树配置、整树终止与退出码解析。
// Windows 没有 Unix 进程组信号；这里用 Job Object（KILL_ON_JOB_CLOSE）持有
// 整个进程树，取消时对 Job 整体终止。Windows 无优雅信号，TERM 与 KILL 都走
// 强制终止，绝不伪造"已优雅退出"；是否真正终止仍由调用方有界 Wait 确认。
//
// Author: fxbin
package tools

import (
	"errors"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// processGroup 持有 Windows Job Object 句柄；加入 Job 的进程及其后续派生的
// 后代受 Job 约束。注意进程启动到 afterProcessStart 加入 Job 之间存在一个
// 短暂窗口，窗口内派生的子进程可能不在 Job 内；这里不声称绝对无逃逸。
type processGroup struct {
	job windows.Handle
}

// prepareProcessGroup 在进程启动前创建 Job Object 并设置 KILL_ON_JOB_CLOSE，
// 使 Job 关闭或终止时整树一同回收。失败时失败关闭，不降级为无约束启动。
func prepareProcessGroup(_ *exec.Cmd) (*processGroup, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, errors.New("create process job failed: " + err.Error())
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		_ = windows.CloseHandle(job)
		return nil, errors.New("configure process job failed: " + err.Error())
	}
	return &processGroup{job: job}, nil
}

// afterProcessStart 在进程启动后把进程加入 Job。加入失败时终止进程并释放
// Job，避免留下不受 Job 约束的子进程树。
func afterProcessStart(command *exec.Cmd, group *processGroup) {
	if group == nil || command == nil || command.Process == nil {
		return
	}
	handle, openErr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if openErr != nil {
		_ = command.Process.Kill()
		_ = windows.CloseHandle(group.job)
		group.job = windows.InvalidHandle
		return
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(group.job, handle); err != nil {
		_ = command.Process.Kill()
		_ = windows.CloseHandle(group.job)
		group.job = windows.InvalidHandle
	}
}

// signalGroupTerm 终止整个 Job 进程树。Windows 无优雅信号，语义为强制终止。
func signalGroupTerm(_ *exec.Cmd, group *processGroup) {
	if group != nil && group.job != windows.InvalidHandle {
		_ = windows.TerminateJobObject(group.job, 1)
	}
}

// signalGroupKill 终止整个 Job 进程树。
func signalGroupKill(_ *exec.Cmd, group *processGroup) {
	if group != nil && group.job != windows.InvalidHandle {
		_ = windows.TerminateJobObject(group.job, 1)
	}
}

// releaseProcessGroup 关闭 Job 句柄，触发 KILL_ON_JOB_CLOSE 兜底回收。
func releaseProcessGroup(group *processGroup) {
	if group != nil && group.job != windows.InvalidHandle {
		_ = windows.CloseHandle(group.job)
		group.job = windows.InvalidHandle
	}
}

// processExitOutcome 从 Wait 错误解析退出码；Windows ExitError 提供 ExitCode，
// 无信号概念，信号名恒定为空。
func processExitOutcome(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	exitError, ok := err.(*exec.ExitError)
	if !ok {
		return -1, ""
	}
	return exitError.ExitCode(), ""
}

// dockerUserArgs 在 Windows 上不注入 --user：Docker Desktop 的 bind mount
// 权限模型与 Unix uid:gid 不同，强行指定会破坏镜像启动。
func dockerUserArgs() []string {
	return nil
}
