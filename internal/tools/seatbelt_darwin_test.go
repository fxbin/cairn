//go:build darwin

package tools

import (
	"strings"
	"testing"
)

// TestSeatbeltProfileWorkspaceWrite 覆盖工作区写档 profile 要点。
func TestSeatbeltProfileWorkspaceWrite(t *testing.T) {
	profile, err := SeatbeltProfile(SeatbeltOptions{
		WorkspaceRoot: "/Users/test/project",
		WriteMode:     SeatbeltWriteWorkspace,
		AllowNetwork:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"(version 1)",
		"(deny default)",
		`(allow file-write* (subpath "/Users/test/project"))`,
		"(deny network*)",
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("profile missing %q:\n%s", want, profile)
		}
	}
}

// TestSeatbeltProfileReadOnly 不开放工作区写。
func TestSeatbeltProfileReadOnly(t *testing.T) {
	profile, err := SeatbeltProfile(SeatbeltOptions{
		WorkspaceRoot: "/Users/test/project",
		WriteMode:     SeatbeltWriteNone,
		AllowNetwork:  false,
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(profile, "/Users/test/project") {
		t.Fatalf("read-only profile must not allow workspace writes:\n%s", profile)
	}
}

// TestSeatbeltProfileRejectsWhitespace 防止 profile 注入。
func TestSeatbeltProfileRejectsWhitespace(t *testing.T) {
	_, err := SeatbeltProfile(SeatbeltOptions{WorkspaceRoot: "/tmp/has space", WriteMode: SeatbeltWriteWorkspace})
	if err == nil {
		t.Fatal("expected error for whitespace root")
	}
}

// TestWrapSeatbeltPrefixesSandboxExec 锁定包装 argv 形状。
func TestWrapSeatbeltPrefixesSandboxExec(t *testing.T) {
	wrapped, err := WrapSeatbelt("/tmp/x.sb", []string{"echo", "hi"})
	if err != nil {
		t.Fatal(err)
	}
	if len(wrapped) < 5 || wrapped[0] != SeatbeltExecutable || wrapped[3] != "--" {
		t.Fatalf("wrapped = %v", wrapped)
	}
}

// TestHostWriteModeFromPolicy 映射写策略。
func TestHostWriteModeFromPolicy(t *testing.T) {
	if HostWriteModeFromPolicy("deny") != SeatbeltWriteNone {
		t.Fatal("deny should map to none")
	}
	if HostWriteModeFromPolicy("ask") != SeatbeltWriteWorkspace {
		t.Fatal("ask should map to workspace")
	}
}
