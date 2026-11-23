//go:build windows

// workspace_dir_windows.go 在 Windows 平台把工作区哈希中的冒号替换为下划线。
// 工作区哈希本身不变，仅目录名做平台安全化。
//
// Author: fxbin
package provider

import "strings"

// safeWorkspaceDir 返回可安全用作目录名的工作区哈希。
func safeWorkspaceDir(hash string) string {
	return strings.ReplaceAll(hash, ":", "_")
}

// safeRecordName 返回可安全用作文件名的内容摘要。摘要固定为 sha256:<64hex>，
// 冒号只出现在第 7 字节，转义无歧义。
func safeRecordName(fingerprint string) string {
	return strings.Replace(fingerprint, ":", "_", 1)
}

// restoreFingerprint 把磁盘文件名还原为 sha256:<64hex> 内容摘要。
func restoreFingerprint(name string) string {
	if strings.HasPrefix(name, "sha256_") && len(name) == len("sha256_")+64 {
		return "sha256:" + name[len("sha256_"):]
	}
	return name
}
