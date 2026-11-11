//go:build windows

// Package lock 提供 Cairn 单工作区运行保障的进程级一半；逻辑上的非终态运行由 Journal 另行检查。
//
// Author: fxbin
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

// ErrHeld 表示已有进程持有同一工作区锁。
var ErrHeld = errors.New("workspace lock is already held")

// Lock 保存当前进程持有的 Windows 文件锁。
type Lock struct{ file *os.File }

// Acquire 创建并以非阻塞独占方式获取给定路径的工作区锁。
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open workspace lock: %w", err)
	}
	overlapped := windows.Overlapped{}
	err = windows.LockFileEx(
		windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		1,
		0,
		&overlapped,
	)
	if err != nil {
		_ = file.Close()
		if errors.Is(err, windows.ERROR_LOCK_VIOLATION) || errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("acquire workspace lock: %w", err)
	}
	return &Lock{file: file}, nil
}

// Release 释放文件锁并关闭底层文件；对空锁调用是安全的。
func (lock *Lock) Release() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	overlapped := windows.Overlapped{}
	err := windows.UnlockFileEx(windows.Handle(lock.file.Fd()), 0, 1, 0, &overlapped)
	closeErr := lock.file.Close()
	lock.file = nil
	if err != nil {
		return err
	}
	return closeErr
}
