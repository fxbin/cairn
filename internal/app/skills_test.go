package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
)

// TestSkillBootOptInAndDoctorReadOnly 显式配置才装配技能，doctor 不扫描无效来源或创建管理状态。
func TestSkillBootOptInAndDoctorReadOnly(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	workspace := t.TempDir()
	state := t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	boot, err := service.assembleBoot(workspace, state, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, spec := range boot.Registry.Specs() {
		if spec.Name == "skill" {
			t.Fatal("unconfigured skills assembled")
		}
	}
	if err := boot.Close(); err != nil {
		t.Fatal(err)
	}
	configuration.Skills = &config.SkillsConfig{Enabled: true}
	boot, err = service.assembleBoot(workspace, state, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, spec := range boot.Registry.Specs() {
		found = found || spec.Name == "skill"
	}
	if !found {
		t.Fatal("enabled skills not assembled")
	}
	if err := boot.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(workspace, ".agent"), 0700); err != nil && !os.IsExist(err) {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, ".agent", "skills"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := service.Doctor(context.Background(), workspace, t.TempDir(), configuration)
	if err != nil || !slices.Contains(report.Tools, "skill") {
		t.Fatalf("doctor scanned skill source or omitted tool: %v %#v", err, report.Tools)
	}
}

// TestSkillWorkspaceManagementRejectsSymlinkEscape 目标目录不能经 .agent 或 .agents 逃到工作区外。
func TestSkillWorkspaceManagementRejectsSymlinkEscape(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	service := Service{}
	workspace, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, ".agent")); err != nil {
		t.Skip(err)
	}
	if _, err := service.SkillStoreFor(workspace, "workspace"); err == nil {
		t.Fatal("native skill store escaped workspace")
	}
	if err := os.Remove(filepath.Join(workspace, ".agent")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, ".agents")); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Skills(workspace, true); err == nil {
		t.Fatal("shared skill source escaped workspace")
	}
}
