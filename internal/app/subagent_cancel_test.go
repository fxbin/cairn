// subagent_cancel_test 锁定子运行取消传播：仅未终态子 run 收到取消请求。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// cancelMemJournal 是同包测试用追加式 Journal。
type cancelMemJournal struct {
	mu    sync.Mutex
	byRun map[domain.RunID][]domain.Event
}

func newCancelMemJournal() *cancelMemJournal {
	return &cancelMemJournal{byRun: map[domain.RunID][]domain.Event{}}
}

func (j *cancelMemJournal) Append(_ context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	existing := j.byRun[draft.RunID]
	if uint64(len(existing)) != expectedSeq {
		return domain.Event{}, errors.New("seq mismatch")
	}
	prev := domain.GenesisHash
	if len(existing) > 0 {
		prev = existing[len(existing)-1].EventHash
	}
	event, err := domain.NewEvent(draft, expectedSeq, prev)
	if err != nil {
		return domain.Event{}, err
	}
	j.byRun[draft.RunID] = append(existing, event)
	return event, nil
}

func (j *cancelMemJournal) Load(_ context.Context, runID domain.RunID) ([]domain.Event, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]domain.Event(nil), j.byRun[runID]...), nil
}

func (j *cancelMemJournal) NonTerminalRuns(context.Context, string) ([]domain.RunProjection, error) {
	return nil, nil
}

func (j *cancelMemJournal) Check(context.Context) error { return nil }

var _ ports.Journal = (*cancelMemJournal)(nil)

// appendCancelEvent 以链式序号追加事件。
func appendCancelEvent(t *testing.T, journal *cancelMemJournal, runID domain.RunID, eventType domain.EventType, payload string) {
	t.Helper()
	events := journal.byRun[runID]
	prev := domain.GenesisHash
	seq := uint64(1)
	if len(events) > 0 {
		prev = events[len(events)-1].EventHash
		seq = events[len(events)-1].Seq + 1
	}
	draft := domain.EventDraft{
		RunID: runID, WorkspaceHash: "ws", Type: eventType, Payload: []byte(payload),
	}
	event, err := domain.NewEvent(draft, seq, prev)
	if err != nil {
		t.Fatalf("new event: %v", err)
	}
	journal.byRun[runID] = append(journal.byRun[runID], event)
}

// TestPendingChildRunIDsFiltersUnfinished 锁定只收集 delegated 子身份。
func TestPendingChildRunIDsFiltersUnfinished(t *testing.T) {
	projection := domain.RunProjection{Children: map[string]domain.SubagentRecord{
		"run-a": {ChildRunID: "run-a", State: "delegated"},
		"run-b": {ChildRunID: "run-b", State: "completed", Outcome: "ok"},
	}}
	ids := PendingChildRunIDs(projection)
	if len(ids) != 1 || ids[0] != "run-a" {
		t.Fatalf("应只含 run-a，得到 %v", ids)
	}
}

// TestPropagateSubagentCancelOnlyUnfinished 锁定传播只影响未终态子 run。
func TestPropagateSubagentCancelOnlyUnfinished(t *testing.T) {
	journal := newCancelMemJournal()
	parent := domain.RunID("run-p")
	child := domain.RunID("run-a")
	appendCancelEvent(t, journal, parent, domain.EventRunCreated, `{"payload_version":1,"task":"parent","workspace":"."}`)
	appendCancelEvent(t, journal, parent, domain.EventSubagentDelegated, `{"payload_version":1,"child_run_id":"run-a","task_digest":"`+TaskDigest("t")+`"}`)
	appendCancelEvent(t, journal, child, domain.EventRunCreated, `{"payload_version":1,"task":"t","workspace":"."}`)
	appendCancelEvent(t, journal, child, domain.EventRunFailed, `{"payload_version":1,"reason":"done"}`)
	if err := PropagateSubagentCancel(context.Background(), journal, "ws", parent, time.Unix(2, 0).UTC()); err != nil {
		t.Fatalf("传播失败: %v", err)
	}
	for _, event := range journal.byRun[child] {
		if event.Type == domain.EventRunCancelRequested {
			t.Fatal("已终态子 run 不应收到取消请求")
		}
	}
	// 未终态子 run 应收到
	child2 := domain.RunID("run-b")
	appendCancelEvent(t, journal, parent, domain.EventSubagentDelegated, `{"payload_version":1,"child_run_id":"run-b","task_digest":"`+TaskDigest("t2")+`"}`)
	appendCancelEvent(t, journal, child2, domain.EventRunCreated, `{"payload_version":1,"task":"t2","workspace":"."}`)
	if err := PropagateSubagentCancel(context.Background(), journal, "ws", parent, time.Unix(3, 0).UTC()); err != nil {
		t.Fatalf("传播失败: %v", err)
	}
	found := false
	for _, event := range journal.byRun[child2] {
		if event.Type == domain.EventRunCancelRequested {
			found = true
		}
	}
	if !found {
		t.Fatal("未终态子 run 应收到取消请求")
	}
}
