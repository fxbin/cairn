package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

func TestJournalPersistsAndReplaysEvents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "runs.db")
	journal, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	runID := domain.NewRunID()
	_, err = journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: []byte(`{"task":"answer","workspace":"/tmp/workspace"}`)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d", len(events))
	}
	projection, err := domain.Replay(events)
	if err != nil || projection.State != domain.RunRunning {
		t.Fatalf("replay = %#v, %v", projection, err)
	}
	if err := journal.Check(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestJournalRejectsConflictingExpectedSequence(t *testing.T) {
	journal, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	_, err = journal.Append(context.Background(), 1, domain.EventDraft{RunID: domain.NewRunID(), WorkspaceHash: "workspace", Type: domain.EventRunCreated})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestRunIDsReturnsSortedRuns(t *testing.T) {
	journal, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	ctx := context.Background()
	for _, runID := range []domain.RunID{"run_z", "run_a"} {
		if _, err := journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: []byte(`{"task":"x","workspace":"."}`)}); err != nil {
			t.Fatal(err)
		}
	}
	runIDs, err := journal.RunIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(runIDs) != 2 || runIDs[0] != "run_a" || runIDs[1] != "run_z" {
		t.Fatalf("run IDs = %v", runIDs)
	}
}

func TestRunIDsHonorsCanceledContext(t *testing.T) {
	journal, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := journal.RunIDs(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled context, got %v", err)
	}
}

func TestCheckRejectsReplayInvalidRun(t *testing.T) {
	journal, err := Open(filepath.Join(t.TempDir(), "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	ctx := context.Background()
	runID := domain.RunID("run_mixed_workspace")
	if _, err := journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: []byte(`{"task":"x","workspace":"."}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: "other-workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	if err := journal.Check(ctx); err == nil || !strings.Contains(err.Error(), "event workspace hash") {
		t.Fatalf("expected replay integrity error, got %v", err)
	}
}
