// thinking_effort_test 锁定档位解析与归一化。
//
// Author: fxbin
package config

import "testing"

// TestParseThinkingEffort 锁定合法与非法值。
func TestParseThinkingEffort(t *testing.T) {
	if _, ok := ParseThinkingEffort("high"); !ok {
		t.Fatal("high 应合法")
	}
	if _, ok := ParseThinkingEffort("invalid"); ok {
		t.Fatal("非法值应失败")
	}
	if got := NormalizeThinkingEffort(""); got != ThinkingMedium {
		t.Fatalf("空值应 medium，得到 %s", got)
	}
	if got := NormalizeThinkingEffort("LOW"); got != ThinkingLow {
		t.Fatalf("应归一化，得到 %s", got)
	}
}

// TestEffectiveThinkingEffort 锁定 Provider 配置的力度归一化。
func TestEffectiveThinkingEffort(t *testing.T) {
	generation := ProviderGenerationConfig{ReasoningEffort: "high"}
	if got := generation.EffectiveThinkingEffort(); got != ThinkingHigh {
		t.Fatalf("应 high，得到 %s", got)
	}
	empty := ProviderGenerationConfig{}
	if got := empty.EffectiveThinkingEffort(); got != ThinkingMedium {
		t.Fatalf("空值应 medium，得到 %s", got)
	}
}
