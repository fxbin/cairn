// 工作流事件链与重放投影的合同测试。
package workflow

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// buildChain 依序为草稿赋号建链；任何负载不合法都会让测试失败。
func buildChain(t *testing.T, drafts ...EventDraft) []Event {
	t.Helper()
	events := make([]Event, 0, len(drafts))
	for _, draft := range drafts {
		previous := GenesisHash
		if len(events) > 0 {
			previous = events[len(events)-1].EventHash
		}
		event, err := NewEvent(draft, uint64(len(events))+1, previous)
		if err != nil {
			t.Fatalf("build workflow chain event %d: %v", len(events)+1, err)
		}
		events = append(events, event)
	}
	return events
}

// payload 将负载结构编码为事件负载。
func payload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode workflow payload: %v", err)
	}
	return encoded
}

// createdDraft 构造三步顺序计划（执行/验证/审批）的创建事件草稿。
func createdDraft(t *testing.T, id ID, workspaceHash string) EventDraft {
	return EventDraft{
		WorkflowID:    id,
		WorkspaceHash: workspaceHash,
		Type:          EventRunCreated,
		OccurredAt:    time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC),
		Payload: payload(t, RunCreatedPayload{
			Steps: []StepSpec{
				{ID: "prepare", Index: 0, Kind: StepKindExecute},
				{ID: "check", Index: 1, Kind: StepKindVerify},
				{ID: "accept", Index: 2, Kind: StepKindAccept},
			},
			PlanSHA256:          "sha256:plan",
			ConfigFingerprint:   "sha256:config",
			AuthorizationSource: "user-explicit",
		}),
	}
}

// startedDraft 构造步骤开始事件草稿。
func startedDraft(t *testing.T, id ID, workspaceHash, stepID string) EventDraft {
	return EventDraft{WorkflowID: id, WorkspaceHash: workspaceHash, Type: EventStepStarted,
		Payload: payload(t, StepStartedPayload{StepID: stepID, Input: StepIO{Ref: "artifact:in", SHA: "sha256:in"}})}
}

// finishedDraft 构造步骤收束事件草稿。
func finishedDraft(t *testing.T, id ID, workspaceHash, stepID string, state StepState, errorClass string) EventDraft {
	return EventDraft{WorkflowID: id, WorkspaceHash: workspaceHash, Type: EventStepFinished,
		Payload: payload(t, StepFinishedPayload{StepID: stepID, State: state, ErrorClass: errorClass, Output: StepIO{Ref: "artifact:out", SHA: "sha256:out"}})}
}

func TestProjectReplaysSequentialExecution(t *testing.T) {
	id := NewID()
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		startedDraft(t, id, "hash-workspace", "prepare"),
		finishedDraft(t, id, "hash-workspace", "prepare", StepSucceeded, ""),
		startedDraft(t, id, "hash-workspace", "check"),
	)
	snapshot, err := Project(events)
	if err != nil {
		t.Fatalf("project workflow: %v", err)
	}
	if snapshot.WorkflowID != id || snapshot.ConfigFingerprint != "sha256:config" || snapshot.PlanSHA256 != "sha256:plan" {
		t.Fatalf("workflow identity=%#v", snapshot)
	}
	if snapshot.Finished {
		t.Fatal("workflow must not be finished")
	}
	if len(snapshot.Steps) != 3 || snapshot.Steps[0].State != StepSucceeded || snapshot.Steps[1].State != StepRunning || snapshot.Steps[2].State != StepPending {
		t.Fatalf("steps=%#v", snapshot.Steps)
	}
	if snapshot.Kinds["check"] != StepKindVerify {
		t.Fatalf("kinds=%#v", snapshot.Kinds)
	}
	if snapshot.ResumeIndex != 1 {
		t.Fatalf("resume index=%d want 1", snapshot.ResumeIndex)
	}
	if snapshot.Steps[0].Output.Ref != "artifact:out" || snapshot.Steps[1].Input.Ref != "artifact:in" {
		t.Fatalf("step io not replayed: %#v", snapshot.Steps)
	}
}

func TestProjectRejectsStartBeforePredecessorSucceeds(t *testing.T) {
	id := NewID()
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		startedDraft(t, id, "hash-workspace", "prepare"),
		startedDraft(t, id, "hash-workspace", "check"),
	)
	if _, err := Project(events); err == nil || !strings.Contains(err.Error(), "cannot start before") {
		t.Fatalf("want predecessor violation, got %v", err)
	}
}

func TestProjectRejectsConcurrentStepStart(t *testing.T) {
	id := NewID()
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		startedDraft(t, id, "hash-workspace", "prepare"),
		finishedDraft(t, id, "hash-workspace", "prepare", StepSucceeded, ""),
		startedDraft(t, id, "hash-workspace", "check"),
		startedDraft(t, id, "hash-workspace", "accept"),
	)
	if _, err := Project(events); err == nil || !strings.Contains(err.Error(), "cannot start") {
		t.Fatalf("want sequential violation, got %v", err)
	}
}

func TestProjectRejectsFinishWithoutStart(t *testing.T) {
	id := NewID()
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		finishedDraft(t, id, "hash-workspace", "prepare", StepSucceeded, ""),
	)
	if _, err := Project(events); err == nil {
		t.Fatal("pending→succeeded must be rejected")
	}
}

func TestProjectUncertainBlocksResumeAndRequiresReconcile(t *testing.T) {
	id := NewID()
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		startedDraft(t, id, "hash-workspace", "prepare"),
		finishedDraft(t, id, "hash-workspace", "prepare", StepUncertain, "side_effect_unknown"),
		startedDraft(t, id, "hash-workspace", "prepare"),
	)
	if _, err := Project(events); err == nil {
		t.Fatal("uncertain step must not restart without reconciliation")
	}
	withoutRestart := events[:3]
	snapshot, err := Project(withoutRestart)
	if err != nil {
		t.Fatalf("project uncertain workflow: %v", err)
	}
	if len(snapshot.RequiresReconcile) != 1 || snapshot.RequiresReconcile[0] != "prepare" {
		t.Fatalf("reconcile=%v", snapshot.RequiresReconcile)
	}
}

func TestProjectFinishesRunOnce(t *testing.T) {
	id := NewID()
	finish := EventDraft{WorkflowID: id, WorkspaceHash: "hash-workspace", Type: EventRunFinished,
		Payload: payload(t, RunFinishedPayload{State: RunStateCompleted, ResumeIndex: -1})}
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		startedDraft(t, id, "hash-workspace", "prepare"),
		finishedDraft(t, id, "hash-workspace", "prepare", StepSucceeded, ""),
		finish,
		finish,
	)
	if _, err := Project(events); err == nil || !strings.Contains(err.Error(), "duplicate workflow_run_finished") {
		t.Fatalf("want duplicate finish rejection, got %v", err)
	}
}

func TestProjectRejectsMissingCreatedFirst(t *testing.T) {
	id := NewID()
	events := buildChain(t, startedDraft(t, id, "hash-workspace", "prepare"))
	if _, err := Project(events); err == nil || !strings.Contains(err.Error(), "workflow_run_created") {
		t.Fatalf("want created-first rejection, got %v", err)
	}
}

func TestVerifyChainDetectsPayloadTampering(t *testing.T) {
	id := NewID()
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		startedDraft(t, id, "hash-workspace", "prepare"),
	)
	tampered := append([]Event(nil), events...)
	tampered[1].Payload = payload(t, StepStartedPayload{StepID: "check"})
	if err := VerifyChain(tampered); err == nil {
		t.Fatal("tampered payload must break the chain")
	}
	if _, err := Project(tampered); err == nil {
		t.Fatal("projection must reject tampered chain")
	}
}

func TestVerifyChainRejectsSequenceGap(t *testing.T) {
	id := NewID()
	events := buildChain(t,
		createdDraft(t, id, "hash-workspace"),
		startedDraft(t, id, "hash-workspace", "prepare"),
	)
	events[1].Seq = 3
	if err := VerifyChain(events); err == nil || !strings.Contains(err.Error(), "sequence mismatch") {
		t.Fatalf("want sequence rejection, got %v", err)
	}
}

func TestValidatePayloadRejectsInvalidCreatedContent(t *testing.T) {
	cases := map[string]RunCreatedPayload{
		"空步骤":  {PlanSHA256: "s", ConfigFingerprint: "c", AuthorizationSource: "u"},
		"未知种类": {Steps: []StepSpec{{ID: "a", Index: 0, Kind: "custom"}}, PlanSHA256: "s", ConfigFingerprint: "c", AuthorizationSource: "u"},
		"索引错位": {Steps: []StepSpec{{ID: "a", Index: 1, Kind: StepKindExecute}}, PlanSHA256: "s", ConfigFingerprint: "c", AuthorizationSource: "u"},
		"缺指纹":  {Steps: []StepSpec{{ID: "a", Index: 0, Kind: StepKindExecute}}, PlanSHA256: "s", AuthorizationSource: "u"},
	}
	for name, value := range cases {
		if err := validatePayload(EventRunCreated, payload(t, value)); err == nil {
			t.Fatalf("%s must be rejected", name)
		}
	}
	if err := validatePayload(EventType("workflow_unknown"), payload(t, map[string]string{})); err == nil {
		t.Fatal("unknown event type must be rejected")
	}
	var errConflict error = ErrConflict
	if !errors.Is(errConflict, ErrConflict) {
		t.Fatal("conflict sentinel identity")
	}
}
