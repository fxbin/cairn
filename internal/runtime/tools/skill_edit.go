// skill_edit.go 实现 SKL-05 技能编辑、校验与版本比较：仅在合法原生路径
// 编辑，校验结构与相对资源引用，内容摘要可比较；旧运行继续使用历史正文。
// Author: fxbin
package tools

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"strings"
)

// SkillDraft 是一次待写入的技能编辑草稿。
type SkillDraft struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Body        string            `json:"body"`
	Resources   map[string]string `json:"resources,omitempty"`
}

// SkillVersion 是可比较的技能版本摘要。
type SkillVersion struct {
	Name      string `json:"name"`
	BodySHA   string `json:"body_sha256"`
	SourceSHA string `json:"source_sha256"`
}

// ContentDigest 计算正文摘要。
func ContentDigest(body string) string {
	sum := sha256.Sum256([]byte(body))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidateSkillDraft 校验名称、结构与相对资源引用；拒绝越界路径。
func ValidateSkillDraft(draft SkillDraft) error {
	if !skillNamePattern.MatchString(draft.Name) {
		return fmt.Errorf("skill name %q is invalid", draft.Name)
	}
	if strings.TrimSpace(draft.Description) == "" {
		return fmt.Errorf("description is required")
	}
	if strings.TrimSpace(draft.Body) == "" {
		return fmt.Errorf("body is required")
	}
	if len(draft.Body) > SkillMaxInstructionsBytes {
		return fmt.Errorf("body exceeds %d bytes", SkillMaxInstructionsBytes)
	}
	for rel := range draft.Resources {
		if path.IsAbs(rel) || strings.Contains(rel, "..") || strings.Contains(rel, "\x00") {
			return fmt.Errorf("resource %q must be a relative path without ..", rel)
		}
		cleaned := path.Clean(rel)
		if cleaned != rel || strings.HasPrefix(rel, "/") {
			return fmt.Errorf("resource %q is not a clean relative path", rel)
		}
	}
	return nil
}

// SourceDigest 计算技能整体摘要（名称+描述+正文+资源）。
func SourceDigest(draft SkillDraft) string {
	var builder strings.Builder
	builder.WriteString(draft.Name)
	builder.WriteByte('\n')
	builder.WriteString(draft.Description)
	builder.WriteByte('\n')
	builder.WriteString(draft.Body)
	keys := make([]string, 0, len(draft.Resources))
	for key := range draft.Resources {
		keys = append(keys, key)
	}
	sortStrings(keys)
	for _, key := range keys {
		builder.WriteString(key)
		builder.WriteByte('\n')
		builder.WriteString(draft.Resources[key])
		builder.WriteByte('\n')
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// CompareSkillVersions 比较两个版本；相同返回 true。
func CompareSkillVersions(left, right SkillVersion) bool {
	return left.BodySHA == right.BodySHA && left.SourceSHA == right.SourceSHA && left.Name == right.Name
}

// sortStrings 简单插入排序，避免引入排序依赖。
func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
