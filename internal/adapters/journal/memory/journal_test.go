package memory

import (
	"context"
	"encoding/json"
	"testing"

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
