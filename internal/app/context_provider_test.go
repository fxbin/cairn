package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// TestBootContextProvidersRespectExplicitSkillConfiguration 核对真实 Boot 的最小
// 模式和显式 Skill 配置：默认没有扩展，启用后也只能投影已激活的历史正文。
func TestBootContextProvidersRespectExplicitSkillConfiguration(t *testing.T) {
	for _, test := range []struct {
		name    string
		preset  string
		enabled bool
		count   int
	}{
		{name: "minimal", preset: config.PresetMinimal},
		{name: "coding-disabled", preset: config.PresetCoding},
		{name: "coding-enabled", preset: config.PresetCoding, enabled: true, count: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			configuration := withTestProvider(config.Defaults())
			configuration.Preset = test.preset
			if test.enabled {
				configuration.Skills = &config.SkillsConfig{Enabled: true}
			}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
			workspace := t.TempDir()
			if test.enabled {
				directory := filepath.Join(workspace, ".agent", "skills", "context-check")
				if err := os.MkdirAll(directory, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: context-check\ndescription: 上下文接入验收\n---\nHISTORICAL_CONTEXT_BODY\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			boot, err := service.assembleBoot(workspace, t.TempDir(), configuration, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer boot.Close()
			if len(boot.Loop.ContextProviders) != test.count {
				t.Fatalf("context providers=%d want=%d", len(boot.Loop.ContextProviders), test.count)
			}
			if boot.Loop.ConfigFingerprint != configuration.Fingerprint() {
				t.Fatal("context assembly changed frozen configuration identity")
			}
			if !test.enabled {
				return
			}
			extension := boot.Loop.ContextProviders[0]
			blocks, err := extension.ContextBlocks()
			if err != nil || len(blocks) != 0 {
				t.Fatalf("unloaded source entered context: blocks=%v err=%v", blocks, err)
			}
			// 执行读取本身不能激活；仅在核验成功回执的既有边界更新历史投影。
			result := boot.Registry.Execute(context.Background(), "skill", json.RawMessage(`{"name":"context-check"}`))
			if !result.OK {
				t.Fatalf("skill fixture load failed: %s", result.Code)
			}
			if blocks, err = extension.ContextBlocks(); err != nil || len(blocks) != 0 {
				t.Fatal("unaccepted skill result entered context")
			}
			if err := boot.Registry.AcceptSkillResult("skill", result); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(filepath.Join(workspace, ".agent", "skills")); err != nil {
				t.Fatal(err)
			}
			blocks, err = extension.ContextBlocks()
			if err != nil || len(blocks) != 1 || !strings.Contains(blocks[0], "HISTORICAL_CONTEXT_BODY") || !strings.Contains(blocks[0], "不能授予") {
				t.Fatalf("historical snapshot or trust boundary lost: %v", err)
			}
			var content contract.SkillContent
			if err := json.Unmarshal(result.Data, &content); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(blocks[0], content.SourceDigest) || !strings.Contains(blocks[0], content.SHA256) {
				t.Fatal("context dropped historical source identity")
			}
		})
	}
}
