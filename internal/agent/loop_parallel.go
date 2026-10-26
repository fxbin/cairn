// loop_parallel.go 实现只读工具组的分阶段并行：授权事件仍按单写入者顺序
// 落盘，只读操作可并发 started（domain 已允许），仅执行段交给有界并发；
// 副作用工具与需要审批的调用保持完整串行合同。结果按调用下标顺序回传。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// toolCallOutcome 是单个工具调用的执行结果或错误。
type toolCallOutcome struct {
	result tools.Result
	err    error
}

// preparedReadonlyCall 是已写完授权事件、等待并行执行的只读调用。
type preparedReadonlyCall struct {
	index       int
	call        ports.ToolCall
	operationID domain.OperationID
	decision    policy.Decision
}

// executeToolCalls 按批次计划执行工具调用并返回与入参等长的结果切片。
// 只读组分阶段并行，其余串行走 executeTool；err 表示首个失败调用的错误。
func (loop *Loop) executeToolCalls(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, calls []ports.ToolCall) ([]toolCallOutcome, error) {
	outcomes := make([]toolCallOutcome, len(calls))
	if len(calls) == 0 {
		return outcomes, nil
	}
	if loop.Tools == nil {
		return nil, errors.New("tool registry is required")
	}
	isReadonly := ReadonlyFromRegistry(loop.Tools)
	groups := tools.PlanParallelGroups(toolNamesOf(calls), isReadonly)
	for _, group := range groups {
		if !group.Parallel || len(group.Indexes) < 2 {
			for _, index := range group.Indexes {
				result, err := loop.executeTool(ctx, projection, input, turnID, calls[index])
				outcomes[index] = toolCallOutcome{result: result, err: err}
				if err != nil {
					return outcomes, err
				}
			}
			continue
		}
		if err := loop.executeReadonlyParallelGroup(ctx, projection, input, turnID, calls, group.Indexes, outcomes); err != nil {
			return outcomes, err
		}
	}
	return outcomes, nil
}

// executeReadonlyParallelGroup 对一组只读调用执行「授权串行 → 执行并行 →
// 结果串行」。任一调用无法安全并行时回退串行。
func (loop *Loop) executeReadonlyParallelGroup(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, calls []ports.ToolCall, indexes []int, outcomes []toolCallOutcome) error {
	prepared := make([]preparedReadonlyCall, 0, len(indexes))
	for _, index := range indexes {
		call := calls[index]
		if call.ID == "" || call.Name == "" || !json.Valid(call.Arguments) || containsSensitiveToolCall(call) || loop.Tools.MCPNeedsQuiescence() {
			result, err := loop.executeTool(ctx, projection, input, turnID, call)
			outcomes[index] = toolCallOutcome{result: result, err: err}
			if err != nil {
				return err
			}
			continue
		}
		if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
			loop.finishPreparedReadonly(ctx, projection, input, turnID, prepared, outcomes)
			outcomes[index] = toolCallOutcome{err: err}
			return err
		}
		// 策略判定先于事件追加：非 allow 调用直接回退串行路径，由 executeTool
		// 走完整单份提议链，避免组内留下停在 denied/awaiting 的孤儿提议。
		decision, err := loop.Gate.Decide(call.Name, call.Arguments)
		if err != nil {
			loop.finishPreparedReadonly(ctx, projection, input, turnID, prepared, outcomes)
			return err
		}
		decision = freshApprovalDecision(*projection, decision)
		decision = loop.bindMCPDeclaration(call.Name, decision)
		if decision.Effect != policy.Allow {
			result, err := loop.executeTool(ctx, projection, input, turnID, call)
			outcomes[index] = toolCallOutcome{result: result, err: err}
			if err != nil {
				loop.finishPreparedReadonly(ctx, projection, input, turnID, prepared, outcomes)
				return err
			}
			continue
		}
		operationID := domain.NewOperationID()
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: call.ID, ToolName: call.Name, Arguments: call.Arguments})}); err != nil {
			loop.finishPreparedReadonly(ctx, projection, input, turnID, prepared, outcomes)
			return err
		}
		if err := loop.append(ctx, projection, policyDecisionDraft(input, turnID, operationID, decision)); err != nil {
			loop.finishPreparedReadonly(ctx, projection, input, turnID, prepared, outcomes)
			return err
		}
		startedPayload, err := loop.plannedOperationPayload(call)
		if err != nil {
			loop.finishPreparedReadonly(ctx, projection, input, turnID, prepared, outcomes)
			return err
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(startedPayload)}); err != nil {
			loop.finishPreparedReadonly(ctx, projection, input, turnID, prepared, outcomes)
			return err
		}
		prepared = append(prepared, preparedReadonlyCall{index: index, call: call, operationID: operationID, decision: decision})
	}
	if len(prepared) == 0 {
		return nil
	}
	type execOutcome struct {
		result tools.Result
	}
	execResults := make([]execOutcome, len(prepared))
	var wg sync.WaitGroup
	sem := make(chan struct{}, tools.DefaultMaxWorkers)
	for i := range prepared {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				execResults[slot] = execOutcome{result: tools.Failure("cancelled", "context cancelled before execution")}
				return
			}
			item := prepared[slot]
			result := loop.Tools.ExecuteWithMCPIdentity(ctx, item.call.Name, item.call.Arguments, item.decision.MCPIdentity)
			execResults[slot] = execOutcome{result: redactToolResult(result)}
		}(i)
	}
	wg.Wait()
	// 取消不提前返回：首个取消项之后仍需把同组其余已 started 调用逐个闭合，
	// 否则悬空只读操作会使后续取消/终态事件违反状态机合法性。
	cancelledSeen := false
	for i, item := range prepared {
		result := execResults[i].result
		if result.Code == tools.CodeTerminationUnconfirmed {
			outcomes[item.index] = toolCallOutcome{result: result, err: &ports.WaitingReconcileError{RunID: input.RunID, OperationID: item.operationID, Reason: result.Preview}}
			return outcomes[item.index].err
		}
		eventType := domain.EventOperationSucceeded
		cancelled := result.Code == "cancelled" && ctx.Err() != nil
		if cancelled {
			eventType = domain.EventOperationCancelled
		} else if !result.OK {
			eventType = domain.EventOperationFailed
		}
		resultContext, cancel := loop.commitContext(ctx)
		payload := loop.resultPayload(item.call.Name, result)
		payload.HookActions = loop.dispatchPostToolHooks(item.call.Name, result)
		appendErr := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &item.operationID, Payload: mustJSON(payload)})
		cancel()
		if appendErr != nil {
			outcomes[item.index] = toolCallOutcome{result: result, err: appendErr}
			return appendErr
		}
		if err := loop.appendToolResult(ctx, projection, input, turnID, item.operationID, result); err != nil {
			outcomes[item.index] = toolCallOutcome{result: result, err: err}
			return err
		}
		if err := loop.Tools.AcceptMCPResult(item.call.Name, result); err != nil {
			outcomes[item.index] = toolCallOutcome{result: result, err: err}
			return err
		}
		if err := loop.Tools.AcceptSkillResult(item.call.Name, result); err != nil {
			outcomes[item.index] = toolCallOutcome{result: result, err: err}
			return err
		}
		if cancelled {
			outcomes[item.index] = toolCallOutcome{result: result, err: ctx.Err()}
			cancelledSeen = true
			continue
		}
		outcomes[item.index] = toolCallOutcome{result: result}
		if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
			outcomes[item.index] = toolCallOutcome{result: result, err: err}
			return err
		}
	}
	if cancelledSeen {
		return ctx.Err()
	}
	return nil
}

// finishPreparedReadonly 在并行组准备阶段中断时补执行并落盘已 started 的调用，
// 避免留下悬空只读操作。
func (loop *Loop) finishPreparedReadonly(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, prepared []preparedReadonlyCall, outcomes []toolCallOutcome) {
	for _, item := range prepared {
		result := loop.Tools.ExecuteWithMCPIdentity(ctx, item.call.Name, item.call.Arguments, item.decision.MCPIdentity)
		result = redactToolResult(result)
		eventType := domain.EventOperationSucceeded
		if !result.OK {
			eventType = domain.EventOperationFailed
		}
		resultContext, cancel := loop.commitContext(ctx)
		payload := loop.resultPayload(item.call.Name, result)
		payload.HookActions = loop.dispatchPostToolHooks(item.call.Name, result)
		appendErr := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &item.operationID, Payload: mustJSON(payload)})
		cancel()
		if appendErr != nil {
			outcomes[item.index] = toolCallOutcome{result: result, err: appendErr}
			continue
		}
		if err := loop.appendToolResult(ctx, projection, input, turnID, item.operationID, result); err != nil {
			outcomes[item.index] = toolCallOutcome{result: result, err: err}
			continue
		}
		_ = loop.Tools.AcceptMCPResult(item.call.Name, result)
		_ = loop.Tools.AcceptSkillResult(item.call.Name, result)
		outcomes[item.index] = toolCallOutcome{result: result}
	}
}

// toolNamesOf 提取批次工具名，供并行分组使用。
func toolNamesOf(calls []ports.ToolCall) []string {
	names := make([]string, len(calls))
	for index, call := range calls {
		names[index] = call.Name
	}
	return names
}
