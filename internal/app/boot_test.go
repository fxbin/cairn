package app

import (
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/ports"
)

// TestAssembleBootReleasesLockOnContention 锁定生命周期顺序：第二个 Boot 在锁
// 竞争处失败且不泄漏资源；第一个 Boot Close 后同一工作区可再次装配。
func TestAssembleBootReleasesLockOnContention(t *testing.T) {
	configuration := config.Defaults()
	workspace, stateDir := t.TempDir(), t.TempDir()
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &finalProvider{}, nil
	}}
	first, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.assembleBoot(workspace, stateDir, configuration, nil); err == nil {
		t.Fatal("second boot unexpectedly acquired the same workspace lock")
	}
	first.Close()
	retry, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatalf("lock was not released by Close: %v", err)
	}
	retry.Close()
	first.Close() // Close 幂等：重复调用不得 panic 或重复释放。
}

// TestAssembleBootCleansUpWhenLateStageFails 锁定装配中段失败的逆序清理：Provider
// 构造失败时，锁与 Journal 必须已被释放，工作区可立即重试装配。
func TestAssembleBootCleansUpWhenLateStageFails(t *testing.T) {
	configuration := config.Defaults()
	workspace, stateDir := t.TempDir(), t.TempDir()
	failing := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return nil, errors.New("provider unavailable")
	}}
	if _, err := failing.assembleBoot(workspace, stateDir, configuration, nil); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("err = %v", err)
	}
	healthy := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &finalProvider{}, nil
	}}
	boot, err := healthy.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatalf("failed assembly leaked the workspace lock: %v", err)
	}
	boot.Close()
}

// TestBootManifestDescribesCapabilities 锁定 Capability Manifest 的非敏感能力
// 事实：固定五工具、Profile、沙箱后端与验证命令数。
func TestBootManifestDescribesCapabilities(t *testing.T) {
	configuration := config.Defaults()
	configuration.Sandbox.Backend = "docker"
	configuration.Sandbox.DockerImage = "alpine:3.21"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &finalProvider{}, nil
	}}
	boot, err := service.assembleBoot(t.TempDir(), t.TempDir(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	manifest := boot.Manifest()
	if manifest.Profile != config.ProfileDefault || manifest.Sandbox != "docker" || manifest.Verification != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if strings.Join(manifest.Tools, ",") != "list_files,read_file,search_text,apply_patch,exec_command" {
		t.Fatalf("manifest tools = %v", manifest.Tools)
	}
	if strings.Contains(strings.ToLower(strings.Join(manifest.Tools, " ")+manifest.Provider+manifest.Model), "key") {
		t.Fatalf("manifest must not carry credential material: %+v", manifest)
	}
}

// TestCompositionRootDependencyDirection 锁定依赖方向：Domain 不依赖任何内部
// 基础设施包；app 作为组合根只允许引用 sqlite 与 openaicompat 两个适配器，
// 且不得反向依赖 CLI。
func TestCompositionRootDependencyDirection(t *testing.T) {
	collectImports := func(packageDir string) []string {
		entries, err := os.ReadDir(packageDir)
		if err != nil {
			t.Fatal(err)
		}
		var imports []string
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, filepath.Join(packageDir, entry.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", entry.Name(), err)
			}
			for _, imported := range file.Imports {
				imports = append(imports, strings.Trim(imported.Path.Value, "\""))
			}
		}
		return imports
	}
	for _, imported := range collectImports("../domain") {
		if strings.HasPrefix(imported, "github.com/fxbin/cairn/internal/") {
			t.Fatalf("domain must not depend on internal packages, found %q", imported)
		}
	}
	allowedAdapters := map[string]bool{
		"github.com/fxbin/cairn/internal/adapters/journal/sqlite":        true,
		"github.com/fxbin/cairn/internal/adapters/provider/openaicompat": true,
	}
	for _, imported := range collectImports(".") {
		if strings.HasPrefix(imported, "github.com/fxbin/cairn/internal/adapters/") && !allowedAdapters[imported] {
			t.Fatalf("composition root references non-allowlisted adapter %q", imported)
		}
		if strings.HasPrefix(imported, "github.com/fxbin/cairn/internal/cli") {
			t.Fatalf("app must not depend on the CLI package, found %q", imported)
		}
	}
}
