// Package workflow 定义工作流步骤进度、失败与恢复合同：每步输入输出可引用，
// 取消/恢复位置明确；不确定副作用必须对账，禁止整流程重跑掩盖。
// Author: fxbin
package workflow

import (
	"fmt"
	"strings"
	"time"
)

// StepState 是步骤生命周期。
type StepState string

const (
	// StepPending 尚未开始。
	StepPending StepState = "pending"
	// StepRunning 执行中。
	StepRunning StepState = "running"
	// StepSucceeded 成功。
	StepSucceeded StepState = "succeeded"
	// StepFailed 失败。
	StepFailed StepState = "failed"
	// StepUncertain 副作用不确定，必须对账。
	StepUncertain StepState = "uncertain"
	// StepCancelled 已取消。
	StepCancelled StepState = "cancelled"
)

// StepIO 绑定输入输出引用，不内联大正文。
type StepIO struct {
	Ref     string `json:"ref,omitempty"`
	SHA     string `json:"sha,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// Step 是单步执行事实。
type Step struct {
	ID         string    `json:"id"`
	Index      int       `json:"index"`
	State      StepState `json:"state"`
	Input      StepIO    `json:"input"`
	Output     StepIO    `json:"output"`
	ErrorClass string    `json:"error_class,omitempty"`
	StartedAt  time.Time `json:"started_at,omitempty"`
	EndedAt    time.Time `json:"ended_at,omitempty"`
}

// Plan 是有序步骤及其当前位置。
type Plan struct {
	RunID string `json:"run_id"`
	Steps []Step `json:"steps"`
	// ResumeIndex 是恢复时应继续的下标；-1 表示未恢复。
	ResumeIndex int `json:"resume_index"`
}

// NewPlan 创建空计划；步骤 ID 必须唯一。
func NewPlan(runID string, steps []Step) (*Plan, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, fmt.Errorf("run id is required")
	}
	seen := make(map[string]struct{}, len(steps))
	for index := range steps {
		steps[index].Index = index
		if strings.TrimSpace(steps[index].ID) == "" {
			return nil, fmt.Errorf("step id is required")
		}
		if _, dup := seen[steps[index].ID]; dup {
			return nil, fmt.Errorf("duplicate step id %q", steps[index].ID)
		}
		seen[steps[index].ID] = struct{}{}
		if steps[index].State == "" {
			steps[index].State = StepPending
		}
	}
	return &Plan{RunID: runID, Steps: steps, ResumeIndex: -1}, nil
}

// MarkStep 更新单步状态与 IO；非法迁移失败关闭。
func (plan *Plan) MarkStep(id string, state StepState, input, output StepIO, errorClass string, at time.Time) error {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	for index := range plan.Steps {
		step := &plan.Steps[index]
		if step.ID != id {
			continue
		}
		if !allowedTransition(step.State, state) {
			return fmt.Errorf("step %q cannot move from %s to %s", id, step.State, state)
		}
		step.State = state
		if input.Ref != "" || input.SHA != "" || input.Summary != "" {
			step.Input = input
		}
		if output.Ref != "" || output.SHA != "" || output.Summary != "" {
			step.Output = output
		}
		step.ErrorClass = errorClass
		switch state {
		case StepRunning:
			step.StartedAt = at
		case StepSucceeded, StepFailed, StepUncertain, StepCancelled:
			step.EndedAt = at
		}
		return nil
	}
	return fmt.Errorf("step %q not found", id)
}

func allowedTransition(from, to StepState) bool {
	switch from {
	case StepPending:
		return to == StepRunning || to == StepCancelled
	case StepRunning:
		return to == StepSucceeded || to == StepFailed || to == StepUncertain || to == StepCancelled
	case StepUncertain:
		// 只允许对账后收束，不允许直接重跑
		return to == StepSucceeded || to == StepFailed
	case StepFailed, StepCancelled:
		return to == StepRunning // 显式恢复
	case StepSucceeded:
		return false
	}
	return false
}

// CancelRemaining 将未收束步骤标记为已取消，并返回取消位置。
func (plan *Plan) CancelRemaining(at time.Time) int {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	resumeIndex := -1
	for index := range plan.Steps {
		step := &plan.Steps[index]
		if step.State == StepPending || step.State == StepRunning {
			step.State = StepCancelled
			step.EndedAt = at
			if resumeIndex < 0 {
				resumeIndex = index
			}
		}
	}
	plan.ResumeIndex = resumeIndex
	return resumeIndex
}

// ResumeFrom 返回可恢复下标：首个未成功步骤；全成功返回 -1。
func (plan *Plan) ResumeFrom() int {
	for index := range plan.Steps {
		if plan.Steps[index].State != StepSucceeded {
			return index
		}
	}
	return -1
}

// RequiresReconcile 列出副作用不确定的步骤；非空表示禁止重跑整个工作流。
func (plan *Plan) RequiresReconcile() []string {
	var ids []string
	for _, step := range plan.Steps {
		if step.State == StepUncertain {
			ids = append(ids, step.ID)
		}
	}
	return ids
}

// CanRestart 判断是否允许整流程重跑：存在不确定副作用时禁止。
func (plan *Plan) CanRestart() (bool, string) {
	if pending := plan.RequiresReconcile(); len(pending) > 0 {
		return false, "reconcile uncertain steps first: " + strings.Join(pending, ",")
	}
	return true, ""
}
