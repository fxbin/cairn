package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/fxbin/cairn/internal/config"
)

// TestSkillsWithConfigMountsSkillshareAndExtraDirs 锁定只读外部源挂载。
func TestSkillsWithConfigMountsSkillshareAndExtraDirs(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// 伪造 skillshare 源目录（目录名必须与 frontmatter name 一致）
	share := filepath.Join(home, ".config", "skillshare", "skills", "demo")
	if err := os.MkdirAll(share, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(share, "SKILL.md"), []byte("---\nname: demo\ndescription: demo skill\n---\n\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	extra := filepath.Join(t.TempDir(), "ext")
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "SKILL.md"), []byte("---\nname: ext\ndescription: external\n---\n\nx\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workspace := t.TempDir()
	service := Service{}
	catalog, err := service.SkillsWithConfig(workspace, config.SkillsConfig{
		Enabled:    true,
		Skillshare: true,
		ExtraDirs:  []string{filepath.Dir(extra)},
	})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, entry := range catalog.Entries() {
		names[entry.Name] = entry.Scope
	}
	if scope, ok := names["demo"]; !ok || scope != "skillshare" {
		t.Fatalf("skillshare skill missing or wrong scope: %+v", names)
	}
	if scope, ok := names["ext"]; !ok || scope != "external" {
		t.Fatalf("external skill missing or wrong scope: %+v", names)
	}
}
