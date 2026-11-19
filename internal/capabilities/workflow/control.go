// [INPUT]: 依赖版本化步骤、在链尝试预算与冻结返工范围。
// [OUTPUT]: 校验和重放暂停、恢复与返工授权。
// [POS]: 工作流实例的控制事实；既有运行授权仍由应用层装配。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件定义工作流的有界控制规则。
// Author: fxbin
package workflow

import (
	"errors"
	"fmt"
	"strings"
)

const (
	// EventWorkflowPaused 持久化暂停未来节点推进的决定。
	EventWorkflowPaused EventType = "workflow_paused"
	// EventWorkflowContinued 记录操作员解除暂停的决定。
	EventWorkflowContinued EventType = "workflow_continued"
	// EventRetryAuthorized 记录一次有界重试或返工授权。
	EventRetryAuthorized EventType = "workflow_retry_authorized"
)

// ControlPayload 携带显式生命周期控制决定的可读原因。
type ControlPayload struct {
	Reason string `json:"reason"`
}

// RetryPayload 锚定失败的尝试和已声明的返工起点。
type RetryPayload struct {
	FailedStepID  string `json:"failed_step_id"`
	FailedAttempt int    `json:"failed_attempt"`
	FromStepID    string `json:"from_step_id"`
	Reason        string `json:"reason"`
}

// ReconciliationAborted 表示操作因不可确认而终止。该尝试不得返工重派。
const ReconciliationAborted = "reconciliation_aborted"

// RetryTargets 给出当前失败可用的两个授权入口。
func (snapshot *Snapshot) RetryTargets() []string {
	index := snapshot.ResumeIndex
	if index < 0 || snapshot.Finished || snapshot.Steps[index].State != StepFailed || snapshot.Steps[index].ErrorClass == ReconciliationAborted {
		return nil
	}
	current := snapshot.Specs[index]
	var targets []string
	if snapshot.canRetryRange(index, index) {
		targets = append(targets, current.ID)
	}
	if current.RetryFrom != "" {
		for start, spec := range snapshot.Specs[:index] {
			if spec.ID == current.RetryFrom && snapshot.canRetryRange(start, index) {
				targets = append(targets, spec.ID)
				break
			}
		}
	}
	return targets
}

// canRetryRange 要求回退区间内每个已运行节点均有下一次尝试额度。
func (snapshot *Snapshot) canRetryRange(from, failed int) bool {
	for _, spec := range snapshot.Specs[from : failed+1] {
		if len(snapshot.Attempts[spec.ID]) >= spec.MaxAttempts {
			return false
		}
	}
	return true
}

// applyRetry 复核当前失败尝试及返工范围，将计划游标移回起点并保留历史。
func applyRetry(snapshot *Snapshot, plan *Plan, indexes map[string]int, payload RetryPayload) error {
	failed := plan.ResumeFrom()
	if snapshot.Paused || failed < 0 || plan.Steps[failed].State != StepFailed || plan.Steps[failed].ErrorClass == ReconciliationAborted ||
		plan.Steps[failed].ID != payload.FailedStepID || len(snapshot.Attempts[payload.FailedStepID]) != payload.FailedAttempt {
		return errors.New("workflow retry does not match the current settled failure")
	}
	start, found := indexes[payload.FromStepID]
	if !found || start > failed || (start != failed && snapshot.Specs[failed].RetryFrom != payload.FromStepID) {
		return errors.New("workflow retry requires an authorized retry target")
	}
	if !snapshot.canRetryRange(start, failed) {
		return fmt.Errorf("workflow retry attempt budget exhausted at %q", payload.FromStepID)
	}
	for index := start; index <= failed; index++ {
		id := plan.Steps[index].ID
		plan.Steps[index] = Step{ID: id, Index: index, State: StepPending}
		delete(snapshot.StepRuns, id)
	}
	snapshot.LastRetry = &payload
	return nil
}

// validateControlReason 拒绝没有显式原因的工作流控制。
func validateControlReason(reason string) error {
	if strings.TrimSpace(reason) == "" {
		return errors.New("workflow control requires a reason")
	}
	return nil
}
