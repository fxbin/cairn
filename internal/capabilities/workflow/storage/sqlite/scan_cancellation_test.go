// [INPUT]: 真实SQLite行、观察取消替身与损坏负例。
// [OUTPUT]: 验证扫描取消保留错误身份，损坏内容仍失败关闭。
// [POS]: 工作流存储的读取取消与完整性边界回归。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
)

// workflowScanFixture 创建真实事件行，不替换数据库或扫描实现。
func workflowScanFixture(t *testing.T) (*Store, workflow.ID) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	id := workflow.NewID()
	appendCreated(t, store, id, "workspace")
	return store, id
}

// TestScanWorkflowEventPreservesCancellation 在 Next 后观察真实取消与截止时间。
func TestScanWorkflowEventPreservesCancellation(t *testing.T) {
	const (
		queryDeadline    = 100 * time.Millisecond
		cancellationWait = 5 * time.Second
	)
	for _, deadline := range []bool{false, true} {
		name := "cancelled"
		if deadline {
			name = "deadline"
		}
		t.Run(name, func(t *testing.T) {
			store, id := workflowScanFixture(t)
			var ctx context.Context
			var cancel context.CancelFunc
			want := context.Canceled
			if deadline {
				ctx, cancel = context.WithTimeout(context.Background(), queryDeadline)
				want = context.DeadlineExceeded
			} else {
				ctx, cancel = context.WithCancel(context.Background())
			}
			defer cancel()
			rows, err := store.db.QueryContext(ctx, loadWorkflowEventsSQL, id)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("missing row: %v", rows.Err())
			}
			if !deadline {
				cancel()
			}
			timer := time.NewTimer(cancellationWait)
			defer timer.Stop()
			for rows.Err() == nil {
				select {
				case <-timer.C:
					t.Fatal("database/sql did not observe cancellation")
				default:
					runtime.Gosched()
				}
			}
			if !errors.Is(rows.Err(), want) {
				t.Fatalf("fixture cancellation=%v want %v", rows.Err(), want)
			}
			_, err = scanWorkflowEvent(rows)
			if !errors.Is(err, want) || errors.Is(err, ErrPersistedContent) || IsStateCorruptError(err) {
				t.Fatalf("cancelled scan classified as content corruption: %v", err)
			}
		})
	}
}

// TestLoadStillRejectsMalformedWorkflowRow 保留字段转换和时间解码的损坏分类。
func TestLoadStillRejectsMalformedWorkflowRow(t *testing.T) {
	for _, mutation := range []string{"UPDATE workflow_events SET seq = 'invalid'", "UPDATE workflow_events SET occurred_at = 'invalid'"} {
		t.Run(mutation, func(t *testing.T) {
			store, id := workflowScanFixture(t)
			if _, err := store.db.Exec(mutation); err != nil {
				t.Fatal(err)
			}
			events, err := store.Load(context.Background(), id)
			if !errors.Is(err, ErrPersistedContent) || !IsStateCorruptError(err) || len(events) != 0 {
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

// TestScanWorkflowEventHonorsObservedCancellation 锁定观察取消后不再扫描缓冲行。
func TestScanWorkflowEventHonorsObservedCancellation(t *testing.T) {
	for _, want := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(want.Error(), func(t *testing.T) {
			row := &observedCancellationRow{cancellation: errors.Join(errors.New("observed cancellation"), want)}
			_, err := scanWorkflowEvent(row)
			if !errors.Is(err, want) || errors.Is(err, ErrPersistedContent) {
				t.Fatalf("observed cancellation lost identity: %v", err)
			}
			if row.scanned {
				t.Fatal("observed cancellation still scanned a buffered row")
			}
		})
	}
}
