package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyPatchRequiresExactHashAndWritesAtomically(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("one two one"), 0o640); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ApplyPatch{Workspace: workspace}
	staleHash := sha256Reference([]byte("stale content"))
	plan, err := tool.Plan(json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"` + staleHash + `","replacements":[{"old":"one","new":"three","occurrence":2}]}`))
	var stale *PatchPreconditionError
	if err == nil || plan.Path != "" || !errors.As(err, &stale) {
		t.Fatalf("expected stale hash rejection, got %#v, %v", plan, err)
	}
	if stale.Path != "file.txt" || stale.ExpectedSHA256 != staleHash || stale.ActualSHA256 != sha256Reference([]byte("one two one")) || strings.Contains(stale.Error(), "one two one") {
		t.Fatalf("stale error = %#v", stale)
	}
	plan, err = tool.Plan(json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"` + sha256Reference([]byte("one two one")) + `","replacements":[{"old":"one","new":"three","occurrence":2}]}`))
	if err != nil || string(plan.After) != "three two three" {
		t.Fatalf("plan = %#v, %v", plan, err)
	}
	result := tool.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"`+sha256Reference([]byte("one two one"))+`","replacements":[{"old":"one","new":"three","occurrence":2}]}`))
	if !result.OK {
		t.Fatalf("result = %#v", result)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "three two three" {
		t.Fatalf("content = %q, %v", content, err)
	}
}

func TestApplyPatchDefaultsMissingOccurrenceToOneExactMatch(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "file.txt")
	if err := os.WriteFile(path, []byte("one two"), 0o640); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	tool := ApplyPatch{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"`+sha256Reference([]byte("one two"))+`","replacements":[{"old":"one","new":"three"}]}`))
	if !result.OK {
		t.Fatalf("result = %#v", result)
	}
	content, err := os.ReadFile(path)
	if err != nil || string(content) != "three two" {
		t.Fatalf("content = %q, %v", content, err)
	}
}

func TestApplyPatchSpecRequiresExpectedSHA256(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	specification := (ApplyPatch{Workspace: workspace}).Spec()
	if !strings.Contains(specification.Description, "expected_sha256") || !strings.Contains(string(specification.Parameters), `"required":["path","mode","expected_sha256"]`) {
		t.Fatalf("apply_patch spec does not declare hash binding: %#v", specification)
	}
}

// TestApplyPatchSweepsStaleTemporaries 锁定崩溃残留清扫：成功的补丁会清掉
// 历史遗留的 .cairn-patch-* 临时文件。
func TestApplyPatchSweepsStaleTemporaries(t *testing.T) {
	root := t.TempDir()
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "file.txt")
	if err := os.WriteFile(target, []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, ".cairn-patch-stale")
	if err := os.WriteFile(stale, []byte("leftover"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := sha256Hex([]byte("v1"))
	tool := ApplyPatch{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"`+before+`","replacements":[{"old":"v1","new":"v2"}]}`))
	if !result.OK {
		t.Fatalf("result = %#v", result)
	}
	if _, statErr := os.Stat(stale); !os.IsNotExist(statErr) {
		t.Fatalf("stale temporary survived: %v", statErr)
	}
}

// TestApplyPatchLeavesOriginalIntactWhenTempCreationFails 锁定原子性：写入
// 前置步骤失败时目标文件保持原内容。
func TestApplyPatchLeavesOriginalIntactWhenTempCreationFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}
	root := t.TempDir()
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "file.txt")
	if err := os.WriteFile(target, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(root, 0o700) }()
	tool := ApplyPatch{Workspace: workspace}
	result := tool.Execute(context.Background(), json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"`+sha256Hex([]byte("original"))+`","replacements":[{"old":"original","new":"tampered"}]}`))
	if result.OK || result.Code != "io" {
		t.Fatalf("result = %#v", result)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "original" {
		t.Fatalf("original content was modified: %q %v", content, err)
	}
}

// sha256Hex 返回测试用裸十六进制摘要。
func sha256Hex(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
