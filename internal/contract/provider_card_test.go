package contract

import (
	"strings"
	"testing"
)

// TestResolveProviderCardPrefersDeclarationOverBuiltin 锁定解析优先级：
// 显式声明优先（即使与内建同名），未声明时回退内建目录，两者皆无则失败关闭。
func TestResolveProviderCardPrefersDeclarationOverBuiltin(t *testing.T) {
	declared := &ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, ContextWindowTokens: 131072, StopReasons: []string{"stop", "stop", "length"}}
	card, err := ResolveProviderCard(ProviderKindOpenAICompatible, "glm-5.2", declared)
	if err != nil {
		t.Fatal(err)
	}
	if card.Source != ProviderCardSourceConfig || card.ContextWindowTokens != 131072 || len(card.StopReasons) != 2 || card.StopReasons[0] != "length" {
		t.Fatalf("declared card = %#v", card)
	}
	builtin, err := ResolveProviderCard(ProviderKindOpenAICompatible, "deepseek-v4-flash", nil)
	if err != nil {
		t.Fatal(err)
	}
	if builtin.Source != ProviderCardSourceBuiltin || !builtin.ToolCalls || !builtin.StreamingUsage || builtin.ContextWindowTokens != 0 || builtin.SchemaVersion != 1 || builtin.Protocol != "openai-chat-completions" || builtin.ProviderKind != ProviderKindOpenAICompatible {
		t.Fatalf("builtin card = %#v", builtin)
	}
	if len(builtin.StopReasons) != 4 || builtin.StopReasons[0] != "content_filter" {
		t.Fatalf("builtin stop reasons = %#v", builtin.StopReasons)
	}
	_, err = ResolveProviderCard(ProviderKindOpenAICompatible, "totally-unknown-model", nil)
	if err == nil || !strings.Contains(err.Error(), "declare provider.capability") {
		t.Fatalf("unknown model must fail closed with actionable error: %v", err)
	}
}

// TestResolveProviderCardRejectsUnsupportedCapabilities 锁定失败关闭边界：
// 空模型、未知线协议种类与声明不支持工具调用/流式 usage 的端点一律拒绝。
func TestResolveProviderCardRejectsUnsupportedCapabilities(t *testing.T) {
	if _, err := ResolveProviderCard(ProviderKindOpenAICompatible, "", nil); err == nil {
		t.Fatal("empty model must be rejected")
	}
	if _, err := ResolveProviderCard("anthropic_messages", "m", nil); err == nil {
		t.Fatal("unknown provider kind must be rejected")
	}
	for name, declaration := range map[string]*ProviderCapabilityDeclaration{
		"no tool calls":      {ToolCalls: false, StreamingUsage: true},
		"no streaming usage": {ToolCalls: true, StreamingUsage: false},
		"negative limits":    {ToolCalls: true, StreamingUsage: true, ContextWindowTokens: -1},
	} {
		if _, err := ResolveProviderCard(ProviderKindOpenAICompatible, "m", declaration); err == nil {
			t.Fatalf("expected rejection for %s", name)
		}
	}
}

// TestProviderCardValidatesBudgetAgainstDeclaredLimits 锁定预算对照：已声明
// 上限必须覆盖执行预算，未声明（0）的上限不做对照。
func TestProviderCardValidatesBudgetAgainstDeclaredLimits(t *testing.T) {
	card := ProviderCapabilityCard{ContextWindowTokens: 100_000, MaxOutputTokens: 8_000}
	if err := card.ValidateBudget(50_000, 4_000); err != nil {
		t.Fatalf("within limits: %v", err)
	}
	if err := card.ValidateBudget(200_000, 4_000); err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("context overflow must be rejected: %v", err)
	}
	if err := card.ValidateBudget(50_000, 9_000); err == nil || !strings.Contains(err.Error(), "output limit") {
		t.Fatalf("output overflow must be rejected: %v", err)
	}
	undeclared := ProviderCapabilityCard{}
	if err := undeclared.ValidateBudget(999_999, 999_999); err != nil {
		t.Fatalf("undeclared limits must not constrain: %v", err)
	}
}
