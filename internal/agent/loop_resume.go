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
	if projection.State == domain.RunCancelling {
		result, _, cancellationErr := loop.repairMissingCancellationTerminal(ctx, &projection, input, events)
		return result, cancellationErr
	}
	operationRepaired, err := loop.repairUnstartedOperationTerminals(ctx, &projection, input, events)
	if err != nil {
		return Result{}, err
	}
	if operationRepaired {
		events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if err != nil {
			return Result{}, fmt.Errorf("reload run after repairing unstarted operations: %w", err)
		}
		projection, err = domain.Replay(events)
		if err != nil {
			return Result{}, fmt.Errorf("replay run after repairing unstarted operations: %w", err)
		}
	}
	repaired, err := loop.repairMissingToolResults(ctx, &projection, input, events)
	if err != nil {
		return Result{}, err
	}
	if repaired {
		events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if err != nil {
			return Result{}, fmt.Errorf("reload run after repairing tool results: %w", err)
		}
		projection, err = domain.Replay(events)
		if err != nil {
			return Result{}, fmt.Errorf("replay run after repairing tool results: %w", err)
		}
	}
	verificationRepaired, err := loop.repairIncompleteVerification(ctx, &projection, input, events)
	if err != nil {
		return Result{RunID: input.RunID}, err
	}
	if verificationRepaired {
		events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if err != nil {
			return Result{}, fmt.Errorf("reload run after repairing verification: %w", err)
		}
		projection, err = domain.Replay(events)
		if err != nil {
			return Result{}, fmt.Errorf("replay run after repairing verification: %w", err)
		}
	}
	if result, handled, cancellationErr := loop.repairMissingCancellationTerminal(ctx, &projection, input, events); handled {
		return result, cancellationErr
	}
	if result, handled, terminalErr := loop.repairMissingRunTerminal(ctx, &projection, input, events); handled {
		return result, terminalErr
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
		denied := &PolicyDeniedError{Summary: approval.Reason}
		result := tools.Failure("approval_denied", approval.Reason)
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
			Fingerprint string `json:"fingerprint"`
			Actor       string `json:"actor"`
			Reason      string `json:"reason"`
		}{Fingerprint: pending.Fingerprint, Actor: "user", Reason: approval.Reason})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
			return Result{}, err
		}
		if verificationProposal {
			if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{VerificationID: verification.VerificationID, OK: false})}); err != nil {
				return Result{RunID: input.RunID}, &verificationPersistenceError{cause: err}
			}
		} else if err := loop.appendToolResult(ctx, &projection, input, turnID, pending.OperationID, result); err != nil {
			return Result{}, err
		}
		return loop.persistRunFailure(ctx, &projection, input, &turnID, runFailurePayload{Error: "approval denied", Reason: approval.Reason}, denied)
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
		return loop.expireApproval(ctx, &projection, input, turnID, pending, call, verification, verificationProposal, decision)
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
		commandsFingerprint, fingerprintErr := persistedVerificationCommandsFingerprint(events, turnID, verification.VerificationID)
		if fingerprintErr != nil {
			return Result{}, fingerprintErr
		}
		command, commandErr := loop.verificationCommand(verification.VerificationID, verification.PlanRef, commandsFingerprint, verification.CommandIndex)
		if commandErr != nil {
			return Result{}, commandErr
		}
		if err := loop.executeVerificationCommand(ctx, &projection, input, turnID, pending.OperationID, command, observation); err != nil {
			var persistence *verificationPersistenceError
			if errors.As(err, &persistence) {
				return Result{RunID: input.RunID}, err
			}
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
		if err := loop.runVerificationCommands(ctx, &projection, input, turnID, verification.VerificationID, verification.PlanRef, commandsFingerprint, verification.CommandIndex+1); err != nil {
			var persistence *verificationPersistenceError
			if errors.As(err, &persistence) {
				return Result{RunID: input.RunID}, err
			}
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
	if err := loop.appendToolResult(ctx, &projection, input, turnID, pending.OperationID, result); err != nil {
		return Result{}, err
	}
	if cancelled {
		return loop.persistCancellation(ctx, &projection, input, &turnID)
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
		var persistence *verificationPersistenceError
		if errors.As(err, &persistence) {
			return Result{RunID: input.RunID}, err
		}
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
			var persistence *toolResultPersistenceError
			if errors.As(err, &persistence) {
				return Result{RunID: input.RunID}, err
			}
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
func (loop *Loop) expireApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, pending *domain.PendingApproval, call ports.ToolCall, verification verificationProposalPayload, verificationProposal bool, _ policy.Decision) (Result, error) {
	reason := "approval expired because the command, policy, or workspace observation changed; re-decision required"
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
		StopCause   string `json:"stop_cause"`
	}{Fingerprint: pending.Fingerprint, Actor: "system", Reason: reason, StopCause: "approval_stale"})}); err != nil {
		return Result{}, err
	}
	result := tools.Failure("approval_stale", reason)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
		return Result{}, err
	}
	if verificationProposal {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{VerificationID: verification.VerificationID, OK: false})}); err != nil {
			return Result{RunID: input.RunID}, &verificationPersistenceError{cause: err}
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFeedbackAppended, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
			Text string `json:"text"`
		}{Text: "Verification approval expired after workspace drift. Re-inspect the workspace and decide whether to repair or rerun verification."})}); err != nil {
			return Result{RunID: input.RunID}, &verificationPersistenceError{cause: err}
		}
	} else if err := loop.appendToolResult(ctx, projection, input, turnID, pending.OperationID, result); err != nil {
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

// runCancelRequestedPayload 保存取消请求决定的预期终态与失败维度。
type runCancelRequestedPayload struct {
	Cause            string `json:"cause"`
	ExpectedTerminal string `json:"expected_terminal,omitempty"`
	BudgetDimension  string `json:"budget_dimension,omitempty"`
}

// repairMissingCancellationTerminal 在普通恢复路径之前收束已持久化的取消事实；
// operation_cancelled 只用于补写取消序列，绝不重新执行对应工具。
func (loop *Loop) repairMissingCancellationTerminal(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (Result, bool, error) {
	if projection.State == domain.RunCancelling {
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
		return &BudgetExceededError{Dimension: request.BudgetDimension}
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
func PendingUnstartedOperationRepairs(events []domain.Event) ([]UnstartedOperationRepair, error) {
	type operationRecord struct {
		turnID       domain.TurnID
		toolName     string
		started      bool
		terminal     bool
		authorized   bool
		verification bool
		failure      *tools.Result
	}
	records := make(map[domain.OperationID]*operationRecord)
	order := make([]domain.OperationID, 0)
	for _, event := range events {
		if event.OperationID == nil {
			continue
		}
		operationID := *event.OperationID
		switch event.Type {
		case domain.EventToolCallProposed:
			if event.TurnID == nil {
				return nil, fmt.Errorf("tool proposal %s has no turn", operationID)
			}
			var payload toolCallPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode tool proposal %s: %w", operationID, err)
			}
			records[operationID] = &operationRecord{turnID: *event.TurnID, toolName: payload.ToolName}
			order = append(order, operationID)
		case domain.EventVerificationCommandProposed:
			if event.TurnID == nil {
				return nil, fmt.Errorf("verification proposal %s has no turn", operationID)
			}
			records[operationID] = &operationRecord{turnID: *event.TurnID, toolName: "exec_command", verification: true}
			order = append(order, operationID)
		case domain.EventPolicyDecided:
			record, found := records[operationID]
			if !found {
				continue
			}
			var payload policyDecisionPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode policy decision for operation %s: %w", operationID, err)
			}
			switch payload.Effect {
			case "allow":
				record.authorized = true
			case "deny":
				result := policyFailureResult(policy.Decision{
					Summary:     payload.Summary,
					FailureCode: payload.FailureCode,
					FailureData: payload.FailureData,
				})
				record.failure = &result
			}
		case domain.EventApprovalGranted:
			if record, found := records[operationID]; found {
				record.authorized = true
			}
		case domain.EventApprovalDenied:
			record, found := records[operationID]
			if !found {
				continue
			}
			var payload struct {
				Actor     string `json:"actor"`
				Reason    string `json:"reason"`
				StopCause string `json:"stop_cause"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decode approval denial for operation %s: %w", operationID, err)
			}
			human, _, err := humanApprovalDenial(event.Payload)
			if err != nil {
				return nil, fmt.Errorf("classify approval denial for operation %s: %w", operationID, err)
			}
			code := ""
			switch {
			case human:
				code = "approval_denied"
			case payload.StopCause == "approval_stale" || strings.Contains(strings.ToLower(payload.Reason), "approval expired because the command, policy, or workspace observation changed"):
				code = "approval_stale"
			}
			if code != "" {
				result := tools.Failure(code, payload.Reason)
				record.failure = &result
			}
		case domain.EventOperationStarted:
			if record, found := records[operationID]; found {
				record.started = true
			}
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			if record, found := records[operationID]; found {
				record.terminal = true
			}
		}
	}
	repairs := make([]UnstartedOperationRepair, 0)
	for _, operationID := range order {
		record := records[operationID]
		if record.started || record.terminal {
			continue
		}
		result := record.failure
		if result == nil && record.authorized && !record.verification {
			failure := tools.Failure("operation_not_started", "operation authorization was persisted, but execution did not start; automatic replay is prohibited")
			result = &failure
		}
		if result == nil {
			continue
		}
		repairs = append(repairs, UnstartedOperationRepair{
			OperationID: operationID,
			TurnID:      record.turnID,
			ToolName:    record.toolName,
			Result:      *result,
		})
	}
	return repairs, nil
}

// repairUnstartedOperationTerminals 为确定性中断窗口补写失败终态；它不执行工具，
// 后续恢复器再根据提议类型补齐模型结果或验证收束事件。
func (loop *Loop) repairUnstartedOperationTerminals(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (bool, error) {
	repairs, err := PendingUnstartedOperationRepairs(events)
	if err != nil {
		return false, err
	}
	if len(repairs) == 0 {
		return false, nil
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	for _, repair := range repairs {
		operationID := repair.OperationID
		turnID := repair.TurnID
		if err := loop.append(commitContext, projection, domain.EventDraft{
			RunID:         input.RunID,
			WorkspaceHash: input.WorkspaceHash,
			Type:          domain.EventOperationFailed,
			TurnID:        &turnID,
			OperationID:   &operationID,
			Payload:       mustJSON(operationPayload{ToolName: repair.ToolName, Result: repair.Result}),
		}); err != nil {
			return false, fmt.Errorf("repair unstarted operation %s: %w", operationID, err)
		}
	}
	return true, nil
}

// repairMissingToolResults 从工具操作的既有终态证据补写缺失结果；它只处理
// 模型工具提议，不执行工具，也不为验证命令制造模型可见结果。
func (loop *Loop) repairMissingToolResults(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (bool, error) {
	type operationRecord struct {
		turnID         domain.TurnID
		terminal       domain.Event
		hasTerminal    bool
		resultAppended bool
	}
	records := make(map[domain.OperationID]*operationRecord)
	order := make([]domain.OperationID, 0)
	for _, event := range events {
		if event.OperationID == nil {
			continue
		}
		operationID := *event.OperationID
		switch event.Type {
		case domain.EventToolCallProposed:
			if event.TurnID == nil {
				return false, fmt.Errorf("tool proposal %s has no turn", operationID)
			}
			records[operationID] = &operationRecord{turnID: *event.TurnID}
			order = append(order, operationID)
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			if record, found := records[operationID]; found {
				record.terminal = event
				record.hasTerminal = true
			}
		case domain.EventToolResultAppended:
			if record, found := records[operationID]; found {
				record.resultAppended = true
			}
		}
	}
	repaired := false
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	for _, operationID := range order {
		record := records[operationID]
		if !record.hasTerminal || record.resultAppended {
			continue
		}
		if record.terminal.Type == domain.EventOperationReconciled {
			var payload struct {
				Verdict string `json:"verdict"`
			}
			if err := json.Unmarshal(record.terminal.Payload, &payload); err != nil {
				return repaired, fmt.Errorf("decode operation_reconciled payload: %w", err)
			}
			if payload.Verdict == "abort_run" {
				continue
			}
		}
		result, err := toolResultFromOperationTerminal(record.terminal)
		if err != nil {
			return repaired, fmt.Errorf("rebuild tool result for operation %s: %w", operationID, err)
		}
		turnID := record.turnID
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(result)}); err != nil {
			return repaired, err
		}
		repaired = true
	}
	return repaired, nil
}

// toolResultFromOperationTerminal 从普通操作终态或人工对账结论严格重建
// 模型可见结果，不依赖当前工作区，也不再次执行副作用。
func toolResultFromOperationTerminal(event domain.Event) (tools.Result, error) {
	switch event.Type {
	case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled:
		var payload struct {
			Result json.RawMessage `json:"result"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return tools.Result{}, fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		if len(payload.Result) == 0 || string(payload.Result) == "null" {
			return tools.Result{}, fmt.Errorf("%s payload has no inline result", event.Type)
		}
		var result tools.Result
		if err := json.Unmarshal(payload.Result, &result); err != nil {
			return tools.Result{}, fmt.Errorf("decode %s result: %w", event.Type, err)
		}
		return result, nil
	case domain.EventOperationReconciled:
		var payload struct {
			Verdict string `json:"verdict"`
			Note    string `json:"note"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return tools.Result{}, fmt.Errorf("decode operation_reconciled payload: %w", err)
		}
		switch payload.Verdict {
		case "applied_success", "applied_failure", "not_applied":
		case "abort_run":
			return tools.Result{}, errors.New("operation_reconciled abort_run has no model-visible tool result")
		default:
			return tools.Result{}, fmt.Errorf("operation_reconciled has invalid verdict %q", payload.Verdict)
		}
		return tools.Result{OK: payload.Verdict == "applied_success", Code: "reconciled_" + payload.Verdict, Preview: payload.Note}, nil
	default:
		return tools.Result{}, fmt.Errorf("event %s is not an operation terminal", event.Type)
	}
}

// repairMissingRunTerminal 根据开放轮次的确定失败证据、已关闭轮次或 abort_run
// 对账事实补写缺失终态；可重试模型失败只关闭轮次，保持运行可继续。
func (loop *Loop) repairMissingRunTerminal(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) (Result, bool, error) {
	if projection.State != domain.RunRunning && projection.State != domain.RunCancelling {
		return Result{}, false, nil
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if projection.CurrentTurn != nil {
		evidence, found, err := openTurnFailureEvidence(events, *projection.CurrentTurn)
		if err != nil {
			return Result{RunID: input.RunID}, true, err
		}
		if !found {
			return Result{}, false, nil
		}
		cause := errors.New(evidence.failure.Error)
		if evidence.approvalDenied || evidence.policyDenied {
			cause = &PolicyDeniedError{Summary: evidence.failure.Reason}
		}
		if evidence.retryable {
			turnID := *projection.CurrentTurn
			if appendErr := loop.append(commitContext, projection, domain.EventDraft{
				RunID:         input.RunID,
				WorkspaceHash: input.WorkspaceHash,
				Type:          domain.EventTurnClosed,
				TurnID:        &turnID,
				Payload:       mustJSON(domain.TurnClosedPayload{Outcome: "failed"}),
			}); appendErr != nil {
				return Result{RunID: input.RunID}, true, errors.Join(cause, appendErr)
			}
			return Result{}, false, nil
		}
		result, persistErr := loop.persistRunFailure(commitContext, projection, input, projection.CurrentTurn, evidence.failure, cause)
		return result, true, persistErr
	}
	closed, closeIndex, found, err := latestClosedTurn(events)
	if err != nil {
		return Result{RunID: input.RunID}, true, err
	}
	if !found {
		return Result{}, false, nil
	}
	switch closed.outcome {
	case "final":
		response, responseErr := loop.closedTurnFinalResponse(events, closeIndex, closed.turnID, *projection)
		if responseErr != nil {
			return Result{RunID: input.RunID}, true, responseErr
		}
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: response})}); err != nil {
			return Result{RunID: input.RunID}, true, err
		}
		return Result{RunID: input.RunID, Response: response}, true, nil
	case "failed":
		failure, retryable, failureErr := failedTurnEvidence(events, closeIndex, closed.turnID)
		if failureErr != nil {
			return Result{RunID: input.RunID}, true, failureErr
		}
		if retryable && projection.State == domain.RunRunning {
			return Result{}, false, nil
		}
		cause := errors.New(failure.Error)
		result, persistErr := loop.persistRunFailure(commitContext, projection, input, nil, failure, cause)
		return result, true, persistErr
	default:
		return Result{}, false, nil
	}
}

// openTurnFailureRecord 描述开放轮次中已持久化且足以决定恢复方向的失败事实。
type openTurnFailureRecord struct {
	failure        runFailurePayload
	retryable      bool
	approvalDenied bool
	policyDenied   bool
}

// openTurnFailureEvidence 查找开放轮次中的运行级失败证据；人工拒绝、验证策略
// 拒绝和 abort_run 总是终止运行，模型失败仅在它是最近模型尝试结果时生效。
func openTurnFailureEvidence(events []domain.Event, turnID domain.TurnID) (openTurnFailureRecord, bool, error) {
	hasModelResponse := false
	verificationOperations := make(map[domain.OperationID]struct{})
	for _, event := range events {
		if event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		if event.Type == domain.EventModelResponseFinished {
			hasModelResponse = true
		}
		if event.Type == domain.EventVerificationCommandProposed && event.OperationID != nil {
			verificationOperations[*event.OperationID] = struct{}{}
		}
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		switch event.Type {
		case domain.EventPolicyDecided:
			if event.OperationID == nil {
				continue
			}
			if _, found := verificationOperations[*event.OperationID]; !found {
				continue
			}
			var payload policyDecisionPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return openTurnFailureRecord{}, false, fmt.Errorf("decode verification policy decision: %w", err)
			}
			if payload.Effect != string(policy.Deny) {
				continue
			}
			result := policyFailureResult(policy.Decision{
				Summary:     payload.Summary,
				FailureCode: payload.FailureCode,
				FailureData: payload.FailureData,
			})
			return openTurnFailureRecord{
				failure: runFailurePayload{
					Error:      (&PolicyDeniedError{Summary: payload.Summary}).Error(),
					ErrorClass: result.Code,
					Reason:     payload.Summary,
				},
				policyDenied: true,
			}, true, nil
		case domain.EventOperationReconciled:
			var payload struct {
				Verdict string `json:"verdict"`
				Note    string `json:"note"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return openTurnFailureRecord{}, false, fmt.Errorf("decode operation_reconciled payload: %w", err)
			}
			if payload.Verdict == "abort_run" {
				return openTurnFailureRecord{
					failure: runFailurePayload{Error: "reconciliation aborted", Reason: payload.Note},
				}, true, nil
			}
		case domain.EventApprovalDenied:
			human, reason, err := humanApprovalDenial(event.Payload)
			if err != nil {
				return openTurnFailureRecord{}, false, fmt.Errorf("decode approval_denied payload: %w", err)
			}
			if !human {
				continue
			}
			return openTurnFailureRecord{
				failure:        runFailurePayload{Error: "approval denied", ErrorClass: "approval_denied", Reason: reason},
				approvalDenied: true,
			}, true, nil
		case domain.EventOperationFailed:
			if hasModelResponse {
				continue
			}
			result, err := toolResultFromOperationTerminal(event)
			if err != nil {
				return openTurnFailureRecord{}, false, err
			}
			message := strings.TrimSpace(result.Preview)
			if message == "" {
				message = result.Code
			}
			if message == "" {
				message = "scripted operation failed"
			}
			return openTurnFailureRecord{
				failure: runFailurePayload{Error: message, ErrorClass: result.Code},
			}, true, nil
		}
	}
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		switch event.Type {
		case domain.EventModelRequestFailed:
			var payload modelRequestFailedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return openTurnFailureRecord{}, false, fmt.Errorf("decode model_request_failed payload: %w", err)
			}
			failure := runFailurePayload{Error: payload.Error, ErrorClass: payload.ErrorClass}
			if failure.Error == "" {
				failure.Error = "model request failed"
			}
			return openTurnFailureRecord{failure: failure, retryable: payload.Retryable}, true, nil
		case domain.EventModelRequestStarted, domain.EventModelResponseFinished:
			return openTurnFailureRecord{}, false, nil
		}
	}
	return openTurnFailureRecord{}, false, nil
}

// closedTurnRecord 保存最近关闭轮次的标识与结果。
type closedTurnRecord struct {
	turnID  domain.TurnID
	outcome string
}

// latestClosedTurn 返回事件流中最近一次轮次关闭及其位置。
func latestClosedTurn(events []domain.Event) (closedTurnRecord, int, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventTurnClosed || event.TurnID == nil {
			continue
		}
		var payload domain.TurnClosedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return closedTurnRecord{}, 0, false, fmt.Errorf("decode turn_closed payload: %w", err)
		}
		return closedTurnRecord{turnID: *event.TurnID, outcome: payload.Outcome}, index, true, nil
	}
	return closedTurnRecord{}, 0, false, nil
}

// closedTurnFinalResponse 从同轮次的助手消息恢复最终文本；工具轮次自动验证
// 收束时复用既有运行时结论生成规则。
func (loop *Loop) closedTurnFinalResponse(events []domain.Event, closeIndex int, turnID domain.TurnID, projection domain.RunProjection) (string, error) {
	for index := closeIndex - 1; index >= 0; index-- {
		event := events[index]
		if event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		if event.Type == domain.EventAssistantMessageAppended {
			var payload domain.AssistantMessagePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return "", fmt.Errorf("decode assistant message for closed turn: %w", err)
			}
			return payload.Text, nil
		}
		if event.Type == domain.EventModelResponseFinished {
			var payload modelResponsePayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return "", fmt.Errorf("decode model response for closed turn: %w", err)
			}
			if len(payload.ToolCalls) == 0 {
				return payload.Text, nil
			}
			break
		}
	}
	return loop.completionResponse(projection), nil
}

// failedTurnEvidence 从失败轮次已有事件中选择最近且最具体的失败证据；
// 没有细节时仅记录轮次已失败这一事实，不猜测额外原因。
func failedTurnEvidence(events []domain.Event, closeIndex int, turnID domain.TurnID) (runFailurePayload, bool, error) {
	for index := closeIndex - 1; index >= 0; index-- {
		event := events[index]
		sameTurn := event.TurnID != nil && *event.TurnID == turnID
		switch event.Type {
		case domain.EventModelRequestFailed:
			if !sameTurn {
				continue
			}
			var payload modelRequestFailedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return runFailurePayload{}, false, fmt.Errorf("decode model_request_failed payload: %w", err)
			}
			failure := runFailurePayload{Error: payload.Error, ErrorClass: payload.ErrorClass}
			if failure.Error == "" {
				failure.Error = "model request failed"
			}
			return failure, payload.Retryable, nil
		case domain.EventOperationFailed:
			if !sameTurn {
				continue
			}
			result, err := toolResultFromOperationTerminal(event)
			if err != nil {
				return runFailurePayload{}, false, err
			}
			message := strings.TrimSpace(result.Preview)
			if message == "" {
				message = result.Code
			}
			if message == "" {
				message = "operation failed"
			}
			return runFailurePayload{Error: message, ErrorClass: result.Code}, false, nil
		case domain.EventApprovalDenied:
			if !sameTurn {
				continue
			}
			human, reason, err := humanApprovalDenial(event.Payload)
			if err != nil {
				return runFailurePayload{}, false, fmt.Errorf("decode approval_denied payload: %w", err)
			}
			if !human {
				continue
			}
			return runFailurePayload{Error: "approval denied", ErrorClass: "approval_denied", Reason: reason}, false, nil
		case domain.EventOperationReconciled:
			if !sameTurn {
				continue
			}
			var payload struct {
				Verdict string `json:"verdict"`
				Note    string `json:"note"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return runFailurePayload{}, false, fmt.Errorf("decode operation_reconciled payload: %w", err)
			}
			if payload.Verdict == "abort_run" {
				return runFailurePayload{Error: "reconciliation aborted", Reason: payload.Note}, false, nil
			}
			if payload.Verdict == "applied_failure" {
				return runFailurePayload{Error: "operation reconciled as applied_failure", Reason: payload.Note}, false, nil
			}
		case domain.EventPolicyDecided:
			if !sameTurn {
				continue
			}
			var payload struct {
				Effect  string `json:"effect"`
				Summary string `json:"summary"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return runFailurePayload{}, false, fmt.Errorf("decode policy_decided payload: %w", err)
			}
			if payload.Effect == "deny" {
				return runFailurePayload{Error: "policy denied", Reason: payload.Summary}, false, nil
			}
		case domain.EventRunCancelRequested:
			var payload struct {
				ExpectedTerminal string `json:"expected_terminal"`
				Dimension        string `json:"budget_dimension"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return runFailurePayload{}, false, fmt.Errorf("decode run_cancel_requested payload: %w", err)
			}
			if payload.ExpectedTerminal == "failed" {
				failure := runFailurePayload{Error: "run cancellation requested", BudgetDimension: payload.Dimension}
				if payload.Dimension != "" {
					failure.Error = (&BudgetExceededError{Dimension: payload.Dimension}).Error()
				}
				return failure, false, nil
			}
		}
	}
	return runFailurePayload{Error: "turn closed with failed outcome"}, false, nil
}

// humanApprovalDenial 区分人工拒绝与系统关闭、过期拒绝；旧事件缺少 actor
// 时，除已知 stale 文本外继续按人工拒绝解释，保持历史行为兼容。
func humanApprovalDenial(raw json.RawMessage) (bool, string, error) {
	var payload struct {
		Actor     string `json:"actor"`
		Reason    string `json:"reason"`
		StopCause string `json:"stop_cause"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return false, "", err
	}
	switch strings.ToLower(strings.TrimSpace(payload.Actor)) {
	case "user":
		return true, payload.Reason, nil
	case "":
		if strings.TrimSpace(payload.StopCause) != "" || strings.Contains(strings.ToLower(payload.Reason), "approval expired because the command, policy, or workspace observation changed") {
			return false, payload.Reason, nil
		}
		return true, payload.Reason, nil
	default:
		return false, payload.Reason, nil
	}
}
