package memory

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

func TestAppendChecksExpectedSequenceAndReplay(t *testing.T) {
	journal := New()
	runID := domain.RunID("run_memory")
	created, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"x","workspace":"."}`)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err == nil {
		t.Fatal("expected sequence conflict")
	}
	if _, err := journal.Append(context.Background(), created.Seq, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.Replay(events); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestRunIDsReturnsStableSortedCopy(t *testing.T) {
	journal := New()
	ctx := context.Background()
	for _, runID := range []domain.RunID{"run_z", "run_a"} {
		if _, err := journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"x","workspace":"."}`)}); err != nil {
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
	runIDs[0] = "mutated"
	again, err := journal.RunIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != "run_a" {
		t.Fatalf("journal state was mutated: %v", again)
	}
}

func TestRunIDsHonorsCancellationWhileWaitingForLock(t *testing.T) {
	journal := New()
	journal.mu.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := journal.RunIDs(ctx)
		result <- err
	}()
	runtime.Gosched()
	select {
	case err := <-result:
		journal.mu.Unlock()
		t.Fatalf("RunIDs returned before lock release: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	cancel()
	journal.mu.Unlock()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected canceled context, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunIDs did not return after lock release")
	}
}
