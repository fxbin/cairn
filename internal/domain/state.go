package domain

import (
	"encoding/json"
	"errors"
	"fmt"
)

type RunState string

const (
	RunCreated    RunState = "created"
	RunRunning    RunState = "running"
	RunCancelling RunState = "cancelling"
	RunCompleted  RunState = "completed"
	RunFailed     RunState = "failed"
	RunCancelled  RunState = "cancelled"
)

type RunProjection struct {
	ID                     RunID
	Task                   string
	Workspace              string
	WorkspaceHash          string
	WorkspaceObservation   string
	State                  RunState
	LastSeq                uint64
	CurrentTurn            *TurnID
	LastAssistantMessage   string
	VerificationSucceeded  bool
	LastVerificationSeq    uint64
	LastMutationSeq        uint64
	PendingApproval        *PendingApproval
	PendingOperation       *PendingOperation
	operationFingerprints  map[OperationID]string
	reconciledFingerprints map[string]struct{}
	operations             map[OperationID]*operationRecord
}

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

type operationRecord struct {
	state              operationState
	toolResultAppended bool
	turnID             TurnID
	requiresToolResult bool
}

type PendingApproval struct {
	OperationID OperationID
	Fingerprint string
	Summary     string
}

type PendingOperation struct {
	ID              OperationID
	ToolName        string
	SideEffectClass string
	MutationBarrier bool
	PlannedPath     string
	PlannedBefore   string
	PlannedAfter    string
}

type RunCreatedPayload struct {
	Task      string `json:"task"`
	Workspace string `json:"workspace"`
}

type TurnClosedPayload struct {
	Outcome string `json:"outcome"`
}

type AssistantMessagePayload struct {
	Text string `json:"text"`
}

type VerificationFinishedPayload struct {
	OK bool `json:"ok"`
}

type RunCompletedPayload struct {
	FinalResponse string `json:"final_response"`
}

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

func (projection RunProjection) Terminal() bool {
	return projection.State == RunCompleted || projection.State == RunFailed || projection.State == RunCancelled
}

// RequiresFreshApproval returns true when the same action fingerprint was
// previously reconciled after an uncertain external side effect. Replaying an
// operation never grants permission to execute it again.
func (projection RunProjection) RequiresFreshApproval(fingerprint string) bool {
	_, exists := projection.reconciledFingerprints[fingerprint]
	return fingerprint != "" && exists
}

// Phase is the externally visible recovery phase. A dangling side-effecting
// operation is deliberately not silently retried after replay.
func (projection RunProjection) Phase() string {
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return "waiting_reconcile"
	}
	if projection.PendingApproval != nil {
		return "waiting_approval"
	}
	return string(projection.State)
}
