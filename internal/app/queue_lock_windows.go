//go:build windows

// queue_lock_windows.go 在 Windows 平台用 LockFileEx 实现队列文件的跨进程
// 非阻塞排他锁，语义与 Unix flock 一致：冲突立即失败，不无限等待。
//
// Author: fxbin
package app

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockQueueFile 以非阻塞方式获取排他锁；被占用时返回错误。
func lockQueueFile(file *os.File) error {
	ol := new(windows.Overlapped)
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, ol,
	)
}

// unlockQueueFile 释放队列文件锁。
func unlockQueueFile(file *os.File) {
	ol := new(windows.Overlapped)
	_ = windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, ol)
}
