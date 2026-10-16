package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestSkillsConfigurationFingerprintAndPresetIsolation 锁定默认兼容、配置身份和受限工具集隔离。
func TestSkillsConfigurationFingerprintAndPresetIsolation(t *testing.T) {
	c := validDefaults()
	before := c.Fingerprint()
	c.Skills = &SkillsConfig{}
	if err := Validate(c); err != nil || c.Fingerprint() != before {
		t.Fatal("disabled skills changed legacy configuration identity")
	}
	c.Skills.Enabled = true
	if err := Validate(c); err != nil || c.Fingerprint() == before {
		t.Fatal("enabled skills did not change execution identity")
	}
	enabled := c.Fingerprint()
	c.Skills.Shared = true
	if c.Fingerprint() == enabled {
		t.Fatal("shared skill sources omitted from fingerprint")
	}
	for _, preset := range []string{PresetReadOnly, PresetBenchmark} {
		c.Preset = preset
		if Validate(c) == nil {
			t.Fatalf("skills enabled in %s", preset)
		}
	}
	c.Preset = PresetCoding
	c.Skills.Enabled = false
	if Validate(c) == nil {
		t.Fatal("shared skills without enabled accepted")
	}
}

// TestSkillsLoadRejectsUnknownFields 严格 YAML 解析也覆盖新的技能块。
func TestSkillsLoadRejectsUnknownFields(t *testing.T) {
	file := filepath.Join(t.TempDir(), "config.yaml")
	base := "version: 2\nprovider:\n" + validProviderYAML + "skills:\n  enabled: true\n"
	if err := os.WriteFile(file, []byte(base), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err := Load(file, Overrides{}); err != nil || c.Skills == nil || !c.Skills.Enabled {
		t.Fatalf("%#v %v", c.Skills, err)
	}
	if err := os.WriteFile(file, []byte(base+"  hooks: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(file, Overrides{}); err == nil {
		t.Fatal("unknown skill configuration accepted")
	}
}
