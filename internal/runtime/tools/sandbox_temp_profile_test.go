//go:build darwin

// sandbox_temp_profile_test.go 锁定 profile 的写范围在自定义临时目录下的形态：除固定
// 临时根与工作区外，只多出当前临时目录的规范路径，且每条放行恰好出现一次；过宽、吞掉
// 工作区（仅只读档）或无法安全引用的临时目录让 profile 生成失败关闭。
// Author: fxbin
package tools

import (
	"strings"
	"testing"
)

const (
	// profileTestWorkspace 是用例的工作区根。
	profileTestWorkspace = "/Users/example/project/ws"
	// profileTestTempDir 是位于固定根之外、不吞掉工作区的专用临时目录。
	profileTestTempDir = "/Users/example/custom-tmp"
	// profileTestHome 是用例的用户主目录。
	profileTestHome = "/Users/example"
)

// profileWriteAllows 统计 profile 里每条写放行规则出现的次数，键是它的 subpath。
func profileWriteAllows(profile string) map[string]int {
	seen := map[string]int{}
	for _, match := range seatbeltWriteAllowPattern.FindAllStringSubmatch(profile, -1) {
		seen[match[1]]++
	}
	return seen
}

// TestSeatbeltProfileAllowsOnlyTempRootsWorkspaceAndCurrentTempDir 锁定写范围是封闭集合。
func TestSeatbeltProfileAllowsOnlyTempRootsWorkspaceAndCurrentTempDir(t *testing.T) {
	profile, err := seatbeltProfile(SeatbeltOptions{WorkspaceRoot: profileTestWorkspace, WriteMode: SeatbeltWriteWorkspace}, fixedSandboxTempInput(profileTestTempDir, profileTestHome, identityResolve))
	if err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{profileTestWorkspace: true, profileTestTempDir: true}
	for _, tempRoot := range seatbeltAlwaysWritableRoots {
		allowed[tempRoot] = true
	}
	seen := profileWriteAllows(profile)
	for path := range seen {
		if !allowed[path] {
			t.Fatalf("写范围越界：%s 不在临时根、当前临时目录与工作区之列:\n%s", path, profile)
		}
	}
	if len(seen) != len(allowed) {
		t.Fatalf("放行数量不符: seen=%v allowed=%v", seen, allowed)
	}
	for path := range allowed {
		if seen[path] != 1 {
			t.Fatalf("写放行 %s 应恰好出现一次，实际 %d 次:\n%s", path, seen[path], profile)
		}
	}
}

// TestSeatbeltProfileReadOnlyKeepsTempDirButNotWorkspace 锁定只读档仍放行不吞掉工作区的
// 临时目录：编译与验证子进程的临时写不依赖工作区写权限。
func TestSeatbeltProfileReadOnlyKeepsTempDirButNotWorkspace(t *testing.T) {
	profile, err := seatbeltProfile(SeatbeltOptions{WorkspaceRoot: profileTestWorkspace, WriteMode: SeatbeltWriteNone}, fixedSandboxTempInput(profileTestTempDir, profileTestHome, identityResolve))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(allow file-write* (subpath "`+profileTestTempDir+`"))`) {
		t.Fatalf("只读档应放行当前临时目录:\n%s", profile)
	}
	if strings.Contains(profile, profileTestWorkspace) {
		t.Fatalf("只读档不得放行工作区写入:\n%s", profile)
	}
}

// TestSeatbeltProfileFailsClosedForBroadTempDir 锁定过宽的临时目录让 profile 生成失败，
// 而不是生成一条把写边界架空的规则。
func TestSeatbeltProfileFailsClosedForBroadTempDir(t *testing.T) {
	for _, tempDir := range []string{"/", "/Users/example", "/Users"} {
		profile, err := seatbeltProfile(SeatbeltOptions{WorkspaceRoot: profileTestWorkspace, WriteMode: SeatbeltWriteWorkspace}, fixedSandboxTempInput(tempDir, profileTestHome, identityResolve))
		if err == nil {
			t.Fatalf("临时目录 %q 应让 profile 生成失败:\n%s", tempDir, profile)
		}
		if !strings.Contains(err.Error(), "too broad") {
			t.Fatalf("错误应说明临时目录过宽: %v", err)
		}
	}
}

// TestSeatbeltProfileGovernsCoveringTempDirByWriteMode 锁定覆盖工作区的临时目录按写入档位
// 区分：只读档下不能成为写规则，否则工作区会经它变得可写；可写档下照常放行，与工作区
// 位于系统临时根之下时的既有行为一致。
func TestSeatbeltProfileGovernsCoveringTempDirByWriteMode(t *testing.T) {
	for _, tempDir := range []string{"/Users/example/project", profileTestWorkspace} {
		profile, err := seatbeltProfile(SeatbeltOptions{WorkspaceRoot: profileTestWorkspace, WriteMode: SeatbeltWriteNone}, fixedSandboxTempInput(tempDir, profileTestHome, identityResolve))
		if err == nil || !strings.Contains(err.Error(), "covers the workspace") {
			t.Fatalf("只读档下覆盖工作区的临时目录 %q 应让 profile 生成失败:\n%s\n%v", tempDir, profile, err)
		}
		profile, err = seatbeltProfile(SeatbeltOptions{WorkspaceRoot: profileTestWorkspace, WriteMode: SeatbeltWriteWorkspace}, fixedSandboxTempInput(tempDir, profileTestHome, identityResolve))
		if err != nil || !strings.Contains(profile, `(allow file-write* (subpath "`+tempDir+`"))`) {
			t.Fatalf("可写档下覆盖工作区的临时目录 %q 应被放行:\n%s\n%v", tempDir, profile, err)
		}
	}
}

// TestSeatbeltProfileFailsClosedForUnquotableTempDir 锁定无法安全写进 profile 字符串的临时
// 目录让生成失败，与工作区根的注入防护同一口径。
func TestSeatbeltProfileFailsClosedForUnquotableTempDir(t *testing.T) {
	for _, tempDir := range []string{"/Users/example/my tmp", `/Users/example/a"b`, "/Users/example/a\nb"} {
		profile, err := seatbeltProfile(SeatbeltOptions{WorkspaceRoot: profileTestWorkspace, WriteMode: SeatbeltWriteWorkspace}, fixedSandboxTempInput(tempDir, profileTestHome, identityResolve))
		if err == nil || !strings.Contains(err.Error(), "whitespace or quotes") {
			t.Fatalf("无法安全引用的临时目录 %q 应让 profile 生成失败:\n%s\n%v", tempDir, profile, err)
		}
	}
}
