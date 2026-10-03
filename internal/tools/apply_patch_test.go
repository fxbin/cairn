package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

// TestPublishPatchPreservesTargetOnTemporaryFailure 验证每类临时文件失败均阻断发布并释放资源。
func TestPublishPatchPreservesTargetOnTemporaryFailure(t *testing.T) {
	for _, failure := range []string{"chmod", "write", "partial_write", "short_write", "sync", "close"} {
		for _, exists := range []bool{false, true} {
			name := "create"
			if exists {
				name = "update"
			}
			t.Run(failure+"/"+name, func(t *testing.T) {
				root := t.TempDir()
				target := filepath.Join(root, "file.txt")
				if exists {
					if err := os.WriteFile(target, []byte("original"), 0o640); err != nil {
						t.Fatal(err)
					}
				}
				file, err := os.CreateTemp(root, ".cairn-patch-*")
				if err != nil {
					t.Fatal(err)
				}
				injected := errors.New("temporary " + failure + " failure")
				temporary := &failingPatchTemporary{File: file, failure: failure, failureErr: injected}
				err = publishPatch(temporary, target, 0o640, []byte("replacement"))
				wanted := injected
				if failure == "short_write" {
					wanted = io.ErrShortWrite
				}
				if !errors.Is(err, wanted) {
					t.Errorf("发布未返回预期故障: got=%v want=%v", err, wanted)
				}
				content, readErr := os.ReadFile(target)
				if exists {
					if readErr != nil || string(content) != "original" {
						t.Errorf("原文件被替换: content=%q err=%v", content, readErr)
					}
					info, statErr := os.Stat(target)
					if statErr != nil {
						t.Fatal(statErr)
					}
					if info.Mode().Perm() != 0o640 {
						t.Errorf("原文件权限被修改: %v", info.Mode().Perm())
					}
				} else if !errors.Is(readErr, os.ErrNotExist) {
					t.Errorf("失败后仍发布新文件: content=%q err=%v", content, readErr)
				}
				if _, statErr := os.Stat(file.Name()); !errors.Is(statErr, os.ErrNotExist) {
					t.Errorf("临时文件未清理: %v", statErr)
				}
				if _, statErr := file.Stat(); !errors.Is(statErr, os.ErrClosed) {
					t.Errorf("临时文件句柄未关闭: %v", statErr)
				}
			})
		}
	}
}

// failingPatchTemporary 在真实临时文件上注入单步失败，保留实际写入和重命名行为。
type failingPatchTemporary struct {
	*os.File
	failure    string
	failureErr error
}

// Chmod 可拒绝权限修改，使发布路径验证此失败不会进入写入。
func (temporary *failingPatchTemporary) Chmod(mode os.FileMode) error {
	if temporary.failure == "chmod" {
		return temporary.failureErr
	}
	return temporary.File.Chmod(mode)
}

// Write 可产生零写入或真实部分写入，分别覆盖带错误和无错误的短写。
func (temporary *failingPatchTemporary) Write(content []byte) (int, error) {
	if temporary.failure == "write" {
		return 0, temporary.failureErr
	}
	if temporary.failure == "partial_write" || temporary.failure == "short_write" {
		written, err := temporary.File.Write(content[:len(content)/2])
		if err == nil && temporary.failure == "partial_write" {
			err = temporary.failureErr
		}
		return written, err
	}
	return temporary.File.Write(content)
}

// Sync 可拒绝落盘，验证已写完整的临时内容也不会提前发布。
func (temporary *failingPatchTemporary) Sync() error {
	if temporary.failure == "sync" {
		return temporary.failureErr
	}
	return temporary.File.Sync()
}

// Close 始终关闭真实句柄，再按用例返回关闭故障。
func (temporary *failingPatchTemporary) Close() error {
	err := temporary.File.Close()
	if err == nil && temporary.failure == "close" {
		return temporary.failureErr
	}
	return err
}
