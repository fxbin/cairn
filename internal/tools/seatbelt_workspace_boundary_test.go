//go:build darwin

// seatbelt_workspace_boundary_test.go 锁定工作区父目录的写边界规则：拒绝规则
// 必须排在临时目录放行之后、工作区放行之前，否则越界写要么被临时规则放行，
// 要么连工作区内的合法写入一起被拒。
// Author: fxbin
package tools

import (
	"strings"
	"testing"
)

// TestSeatbeltProfileDeniesWorkspaceParentWrite 锁定父目录写边界的规则顺序。
func TestSeatbeltProfileDeniesWorkspaceParentWrite(t *testing.T) {
	const root = "/private/var/folders/ab/T/root/ws"
	profile, err := SeatbeltProfile(SeatbeltOptions{WorkspaceRoot: root, WriteMode: SeatbeltWriteWorkspace})
	if err != nil {
		t.Fatal(err)
	}
	tempRule := `(allow file-write* (subpath "/private/var/folders"))`
	parentRule := `(deny file-write* (subpath "/private/var/folders/ab/T/root"))`
	workspaceRule := `(allow file-write* (subpath "` + root + `"))`
	tempIndex := strings.Index(profile, tempRule)
	parentIndex := strings.Index(profile, parentRule)
	workspaceIndex := strings.Index(profile, workspaceRule)
	if tempIndex < 0 || parentIndex < 0 || workspaceIndex < 0 {
		t.Fatalf("profile 缺少写边界规则:\n%s", profile)
	}
	if parentIndex < tempIndex {
		t.Fatalf("父目录拒绝必须排在临时目录放行之后:\n%s", profile)
	}
	if workspaceIndex < parentIndex {
		t.Fatalf("工作区放行必须排在父目录拒绝之后:\n%s", profile)
	}
}

// TestSeatbeltProfileSkipsParentBoundaryForAlwaysWritableRoots 确认工作区直接
// 位于临时目录根之下时不叠加父目录拒绝：拒绝临时目录根会连带拒绝临时写。
func TestSeatbeltProfileSkipsParentBoundaryForAlwaysWritableRoots(t *testing.T) {
	for _, root := range []string{"/private/var/folders/ws", "/private/tmp/ws", "/ws"} {
		profile, err := SeatbeltProfile(SeatbeltOptions{WorkspaceRoot: root, WriteMode: SeatbeltWriteWorkspace})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(profile, "(deny file-write*") {
			t.Fatalf("工作区 %s 不应叠加父目录拒绝规则:\n%s", root, profile)
		}
	}
}

// TestSeatbeltProfileReadOnlyAlsoDeniesWorkspaceParent 锁定只读档同样拒绝
// 工作区父目录：只读档仍保留临时目录写，越界写不得因此被放行。
func TestSeatbeltProfileReadOnlyAlsoDeniesWorkspaceParent(t *testing.T) {
	const root = "/private/var/folders/ab/T/root/ws"
	profile, err := SeatbeltProfile(SeatbeltOptions{WorkspaceRoot: root, WriteMode: SeatbeltWriteNone})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(profile, `(deny file-write* (subpath "/private/var/folders/ab/T/root"))`) {
		t.Fatalf("只读档缺少父目录写边界:\n%s", profile)
	}
	if strings.Contains(profile, root) {
		t.Fatalf("只读 profile 不得放行工作区写入:\n%s", profile)
	}
}
