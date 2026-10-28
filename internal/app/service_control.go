// 控制面用例：Resume / Reconcile / Abort。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// Boot，关闭错误与执行错误一并合并上报，绝不静默丢弃。
func (service Service) Resume(ctx context.Context, options ResumeOptions) (outcome RunResult, err error) {
	boot, err := service.assembleBoot(options.Workspace, options.StateDir, options.Config, options.OnEvent)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	events, err := boot.Journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.State != domain.RunRunning {
		return RunResult{ID: options.RunID}, errors.New("run is not resumable")
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkRunArtifactReferences(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkFrozenRunContract(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	// 异步委派启用时，含非终态子的父运行恢复失败关闭：不自动续跑子、不重发
	// 未知副作用，先显式恢复或收尾子运行。
	if err := rejectResumeWithUnfinishedAsyncChildren(options.Config.Tools.DelegateAsync, events); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=<verdict>", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	if err := enforceAdmissionGate(options.StateDir, boot.WorkspaceHash, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	// 回退 run（task_ref 承载计划制品）不进入模型续跑：按同一计划重推导反放
	// 调用后交给脚本化执行器收束；普通与回退 run 都必须先通过当前 required
	// 准入门。
	if plan, isRewind, planErr := rewindPlanOfRun(ctx, boot.Journal, boot.Artifacts, options.RunID); planErr != nil {
		return RunResult{ID: options.RunID}, planErr
	} else if isRewind {
		return service.driveRewindRun(ctx, boot, options.RunID, plan.Runs, RewindOptions{Workspace: options.Workspace, StateDir: options.StateDir, Config: options.Config, Approval: options.Approval, OnEvent: options.OnEvent})
	}
	observation, err := boot.Workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	defer cancelRun()
	resumeInput := agent.RunInput{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, ResumeInvocation: domain.NewID("invoke"), ResumeFromSeq: projection.LastSeq, ResumeObservation: observation}
	boot.Loop.AdoptSteering(options.Steering)
	result, err := boot.Loop.Resume(runContext, resumeInput, options.Approval)
	if wallBudgetExpired(runContext) && !runReachedTerminal(boot.Journal, options.RunID) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		resumeInput.ResumeInvocation = ""
		result, err = boot.Loop.Resume(expiredContext, resumeInput, options.Approval)
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, result.Response, err)
}

// Reconcile 为已开始但未持久化终态的外部操作写入人工判定；它绝不重新执行原命令或 patch。
// 返回前关闭 Boot，关闭错误与执行错误一并合并上报，绝不静默丢弃。
func (service Service) Reconcile(ctx context.Context, options ReconcileOptions) (outcome RunResult, err error) {
	var boot *Boot
	if options.Verdict == "abort_run" {
		boot, err = assembleRecoveryBoot(options.Workspace, options.StateDir, options.Config)
	} else {
		boot, err = service.assembleBoot(options.Workspace, options.StateDir, options.Config, options.OnEvent)
	}
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	events, err := boot.Journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkRunArtifactReferences(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkFrozenRunContract(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.State != domain.RunRunning || projection.PendingOperation == nil || projection.PendingOperation.ID != options.OperationID {
		return RunResult{}, ErrWaitingReconcile
	}
	if projection.PendingOperation.SideEffectClass != "workspace" && projection.PendingOperation.SideEffectClass != "process" {
		return RunResult{}, errors.New("only workspace and process operations require reconciliation")
	}
	switch options.Verdict {
	case "applied_success", "applied_failure", "not_applied", "abort_run":
	default:
		return RunResult{}, errors.New("verdict must be applied_success, applied_failure, not_applied, or abort_run")
	}
	if strings.TrimSpace(options.Note) == "" {
		return RunResult{}, errors.New("reconciliation note is required")
	}
	if projection.PendingOperation.SideEffectClass == "process" && strings.TrimSpace(options.Evidence) == "" {
		return RunResult{}, errors.New("process reconciliation requires evidence")
	}
	var rewindPlan rewindPlanArtifact
	var isRewind bool
	if options.Verdict != "abort_run" {
		if err := enforceAdmissionGate(options.StateDir, boot.WorkspaceHash, options.Config); err != nil {
			return RunResult{ID: options.RunID}, err
		}
		var planErr error
		rewindPlan, isRewind, planErr = rewindPlanOfRun(ctx, boot.Journal, boot.Artifacts, options.RunID)
		if planErr != nil {
			return RunResult{ID: options.RunID}, planErr
		}
	}
	if projection.PendingOperation.ToolName == "apply_patch" && projection.PendingOperation.PlannedPath != "" {
		observation, err := patchObservation(boot.Workspace, projection.PendingOperation.PlannedPath)
		if err != nil {
			return RunResult{ID: options.RunID}, err
		}
		switch options.Verdict {
		case "applied_success":
			if observation != projection.PendingOperation.PlannedAfter {
				return RunResult{}, fmt.Errorf("patch observation %s does not match planned after hash %s", observation, projection.PendingOperation.PlannedAfter)
			}
		case "not_applied":
			if observation != projection.PendingOperation.PlannedBefore {
				return RunResult{}, fmt.Errorf("patch observation %s does not match planned before hash %s", observation, projection.PendingOperation.PlannedBefore)
			}
		}
	}
	operationID := options.OperationID
	turnID := projection.CurrentTurn
	if turnID == nil {
		return RunResult{}, errors.New("dangling operation has no open turn")
	}
	// 对账事件的观察值与 resume 一致采用真实工作区摘要，而不是误导性的根路径。
	observation, err := boot.Workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{}, err
	}
	appendEvent := func(eventType domain.EventType, payload any) error {
		draft := domain.EventDraft{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, Type: eventType, TurnID: turnID, OperationID: &operationID, Payload: artifact.MustEncodeRedactedJSON(payload)}
		// 写入前先在克隆投影上模拟迁移，避免把不可回放的事件永久写入日志。
		if err := projection.ValidateDraft(draft); err != nil {
			return fmt.Errorf("validate %s after sequence %d: %w", eventType, projection.LastSeq, err)
		}
		event, err := boot.Journal.Append(ctx, projection.LastSeq, draft)
		if err != nil {
			return err
		}
		if err := projection.Apply(event); err != nil {
			return err
		}
		if options.OnEvent != nil {
			options.OnEvent(event)
		}
		return nil
	}
	if err := appendEvent(domain.EventOperationReconciled, struct {
		Verdict              string `json:"verdict"`
		Actor                string `json:"actor"`
		Note                 string `json:"note"`
		Evidence             string `json:"evidence"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Verdict: options.Verdict, Actor: "human", Note: options.Note, Evidence: options.Evidence, WorkspaceObservation: observation}); err != nil {
		return RunResult{}, err
	}
	if options.Verdict == "abort_run" {
		if err := appendEvent(domain.EventTurnClosed, domain.TurnClosedPayload{Outcome: "failed"}); err != nil {
			return RunResult{}, err
		}
		if err := appendEvent(domain.EventRunFailed, struct {
			Error string `json:"error"`
		}{Error: "reconciliation aborted"}); err != nil {
			return RunResult{}, err
		}
		return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, "", nil)
	}
	if err := appendEvent(domain.EventToolResultAppended, tools.Result{OK: options.Verdict == "applied_success", Code: "reconciled_" + options.Verdict, Preview: options.Note}); err != nil {
		return RunResult{}, err
	}
	if err := appendEvent(domain.EventTurnClosed, domain.TurnClosedPayload{Outcome: "continue"}); err != nil {
		return RunResult{}, err
	}
	// 对账本身不执行任何副作用，重算观察值以绑定恢复时刻的真实工作区状态。
	observation, err = boot.Workspace.ObservationView(options.Config.Verification.VolatilePaths).Observation()
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	resumedDraft := domain.EventDraft{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, Type: domain.EventRunResumed, Payload: artifact.MustEncodeRedactedJSON(struct {
		Invocation           string `json:"invocation"`
		FromSeq              uint64 `json:"from_seq"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Invocation: domain.NewID("invoke"), FromSeq: projection.LastSeq, WorkspaceObservation: observation})}
	if err := projection.ValidateDraft(resumedDraft); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("validate run_resumed after sequence %d: %w", projection.LastSeq, err)
	}
	resumed, err := boot.Journal.Append(ctx, projection.LastSeq, resumedDraft)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := projection.Apply(resumed); err != nil {
		return RunResult{}, err
	}
	if options.OnEvent != nil {
		options.OnEvent(resumed)
	}
	// 回退 run 对账后仍由脚本化执行器续跑，禁止落入模型 Loop 产生计划外操作。
	if isRewind {
		return service.driveRewindRun(ctx, boot, options.RunID, rewindPlan.Runs, RewindOptions{Workspace: options.Workspace, StateDir: options.StateDir, Config: options.Config, OnEvent: options.OnEvent})
	}

	// 对账后继续模型续跑同样受异步子门禁约束：非终态子未收束前不续跑父运行。
	if err := rejectResumeWithUnfinishedAsyncChildren(options.Config.Tools.DelegateAsync, events); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	runContext, cancelRun, err := runBudgetContext(ctx, events[0].OccurredAt, options.Config)
	if err != nil {
		return RunResult{ID: options.RunID}, err
	}
	defer cancelRun()
	result, err := boot.Loop.Run(runContext, agent.RunInput{RunID: options.RunID, Task: projection.Task, WorkspaceHash: boot.WorkspaceHash})
	if wallBudgetExpired(runContext) && !runReachedTerminal(boot.Journal, options.RunID) {
		expiredContext, cancelExpired := expiredWallBudgetContext(ctx)
		defer cancelExpired()
		result, err = boot.Loop.Run(expiredContext, agent.RunInput{RunID: options.RunID, Task: projection.Task, WorkspaceHash: boot.WorkspaceHash})
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, result.Response, err)
}

// Abort 在不启动模型、工具或 verifier 的情况下终止非悬挂运行；已经开始的副作用
// 所具有的不确定性只能通过 Reconcile 的判定解决。除 running 外，它也是 created 与
// cancelling 运行的唯一恢复出口：进程在 run_created/run_started 之间或取消序列中途
// 崩溃后，工作区会被非终态 run 永久阻塞，此处按状态机允许的起点补写终态。
func (service Service) Abort(ctx context.Context, options AbortOptions) (outcome RunResult, err error) {
	if strings.TrimSpace(options.Reason) == "" {
		return RunResult{}, errors.New("abort reason is required")
	}
	boot, err := assembleRecoveryBoot(options.Workspace, options.StateDir, options.Config)
	if err != nil {
		return RunResult{}, err
	}
	defer func() { err = errors.Join(err, boot.Close()) }()
	events, err := boot.Journal.Load(ctx, options.RunID)
	if err != nil {
		return RunResult{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return RunResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	switch projection.State {
	case domain.RunCreated, domain.RunRunning, domain.RunCancelling:
	default:
		return RunResult{ID: options.RunID}, errors.New("run is not abortable")
	}
	if err := requireMatchingConfig(events, options.Config); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	if err := checkRunArtifactReferences(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkFrozenRunContract(events, options.StateDir, boot.WorkspaceHash); err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if projection.PendingOperation != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: use --resolve %s=abort_run", ErrWaitingReconcile, projection.PendingOperation.ID)
	}
	terminal := abortTerminal{
		eventType:         domain.EventRunFailed,
		turnOutcome:       "failed",
		approvalStopCause: "user_abort",
		payload: struct {
			Error  string `json:"error"`
			Reason string `json:"reason"`
		}{Error: "user_aborted", Reason: options.Reason},
		approvalReason: options.Reason,
	}
	if projection.Cancelling() {
		request, found, requestErr := latestAbortCancellationRequest(events)
		if requestErr != nil {
			return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, requestErr)
		}
		if !found {
			return RunResult{ID: options.RunID}, fmt.Errorf("%w: cancelling run has no persisted cancellation request", ErrStateCorrupt)
		}
		terminal = abortTerminalFromCancellation(request)
	}
	appendEvent := func(eventType domain.EventType, turnID *domain.TurnID, operationID *domain.OperationID, payload any) error {
		draft := domain.EventDraft{RunID: options.RunID, WorkspaceHash: boot.WorkspaceHash, Type: eventType, TurnID: turnID, OperationID: operationID, Payload: artifact.MustEncodeRedactedJSON(payload)}
		// 与 Reconcile 相同：先在克隆投影上模拟迁移再写入，防止不可回放事件落盘。
		if err := projection.ValidateDraft(draft); err != nil {
			return fmt.Errorf("validate %s after sequence %d: %w", eventType, projection.LastSeq, err)
		}
		event, err := boot.Journal.Append(ctx, projection.LastSeq, draft)
		if err != nil {
			return err
		}
		if err := projection.Apply(event); err != nil {
			return err
		}
		events = append(events, event)
		if options.OnEvent != nil {
			options.OnEvent(event)
		}
		return nil
	}
	repairs, err := agent.PendingUnstartedOperationRepairs(events)
	if err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	for _, repair := range repairs {
		if repair.Result.Code != "approval_denied" {
			continue
		}
		turnID, operationID := repair.TurnID, repair.OperationID
		if err := appendEvent(domain.EventOperationFailed, &turnID, &operationID, struct {
			ToolName string       `json:"tool_name"`
			Result   tools.Result `json:"result"`
		}{ToolName: repair.ToolName, Result: repair.Result}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	cancellation, found, err := agent.ResolvePendingApprovalCancellation(events, &projection, terminal.approvalStopCause)
	if err != nil {
		return RunResult{ID: options.RunID}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if found {
		turnID, operationID := cancellation.TurnID, cancellation.OperationID
		if !cancellation.ApprovalDenied {
			if err := appendEvent(domain.EventApprovalDenied, &turnID, &operationID, struct {
				Fingerprint string `json:"fingerprint"`
				Actor       string `json:"actor"`
				Reason      string `json:"reason"`
				StopCause   string `json:"stop_cause"`
			}{
				Fingerprint: cancellation.Fingerprint,
				Actor:       "system",
				Reason:      terminal.approvalReason,
				StopCause:   terminal.approvalStopCause,
			}); err != nil {
				return RunResult{ID: options.RunID}, err
			}
		}
		if !cancellation.OperationTerminal {
			if err := appendEvent(domain.EventOperationFailed, &turnID, &operationID, struct {
				ToolName string       `json:"tool_name"`
				Result   tools.Result `json:"result"`
			}{ToolName: cancellation.ToolName, Result: cancellation.Result}); err != nil {
				return RunResult{ID: options.RunID}, err
			}
		}
		if cancellation.VerificationID != "" && !cancellation.VerificationFinished {
			if err := appendEvent(domain.EventVerificationFinished, &turnID, nil, domain.VerificationFinishedPayload{
				VerificationID: cancellation.VerificationID,
				OK:             false,
			}); err != nil {
				return RunResult{ID: options.RunID}, err
			}
		}
	}
	if projection.PendingApproval != nil {
		return RunResult{ID: options.RunID}, errors.New("pending approval cancellation did not close the approval")
	}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := appendEvent(domain.EventTurnClosed, &turnID, nil, domain.TurnClosedPayload{Outcome: terminal.turnOutcome}); err != nil {
			return RunResult{ID: options.RunID}, err
		}
	}
	if err := appendEvent(terminal.eventType, nil, nil, terminal.payload); err != nil {
		return RunResult{ID: options.RunID}, err
	}
	// 父运行终止后向未终态子 run 合作式传播取消；已终态子跳过，失败不掩盖本 run 终态。
	if propagateErr := PropagateSubagentCancel(ctx, boot.Journal, boot.WorkspaceHash, options.RunID, time.Now().UTC()); propagateErr != nil {
		return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, "", errors.Join(nil, propagateErr))
	}
	return projectedRunOutcome(boot.Journal, options.StateDir, boot.WorkspaceHash, options.RunID, "", nil)
}

// abortCancellationRequest 是应用层恢复已持久化取消意图所需的最小投影。
type abortCancellationRequest struct {
	Cause            string `json:"cause"`
	ExpectedTerminal string `json:"expected_terminal,omitempty"`
	BudgetDimension  string `json:"budget_dimension,omitempty"`
}

// abortTerminal 描述 Abort 补写开放轮次与运行终态时使用的一致决定。
type abortTerminal struct {
	eventType         domain.EventType
	turnOutcome       string
	payload           any
	approvalReason    string
	approvalStopCause string
}

// latestAbortCancellationRequest 返回最近一次取消请求，并兼容未记录预期终态
// 的旧事件：预算原因或预算维度映射为 failed，其余原因映射为 cancelled。
func latestAbortCancellationRequest(events []domain.Event) (abortCancellationRequest, bool, error) {
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EventRunCancelRequested {
			continue
		}
		var request abortCancellationRequest
		if err := json.Unmarshal(events[index].Payload, &request); err != nil {
			return abortCancellationRequest{}, false, fmt.Errorf("decode run_cancel_requested payload: %w", err)
		}
		if request.ExpectedTerminal == "" {
			if request.Cause == "budget_exhausted" || request.BudgetDimension != "" {
				request.ExpectedTerminal = "failed"
			} else {
				request.ExpectedTerminal = "cancelled"
			}
		}
		switch request.ExpectedTerminal {
		case "failed":
			if request.Cause == "" {
				request.Cause = "budget_exhausted"
			}
		case "cancelled":
			if request.Cause == "" {
				request.Cause = "user_interrupt"
			}
		default:
			return abortCancellationRequest{}, false, fmt.Errorf("run_cancel_requested has invalid expected terminal %q", request.ExpectedTerminal)
		}
		return request, true, nil
	}
	return abortCancellationRequest{}, false, nil
}

// abortTerminalFromCancellation 把持久化取消决定转换为终态事件，保留预算
// 失败维度；错误文本复用 Agent 的稳定预算错误格式。
func abortTerminalFromCancellation(request abortCancellationRequest) abortTerminal {
	if request.ExpectedTerminal == "cancelled" {
		return abortTerminal{
			eventType: domain.EventRunCancelled, turnOutcome: "cancelled",
			payload: struct {
				Cause string `json:"cause"`
			}{Cause: request.Cause},
			approvalReason:    request.Cause,
			approvalStopCause: request.Cause,
		}
	}
	errorText := request.Cause
	if request.Cause == "budget_exhausted" {
		errorText = "budget exhausted"
		if request.BudgetDimension != "" {
			errorText = (&ports.BudgetExceededError{Dimension: request.BudgetDimension}).Error()
		}
	}
	return abortTerminal{
		eventType: domain.EventRunFailed, turnOutcome: "failed",
		payload: struct {
			Error           string `json:"error"`
			BudgetDimension string `json:"budget_dimension,omitempty"`
		}{Error: errorText, BudgetDimension: request.BudgetDimension},
		approvalReason:    request.Cause,
		approvalStopCause: request.Cause,
	}
}

// provider 使用注入构造器或默认 OpenAI-compatible 适配器创建模型端口。能力
// 卡声明的 usage 与 stop reason 合同随适配器下发，由 Provider 层在每次流结束
