package agent

import (
	"context"
	"encoding/json"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

const fallbackMaxTurns = 40

// budgetUsage 保存从持久化响应中累计的输入与输出 token。
type budgetUsage struct {
	InputTokens  int
	OutputTokens int
}

// add 将单次模型 token 用量累加到运行总量。
func (usage *budgetUsage) add(value *ports.Usage) {
	if value == nil {
		return
	}
	usage.InputTokens += value.PromptTokens
	usage.OutputTokens += value.CompletionTokens
}

// budgetUsageFromEvents 从已持久化模型响应事件重建累计 token 消耗。
func budgetUsageFromEvents(events []domain.Event) budgetUsage {
	var usage budgetUsage
	for _, event := range events {
		if event.Type != domain.EventModelResponseFinished {
			continue
		}
		var payload modelResponsePayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			usage.add(payload.Usage)
		}
	}
	return usage
}

// responseBudgetExceeded 检查 token 和显式定价成本是否超过配置上限。
func (loop *Loop) responseBudgetExceeded(usage budgetUsage) *BudgetExceededError {
	if loop.MaxInputTokens > 0 && usage.InputTokens > loop.MaxInputTokens {
		return &BudgetExceededError{Dimension: "input_tokens"}
	}
	if loop.MaxOutputTokens > 0 && usage.OutputTokens > loop.MaxOutputTokens {
		return &BudgetExceededError{Dimension: "output_tokens"}
	}
	if loop.MaxCostUSD > 0 && usage.cost(loop.InputTokenUSD, loop.OutputTokenUSD) > loop.MaxCostUSD {
		return &BudgetExceededError{Dimension: "cost_usd"}
	}
	return nil
}

// cost 按显式输入、输出单价计算累计 token 的美元成本。
func (usage budgetUsage) cost(inputRate, outputRate float64) float64 {
	return float64(usage.InputTokens)*inputRate + float64(usage.OutputTokens)*outputRate
}

// ensureToolBudget 统计已启动操作，并在达到上限时拒绝新工具执行。
// 首次调用从事件日志惰性初始化计数，之后由 append 随 operation_started 事件
// 增量维护，避免每次工具调用都全量重载日志；惰性初始化始终以 journal 为准
// 重算并覆盖计数，因此增量值不会产生偏差。
func (loop *Loop) ensureToolBudget(ctx context.Context, runID domain.RunID) error {
	if loop.MaxToolCalls <= 0 {
		return nil
	}
	if !loop.startedOperationsKnown {
		events, err := loop.Journal.Load(ctx, runID)
		if err != nil {
			return err
		}
		started := 0
		for _, event := range events {
			if event.Type == domain.EventOperationStarted {
				started++
			}
		}
		loop.startedOperations = started
		loop.startedOperationsKnown = true
	}
	if loop.startedOperations >= loop.MaxToolCalls {
		return &BudgetExceededError{Dimension: "tool_calls"}
	}
	return nil
}
