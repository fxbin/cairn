package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// eventChain 以递增序号与哈希链构造可回放事件序列，供迁移矩阵测试复用。
type eventChain struct {
	runID  RunID
	events []Event
}

func newEventChain(t *testing.T) *eventChain {
	t.Helper()
	chain := &eventChain{runID: RunID("run_matrix")}
	chain.add(t, EventDraft{Type: EventRunCreated, Payload: json.RawMessage(`{"task":"matrix","workspace":"."}`)})
	chain.add(t, EventDraft{Type: EventRunStarted})
	return chain
}

// add 追加一个事件并维护 seq 与 prevHash 链；构造失败立即终止测试。
func (chain *eventChain) add(t *testing.T, draft EventDraft) Event {
	t.Helper()
	draft.RunID = chain.runID
	draft.WorkspaceHash = "workspace"
	if draft.OccurredAt.IsZero() {
		draft.OccurredAt = time.Unix(int64(len(chain.events))+1, 0)
	}
	prevHash := GenesisHash
	seq := uint64(1)
	if last := len(chain.events) - 1; last >= 0 {
		prevHash = chain.events[last].EventHash
		seq = chain.events[last].Seq + 1
	}
	event, err := NewEvent(draft, seq, prevHash)
	if err != nil {
		t.Fatalf("build %s event: %v", draft.Type, err)
	}
	chain.events = append(chain.events, event)
	return event
}

// replay 断言当前序列可完整回放并返回投影。
func (chain *eventChain) replay(t *testing.T) RunProjection {
	t.Helper()
	projection, err := Replay(chain.events)
	if err != nil {
		t.Fatalf("replay chain: %v", err)
	}
	return projection
}

// replayReject 断言当前序列回放失败且错误包含 want。
func (chain *eventChain) replayReject(t *testing.T, want string) {
	t.Helper()
	_, err := Replay(chain.events)
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("expected replay error containing %q, got %v", want, err)
	}
}

// runFailedMatrixFixture 构造到达指定 from-state 的最小序列。
func runFailedMatrixFixture(t *testing.T, state RunState) *eventChain {
	t.Helper()
	chain := newEventChain(t)
	turnID := TurnID("turn_matrix")
	switch state {
	case RunCreated:
		chain.events = chain.events[:1]
	case RunRunning:
	case RunCancelling:
		chain.add(t, EventDraft{Type: EventRunCancelRequested, Payload: json.RawMessage(`{"cause":"user_interrupt"}`)})
	case RunFailed:
		chain.add(t, EventDraft{Type: EventRunFailed, Payload: json.RawMessage(`{"error":"first"}`)})
	case RunCompleted:
		chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
		chain.add(t, EventDraft{Type: EventVerificationFinished, TurnID: &turnID, Payload: json.RawMessage(`{"ok":true}`)})
		chain.add(t, EventDraft{Type: EventTurnClosed, TurnID: &turnID, Payload: json.RawMessage(`{"outcome":"final"}`)})
		chain.add(t, EventDraft{Type: EventRunCompleted, Payload: json.RawMessage(`{"final_response":"done"}`)})
	case RunCancelled:
		chain.add(t, EventDraft{Type: EventRunCancelRequested, Payload: json.RawMessage(`{"cause":"user_interrupt"}`)})
		chain.add(t, EventDraft{Type: EventRunCancelled, Payload: json.RawMessage(`{"cause":"user_interrupt"}`)})
	}
	return chain
}

// TestRunFailedTransitionMatrix 锁定 run_failed 的完整 from-state 矩阵：
// created/running/cancelling 合法（Abort 与预算路径依赖），终态及 failed 自身拒绝。
func TestRunFailedTransitionMatrix(t *testing.T) {
	allowed := []RunState{RunCreated, RunRunning, RunCancelling}
	for _, state := range allowed {
		chain := runFailedMatrixFixture(t, state)
		chain.add(t, EventDraft{Type: EventRunFailed, Payload: json.RawMessage(`{"error":"boom"}`)})
		if projection := chain.replay(t); projection.State != RunFailed {
			t.Fatalf("run_failed from %s: state = %s", state, projection.State)
		}
	}
	rejected := []RunState{RunFailed, RunCompleted, RunCancelled}
	for _, state := range rejected {
		chain := runFailedMatrixFixture(t, state)
		chain.add(t, EventDraft{Type: EventRunFailed, Payload: json.RawMessage(`{"error":"again"}`)})
		chain.replayReject(t, "run_failed from terminal state")
	}
}

// TestReplayPreservesOptionalRunIdentityAndObservation 锁定新 Run 身份在 replay
// 后可供 inspect 使用，旧事件缺少这些字段时仍维持已有状态机兼容性。
func TestReplayPreservesOptionalRunIdentityAndObservation(t *testing.T) {
	chain := &eventChain{runID: RunID("run_identity")}
	identity := BuildIdentity{Revision: "abc", Dirty: "true", Flavor: "plain"}
	identity.Fingerprint = identity.ComputeFingerprint()
	payload, err := json.Marshal(RunCreatedPayload{Task: "identity", Workspace: ".", ConfigFingerprint: "sha256:config", Build: &identity})
	if err != nil {
		t.Fatal(err)
	}
	chain.add(t, EventDraft{Type: EventRunCreated, Payload: payload})
	chain.add(t, EventDraft{Type: EventRunStarted})
	chain.add(t, EventDraft{Type: EventVerificationStarted, Payload: json.RawMessage(`{"workspace_observation":"sha256:observation"}`)})
	projection := chain.replay(t)
	if projection.ConfigFingerprint != "sha256:config" || projection.Build == nil || projection.Build.Revision != "abc" || projection.Build.Dirty != "true" || projection.Build.Flavor != "plain" || projection.WorkspaceObservation != "sha256:observation" {
		t.Fatalf("projection = %+v", projection)
	}

	legacy := &eventChain{runID: RunID("run_legacy")}
	legacy.add(t, EventDraft{Type: EventRunCreated, Payload: json.RawMessage(`{"task":"legacy","workspace":"."}`)})
	legacy.add(t, EventDraft{Type: EventRunStarted})
	legacyProjection := legacy.replay(t)
	if legacyProjection.ConfigFingerprint != "" || legacyProjection.Build != nil || legacyProjection.State != RunRunning {
		t.Fatalf("legacy projection = %+v", legacyProjection)
	}

	invalid := &eventChain{runID: RunID("run_invalid_identity")}
	invalid.add(t, EventDraft{Type: EventRunCreated, Payload: json.RawMessage(`{"task":"invalid","workspace":".","build":{"revision":"abc","dirty":"maybe","flavor":"plain","fingerprint":"sha256:build"}}`)})
	invalid.replayReject(t, "build dirty status")
}

// TestReplayProjectsSessionChainAndEventTimes 锁定会话关系与首末事件时间由
// reducer 统一投影；旧事件缺少会话字段时保持空值兼容。
func TestReplayProjectsSessionChainAndEventTimes(t *testing.T) {
	chain := &eventChain{runID: RunID("run_session")}
	createdAt := time.Unix(10, 0)
	updatedAt := time.Unix(20, 0)
	payload, err := json.Marshal(RunCreatedPayload{Task: "continue", Workspace: ".", SessionID: "session-1", PrevRunID: RunID("run_previous")})
	if err != nil {
		t.Fatal(err)
	}
	chain.add(t, EventDraft{Type: EventRunCreated, OccurredAt: createdAt, Payload: payload})
	chain.add(t, EventDraft{Type: EventRunStarted, OccurredAt: updatedAt})
	projection := chain.replay(t)
	if projection.SessionID != "session-1" || projection.PrevRunID != "run_previous" {
		t.Fatalf("session projection = %+v", projection)
	}
	if !projection.CreatedAt.Equal(createdAt) || !projection.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("projection times = created %s updated %s", projection.CreatedAt, projection.UpdatedAt)
	}

	legacy := newEventChain(t).replay(t)
	if legacy.SessionID != "" || legacy.PrevRunID != "" || legacy.CreatedAt.IsZero() || legacy.UpdatedAt.IsZero() {
		t.Fatalf("legacy projection = %+v", legacy)
	}
}

// TestReplayRejectsMixedWorkspaceHashes 锁定单条 Run 的全部事件必须属于同一
// workspace，即使攻击者重算了后续事件哈希也不能形成合法投影。
func TestReplayRejectsMixedWorkspaceHashes(t *testing.T) {
	chain := &eventChain{runID: RunID("run_mixed_workspace")}
	chain.add(t, EventDraft{Type: EventRunCreated, Payload: json.RawMessage(`{"task":"mixed","workspace":"."}`)})
	second, err := NewEvent(EventDraft{RunID: chain.runID, WorkspaceHash: "other-workspace", Type: EventRunStarted, OccurredAt: time.Unix(2, 0)}, 2, chain.events[0].EventHash)
	if err != nil {
		t.Fatal(err)
	}
	chain.events = append(chain.events, second)
	chain.replayReject(t, "event workspace hash")
}

// TestRunCancelledRequiresCancellingState 锁定 run_cancelled 只能从 cancelling 进入。
func TestRunCancelledRequiresCancellingState(t *testing.T) {
	chain := newEventChain(t)
	chain.add(t, EventDraft{Type: EventRunCancelled, Payload: json.RawMessage(`{"cause":"user_interrupt"}`)})
	chain.replayReject(t, "run_cancelled from terminal state")

	chain = runFailedMatrixFixture(t, RunCancelling)
	chain.add(t, EventDraft{Type: EventRunCancelled, Payload: json.RawMessage(`{"cause":"user_interrupt"}`)})
	if projection := chain.replay(t); projection.State != RunCancelled {
		t.Fatalf("state = %s", projection.State)
	}
}

// TestRunCancelRequestedOnlyFromActiveStates 锁定 cancel_requested 仅接受
// created/running；cancelling 与终态拒绝（防重复取消）。
func TestRunCancelRequestedOnlyFromActiveStates(t *testing.T) {
	chain := newEventChain(t)
	chain.add(t, EventDraft{Type: EventRunCancelRequested, Payload: json.RawMessage(`{"cause":"user_interrupt"}`)})
	if projection := chain.replay(t); projection.State != RunCancelling {
		t.Fatalf("state = %s", projection.State)
	}
	chain.add(t, EventDraft{Type: EventRunCancelRequested, Payload: json.RawMessage(`{"cause":"user_interrupt"}`)})
	chain.replayReject(t, "run_cancel_requested from cancelling")
}

// TestRunStartedOnlyFromCreated 锁定 run_started 仅接受 created。
func TestRunStartedOnlyFromCreated(t *testing.T) {
	chain := newEventChain(t)
	chain.add(t, EventDraft{Type: EventRunStarted})
	chain.replayReject(t, "run_started")

	chain = &eventChain{runID: RunID("run_matrix")}
	chain.add(t, EventDraft{Type: EventRunCreated, Payload: json.RawMessage(`{"task":"matrix","workspace":"."}`)})
	chain.add(t, EventDraft{Type: EventRunStarted})
	chain.add(t, EventDraft{Type: EventRunStarted})
	chain.replayReject(t, "run_started")
}

// TestRunResumedRequiresCleanRunningState 锁定 run_resumed 的 payload 完整性、
// from_seq 上界与 pending 覆盖层拒绝。
func TestRunResumedRequiresCleanRunningState(t *testing.T) {
	valid := func(overrides string) json.RawMessage {
		payload := map[string]any{"invocation": "resume_test", "from_seq": 1, "workspace_observation": "sha256:obs"}
		if overrides != "" {
			merged := map[string]any{}
			for key, value := range payload {
				merged[key] = value
			}
			if err := json.Unmarshal(json.RawMessage(overrides), &merged); err != nil {
				t.Fatalf("merge override payload: %v", err)
			}
			encoded, err := json.Marshal(merged)
			if err != nil {
				t.Fatalf("encode resume payload: %v", err)
			}
			return encoded
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatalf("encode resume payload: %v", err)
		}
		return encoded
	}
	chain := newEventChain(t)
	chain.add(t, EventDraft{Type: EventRunResumed, Payload: valid("")})
	chain.replay(t)

	chain = newEventChain(t)
	chain.add(t, EventDraft{Type: EventRunResumed, Payload: valid(`{"invocation":""}`)})
	chain.replayReject(t, "run_resumed")

	chain = newEventChain(t)
	chain.add(t, EventDraft{Type: EventRunResumed, Payload: valid(`{"workspace_observation":""}`)})
	chain.replayReject(t, "run_resumed")

	// from_seq 必须早于 run_resumed 自身序号（此处 seq=3）。
	chain = newEventChain(t)
	chain.add(t, EventDraft{Type: EventRunResumed, Payload: valid(`{"from_seq":3}`)})
	chain.replayReject(t, "run_resumed")
}

// TestRunResumedRejectedWithPendingApproval 锁定悬而未决审批时不得记录恢复。
func TestRunResumedRejectedWithPendingApproval(t *testing.T) {
	chain := newEventChain(t)
	turnID, operationID := TurnID("turn_matrix"), OperationID("op_matrix")
	chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
	chain.add(t, EventDraft{Type: EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"read","tool_name":"read_file","arguments":{"path":"x"}}`)})
	chain.add(t, EventDraft{Type: EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"ask","fingerprint":"sha256:pending"}`)})
	chain.add(t, EventDraft{Type: EventApprovalRequested, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"fingerprint":"sha256:pending","summary":"read"}`)})
	chain.replay(t)
	chain.add(t, EventDraft{Type: EventRunResumed, Payload: json.RawMessage(`{"invocation":"resume_test","from_seq":1,"workspace_observation":"sha256:obs"}`)})
	chain.replayReject(t, "run_resumed")
}

// TestAssistantAndVerificationEventsRequireRunningTurn 锁定新增守卫：
// 两个上下文事件必须发生在 running 状态的当前开轮内。
func TestAssistantAndVerificationEventsRequireRunningTurn(t *testing.T) {
	turnID := TurnID("turn_matrix")
	cases := []struct {
		name  string
		event EventDraft
	}{
		{"assistant_message_appended", EventDraft{Type: EventAssistantMessageAppended, TurnID: &turnID, Payload: json.RawMessage(`{"text":"hello"}`)}},
		{"verification_finished", EventDraft{Type: EventVerificationFinished, TurnID: &turnID, Payload: json.RawMessage(`{"ok":true}`)}},
	}
	for _, item := range cases {
		chain := newEventChain(t)
		chain.add(t, item.event)
		chain.replayReject(t, "current running turn")

		chain = newEventChain(t)
		chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
		chain.add(t, item.event)
		chain.replay(t)

		mismatched := TurnID("turn_other")
		chain = newEventChain(t)
		chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
		draft := item.event
		draft.TurnID = &mismatched
		chain.add(t, draft)
		chain.replayReject(t, "current running turn")

		chain = newEventChain(t)
		chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
		chain.add(t, EventDraft{Type: EventTurnClosed, TurnID: &turnID, Payload: json.RawMessage(`{"outcome":"continue"}`)})
		chain.add(t, item.event)
		chain.replayReject(t, "current running turn")
	}
}

// TestApprovalResultMustMatchPendingFingerprint 锁定审批结果指纹必须与
// 悬置审批一致。
func TestApprovalResultMustMatchPendingFingerprint(t *testing.T) {
	chain := newEventChain(t)
	turnID, operationID := TurnID("turn_matrix"), OperationID("op_matrix")
	chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
	chain.add(t, EventDraft{Type: EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"read","tool_name":"read_file","arguments":{"path":"x"}}`)})
	chain.add(t, EventDraft{Type: EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"ask","fingerprint":"sha256:pending"}`)})
	chain.add(t, EventDraft{Type: EventApprovalRequested, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"fingerprint":"sha256:pending","summary":"read"}`)})
	chain.add(t, EventDraft{Type: EventApprovalGranted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"fingerprint":"sha256:other"}`)})
	chain.replayReject(t, "fingerprint does not match")
}

// TestToolResultAppendedOnlyOnce 锁定同一操作的工具结果只能追加一次。
func TestToolResultAppendedOnlyOnce(t *testing.T) {
	chain := newEventChain(t)
	turnID, operationID := TurnID("turn_matrix"), OperationID("op_matrix")
	chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
	chain.add(t, EventDraft{Type: EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"read","tool_name":"read_file","arguments":{"path":"x"}}`)})
	chain.add(t, EventDraft{Type: EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:read"}`)})
	chain.add(t, EventDraft{Type: EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"read_file","side_effect_class":"none"}`)})
	chain.add(t, EventDraft{Type: EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"read_file","result":{"ok":true}}`)})
	chain.add(t, EventDraft{Type: EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"ok":true}`)})
	chain.replay(t)
	chain.add(t, EventDraft{Type: EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"ok":true}`)})
	chain.replayReject(t, "tool_result_appended requires one terminal operation result")
}

// TestApplyAcceptsTelemetryEventsWithoutStateChange 锁定五类遥测事件在投影中的
// 显式穿透语义（不改变运行状态），以及 Apply 对未知事件类型的防御性拒绝。
func TestApplyAcceptsTelemetryEventsWithoutStateChange(t *testing.T) {
	chain := newEventChain(t)
	chain.add(t, EventDraft{Type: EventModelRequestStarted, Payload: json.RawMessage(`{"provider":"p","model":"m"}`)})
	chain.add(t, EventDraft{Type: EventModelResponseFinished, Payload: json.RawMessage(`{"text":"ok","semantic_event_count":1}`)})
	chain.add(t, EventDraft{Type: EventModelRequestFailed, Payload: json.RawMessage(`{"error":"boom"}`)})
	chain.add(t, EventDraft{Type: EventVerificationStarted, Payload: json.RawMessage(`{"verification_id":"v1"}`)})
	chain.add(t, EventDraft{Type: EventVerificationFeedbackAppended, Payload: json.RawMessage(`{"text":"feedback"}`)})
	projection := chain.replay(t)
	if projection.State != RunRunning {
		t.Fatalf("telemetry events must not change run state, got %s", projection.State)
	}
	unknown := Event{Seq: projection.LastSeq + 1, RunID: chain.runID, WorkspaceHash: "workspace", Type: EventType("nonexistent_event"), Payload: json.RawMessage(`{}`)}
	if err := projection.Apply(unknown); err == nil || !strings.Contains(err.Error(), "unhandled event type") {
		t.Fatalf("apply unknown type err = %v", err)
	}
}

// TestContextCompactedEvidenceFieldsValidateAndLegacyReplays 锁定 ADR-0007：
// 证据字段（summary_version/trigger/units_source/last_provider_prompt_tokens/
// compacted_turn_ids/retained_turn_ids）出现即必须合法，缺省（旧事件）可完整
// 回放，非法取值失败关闭且不落入不确定状态。
func TestContextCompactedEvidenceFieldsValidateAndLegacyReplays(t *testing.T) {
	build := func(payload string) *eventChain {
		chain := newEventChain(t)
		turnID := NewTurnID()
		chain.add(t, EventDraft{Type: EventTurnOpened, TurnID: &turnID})
		chain.add(t, EventDraft{Type: EventTurnClosed, TurnID: &turnID, Payload: json.RawMessage(`{"outcome":"continue"}`)})
		chain.add(t, EventDraft{Type: EventContextCompacted, Payload: json.RawMessage(payload)})
		return chain
	}
	legacyPayload := `{"through_seq":4,"summary":"旧检查点","estimated_units_before":100,"estimated_units_after":20,"strategy":"deterministic_journal_checkpoint_v4"}`
	if projection := build(legacyPayload).replay(t); projection.LastContextCompactSeq != 5 || projection.ContextCompactedThrough != 4 {
		t.Fatalf("legacy compaction must replay unchanged: %+v", projection)
	}
	fullPayload := `{"through_seq":4,"summary":"检查点","estimated_units_before":100,"estimated_units_after":20,"strategy":"deterministic_journal_checkpoint_v4","summary_version":1,"trigger":"prompt_estimate_over_threshold","units_source":"estimate_bytes_div2","last_provider_prompt_tokens":4321,"compacted_turn_ids":["turn-a"],"retained_turn_ids":[]}`
	if projection := build(fullPayload).replay(t); projection.LastContextCompactSeq != 5 || projection.ContextCompactedThrough != 4 {
		t.Fatalf("full evidence compaction projection = %+v", projection)
	}
	base := `"through_seq":4,"summary":"s","estimated_units_before":100,"estimated_units_after":20,"strategy":"deterministic_journal_checkpoint_v4"`
	if projection := build("{" + base + `,"summary_version":0}`).replay(t); projection.LastContextCompactSeq != 5 {
		t.Fatalf("explicit summary_version 0 must replay like a legacy event: %+v", projection)
	}
	build("{"+base+`,"summary_version":-1}`).replayReject(t, "summary_version -1 is unsupported")
	build("{"+base+`,"summary_version":1,"retained_turn_ids":["turn-x"]}`).replayReject(t, "summary v1 must not retain full turns")
	for name, extension := range map[string]string{
		"summary_version 2 is unsupported":            `,"summary_version":2`,
		"trigger \"manual\" is unsupported":           `,"trigger":"manual"`,
		"units_source \"provider_usage\" is unsuppor": `,"units_source":"provider_usage"`,
		"last_provider_prompt_tokens must not be ne":  `,"last_provider_prompt_tokens":-1`,
	} {
		payload := "{" + base + extension + "}"
		build(payload).replayReject(t, name)
	}
}
