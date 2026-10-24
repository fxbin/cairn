// 取消请求对账与终态收尾。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

func (loop *Loop) repairMissingCancellationTerminal(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (Result, bool, error) {
	if projection.Cancelling() {
		request, found, err := latestRunCancellationRequest(events)
		if err != nil {
			return Result{RunID: input.RunID}, true, err
		}
		if !found {
			return Result{RunID: input.RunID}, true, errors.New("cancelling run has no persisted cancellation request")
		}
		result, finishErr := loop.finishPersistedCancellation(ctx, projection, input, request)
		return result, true, finishErr
	}
	if projection.State != domain.RunRunning {
		return Result{}, false, nil
	}
	turnID, cause, found, err := openTurnOperationCancellation(events, projection.CurrentTurn)
	if err != nil {
		return Result{RunID: input.RunID}, true, err
	}
	if !found {
		return Result{}, false, nil
	}
	request := runCancelRequestedPayload{Cause: cause, ExpectedTerminal: "cancelled"}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventRunCancelRequested,
		TurnID:        &turnID,
		Payload:       mustJSON(request),
	}); err != nil {
		return Result{RunID: input.RunID}, true, errors.Join(context.Canceled, err)
	}
	result, finishErr := loop.finishPersistedCancellation(commitContext, projection, input, request)
	return result, true, finishErr
}

// latestRunCancellationRequest 返回最近一次取消请求并兼容未显式记录预期终态的
// 旧事件；预算原因确定性映射为 failed，其余取消原因映射为 cancelled。
func latestRunCancellationRequest(events []domain.Event) (runCancelRequestedPayload, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventRunCancelRequested {
			continue
		}
		var payload runCancelRequestedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return runCancelRequestedPayload{}, false, fmt.Errorf("decode run_cancel_requested payload: %w", err)
		}
		if payload.ExpectedTerminal == "" {
			if payload.Cause == "budget_exhausted" || payload.BudgetDimension != "" {
				payload.ExpectedTerminal = "failed"
			} else {
				payload.ExpectedTerminal = "cancelled"
			}
		}
		switch payload.ExpectedTerminal {
		case "failed":
			if payload.Cause == "" {
				payload.Cause = "budget_exhausted"
			}
		case "cancelled":
			if payload.Cause == "" {
				payload.Cause = "user_interrupt"
			}
		default:
			return runCancelRequestedPayload{}, false, fmt.Errorf("run_cancel_requested has invalid expected terminal %q", payload.ExpectedTerminal)
		}
		return payload, true, nil
	}
	return runCancelRequestedPayload{}, false, nil
}

// openTurnOperationCancellation 检查当前开放轮次是否已有操作取消终态，或
// 未启动验证因 user_interrupt 失败关闭的结构化证据；恢复只能据此收束运行。
func openTurnOperationCancellation(events []domain.Event, currentTurn *domain.TurnID) (domain.TurnID, string, bool, error) {
	if currentTurn == nil {
		return "", "", false, nil
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.TurnID == nil || *event.TurnID != *currentTurn {
			continue
		}
		switch event.Type {
		case domain.EventOperationCancelled:
			return *currentTurn, "user_interrupt", true, nil
		case domain.EventOperationFailed:
			cause, found, err := verificationCancellationCause(event)
			if err != nil {
				return "", "", false, err
			}
			if found {
				return *currentTurn, cause, true, nil
			}
		}
	}
	return "", "", false, nil
}

// finishPersistedCancellation 根据已持久化请求关闭审批与当前轮次，并补写唯一
// 预期运行终态；持久化错误与取消或预算错误会同时返回。
func (loop *Loop) finishPersistedCancellation(ctx context.Context, projection *domain.RunProjection, input RunInput, request runCancelRequestedPayload) (Result, error) {
	cause := cancellationTerminalCause(request)
	if projection.PendingOperation != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, errors.New("cancelling run still has a pending operation that requires reconciliation"))
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.cancelPendingApproval(commitContext, projection, input, request.Cause); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	if request.ExpectedTerminal == "failed" {
		return loop.persistRunFailure(commitContext, projection, input, projection.CurrentTurn, runFailurePayload{
			Error:           cause.Error(),
			BudgetDimension: request.BudgetDimension,
		}, cause)
	}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := loop.append(commitContext, projection, domain.EventDraft{
			RunID:         input.RunID,
			WorkspaceHash: input.WorkspaceHash,
			Type:          domain.EventTurnClosed,
			TurnID:        &turnID,
			Payload:       mustJSON(domain.TurnClosedPayload{Outcome: "cancelled"}),
		}); err != nil {
			return Result{RunID: input.RunID}, errors.Join(cause, err)
		}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventRunCancelled,
		Payload: mustJSON(struct {
			Cause string `json:"cause"`
		}{Cause: request.Cause}),
	}); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	return Result{RunID: input.RunID}, cause
}

// cancellationTerminalCause 将取消请求转换为调用方可识别的稳定业务错误。
func cancellationTerminalCause(request runCancelRequestedPayload) error {
	if request.ExpectedTerminal != "failed" {
		return context.Canceled
	}
	if request.Cause == "budget_exhausted" {
		if request.BudgetDimension == "" {
			return errors.New("budget exhausted")
		}
		return &ports.BudgetExceededError{Dimension: request.BudgetDimension}
	}
	if request.Cause != "" {
		return errors.New(request.Cause)
	}
	return errors.New("run cancellation requested a failed terminal")
}

// UnstartedOperationRepair 描述一个已作出拒绝决定，或普通工具已获授权但
// 尚未开始执行且没有终态的操作；验证命令的授权恢复由冻结验证流程负责。
type UnstartedOperationRepair struct {
	OperationID domain.OperationID
	TurnID      domain.TurnID
	ToolName    string
	Result      tools.Result
}

// PendingUnstartedOperationRepairs 从事件流重建操作生命周期，找出审批或策略
// 已经给出确定结论、但 operation_started 尚未落盘的中断窗口。
