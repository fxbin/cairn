package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestVerifyChainRejectsPayloadTampering(t *testing.T) {
	runID := RunID("run_test")
	first, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCreated, OccurredAt: time.Unix(1, 0), Payload: json.RawMessage(`{"task":"test","workspace":"."}`)}, 1, GenesisHash)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, OccurredAt: time.Unix(2, 0)}, 2, first.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyChain([]Event{first, second}); err != nil {
		t.Fatalf("verify valid chain: %v", err)
	}
	second.Payload = json.RawMessage(`{"payload_version":1,"invocation":"tampered"}`)
	if err := VerifyChain([]Event{first, second}); err == nil || !strings.Contains(err.Error(), "hash mismatch") {
		t.Fatalf("expected hash mismatch, got %v", err)
	}
}

func TestNewEventFreezesKnownPayloadSchemaVersion(t *testing.T) {
	runID := RunID("run_test")
	event, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted}, 1, GenesisHash)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["payload_version"]) != "1" {
		t.Fatalf("stored payload is missing version: %s", event.Payload)
	}
	if _, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, Payload: json.RawMessage(`{"payload_version":2}`)}, 1, GenesisHash); err == nil || !strings.Contains(err.Error(), "payload_version") {
		t.Fatalf("unsupported payload version was accepted: %v", err)
	}
	if _, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, Payload: json.RawMessage(`{"unexpected":true}`)}, 1, GenesisHash); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown payload field was accepted: %v", err)
	}
	if _, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventType("unknown_event")}, 1, GenesisHash); err == nil || !strings.Contains(err.Error(), "unknown event type") {
		t.Fatalf("unknown event type was accepted: %v", err)
	}
}

func TestNewEventAcceptsLoopGuardEvidenceFields(t *testing.T) {
	runID := RunID("run_test")
	turnID := TurnID("turn_test")
	if _, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventTurnClosed, TurnID: &turnID, Payload: json.RawMessage(`{"outcome":"continue","operation_signature":"sha256:operation","progress_fingerprint":"sha256:progress"}`)}, 1, GenesisHash); err != nil {
		t.Fatalf("turn guard evidence rejected: %v", err)
	}
	if _, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventVerificationFinished, TurnID: &turnID, Payload: json.RawMessage(`{"ok":false,"result_fingerprint":"sha256:result"}`)}, 1, GenesisHash); err != nil {
		t.Fatalf("verification guard evidence rejected: %v", err)
	}
}

// TestNewEventRejectsRemovedUnionPayloadFields 锁定封闭 schema：已删除的
// 联合语义字段（历史上从未有写入方）不得重新进入任何事件 payload。
func TestNewEventRejectsRemovedUnionPayloadFields(t *testing.T) {
	runID := RunID("run_test")
	turnID := TurnID("turn_test")
	removed := []struct {
		name    string
		draft   EventDraft
		payload string
	}{
		{
			name:    "run_cancel_requested.signal_or_budget_dimension",
			draft:   EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCancelRequested},
			payload: `{"cause":"budget_exhausted","signal_or_budget_dimension":"turns"}`,
		},
		{
			name:    "policy_decided.summary_or_reason",
			draft:   EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventPolicyDecided, TurnID: &turnID},
			payload: `{"effect":"allow","summary_or_reason":"ok"}`,
		},
		{
			name:    "operation_succeeded.exit_or_after_observation",
			draft:   EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventOperationSucceeded, TurnID: &turnID},
			payload: `{"tool_name":"exec_command","exit_or_after_observation":"sha256:obs"}`,
		},
	}
	for _, item := range removed {
		draft := item.draft
		draft.Payload = json.RawMessage(item.payload)
		if _, err := NewEvent(draft, 1, GenesisHash); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s was accepted after removal: %v", item.name, err)
		}
	}
}

func TestReplayRequiresClosedSuccessfulVerification(t *testing.T) {
	runID := RunID("run_test")
	turnID := TurnID("turn_test")
	created, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCreated, OccurredAt: time.Unix(1, 0), Payload: json.RawMessage(`{"task":"test","workspace":"."}`)}, 1, GenesisHash)
	started, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, OccurredAt: time.Unix(2, 0)}, 2, created.EventHash)
	opened, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventTurnOpened, TurnID: &turnID, OccurredAt: time.Unix(3, 0)}, 3, started.EventHash)
	finished, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventVerificationFinished, TurnID: &turnID, OccurredAt: time.Unix(4, 0), Payload: json.RawMessage(`{"ok":true}`)}, 4, opened.EventHash)
	closed, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventTurnClosed, TurnID: &turnID, OccurredAt: time.Unix(5, 0), Payload: json.RawMessage(`{"outcome":"final"}`)}, 5, finished.EventHash)
	completed, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCompleted, OccurredAt: time.Unix(6, 0), Payload: json.RawMessage(`{"final_response":"done"}`)}, 6, closed.EventHash)
	projection, err := Replay([]Event{created, started, opened, finished, closed, completed})
	if err != nil {
		t.Fatalf("replay valid terminal chain: %v", err)
	}
	if projection.State != RunCompleted {
		t.Fatalf("state = %s", projection.State)
	}
}

func TestReplayAcceptsContextCheckpointOnlyAtClosedTurnBoundary(t *testing.T) {
	runID := RunID("run_test")
	created, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCreated, OccurredAt: time.Unix(1, 0), Payload: json.RawMessage(`{"task":"test","workspace":"."}`)}, 1, GenesisHash)
	started, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, OccurredAt: time.Unix(2, 0)}, 2, created.EventHash)
	checkpoint, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventContextCompacted, OccurredAt: time.Unix(3, 0), Payload: json.RawMessage(`{"through_seq":2,"summary":"tool read_file succeeded","estimated_units_before":120,"estimated_units_after":40,"strategy":"deterministic_tool_output_checkpoint_v1"}`)}, 3, started.EventHash)
	projection, err := Replay([]Event{created, started, checkpoint})
	if err != nil {
		t.Fatalf("replay checkpoint: %v", err)
	}
	if projection.LastContextCompactSeq != 3 || projection.ContextCompactedThrough != 2 {
		t.Fatalf("projection = %#v", projection)
	}
	turnID := TurnID("turn_test")
	opened, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventTurnOpened, TurnID: &turnID, OccurredAt: time.Unix(4, 0)}, 4, checkpoint.EventHash)
	invalid, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventContextCompacted, OccurredAt: time.Unix(5, 0), Payload: json.RawMessage(`{"through_seq":4,"summary":"invalid","estimated_units_before":120,"estimated_units_after":40,"strategy":"deterministic_tool_output_checkpoint_v1"}`)}, 5, opened.EventHash)
	if _, err := Replay([]Event{created, started, checkpoint, opened, invalid}); err == nil || !strings.Contains(err.Error(), "context_compacted") {
		t.Fatalf("open-turn checkpoint was accepted: %v", err)
	}
}

func TestReplayRejectsOperationStartedBeforePolicyAuthorization(t *testing.T) {
	runID, turnID, operationID := RunID("run_test"), TurnID("turn_test"), OperationID("op_test")
	created, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCreated, OccurredAt: time.Unix(1, 0), Payload: json.RawMessage(`{"task":"test","workspace":"."}`)}, 1, GenesisHash)
	started, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, OccurredAt: time.Unix(2, 0)}, 2, created.EventHash)
	turn, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventTurnOpened, TurnID: &turnID, OccurredAt: time.Unix(3, 0)}, 3, started.EventHash)
	proposed, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, OccurredAt: time.Unix(4, 0), Payload: json.RawMessage(`{"call_id":"write","tool_name":"apply_patch","arguments":{}}`)}, 4, turn.EventHash)
	operationStarted, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventOperationStarted, TurnID: &turnID, OperationID: &operationID, OccurredAt: time.Unix(5, 0), Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)}, 5, proposed.EventHash)
	if _, err := Replay([]Event{created, started, turn, proposed, operationStarted}); err == nil || !strings.Contains(err.Error(), "authorized proposal") {
		t.Fatalf("unauthorized operation start was accepted: %v", err)
	}
}

func TestReplayRejectsTurnClosureBeforeModelToolResultIsAppended(t *testing.T) {
	runID, turnID, operationID := RunID("run_test"), TurnID("turn_test"), OperationID("op_test")
	created, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCreated, OccurredAt: time.Unix(1, 0), Payload: json.RawMessage(`{"task":"test","workspace":"."}`)}, 1, GenesisHash)
	started, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, OccurredAt: time.Unix(2, 0)}, 2, created.EventHash)
	turn, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventTurnOpened, TurnID: &turnID, OccurredAt: time.Unix(3, 0)}, 3, started.EventHash)
	proposed, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, OccurredAt: time.Unix(4, 0), Payload: json.RawMessage(`{"call_id":"read","tool_name":"read_file","arguments":{"path":"x.txt"}}`)}, 4, turn.EventHash)
	authorized, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, OccurredAt: time.Unix(5, 0), Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)}, 5, proposed.EventHash)
	operationStarted, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventOperationStarted, TurnID: &turnID, OperationID: &operationID, OccurredAt: time.Unix(6, 0), Payload: json.RawMessage(`{"tool_name":"read_file","side_effect_class":"none"}`)}, 6, authorized.EventHash)
	succeeded, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, OccurredAt: time.Unix(7, 0), Payload: json.RawMessage(`{"tool_name":"read_file","result":{"ok":true}}`)}, 7, operationStarted.EventHash)
	closed, _ := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventTurnClosed, TurnID: &turnID, OccurredAt: time.Unix(8, 0), Payload: json.RawMessage(`{"outcome":"continue"}`)}, 8, succeeded.EventHash)
	if _, err := Replay([]Event{created, started, turn, proposed, authorized, operationStarted, succeeded, closed}); err == nil || !strings.Contains(err.Error(), "tool result") {
		t.Fatalf("turn closed without its model tool result: %v", err)
	}
}
