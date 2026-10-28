package agent

import (
	"context"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// Resume 恢复等待审批或普通运行；返回错误前与 Run 相同地兜底持久化取消。
func (loop *Loop) Resume(ctx context.Context, input RunInput, approval ports.Approval) (Result, error) {
	result, err := loop.resume(ctx, input, approval)
	return result, loop.persistCancellationIfInterrupted(ctx, input, err)
}

// resume 执行恢复主流程：处理挂起审批、未完成模型尝试与悬挂读操作。
func (loop *Loop) resume(ctx context.Context, input RunInput, approval ports.Approval) (Result, error) {
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
	if projection.Cancelling() {
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
	if err := loop.restoreMCPResults(events); err != nil {
		return Result{}, err
	}
	if err := loop.restoreSkillResults(events); err != nil {
		return Result{}, err
	}
	if err := loop.restoreProjectInstructions(events); err != nil {
		return Result{}, err
	}
	if result, stopped, budgetErr := loop.stopForPersistedResponseBudget(ctx, &projection, input, events); stopped {
		return result, budgetErr
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
		return Result{}, &ports.WaitingReconcileError{RunID: input.RunID, OperationID: projection.PendingOperation.ID, Reason: "run has a dangling side-effect operation that requires reconciliation"}
	}
	// 只读中断：含并行 started 的多个 none 操作；副作用已在上方拦截。
	if len(startedReadOperations(projection)) > 0 {
		if projection.CurrentTurn == nil {
			return Result{}, errors.New("dangling read operation has no open turn")
		}
		turnID := *projection.CurrentTurn
		if err := loop.repairInterruptedRead(ctx, &projection, input, events); err != nil {
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
		denied := &ports.PolicyDeniedError{Summary: approval.Reason}
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
		var budget *ports.BudgetExceededError
		if errors.As(err, &budget) {
			return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
		}
		return Result{}, err
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalGranted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Actor       string `json:"actor"`
	}{Fingerprint: pending.Fingerprint, Actor: "user"})}); err != nil {
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
			if errors.Is(err, ports.ErrWaitingReconcile) {
				return Result{RunID: input.RunID}, err
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			var guard *ports.LoopGuardExceededError
			if errors.As(err, &guard) {
				return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
			}
			var verificationFailure *ports.VerificationFailureError
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
			if errors.Is(err, ports.ErrWaitingReconcile) {
				return Result{RunID: input.RunID}, err
			}
			var waiting *ports.WaitingApprovalError
			if errors.As(err, &waiting) {
				return Result{RunID: input.RunID}, waiting
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			var guard *ports.LoopGuardExceededError
			if errors.As(err, &guard) {
				return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
			}
			var verificationFailure *ports.VerificationFailureError
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
	result := loop.Tools.ExecuteWithMCPIdentity(ctx, call.Name, call.Arguments, decision.MCPIdentity)
	result = redactToolResult(result)
	if result.Code == tools.CodeTerminationUnconfirmed {
		return Result{RunID: input.RunID}, &ports.WaitingReconcileError{RunID: input.RunID, OperationID: pending.OperationID, Reason: result.Preview}
	}
	eventType := domain.EventOperationSucceeded
	cancelled := result.Code == "cancelled" && ctx.Err() != nil
	if cancelled {
		eventType = domain.EventOperationCancelled
	} else if !result.OK {
		eventType = domain.EventOperationFailed
	}
	resultContext, cancel := loop.commitContext(ctx)
	defer cancel()
	payload := loop.resultPayload(call.Name, result)
	if err := loop.append(resultContext, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(payload)}); err != nil {
		return Result{}, err
	}
	if err := loop.appendToolResult(ctx, &projection, input, turnID, pending.OperationID, result); err != nil {
		return Result{}, err
	}
	if err := loop.Tools.AcceptMCPResult(call.Name, result); err != nil {
		return Result{}, err
	}
	if err := loop.Tools.AcceptSkillResult(call.Name, result); err != nil {
		return Result{}, err
	}
	if cancelled {
		return loop.persistCancellation(ctx, &projection, input, &turnID)
	}
	if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
		var guard *ports.LoopGuardExceededError
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

// startedReadOperations 返回已 started 未终结的只读（none）操作。
func startedReadOperations(projection domain.RunProjection) []domain.StartedOperation {
	var out []domain.StartedOperation
	for _, op := range projection.StartedOperations() {
		if op.SideEffectClass != "workspace" && op.SideEffectClass != "process" {
			out = append(out, op)
		}
	}
	return out
}

// repairInterruptedRead 仅闭合已中断的只读操作，不启动工具或关闭轮次；
// 调用方随后决定继续运行，还是按已落盘预算停止。支持只读并行下的多个中断窗口。
func (loop *Loop) repairInterruptedRead(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event) error {
	reads := startedReadOperations(*projection)
	if len(reads) == 0 {
		return errors.New("interrupted read repair requires a pending read-only operation")
	}
	if projection.CurrentTurn == nil {
		return errors.New("dangling read operation has no open turn")
	}
	turnID := *projection.CurrentTurn
	for _, pending := range reads {
		call, found := proposedToolCall(events, pending.ID)
		if !found {
			return errors.New("dangling read operation has no replayable tool proposal")
		}
		result := tools.Failure("interrupted_read", "read-only operation was interrupted before its result was persisted")
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.ID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
			return err
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &pending.ID, Payload: mustJSON(result)}); err != nil {
			return err
		}
	}
	return nil
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
	// 异步委派开启时，子事实未收束（非终态子或未交付完成子）的最终响应
	// 不得终态化运行：按继续轮次收束并回到 run()，由安全点交付与停靠推进。
	if loop.AsyncDelegateEnabled && asyncAdvanceRequired(projection, events) {
		if !hasAssistantMessage(events, turnID) {
			if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventAssistantMessageAppended, TurnID: &turnID, Payload: mustJSON(domain.AssistantMessagePayload{Text: response.Text})}); err != nil {
				return Result{}, err
			}
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
			return Result{}, err
		}
		input.Task = projection.Task
		return loop.run(ctx, input)
	}
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
		if errors.Is(err, ports.ErrWaitingReconcile) {
			return Result{RunID: input.RunID}, err
		}
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, projection, input, &turnID)
		}
		var waiting *ports.WaitingApprovalError
		if errors.As(err, &waiting) {
			return Result{RunID: input.RunID}, waiting
		}
		var budget *ports.BudgetExceededError
		if errors.As(err, &budget) {
			return loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
		}
		var guard *ports.LoopGuardExceededError
		if errors.As(err, &guard) {
			return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
		}
		var verificationFailure *ports.VerificationFailureError
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
			if errors.Is(err, ports.ErrWaitingReconcile) {
				return Result{RunID: input.RunID}, err
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, projection, input, &turnID)
			}
			var waiting *ports.WaitingApprovalError
			if errors.As(err, &waiting) {
				return Result{RunID: input.RunID}, waiting
			}
			var budget *ports.BudgetExceededError
			if errors.As(err, &budget) {
				return loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
			}
			var guard *ports.LoopGuardExceededError
			if errors.As(err, &guard) {
				return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
			}
			var denied *ports.PolicyDeniedError
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
