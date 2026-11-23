//go:build windows

// sync_windows.go 在 Windows 平台处理制品目录同步。Windows 目录句柄不支持
// FlushFileBuffers；文件内容持久化由临时文件的 File.Sync 保证，目录项由
// 文件系统元数据语义保证，故此处只确认目录存在与可访问，不伪造目录同步。
//
// Author: fxbin
package artifact

import "os"

// syncDirectory 在 Windows 上确认目录存在且可访问。
func syncDirectory(directory string) error {
	info, err := os.Stat(directory)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return os.ErrInvalid
	}
	return nil
}
