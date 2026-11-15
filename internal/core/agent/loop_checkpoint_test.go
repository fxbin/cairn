// [INPUT]: Journal、基础端口与显式能力的固定生命周期提议。
// [OUTPUT]: 验证既有执行、恢复与最小装配合同。
// [POS]: Core 的接口迁移回归，保留原安全断言。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

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
	if (&Loop{}).openedTurnCount(events) < 4 {
		t.Fatalf("turns=%d", (&Loop{}).openedTurnCount(events))
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
