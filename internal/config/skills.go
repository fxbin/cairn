// Package config 声明显式启用的本地技能视图，技能不能变更执行权限。
// Author: fxbin
package config

import "errors"

// SkillsConfig 控制技能目录装配；Shared 额外读取跨客户端 .agents/skills 目录。
type SkillsConfig struct {
	Enabled bool `yaml:"enabled" json:"enabled"`
	Shared  bool `yaml:"shared" json:"shared"`
}

// validateSkills 使技能仅在显式 coding 配置下启用，冻结评测与只读工具面不扩张。
func validateSkills(skills *SkillsConfig, preset string) error {
	if skills == nil {
		return nil
	}
	if skills.Shared && !skills.Enabled {
		return errors.New("skills.shared requires skills.enabled")
	}
	if skills.Enabled && (preset == PresetReadOnly || preset == PresetBenchmark) {
		return errors.New("skills are unavailable in read-only and benchmark presets")
	}
	return nil
}

// skillsFingerprint 把未启用的空块归一为缺省，保持旧配置的逐位身份。
func skillsFingerprint(skills *SkillsConfig) *SkillsConfig {
	if skills == nil || !skills.Enabled {
		return nil
	}
	copy := *skills
	return &copy
}
