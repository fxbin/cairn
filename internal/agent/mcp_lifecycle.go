// Package agent 在审批暂停、验证和运行停止前受审计地回收外部 MCP 会话。
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// quiesceMCP 只关闭已由本次 Boot 持有的资源；失败保持 started，不能先完成或验证运行。
// 清理不消耗模型调用额度，资源回收必须能够在业务预算耗尽后继续执行。
func (loop *Loop) quiesceMCP(ctx context.Context, projection *domain.RunProjection, input RunInput) error {
	if !loop.Tools.MCPNeedsQuiescence() {
		return nil
	}
	if projection.PendingOperation != nil {
		return &ports.WaitingReconcileError{RunID: input.RunID, OperationID: projection.PendingOperation.ID, Reason: "MCP lifecycle cleanup requires the pending operation to be reconciled first"}
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	createdTurn := projection.CurrentTurn == nil
	turnID := domain.NewTurnID()
	if createdTurn {
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID}); err != nil {
			return err
		}
	} else {
		turnID = *projection.CurrentTurn
	}
	operationID := domain.NewOperationID()
	call := ports.ToolCall{ID: "cairn-mcp-close-" + string(operationID), Name: tools.MCPQuiesceTool, Arguments: json.RawMessage(`{}`)}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: call.ID, ToolName: call.Name, Arguments: call.Arguments})}); err != nil {
		return err
	}
	decision, err := loop.Gate.Decide(call.Name, call.Arguments)
	if err != nil {
		return err
	}
	if decision.Effect != policy.Allow {
		return errors.New("MCP lifecycle cleanup must only release owned resources")
	}
	if err := loop.append(commitContext, projection, policyDecisionDraft(input, turnID, operationID, decision)); err != nil {
		return err
	}
	payload, err := loop.plannedOperationPayload(call)
	if err != nil {
		return err
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(payload)}); err != nil {
		return err
	}
	result := loop.Tools.Execute(context.WithoutCancel(ctx), call.Name, call.Arguments)
	resultContext, resultCancel := loop.commitContext(ctx)
	defer resultCancel()
	if !result.OK {
		return &ports.WaitingReconcileError{RunID: input.RunID, OperationID: operationID, Reason: "MCP session cleanup could not be confirmed"}
	}
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(loop.resultPayload(call.Name, result))}); err != nil {
		return err
	}
	if err := loop.appendToolResult(resultContext, projection, input, turnID, operationID, result); err != nil {
		return err
	}
	if createdTurn {
		return loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})})
	}
	return nil
}

// isMCPLifecyclePayload 区分资源回收与业务工具，预算和业务熔断不能把内部清理当作进展。
func isMCPLifecyclePayload(raw json.RawMessage) bool {
	var payload struct {
		ToolName string `json:"tool_name"`
	}
	return json.Unmarshal(raw, &payload) == nil && payload.ToolName == tools.MCPQuiesceTool
}

// mcpLifecycleOperations 从受审计提议恢复内部资源回收操作集合。
func mcpLifecycleOperations(events []domain.Event) map[domain.OperationID]bool {
	operations := make(map[domain.OperationID]bool)
	for _, event := range events {
		if event.Type == domain.EventToolCallProposed && event.OperationID != nil && isMCPLifecyclePayload(event.Payload) {
			operations[*event.OperationID] = true
		}
	}
	return operations
}

// mcpLifecycleOnlyTurns 标记只为回收资源开启的内部轮次，不能消耗模型轮次预算。
func mcpLifecycleOnlyTurns(events []domain.Event) map[domain.TurnID]bool {
	lifecycle := make(map[domain.TurnID]bool)
	business := make(map[domain.TurnID]bool)
	for _, event := range events {
		if event.TurnID == nil {
			continue
		}
		turnID := *event.TurnID
		switch event.Type {
		case domain.EventToolCallProposed:
			if isMCPLifecyclePayload(event.Payload) {
				lifecycle[turnID] = true
			} else {
				business[turnID] = true
			}
		case domain.EventModelRequestStarted, domain.EventModelResponseFinished, domain.EventVerificationStarted, domain.EventVerificationCommandProposed:
			business[turnID] = true
		}
	}
	for turnID := range business {
		delete(lifecycle, turnID)
	}
	return lifecycle
}
