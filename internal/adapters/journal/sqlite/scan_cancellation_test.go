// [INPUT]: 真实SQLite行、观察取消替身与可取消查询。
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

// TestScanJournalEventPreservesCancellation 在 Next 后取消真实查询，验证已观察取消的身份。
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

// observedCancellationRow 固定 Err 已可见、Scan 仍能读取缓冲行的标准库窗口。
type observedCancellationRow struct {
	cancellation error
	scanned      bool
}

// Err 返回已经观察到的取消身份。
func (row *observedCancellationRow) Err() error {
	return row.cancellation
}

// Scan 保留关闭发生前仍可成功解码的时间字段，并记录是否触碰当前行。
func (row *observedCancellationRow) Scan(dest ...any) error {
	row.scanned = true
	*dest[5].(*string) = time.Unix(1_700_000_000, 0).UTC().Format(time.RFC3339Nano)
	return nil
}

// TestScanJournalEventHonorsObservedCancellation 锁定观察取消后不再扫描缓冲行。
func TestScanJournalEventHonorsObservedCancellation(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			row := &observedCancellationRow{cancellation: errors.Join(errors.New("observed cancellation"), want)}
			_, err := scanJournalEvent(row)
			if !errors.Is(err, want) || errors.Is(err, ErrPersistedContent) {
				t.Fatalf("observed cancellation lost identity: %v", err)
			}
			if row.scanned {
				t.Fatal("observed cancellation still scanned a buffered row")
			}
		})
	}
}
