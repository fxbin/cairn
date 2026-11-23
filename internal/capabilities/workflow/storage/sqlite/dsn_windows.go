//go:build windows

// dsn_windows.go 在 Windows 平台构造工作流库的 SQLite DSN。Windows 绝对路径含
// 盘符冒号，用正斜杠拼接 file: 前缀避免被解析为非法 authority。
//
// Author: fxbin
package sqlite

import (
	"path/filepath"
	"strings"
)

// sqliteDSN 返回带查询参数的工作流库连接串。
func sqliteDSN(path, query string) string {
	return "file:" + strings.ReplaceAll(filepath.ToSlash(path), "?", "%3f") + "?" + query
}
