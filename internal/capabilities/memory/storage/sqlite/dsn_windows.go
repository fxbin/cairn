//go:build windows

// dsn_windows.go 在 Windows 平台构造只读记忆库的 SQLite DSN。Windows 绝对路径
// 含盘符冒号，用正斜杠路径拼接 file: 前缀避免被解析为非法 authority。
//
// Author: fxbin
package sqlite

import (
	"path/filepath"
	"strings"
)

// readOnlyMemoryDSN 返回只读且不可变的连接串。
func readOnlyMemoryDSN(path string) string {
	return "file:" + strings.ReplaceAll(filepath.ToSlash(path), "?", "%3f") + "?mode=ro&immutable=1"
}
