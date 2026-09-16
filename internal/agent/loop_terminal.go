package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

// systemCancellationFallbackCode 是缺少明确 stop_cause 时使用的系统停止失败码。
const systemCancellationFallbackCode = "run_stopping"

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

// PendingApprovalCancellation 描述系统停止时待闭合审批操作的既有 Journal
// 事实及确定性失败结果，供 Agent 与应用层恢复入口共享。
type PendingApprovalCancellation struct {
	TurnID               domain.TurnID
	OperationID          domain.OperationID
	Fingerprint          string
	ToolName             string
	VerificationID       string
	Result               tools.Result
	ApprovalDenied       bool
	OperationTerminal    bool
	VerificationFinished bool
}

// cancelPendingApproval 在终止前按 Journal 事实闭合审批、操作及验证覆盖层；
// 它不消费人工授权、不启动操作，也不生成模型可见结果或验证修复反馈。
func (loop *Loop) cancelPendingApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, cause string) error {
	events, err := loop.Journal.Load(ctx, input.RunID)
	if err != nil {
		return fmt.Errorf("load pending approval cancellation state: %w", err)
	}
	record, found, err := ResolvePendingApprovalCancellation(events, projection, cause)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	turnID := record.TurnID
	operationID := record.OperationID
	if !record.ApprovalDenied {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
			Fingerprint string `json:"fingerprint"`
			Actor       string `json:"actor"`
			Reason      string `json:"reason"`
			StopCause   string `json:"stop_cause"`
		}{Fingerprint: record.Fingerprint, Actor: "system", Reason: "approval closed while the run was stopping", StopCause: cause})}); err != nil {
			return fmt.Errorf("persist system approval denial: %w", err)
		}
	}
	if !record.OperationTerminal {
		if err := loop.append(ctx, projection, domain.EventDraft{
			RunID:         input.RunID,
			WorkspaceHash: input.WorkspaceHash,
			Type:          domain.EventOperationFailed,
			TurnID:        &turnID,
			OperationID:   &operationID,
			Payload:       mustJSON(operationPayload{ToolName: record.ToolName, Result: record.Result}),
		}); err != nil {
			return fmt.Errorf("persist cancelled approval operation terminal: %w", err)
		}
	}
	if record.VerificationID != "" && !record.VerificationFinished {
		if err := loop.append(ctx, projection, domain.EventDraft{
			RunID:         input.RunID,
			WorkspaceHash: input.WorkspaceHash,
			Type:          domain.EventVerificationFinished,
			TurnID:        &turnID,
			Payload: mustJSON(domain.VerificationFinishedPayload{
				VerificationID: record.VerificationID,
				OK:             false,
			}),
		}); err != nil {
			return &verificationPersistenceError{cause: fmt.Errorf("persist cancelled verification terminal: %w", err)}
		}
	}
	return nil
}

// ResolvePendingApprovalCancellation 从 Journal 定位当前待审批操作，或恢复
// 已写拒绝但尚未闭合的操作；提议类型和验证标识只取自持久化事件。
func ResolvePendingApprovalCancellation(events []domain.Event, projection *domain.RunProjection, cause string) (PendingApprovalCancellation, bool, error) {
	record := PendingApprovalCancellation{
		Result: systemCancellationResult(cause),
	}
	if projection.PendingApproval != nil {
		if projection.CurrentTurn == nil {
			return PendingApprovalCancellation{}, false, errors.New("pending approval has no open turn")
		}
		record.TurnID = *projection.CurrentTurn
		record.OperationID = projection.PendingApproval.OperationID
		record.Fingerprint = projection.PendingApproval.Fingerprint
	} else {
		for index := len(events) - 1; index >= 0; index-- {
			event := events[index]
			if event.Type != domain.EventApprovalDenied || event.OperationID == nil || event.TurnID == nil {
				continue
			}
			if projection.CurrentTurn != nil && *event.TurnID != *projection.CurrentTurn {
				continue
			}
			payload, result, matches, err := pendingApprovalDenial(event.Payload, cause)
			if err != nil {
				return PendingApprovalCancellation{}, false, fmt.Errorf("decode approval denial: %w", err)
			}
			if !matches {
				continue
			}
			record.TurnID = *event.TurnID
			record.OperationID = *event.OperationID
			record.Fingerprint = payload.Fingerprint
			record.Result = result
			record.ApprovalDenied = true
			break
		}
		if record.OperationID == "" {
			return PendingApprovalCancellation{}, false, nil
		}
	}

	call, toolProposal := proposedToolCall(events, record.OperationID)
	verification, verificationProposal := proposedVerificationCommand(events, record.OperationID)
	if toolProposal == verificationProposal {
		return PendingApprovalCancellation{}, false, fmt.Errorf("approval operation %s must have exactly one replayable proposal", record.OperationID)
	}
	if verificationProposal {
		record.ToolName = "exec_command"
		record.VerificationID = verification.VerificationID
	} else {
		record.ToolName = call.Name
	}

	proposalIndex := -1
	for index, event := range events {
		if event.OperationID != nil && *event.OperationID == record.OperationID {
			switch event.Type {
			case domain.EventToolCallProposed, domain.EventVerificationCommandProposed:
				proposalIndex = index
			case domain.EventApprovalDenied:
				record.ApprovalDenied = true
			case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
				record.OperationTerminal = true
			}
		}
		if record.VerificationID == "" || proposalIndex < 0 || index <= proposalIndex || event.Type != domain.EventVerificationFinished || event.TurnID == nil || *event.TurnID != record.TurnID {
			continue
		}
		var payload domain.VerificationFinishedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return PendingApprovalCancellation{}, false, fmt.Errorf("decode verification terminal: %w", err)
		}
		if payload.VerificationID == "" || payload.VerificationID == record.VerificationID {
			record.VerificationFinished = true
		}
	}
	return record, true, nil
}

// systemApprovalDenialPayload 保存审批拒绝恢复所需的最小字段。
type systemApprovalDenialPayload struct {
	Fingerprint string `json:"fingerprint"`
	Actor       string `json:"actor"`
	Reason      string `json:"reason"`
	StopCause   string `json:"stop_cause"`
}

// systemApprovalDenial 解析并规范化审批拒绝字段。
func systemApprovalDenial(raw json.RawMessage) (systemApprovalDenialPayload, error) {
	var payload systemApprovalDenialPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return systemApprovalDenialPayload{}, err
	}
	payload.Actor = strings.ToLower(strings.TrimSpace(payload.Actor))
	payload.Reason = strings.TrimSpace(payload.Reason)
	payload.StopCause = strings.TrimSpace(payload.StopCause)
	return payload, nil
}

// pendingApprovalDenial 判断既有拒绝是否可由当前停止路径继续闭合，并保留人工
// 拒绝的失败语义；旧系统拒绝缺少 stop_cause 时由当前持久化停止决定接管。
func pendingApprovalDenial(raw json.RawMessage, cause string) (systemApprovalDenialPayload, tools.Result, bool, error) {
	payload, err := systemApprovalDenial(raw)
	if err != nil {
		return systemApprovalDenialPayload{}, tools.Result{}, false, err
	}
	human, reason, err := humanApprovalDenial(raw)
	if err != nil {
		return systemApprovalDenialPayload{}, tools.Result{}, false, err
	}
	if human {
		return payload, tools.Failure("approval_denied", reason), true, nil
	}
	matchesCause := payload.Actor == "system" && payload.StopCause == strings.TrimSpace(cause)
	legacySystemCancellation := payload.Actor == "system" && payload.StopCause == ""
	if !matchesCause && !legacySystemCancellation {
		return payload, tools.Result{}, false, nil
	}
	return payload, systemCancellationResult(cause), true, nil
}

// systemCancellationResult 构造未启动操作因运行停止而失败的结构化结果。
func systemCancellationResult(cause string) tools.Result {
	code := strings.TrimSpace(cause)
	if code == "" {
		code = systemCancellationFallbackCode
	}
	result := tools.Failure(code, "operation was cancelled before approval because the run was stopping")
	result.Data = mustJSON(struct {
		StopCause string `json:"stop_cause"`
	}{StopCause: cause})
	return result
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
