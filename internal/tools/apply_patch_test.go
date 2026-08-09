package tools

import (
	"context"
	"encoding/json"
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
	plan, err := tool.Plan(json.RawMessage(`{"path":"file.txt","mode":"update","expected_sha256":"sha256:2268f0a9ad9a5a3e7b413dc6e50f094cbf05ecb89f9f87f7a7300c13e74d5a51","replacements":[{"old":"one","new":"three","occurrence":2}]}`))
	if err == nil || plan.Path != "" {
		t.Fatalf("expected stale hash rejection, got %#v, %v", plan, err)
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
