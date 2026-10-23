// subagent_journal_test 使用真实 Journal 锁定序号、可回放性与失败关闭。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

func seedSubagentParent(t *testing.T, journal ports.Journal) {
	t.Helper()
	_, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: "run-p", WorkspaceHash: "ws", Type: domain.EventRunCreated, OccurredAt: time.Unix(1, 0).UTC(), Payload: json.RawMessage(`{"task":"parent","workspace":"/workspace"}`)})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRecordSubagentDelegatedAppendsToParent(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var journal ports.Journal = memory.New()
			if backend == "sqlite" {
				real, err := sqlite.Open(filepath.Join(t.TempDir(), "events.db"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = real.Close() })
				journal = real
			}
			seedSubagentParent(t, journal)
			first, err := RecordSubagentDelegated(context.Background(), journal, "run-p", "ws", DelegateOptions{ParentRunID: "run-p", ChildRunID: "run-c", Task: "盘点", Budget: SubagentBudget{MaxTurns: 2}}, time.Unix(2, 0).UTC())
			if err != nil {
				t.Fatal(err)
			}
			second, err := RecordSubagentCompleted(context.Background(), journal, "run-p", "ws", "run-c", "ok", nil, time.Unix(3, 0).UTC())
			if err != nil {
				t.Fatal(err)
			}
			if first.Seq != 2 || second.Seq != 3 || second.PrevHash != first.EventHash {
				t.Fatalf("序号或哈希链错误: %+v %+v", first, second)
			}
			repeated, err := RecordSubagentCompleted(context.Background(), journal, "run-p", "ws", "run-c", "ok", nil, time.Unix(4, 0).UTC())
			if err != nil || repeated.EventID != second.EventID {
				t.Fatalf("重复通知不幂等: %+v %v", repeated, err)
			}
			if _, err := RecordSubagentCompleted(context.Background(), journal, "run-p", "ws", "run-c", "failed", nil, time.Unix(5, 0).UTC()); !errors.Is(err, ErrSubagentAppend) {
				t.Fatalf("完成结果被改写: %v", err)
			}
			if err := journal.Check(context.Background()); err != nil {
				t.Fatalf("日志不可回放: %v", err)
			}
			events, _ := journal.Load(context.Background(), "run-p")
			if len(events) != 3 {
				t.Fatalf("重复回执产生额外事实: %d", len(events))
			}
		})
	}
}

func TestRecordSubagentDelegatedFailsClosed(t *testing.T) {
	journal := memory.New()
	seedSubagentParent(t, journal)
	for _, test := range []struct {
		parent, workspace, declared, child string
		budget                             SubagentBudget
	}{
		{parent: "missing", workspace: "ws", declared: "missing", child: "c"},
		{parent: "run-p", workspace: "wrong", declared: "run-p", child: "c"},
		{parent: "run-p", workspace: "ws", declared: "wrong", child: "c"},
		{parent: "run-p", workspace: "ws", declared: "run-p", child: "run-p"},
		{parent: "run-p", workspace: "ws", declared: "run-p", child: "c", budget: SubagentBudget{MaxTurns: -1}},
	} {
		_, err := RecordSubagentDelegated(context.Background(), journal, domain.RunID(test.parent), test.workspace, DelegateOptions{ParentRunID: domain.RunID(test.declared), ChildRunID: domain.RunID(test.child), Task: "task", Budget: test.budget}, time.Unix(2, 0).UTC())
		if !errors.Is(err, ErrSubagentAppend) {
			t.Fatalf("非法归属未拒绝: %+v %v", test, err)
		}
	}
	if _, err := RecordSubagentCompleted(context.Background(), journal, "run-p", "ws", "unknown", "ok", nil, time.Unix(2, 0).UTC()); !errors.Is(err, ErrSubagentAppend) {
		t.Fatalf("未委派子运行被收束: %v", err)
	}
	if _, err := RecordSubagentDelegated(context.Background(), nil, "run-p", "ws", DelegateOptions{ParentRunID: "run-p", ChildRunID: "c", Task: "t"}, time.Unix(2, 0).UTC()); !errors.Is(err, ErrSubagentAppend) {
		t.Fatalf("空日志被接受: %v", err)
	}
	events, _ := journal.Load(context.Background(), "run-p")
	if len(events) != 1 {
		t.Fatalf("非法请求写入了事实: %d", len(events))
	}
	if err := journal.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type failingSubagentJournal struct {
	ports.Journal
	failure error
	calls   int
}

func (journal *failingSubagentJournal) Append(context.Context, uint64, domain.EventDraft) (domain.Event, error) {
	journal.calls++
	return domain.Event{}, journal.failure
}

func TestRecordSubagentAppendPreservesFailureWithoutRetry(t *testing.T) {
	base := memory.New()
	seedSubagentParent(t, base)
	cause := errors.New("injected append conflict")
	journal := &failingSubagentJournal{Journal: base, failure: cause}
	_, err := RecordSubagentDelegated(context.Background(), journal, "run-p", "ws", DelegateOptions{ParentRunID: "run-p", ChildRunID: "c", Task: "t"}, time.Unix(2, 0).UTC())
	if !errors.Is(err, ErrSubagentAppend) || !errors.Is(err, cause) || journal.calls != 1 {
		t.Fatalf("错误被吞或自动重试: %v calls=%d", err, journal.calls)
	}
}
