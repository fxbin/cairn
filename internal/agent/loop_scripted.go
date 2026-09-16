package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// RunScriptedToolPlan 在不调用模型的前提下，按固定顺序执行一组由调用方确定
// 的工具调用，并在完成后走既有验证收束完成运行。每个调用都完整经过
// 提议 → 策略 → 审批 → operation_started → 结果 → tool_result 事件链；等待
// 审批时返回 WaitingApprovalError，由调用方携带审批决定再次进入本方法续跑。
// 已提议过的调用按事件日志去重，因此续跑是幂等的。
func (loop *Loop) RunScriptedToolPlan(ctx context.Context, input RunInput, approval Approval, calls []ports.ToolCall) (Result, error) {
	result, err := loop.runScriptedToolPlan(ctx, input, approval, calls)
	return result, loop.persistCancellationIfInterrupted(ctx, input, err)
}

// runScriptedToolPlan 执行脚本化计划主流程：处理挂起审批与中断恢复，随后
// 依次执行剩余调用、运行冻结验证并以确定性结论收束运行。
func (loop *Loop) runScriptedToolPlan(ctx context.Context, input RunInput, approval Approval, calls []ports.ToolCall) (Result, error) {
	// 与模型循环不同，脚本化计划不触碰 Provider；模型无关性是它存在的意义。
	if loop.Journal == nil || loop.Tools == nil || loop.Gate == nil {
		return Result{}, errors.New("agent loop dependencies and policy gate are required")
	}
	if input.RunID == "" || input.WorkspaceHash == "" {
		return Result{}, errors.New("run ID and workspace hash are required")
	}
	if err := validateScriptedCalls(calls); err != nil {
		return Result{}, err
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
		return Result{RunID: input.RunID}, err
	}
	if operationRepaired {
		events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if err != nil {
			return Result{}, fmt.Errorf("reload scripted run after repairing unstarted operations: %w", err)
		}
		projection, err = domain.Replay(events)
		if err != nil {
			return Result{}, fmt.Errorf("replay scripted run after repairing unstarted operations: %w", err)
		}
	}
	repaired, err := loop.repairMissingToolResults(ctx, &projection, input, events)
	if err != nil {
		return Result{RunID: input.RunID}, err
	}
	if repaired {
		events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if err != nil {
			return Result{}, fmt.Errorf("reload scripted run after repairing tool results: %w", err)
		}
		projection, err = domain.Replay(events)
		if err != nil {
			return Result{}, fmt.Errorf("replay scripted run after repairing tool results: %w", err)
		}
	}
	verificationRepaired, err := loop.repairIncompleteVerification(ctx, &projection, input, events)
	if err != nil {
		return Result{RunID: input.RunID}, err
	}
	if verificationRepaired {
		events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if err != nil {
			return Result{}, fmt.Errorf("reload scripted run after repairing verification: %w", err)
		}
		projection, err = domain.Replay(events)
		if err != nil {
			return Result{}, fmt.Errorf("replay scripted run after repairing verification: %w", err)
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
		return Result{}, fmt.Errorf("run %s is not runnable from state %s", input.RunID, projection.State)
	}
	if projection.PendingOperation != nil {
		if projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process" {
			return Result{}, fmt.Errorf("run has a dangling side-effect operation that requires reconciliation")
		}
		return Result{}, errors.New("dangling read operation has no scripted continuation")
	}
	if projection.PendingApproval != nil {
		return loop.resumeScriptedApproval(ctx, &projection, input, events, approval, calls)
	}
	proposed := activeScriptedToolCallIDs(events)
	for _, call := range calls {
		if !proposed[call.ID] {
			continue
		}
		if err := loop.healScriptedProposal(ctx, &projection, input, events, call); err != nil {
			return loop.failCurrentTurn(ctx, &projection, input, currentTurnOf(&projection), err)
		}
	}
	if projection.CurrentTurn == nil {
		turnID := domain.NewTurnID()
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID, Payload: mustJSON(struct {
			Turn int `json:"turn"`
		}{Turn: openedTurnCount(events) + 1})}); err != nil {
			return Result{}, err
		}
	}
	turnID := *projection.CurrentTurn
	for _, call := range calls {
		if proposed[call.ID] {
			continue
		}
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, &projection, input, &turnID)
		}
		result, err := loop.executeTool(ctx, &projection, input, turnID, call)
		if err != nil {
			return loop.handleScriptedToolError(ctx, &projection, input, turnID, err)
		}
		if !result.OK {
			// 计划中的调用未生效即如实终态化：继续执行会让调用方误以为整份
			// 计划已完成，留下与计划不一致的工作区状态。
			return loop.failCurrentTurn(ctx, &projection, input, turnID, fmt.Errorf("scripted tool call %s did not take effect: %s", call.ID, result.Code))
		}
	}
	return loop.finishScriptedRun(ctx, &projection, input, turnID)
}

// finishScriptedRun 运行冻结验证并完成脚本化运行；验证失败按既有"完成与
// 可信分离"语义完成但不可信（验证已如实收束为未通过，文件效果是既成事实），
// 不伪造模型修复轮次。
func (loop *Loop) finishScriptedRun(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) (Result, error) {
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
			return loop.completeScriptedRun(ctx, projection, input, turnID)
		}
		return loop.failCurrentTurn(ctx, projection, input, turnID, err)
	}
	return loop.completeScriptedRun(ctx, projection, input, turnID)
}

// completeScriptedRun 关闭当前轮次并以运行时确定性结论完成运行。
func (loop *Loop) completeScriptedRun(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) (Result, error) {
	response := loop.completionResponse(*projection)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: response})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	return Result{RunID: input.RunID, Response: response}, nil
}

// handleScriptedToolError 将执行期错误转换为与模型循环同型的终止路径。
func (loop *Loop) handleScriptedToolError(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, err error) (Result, error) {
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
	var budget *BudgetExceededError
	if errors.As(err, &budget) {
		return loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
	}
	var guard *LoopGuardExceededError
	if errors.As(err, &guard) {
		return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
	}
	var waiting *WaitingApprovalError
	if errors.As(err, &waiting) {
		return Result{RunID: input.RunID}, waiting
	}
	return loop.failCurrentTurn(ctx, projection, input, turnID, err)
}

// resumeScriptedApproval 处理脚本化运行的挂起审批：校验指纹后按用户决定拒绝
// 或授权执行，并继续剩余计划。工具提议与验证命令提议共用同一审批事件合同。
func (loop *Loop) resumeScriptedApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event, approval Approval, calls []ports.ToolCall) (Result, error) {
	if projection.CurrentTurn == nil {
		return Result{}, errors.New("run is not waiting for an approval")
	}
	pending := projection.PendingApproval
	if approval.Fingerprint == "" || approval.Fingerprint != pending.Fingerprint {
		return Result{}, errors.New("approval fingerprint does not match the pending operation")
	}
	turnID := *projection.CurrentTurn
	call, modelProposal := proposedToolCall(events, pending.OperationID)
	verification, verificationProposal := proposedVerificationCommand(events, pending.OperationID)
	if !modelProposal && !verificationProposal {
		return Result{}, errors.New("pending approval has no replayable tool proposal")
	}
	if verificationProposal {
		call = ports.ToolCall{ID: string(pending.OperationID), Name: "exec_command", Arguments: verification.Arguments}
	}
	if !approval.Approve {
		return loop.denyScriptedApproval(ctx, projection, input, turnID, pending, call, verification, verificationProposal, approval.Reason)
	}
	// 审批判定与其后执行的观察值共享同一次 verifier 扫描（工作区已加锁）。
	if verificationProposal {
		observation, err := loop.verificationObservation()
		if err != nil {
			return Result{}, fmt.Errorf("observe workspace for verification approval: %w", err)
		}
		decision, err := loop.resumeDecision(*projection, call, verification, true, observation)
		if err != nil {
			return Result{}, err
		}
		if decision.Effect != policy.Ask || decision.Fingerprint != pending.Fingerprint {
			return loop.expireScriptedApproval(ctx, projection, input, turnID, pending, call, verification, true)
		}
		return loop.grantScriptedVerification(ctx, projection, input, turnID, pending, verification, observation)
	}
	decision, err := loop.resumeDecision(*projection, call, verification, false, "")
	if err != nil {
		return Result{}, err
	}
	if decision.Effect != policy.Ask || decision.Fingerprint != pending.Fingerprint {
		return loop.expireScriptedApproval(ctx, projection, input, turnID, pending, call, verification, false)
	}
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		var budget *BudgetExceededError
		if errors.As(err, &budget) {
			return loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
		}
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalGranted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
	}{Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
	}{Effect: "allow", Rule: "approved_once", Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	// 以下授权执行序列与 Loop.resume 的工具审批分支语义一致（审批授予 → 策
	// 略回放 → operation_started 携带 before 快照 → 执行 → 终态 → 结果），
	// 差异仅在执行完毕后继续脚本化剩余计划而不是回放模型响应。
	startedPayload, err := loop.plannedOperationPayload(call)
	if err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(startedPayload)}); err != nil {
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
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(payload)}); err != nil {
		return Result{}, err
	}
	if err := loop.appendToolResult(ctx, projection, input, turnID, pending.OperationID, result); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if cancelled {
		return loop.persistCancellation(ctx, projection, input, &turnID)
	}
	if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
		var guard *LoopGuardExceededError
		if errors.As(err, &guard) {
			return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
		}
		return Result{}, err
	}
	if !result.OK {
		return loop.failCurrentTurn(ctx, projection, input, turnID, fmt.Errorf("scripted tool call %s did not take effect: %s", call.ID, result.Code))
	}
	reloaded, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, err
	}
	return loop.runScriptedToolPlan(ctx, input, Approval{}, remainingScriptedCalls(reloaded, calls))
}

// grantScriptedVerification 批准并执行挂起的验证命令，随后收束剩余验证并
// 完成脚本化运行。
func (loop *Loop) grantScriptedVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, pending *domain.PendingApproval, verification verificationProposalPayload, observation string) (Result, error) {
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, fmt.Errorf("load scripted verification plan evidence: %w", err)
	}
	commandsFingerprint, err := persistedVerificationCommandsFingerprint(events, turnID, verification.VerificationID)
	if err != nil {
		return Result{}, err
	}
	command, err := loop.verificationCommand(verification.VerificationID, verification.PlanRef, commandsFingerprint, verification.CommandIndex)
	if err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalGranted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
	}{Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
	}{Effect: "allow", Rule: "approved_once", Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	if err := loop.executeVerificationCommand(ctx, projection, input, turnID, pending.OperationID, command, observation); err != nil {
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
		var verificationFailure *VerificationFailureError
		if errors.As(err, &verificationFailure) {
			return loop.completeScriptedRun(ctx, projection, input, turnID)
		}
		return loop.failCurrentTurn(ctx, projection, input, turnID, err)
	}
	if err := loop.runVerificationCommands(ctx, projection, input, turnID, verification.VerificationID, verification.PlanRef, commandsFingerprint, verification.CommandIndex+1); err != nil {
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
			return loop.persistCancellation(ctx, projection, input, &turnID)
		}
		var verificationFailure *VerificationFailureError
		if errors.As(err, &verificationFailure) {
			return loop.completeScriptedRun(ctx, projection, input, turnID)
		}
		return loop.failCurrentTurn(ctx, projection, input, turnID, err)
	}
	return loop.completeScriptedRun(ctx, projection, input, turnID)
}

// denyScriptedApproval 将用户拒绝持久化为可审计的审批与操作失败事件并终
// 态化运行；拒绝意味着回退中止，工作区停在拒绝点。
func (loop *Loop) denyScriptedApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, pending *domain.PendingApproval, call ports.ToolCall, verification verificationProposalPayload, verificationProposal bool, reason string) (Result, error) {
	denied := &PolicyDeniedError{Summary: reason}
	result := tools.Failure("approval_denied", reason)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
	}{Fingerprint: pending.Fingerprint, Actor: "user", Reason: reason})}); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
		return Result{}, err
	}
	if verificationProposal {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{VerificationID: verification.VerificationID, OK: false})}); err != nil {
			return Result{RunID: input.RunID}, &verificationPersistenceError{cause: err}
		}
	} else if err := loop.appendToolResult(ctx, projection, input, turnID, pending.OperationID, result); err != nil {
		return Result{RunID: input.RunID}, err
	}
	return loop.persistRunFailure(ctx, projection, input, &turnID, runFailurePayload{Error: "approval denied", Reason: reason}, denied)
}

// expireScriptedApproval 将过期或不匹配的审批转换为可审计拒绝：命令、策略
// 或工作区观察已变化，原审批不再描述当前事实，脚本化计划就地终止。
func (loop *Loop) expireScriptedApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, pending *domain.PendingApproval, call ports.ToolCall, verification verificationProposalPayload, verificationProposal bool) (Result, error) {
	const reason = "approval expired because the command, policy, or workspace observation changed; re-decision required"
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
	} else if err := loop.appendToolResult(ctx, projection, input, turnID, pending.OperationID, result); err != nil {
		return Result{RunID: input.RunID}, err
	}
	cause := errors.New(reason)
	return loop.persistRunFailure(ctx, projection, input, &turnID, runFailurePayload{Error: reason, Reason: reason}, cause)
}

// healScriptedProposal 修复中断恢复窗口：从后向前选择同一 call ID 的最新有效
// 提议，避免 not_applied 后重试时误用旧 operation；已终态但结果事件缺失时，
// 从该 operation 的终态载荷确定性重建 tool_result。
func (loop *Loop) healScriptedProposal(ctx context.Context, projection *domain.RunProjection, input RunInput, events []domain.Event, call ports.ToolCall) error {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventToolCallProposed || event.OperationID == nil {
			continue
		}
		var payload toolCallPayload
		if json.Unmarshal(event.Payload, &payload) != nil || payload.CallID != call.ID {
			continue
		}
		operationID := *event.OperationID
		var terminal domain.Event
		appended := false
		for _, scan := range events {
			if scan.OperationID == nil || *scan.OperationID != operationID {
				continue
			}
			switch scan.Type {
			case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
				terminal = scan
			case domain.EventToolResultAppended:
				appended = true
			}
		}
		if terminal.Type == "" {
			return fmt.Errorf("scripted call %s has an unresolved operation; abort the run before retrying", call.ID)
		}
		if appended {
			return nil
		}
		result, err := toolResultFromOperationTerminal(terminal)
		if err != nil {
			return fmt.Errorf("rebuild terminal result for scripted call %s: %w", call.ID, err)
		}
		turnID := domain.TurnID("")
		if event.TurnID != nil {
			turnID = *event.TurnID
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(result)}); err != nil {
			return err
		}
		if !result.OK {
			return fmt.Errorf("scripted call %s did not take effect: %s", call.ID, result.Code)
		}
		return nil
	}
	return nil
}

// remainingScriptedCalls 返回计划中尚无有效提议的调用，供审批续跑后继续执行。
func remainingScriptedCalls(events []domain.Event, calls []ports.ToolCall) []ports.ToolCall {
	proposed := activeScriptedToolCallIDs(events)
	remaining := make([]ports.ToolCall, 0, len(calls))
	for _, call := range calls {
		if !proposed[call.ID] {
			remaining = append(remaining, call)
		}
	}
	return remaining
}

// validateScriptedCalls 确保计划调用标识非空且唯一，防止一次入口内重复执行
// 同一确定性副作用。
func validateScriptedCalls(calls []ports.ToolCall) error {
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		if call.ID == "" {
			return errors.New("scripted tool call ID is required")
		}
		if _, exists := seen[call.ID]; exists {
			return fmt.Errorf("duplicate scripted tool call ID %q", call.ID)
		}
		seen[call.ID] = struct{}{}
	}
	return nil
}

// activeScriptedToolCallIDs 返回仍应视为已提议的脚本调用；人工对账明确
// not_applied 的旧 operation 不具备副作用，允许同一确定性调用在新 operation
// 下重新提议和执行，其他提议保持防重语义。
func activeScriptedToolCallIDs(events []domain.Event) map[string]bool {
	operationCalls := make(map[domain.OperationID]string)
	active := make(map[string]bool)
	for _, event := range events {
		if event.Type == domain.EventToolCallProposed && event.OperationID != nil {
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" {
				operationCalls[*event.OperationID] = payload.CallID
				active[payload.CallID] = true
			}
			continue
		}
		if event.Type != domain.EventOperationReconciled || event.OperationID == nil {
			continue
		}
		var payload struct {
			Verdict string `json:"verdict"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Verdict == "not_applied" {
			delete(active, operationCalls[*event.OperationID])
		}
	}
	return active
}

// currentTurnOf 返回投影当前轮次；无轮次时返回零值供错误路径使用。
func currentTurnOf(projection *domain.RunProjection) domain.TurnID {
	if projection.CurrentTurn == nil {
		return domain.TurnID("")
	}
	return *projection.CurrentTurn
}
