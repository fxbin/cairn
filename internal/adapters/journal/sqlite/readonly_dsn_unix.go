//go:build darwin || linux

// readonly_dsn_unix.go 在 Unix 平台构造只读 Journal 的 SQLite DSN。
//
// Author: fxbin
package sqlite

import "net/url"

// readOnlyDSN 返回带 mode=ro 的只读连接串。Unix 路径可直接作为 URI path。
func readOnlyDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
}
