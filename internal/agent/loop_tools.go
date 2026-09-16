package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// toolCallPayload 是写入工具提议事件的模型调用事实。
type toolCallPayload struct {
	CallID    string          `json:"call_id"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
}

// operationPayload 是工具操作开始或终态事件的可回放证据字段。
type operationPayload struct {
	ToolName           string        `json:"tool_name"`
	SideEffectClass    string        `json:"side_effect_class,omitempty"`
	MutationBarrier    bool          `json:"mutation_barrier,omitempty"`
	PlannedPath        string        `json:"planned_path,omitempty"`
	PlannedBefore      string        `json:"planned_before,omitempty"`
	PlannedBeforeRef   *artifact.Ref `json:"planned_before_ref,omitempty"`
	PlannedAfter       string        `json:"planned_after,omitempty"`
	Result             any           `json:"result,omitempty"`
	ResultRef          *artifact.Ref `json:"result_ref,omitempty"`
	ResultStorageError string        `json:"result_storage_error,omitempty"`
}

// toolResultPersistenceError 表示操作终态已经持久化，但模型可见的 Tool Result
// 尚未写入；调用方必须保留运行现场，交由 Resume 从终态证据确定性补齐。
type toolResultPersistenceError struct {
	operationID domain.OperationID
	cause       error
}

// Error 返回不包含工具原始输出的稳定恢复提示。
func (err *toolResultPersistenceError) Error() string {
	return fmt.Sprintf("persist tool result for operation %s: %v", err.operationID, err.cause)
}

// Unwrap 暴露底层存储错误，便于调用方保留原始失败分类。
func (err *toolResultPersistenceError) Unwrap() error {
	return err.cause
}

// Is 让尚未适配专用错误的执行入口停在可恢复边界，而不是终态化运行。
func (err *toolResultPersistenceError) Is(target error) bool {
	return target == ErrWaitingReconcile
}

// resumeDecision 为已提议操作重建策略判定，并保留验证计划的独立审批边界。
// verificationObservation 是调用方刚计算的 verifier 视图观察值（仅验证提议使用），
// 复用它避免同一无变更窗口内的重复全树扫描。
func (loop *Loop) resumeDecision(projection domain.RunProjection, call ports.ToolCall, verification verificationProposalPayload, verificationProposal bool, verificationObservation string) (policy.Decision, error) {
	if verificationProposal {
		planSHA := ""
		if verification.PlanRef != nil {
			planSHA = verification.PlanRef.SHA256
		}
		decision, err := loop.Gate.DecideVerificationWithObservation(verification.Arguments, planSHA, verification.CommandIndex, loop.ConfigFingerprint, verificationObservation)
		if err != nil {
			return policy.Decision{}, err
		}
		return freshApprovalDecision(projection, decision), nil
	}
	decision, err := loop.Gate.Decide(call.Name, call.Arguments)
	if err != nil {
		return policy.Decision{}, err
	}
	return freshApprovalDecision(projection, decision), nil
}

// proposedToolCall 从事件历史恢复某操作对应的工具调用。
func proposedToolCall(events []domain.Event, operationID domain.OperationID) (ports.ToolCall, bool) {
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed || event.OperationID == nil || *event.OperationID != operationID {
			continue
		}
		var payload toolCallPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return ports.ToolCall{}, false
		}
		return ports.ToolCall{ID: payload.CallID, Name: payload.ToolName, Arguments: payload.Arguments}, payload.CallID != "" && payload.ToolName != "" && json.Valid(payload.Arguments)
	}
	return ports.ToolCall{}, false
}

// executeTool 在策略、审批和日志授权后执行一次模型提议的固定工具调用。
func (loop *Loop) executeTool(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, call ports.ToolCall) (tools.Result, error) {
	if call.ID == "" || call.Name == "" || !json.Valid(call.Arguments) {
		return tools.Result{}, errors.New("model returned invalid tool call")
	}
	if containsSensitiveToolCall(call) {
		return tools.Result{}, errors.New("model tool arguments contain a secret-like value")
	}
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		return tools.Result{}, err
	}
	operationID := domain.NewOperationID()
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: call.ID, ToolName: call.Name, Arguments: call.Arguments})}); err != nil {
		return tools.Result{}, err
	}
	if loop.Gate == nil {
		return tools.Result{}, errors.New("policy gate is required")
	}
	decision, err := loop.Gate.Decide(call.Name, call.Arguments)
	if err != nil {
		return tools.Result{}, err
	}
	decision = freshApprovalDecision(*projection, decision)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
		Summary     string `json:"summary"`
	}{Effect: string(decision.Effect), Rule: decision.RuleID, Fingerprint: decision.Fingerprint, Summary: decision.Summary})}); err != nil {
		return tools.Result{}, err
	}
	if decision.Effect == policy.Ask {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalRequested, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
			Fingerprint string `json:"fingerprint"`
			Summary     string `json:"summary"`
			Rule        string `json:"rule"`
		}{Fingerprint: decision.Fingerprint, Summary: decision.Summary, Rule: decision.RuleID})}); err != nil {
			return tools.Result{}, err
		}
		return tools.Result{}, &WaitingApprovalError{RunID: input.RunID, OperationID: operationID, Fingerprint: decision.Fingerprint, Summary: decision.Summary}
	}
	if decision.Effect == policy.Deny {
		failureCode := decision.FailureCode
		if failureCode == "" {
			failureCode = "policy_denied"
		}
		result := tools.Failure(failureCode, decision.Summary)
		if len(decision.FailureData) != 0 {
			result.Data = append(json.RawMessage(nil), decision.FailureData...)
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
			return tools.Result{}, err
		}
		// 策略拒绝不会执行副作用，但模型仍应得到可审计的失败结果，以便在剩余轮次中
		// 修正参数或改用允许的方案；人工审批拒绝与验证计划拒绝仍保持终态语义。
		if err := loop.appendToolResult(ctx, projection, input, turnID, operationID, result); err != nil {
			return tools.Result{}, err
		}
		if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
			return result, err
		}
		return result, nil
	}
	startedPayload, err := loop.plannedOperationPayload(call)
	if err != nil {
		return tools.Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(startedPayload)}); err != nil {
		return tools.Result{}, err
	}
	result := loop.Tools.Execute(ctx, call.Name, call.Arguments)
	result = redactToolResult(result)
	if result.Code == tools.CodeTerminationUnconfirmed {
		return result, &WaitingReconcileError{RunID: input.RunID, OperationID: operationID, Reason: result.Preview}
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
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(payload)}); err != nil {
		return tools.Result{}, err
	}
	if err := loop.appendToolResult(ctx, projection, input, turnID, operationID, result); err != nil {
		return tools.Result{}, err
	}
	if cancelled {
		return result, ctx.Err()
	}
	if err := loop.currentOperationFailureGuard(ctx, input.RunID); err != nil {
		return result, err
	}
	return result, nil
}

// appendToolResult 使用不受调用方取消影响的提交上下文写入模型可见结果；若
// Journal 写入失败，返回专用可恢复错误，禁止上层关闭 Turn 或终态化 Run。
func (loop *Loop) appendToolResult(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, operationID domain.OperationID, result tools.Result) error {
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if err := loop.append(commitContext, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventToolResultAppended,
		TurnID:        &turnID,
		OperationID:   &operationID,
		Payload:       mustJSON(result),
	}); err != nil {
		return &toolResultPersistenceError{operationID: operationID, cause: err}
	}
	return nil
}

// mustJSON 将事件载荷序列化并脱敏；仅编程错误会触发 panic。
func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("encode event payload: %w", err))
	}
	return append(json.RawMessage(nil), artifact.RedactJSON(encoded)...)
}

// containsSecretJSON 通过比较脱敏前后 JSON 判断载荷是否含有敏感内容。
func containsSecretJSON(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return !bytes.Equal(canonical, artifact.RedactJSON(canonical))
}

// freshApprovalDecision 为曾被不确定副作用对账过的同一动作强制重新人工审批。
func freshApprovalDecision(projection domain.RunProjection, decision policy.Decision) policy.Decision {
	if decision.Effect == policy.Allow && projection.RequiresFreshApproval(decision.Fingerprint) {
		decision.Effect = policy.Ask
		decision.RuleID = "reconciled_action_requires_approval"
		decision.Summary = "the same action was previously reconciled after an uncertain side effect; explicit approval is required"
	}
	return decision
}

// plannedOperationPayload 为已授权的工具调用构造 operation_started 载荷：
// 标注副作用类别，apply_patch 额外携带确定性改写计划（相对路径与前后摘要），
// 并在改写现文件前生成可回退的 before 快照引用。
func (loop *Loop) plannedOperationPayload(call ports.ToolCall) (operationPayload, error) {
	sideEffect := loop.Tools.SideEffect(call.Name)
	payload := operationPayload{ToolName: call.Name, SideEffectClass: string(sideEffect), MutationBarrier: sideEffect == tools.SideEffectProcess}
	if call.Name != "apply_patch" {
		return payload, nil
	}
	plan, err := loop.Gate.Patch.Plan(call.Arguments)
	if err != nil {
		return operationPayload{}, err
	}
	payload.PlannedPath, payload.PlannedBefore, payload.PlannedAfter = plan.Path, plan.BeforeHash, plan.AfterHash
	payload.PlannedBeforeRef = loop.snapshotPatchBefore(plan)
	return payload, nil
}

// snapshotPatchBefore 在 update/delete 改写现文件前，把旧内容制品化为回退
// 快照。制品库会先脱敏再存储：只有写入后的内容哈希仍等于原文件摘要（无损）
// 才返回引用；脱敏改写了内容或制品库不可用时返回 nil，该次改写将如实不可
// 回退，绝不以被改写的副本冒充快照。create 无旧内容，恒返回 nil。
func (loop *Loop) snapshotPatchBefore(plan tools.PatchPlan) *artifact.Ref {
	if (plan.Mode != "update" && plan.Mode != "delete") || !plan.Exists || loop.Artifacts == nil {
		return nil
	}
	ref, err := loop.Artifacts.Put(plan.Before, "application/octet-stream")
	if err != nil || ref.SHA256 != plan.BeforeHash {
		return nil
	}
	return &ref
}

// resultPayload 将工具结果包装为事件载荷，并尽力持久化脱敏制品引用。
// 即使制品盘暂不可用，结果本身仍会写入事件以关闭已启动的操作，避免产生不可回放日志。
func (loop *Loop) resultPayload(toolName string, result tools.Result) operationPayload {
	payload := operationPayload{ToolName: toolName, Result: result}
	if loop.Artifacts == nil {
		return payload
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		payload.ResultStorageError = "encode tool result artifact failed"
		return payload
	}
	ref, err := loop.Artifacts.Put(encoded, "application/json")
	if err != nil {
		payload.ResultStorageError = "store tool result artifact failed"
		return payload
	}
	payload.ResultRef = &ref
	return payload
}

// redactToolResult 清除工具结果预览和 JSON 数据中可能泄露的凭据。
func redactToolResult(result tools.Result) tools.Result {
	result.Preview = string(artifact.Redact([]byte(result.Preview)))
	if len(result.Data) != 0 {
		result.Data = append(json.RawMessage(nil), artifact.RedactJSON(result.Data)...)
	}
	return result
}
