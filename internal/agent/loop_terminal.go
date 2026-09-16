package agent

import (
	"context"
	"errors"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// runFailurePayload 保存运行失败的可回放分类与说明。
type runFailurePayload struct {
	Error           string `json:"error"`
	ErrorClass      string `json:"error_class,omitempty"`
	Reason          string `json:"reason,omitempty"`
	BudgetDimension string `json:"budget_dimension,omitempty"`
}

// persistCancellation 关闭待审批状态和当前轮次，并把取消原因持久化为终态事件。
func (loop *Loop) persistCancellation(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID *domain.TurnID) (Result, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return loop.persistBudgetExceeded(ctx, projection, input, "wall_time")
	}
	cause := context.Canceled
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if projection.State == domain.RunCancelling {
		return Result{RunID: input.RunID}, cause
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCancelRequested, TurnID: turnID, Payload: mustJSON(struct {
		Cause            string `json:"cause"`
		ExpectedTerminal string `json:"expected_terminal"`
	}{Cause: "user_interrupt", ExpectedTerminal: "cancelled"})}); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	if err := loop.cancelPendingApproval(commitContext, projection, input, "user_interrupt"); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	// A pending approval is the highest-priority recovery overlay. The model's
	// candidate response may already be durable, but it must not start a second
	// verification plan or another tool path before that exact operation is
	// resolved.
	if projection.CurrentTurn != nil && projection.PendingApproval == nil {
		currentTurn := *projection.CurrentTurn
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &currentTurn, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "cancelled"})}); err != nil {
			return Result{RunID: input.RunID}, errors.Join(cause, err)
		}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCancelled, Payload: mustJSON(struct {
		Cause string `json:"cause"`
	}{Cause: "user_interrupt"})}); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	return Result{RunID: input.RunID}, cause
}

// persistBudgetExceeded 将触发的预算维度及失败状态追加到事件日志。
func (loop *Loop) persistBudgetExceeded(ctx context.Context, projection *domain.RunProjection, input RunInput, dimension string) (Result, error) {
	cause := &BudgetExceededError{Dimension: dimension}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if projection.State == domain.RunCancelling {
		return Result{RunID: input.RunID}, cause
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCancelRequested, Payload: mustJSON(struct {
		Cause            string `json:"cause"`
		ExpectedTerminal string `json:"expected_terminal"`
		Dimension        string `json:"budget_dimension"`
	}{Cause: "budget_exhausted", ExpectedTerminal: "failed", Dimension: dimension})}); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	if err := loop.cancelPendingApproval(commitContext, projection, input, "budget_exhausted"); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})}); err != nil {
			return Result{RunID: input.RunID}, errors.Join(cause, err)
		}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
		Error     string `json:"error"`
		Dimension string `json:"budget_dimension"`
	}{Error: cause.Error(), Dimension: dimension})}); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	return Result{RunID: input.RunID}, cause
}

// cancelPendingApproval 在终止前关闭审批覆盖层；它明确由系统发起，不会消费
// 人工授权或启动操作，但会留下可回放的审计原因。
func (loop *Loop) cancelPendingApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, cause string) error {
	if projection.PendingApproval == nil {
		return nil
	}
	if projection.CurrentTurn == nil {
		return errors.New("pending approval has no open turn")
	}
	pending := projection.PendingApproval
	turnID := *projection.CurrentTurn
	return loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
		StopCause   string `json:"stop_cause"`
	}{Fingerprint: pending.Fingerprint, Actor: "system", Reason: "approval closed while the run was stopping", StopCause: cause})})
}

// persistRunFailure 可选关闭当前轮次后写入运行失败终态；任一持久化失败都与
// 原始业务错误合并返回，避免终态缺口掩盖真正的失败原因。
func (loop *Loop) persistRunFailure(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID *domain.TurnID, failure runFailurePayload, cause error) (Result, error) {
	if failure.Error == "" {
		failure.Error = "run failed"
	}
	if cause == nil {
		cause = errors.New(failure.Error)
	}
	if turnID != nil {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})}); err != nil {
			return Result{RunID: input.RunID}, errors.Join(cause, err)
		}
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(failure)}); err != nil {
		return Result{RunID: input.RunID}, errors.Join(cause, err)
	}
	return Result{RunID: input.RunID}, cause
}

// failCurrentTurn 持久化当前轮次失败和运行失败，返回原始失败原因。
func (loop *Loop) failCurrentTurn(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, cause error) (Result, error) {
	return loop.persistRunFailure(ctx, projection, input, &turnID, runFailurePayload{Error: cause.Error()}, cause)
}

// persistLoopGuardExceeded 关闭当前轮次并以独立 loop_guard 分类持久化提前终止原因。
func (loop *Loop) persistLoopGuardExceeded(ctx context.Context, projection *domain.RunProjection, input RunInput, reason string) error {
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	guardErr := &LoopGuardExceededError{Reason: reason}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})}); err != nil {
			return errors.Join(guardErr, err)
		}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
		Error      string `json:"error"`
		ErrorClass string `json:"error_class"`
		Reason     string `json:"reason"`
	}{Error: guardErr.Error(), ErrorClass: "loop_guard", Reason: reason})}); err != nil {
		return errors.Join(guardErr, err)
	}
	return guardErr
}

// commitContext 派生保留调用方值但不受运行取消影响的短暂持久化上下文。
func (loop *Loop) commitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}
