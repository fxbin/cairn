// subagent_progress_test 锁定 AGT-03 子进度投影：真实状态、消息身份与工具计数。
//
// Author: fxbin
package app

import (
	"context"
	"github.com/fxbin/cairn/internal/domain"
	"testing"
)

// TestProjectSubagentProgressTracksLifecycle 锁定委派→完成的消息与状态。
func TestProjectSubagentProgressTracksLifecycle(t *testing.T) {
	journal := newCancelMemJournal()
	parent := domain.RunID("run-p")
	appendCancelEvent(t, journal, parent, domain.EventRunCreated, `{"payload_version":1,"task":"parent","workspace":"."}`)
	appendCancelEvent(t, journal, parent, domain.EventSubagentDelegated,
		`{"payload_version":1,"child_run_id":"run-c","task_digest":"`+TaskDigest("t")+`"}`)
	appendCancelEvent(t, journal, parent, domain.EventSubagentCompleted,
		`{"payload_version":1,"child_run_id":"run-c","outcome":"ok"}`)
	events := journal.byRun[parent]
	progress := ProjectSubagentProgress(parent, events)
	if len(progress) != 1 {
		t.Fatalf("应投影 1 个子运行: %+v", progress)
	}
	got := progress[0]
	if got.State != "completed" || got.Outcome != "ok" {
		t.Fatalf("状态不符: %+v", got)
	}
	if len(got.Messages) < 2 {
		t.Fatalf("应含 delegate/complete 消息: %+v", got.Messages)
	}
	if got.Messages[0].From != "run-p" || got.Messages[1].From != "run-c" {
		t.Fatalf("消息身份不符: %+v", got.Messages)
	}
}

// TestLoadChildDetailsCountsToolsAndApproval 锁定子运行详情只读补全。
func TestLoadChildDetailsCountsToolsAndApproval(t *testing.T) {
	journal := newCancelMemJournal()
	child := domain.RunID("run-c")
	appendCancelEvent(t, journal, child, domain.EventRunCreated, `{"payload_version":1,"task":"t","workspace":"."}`)
	appendCancelEvent(t, journal, child, domain.EventRunStarted, `{"payload_version":1,"invocation":"i"}`)
	appendCancelEventWithTurn(t, journal, child, domain.EventTurnOpened, `{"payload_version":1,"turn_index":1}`, "turn-1", "")
	appendCancelEventWithTurn(t, journal, child, domain.EventToolCallProposed, `{"payload_version":1,"call_id":"c1","tool_name":"read_file","arguments":{}}`, "turn-1", "op-1")
	appendCancelEventWithTurn(t, journal, child, domain.EventTurnClosed, `{"payload_version":1,"outcome":"final"}`, "turn-1", "")
	appendCancelEvent(t, journal, child, domain.EventRunFailed, `{"payload_version":1,"reason":"done"}`)
	progress := SubagentProgress{ChildRunID: "run-c"}
	if err := LoadChildDetails(context.Background(), journal, &progress); err != nil {
		t.Fatalf("补全失败: %v", err)
	}
	if progress.ToolCalls != 1 || progress.Turns != 1 {
		t.Fatalf("计数不符: %+v", progress)
	}
}

// appendCancelEventWithTurn 追加带 Turn/Operation 上下文的事件。
func appendCancelEventWithTurn(t *testing.T, journal *cancelMemJournal, runID domain.RunID, eventType domain.EventType, payload string, turnID, operationID string) {
	t.Helper()
	events := journal.byRun[runID]
	seq := uint64(1)
	prev := domain.GenesisHash
	if len(events) > 0 {
		seq = events[len(events)-1].Seq + 1
		prev = events[len(events)-1].EventHash
	}
	draft := domain.EventDraft{
		RunID: runID, WorkspaceHash: "ws", Type: eventType, Payload: []byte(payload),
	}
	if turnID != "" {
		id := domain.TurnID(turnID)
		draft.TurnID = &id
	}
	if operationID != "" {
		id := domain.OperationID(operationID)
		draft.OperationID = &id
	}
	event, err := domain.NewEvent(draft, seq, prev)
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	journal.byRun[runID] = append(journal.byRun[runID], event)
}
