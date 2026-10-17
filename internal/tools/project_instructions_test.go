package tools

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/artifact"
)

// writeProjectInstructionTestFile 在隔离目录写入规约夹具，返回实际文件位置。
func writeProjectInstructionTestFile(t *testing.T, root, relative, content string) string {
	t.Helper()
	name := filepath.Join(root, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	return name
}

// projectInstructionsTestWorkspace 只使用测试自有文件，避免继承本机仓库规约。
func projectInstructionsTestWorkspace(t *testing.T, root string, ignored ...string) Workspace {
	t.Helper()
	workspace, err := NewWorkspace(root, ignored, 0, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	return workspace
}

// TestProjectInstructionsDiscoveryScopesAndRedaction 锁定祖先顺序、兄弟作用域、忽略目录、空文件和源正文脱敏。
func TestProjectInstructionsDiscoveryScopesAndRedaction(t *testing.T) {
	root := t.TempDir()
	const secret = "synthetic-private-value-123456789"
	for name, content := range map[string]string{
		"AGENTS.md":                     "# 根规约\nAPI_KEY=" + secret + "\n",
		"z/AGENTS.md":                   "# Z 规则",
		"a/AGENTS.md":                   "# A 规则",
		"a/deep/AGENTS.md":              "# 更深规则",
		"empty/AGENTS.md":               " \n\t\r\n",
		"AGENTS.override.md":            "不是标准来源",
		"ignored/AGENTS.md":             "不应加载",
		"nested/node_modules/AGENTS.md": "不应加载",
		"nested/vendor/AGENTS.md":       "不应加载",
		".agent/AGENTS.md":              "不应加载",
		".git/AGENTS.md":                "不应加载",
		".vidt/cache/AGENTS.md":         "不应加载",
		".ssh/AGENTS.md":                "不应加载",
	} {
		writeProjectInstructionTestFile(t, root, name, content)
	}
	workspace := projectInstructionsTestWorkspace(t, root, "ignored")
	first, err := LoadProjectInstructions(workspace)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadProjectInstructions(workspace)
	if err != nil || !reflect.DeepEqual(first, second) {
		t.Fatalf("discovery is not deterministic: %v", err)
	}
	var sources, scopes []string
	for _, document := range first.Documents {
		sources = append(sources, document.Path)
		scopes = append(scopes, document.Scope)
	}
	if !reflect.DeepEqual(sources, []string{"AGENTS.md", "a/AGENTS.md", "z/AGENTS.md", "a/deep/AGENTS.md"}) || !reflect.DeepEqual(scopes, []string{".", "a", "z", "a/deep"}) {
		t.Fatalf("incorrect instruction scope chain: sources=%v scopes=%v", sources, scopes)
	}
	encoded, err := json.Marshal(first)
	if err != nil || strings.Contains(string(encoded), secret) || !strings.Contains(first.Documents[0].Content, "[REDACTED]") || first.Documents[0].SHA256 == first.Documents[0].SourceSHA256 {
		t.Fatal("source secret leaked or redacted/source identities collapsed")
	}
}

// TestProjectInstructionsRejectInvalidAndOversizedSources 无效或超限规约不得被部分加载或截断后继续运行。
func TestProjectInstructionsRejectInvalidAndOversizedSources(t *testing.T) {
	for _, test := range []struct {
		name  string
		files map[string]string
	}{
		{"invalid-utf8", map[string]string{"AGENTS.md": string([]byte{0xff})}},
		{"control-character", map[string]string{"AGENTS.md": "# rule\x1b[31m"}},
		{"one-file-limit", map[string]string{"AGENTS.md": strings.Repeat("x", ProjectInstructionsMaxBytes+1)}},
		{"combined-limit", map[string]string{"AGENTS.md": strings.Repeat("r", ProjectInstructionsMaxBytes/2+1), "sub/AGENTS.md": strings.Repeat("s", ProjectInstructionsMaxBytes/2)}},
		{"redacted-root-limit", map[string]string{"AGENTS.md": "API_KEY=" + strings.Repeat("a", 30000), "sub/AGENTS.md": strings.Repeat("b", 3000)}},
		{"redacted-child-limit", map[string]string{"AGENTS.md": strings.Repeat("b", 3000), "sub/AGENTS.md": "API_KEY=" + strings.Repeat("a", 30000)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			for name, content := range test.files {
				writeProjectInstructionTestFile(t, root, name, content)
			}
			if _, err := LoadProjectInstructions(projectInstructionsTestWorkspace(t, root)); err == nil {
				t.Fatal("invalid instruction source was accepted")
			}
		})
	}
	t.Run("document-limit", func(t *testing.T) {
		root := t.TempDir()
		for index := 0; index <= ProjectInstructionsMaxDocuments; index++ {
			writeProjectInstructionTestFile(t, root, fmt.Sprintf("scope-%03d/AGENTS.md", index), "rule")
		}
		if _, err := LoadProjectInstructions(projectInstructionsTestWorkspace(t, root)); err == nil {
			t.Fatal("oversized instruction set was accepted")
		}
	})
}

// TestProjectInstructionsSymlinkBoundaries 文件链接受可见工作区约束，目录链接不能引入外部或重复作用域。
func TestProjectInstructionsSymlinkBoundaries(t *testing.T) {
	for _, kind := range []string{"escape-file", "ignored-file", "safe-file", "directory"} {
		t.Run(kind, func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			target := writeProjectInstructionTestFile(t, outside, "AGENTS.md", "OUTSIDE_RULE")
			link := filepath.Join(root, "AGENTS.md")
			switch kind {
			case "ignored-file":
				target = writeProjectInstructionTestFile(t, root, ".agent/guide.md", "PRIVATE_RULE")
			case "safe-file":
				target = writeProjectInstructionTestFile(t, root, "guide.md", "SAFE_RULE")
			case "directory":
				target, link = outside, filepath.Join(root, "linked")
			}
			if err := os.Symlink(target, link); err != nil {
				t.Skip(err)
			}
			snapshot, err := LoadProjectInstructions(projectInstructionsTestWorkspace(t, root))
			switch kind {
			case "escape-file", "ignored-file":
				if err == nil {
					t.Fatal("unsafe file link loaded")
				}
			case "directory":
				if err != nil || len(snapshot.Documents) != 0 {
					t.Fatalf("directory link was traversed: %v", err)
				}
			case "safe-file":
				if err != nil || len(snapshot.Documents) != 1 || snapshot.Documents[0].Path != "AGENTS.md" || snapshot.Documents[0].Content != "SAFE_RULE" {
					t.Fatalf("confined file alias failed: %v", err)
				}
			}
		})
	}
}

// TestProjectInstructionsSnapshotIntegrity 历史读取独立于当前文件，拒绝作用域、正文、结构或内容寻址引用的损坏。
func TestProjectInstructionsSnapshotIntegrity(t *testing.T) {
	root := t.TempDir()
	source := writeProjectInstructionTestFile(t, root, "AGENTS.md", "HISTORICAL_RULE")
	snapshot, err := LoadProjectInstructions(projectInstructionsTestWorkspace(t, root))
	if err != nil {
		t.Fatal(err)
	}
	store, err := artifact.New(t.TempDir(), 256)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := store.Put(encoded, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	rawReference, _ := json.Marshal(reference)
	if err := os.Remove(source); err != nil {
		t.Fatal(err)
	}
	loaded, err := ReadProjectInstructions(store, rawReference)
	if err != nil || !reflect.DeepEqual(loaded, snapshot) {
		t.Fatalf("historical snapshot reread a missing source: %v", err)
	}
	for name, content := range map[string][]byte{
		"body-digest": bytesReplaceProjectInstruction(encoded, "HISTORICAL_RULE", "CHANGED_RULE"),
		"scope":       bytesReplaceProjectInstruction(encoded, `"scope":"."`, `"scope":"other"`),
		"unknown":     []byte(`{"version":1,"documents":[],"unknown":true}`),
		"trailing":    append(append([]byte(nil), encoded...), []byte("\n{}")...),
	} {
		t.Run(name, func(t *testing.T) {
			badRef, err := store.Put(content, "application/json")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(badRef)
			if _, err := ReadProjectInstructions(store, raw); err == nil {
				t.Fatal("corrupt snapshot accepted")
			}
		})
	}
	for _, raw := range []json.RawMessage{json.RawMessage("null"), json.RawMessage(`{"sha256":"bad"}`)} {
		if _, err := ReadProjectInstructions(store, raw); err == nil {
			t.Fatal("invalid snapshot reference accepted")
		}
	}
	digest := strings.TrimPrefix(reference.SHA256, "sha256:")
	entity := filepath.Join(store.Root, "sha256", digest[:2], digest)
	if err := os.WriteFile(entity, []byte(strings.Repeat("x", ProjectInstructionsMaxSnapshotBytes+1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenBounded(reference, ProjectInstructionsMaxSnapshotBytes); err == nil || !strings.Contains(err.Error(), "read limit") {
		t.Fatalf("oversized cached snapshot reached hash checking before a read bound: %v", err)
	}
	if _, err := ReadProjectInstructions(store, rawReference); err == nil {
		t.Fatal("oversized snapshot entity accepted despite its small declared size")
	}
	if err := os.WriteFile(entity, []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadProjectInstructions(store, rawReference); err == nil {
		t.Fatal("artifact content tampering accepted")
	}
}

// bytesReplaceProjectInstruction 仅篡改指定合同字段，不经反射或 JSON 往返重建夹具。
func bytesReplaceProjectInstruction(content []byte, old, replacement string) []byte {
	return []byte(strings.Replace(string(content), old, replacement, 1))
}

// TestUserProjectInstructionsSourceAndLegacy 验证全局来源标签、同名根文件组合、受限发现和旧版快照兼容。
func TestUserProjectInstructionsSourceAndLegacy(t *testing.T) {
	root, userDirectory := t.TempDir(), t.TempDir()
	writeProjectInstructionTestFile(t, root, "AGENTS.md", "ROOT_RULE")
	writeProjectInstructionTestFile(t, root, "sub/AGENTS.md", "CHILD_RULE")
	writeProjectInstructionTestFile(t, userDirectory, "AGENTS.md", "USER_RULE\nAPI_KEY=synthetic-user-secret-123456789")
	writeProjectInstructionTestFile(t, userDirectory, "nested/AGENTS.md", string([]byte{0xff}))
	workspace := projectInstructionsTestWorkspace(t, root)
	snapshot, err := LoadProjectInstructionsWithUser(workspace, userDirectory)
	if err != nil || len(snapshot.Documents) != 3 || snapshot.Documents[0].Source != ProjectInstructionUser || snapshot.Documents[1].Source != "" || snapshot.Documents[1].Path != "AGENTS.md" || snapshot.Documents[2].Scope != "sub" {
		t.Fatalf("user/project source chain failed: %+v %v", snapshot, err)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil || strings.Contains(string(encoded), userDirectory) || strings.Contains(string(encoded), "synthetic-user-secret-123456789") {
		t.Fatal("absolute user directory or secret entered the snapshot")
	}
	for _, mutation := range []string{"unknown-source", "nested-user", "legacy-user", "duplicate-user", "user-last", "unknown-version"} {
		t.Run(mutation, func(t *testing.T) {
			bad := ProjectInstructions{Version: snapshot.Version, Documents: append([]ProjectInstruction(nil), snapshot.Documents...)}
			switch mutation {
			case "unknown-source":
				bad.Documents[0].Source = "unknown"
			case "nested-user":
				bad.Documents[0].Path, bad.Documents[0].Scope = "sub/AGENTS.md", "sub"
			case "legacy-user":
				bad.Version = 1
			case "duplicate-user":
				bad.Documents = append([]ProjectInstruction{bad.Documents[0]}, bad.Documents...)
			case "user-last":
				bad.Documents[0], bad.Documents[1] = bad.Documents[1], bad.Documents[0]
			case "unknown-version":
				bad.Version = ProjectInstructionsVersion + 1
			}
			if bad.Validate() == nil {
				t.Fatal("invalid source contract accepted")
			}
		})
	}
	legacy, err := LoadProjectInstructions(workspace)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Version = 1
	store, err := artifact.New(t.TempDir(), 256)
	if err != nil {
		t.Fatal(err)
	}
	for _, frozen := range []ProjectInstructions{snapshot, legacy} {
		raw, _ := json.Marshal(frozen)
		reference, err := store.Put(raw, "application/json")
		if err != nil {
			t.Fatal(err)
		}
		rawReference, _ := json.Marshal(reference)
		loaded, err := ReadProjectInstructions(store, rawReference)
		if err != nil || !reflect.DeepEqual(loaded, frozen) {
			t.Fatalf("snapshot did not round trip: %v", err)
		}
	}
}

// TestUserProjectInstructionsBoundaries 全局文件同样受链接、格式及共享原文字节预算约束，不扫描用户目录树。
func TestUserProjectInstructionsBoundaries(t *testing.T) {
	for _, kind := range []string{"missing", "missing-directory", "blank", "invalid-utf8", "directory", "escape-link", "credential-link", "safe-link", "raw-combined-limit", "blank-combined-limit"} {
		t.Run(kind, func(t *testing.T) {
			root, userDirectory := t.TempDir(), t.TempDir()
			writeProjectInstructionTestFile(t, root, "AGENTS.md", strings.Repeat("r", 3000))
			name := filepath.Join(userDirectory, "AGENTS.md")
			switch kind {
			case "missing-directory":
				userDirectory = filepath.Join(userDirectory, "absent")
			case "blank":
				writeProjectInstructionTestFile(t, userDirectory, "AGENTS.md", " \n\t")
			case "invalid-utf8":
				writeProjectInstructionTestFile(t, userDirectory, "AGENTS.md", string([]byte{0xff}))
			case "directory":
				if err := os.Mkdir(name, 0700); err != nil {
					t.Fatal(err)
				}
			case "escape-link", "credential-link", "safe-link":
				targetRoot, relative := t.TempDir(), "guide.md"
				if kind != "escape-link" {
					targetRoot = userDirectory
				}
				if kind == "credential-link" {
					relative = "keys.env"
				}
				target := writeProjectInstructionTestFile(t, targetRoot, relative, "LINK_RULE")
				if err := os.Symlink(target, name); err != nil {
					t.Skip(err)
				}
			case "raw-combined-limit":
				writeProjectInstructionTestFile(t, userDirectory, "AGENTS.md", "API_KEY="+strings.Repeat("s", 30000))
			case "blank-combined-limit":
				writeProjectInstructionTestFile(t, userDirectory, "AGENTS.md", strings.Repeat(" ", 30000))
			}
			snapshot, err := LoadProjectInstructionsWithUser(projectInstructionsTestWorkspace(t, root), userDirectory)
			valid := kind == "missing" || kind == "missing-directory" || kind == "blank" || kind == "safe-link"
			if !valid {
				if err == nil {
					t.Fatal("invalid global source accepted")
				}
				return
			}
			expected := 1
			if kind == "safe-link" {
				expected = 2
			}
			if err != nil || len(snapshot.Documents) != expected {
				t.Fatalf("optional or confined global source failed: %v", err)
			}
		})
	}
}
