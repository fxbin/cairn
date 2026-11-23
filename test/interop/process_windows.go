//go:build windows

// process_windows.go 在 Windows 平台以 Job Object 管理受控子进程。Windows 无
// Unix 信号；任意中断阶段都对 Job 整体强制终止，不伪造优雅退出语义。
//
// Author: fxbin
package interop

import (
	"errors"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// interopProcessGuard 持有 Job Object 句柄。
type interopProcessGuard struct {
	job windows.Handle
}

// interopPrepareProcess 创建带 KILL_ON_JOB_CLOSE 的 Job Object。
func interopPrepareProcess(_ *exec.Cmd) (*interopProcessGuard, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, errors.New("create job: " + err.Error())
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, errors.New("configure job: " + err.Error())
	}
	return &interopProcessGuard{job: job}, nil
}

// interopAfterStart 把进程加入 Job；失败时终止进程避免留下无约束子进程。
func interopAfterStart(command *exec.Cmd, guard *interopProcessGuard) {
	if guard == nil || command == nil || command.Process == nil {
		return
	}
	handle, openErr := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(command.Process.Pid))
	if openErr != nil {
		_ = command.Process.Kill()
		_ = windows.CloseHandle(guard.job)
		guard.job = windows.InvalidHandle
		return
	}
	defer windows.CloseHandle(handle)
	if err := windows.AssignProcessToJobObject(guard.job, handle); err != nil {
		_ = command.Process.Kill()
		_ = windows.CloseHandle(guard.job)
		guard.job = windows.InvalidHandle
	}
}

// interopSignal 对 Job 整体强制终止；phase 在 Windows 无差异化语义。
func interopSignal(_ *exec.Cmd, guard *interopProcessGuard, _ string) {
	if guard != nil && guard.job != windows.InvalidHandle {
		_ = windows.TerminateJobObject(guard.job, 1)
	}
}

// interopReleaseProcess 关闭 Job 句柄，触发兜底回收。
func interopReleaseProcess(guard *interopProcessGuard) {
	if guard != nil && guard.job != windows.InvalidHandle {
		_ = windows.CloseHandle(guard.job)
		guard.job = windows.InvalidHandle
	}
}
