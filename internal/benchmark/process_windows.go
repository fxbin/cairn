//go:build windows

// process_windows.go 在 Windows 平台用 Job Object 管理评测子进程树。Windows 无
// 优雅信号，任意终止阶段都对 Job 整体强制终止。
//
// Author: fxbin
package benchmark

import (
	"errors"
	"os/exec"
	"unsafe"

	"golang.org/x/sys/windows"
)

// benchmarkProcessGuard 持有 Job Object 句柄。
type benchmarkProcessGuard struct {
	job windows.Handle
}

// benchmarkPrepareProcess 创建带 KILL_ON_JOB_CLOSE 的 Job Object。
func benchmarkPrepareProcess(_ *exec.Cmd) (*benchmarkProcessGuard, error) {
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
	return &benchmarkProcessGuard{job: job}, nil
}

// benchmarkAfterStart 把进程加入 Job；失败时终止进程避免无约束子进程。
func benchmarkAfterStart(command *exec.Cmd, guard *benchmarkProcessGuard) {
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

// benchmarkSignal 对 Job 整体强制终止；phase 在 Windows 无差异化语义。
func benchmarkSignal(_ *exec.Cmd, guard *benchmarkProcessGuard, _ string) {
	if guard != nil && guard.job != windows.InvalidHandle {
		_ = windows.TerminateJobObject(guard.job, 1)
	}
}

// benchmarkReleaseProcess 关闭 Job 句柄触发兜底回收。
func benchmarkReleaseProcess(guard *benchmarkProcessGuard) {
	if guard != nil && guard.job != windows.InvalidHandle {
		_ = windows.CloseHandle(guard.job)
		guard.job = windows.InvalidHandle
	}
}
