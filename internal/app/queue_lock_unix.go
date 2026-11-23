//go:build darwin || linux

// queue_lock_unix.go 在 Unix 平台用 flock 实现队列文件的跨进程非阻塞排他锁。
//
// Author: fxbin
package app

import (
	"os"
	"syscall"
)

// lockQueueFile 以非阻塞方式获取排他锁；被占用时返回错误。
func lockQueueFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlockQueueFile 释放队列文件锁。
func unlockQueueFile(file *os.File) {
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}
