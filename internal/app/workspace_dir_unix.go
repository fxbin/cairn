//go:build darwin || linux

// workspace_dir_unix.go 在 Unix 平台保持工作区哈希目录名原样（含 sha256: 前缀），
// 与既有数据布局完全兼容。
//
// Author: fxbin
package app

// safeWorkspaceDir 返回可安全用作目录名的工作区哈希。Unix 无需转义。
func safeWorkspaceDir(hash string) string {
	return hash
}
