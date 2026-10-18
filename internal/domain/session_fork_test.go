// session_fork_test 覆盖跨会话分支锚点的事件契约与回放投影：fork_source 与
// prev_run_id 互斥、锚点字段完整、Replay 如实投影来源。
//
// Author: fxbin
package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestRunCreatedRejectsForkSourceWithPrevRun 锁定跨会话来源边只允许出现在
// 新分支根，不得与同 session 前驱同时携带。
func TestRunCreatedRejectsForkSourceWithPrevRun(t *testing.T) {
	payload := `{"payload_version":1,"task":"t","workspace":".","prev_run_id":"run_prev","fork_source":{"run_id":"run_src","workspace_hash":"sha256:abc","last_seq":3,"last_event_hash":"sha256:def"}}`
	if err := validatePayload(EventRunCreated, json.RawMessage(payload)); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("fork_source+prev_run_id error = %v, want mutually exclusive", err)
	}
}

// TestRunCreatedValidatesForkSourceAnchor 锁定锚点字段必须完整绑定来源 run、
// 工作区身份与事件边界。
func TestRunCreatedValidatesForkSourceAnchor(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		want    string
	}{
		{name: "缺 run_id", payload: `{"payload_version":1,"task":"t","workspace":".","fork_source":{"workspace_hash":"sha256:abc","last_seq":1,"last_event_hash":"sha256:def"}}`, want: "run_id"},
		{name: "缺 workspace_hash", payload: `{"payload_version":1,"task":"t","workspace":".","fork_source":{"run_id":"run_src","last_seq":1,"last_event_hash":"sha256:def"}}`, want: "workspace_hash"},
		{name: "last_seq 为零", payload: `{"payload_version":1,"task":"t","workspace":".","fork_source":{"run_id":"run_src","workspace_hash":"sha256:abc","last_seq":0,"last_event_hash":"sha256:def"}}`, want: "last_seq"},
		{name: "缺 last_event_hash", payload: `{"payload_version":1,"task":"t","workspace":".","fork_source":{"run_id":"run_src","workspace_hash":"sha256:abc","last_seq":1}}`, want: "last_event_hash"},
		{name: "null 锚点", payload: `{"payload_version":1,"task":"t","workspace":".","fork_source":null}`, want: "fork_source"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validatePayload(EventRunCreated, json.RawMessage(test.payload)); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

// TestRunCreatedAcceptsValidForkSource 锁定完整锚点可写入新分支根。
func TestRunCreatedAcceptsValidForkSource(t *testing.T) {
	payload := `{"payload_version":1,"task":"t","workspace":".","fork_source":{"run_id":"run_src","workspace_hash":"sha256:abc","last_seq":3,"last_event_hash":"sha256:def"}}`
	if err := validatePayload(EventRunCreated, json.RawMessage(payload)); err != nil {
		t.Fatal(err)
	}
	event, err := NewEvent(EventDraft{RunID: "run-new", WorkspaceHash: "workspace", Type: EventRunCreated, Payload: json.RawMessage(payload)}, 1, GenesisHash)
	if err != nil {
		t.Fatal(err)
	}
	var created RunCreatedPayload
	if err := json.Unmarshal(event.Payload, &created); err != nil {
		t.Fatal(err)
	}
	if created.ForkSource == nil || created.ForkSource.RunID != "run_src" || created.ForkSource.LastSeq != 3 {
		t.Fatalf("fork source projection = %#v", created.ForkSource)
	}
}

// TestReplayProjectsForkSource 锁定回放把 fork_source 投影到运行状态，且与
// prev_run_id 互斥的根运行不携带前驱。
func TestReplayProjectsForkSource(t *testing.T) {
	runID := RunID("run-fork-root")
	payload := `{"task":"branch","workspace":".","session_id":"session-b","fork_source":{"run_id":"run-a2","workspace_hash":"sha256:abc","last_seq":4,"last_event_hash":"sha256:def"}}`
	created, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunCreated, OccurredAt: time.Unix(1, 0), Payload: json.RawMessage(payload)}, 1, GenesisHash)
	if err != nil {
		t.Fatal(err)
	}
	started, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunStarted, OccurredAt: time.Unix(2, 0)}, 2, created.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	failed, err := NewEvent(EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: EventRunFailed, OccurredAt: time.Unix(3, 0), Payload: json.RawMessage(`{"error":"fixture"}`)}, 3, started.EventHash)
	if err != nil {
		t.Fatal(err)
	}
	projection, err := Replay([]Event{created, started, failed})
	if err != nil {
		t.Fatal(err)
	}
	if projection.ForkSource == nil || projection.ForkSource.RunID != "run-a2" {
		t.Fatalf("projection.ForkSource = %#v", projection.ForkSource)
	}
	if projection.PrevRunID != "" || projection.SessionID != "session-b" {
		t.Fatalf("projection session edge = session %q prev %q", projection.SessionID, projection.PrevRunID)
	}
}
