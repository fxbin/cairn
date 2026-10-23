// skill_edit_test 锁定 SKL-05 技能草稿校验、摘要与版本比较。
//
// Author: fxbin
package tools

import "testing"

// TestValidateSkillDraft 锁定名称/正文/资源路径边界。
func TestValidateSkillDraft(t *testing.T) {
	ok := SkillDraft{Name: "demo-skill", Description: "演示", Body: "# 说明", Resources: map[string]string{"refs/a.md": "x"}}
	if err := ValidateSkillDraft(ok); err != nil {
		t.Fatalf("合法草稿应通过: %v", err)
	}
	bad := []SkillDraft{
		{Name: "BAD_NAME", Description: "d", Body: "b"},
		{Name: "ok-name", Description: "", Body: "b"},
		{Name: "ok-name", Description: "d", Body: ""},
		{Name: "ok-name", Description: "d", Body: "b", Resources: map[string]string{"../etc/passwd": "x"}},
		{Name: "ok-name", Description: "d", Body: "b", Resources: map[string]string{"/abs": "x"}},
	}
	for _, draft := range bad {
		if err := ValidateSkillDraft(draft); err == nil {
			t.Fatalf("应拒绝 %+v", draft)
		}
	}
}

// TestContentDigestAndCompare 锁定摘要稳定与版本比较。
func TestContentDigestAndCompare(t *testing.T) {
	draft := SkillDraft{Name: "n", Description: "d", Body: "b"}
	sha := ContentDigest(draft.Body)
	if sha == ContentDigest("b2") {
		t.Fatal("不同正文应不同摘要")
	}
	left := SkillVersion{Name: "n", BodySHA: sha, SourceSHA: SourceDigest(draft)}
	right := left
	if !CompareSkillVersions(left, right) {
		t.Fatal("相同版本应相等")
	}
	right.BodySHA = "other"
	if CompareSkillVersions(left, right) {
		t.Fatal("不同摘要应不等")
	}
}
