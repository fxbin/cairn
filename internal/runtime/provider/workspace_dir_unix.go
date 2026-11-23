//go:build darwin || linux

// workspace_dir_unix.go 在 Unix 平台保持工作区哈希目录名原样。
//
// Author: fxbin
package provider

// safeWorkspaceDir 返回可安全用作目录名的工作区哈希；Unix 无需转义。
func safeWorkspaceDir(hash string) string {
	return hash
}

// safeRecordName 返回可安全用作文件名的内容摘要；Unix 无需转义。
func safeRecordName(fingerprint string) string {
	return fingerprint
}

// restoreFingerprint 把磁盘文件名还原为内容摘要；Unix 无需还原。
func restoreFingerprint(name string) string {
	return name
}
