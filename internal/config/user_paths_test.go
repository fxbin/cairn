package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestUserRootConfigSelection 验证用户配置路径、工作区优先与非法优先来源失败关闭。
func TestUserRootConfigSelection(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, home)
	}
	workspace := t.TempDir()
	if ResolveDefaultPath(workspace) != DefaultPath(workspace) {
		t.Fatal("missing config should resolve to workspace target")
	}
	userPath, err := UserConfigPath()
	if err != nil || userPath != filepath.Join(home, ".cairn", "config.yaml") {
		t.Fatalf("unexpected user config path: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(userPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if ResolveDefaultPath(workspace) != userPath {
		t.Fatal("user config is not selected")
	}
	if err := os.MkdirAll(DefaultPath(workspace), 0700); err != nil {
		t.Fatal(err)
	}
	if ResolveDefaultPath(workspace) != DefaultPath(workspace) {
		t.Fatal("workspace source lost priority")
	}
	if _, err := Load(ResolveDefaultPath(workspace), Overrides{}); err == nil {
		t.Fatal("invalid workspace source accepted")
	}
	if err := os.Remove(DefaultPath(workspace)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userPath, []byte("version: [invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if ResolveDefaultPath(workspace) != userPath {
		t.Fatal("invalid user source lost priority")
	}
	if _, err := Load(ResolveDefaultPath(workspace), Overrides{}); err == nil {
		t.Fatal("invalid user source accepted")
	}
}
