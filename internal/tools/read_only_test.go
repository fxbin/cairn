package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadOnlyToolsRespectWorkspaceAndReturnStableData(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "internal"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "main.go"), []byte("package internal\n// RunCompleted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".git", "hidden"), []byte("hidden"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=not-for-agent"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	listed := (ListFiles{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"path":".","glob":"**/*.go"}`))
	if !listed.OK || string(listed.Data) != `{"files":["internal/main.go"]}` {
		t.Fatalf("list result = %#v", listed)
	}
	read := (ReadFile{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"path":"internal/main.go"}`))
	if !read.OK || read.SHA256 == "" {
		t.Fatalf("read result = %#v", read)
	}
	search := (SearchText{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"query":"RunCompleted","glob":"**/*.go"}`))
	if !search.OK || string(search.Data) == "" {
		t.Fatalf("search result = %#v", search)
	}
	escaped := (ReadFile{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"path":"../outside"}`))
	if escaped.OK || escaped.Code != "invalid_path" {
		t.Fatalf("escape result = %#v", escaped)
	}
	secret := (ReadFile{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"path":".env"}`))
	if secret.OK || secret.Code != "ignored_path" {
		t.Fatalf("secret result = %#v", secret)
	}
}

func TestReadOnlyToolsRejectUnknownArguments(t *testing.T) {
	workspace, err := NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	result := (ListFiles{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"unexpected":true}`))
	if result.OK || result.Code != "invalid" {
		t.Fatalf("result = %#v", result)
	}
}

func TestReadOnlyToolDefaultsBoundContextPayload(t *testing.T) {
	root := t.TempDir()
	for index := 0; index < defaultListResults+1; index++ {
		path := filepath.Join(root, fmt.Sprintf("file-%03d.txt", index))
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	content := strings.Repeat("x", defaultReadLimit+1)
	if err := os.WriteFile(filepath.Join(root, "large.txt"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, int64(defaultReadLimit*2), 1000, 1024*1024)
	if err != nil {
		t.Fatal(err)
	}
	listed := (ListFiles{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"path":"."}`))
	if !listed.OK || !listed.Truncated {
		t.Fatalf("list result = %#v", listed)
	}
	var files struct {
		Files []string `json:"files"`
	}
	if err := json.Unmarshal(listed.Data, &files); err != nil {
		t.Fatal(err)
	}
	if len(files.Files) != defaultListResults {
		t.Fatalf("default list length = %d, want %d", len(files.Files), defaultListResults)
	}
	read := (ReadFile{Workspace: workspace}).Execute(context.Background(), json.RawMessage(`{"path":"large.txt"}`))
	if !read.OK || !read.Truncated {
		t.Fatalf("read result = %#v", read)
	}
	var file struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(read.Data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Content) != defaultReadLimit {
		t.Fatalf("default read length = %d, want %d", len(file.Content), defaultReadLimit)
	}
}

func TestWorkspaceObservationTracksVisibleFilesButExcludesSensitivePaths(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=first"), 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := workspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, ".env"), []byte("TOKEN=changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterSecretChange, err := workspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if afterSecretChange != initial {
		t.Fatalf("sensitive path changed workspace observation: %q != %q", afterSecretChange, initial)
	}
	if err := os.WriteFile(filepath.Join(root, "visible.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	afterVisibleChange, err := workspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if afterVisibleChange == initial {
		t.Fatal("visible workspace change did not alter observation")
	}
}

func TestObservationWorkspaceIncludesToolIgnoredPathsAndTracksSymlinks(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "node_modules"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "dependency.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "target.txt"), []byte("target"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tmp", "volatile.txt"), []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolWorkspace, err := NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	verificationWorkspace := toolWorkspace.ObservationView([]string{"tmp"})
	toolBefore, err := toolWorkspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	verificationBefore, err := verificationWorkspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "node_modules", "dependency.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	toolAfterDependency, err := toolWorkspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	verificationAfterDependency, err := verificationWorkspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if toolAfterDependency != toolBefore {
		t.Fatal("tool workspace observation unexpectedly includes node_modules")
	}
	if verificationAfterDependency == verificationBefore {
		t.Fatal("verification workspace observation did not include node_modules")
	}
	if err := os.Remove(filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("node_modules/dependency.txt", filepath.Join(root, "link.txt")); err != nil {
		t.Fatal(err)
	}
	verificationAfterLinkChange, err := verificationWorkspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if verificationAfterLinkChange == verificationAfterDependency {
		t.Fatal("symlink target change did not alter verification observation")
	}
	if err := os.WriteFile(filepath.Join(root, "tmp", "volatile.txt"), []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}
	verificationAfterVolatileChange, err := verificationWorkspace.Observation()
	if err != nil {
		t.Fatal(err)
	}
	if verificationAfterVolatileChange != verificationAfterLinkChange {
		t.Fatal("configured volatile path changed verification observation")
	}
}
