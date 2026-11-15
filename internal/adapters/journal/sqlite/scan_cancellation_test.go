// [INPUT]: 真实 SQLite 行和可取消查询上下文。
// [OUTPUT]: 验证扫描取消保留错误身份，损坏内容仍失败关闭。
// [POS]: Journal 读取取消与持久化完整性的边界回归。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

func journalScanFixture(t *testing.T) (*Journal, domain.RunID) {
	t.Helper()
	journal, err := Open(filepath.Join(t.TempDir(), "journal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: []byte(`{"task":"read","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	return journal, runID
}

// TestScanJournalEventPreservesCancellation 在 Next 后、Scan 前取消真实查询，固定触发原竞态窗口。
func TestScanJournalEventPreservesCancellation(t *testing.T) {
	journal, runID := journalScanFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, err := journal.db.QueryContext(ctx, loadRunEventsSQL, runID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("missing row: %v", rows.Err())
	}
	cancel()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for rows.Err() == nil {
		select {
		case <-deadline.C:
			t.Fatal("database/sql did not observe cancellation")
		default:
			runtime.Gosched()
		}
	}
	if !errors.Is(rows.Err(), context.Canceled) {
		t.Fatalf("fixture cancellation=%v", rows.Err())
	}
	_, err = scanJournalEvent(rows)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrPersistedContent) {
		t.Fatalf("cancelled scan classified as content corruption: %v", err)
	}
}

// TestLoadStillRejectsMalformedPersistedRow 扫描转换和时间解码错误仍保持损坏哨兵。
func TestLoadStillRejectsMalformedPersistedRow(t *testing.T) {
	for _, mutation := range []string{"UPDATE events SET seq = 'invalid'", "UPDATE events SET occurred_at = 'invalid'"} {
		t.Run(mutation, func(t *testing.T) {
			journal, runID := journalScanFixture(t)
			if _, err := journal.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			events, err := journal.Load(context.Background(), runID)
			if !errors.Is(err, ErrPersistedContent) || len(events) != 0 {
				t.Fatalf("malformed row accepted: events=%v err=%v", events, err)
			}
		})
	}
}
