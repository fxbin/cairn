package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRenderTemplateExplicitParams 拒绝未知参数与缺参，禁止 shell 展开。
func TestRenderTemplateExplicitParams(t *testing.T) {
	doc := TemplateDocument{
		Name:       "review-pr",
		Parameters: []TemplateParameter{{Name: "path", Required: true}},
		Body:       "请审查 {{path}}；忽略 $(whoami) 与 `env`。",
	}
	out, err := RenderTemplate(doc, map[string]string{"path": "src/a.go"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "src/a.go") || strings.Contains(out, "{{path}}") {
		t.Fatalf("unexpected render: %q", out)
	}
	if strings.Contains(out, "root") && strings.Contains(out, "$(whoami)") {
		t.Fatalf("must not expand shell: %q", out)
	}
	if _, err := RenderTemplate(doc, map[string]string{"other": "x"}); err == nil {
		t.Fatal("unknown parameter must fail")
	}
	if _, err := RenderTemplate(doc, map[string]string{}); err == nil {
		t.Fatal("missing required parameter must fail")
	}
}

// TestRenderTemplateRejectsUndeclaredPlaceholder 正文引用未声明参数时失败关闭。
func TestRenderTemplateRejectsUndeclaredPlaceholder(t *testing.T) {
	if _, err := parseTemplate([]byte("---\nname: bad\nparameters: []\n---\nHello {{secret}}\n"), "bad"); err == nil {
		t.Fatal("undeclared placeholder must fail")
	}
}

// TestTemplateStoreSaveLoadRoundTrip 原子保存后可读回且摘要稳定。
func TestTemplateStoreSaveLoadRoundTrip(t *testing.T) {
	root := t.TempDir()
	store, err := NewTemplateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	doc := TemplateDocument{
		Name:        "fix-login",
		Description: "修复登录",
		Parameters:  []TemplateParameter{{Name: "file", Required: true}},
		Body:        "定位 {{file}} 中的登录问题。",
	}
	if err := store.Save(doc); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load("fix-login")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Body != doc.Body || len(loaded.Parameters) != 1 || loaded.SHA256 == "" {
		t.Fatalf("unexpected loaded: %+v", loaded)
	}
	if err := store.Save(doc); err == nil {
		t.Fatal("duplicate save must fail closed")
	}
	catalog, err := store.List()
	if err != nil || len(catalog.Entries) != 1 {
		t.Fatalf("catalog: %+v %v", catalog, err)
	}
}

// TestTemplateStoreListDiagnostics 无效条目进入诊断，不阻断其余发现。
func TestTemplateStoreListDiagnostics(t *testing.T) {
	root := t.TempDir()
	store, err := NewTemplateStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "broken.md"), []byte("no frontmatter {{x}}"), 0o600); err != nil {
		t.Fatal(err)
	}
	good := TemplateDocument{Name: "ok", Body: "plain text"}
	if err := store.Save(good); err != nil {
		t.Fatal(err)
	}
	catalog, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog.Entries) != 1 || len(catalog.Diagnostics) == 0 {
		t.Fatalf("expected 1 entry + diagnostics, got %+v", catalog)
	}
}

// TestValidTemplateName 拒绝路径字符与空名。
func TestValidTemplateName(t *testing.T) {
	for _, name := range []string{"", "../evil", "a/b", "A", "a--b", strings.Repeat("n", 65)} {
		if ValidTemplateName(name) {
			t.Fatalf("expected reject %q", name)
		}
	}
	if !ValidTemplateName("fix-login") {
		t.Fatal("expected accept fix-login")
	}
}
