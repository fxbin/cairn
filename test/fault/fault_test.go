//go:build faultinject

// Package fault covers replay outcomes at the persistence boundaries where a
// 该套件按 AGENTS.md 的验证流程显式启用：go test ./test/fault -tags=faultinject。
// process can die between a committed operation start and its terminal event.
package fault

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

func TestFaultInjectStartedWorkspaceOperationRequiresReconciliation(t *testing.T) {
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	created, err := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"fault","workspace":"/workspace"}`)}, 1, domain.GenesisHash)
	if err != nil {
		t.Fatal(err)
	}
	started, err := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}, 2, created.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	turn, err := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID}, 3, started.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	proposed, err := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"patch","tool_name":"apply_patch","arguments":{"path":"x.txt","mode":"create","content":"x"}}`)}, 4, turn.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	authorized, err := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)}, 5, proposed.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	operationStarted, err := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"x.txt","planned_before":"absent","planned_after":"sha256:after"}`)}, 6, authorized.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := domain.Replay([]domain.Event{created, started, turn, proposed, authorized, operationStarted})
	if err != nil || projection.Phase() != "waiting_reconcile" || projection.PendingOperation == nil {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
}

func TestFaultInjectStartedReadOperationCanRemainRunnable(t *testing.T) {
	runID, turnID, operationID := domain.NewRunID(), domain.NewTurnID(), domain.NewOperationID()
	created, _ := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"fault","workspace":"/workspace"}`)}, 1, domain.GenesisHash)
	started, _ := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted}, 2, created.EventHash)
	turn, _ := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID}, 3, started.EventHash)
	proposed, _ := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"read","tool_name":"read_file","arguments":{"path":"x.txt"}}`)}, 4, turn.EventHash)
	authorized, _ := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)}, 5, proposed.EventHash)
	operationStarted, _ := domain.NewEvent(domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"read_file","side_effect_class":"none"}`)}, 6, authorized.EventHash)
	projection, err := domain.Replay([]domain.Event{created, started, turn, proposed, authorized, operationStarted})
	if err != nil || projection.Phase() != "running" || projection.PendingOperation == nil {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}
}
