package contract

import (
	"strings"
	"testing"
)

// TestResolveProviderCardRequiresExplicitDeclaration 锁定解析边界：显式声明
// 产出 config 卡；未声明时任何模型都失败关闭（编译期无模型目录），报错
// 给出可操作的配置指引。
func TestResolveProviderCardRequiresExplicitDeclaration(t *testing.T) {
	declared := &ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true, ContextWindowTokens: 131072, StopReasons: []string{"stop", "stop", "length"}}
	card, err := ResolveProviderCard(ProviderKindOpenAICompatible, "glm-5.2", declared)
	if err != nil {
		t.Fatal(err)
	}
	if card.Source != ProviderCardSourceConfig || card.ContextWindowTokens != 131072 || len(card.StopReasons) != 2 || card.StopReasons[0] != "length" {
		t.Fatalf("declared card = %#v", card)
	}
	for _, model := range []string{"deepseek-v4-flash", "glm-5.2", "totally-unknown-model"} {
		if _, err := ResolveProviderCard(ProviderKindOpenAICompatible, model, nil); err == nil || !strings.Contains(err.Error(), "declare provider.capability") {
			t.Fatalf("model %q without declaration must fail closed with actionable error: %v", model, err)
		}
	}
}

// TestResolveProviderCardRejectsUnsupportedCapabilities 锁定失败关闭边界：
// 空模型、未知线协议种类与声明不支持工具调用/流式 usage 的端点一律拒绝。
func TestResolveProviderCardRejectsUnsupportedCapabilities(t *testing.T) {
	if _, err := ResolveProviderCard(ProviderKindOpenAICompatible, "", nil); err == nil {
		t.Fatal("empty model must be rejected")
	}
	if _, err := ResolveProviderCard("gemini_native", "m", &ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true}); err == nil {
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

// TestResolveProviderCardAnthropicDefaultStopReasons 锁定 anthropic 空声明
// 回退到 Anthropic 词表，不得混用 OpenAI finish_reason。
func TestResolveProviderCardAnthropicDefaultStopReasons(t *testing.T) {
	card, err := ResolveProviderCard(ProviderKindAnthropicMessages, "claude-sonnet-5", &ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true})
	if err != nil {
		t.Fatal(err)
	}
	if card.Protocol != "anthropic-messages" {
		t.Fatalf("protocol = %q", card.Protocol)
	}
	joined := strings.Join(card.StopReasons, ",")
	if !strings.Contains(joined, "end_turn") || !strings.Contains(joined, "tool_use") {
		t.Fatalf("stop reasons = %v", card.StopReasons)
	}
	for _, reason := range card.StopReasons {
		if reason == "stop" || reason == "length" || reason == "tool_calls" {
			t.Fatalf("anthropic card must not carry OpenAI finish_reason %q: %v", reason, card.StopReasons)
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
