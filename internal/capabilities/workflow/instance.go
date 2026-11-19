// [INPUT]: 版本化定义引用、工作流事件与 Run 标识。
// [OUTPUT]: 从版本化事件链投影尝试、人工决定与步骤依赖。
// [POS]: 工作流能力的版本化重放规则。应用层核验 Run Journal 和制品。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件定义版本化实例的有序步骤和尝试合同。
// Author: fxbin
package workflow

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

const (
	// EventGateDecided 在单个事件中记录人工门决定及尝试终态。
	EventGateDecided EventType = "workflow_gate_decided"
	// GateApproved 表示操作员显式通过当前人工门。
	GateApproved = "approve"
	// GateRejected 表示操作员显式拒绝当前人工门。
	GateRejected = "reject"
)

// RunAnchor 指向已绑定 Run 的 Journal 终态事件。
type RunAnchor struct {
	Sequence  uint64 `json:"seq"`
	EventHash string `json:"event_hash"`
}

// Dependency 冻结一个前序节点的已成功尝试和输出。
type Dependency struct {
	StepID  string `json:"step_id"`
	Attempt int    `json:"attempt"`
	Output  StepIO `json:"output"`
}

// Attempt 记录一次执行或人工决定。新尝试不会替换历史尝试。
type Attempt struct {
	Number            int          `json:"number"`
	StartedSeq        uint64       `json:"started_seq"`
	StartedAt         time.Time    `json:"started_at"`
	FinishedAt        time.Time    `json:"finished_at,omitempty"`
	RunID             domain.RunID `json:"run_id,omitempty"`
	ConfigFingerprint string       `json:"config_fingerprint,omitempty"`
	CreationEventID   string       `json:"creation_event_id,omitempty"`
	State             StepState    `json:"state"`
	Dependencies      []Dependency `json:"dependencies,omitempty"`
	Output            StepIO       `json:"output"`
	Terminal          *RunAnchor   `json:"terminal,omitempty"`
	ErrorClass        string       `json:"error_class,omitempty"`
	Decision          string       `json:"decision,omitempty"`
	Reason            string       `json:"reason,omitempty"`
}

// GateDecidedPayload 将显式人工决定与当前步骤、尝试和依赖绑定。
type GateDecidedPayload struct {
	StepID       string       `json:"step_id"`
	Attempt      int          `json:"attempt"`
	Decision     string       `json:"decision"`
	Reason       string       `json:"reason"`
	Dependencies []Dependency `json:"dependencies,omitempty"`
}

// Dependencies 返回本节点声明的前序产物锚点，顺序与创建事件一致。
func (snapshot *Snapshot) Dependencies(stepIndex int) ([]Dependency, error) {
	spec := snapshot.Specs[stepIndex]
	var output []Dependency
	for _, nodeID := range spec.Inputs {
		attempts := snapshot.Attempts[nodeID]
		if len(attempts) == 0 {
			return nil, fmt.Errorf("workflow input %q has no attempt", nodeID)
		}
		attempt := attempts[len(attempts)-1]
		if attempt.State != StepSucceeded || attempt.Output.Ref == "" {
			return nil, fmt.Errorf("workflow input %q has no successful output", nodeID)
		}
		output = append(output, Dependency{StepID: nodeID, Attempt: attempt.Number, Output: attempt.Output})
	}
	return output, nil
}

// validateInstanceSpecs 校验新实例的步骤身份、安全策略及线性依赖。
func validateInstanceSpecs(created RunCreatedPayload) error {
	if created.Definition.Validate() != nil || !validDefinitionID(created.SessionID) {
		return errors.New("versioned workflow requires definition and session binding")
	}
	previous := make(map[string]string, len(created.Steps))
	for _, step := range created.Steps {
		if step.MaxAttempts < 1 || step.MaxAttempts > maxNodeAttempts {
			return fmt.Errorf("workflow step %q has invalid attempt limit", step.ID)
		}
		if step.RetryFrom != "" && previous[step.RetryFrom] != StepKindExecute && previous[step.RetryFrom] != StepKindScript {
			return fmt.Errorf("workflow step %q has invalid rework entry", step.ID)
		}
		seen := make(map[string]struct{}, len(step.Inputs))
		for _, input := range step.Inputs {
			if previous[input] == "" || previous[input] == StepKindAccept {
				return fmt.Errorf("workflow step %q has invalid input %q", step.ID, input)
			}
			if _, duplicate := seen[input]; duplicate {
				return fmt.Errorf("workflow step %q has duplicate input %q", step.ID, input)
			}
			seen[input] = struct{}{}
		}
		previous[step.ID] = step.Kind
	}
	return nil
}

// applyInstanceEvent 严格推进版本化实例。旧实例使用原事件迁移规则。
func applyInstanceEvent(snapshot *Snapshot, plan *Plan, indexes map[string]int, event Event) error {
	if snapshot.Finished {
		return errors.New("workflow event follows a terminal instance")
	}
	switch event.Type {
	case EventWorkflowPaused, EventWorkflowContinued:
		wantPause := event.Type == EventWorkflowPaused
		if snapshot.Paused == wantPause || plan.ResumeFrom() < 0 {
			return errors.New("invalid workflow pause transition")
		}
		snapshot.Paused = wantPause
		return nil
	case EventRetryAuthorized:
		var retry RetryPayload
		if err := json.Unmarshal(event.Payload, &retry); err != nil {
			return err
		}
		return applyRetry(snapshot, plan, indexes, retry)
	case EventStepStarted:
		var started StepStartedPayload
		if err := json.Unmarshal(event.Payload, &started); err != nil {
			return err
		}
		return startInstanceAttempt(snapshot, plan, indexes, event, started)
	case EventStepFinished:
		var finished StepFinishedPayload
		if err := json.Unmarshal(event.Payload, &finished); err != nil {
			return err
		}
		return finishInstanceAttempt(snapshot, plan, event, finished)
	case EventGateDecided:
		var decided GateDecidedPayload
		if err := json.Unmarshal(event.Payload, &decided); err != nil {
			return err
		}
		return decideInstanceGate(snapshot, plan, indexes, event, decided)
	case EventRunFinished:
		var finished RunFinishedPayload
		if err := json.Unmarshal(event.Payload, &finished); err != nil {
			return err
		}
		return finishInstance(snapshot, plan, finished)
	default:
		return fmt.Errorf("invalid versioned workflow event %q", event.Type)
	}
}

// validateNewAttempt 核对顺序、预算与当前依赖身份。
func validateNewAttempt(snapshot *Snapshot, plan *Plan, indexes map[string]int, stepID string, number int, dependencies []Dependency) (int, error) {
	if snapshot.Paused {
		return -1, errors.New("workflow is paused")
	}
	if err := ensureSequentialStart(plan, indexes, stepID); err != nil {
		return -1, err
	}
	index := indexes[stepID]
	attempts := snapshot.Attempts[stepID]
	if plan.Steps[index].State != StepPending || number != len(attempts)+1 || number > snapshot.Specs[index].MaxAttempts {
		return -1, fmt.Errorf("invalid new attempt for workflow step %q", stepID)
	}
	expected, err := snapshot.Dependencies(index)
	if err != nil {
		return -1, err
	}
	if !slices.Equal(expected, dependencies) {
		return -1, fmt.Errorf("workflow step %q dependency binding mismatch", stepID)
	}
	return index, nil
}

// startInstanceAttempt 绑定下一次尝试与独立 Run 的创建锚点。
func startInstanceAttempt(snapshot *Snapshot, plan *Plan, indexes map[string]int, event Event, started StepStartedPayload) error {
	index, err := validateNewAttempt(snapshot, plan, indexes, started.StepID, started.Attempt, started.Dependencies)
	if err != nil {
		return err
	}
	if snapshot.Specs[index].Kind == StepKindAccept || started.RunID == "" || started.CreationEventID == "" || started.ConfigFingerprint == "" {
		return fmt.Errorf("invalid run binding for workflow step %q", started.StepID)
	}
	for _, attempts := range snapshot.Attempts {
		for _, attempt := range attempts {
			if attempt.RunID == domain.RunID(started.RunID) || attempt.CreationEventID == started.CreationEventID {
				return errors.New("workflow run attempt binding is already in use")
			}
		}
	}
	if err := plan.MarkStep(started.StepID, StepRunning, started.Input, StepIO{}, "", event.OccurredAt); err != nil {
		return err
	}
	attempt := Attempt{
		Number: started.Attempt, StartedSeq: event.Seq, StartedAt: event.OccurredAt,
		RunID: domain.RunID(started.RunID), CreationEventID: started.CreationEventID, ConfigFingerprint: started.ConfigFingerprint,
		State: StepRunning, Dependencies: started.Dependencies,
	}
	snapshot.StepRuns[started.StepID] = started.RunID
	snapshot.Attempts[started.StepID] = append(snapshot.Attempts[started.StepID], attempt)
	return nil
}

// finishInstanceAttempt 收束同一尝试，或保留未确认的副作用。
func finishInstanceAttempt(snapshot *Snapshot, plan *Plan, event Event, finished StepFinishedPayload) error {
	attempts := snapshot.Attempts[finished.StepID]
	if len(attempts) == 0 {
		return errors.New("workflow step has no active attempt")
	}
	attempt := &attempts[len(attempts)-1]
	if finished.Attempt != attempt.Number || attempt.RunID == "" ||
		(attempt.State != StepRunning && attempt.State != StepUncertain) {
		return errors.New("workflow step result does not reference the active run attempt")
	}
	if finished.State == StepUncertain {
		if finished.Terminal != nil || finished.Output != (StepIO{}) {
			return errors.New("uncertain workflow step cannot report terminal evidence")
		}
	} else if finished.Output.Ref != finished.Output.SHA || finished.Terminal == nil || finished.Terminal.Sequence == 0 || finished.Terminal.EventHash == "" ||
		(finished.State == StepSucceeded && (finished.Output.Ref == "" || finished.Output.Ref != finished.Output.SHA)) {
		return errors.New("settled workflow attempt requires anchored run evidence")
	}
	if err := plan.MarkStep(finished.StepID, finished.State, StepIO{}, finished.Output, finished.ErrorClass, event.OccurredAt); err != nil {
		return err
	}
	attempt.State, attempt.Output, attempt.ErrorClass, attempt.Terminal = finished.State, finished.Output, finished.ErrorClass, finished.Terminal
	if finished.State != StepUncertain {
		attempt.FinishedAt = event.OccurredAt
	}
	return nil
}

// decideInstanceGate 以单一事件开始并收束人工门尝试，消除两次追加之间的中断。
func decideInstanceGate(snapshot *Snapshot, plan *Plan, indexes map[string]int, event Event, decided GateDecidedPayload) error {
	index, err := validateNewAttempt(snapshot, plan, indexes, decided.StepID, decided.Attempt, decided.Dependencies)
	if err != nil {
		return err
	}
	if snapshot.Specs[index].Kind != StepKindAccept {
		return errors.New("workflow decision targets a non-human step")
	}
	state, classification := StepSucceeded, ""
	if decided.Decision == GateRejected {
		state, classification = StepFailed, "human_rejected"
	}
	if err := plan.MarkStep(decided.StepID, StepRunning, StepIO{}, StepIO{}, "", event.OccurredAt); err != nil {
		return err
	}
	if err := plan.MarkStep(decided.StepID, state, StepIO{}, StepIO{}, classification, event.OccurredAt); err != nil {
		return err
	}
	snapshot.Attempts[decided.StepID] = append(snapshot.Attempts[decided.StepID], Attempt{
		Number: decided.Attempt, StartedSeq: event.Seq, State: state, StartedAt: event.OccurredAt, FinishedAt: event.OccurredAt,
		Dependencies: decided.Dependencies, ErrorClass: classification, Decision: decided.Decision, Reason: decided.Reason,
	})
	return nil
}

// finishInstance 核对终态不掩盖等待对账或仍在执行的尝试。
func finishInstance(snapshot *Snapshot, plan *Plan, finished RunFinishedPayload) error {
	index := plan.ResumeFrom()
	if index != finished.ResumeIndex || (finished.State == RunStateCompleted && index >= 0) ||
		(finished.State != RunStateCompleted && index < 0) {
		return errors.New("workflow terminal does not match replayed resume position")
	}
	for _, step := range plan.Steps {
		if step.State == StepRunning || step.State == StepUncertain {
			return errors.New("workflow cannot finish with an unsettled run")
		}
	}
	if finished.State == RunStateFailed && plan.Steps[index].State != StepFailed {
		return errors.New("workflow failure requires a failed step")
	}
	snapshot.Finished, snapshot.FinishState, snapshot.FinishReason = true, finished.State, finished.Reason
	snapshot.Paused = false
	return nil
}
