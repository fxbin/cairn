package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func packageDigestOf(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// writePackageSource 构造带清单的内容包源目录。
func writePackageSource(t *testing.T, name, version string, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	var builder strings.Builder
	builder.WriteString("version: 1\nname: " + name + "\npackage: \"" + version + "\"\nresources:\n")
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		raw := []byte(content)
		if err := os.WriteFile(full, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		builder.WriteString("  - path: " + path + "\n    sha256: \"" + packageDigestOf(raw) + "\"\n")
	}
	if err := os.WriteFile(filepath.Join(root, PackageManifestFile), []byte(builder.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestParsePackageManifestRejectsTraversal 验证路径穿越与可执行资源失败关闭。
func TestParsePackageManifestRejectsTraversal(t *testing.T) {
	for _, path := range []string{"../etc/passwd", "/abs", "a/../../b", "install.sh", "hook.yaml"} {
		raw := []byte("version: 1\nname: demo\npackage: \"1.0\"\nresources:\n  - path: " + path + "\n    sha256: " + strings.Repeat("a", 64) + "\n")
		if _, err := ParsePackageManifest(raw); err == nil {
			t.Fatalf("expected reject %q", path)
		}
	}
}

// TestInspectAndInstallPackage 验证只读校验与原子安装。
func TestInspectAndInstallPackage(t *testing.T) {
	source := writePackageSource(t, "demo-pack", "1.2.0", map[string]string{
		"skills/demo/SKILL.md": "# demo",
		"templates/fix.md":     "---\nname: fix\n---\nbody",
	})
	manifest, err := InspectPackage(source)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Name != "demo-pack" || len(manifest.Resources) != 2 {
		t.Fatalf("unexpected manifest %+v", manifest)
	}
	store, err := NewPackageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	entry, err := store.InstallPackage(source)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "demo-pack" || entry.Files != 2 {
		t.Fatalf("unexpected entry %+v", entry)
	}
	if _, err := store.InstallPackage(source); err == nil {
		t.Fatal("duplicate install must fail closed")
	}
	items, err := store.ListPackages()
	if err != nil || len(items) != 1 || items[0].Name != "demo-pack" {
		t.Fatalf("list %+v %v", items, err)
	}
	if err := store.SetPackageEnabled("demo-pack", false); err != nil {
		t.Fatal(err)
	}
	items, _ = store.ListPackages()
	if len(items) != 1 || !items[0].Disabled {
		t.Fatalf("expected disabled package %+v", items)
	}
}

// TestInspectPackageRejectsDigestMismatch 验证摘要漂移失败关闭。
func TestInspectPackageRejectsDigestMismatch(t *testing.T) {
	source := writePackageSource(t, "bad-pack", "1.0.0", map[string]string{"a.md": "hello"})
	raw := []byte("version: 1\nname: bad-pack\npackage: \"1.0\"\nresources:\n  - path: a.md\n    sha256: " + strings.Repeat("b", 64) + "\n")
	if err := os.WriteFile(filepath.Join(source, PackageManifestFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPackage(source); err == nil {
		t.Fatal("expected digest mismatch")
	}
}
