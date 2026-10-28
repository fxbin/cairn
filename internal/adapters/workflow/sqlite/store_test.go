// 工作流 SQLite 存储的合同测试：追加 CAS、往返读取、只读打开与完整性校验。
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/fxbin/cairn/internal/workflow"
)

// appendCreated 追加创建事件并返回新链的事件列表。
func appendCreated(t *testing.T, store *Store, id workflow.ID, workspaceHash string) []workflow.Event {
	t.Helper()
	draft := workflow.EventDraft{WorkflowID: id, WorkspaceHash: workspaceHash, Type: workflow.EventRunCreated,
		Payload: mustPayload(t, workflow.RunCreatedPayload{
			Steps:               []workflow.StepSpec{{ID: "prepare", Index: 0, Kind: workflow.StepKindExecute}},
			PlanSHA256:          "sha256:plan",
			ConfigFingerprint:   "sha256:config",
			AuthorizationSource: "user-explicit",
		})}
	event, err := store.Append(context.Background(), 0, draft)
	if err != nil {
		t.Fatalf("append created: %v", err)
	}
	return []workflow.Event{event}
}

// mustPayload 编码负载并在失败时终止测试。
func mustPayload(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode payload: %v", err)
	}
	return encoded
}

func TestStoreAppendLoadRoundtrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	events := appendCreated(t, store, id, "hash-workspace")
	started, err := store.Append(context.Background(), uint64(len(events)), workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: "hash-workspace", Type: workflow.EventStepStarted,
		Payload: mustPayload(t, workflow.StepStartedPayload{StepID: "prepare"})})
	if err != nil {
		t.Fatalf("append started: %v", err)
	}
	events = append(events, started)
	loaded, err := store.Load(context.Background(), id)
	if err != nil {
		t.Fatalf("load workflow: %v", err)
	}
	if len(loaded) != 2 || loaded[0].Type != workflow.EventRunCreated || loaded[1].Type != workflow.EventStepStarted {
		t.Fatalf("loaded=%#v", loaded)
	}
	if err := workflow.VerifyChain(loaded); err != nil {
		t.Fatalf("verify loaded chain: %v", err)
	}
	if !loaded[0].OccurredAt.Equal(events[0].OccurredAt) {
		t.Fatalf("occurred_at roundtrip drifted: %v vs %v", loaded[0].OccurredAt, events[0].OccurredAt)
	}
	ids, err := store.WorkflowIDs(context.Background(), "hash-workspace")
	if err != nil || len(ids) != 1 || ids[0] != id {
		t.Fatalf("workflow ids=%v err=%v", ids, err)
	}
	if ids, err := store.WorkflowIDs(context.Background(), "hash-other"); err != nil || len(ids) != 0 {
		t.Fatalf("other workspace ids=%v err=%v", ids, err)
	}
}

func TestStoreAppendDetectsConflict(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	id := workflow.NewID()
	appendCreated(t, store, id, "hash-workspace")
	_, err = store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: "hash-workspace", Type: workflow.EventStepStarted,
		Payload: mustPayload(t, workflow.StepStartedPayload{StepID: "prepare"})})
	if !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("want conflict sentinel, got %v", err)
	}
}

func TestStoreReadOnlyRequiresExistingFile(t *testing.T) {
	if _, err := OpenReadOnly(filepath.Join(t.TempDir(), "absent.db")); err == nil {
		t.Fatal("read-only open must reject missing store")
	}
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	id := workflow.NewID()
	appendCreated(t, store, id, "hash-workspace")
	if err := store.Close(); err != nil {
		t.Fatalf("close writable store: %v", err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatalf("open read-only: %v", err)
	}
	defer readOnly.Close()
	loaded, err := readOnly.Load(context.Background(), id)
	if err != nil || len(loaded) != 1 {
		t.Fatalf("read-only load=%d err=%v", len(loaded), err)
	}
}

func TestStoreCheckDetectsTamperedRow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	id := workflow.NewID()
	appendCreated(t, store, id, "hash-workspace")
	started, err := store.Append(context.Background(), 1, workflow.EventDraft{
		WorkflowID: id, WorkspaceHash: "hash-workspace", Type: workflow.EventStepStarted,
		Payload: mustPayload(t, workflow.StepStartedPayload{StepID: "prepare"})})
	if err != nil {
		t.Fatalf("append started: %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("check before tampering: %v", err)
	}
	tamper, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open tamper connection: %v", err)
	}
	defer tamper.Close()
	if _, err := tamper.Exec("UPDATE workflow_events SET payload_json = ? WHERE workflow_id = ? AND seq = 2",
		mustPayload(t, workflow.StepStartedPayload{StepID: "check"}), id); err != nil {
		t.Fatalf("tamper payload: %v", err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("check must detect tampered row")
	}
	if started.Seq != 2 {
		t.Fatalf("started seq=%d", started.Seq)
	}
}

func TestStoreReopenMigrationIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "workflow.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	store.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen workflow store: %v", err)
	}
	defer reopened.Close()
	if err := reopened.Check(context.Background()); err != nil {
		t.Fatalf("check reopened store: %v", err)
	}
}

func TestStoreRejectsInvalidDraftBeforeWriting(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "workflow.db"))
	if err != nil {
		t.Fatalf("open workflow store: %v", err)
	}
	defer store.Close()
	if _, err := store.Append(context.Background(), 0, workflow.EventDraft{
		WorkflowID: workflow.NewID(), WorkspaceHash: "hash-workspace", Type: workflow.EventRunFinished,
		Payload: mustPayload(t, workflow.RunFinishedPayload{State: "mysterious"})}); err == nil {
		t.Fatal("invalid terminal state must fail closed")
	}
}
