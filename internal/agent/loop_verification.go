package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/tools"
)

const automaticVerificationResponse = "Cairn runtime: frozen verification passed after successful apply_patch."

// VerificationCommand 是 Loop 在完成前按配置执行的冻结命令。
type VerificationCommand struct {
	ID        string
	Arguments json.RawMessage
}

// verificationPlan 是持久化为制品并绑定审批的验证计划快照。
type verificationPlan struct {
	Version             string                `json:"version"`
	VerificationID      string                `json:"verification_id"`
	ConfigFingerprint   string                `json:"config_fingerprint"`
	LastMutationSeq     uint64                `json:"last_mutation_seq"`
	CommandsFingerprint string                `json:"commands_fingerprint"`
	Commands            []VerificationCommand `json:"commands"`
}

// verificationProposalPayload 是验证命令提议事件的可回放载荷。
type verificationProposalPayload struct {
	VerificationID string          `json:"verification_id"`
	CommandIndex   int             `json:"command_index"`
	PlanRef        *artifact.Ref   `json:"plan_ref,omitempty"`
	Arguments      json.RawMessage `json:"arguments"`
}

// runVerification 创建并执行冻结验证计划，任何失败都会反馈给后续模型轮次。
func (loop *Loop) runVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) error {
	if projection.LastMutationSeq != 0 && len(loop.Verification) == 0 {
		return errors.New("workspace changed but no verification commands are configured")
	}
	verificationID := domain.NewID("verify")
	plan, planRef, err := loop.createVerificationPlan(verificationID, projection.LastMutationSeq)
	if err != nil {
		return err
	}
	observation, err := loop.verificationObservation()
	if err != nil {
		return fmt.Errorf("observe workspace before verification plan: %w", err)
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(struct {
		VerificationID       string        `json:"verification_id"`
		MutationSeq          uint64        `json:"last_mutation_seq"`
		PlanRef              *artifact.Ref `json:"plan_ref,omitempty"`
		CommandsFingerprint  string        `json:"commands_fingerprint"`
		WorkspaceObservation string        `json:"workspace_observation"`
	}{VerificationID: verificationID, MutationSeq: projection.LastMutationSeq, PlanRef: planRef, CommandsFingerprint: plan.CommandsFingerprint, WorkspaceObservation: observation})}); err != nil {
		return err
	}
	if projection.LastMutationSeq == 0 {
		return loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: true})})
	}
	return loop.runVerificationCommands(ctx, projection, input, turnID, verificationID, planRef, 0)
}

// createVerificationPlan 冻结验证命令及配置指纹，并将计划作为脱敏制品保存。
func (loop *Loop) createVerificationPlan(verificationID string, lastMutationSeq uint64) (verificationPlan, *artifact.Ref, error) {
	commands, err := json.Marshal(loop.Verification)
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("encode verification commands: %w", err)
	}
	digest := sha256.Sum256(commands)
	plan := verificationPlan{
		Version:             "v1",
		VerificationID:      verificationID,
		ConfigFingerprint:   loop.ConfigFingerprint,
		LastMutationSeq:     lastMutationSeq,
		CommandsFingerprint: "sha256:" + hex.EncodeToString(digest[:]),
		Commands:            append([]VerificationCommand(nil), loop.Verification...),
	}
	if loop.Artifacts == nil {
		return plan, nil, nil
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("encode verification plan: %w", err)
	}
	ref, err := loop.Artifacts.Put(encoded, "application/json")
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("store verification plan: %w", err)
	}
	return plan, &ref, nil
}

// runVerificationCommands 从指定索引顺序执行冻结计划中的验证命令。
func (loop *Loop) runVerificationCommands(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, verificationID string, planRef *artifact.Ref, startIndex int) error {
	for index := startIndex; index < len(loop.Verification); index++ {
		if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
			return err
		}
		command := loop.Verification[index]
		operationID := domain.NewOperationID()
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationCommandProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(verificationProposalPayload{VerificationID: verificationID, CommandIndex: index, PlanRef: planRef, Arguments: command.Arguments})}); err != nil {
			return err
		}
		planSHA := ""
		if planRef != nil {
			planSHA = planRef.SHA256
		}
		observation, err := loop.verificationObservation()
		if err != nil {
			return fmt.Errorf("observe workspace for verification approval: %w", err)
		}
		decision, err := loop.Gate.DecideVerificationWithObservation(command.Arguments, planSHA, index, loop.ConfigFingerprint, observation)
		if err != nil {
			return err
		}
		decision = freshApprovalDecision(*projection, decision)
		if err := loop.append(ctx, projection, policyDecisionDraft(input, turnID, operationID, decision)); err != nil {
			return err
		}
		if decision.Effect == policy.Ask {
			if err := loop.append(ctx, projection, approvalRequestedDraft(input, turnID, operationID, decision)); err != nil {
				return err
			}
			return &WaitingApprovalError{RunID: input.RunID, OperationID: operationID, Fingerprint: decision.Fingerprint, Summary: decision.Summary}
		}
		if decision.Effect == policy.Deny {
			return &PolicyDeniedError{Summary: decision.Summary}
		}
		if err := loop.executeVerificationCommand(ctx, projection, input, turnID, operationID, command, observation); err != nil {
			return err
		}
	}
	return loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: true})})
}

// policyDecisionDraft 将策略决定编码为可追加的领域事件草稿。
func policyDecisionDraft(input RunInput, turnID domain.TurnID, operationID domain.OperationID, decision policy.Decision) domain.EventDraft {
	return domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
		Summary     string `json:"summary"`
	}{Effect: string(decision.Effect), Rule: decision.RuleID, Fingerprint: decision.Fingerprint, Summary: decision.Summary})}
}

// approvalRequestedDraft 将待审批操作编码为可追加的领域事件草稿。
func approvalRequestedDraft(input RunInput, turnID domain.TurnID, operationID domain.OperationID, decision policy.Decision) domain.EventDraft {
	return domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalRequested, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Summary     string `json:"summary"`
		Rule        string `json:"rule"`
	}{Fingerprint: decision.Fingerprint, Summary: decision.Summary, Rule: decision.RuleID})}
}

// executeVerificationCommand 在策略授权后执行单条冻结验证命令并记录证据。
// beforeObservation 是调用方在审批决定时已计算的同一 verifier 视图观察值；
// 两次计算之间只发生事件追加（工作区已加锁且无工具执行），因此可安全复用，
// 避免同一无变更窗口内的重复全树扫描。
func (loop *Loop) executeVerificationCommand(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, operationID domain.OperationID, command VerificationCommand, beforeObservation string) error {
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		return err
	}
	if loop.Gate == nil {
		return errors.New("policy gate is required for verification")
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "exec_command", SideEffectClass: string(tools.SideEffectProcess), MutationBarrier: false})}); err != nil {
		return err
	}
	result := redactToolResult(loop.Tools.Execute(ctx, "exec_command", command.Arguments))
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
	if result.OK {
		afterObservation, observationErr := loop.verificationObservation()
		if observationErr != nil {
			return fmt.Errorf("observe workspace after verification: %w", observationErr)
		}
		if afterObservation != beforeObservation {
			result = tools.Failure("verification_mutated_workspace", "verification command modified the tool-visible workspace; repair or rerun verification before completion")
			eventType = domain.EventOperationFailed
		}
	}
	payload := loop.resultPayload("exec_command", result)
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(payload)}); err != nil {
		return err
	}
	if cancelled {
		return ctx.Err()
	}
	if result.OK {
		return nil
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: false, ResultFingerprint: stableToolResultFingerprint(result)})}); err != nil {
		return err
	}
	feedback := fmt.Sprintf("Verification command %s failed (%s). Inspect the result, repair the workspace, and run the configured verification again. Evidence: %s", command.ID, result.Code, result.Preview)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFeedbackAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Text string `json:"text"`
	}{Text: feedback})}); err != nil {
		return err
	}
	if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
		return err
	}
	return &VerificationFailureError{CommandID: command.ID, Feedback: feedback}
}

// verificationObservation 返回 verifier 视图的稳定摘要，用于绑定命令审批。
func (loop *Loop) verificationObservation() (string, error) {
	if loop.VerificationWorkspace.Root != "" {
		return loop.VerificationWorkspace.Observation()
	}
	if loop.Gate == nil {
		return "", errors.New("policy gate is required for workspace observation")
	}
	return loop.Gate.Patch.Workspace.Observation()
}

// finishAutomaticVerification 在同一模型响应的所有工具调用成功结束且有补丁写入后，
// 执行冻结 verifier；通过时直接完成，失败时保留既有证据并返回给下一模型轮次。
func (loop *Loop) finishAutomaticVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) (Result, string, error) {
	if err := loop.runVerification(ctx, projection, input, turnID); err != nil {
		if ctx.Err() != nil {
			result, cancellationErr := loop.persistCancellation(ctx, projection, input, &turnID)
			return result, "", cancellationErr
		}
		var waiting *WaitingApprovalError
		if errors.As(err, &waiting) {
			return Result{RunID: input.RunID}, "", waiting
		}
		var budget *BudgetExceededError
		if errors.As(err, &budget) {
			result, budgetErr := loop.persistBudgetExceeded(ctx, projection, input, budget.Dimension)
			return result, "", budgetErr
		}
		var guard *LoopGuardExceededError
		if errors.As(err, &guard) {
			return Result{RunID: input.RunID}, "", loop.persistLoopGuardExceeded(ctx, projection, input, guard.Reason)
		}
		var verificationFailure *VerificationFailureError
		if errors.As(err, &verificationFailure) {
			return Result{RunID: input.RunID}, verificationFailure.Feedback, nil
		}
		result, failureErr := loop.failCurrentTurn(ctx, projection, input, turnID, err)
		return result, "", failureErr
	}
	result, completionErr := loop.completeAutomaticVerification(ctx, projection, input, turnID)
	return result, "", completionErr
}

// completeAutomaticVerification 以明确标记的运行时结论关闭已通过自动验证的运行，
// 不将空工具调用响应伪装为模型最终文本。
func (loop *Loop) completeAutomaticVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) (Result, error) {
	response := loop.completionResponse(*projection)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: response})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	return Result{RunID: input.RunID, Response: response}, nil
}

// completionResponse 返回已持久化的模型最终消息；自动验证没有模型最终文本时，
// 返回可辨识的 Cairn 运行时结论。
func (loop *Loop) completionResponse(projection domain.RunProjection) string {
	if strings.TrimSpace(projection.LastAssistantMessage) != "" {
		return projection.LastAssistantMessage
	}
	return automaticVerificationResponse
}
