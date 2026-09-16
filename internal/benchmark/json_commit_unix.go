//go:build !windows

// Package benchmark 提供隔离评测、冻结合同与可恢复结果留档。
// Author: fxbin
package benchmark

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// commitJSONFile 在 Unix 平台提交已同步的临时文件；不可变结果使用硬链接
// 保持 create-if-absent，更新结果使用同文件系统原子重命名，随后同步目录项。
func commitJSONFile(temporaryPath, path string, exclusive bool) error {
	if exclusive {
		if err := os.Link(temporaryPath, path); err != nil {
			return err
		}
	} else if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	if err := syncJSONDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync JSON directory: %w", err)
	}
	return nil
}

// syncJSONDirectory 持久化文件发布后的目录项，任何失败都阻止调用方宣称提交成功。
func syncJSONDirectory(path string) (err error) {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open JSON directory: %w", err)
	}
	defer func() {
		if closeErr := directory.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close JSON directory: %w", closeErr))
		}
	}()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync JSON directory: %w", err)
	}
	return nil
}
