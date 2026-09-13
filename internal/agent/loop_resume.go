package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// Approval 是 cairn resume 提供的显式、一次性审批决定。
type Approval struct {
	Fingerprint string
	Approve     bool
	Reason      string
}

// Resume 恢复等待审批或普通运行；返回错误前与 Run 相同地兜底持久化取消。
func (loop *Loop) Resume(ctx context.Context, input RunInput, approval Approval) (Result, error) {
	result, err := loop.resume(ctx, input, approval)
	return result, loop.persistCancellationIfInterrupted(ctx, input, err)
}

// resume 执行恢复主流程：处理挂起审批、未完成模型尝试与悬挂读操作。
func (loop *Loop) resume(ctx context.Context, input RunInput, approval Approval) (Result, error) {
	// 会话前情装配（ADR-0018）：恢复场景下消息组装按创建事件回放补齐，
	// 输入显式携带仍优先——与 run 的装配语义一致。
	loop.PriorContext = input.PriorContext
	loop.PriorContextRef = input.PriorContextRef
	if loop.Journal == nil || loop.Provider == nil || loop.Tools == nil || loop.Gate == nil {
		return Result{}, errors.New("agent loop dependencies and policy gate are required")
	}
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return loop.persistCancellation(ctx, &projection, input, projection.CurrentTurn)
	}
	if projection.State != domain.RunRunning {
		return Result{}, errors.New("run is not resumable")
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return Result{}, errors.New("run has a dangling side-effect operation that requires reconciliation")
	}
	if projection.PendingOperation != nil {
		if projection.CurrentTurn == nil {
			return Result{}, errors.New("dangling read operation has no open turn")
		}
		pending := projection.PendingOperation
		call, found := proposedToolCall(events, pending.ID)
		if !found {
			return Result{}, errors.New("dangling read operation has no replayable tool proposal")
		}
		turnID := *projection.CurrentTurn
		result := tools.Failure("interrupted_read", "read-only operation was interrupted before its result was persisted")
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.ID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &pending.ID, Payload: mustJSON(result)}); err != nil {
			return Result{}, err
		}
		if err := loop.closeContinuationTurn(ctx, &projection, input, turnID); err != nil {
			return Result{}, err
		}
		if err := loop.appendResume(ctx, &projection, &input); err != nil {
			return Result{}, err
		}
		input.Task = projection.Task
		return loop.run(ctx, input)
	}
	if projection.PendingApproval == nil {
		if err := loop.appendResume(ctx, &projection, &input); err != nil {
			return Result{}, err
		}
	}
	// A persisted approval is the current authoritative continuation point. A
	// completed model response can coexist with a verification approval, so it
	// must not be replayed until that approval has been resolved.
	if projection.CurrentTurn != nil && projection.PendingApproval == nil {
		turnID := *projection.CurrentTurn
		if response, found := completedModelResponseForTurn(events, turnID); found {
			input.Task = projection.Task
			if len(response.ToolCalls) == 0 {
				return loop.resumeFinalModelResponse(ctx, &projection, input, turnID, response, events)
			}
			return loop.resumeToolResponse(ctx, &projection, input, turnID, response, false)
		}
	}
	if projection.PendingApproval == nil {
		attemptID, turnID, incomplete := incompleteModelAttempt(events)
		if !incomplete {
			if projection.CurrentTurn == nil {
				input.Task = projection.Task
				return loop.run(ctx, input)
			}
			return Result{}, errors.New("open turn has no resumable model attempt or approval")
		}
		if projection.CurrentTurn == nil || *projection.CurrentTurn != turnID {
			return Result{}, errors.New("interrupted model attempt does not match the open turn")
		}
		input.Task = projection.Task
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(struct {
			Error     string `json:"error"`
			Retryable bool   `json:"retryable"`
		}{Error: "interrupted", Retryable: true})}); err != nil {
			return Result{}, err
		}
		if err := loop.closeContinuationTurn(ctx, &projection, input, turnID); err != nil {
			return Result{}, err
		}
		return loop.run(ctx, input)
	}
	if projection.CurrentTurn == nil {
		return Result{}, errors.New("run is not waiting for an approval")
	}
	input.Task = projection.Task
	pending := projection.PendingApproval
	if approval.Fingerprint == "" || approval.Fingerprint != pending.Fingerprint {
		return Result{}, errors.New("approval fingerprint does not match the pending operation")
	}
	call, modelProposal := proposedToolCall(events, pending.OperationID)
	verification, verificationProposal := proposedVerificationCommand(events, pending.OperationID)
	if !modelProposal && !verificationProposal {
		return Result{}, errors.New("pending approval has no replayable tool proposal")
	}
	if verificationProposal {
		call = ports.ToolCall{ID: string(pending.OperationID), Name: "exec_command", Arguments: verification.Arguments}
	}
	turnID := *projection.CurrentTurn
	if !approval.Approve {
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
			Fingerprint string `json:"fingerprint"`
			Reason      string `json:"reason"`
		}{Fingerprint: pending.Fingerprint, Reason: approval.Reason})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: tools.Failure("approval_denied", approval.Reason)})}); err != nil {
			return Result{}, err
		}
		if verificationProposal {
			if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: false})}); err != nil {
				return Result{}, err
			}
		}
		_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})})
		_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
			Error string `json:"error"`
		}{Error: "approval denied"})})
		return Result{RunID: input.RunID}, &PolicyDeniedError{Summary: approval.Reason}
	}
	// 审批判定与其后执行的 before 观察共享同一次 verifier 全树扫描：
	// 两者之间只发生事件追加（工作区已加锁），不存在可观察的工作区变更。
	observation := ""
	if verificationProposal {
		observation, err = loop.verificationObservation()
		if err != nil {
			return Result{}, fmt.Errorf("observe workspace for verification approval: %w", err)
		}
	}
	decision, err := loop.resumeDecision(projection, call, verification, verificationProposal, observation)
	if err != nil {
		return Result{}, err
	}
	if decision.Effect != policy.Ask || decision.Fingerprint != pending.Fingerprint {
		return loop.expireApproval(ctx, &projection, input, turnID, pending, call, verificationProposal, decision)
	}
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		var budget *BudgetExceededError
		if errors.As(err, &budget) {
			return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
		}
		return Result{}, err
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalGranted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
	}{Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	if err := loop.appendResume(ctx, &projection, &input); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
	}{Effect: "allow", Rule: "approved_once", Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	if verificationProposal {
		if verification.CommandIndex < 0 || verification.CommandIndex >= len(loop.Verification) {
			return Result{}, errors.New("verification command index does not match frozen configuration")
		}
		if err := loop.executeVerificationCommand(ctx, &projection, input, turnID, pending.OperationID, loop.Verification[verification.CommandIndex], observation); err != nil {
			if errors.Is(err, ErrWaitingReconcile) {
				return Result{RunID: input.RunID}, err
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			var guard *LoopGuardExceededError
			if errors.As(err, &guard) {
				return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
			}
			var verificationFailure *VerificationFailureError
			if errors.As(err, &verificationFailure) {
				if err := loop.closeContinuationTurn(ctx, &projection, input, turnID); err != nil {
					return Result{}, err
				}
				return loop.run(ctx, input)
			}
			return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
		}
		if err := loop.runVerificationCommands(ctx, &projection, input, turnID, verification.VerificationID, verification.PlanRef, verification.CommandIndex+1); err != nil {
			if errors.Is(err, ErrWaitingReconcile) {
				return Result{RunID: input.RunID}, err
			}
			var waiting *WaitingApprovalError
			if errors.As(err, &waiting) {
				return Result{RunID: input.RunID}, waiting
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			var guard *LoopGuardExceededError
			if errors.As(err, &guard) {
				return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
			}
			var verificationFailure *VerificationFailureError
			if errors.As(err, &verificationFailure) {
				if err := loop.closeContinuationTurn(ctx, &projection, input, turnID); err != nil {
					return Result{}, err
				}
				return loop.run(ctx, input)
			}
			return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
		}
		finalResponse := loop.completionResponse(projection)
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: finalResponse})}); err != nil {
			return Result{}, err
		}
		return Result{RunID: input.RunID, Response: finalResponse}, nil
	}
	startedPayload, err := loop.plannedOperationPayload(call)
	if err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(startedPayload)}); err != nil {
		return Result{}, err
	}
	result := loop.Tools.Execute(ctx, call.Name, call.Arguments)
	result = redactToolResult(result)
	if result.Code == tools.CodeTerminationUnconfirmed {
		return Result{RunID: input.RunID}, &WaitingReconcileError{RunID: input.RunID, OperationID: pending.OperationID, Reason: result.Preview}
	}
	eventType := domain.EventOperationSucceeded
	cancelled := result.Code == "cancelled" && ctx.Err() != nil
	if cancelled {
		eventType = domain.EventOperationCancelled
	} else if !result.OK {
		eventType = domain.EventOperationFailed
	}
	resultContext := ctx
	var cancel context.CancelFunc
	if cancelled {
		resultContext, cancel = loop.commitContext(ctx)
		defer cancel()
	}
	payload := loop.resultPayload(call.Name, result)
	if err := loop.append(resultContext, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(payload)}); err != nil {
		return Result{}, err
	}
	if cancelled {
		return loop.persistCancellation(ctx, &projection, input, &turnID)
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(result)}); err != nil {
		return Result{}, err
	}
	if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
		var guard *LoopGuardExceededError
		if errors.As(err, &guard) {
			return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
		}
		return Result{}, err
	}
	events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, err
	}
	response, found := completedModelResponseForTurn(events, turnID)
	if !found || len(response.ToolCalls) == 0 {
		return loop.failCurrentTurn(ctx, &projection, input, turnID, errors.New("approved tool has no replayable model response"))
	}
	return loop.resumeToolResponse(ctx, &projection, input, turnID, response, true)
}

// appendResume records an invocation only at a recovery boundary where there
// is no unresolved overlay. In particular, an invalid approval must not leave
// a misleading run_resumed event behind.
// appendResume 在恢复前记录调用标识、来源序号和 verifier 工作区观察值。
func (loop *Loop) appendResume(ctx context.Context, projection *domain.RunProjection, input *RunInput) error {
	if input.ResumeInvocation == "" {
		return nil
	}
	if input.ResumeObservation == "" || input.ResumeFromSeq == 0 {
		return errors.New("resume observation metadata is incomplete")
	}
	if projection.PendingApproval != nil || projection.PendingOperation != nil {
		return errors.New("run_resumed cannot be recorded with an unresolved overlay")
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunResumed, Payload: mustJSON(struct {
		Invocation           string `json:"invocation"`
		FromSeq              uint64 `json:"from_seq"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Invocation: input.ResumeInvocation, FromSeq: input.ResumeFromSeq, WorkspaceObservation: input.ResumeObservation})}); err != nil {
		return err
	}
	input.ResumeInvocation = ""
	return nil
}

// resumeFinalModelResponse 在恢复时重新处理已完成模型响应的最终回复或工具调用。
func (loop *Loop) resumeFinalModelResponse(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, response modelResponsePayload, events []domain.Event) (Result, error) {
	if !hasAssistantMessage(events, turnID) {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventAssistantMessageAppended, TurnID: &turnID, Payload: mustJSON(domain.AssistantMessagePayload{Text: response.Text})}); err != nil {
			return Result{}, err
		}
	}
	if err := loop.runVerification(ctx, projection, input, turnID); err != nil {
		if errors.Is(err, ErrWaitingReconcile) {
			return Result{RunID: input.RunID}, err
		}
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, projection, input, &turnID)
		}
		var waiting *WaitingApprovalError
		if errors.As(err, &waiting) {
			return Result{RunID: input.RunID}, waiting
		}
		var budget *BudgetExceededError
		if errors.As(err, &budget) {
			return loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
		}
		var guard *LoopGuardExceededError
		if errors.As(err, &guard) {
			return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
		}
		var verificationFailure *VerificationFailureError
		if errors.As(err, &verificationFailure) {
			messages, rebuildErr := loop.messagesFromEvents(events, input.Task)
			if rebuildErr != nil {
				return Result{RunID: input.RunID}, rebuildErr
			}
			messages = append(messages, ports.Message{Role: "user", Content: verificationFailure.Feedback})
			if _, closeErr := loop.closeTurnAndCompact(ctx, projection, input, turnID, messages); closeErr != nil {
				return Result{RunID: input.RunID}, closeErr
			}
			return loop.run(ctx, input)
		}
		return loop.failCurrentTurn(ctx, projection, input, turnID, err)
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: response.Text})}); err != nil {
		return Result{}, err
	}
	return Result{RunID: input.RunID, Response: response.Text}, nil
}

// resumeToolResponse 消费已持久化模型响应中尚未提议的工具调用；在最后一个成功
// apply_patch 后按开关运行冻结验证，保证恢复不会绕过自动验证闭环。
func (loop *Loop) resumeToolResponse(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, response modelResponsePayload, resumedApproval bool) (Result, error) {
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, err
	}
	proposed := proposedToolCallIDs(events)
	for _, call := range response.ToolCalls {
		if proposed[call.ID] {
			continue
		}
		_, err := loop.executeTool(ctx, projection, input, turnID, call)
		if err != nil {
			if errors.Is(err, ErrWaitingReconcile) {
				return Result{RunID: input.RunID}, err
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, projection, input, &turnID)
			}
			var waiting *WaitingApprovalError
			if errors.As(err, &waiting) {
				return Result{RunID: input.RunID}, waiting
			}
			var budget *BudgetExceededError
			if errors.As(err, &budget) {
				return loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
			}
			var guard *LoopGuardExceededError
			if errors.As(err, &guard) {
				return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
			}
			var denied *PolicyDeniedError
			if errors.As(err, &denied) {
				return loop.failCurrentTurn(ctx, projection, input, turnID, denied)
			}
			return loop.failCurrentTurn(ctx, projection, input, turnID, err)
		}
	}
	if resumedApproval {
		if err := loop.appendResume(ctx, projection, &input); err != nil {
			return Result{}, err
		}
	}
	events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, err
	}
	complete, patched, allSucceeded := completedResponseToolResults(events, response)
	if loop.VerificationAutoAfterPatch && complete && patched && allSucceeded {
		result, feedback, err := loop.finishAutomaticVerification(ctx, projection, input, turnID)
		if err != nil || feedback == "" {
			return result, err
		}
		messages, rebuildErr := loop.messagesFromEvents(events, input.Task)
		if rebuildErr != nil {
			return Result{RunID: input.RunID}, rebuildErr
		}
		messages = append(messages, ports.Message{Role: "user", Content: feedback})
		if _, err := loop.closeTurnAndCompact(ctx, projection, input, turnID, messages); err != nil {
			return Result{}, err
		}
		return loop.run(ctx, input)
	}
	if err := loop.closeContinuationTurn(ctx, projection, input, turnID); err != nil {
		return Result{}, err
	}
	return loop.run(ctx, input)
}

// expireApproval records that a one-shot approval no longer describes the
// current workspace, then lets the model make a new decision from durable
// tool/verification feedback. The old command is never started.
// expireApproval 将过期或不匹配的审批转换为可审计拒绝，而不执行关联操作。
func (loop *Loop) expireApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, pending *domain.PendingApproval, call ports.ToolCall, verificationProposal bool, _ policy.Decision) (Result, error) {
	reason := "approval expired because the command, policy, or workspace observation changed; re-decision required"
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Reason      string `json:"reason"`
	}{Fingerprint: pending.Fingerprint, Reason: reason})}); err != nil {
		return Result{}, err
	}
	result := tools.Failure("approval_stale", reason)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
		return Result{}, err
	}
	if verificationProposal {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: false})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFeedbackAppended, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
			Text string `json:"text"`
		}{Text: "Verification approval expired after workspace drift. Re-inspect the workspace and decide whether to repair or rerun verification."})}); err != nil {
			return Result{}, err
		}
	} else if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(result)}); err != nil {
		return Result{}, err
	}
	if err := loop.closeContinuationTurn(ctx, projection, input, turnID); err != nil {
		return Result{}, err
	}
	if err := loop.appendResume(ctx, projection, &input); err != nil {
		return Result{}, err
	}
	return loop.run(ctx, input)
}

// completedModelResponseForTurn 查找指定轮次已持久化的完整模型响应。
func completedModelResponseForTurn(events []domain.Event, turnID domain.TurnID) (modelResponsePayload, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventModelResponseFinished || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload modelResponsePayload
		return payload, json.Unmarshal(event.Payload, &payload) == nil
	}
	return modelResponsePayload{}, false
}

// hasAnyToolProposal 判断模型响应中的任一工具调用是否已经被提议并持久化。
func hasAnyToolProposal(events []domain.Event, calls []ports.ToolCall) bool {
	callIDs := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		callIDs[call.ID] = struct{}{}
	}
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload toolCallPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			if _, found := callIDs[payload.CallID]; found {
				return true
			}
		}
	}
	return false
}

// proposedToolCallIDs 返回已经持久化提议的模型工具调用 ID，用于恢复一条
// 部分执行的模型响应时避免重复提议或重复执行。
func proposedToolCallIDs(events []domain.Event) map[string]bool {
	ids := make(map[string]bool)
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload toolCallPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" {
			ids[payload.CallID] = true
		}
	}
	return ids
}

// completedResponseToolResults 汇总一条模型响应中各调用是否都已有持久化结果，
// 并确认是否至少成功写入过一个补丁。它用于恢复路径，避免在部分工具批次中
// 提前验证或重复执行已有调用。
func completedResponseToolResults(events []domain.Event, response modelResponsePayload) (complete, successfulPatch, allSucceeded bool) {
	proposals := make(map[domain.OperationID]toolCallPayload)
	results := make(map[domain.OperationID]tools.Result)
	for _, event := range events {
		if event.OperationID == nil {
			continue
		}
		switch event.Type {
		case domain.EventToolCallProposed:
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				proposals[*event.OperationID] = payload
			}
		case domain.EventToolResultAppended:
			var result tools.Result
			if json.Unmarshal(event.Payload, &result) == nil {
				results[*event.OperationID] = result
			}
		}
	}
	byCallID := make(map[string]tools.Result)
	for operationID, proposal := range proposals {
		if result, found := results[operationID]; found {
			byCallID[proposal.CallID] = result
		}
	}
	if len(response.ToolCalls) == 0 {
		return true, false, true
	}
	allSucceeded = true
	for _, call := range response.ToolCalls {
		result, found := byCallID[call.ID]
		if !found {
			return false, false, false
		}
		allSucceeded = allSucceeded && result.OK
		successfulPatch = successfulPatch || call.Name == "apply_patch" && result.OK
	}
	return true, successfulPatch, allSucceeded
}

// hasAssistantMessage 判断指定轮次是否已写入用户可见的助手消息。
func hasAssistantMessage(events []domain.Event, turnID domain.TurnID) bool {
	for _, event := range events {
		if event.Type == domain.EventAssistantMessageAppended && event.TurnID != nil && *event.TurnID == turnID {
			return true
		}
	}
	return false
}

// incompleteModelAttempt 查找请求已开始但未持久化终态的模型尝试。
func incompleteModelAttempt(events []domain.Event) (domain.AttemptID, domain.TurnID, bool) {
	pending := make(map[domain.AttemptID]domain.TurnID)
	for _, event := range events {
		switch event.Type {
		case domain.EventModelRequestStarted:
			if event.AttemptID != nil && event.TurnID != nil {
				pending[*event.AttemptID] = *event.TurnID
			}
		case domain.EventModelResponseFinished, domain.EventModelRequestFailed:
			if event.AttemptID != nil {
				delete(pending, *event.AttemptID)
			}
		}
	}
	var latest domain.AttemptID
	var turn domain.TurnID
	for attempt, turnID := range pending {
		if attemptOrderAfter(attempt, latest) {
			latest, turn = attempt, turnID
		}
	}
	return latest, turn, latest != ""
}

// attemptOrderAfter 判断 attempt a 是否晚于 b。ID 形如 前缀_UnixNano_熵：
// 先按时间戳数值比较（同纳秒或时钟回拨时字典序不可靠），时间戳解析失败时
// 回退字典序以保持确定性。
func attemptOrderAfter(a, b domain.AttemptID) bool {
	aTS, aOK := attemptTimestamp(a)
	bTS, bOK := attemptTimestamp(b)
	if aOK && bOK && aTS != bTS {
		return aTS > bTS
	}
	return string(a) > string(b)
}

// attemptTimestamp 提取 attempt ID 中第二段的 UnixNano 时间戳。
func attemptTimestamp(id domain.AttemptID) (int64, bool) {
	parts := strings.Split(string(id), "_")
	if len(parts) != 3 {
		return 0, false
	}
	value, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}

// proposedVerificationCommand 从事件历史恢复某操作对应的验证命令提议。
func proposedVerificationCommand(events []domain.Event, operationID domain.OperationID) (verificationProposalPayload, bool) {
	for _, event := range events {
		if event.Type != domain.EventVerificationCommandProposed || event.OperationID == nil || *event.OperationID != operationID {
			continue
		}
		var payload verificationProposalPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return verificationProposalPayload{}, false
		}
		return payload, payload.VerificationID != "" && payload.CommandIndex >= 0 && json.Valid(payload.Arguments)
	}
	return verificationProposalPayload{}, false
}
