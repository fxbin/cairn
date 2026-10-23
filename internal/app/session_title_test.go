// session_title_test 锁定标题模型选择与修剪。
//
// Author: fxbin
package app

import "testing"

// TestChooseTitleModelPrefersLowCost 锁定成本优先选择。
func TestChooseTitleModelPrefersLowCost(t *testing.T) {
	config := DefaultSessionTitleConfig()
	available := []string{"opus-max", "gpt-6.1-mini", "claude-sonnet"}
	got := ChooseTitleModel(config, available)
	if got != "gpt-6.1-mini" {
		t.Fatalf("应选 mini，得到 %s", got)
	}
	got = ChooseTitleModel(config, []string{"claude-flash", "opus"})
	if got != "claude-flash" {
		t.Fatalf("应选 flash，得到 %s", got)
	}
}

// TestSanitizeTitle 锁定修剪。
func TestSanitizeTitle(t *testing.T) {
	if got := SanitizeTitle("  修复登录  ", 10); got != "修复登录" {
		t.Fatalf("应去空白: %q", got)
	}
	if got := SanitizeTitle("这是一个很长的标题应该被截断", 5); len([]rune(got)) != 5 {
		t.Fatalf("应截断: %q", got)
	}
}
