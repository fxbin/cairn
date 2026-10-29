//go:build darwin

// seatbelt_workspace_boundary_test.go 锁定 host 后端的写范围：可写集合只能是
// 临时目录根与工作区本身，工作区之外默认不可写；同时任何拒绝规则都不得覆盖
// 临时写范围——那会连带拒绝编译器缓存与验证子进程的临时写。
// Author: fxbin
package tools

import (
	"regexp"
	"strings"
	"testing"
)

// seatbeltWriteAllowPattern 匹配写放行规则，捕获其 subpath。
var seatbeltWriteAllowPattern = regexp.MustCompile(`\(allow file-write[^)]*\(subpath "([^"]+)"\)`)

// seatbeltWriteDenyPattern 匹配写拒绝规则，捕获其 subpath。
var seatbeltWriteDenyPattern = regexp.MustCompile(`\(deny file-write[^)]*\(subpath "([^"]+)"\)`)

// TestSeatbeltProfileWriteScopeIsTempRootsAndWorkspace 锁定写范围：放行集合
// 只能是临时目录根与工作区根。工作区位于真实目录时，其父目录不在放行集合内，
// 越界写因此被默认拒绝规则挡住。
func TestSeatbeltProfileWriteScopeIsTempRootsAndWorkspace(t *testing.T) {
	const root = "/Users/example/project/ws"
	profile, err := SeatbeltProfile(SeatbeltOptions{WorkspaceRoot: root, WriteMode: SeatbeltWriteWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{root: true}
	for _, tempRoot := range seatbeltAlwaysWritableRoots {
		allowed[tempRoot] = true
	}
	for _, match := range seatbeltWriteAllowPattern.FindAllStringSubmatch(profile, -1) {
		if !allowed[match[1]] {
			t.Fatalf("写范围越界：%s 不在临时根与工作区之列:\n%s", match[1], profile)
		}
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "`+root+`"))`) {
		t.Fatalf("工作区写入未被放行:\n%s", profile)
	}
}

// TestSeatbeltProfileReadOnlyDoesNotAllowWorkspaceWrite 锁定只读档：工作区写入
// 不得放行，写范围只剩临时目录根。
func TestSeatbeltProfileReadOnlyDoesNotAllowWorkspaceWrite(t *testing.T) {
	const root = "/Users/example/project/ws"
	profile, err := SeatbeltProfile(SeatbeltOptions{WorkspaceRoot: root, WriteMode: SeatbeltWriteNone})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(profile, `(allow file-write* (subpath "`+root+`"))`) {
		t.Fatalf("只读档不得放行工作区写入:\n%s", profile)
	}
}

// TestSeatbeltProfileNeverDeniesTempWriteScope 锁定临时写范围不被拒绝规则覆盖：
// 在临时根之下拒绝某棵子树会连带拒绝编译器缓存与验证子进程的临时写，之前
// 粗粒度的父目录拒绝就是这样打断端到端流程的。
func TestSeatbeltProfileNeverDeniesTempWriteScope(t *testing.T) {
	for _, root := range []string{
		"/private/var/folders/ab/T/root/ws",
		"/Users/example/project/ws",
	} {
		profile, err := SeatbeltProfile(SeatbeltOptions{WorkspaceRoot: root, WriteMode: SeatbeltWriteWorkspace})
		if err != nil {
			t.Fatal(err)
		}
		for _, match := range seatbeltWriteDenyPattern.FindAllStringSubmatch(profile, -1) {
			for _, tempRoot := range seatbeltAlwaysWritableRoots {
				if strings.HasPrefix(match[1], tempRoot) {
					t.Fatalf("拒绝规则 %s 覆盖临时写范围 %s:\n%s", match[1], tempRoot, profile)
				}
			}
		}
	}
}
