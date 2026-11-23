//go:build windows

// workspace_dir_windows.go 在 Windows 平台把工作区哈希中的冒号替换为下划线，
// 因为 Windows 文件名不允许冒号。工作区哈希本身（用于事件身份与摘要校验）
// 保持不变，仅目录名做平台安全化。Cairn 在 Windows 上此前不可用，无历史数据
// 兼容负担。
//
// Author: fxbin
package app

import "strings"

// safeWorkspaceDir 返回可安全用作目录名的工作区哈希。
func safeWorkspaceDir(hash string) string {
	return strings.ReplaceAll(hash, ":", "_")
}
