package userdir

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestUserRootPaths 验证统一根、纯路径解析和资源名称边界。
func TestUserRootPaths(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, home)
	}
	for _, name := range []string{"config.yaml", "keys.env", "AGENTS.md", "skills", "mcp-auth"} {
		got, err := Path(name)
		if err != nil || got != filepath.Join(home, ".cairn", name) {
			t.Fatalf("unexpected user resource path: %q %v", got, err)
		}
	}
	if _, err := os.Stat(filepath.Join(home, ".cairn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("path resolution created state")
	}
	for _, name := range []string{"", ".", "../config.yaml", filepath.Join(home, "config.yaml")} {
		if _, err := Path(name); err == nil {
			t.Fatal("unsafe resource name accepted")
		}
	}
}
