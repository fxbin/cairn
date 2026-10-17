package tui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
)

// TestUserRootWizardPreservesScope 用户配置保持用户级编辑入口，读写共用统一目标。
func TestUserRootWizardPreservesScope(t *testing.T) {
	home := t.TempDir()
	for _, key := range []string{"HOME", "USERPROFILE", "APPDATA", "XDG_CONFIG_HOME"} {
		t.Setenv(key, home)
	}
	path, err := config.UserConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 2\n"), 0600); err != nil {
		t.Fatal(err)
	}
	console := RunConsole{input: RunConsoleInput{Workspace: t.TempDir(), Config: config.Defaults()}}
	input := console.nextSetupInput()
	if !input.ScopeUserConfig {
		t.Fatal("user config was treated as workspace config")
	}
	if got, err := targetConfigPath(input); err != nil || got != path {
		t.Fatal("unexpected wizard write target")
	}
	if got, err := resolveExistingConfig(input); err != nil || got != path {
		t.Fatal("unexpected wizard read target")
	}
	wizard := NewConfigWizard(context.Background(), app.SetupInput{Workspace: input.Workspace, ScopeUserConfig: true})
	if !strings.Contains(wizard.existingConfigTip, "将覆盖") || !strings.Contains(wizard.existingConfigTip, path) {
		t.Fatal("wizard does not explain existing target")
	}
}
