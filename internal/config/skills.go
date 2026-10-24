// Package config 声明显式启用的本地技能视图，技能不能变更执行权限。
// Author: fxbin
package config

import (
	"errors"
	"strings"
)

// SkillsConfig 控制技能目录装配；Shared 额外读取跨客户端 .agents/skills 目录。
// Skillshare 显式挂载 skillshare 源目录（只读发现，不同步）；ExtraDirs 为
// 额外只读技能根（绝对路径），同样不授予执行权限。
type SkillsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Shared  bool `yaml:"shared" json:"shared"`
	// Skillshare 为 true 时挂载默认 skillshare skills 源目录（若存在）。
	Skillshare bool `yaml:"skillshare" json:"skillshare"`
	// ExtraDirs 是额外只读技能根（推荐 skillshare 源路径或团队共享目录）。
	ExtraDirs []string `yaml:"extra_dirs,omitempty" json:"extra_dirs,omitempty"`
}

// validateSkills 使技能仅在显式 coding 配置下启用，冻结评测与只读工具面不扩张。
func validateSkills(skills *SkillsConfig, preset string) error {
	if skills == nil {
		return nil
	}
	if (skills.Shared || skills.Skillshare || len(skills.ExtraDirs) > 0) && !skills.Enabled {
		return errors.New("skills.shared/skillshare/extra_dirs require skills.enabled")
	}
	if skills.Enabled && (preset == PresetReadOnly || preset == PresetBenchmark) {
		return errors.New("skills are unavailable in read-only and benchmark presets")
	}
	for _, dir := range skills.ExtraDirs {
		if strings.TrimSpace(dir) == "" {
			return errors.New("skills.extra_dirs entries must be non-empty")
		}
		if strings.Contains(dir, "\x00") {
			return errors.New("skills.extra_dirs contains invalid characters")
		}
	}
	return nil
}

// skillsFingerprint 把未启用的空块归一为缺省，保持旧配置的逐位身份。
func skillsFingerprint(skills *SkillsConfig) *SkillsConfig {
	if skills == nil || !skills.Enabled {
		return nil
	}
	copy := *skills
	if len(copy.ExtraDirs) > 0 {
		copy.ExtraDirs = append([]string(nil), copy.ExtraDirs...)
	}
	return &copy
}
