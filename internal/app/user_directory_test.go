package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
)

// writeUserRootFixture 写入隔离用户目录中的资源夹具。
func writeUserRootFixture(t *testing.T, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

// TestUserRootInstructionsAndSkills 验证全局规约与用户原生技能共用统一根。
func TestUserRootInstructionsAndSkills(t *testing.T) {
	instructions := userProjectInstructionsTestPath(t)
	writeUserRootFixture(t, instructions, "USER_ROOT_GLOBAL_RULE")
	workspace := t.TempDir()
	provider := &projectInstructionsCaptureProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	options := RunOptions{Task: "task", Workspace: workspace, StateDir: t.TempDir(), Config: withTestProvider(config.Defaults())}
	if _, err := service.Run(context.Background(), options); err != nil || len(provider.requests) != 1 {
		t.Fatalf("user guidance run failed: %v", err)
	}
	if !strings.Contains(provider.requests[0].Messages[1].Content, "USER_ROOT_GLOBAL_RULE") {
		t.Fatal("user guidance missing")
	}
	writeUserRootFixture(t, instructions, string([]byte{0xff}))
	if _, err := service.Run(context.Background(), options); err == nil || len(provider.requests) != 1 {
		t.Fatal("invalid global source accepted")
	}
	store, err := service.SkillStoreFor(workspace, "user")
	if err != nil || store.Root != filepath.Join(filepath.Dir(instructions), "skills") {
		t.Fatalf("unexpected skill root: %v", err)
	}
	source := filepath.Join(t.TempDir(), "root-skill")
	writeUserRootFixture(t, filepath.Join(source, "SKILL.md"), "---\nname: root-skill\ndescription: user root skill\n---\nbody")
	if _, err := store.Install(source); err != nil {
		t.Fatal(err)
	}
	catalog, err := service.Skills(workspace, false)
	if err != nil || len(catalog.Entries()) != 1 || catalog.Entries()[0].Name != "root-skill" {
		t.Fatalf("user skills missing: %v", err)
	}
	if err := store.SetEnabled("root-skill", false); err != nil {
		t.Fatal(err)
	}
	catalog, err = service.Skills(workspace, false)
	if err != nil || len(catalog.Entries()) != 1 || !catalog.Entries()[0].Disabled {
		t.Fatalf("skill disabled state missing: %v", err)
	}
	if err := store.Remove("root-skill"); err != nil {
		t.Fatal(err)
	}
}
