//go:build darwin || linux

// dsn_unix.go 在 Unix 平台构造工作流库的 SQLite DSN。
//
// Author: fxbin
package sqlite

import (
	"net/url"
	"path/filepath"
)

// sqliteDSN 返回带查询参数的工作流库连接串。
func sqliteDSN(path, query string) string {
	return (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: query}).String()
}
