//go:build windows

// readonly_dsn_windows.go 在 Windows 平台构造只读 Journal 的 SQLite DSN。
// Windows 绝对路径含盘符冒号，若经 url.URL 的 Path 字段编码会产生非法的
// authority；这里用正斜杠路径直接拼接 file: 前缀，保留盘符语义。
//
// Author: fxbin
package sqlite

import (
	"path/filepath"
	"strings"
)

// readOnlyDSN 返回带 mode=ro 的只读连接串。
func readOnlyDSN(path string) string {
	return "file:" + strings.ReplaceAll(filepath.ToSlash(path), "?", "%3f") + "?mode=ro"
}
