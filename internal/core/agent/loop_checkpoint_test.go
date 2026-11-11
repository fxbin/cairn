package agent

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

func TestDecisionCheckpointFiresOnceWhenReadOnlyEvidenceAndNoPatch(t *testing.T) {
	readOperation := domain.OperationID("read-completed")
	events := []domain.Event{
		{Type: domain.EventTurnOpened},
		{Type: domain.EventToolCallProposed, OperationID: &readOperation, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"src/a.py"}}`)},
		{Type: domain.EventOperationSucceeded, OperationID: &readOperation},
		{Type: domain.EventToolResultAppended, OperationID: &readOperation, Payload: json.RawMessage(`{"ok":true}`)},
		{Type: domain.EventTurnOpened},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"src/b.py"}}`)},
		{Type: domain.EventTurnOpened},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"search_text","arguments":{"query":"x"}}`)},
		{Type: domain.EventTurnOpened},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"src/a.py"}}`)},
	}
	if hasApplyPatchProposal(events) {
		t.Fatal("must not see apply_patch")
	}
	if !hasReadFileEvidence(events) {
		t.Fatal("must see read_file")
	}
	if decisionCheckpointAlreadyFired(events) {
		t.Fatal("must not already be fired")
	}
	if openedTurnCount(events) < 4 {
		t.Fatalf("turns=%d", openedTurnCount(events))
	}
}

func TestDecisionCheckpointSkipsAfterPatchProposalOrAlreadyFired(t *testing.T) {
	withPatch := []domain.Event{
		{Type: domain.EventTurnOpened},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"apply_patch","arguments":{}}`)},
	}
	if !hasApplyPatchProposal(withPatch) {
		t.Fatal("patch proposal")
	}
	fired := []domain.Event{
		{Type: domain.EventVerificationFeedbackAppended, Payload: json.RawMessage(`{"text":"[决策检查点] x"}`)},
	}
	if !decisionCheckpointAlreadyFired(fired) {
		t.Fatal("already fired")
	}
}
