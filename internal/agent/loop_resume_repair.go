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
