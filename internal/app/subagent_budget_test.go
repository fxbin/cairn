// subagent_budget_test 锁定预算余量扣减与用量解析。
//
// Author: fxbin
package app

import (
	"errors"
	"testing"

	"github.com/fxbin/cairn/internal/config"
)

// TestDeductWithinRemaining 锁定扣减成功与余量递减。
func TestDeductWithinRemaining(t *testing.T) {
	remaining := NewBudgetRemaining(config.BudgetConfig{MaxTurns: 5, MaxInputTokens: 100, MaxOutputTokens: 50})
	after, err := remaining.Deduct(BudgetRemaining{MaxTurns: 2, MaxInputTokens: 30, MaxOutputTokens: 10})
	if err != nil {
		t.Fatalf("扣减应成功: %v", err)
	}
	if after.MaxTurns != 3 || after.MaxInputTokens != 70 || after.MaxOutputTokens != 40 {
		t.Fatalf("余量不符: %+v", after)
	}
}

// TestDeductExceedsRemainingFailsClosed 锁定超限失败关闭且不部分扣减。
func TestDeductExceedsRemainingFailsClosed(t *testing.T) {
	remaining := NewBudgetRemaining(config.BudgetConfig{MaxTurns: 1, MaxInputTokens: 10, MaxOutputTokens: 10})
	_, err := remaining.Deduct(BudgetRemaining{MaxTurns: 2})
	if !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("轮次超限应拒绝: %v", err)
	}
	_, err = remaining.Deduct(BudgetRemaining{MaxInputTokens: 20})
	if !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("输入超限应拒绝: %v", err)
	}
	_, err = remaining.Deduct(BudgetRemaining{MaxTurns: -1})
	if !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("负值应拒绝: %v", err)
	}
	// 无上限时不拒绝
	unlimited := NewBudgetRemaining(config.BudgetConfig{})
	if _, err := unlimited.Deduct(BudgetRemaining{MaxTurns: 99, MaxInputTokens: 999}); err != nil {
		t.Fatalf("无上限不应拒绝: %v", err)
	}
}

// TestUsageFromCompletion 锁定用量解析与负值拒绝。
func TestUsageFromCompletion(t *testing.T) {
	usage, err := UsageFromCompletion([]byte(`{"prompt_tokens":10,"completion_tokens":5}`))
	if err != nil || usage.TotalTokens() != 15 {
		t.Fatalf("解析失败: %v %+v", err, usage)
	}
	if _, err := UsageFromCompletion([]byte(`{"prompt_tokens":-1}`)); err == nil {
		t.Fatal("负用量应拒绝")
	}
	if usage, err := UsageFromCompletion(nil); err != nil || usage.TotalTokens() != 0 {
		t.Fatalf("空用量应为零值: %v %+v", err, usage)
	}
}
