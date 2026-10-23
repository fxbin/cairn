// subagent_resume.go 实现 AGT-05 取消恢复与预算汇总：重开从事件重建子状态
// 且不重复派发；共享预算按子身份去重计费；保留资源清理证据。
// Author: fxbin
package app

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// SubagentResumeState 是重开后从事件流重建的子运行状态。
type SubagentResumeState struct {
	ChildRunID string `json:"child_run_id"`
	State      string `json:"state"`
	Outcome    string `json:"outcome,omitempty"`
	// AlreadyDelegated 为 true 时禁止再次派发同一子身份。
	AlreadyDelegated bool `json:"already_delegated"`
}

// CleanupEvidence 记录子运行结束后的资源清理事实。
type CleanupEvidence struct {
	ChildRunID        string    `json:"child_run_id"`
	WorkspaceReleased bool      `json:"workspace_released"`
	SessionsClosed    bool      `json:"sessions_closed"`
	RecordedAt        time.Time `json:"recorded_at"`
}

// BudgetSummary 是父运行下各子运行的去重预算汇总。
type BudgetSummary struct {
	DelegatedTurns  int   `json:"delegated_turns"`
	DelegatedTokens int64 `json:"delegated_tokens"`
	UsedTurns       int   `json:"used_turns"`
	UsedTokens      int64 `json:"used_tokens"`
	UniqueChildren  int   `json:"unique_children"`
}

// RebuildSubagentStates 从父运行事件重建全部子状态；重开时不新增委派。
func RebuildSubagentStates(events []domain.Event) map[string]SubagentResumeState {
	states := map[string]SubagentResumeState{}
	for _, event := range events {
		switch event.Type {
		case domain.EventSubagentDelegated:
			var payload DelegatedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			states[payload.ChildRunID] = SubagentResumeState{
				ChildRunID:       payload.ChildRunID,
				State:            "delegated",
				AlreadyDelegated: true,
			}
		case domain.EventSubagentCompleted:
			var payload CompletedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			state := states[payload.ChildRunID]
			state.ChildRunID = payload.ChildRunID
			state.State = "completed"
			state.Outcome = payload.Outcome
			state.AlreadyDelegated = true
			states[payload.ChildRunID] = state
		}
	}
	return states
}

// ShouldRejectRedelegation 判断重开后是否应拒绝重复派发。
func ShouldRejectRedelegation(states map[string]SubagentResumeState, childRunID string) bool {
	return states[childRunID].AlreadyDelegated
}

// SummarizeBudget 从父事件汇总去重预算：同一子身份只计一次委派额度。
func SummarizeBudget(events []domain.Event) BudgetSummary {
	summary := BudgetSummary{}
	seenDelegated := map[string]SubagentBudget{}
	seenCompleted := map[string]struct{}{}
	for _, event := range events {
		switch event.Type {
		case domain.EventSubagentDelegated:
			var payload DelegatedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			if _, dup := seenDelegated[payload.ChildRunID]; dup {
				continue
			}
			seenDelegated[payload.ChildRunID] = payload.Budget
			summary.DelegatedTurns += payload.Budget.MaxTurns
			summary.DelegatedTokens += payload.Budget.MaxTokens
		case domain.EventSubagentCompleted:
			var payload CompletedPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			if _, dup := seenCompleted[payload.ChildRunID]; dup {
				continue
			}
			seenCompleted[payload.ChildRunID] = struct{}{}
			usage, err := UsageFromCompletion(payload.Usage)
			if err != nil {
				continue
			}
			summary.UsedTurns++
			summary.UsedTokens += usage.TotalTokens()
		}
	}
	summary.UniqueChildren = len(seenDelegated)
	return summary
}

// BuildCleanupEvidence 组装资源清理证据；调用方负责实际释放后标记。
func BuildCleanupEvidence(childRunID domain.RunID, workspaceReleased, sessionsClosed bool, at time.Time) CleanupEvidence {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return CleanupEvidence{
		ChildRunID:        string(childRunID),
		WorkspaceReleased: workspaceReleased,
		SessionsClosed:    sessionsClosed,
		RecordedAt:        at.UTC(),
	}
}

// ExplainChildFailure 为子失败生成可解释摘要。
func ExplainChildFailure(outcome, errorClass string) string {
	if outcome == "" {
		outcome = "unknown"
	}
	if errorClass == "" {
		return fmt.Sprintf("child finished with outcome=%s (no error class)", outcome)
	}
	return fmt.Sprintf("child finished with outcome=%s error_class=%s", outcome, errorClass)
}
