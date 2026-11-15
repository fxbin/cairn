// [INPUT]: 真实 SQLite 行、可取消查询与字段损坏负例。
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

// TestScanWorkflowEventPreservesCancellation 固定 Next 后、Scan 前的真实取消窗口。
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
