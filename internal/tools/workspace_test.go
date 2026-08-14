package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceResolveRejectsSymlinkEscapeForMissingLeaf(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"link/newfile", "link/nested/deeper.txt"} {
		if _, _, err := workspace.Resolve(path); err == nil || err.Error() != "symlink escapes workspace" {
			t.Fatalf("resolve %q err = %v", path, err)
		}
	}
}

func TestWorkspaceResolveCanonicalizesMissingLeafInsideWorkspace(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dir", filepath.Join(root, "inner")); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, relative, err := workspace.Resolve("inner/newfile")
	if err != nil {
		t.Fatal(err)
	}
	if relative != "dir/newfile" || resolved != filepath.Join(canonicalRoot, "dir", "newfile") {
		t.Fatalf("resolve inner/newfile = %q, %q", resolved, relative)
	}
	if _, relative, err := workspace.Resolve("dir/plain.txt"); err != nil || relative != "dir/plain.txt" {
		t.Fatalf("resolve dir/plain.txt = %q, %v", relative, err)
	}
}

func TestApplyPatchPlanRejectsCreateThroughEscapingSymlink(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "ws")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ApplyPatch{Workspace: workspace}
	arguments := json.RawMessage(`{"path":"link/hostile.txt","mode":"create","expected_sha256":"absent","content":"payload"}`)
	if _, err := tool.Plan(arguments); err == nil {
		t.Fatal("plan through escaping symlink must fail")
	}
	result := tool.Execute(context.Background(), arguments)
	if result.OK || result.Code != "invalid" {
		t.Fatalf("execute result = %#v", result)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("outside directory must stay empty, got %v", entries)
	}
}
