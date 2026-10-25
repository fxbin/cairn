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
func (loop *Loop) responseBudgetExceeded(usage budgetUsage) *ports.BudgetExceededError {
	if loop.MaxInputTokens > 0 && usage.InputTokens > loop.MaxInputTokens {
		return &ports.BudgetExceededError{Dimension: "input_tokens"}
	}
	if loop.MaxOutputTokens > 0 && usage.OutputTokens > loop.MaxOutputTokens {
		return &ports.BudgetExceededError{Dimension: "output_tokens"}
	}
	if loop.MaxCostUSD > 0 && usage.cost(loop.InputTokenUSD, loop.OutputTokenUSD) > loop.MaxCostUSD {
		return &ports.BudgetExceededError{Dimension: "cost_usd"}
	}
	return nil
}

// stopForPersistedResponseBudget 在恢复或新模型调用前重新检查 Journal 用量；
// 已 started 的副作用先对账，读操作可如实闭合，超限不得继续消费模型响应。
func (loop *Loop) stopForPersistedResponseBudget(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (Result, bool, error) {
	if projection.State != domain.RunRunning {
		return Result{}, false, nil
	}
	exceeded := loop.responseBudgetExceeded(budgetUsageFromEvents(events))
	if exceeded == nil {
		return Result{}, false, nil
	}
	if pending := projection.PendingOperation; pending != nil {
		if pending.SideEffectClass == "workspace" || pending.SideEffectClass == "process" {
			return Result{RunID: input.RunID}, true, &ports.WaitingReconcileError{RunID: input.RunID, OperationID: pending.ID, Reason: "persisted response budget is exceeded; reconcile the started operation before stopping"}
		}
	}
	if len(startedReadOperations(*projection)) > 0 {
		commitContext, cancel := loop.commitContext(ctx)
		defer cancel()
		if err := loop.repairInterruptedRead(commitContext, projection, input, events); err != nil {
			return Result{RunID: input.RunID}, true, err
		}
	}
	result, err := loop.persistBudgetExceeded(ctx, projection, input, exceeded.Dimension)
	return result, true, err
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
			if event.Type == domain.EventOperationStarted && !isMCPLifecyclePayload(event.Payload) {
				started++
			}
		}
		loop.startedOperations = started
		loop.startedOperationsKnown = true
	}
	if loop.startedOperations >= loop.MaxToolCalls {
		return &ports.BudgetExceededError{Dimension: "tool_calls"}
	}
	return nil
}
