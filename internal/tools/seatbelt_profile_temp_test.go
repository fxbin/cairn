//go:build darwin

// seatbelt_profile_temp_test.go 锁定临时目录写规则的规范路径覆盖：macOS
// 内核按符号链接解析后的路径匹配 subpath 规则，用户级临时目录的规范路径是
// /private/var/folders，缺少该规则时编译器/测试缓存等临时写全部被拒。
// Author: fxbin
package tools

import (
	"strings"
	"testing"
)

// TestSeatbeltProfileAllowsCanonicalTempPaths 锁定 profile 同时包含临时目录
// 的符号链接拼写与规范路径拼写，两种工作目录形态下临时写均可用。
func TestSeatbeltProfileAllowsCanonicalTempPaths(t *testing.T) {
	profile, err := SeatbeltProfile(SeatbeltOptions{
		WorkspaceRoot: "/Users/test/project",
		WriteMode:     SeatbeltWriteWorkspace,
		AllowNetwork:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`(allow file-write* (subpath "/private/tmp"))`,
		`(allow file-write* (subpath "/var/folders"))`,
		`(allow file-write* (subpath "/private/var/folders"))`,
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing canonical temp rule %q:\n%s", want, profile)
		}
	}
}
