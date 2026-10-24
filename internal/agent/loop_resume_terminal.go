// 缺失 Run 终态修复与失败证据装配。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
)

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
			cause = &ports.PolicyDeniedError{Summary: evidence.failure.Reason}
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
		if err := loop.requireCompletedVerificationObservation(events, closed.turnID, projection); err != nil {
			return Result{RunID: input.RunID}, true, err
		}
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
					Error:      (&ports.PolicyDeniedError{Summary: payload.Summary}).Error(),
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
					failure.Error = (&ports.BudgetExceededError{Dimension: payload.Dimension}).Error()
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
