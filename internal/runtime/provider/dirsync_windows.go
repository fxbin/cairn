//go:build windows

// dirsync_windows.go 在 Windows 平台处理目录同步。Windows 目录句柄不支持
// FlushFileBuffers，文件内容的持久化由 appendContentAddressed 中的 File.Sync
// 保证；目录项本身由文件系统元数据语义保证，故此处不伪造目录同步。
//
// Author: fxbin
package provider

import "os"

// syncProviderDirectory 在 Windows 上确认目录存在且可访问，不执行无意义的目录同步。
func syncProviderDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}
