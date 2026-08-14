package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

// RunState 表示一次 Agent 运行在回放状态机中的生命周期阶段。
type RunState string

const (
	RunCreated    RunState = "created"
	RunRunning    RunState = "running"
	RunCancelling RunState = "cancelling"
	RunCompleted  RunState = "completed"
	RunFailed     RunState = "failed"
	RunCancelled  RunState = "cancelled"
)

// RunProjection 是由事件序列重放得到的当前运行状态与恢复依据。
type RunProjection struct {
	ID                      RunID
	Task                    string
	Workspace               string
	WorkspaceHash           string
	WorkspaceObservation    string
	State                   RunState
	LastSeq                 uint64
	CurrentTurn             *TurnID
	LastAssistantMessage    string
	VerificationSucceeded   bool
	LastVerificationSeq     uint64
	LastMutationSeq         uint64
	LastContextCompactSeq   uint64
	ContextCompactedThrough uint64
	PendingApproval         *PendingApproval
	PendingOperation        *PendingOperation
	operationFingerprints   map[OperationID]string
	reconciledFingerprints  map[string]struct{}
	operations              map[OperationID]*operationRecord
}

// ValidateDraft 在不改变当前投影的前提下校验一条待追加事件的 payload 与状态迁移。
// 它让调用方能够在写入不可变 Journal 之前拒绝非法事件，避免留下不可回放的日志。
func (projection RunProjection) ValidateDraft(draft EventDraft) error {
	event, err := NewEvent(draft, projection.LastSeq+1, GenesisHash)
	if err != nil {
		return err
	}
	candidate := projection.clone()
	return candidate.Apply(event)
}

// clone 复制投影及其内部可变映射，供写前校验模拟状态迁移使用。
func (projection RunProjection) clone() RunProjection {
	copyTurn := func(source *TurnID) *TurnID {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	copyApproval := func(source *PendingApproval) *PendingApproval {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	copyOperation := func(source *PendingOperation) *PendingOperation {
		if source == nil {
			return nil
		}
		value := *source
		return &value
	}
	candidate := projection
	candidate.CurrentTurn = copyTurn(projection.CurrentTurn)
	candidate.PendingApproval = copyApproval(projection.PendingApproval)
	candidate.PendingOperation = copyOperation(projection.PendingOperation)
	if projection.operationFingerprints != nil {
		candidate.operationFingerprints = make(map[OperationID]string, len(projection.operationFingerprints))
		for id, fingerprint := range projection.operationFingerprints {
			candidate.operationFingerprints[id] = fingerprint
		}
	}
	if projection.reconciledFingerprints != nil {
		candidate.reconciledFingerprints = make(map[string]struct{}, len(projection.reconciledFingerprints))
		for fingerprint := range projection.reconciledFingerprints {
			candidate.reconciledFingerprints[fingerprint] = struct{}{}
		}
	}
	if projection.operations != nil {
		candidate.operations = make(map[OperationID]*operationRecord, len(projection.operations))
		for id, record := range projection.operations {
			copyRecord := *record
			candidate.operations[id] = &copyRecord
		}
	}
	return candidate
}

// operationState 记录单个工具操作在投议、授权与终态之间的内部迁移。
type operationState string

const (
	operationProposed        operationState = "proposed"
	operationAwaitingApprove operationState = "awaiting_approval"
	operationApprovalGranted operationState = "approval_granted"
	operationAuthorized      operationState = "authorized"
	operationDenied          operationState = "denied"
	operationStarted         operationState = "started"
	operationTerminal        operationState = "terminal"
)

// operationRecord 记录回放时验证操作合法性所需的最小内部事实。
type operationRecord struct {
	state              operationState
	toolResultAppended bool
	turnID             TurnID
	requiresToolResult bool
}

// PendingApproval 描述等待人工确认的副作用操作。
type PendingApproval struct {
	OperationID OperationID
	Fingerprint string
	Summary     string
}

// PendingOperation 描述已启动但尚未持久化终态的外部操作。
type PendingOperation struct {
	ID              OperationID
	ToolName        string
	SideEffectClass string
	MutationBarrier bool
	PlannedPath     string
	PlannedBefore   string
	PlannedAfter    string
}

// RunCreatedPayload 是 run_created 事件的可回放业务字段。
type RunCreatedPayload struct {
	Task      string `json:"task"`
	Workspace string `json:"workspace"`
}

// TurnClosedPayload 表示一次推理轮次的关闭结果。
type TurnClosedPayload struct {
	Outcome             string `json:"outcome"`
	OperationSignature  string `json:"operation_signature,omitempty"`
	ProgressFingerprint string `json:"progress_fingerprint,omitempty"`
}

// AssistantMessagePayload 保存模型输出给用户的文本。
type AssistantMessagePayload struct {
	Text string `json:"text"`
}

// VerificationFinishedPayload 记录一次验证是否通过。
type VerificationFinishedPayload struct {
	OK                bool   `json:"ok"`
	ResultFingerprint string `json:"result_fingerprint,omitempty"`
}

// ContextCompactedPayload 保存一次可重放的对话检查点及其压缩范围。
type ContextCompactedPayload struct {
	ThroughSeq            uint64 `json:"through_seq"`
	Summary               string `json:"summary"`
	EstimatedTokensBefore int    `json:"estimated_units_before"`
	EstimatedTokensAfter  int    `json:"estimated_units_after"`
	Strategy              string `json:"strategy"`
}

// RunCompletedPayload 保存已完成运行的最终回复。
type RunCompletedPayload struct {
	FinalResponse string `json:"final_response"`
}

// Replay 先校验哈希链，再按顺序应用事件得到运行投影。
func Replay(events []Event) (RunProjection, error) {
	if err := VerifyChain(events); err != nil {
		return RunProjection{}, err
	}
	var projection RunProjection
	for _, event := range events {
		if err := projection.Apply(event); err != nil {
			return RunProjection{}, err
		}
	}
	return projection, nil
}

// Apply 将单个合法事件应用到投影，并拒绝非法状态迁移。
func (projection *RunProjection) Apply(event Event) error {
	if event.Seq != projection.LastSeq+1 {
		return fmt.Errorf("apply event sequence %d after %d", event.Seq, projection.LastSeq)
	}
	if projection.ID != "" && event.RunID != projection.ID {
		return fmt.Errorf("event run id %q does not match projection run id %q", event.RunID, projection.ID)
	}
	switch event.Type {
	case EventRunCreated:
		if projection.ID != "" || event.Seq != 1 {
			return fmt.Errorf("run_created must be the first event")
		}
		var payload RunCreatedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode run_created payload: %w", err)
		}
		projection.ID = event.RunID
		projection.Task = payload.Task
		projection.Workspace = payload.Workspace
		projection.WorkspaceHash = event.WorkspaceHash
		projection.State = RunCreated
	case EventRunStarted:
		if projection.State != RunCreated {
			return fmt.Errorf("run_started from %s", projection.State)
		}
		projection.State = RunRunning
	case EventRunResumed:
		if projection.State != RunRunning || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("run_resumed from %s", projection.State)
		}
		var payload struct {
			Invocation           string `json:"invocation"`
			FromSeq              uint64 `json:"from_seq"`
			WorkspaceObservation string `json:"workspace_observation"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode run_resumed payload: %w", err)
		}
		if payload.Invocation == "" || payload.WorkspaceObservation == "" || payload.FromSeq >= event.Seq {
			return errors.New("run_resumed payload is incomplete")
		}
		projection.WorkspaceObservation = payload.WorkspaceObservation
	case EventTurnOpened:
		if projection.State != RunRunning || event.TurnID == nil || projection.CurrentTurn != nil {
			return fmt.Errorf("turn_opened is not legal for run state %s", projection.State)
		}
		turnID := *event.TurnID
		projection.CurrentTurn = &turnID
	case EventTurnClosed:
		if (projection.State != RunRunning && projection.State != RunCancelling) || event.TurnID == nil || projection.CurrentTurn == nil || *event.TurnID != *projection.CurrentTurn || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("turn_closed does not match current turn")
		}
		var payload TurnClosedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode turn_closed payload: %w", err)
		}
		if payload.Outcome != "continue" && payload.Outcome != "final" && payload.Outcome != "failed" && payload.Outcome != "cancelled" {
			return fmt.Errorf("turn_closed has invalid outcome %q", payload.Outcome)
		}
		if payload.Outcome == "continue" || payload.Outcome == "final" {
			for operationID, record := range projection.operations {
				if record.turnID == *event.TurnID && record.requiresToolResult && record.state == operationTerminal && !record.toolResultAppended {
					return fmt.Errorf("turn_closed leaves tool result for operation %s unappended", operationID)
				}
			}
		}
		projection.CurrentTurn = nil
	case EventToolCallProposed, EventVerificationCommandProposed:
		if projection.State != RunRunning || event.TurnID == nil || projection.CurrentTurn == nil || *event.TurnID != *projection.CurrentTurn || event.OperationID == nil {
			return fmt.Errorf("%s requires the current running turn and operation id", event.Type)
		}
		if projection.operations == nil {
			projection.operations = make(map[OperationID]*operationRecord)
		}
		if _, exists := projection.operations[*event.OperationID]; exists {
			return fmt.Errorf("operation %s was proposed more than once", *event.OperationID)
		}
		projection.operations[*event.OperationID] = &operationRecord{state: operationProposed, turnID: *event.TurnID, requiresToolResult: event.Type == EventToolCallProposed}
	case EventAssistantMessageAppended:
		var payload AssistantMessagePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode assistant message: %w", err)
		}
		projection.LastAssistantMessage = payload.Text
	case EventVerificationFinished:
		var payload VerificationFinishedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode verification result: %w", err)
		}
		projection.VerificationSucceeded = payload.OK
		projection.LastVerificationSeq = event.Seq
	case EventContextCompacted:
		if projection.State != RunRunning || projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("context_compacted is not legal for run state %s", projection.State)
		}
		var payload ContextCompactedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode context compacted payload: %w", err)
		}
		if payload.ThroughSeq == 0 || payload.ThroughSeq >= event.Seq || payload.Summary == "" || payload.EstimatedTokensBefore < 1 || payload.EstimatedTokensAfter < 1 || payload.EstimatedTokensAfter > payload.EstimatedTokensBefore || payload.Strategy == "" {
			return errors.New("context_compacted payload is incomplete")
		}
		projection.LastContextCompactSeq = event.Seq
		projection.ContextCompactedThrough = payload.ThroughSeq
	case EventOperationStarted:
		if projection.State != RunRunning || projection.PendingOperation != nil {
			return fmt.Errorf("operation_started is not legal for run state %s", projection.State)
		}
		if event.OperationID == nil {
			return fmt.Errorf("operation_started requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationAuthorized {
			return fmt.Errorf("operation_started requires an authorized proposal")
		}
		var payload struct {
			ToolName        string `json:"tool_name"`
			SideEffectClass string `json:"side_effect_class"`
			MutationBarrier bool   `json:"mutation_barrier"`
			PlannedPath     string `json:"planned_path"`
			PlannedBefore   string `json:"planned_before"`
			PlannedAfter    string `json:"planned_after"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode operation start: %w", err)
		}
		if payload.SideEffectClass == "workspace" || (payload.SideEffectClass == "process" && payload.MutationBarrier) {
			projection.LastMutationSeq = event.Seq
		}
		projection.PendingOperation = &PendingOperation{ID: *event.OperationID, ToolName: payload.ToolName, SideEffectClass: payload.SideEffectClass, MutationBarrier: payload.MutationBarrier, PlannedPath: payload.PlannedPath, PlannedBefore: payload.PlannedBefore, PlannedAfter: payload.PlannedAfter}
		record.state = operationStarted
	case EventPolicyDecided:
		if event.OperationID == nil {
			return fmt.Errorf("policy_decided requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists {
			return fmt.Errorf("policy_decided requires a proposed operation")
		}
		var payload struct {
			Effect      string `json:"effect"`
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode policy decision: %w", err)
		}
		if payload.Fingerprint != "" {
			if projection.operationFingerprints == nil {
				projection.operationFingerprints = make(map[OperationID]string)
			}
			projection.operationFingerprints[*event.OperationID] = payload.Fingerprint
		}
		switch payload.Effect {
		case "allow":
			if record.state != operationProposed && record.state != operationApprovalGranted {
				return fmt.Errorf("policy allow requires a proposed or approved operation")
			}
			record.state = operationAuthorized
		case "ask":
			if record.state != operationProposed {
				return fmt.Errorf("policy ask requires a proposed operation")
			}
			record.state = operationAwaitingApprove
		case "deny":
			if record.state != operationProposed {
				return fmt.Errorf("policy deny requires a proposed operation")
			}
			record.state = operationDenied
		default:
			return fmt.Errorf("policy_decided has invalid effect %q", payload.Effect)
		}
	case EventOperationSucceeded, EventOperationFailed, EventOperationCancelled:
		if event.OperationID == nil {
			return fmt.Errorf("operation terminal event requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || (record.state != operationStarted && !(event.Type == EventOperationFailed && record.state == operationDenied)) {
			return fmt.Errorf("%s requires a started or denied operation", event.Type)
		}
		record.state = operationTerminal
		if projection.PendingOperation != nil && projection.PendingOperation.ID == *event.OperationID {
			projection.PendingOperation = nil
		}
	case EventOperationReconciled:
		if event.OperationID == nil || projection.PendingOperation == nil || projection.PendingOperation.ID != *event.OperationID {
			return fmt.Errorf("operation_reconciled requires the pending operation")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationStarted {
			return fmt.Errorf("operation_reconciled requires a started operation")
		}
		if fingerprint := projection.operationFingerprints[*event.OperationID]; fingerprint != "" {
			if projection.reconciledFingerprints == nil {
				projection.reconciledFingerprints = make(map[string]struct{})
			}
			projection.reconciledFingerprints[fingerprint] = struct{}{}
		}
		projection.PendingOperation = nil
		record.state = operationTerminal
	case EventApprovalRequested:
		if projection.State != RunRunning || projection.PendingApproval != nil {
			return fmt.Errorf("approval_requested is not legal for run state %s", projection.State)
		}
		if event.OperationID == nil {
			return fmt.Errorf("approval_requested requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationAwaitingApprove {
			return fmt.Errorf("approval_requested requires an ask decision")
		}
		var payload struct {
			Fingerprint string `json:"fingerprint"`
			Summary     string `json:"summary"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode approval request: %w", err)
		}
		projection.PendingApproval = &PendingApproval{OperationID: *event.OperationID, Fingerprint: payload.Fingerprint, Summary: payload.Summary}
	case EventApprovalGranted, EventApprovalDenied:
		if event.OperationID == nil || projection.PendingApproval == nil || projection.PendingApproval.OperationID != *event.OperationID {
			return fmt.Errorf("%s does not match a pending approval", event.Type)
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationAwaitingApprove {
			return fmt.Errorf("%s requires an operation waiting for approval", event.Type)
		}
		var payload struct {
			Fingerprint string `json:"fingerprint"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode approval result: %w", err)
		}
		if payload.Fingerprint == "" || payload.Fingerprint != projection.PendingApproval.Fingerprint {
			return fmt.Errorf("approval fingerprint does not match pending operation")
		}
		projection.PendingApproval = nil
		if event.Type == EventApprovalGranted {
			record.state = operationApprovalGranted
		} else {
			record.state = operationDenied
		}
	case EventToolResultAppended:
		if event.OperationID == nil {
			return fmt.Errorf("tool_result_appended requires operation id")
		}
		record, exists := projection.operations[*event.OperationID]
		if !exists || record.state != operationTerminal || record.toolResultAppended {
			return fmt.Errorf("tool_result_appended requires one terminal operation result")
		}
		record.toolResultAppended = true
	case EventRunCancelRequested:
		if projection.State != RunCreated && projection.State != RunRunning {
			return fmt.Errorf("run_cancel_requested from %s", projection.State)
		}
		projection.State = RunCancelling
	case EventRunCompleted:
		if projection.State != RunRunning || projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil || !projection.VerificationSucceeded || projection.LastVerificationSeq == 0 || (projection.LastMutationSeq != 0 && projection.LastVerificationSeq <= projection.LastMutationSeq) {
			return fmt.Errorf("run_completed without a closed successful verification")
		}
		projection.State = RunCompleted
	case EventRunFailed:
		if projection.State == RunCompleted || projection.State == RunCancelled || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("run_failed from terminal state %s", projection.State)
		}
		projection.State = RunFailed
	case EventRunCancelled:
		if projection.State != RunCancelling || projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil {
			return fmt.Errorf("run_cancelled from terminal state %s", projection.State)
		}
		projection.State = RunCancelled
	}
	projection.LastSeq = event.Seq
	return nil
}

// Terminal 报告运行是否已经进入不可继续的终态。
func (projection RunProjection) Terminal() bool {
	return projection.State == RunCompleted || projection.State == RunFailed || projection.State == RunCancelled
}

// RequiresFreshApproval 在同一指纹曾因不确定副作用被对账后返回 true；
// 回放历史绝不能再次授予执行权限。
func (projection RunProjection) RequiresFreshApproval(fingerprint string) bool {
	_, exists := projection.reconciledFingerprints[fingerprint]
	return fingerprint != "" && exists
}

// Phase 返回对外可见的恢复阶段；悬而未决的副作用操作不会在回放后被静默重试。
func (projection RunProjection) Phase() string {
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return "waiting_reconcile"
	}
	if projection.PendingApproval != nil {
		return "waiting_approval"
	}
	return string(projection.State)
}
