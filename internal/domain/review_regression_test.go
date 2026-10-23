package domain

import (
	"encoding/json"
	"testing"
)

func TestReviewValidateDraftDoesNotMutateChildren(t *testing.T) {
	projection := RunProjection{ID: "parent", WorkspaceHash: "ws", State: RunRunning, LastSeq: 1, Children: map[string]SubagentRecord{"child": {ChildRunID: "child", State: "delegated"}}}
	err := projection.ValidateDraft(EventDraft{RunID: "parent", WorkspaceHash: "ws", Type: EventSubagentCompleted, Payload: json.RawMessage(`{"child_run_id":"child","outcome":"ok"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if projection.Children["child"].State != "delegated" || projection.LastSeq != 1 {
		t.Fatalf("validation mutated live projection: %+v", projection.Children)
	}
}

func TestReviewParentIdentitySurvivesReplay(t *testing.T) {
	created, err := NewEvent(EventDraft{RunID: "child", WorkspaceHash: "ws", Type: EventRunCreated, Payload: json.RawMessage(`{"task":"t","workspace":".","parent_run_id":"parent"}`)}, 1, GenesisHash)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Replay([]Event{created})
	if err != nil {
		t.Fatal(err)
	}
	// 检查投影对读取方的序列化合同；旧事件仍允许省略父身份。
	encoded, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	var view struct{ ParentRunID string }
	if err := json.Unmarshal(encoded, &view); err != nil {
		t.Fatal(err)
	}
	if view.ParentRunID != "parent" {
		t.Fatalf("parent identity lost in replay: %s", encoded)
	}
}
