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
