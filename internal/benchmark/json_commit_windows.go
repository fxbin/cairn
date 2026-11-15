//go:build windows

// [INPUT]: 依赖Memory评测合同、冻结资产与受控执行入口。
// [OUTPUT]: 提供Windows的JSON提交边界。按平台编译。。
// [POS]: 带外评测工具链，不进入最小Agent内核。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package benchmark 提供隔离评测、冻结合同与可恢复结果留档。
// Author: fxbin
package benchmark

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// commitJSONFile 在 Windows 上使用带 WRITE_THROUGH 的同卷移动提交已同步临时
// 文件；不可变结果不设置覆盖标志，目标已存在时失败关闭。
func commitJSONFile(temporaryPath, path string, exclusive bool) error {
	source, err := windows.UTF16PtrFromString(temporaryPath)
	if err != nil {
		return fmt.Errorf("encode JSON temporary path: %w", err)
	}
	target, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("encode JSON target path: %w", err)
	}
	flags := uint32(windows.MOVEFILE_WRITE_THROUGH)
	if !exclusive {
		flags |= windows.MOVEFILE_REPLACE_EXISTING
	}
	if err := windows.MoveFileEx(source, target, flags); err != nil {
		return err
	}
	return nil
}
