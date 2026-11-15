// [INPUT]: 固定释放提议、Journal、授权与工具端口。
// [OUTPUT]: 提供安全点释放、历史分类和不完整接线拒绝。
// [POS]: Core 保留通用审计链，释放名称由能力显式声明。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package agent 在安全点按显式提议受审计地释放本次运行资源。
// Author: fxbin
package agent

import (
	"context"
	"errors"

	"encoding/json"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// LifecycleParticipant 仅声明本次运行资源的释放需求和固定提议；查询不得执行外部动作。
// QuiescenceOperation 在实例生命周期内保持不变，执行由 Core 的审计工具链完成。
type LifecycleParticipant interface {
	NeedsQuiescence() bool
	QuiescenceOperation() contract.LifecycleOperation
}

// validateLifecycleParticipants 在模型和工具执行前拒绝不完整或重复接线。
func (loop *Loop) validateLifecycleParticipants() error {
	owned := make(map[string]bool, len(loop.LifecycleParticipants))
	for _, participant := range loop.LifecycleParticipants {
		if participant == nil {
			return errors.New("lifecycle participant is nil")
		}
		plan := participant.QuiescenceOperation()
		if plan.ToolName == "" || plan.CallIDPrefix == "" || !json.Valid(plan.Arguments) || plan.PendingOperationReason == "" || plan.DeniedReason == "" || plan.UnconfirmedReason == "" {
			return errors.New("lifecycle participant has an invalid release proposal")
		}
		if owned[plan.ToolName] {
			return errors.New("lifecycle tool has multiple owners")
		}
		owned[plan.ToolName] = true
		if loop.Tools == nil {
			return errors.New("lifecycle participant requires a tool registry")
		}
		if effect := loop.Tools.SideEffect(plan.ToolName); effect != contract.SideEffectWorkspace && effect != contract.SideEffectProcess {
			return errors.New("lifecycle tool must declare a recoverable side effect")
		}
		for _, spec := range loop.Tools.Specs() {
			if spec.Name == plan.ToolName {
				return errors.New("lifecycle tool must not be model-visible")
			}
		}
		observer, err := loop.resultObserver(plan.ToolName)
		if err != nil {
			return err
		}
		if observer == nil {
			return errors.New("lifecycle tool requires a result observer")
		}
	}
	if required, ok := loop.Tools.(interface{ RequiredLifecycleTools() []string }); ok {
		for _, name := range required.RequiredLifecycleTools() {
			if !owned[name] {
				return errors.New("tool registry requires an explicitly configured lifecycle participant")
			}
		}
	}
	return nil
}

// resourcesNeedQuiescence 仅查询本次已装配资源的占用状态，不连接或释放资源。
func (loop *Loop) resourcesNeedQuiescence() bool {
	for _, participant := range loop.LifecycleParticipants {
		if participant != nil && participant.NeedsQuiescence() {
			return true
		}
	}
	return false
}

// quiesceResources 按装配顺序释放资源；预算耗尽后仍执行清理，失败保持 started 待对账。
func (loop *Loop) quiesceResources(ctx context.Context, projection *domain.RunProjection, input RunInput) error {
	if err := loop.validateLifecycleParticipants(); err != nil {
		return err
	}
	for _, participant := range loop.LifecycleParticipants {
		if !participant.NeedsQuiescence() {
			continue
		}
		if err := loop.quiesceParticipant(ctx, projection, input, participant.QuiescenceOperation()); err != nil {
			return err
		}
	}
	return nil
}

// quiesceParticipant 复用提议、策略、started 与双回执链；仅允许释放自身资源。
func (loop *Loop) quiesceParticipant(ctx context.Context, projection *domain.RunProjection, input RunInput, plan contract.LifecycleOperation) error {
	if projection.PendingOperation != nil {
		return &ports.WaitingReconcileError{RunID: input.RunID, OperationID: projection.PendingOperation.ID, Reason: plan.PendingOperationReason}
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
	call := ports.ToolCall{ID: plan.CallIDPrefix + string(operationID), Name: plan.ToolName, Arguments: plan.Arguments}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: call.ID, ToolName: call.Name, Arguments: call.Arguments})}); err != nil {
		return err
	}
	decision, err := loop.Gate.Decide(call.Name, call.Arguments)
	if err != nil {
		return err
	}
	if decision.Effect != AuthorizationAllow {
		return errors.New(plan.DeniedReason)
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
		return &ports.WaitingReconcileError{RunID: input.RunID, OperationID: operationID, Reason: plan.UnconfirmedReason}
	}
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(loop.resultPayload(call.Name, result))}); err != nil {
		return err
	}
	if err := loop.appendToolResult(resultContext, projection, input, turnID, operationID, result); err != nil {
		return err
	}
	if err := loop.observeToolResult(call.Name, result); err != nil {
		return err
	}
	if createdTurn {
		return loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})})
	}
	return nil
}

// isLifecyclePayload 区分资源回收与业务工具，预算和业务熔断不能把内部清理当作进展。
func (loop *Loop) isLifecyclePayload(raw json.RawMessage) bool {
	var payload struct {
		ToolName string `json:"tool_name"`
	}
	return json.Unmarshal(raw, &payload) == nil && loop.isLifecycleTool(payload.ToolName)
}

// lifecycleOperations 从受审计提议恢复内部资源回收操作集合。
func (loop *Loop) lifecycleOperations(events []domain.Event) map[domain.OperationID]bool {
	operations := make(map[domain.OperationID]bool)
	for _, event := range events {
		if event.Type == domain.EventToolCallProposed && event.OperationID != nil && loop.isLifecyclePayload(event.Payload) {
			operations[*event.OperationID] = true
		}
	}
	return operations
}

// lifecycleOnlyTurns 标记只为回收资源开启的内部轮次，不能消耗模型轮次预算。
func (loop *Loop) lifecycleOnlyTurns(events []domain.Event) map[domain.TurnID]bool {
	lifecycle := make(map[domain.TurnID]bool)
	business := make(map[domain.TurnID]bool)
	for _, event := range events {
		if event.TurnID == nil {
			continue
		}
		turnID := *event.TurnID
		switch event.Type {
		case domain.EventToolCallProposed:
			if loop.isLifecyclePayload(event.Payload) {
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

// isLifecycleTool 只按显式固定释放提议识别内部工具，不解释能力名称。
func (loop *Loop) isLifecycleTool(name string) bool {
	for _, participant := range loop.LifecycleParticipants {
		if participant != nil && participant.QuiescenceOperation().ToolName == name {
			return true
		}
	}
	return false
}
