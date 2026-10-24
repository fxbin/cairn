// 中断操作/缺失终态的确定性修复。
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
	"github.com/fxbin/cairn/internal/tools"
)

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

// repairRecoverableOperationGaps 仅依据既有 Journal 事实补齐普通操作的确定性
// 终态与模型可见结果；每阶段追加后重新加载并回放，绝不重新执行工具。
func (loop *Loop) repairRecoverableOperationGaps(ctx context.Context, input RunInput, events []domain.Event, projection domain.RunProjection) ([]domain.Event, domain.RunProjection, error) {
	operationRepaired, err := loop.repairUnstartedOperationTerminals(ctx, &projection, input, events)
	if err != nil {
		return nil, domain.RunProjection{}, err
	}
	if operationRepaired {
		events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if err != nil {
			return nil, domain.RunProjection{}, fmt.Errorf("reload run after repairing unstarted operations: %w", err)
		}
		projection, err = domain.Replay(events)
		if err != nil {
			return nil, domain.RunProjection{}, fmt.Errorf("replay run after repairing unstarted operations: %w", err)
		}
	}
	resultRepaired, err := loop.repairMissingToolResults(ctx, &projection, input, events)
	if err != nil {
		return nil, domain.RunProjection{}, err
	}
	if !resultRepaired {
		return events, projection, nil
	}
	events, err = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return nil, domain.RunProjection{}, fmt.Errorf("reload run after repairing tool results: %w", err)
	}
	projection, err = domain.Replay(events)
	if err != nil {
		return nil, domain.RunProjection{}, fmt.Errorf("replay run after repairing tool results: %w", err)
	}
	return events, projection, nil
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
