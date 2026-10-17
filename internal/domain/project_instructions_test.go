package domain

import (
	"encoding/json"
	"testing"
	"time"
)

// TestProjectInstructionsRunCreatedCompatibility 新引用可回放，旧创建事件保持合法，显式空值或非对象声明必须拒绝。
func TestProjectInstructionsRunCreatedCompatibility(t *testing.T) {
	for _, test := range []struct {
		name    string
		payload string
		valid   bool
	}{
		{"legacy", `{"task":"legacy","workspace":"."}`, true},
		{"snapshot", `{"task":"new","workspace":".","project_instructions_ref":{"sha256":"sha256:artifact","bytes":100,"media_type":"application/json"}}`, true},
		{"null", `{"task":"new","workspace":".","project_instructions_ref":null}`, false},
		{"string", `{"task":"new","workspace":".","project_instructions_ref":"artifact"}`, false},
		{"array", `{"task":"new","workspace":".","project_instructions_ref":[]}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			event, err := NewEvent(EventDraft{RunID: "run_project_instructions", WorkspaceHash: "workspace", Type: EventRunCreated, OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Payload: json.RawMessage(test.payload)}, 1, GenesisHash)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := Replay([]Event{event})
			if !test.valid {
				if err == nil {
					t.Fatal("invalid project instructions reference shape replayed")
				}
				return
			}
			if err != nil || projection.State != RunCreated {
				t.Fatalf("creation event no longer replays: %v", err)
			}
		})
	}
}
