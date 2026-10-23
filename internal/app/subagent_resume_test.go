// subagent_resume_test 锁定 AGT-05 重开不重复派发与预算去重汇总。
//
// Author: fxbin
package app

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// TestRebuildSubagentStatesRejectsRedelegation 锁定重开后已委派不可再派。
func TestRebuildSubagentStatesRejectsRedelegation(t *testing.T) {
	delegated, _ := json.Marshal(DelegatedPayload{ChildRunID: "run-c", TaskDigest: "d", Budget: SubagentBudget{MaxTurns: 2}})
	completed, _ := json.Marshal(CompletedPayload{ChildRunID: "run-c", Outcome: "ok"})
	events := []domain.Event{
		{Type: domain.EventSubagentDelegated, Payload: delegated},
		{Type: domain.EventSubagentCompleted, Payload: completed},
	}
	states := RebuildSubagentStates(events)
	state := states["run-c"]
	if state.State != "completed" || state.Outcome != "ok" || !state.AlreadyDelegated {
		t.Fatalf("重建状态不符: %+v", state)
	}
	if !ShouldRejectRedelegation(states, "run-c") {
		t.Fatal("已委派应拒绝重复派发")
	}
	if ShouldRejectRedelegation(states, "run-other") {
		t.Fatal("未委派身份不应拒绝")
	}
}

// TestSummarizeBudgetDeduplicatesChildren 锁定同一子身份只计一次。
func TestSummarizeBudgetDeduplicatesChildren(t *testing.T) {
	d1, _ := json.Marshal(DelegatedPayload{ChildRunID: "run-a", TaskDigest: "d", Budget: SubagentBudget{MaxTurns: 2, MaxTokens: 100}})
	d2, _ := json.Marshal(DelegatedPayload{ChildRunID: "run-a", TaskDigest: "d", Budget: SubagentBudget{MaxTurns: 2, MaxTokens: 100}})
	c1, _ := json.Marshal(CompletedPayload{ChildRunID: "run-a", Outcome: "ok", Usage: json.RawMessage(`{"prompt_tokens":10,"completion_tokens":5}`)})
	events := []domain.Event{
		{Type: domain.EventSubagentDelegated, Payload: d1},
		{Type: domain.EventSubagentDelegated, Payload: d2},
		{Type: domain.EventSubagentCompleted, Payload: c1},
		{Type: domain.EventSubagentCompleted, Payload: c1},
	}
	summary := SummarizeBudget(events)
	if summary.UniqueChildren != 1 || summary.DelegatedTurns != 2 || summary.DelegatedTokens != 100 {
		t.Fatalf("委派汇总不符: %+v", summary)
	}
	if summary.UsedTurns != 1 || summary.UsedTokens != 15 {
		t.Fatalf("用量汇总应去重: %+v", summary)
	}
}

// TestCleanupEvidenceAndFailureExplain 锁定清理证据与失败解释。
func TestCleanupEvidenceAndFailureExplain(t *testing.T) {
	at := time.Unix(10, 0).UTC()
	evidence := BuildCleanupEvidence("run-c", true, true, at)
	if !evidence.WorkspaceReleased || !evidence.SessionsClosed || !evidence.RecordedAt.Equal(at) {
		t.Fatalf("清理证据不符: %+v", evidence)
	}
	if got := ExplainChildFailure("failed", "timeout"); got != "child finished with outcome=failed error_class=timeout" {
		t.Fatalf("失败解释不符: %q", got)
	}
}
