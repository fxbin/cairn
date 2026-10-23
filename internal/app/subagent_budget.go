// subagent_budget.go 实现父子预算余量与扣减：子预算不得超出委派额度，
// 扣减失败关闭，不静默扩大预算。
// Author: fxbin
package app

import (
	"encoding/json"
	"fmt"

	"github.com/fxbin/cairn/internal/config"
)

// BudgetRemaining 表示可分配的轮次与 token 余量。
type BudgetRemaining struct {
	MaxTurns        int
	MaxInputTokens  int64
	MaxOutputTokens int64
}

// NewBudgetRemaining 从预算配置构造余量。
func NewBudgetRemaining(budget config.BudgetConfig) BudgetRemaining {
	return BudgetRemaining{
		MaxTurns:        budget.MaxTurns,
		MaxInputTokens:  int64(budget.MaxInputTokens),
		MaxOutputTokens: int64(budget.MaxOutputTokens),
	}
}

// Deduct 从余量扣除子需求；任一维度不足则失败关闭且不部分扣减。
func (remaining BudgetRemaining) Deduct(need BudgetRemaining) (BudgetRemaining, error) {
	if need.MaxTurns < 0 || need.MaxInputTokens < 0 || need.MaxOutputTokens < 0 {
		return remaining, fmt.Errorf("%w: budget need must not be negative", ErrInvalidDelegation)
	}
	if remaining.MaxTurns > 0 && need.MaxTurns > remaining.MaxTurns {
		return remaining, fmt.Errorf("%w: turns %d exceed remaining %d", ErrInvalidDelegation, need.MaxTurns, remaining.MaxTurns)
	}
	if remaining.MaxInputTokens > 0 && need.MaxInputTokens > remaining.MaxInputTokens {
		return remaining, fmt.Errorf("%w: input tokens %d exceed remaining %d", ErrInvalidDelegation, need.MaxInputTokens, remaining.MaxInputTokens)
	}
	if remaining.MaxOutputTokens > 0 && need.MaxOutputTokens > remaining.MaxOutputTokens {
		return remaining, fmt.Errorf("%w: output tokens %d exceed remaining %d", ErrInvalidDelegation, need.MaxOutputTokens, remaining.MaxOutputTokens)
	}
	result := remaining
	if result.MaxTurns > 0 {
		result.MaxTurns -= need.MaxTurns
	}
	if result.MaxInputTokens > 0 {
		result.MaxInputTokens -= need.MaxInputTokens
	}
	if result.MaxOutputTokens > 0 {
		result.MaxOutputTokens -= need.MaxOutputTokens
	}
	return result, nil
}

// BudgetUsage 是子运行累计用量。
type BudgetUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// TotalTokens 返回输入+输出总用量。
func (usage BudgetUsage) TotalTokens() int64 {
	return usage.PromptTokens + usage.CompletionTokens
}

// UsageFromCompletion 从 completed 载荷解析用量；负值失败关闭。
func UsageFromCompletion(usageJSON []byte) (BudgetUsage, error) {
	if len(usageJSON) == 0 {
		return BudgetUsage{}, nil
	}
	var usage BudgetUsage
	if err := jsonUnmarshal(usageJSON, &usage); err != nil {
		return BudgetUsage{}, fmt.Errorf("decode subagent usage: %w", err)
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 {
		return BudgetUsage{}, fmt.Errorf("subagent usage must not be negative")
	}
	return usage, nil
}

// jsonUnmarshal 是编码解码薄封装，便于测试注入。
var jsonUnmarshal = func(data []byte, v any) error {
	return json.Unmarshal(data, v)
}
