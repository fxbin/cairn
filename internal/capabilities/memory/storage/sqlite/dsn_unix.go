//go:build darwin || linux

// dsn_unix.go 在 Unix 平台构造只读记忆库的 SQLite DSN。
//
// Author: fxbin
package sqlite

import "net/url"

// readOnlyMemoryDSN 返回只读且不可变的连接串。
func readOnlyMemoryDSN(path string) string {
	return (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}).String()
}
