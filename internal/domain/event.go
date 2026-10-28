package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion 是事件信封的当前全局版本；payload 级扩展以可选字段推进，
// 不递增本值（见 ADR-0007）。
const SchemaVersion uint32 = 1

// GenesisHash 是哈希链起点之前的空摘要占位。
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// ContextSummaryVersion 是 context_compacted 检查点摘要格式的当前版本；
// 0 表示未标注该字段的历史事件。摘要算法演进必须递增此版本并保持旧事件回放。
const ContextSummaryVersion = 1

// ContextCompactionTriggerPromptEstimate 是当前唯一的压缩触发原因：
// 下一次请求的估算输入越过保留余量后的阈值。
const ContextCompactionTriggerPromptEstimate = "prompt_estimate_over_threshold"

// ContextUnitsSourceEstimateBytesDiv2 标注 estimated_units_* 字段的估算器
// 身份：字节总数除以二的悲观近似。
const ContextUnitsSourceEstimateBytesDiv2 = "estimate_bytes_div2"

// EventType 标识事件日志中可回放的事件种类。
type EventType string

const (
	EventRunCreated                   EventType = "run_created"
	EventRunStarted                   EventType = "run_started"
	EventRunResumed                   EventType = "run_resumed"
	EventRunCancelRequested           EventType = "run_cancel_requested"
	EventRunCompleted                 EventType = "run_completed"
	EventRunFailed                    EventType = "run_failed"
	EventRunCancelled                 EventType = "run_cancelled"
	EventTurnOpened                   EventType = "turn_opened"
	EventTurnClosed                   EventType = "turn_closed"
	EventModelRequestStarted          EventType = "model_request_started"
	EventModelResponseFinished        EventType = "model_response_finished"
	EventModelRequestFailed           EventType = "model_request_failed"
	EventToolCallProposed             EventType = "tool_call_proposed"
	EventVerificationCommandProposed  EventType = "verification_command_proposed"
	EventPolicyDecided                EventType = "policy_decided"
	EventApprovalRequested            EventType = "approval_requested"
	EventApprovalGranted              EventType = "approval_granted"
	EventApprovalDenied               EventType = "approval_denied"
	EventOperationStarted             EventType = "operation_started"
	EventOperationSucceeded           EventType = "operation_succeeded"
	EventOperationFailed              EventType = "operation_failed"
	EventOperationCancelled           EventType = "operation_cancelled"
	EventOperationReconciled          EventType = "operation_reconciled"
	EventToolResultAppended           EventType = "tool_result_appended"
	EventAssistantMessageAppended     EventType = "assistant_message_appended"
	EventVerificationFeedbackAppended EventType = "verification_feedback_appended"
	EventContextCompacted             EventType = "context_compacted"
	EventVerificationStarted          EventType = "verification_started"
	EventVerificationFinished         EventType = "verification_finished"
	EventSteeringInputSubmitted       EventType = "steering_input_submitted"
	EventSteeringInputConsumed        EventType = "steering_input_consumed"
	EventQueueInputSubmitted          EventType = "queue_input_submitted"
	EventSubagentDelegated            EventType = "subagent_delegated"
	EventSubagentCompleted            EventType = "subagent_completed"
	EventQueueInputConsumed           EventType = "queue_input_consumed"
	EventQueueInputCancelled          EventType = "queue_input_cancelled"
)

// Event 是已规范化、串入哈希链且可持久化的领域事件。
type Event struct {
	SchemaVersion uint32          `json:"schema_version"`
	EventID       string          `json:"event_id"`
	RunID         RunID           `json:"run_id"`
	WorkspaceHash string          `json:"workspace_hash"`
	Seq           uint64          `json:"seq"`
	OccurredAt    time.Time       `json:"occurred_at"`
	Type          EventType       `json:"type"`
	TurnID        *TurnID         `json:"turn_id,omitempty"`
	OperationID   *OperationID    `json:"operation_id,omitempty"`
	AttemptID     *AttemptID      `json:"attempt_id,omitempty"`
	Payload       json.RawMessage `json:"payload"`
	PrevHash      string          `json:"prev_hash"`
	EventHash     string          `json:"event_hash"`
}

// EventDraft 保存创建事件前的业务字段，不包含序号与哈希。
type EventDraft struct {
	EventID       string
	RunID         RunID
	WorkspaceHash string
	OccurredAt    time.Time
	Type          EventType
	TurnID        *TurnID
	OperationID   *OperationID
	AttemptID     *AttemptID
	Payload       json.RawMessage
}

// payloadSchemas 在 V0 中刻意封闭；除验证哈希链完整性外，还保证当前
// reducer 能理解每一个持久化字段。每个 payload 都必须包含 payload_version。
var payloadSchemas = map[EventType]map[string]struct{}{
	EventRunCreated: {
		"payload_version": {}, "task": {}, "task_ref": {}, "workspace": {}, "workspace_hash": {}, "config_ref": {}, "config_fingerprint": {}, "build": {}, "budget": {},
		"session_id": {}, "prev_run_id": {}, "prior_context_ref": {}, "project_instructions_ref": {}, "fork_source": {}, "parent_run_id": {},
	},
	EventRunStarted: {"payload_version": {}, "invocation": {}, "invocation_id": {}},
	EventRunResumed: {"payload_version": {}, "invocation": {}, "invocation_id": {}, "from_seq": {}, "workspace_observation": {}},
	EventRunCancelRequested: {
		"payload_version": {}, "cause": {}, "expected_terminal": {}, "budget_dimension": {},
	},
	EventRunCompleted: {"payload_version": {}, "final_response": {}, "final_response_ref": {}, "verification_seq": {}},
	EventRunFailed:    {"payload_version": {}, "error": {}, "error_class": {}, "reason": {}, "reason_ref": {}, "budget_dimension": {}},
	EventRunCancelled: {"payload_version": {}, "cause": {}, "evidence_ref": {}},
	EventTurnOpened:   {"payload_version": {}, "turn": {}, "turn_index": {}},
	EventTurnClosed:   {"payload_version": {}, "outcome": {}, "model_response_ref": {}, "operation_signature": {}, "progress_fingerprint": {}},
	EventModelRequestStarted: {
		"payload_version": {}, "provider": {}, "model": {}, "request_ref": {}, "retry_index": {},
	},
	EventModelResponseFinished: {"payload_version": {}, "text": {}, "tool_calls": {}, "usage": {}, "usage_estimated": {}, "stop_reason": {}, "response_ref": {}, "semantic_event_count": {}, "protocol_blocks_ref": {}},
	EventModelRequestFailed:    {"payload_version": {}, "error": {}, "error_class": {}, "retryable": {}, "semantic_event_count": {}, "reason_ref": {}},
	EventToolCallProposed:      {"payload_version": {}, "call_id": {}, "tool_name": {}, "spec_version": {}, "arguments": {}, "arguments_ref": {}, "fingerprint": {}},
	EventVerificationCommandProposed: {
		"payload_version": {}, "verification_id": {}, "command_index": {}, "tool_name": {}, "arguments": {}, "arguments_ref": {}, "fingerprint": {}, "plan_ref": {},
	},
	EventPolicyDecided:     {"payload_version": {}, "effect": {}, "rule": {}, "rule_id": {}, "fingerprint": {}, "summary": {}, "reason": {}, "failure_code": {}, "failure_data": {}},
	EventApprovalRequested: {"payload_version": {}, "fingerprint": {}, "summary": {}, "rule": {}, "rule_id": {}, "workspace_observation": {}},
	EventApprovalGranted:   {"payload_version": {}, "fingerprint": {}, "actor": {}},
	EventApprovalDenied:    {"payload_version": {}, "fingerprint": {}, "actor": {}, "reason": {}, "stop_cause": {}},
	EventOperationStarted: {
		"payload_version": {}, "tool_name": {}, "kind": {}, "side_effect_class": {}, "mutation_barrier": {}, "request_ref": {}, "planned_path": {}, "planned_before": {}, "planned_after": {}, "planned_before_ref": {},
	},
	EventOperationSucceeded: {"payload_version": {}, "tool_name": {}, "result": {}, "result_ref": {}, "result_storage_error": {}, "duration_ms": {}, "hook_actions": {}},
	EventOperationFailed:    {"payload_version": {}, "tool_name": {}, "result": {}, "result_ref": {}, "result_storage_error": {}, "error_class": {}, "after_observation": {}, "duration_ms": {}, "hook_actions": {}},
	EventOperationCancelled: {"payload_version": {}, "tool_name": {}, "result": {}, "result_ref": {}, "result_storage_error": {}, "cause": {}, "confirmed_stopped": {}, "hook_actions": {}},
	EventOperationReconciled: {
		"payload_version": {}, "verdict": {}, "actor": {}, "note": {}, "evidence": {}, "evidence_ref": {}, "workspace_observation": {},
	},
	EventToolResultAppended:           {"payload_version": {}, "ok": {}, "code": {}, "data": {}, "preview": {}, "truncated": {}, "bytes": {}, "sha256": {}, "result_ref": {}, "is_error": {}},
	EventAssistantMessageAppended:     {"payload_version": {}, "text": {}, "message_ref": {}},
	EventVerificationFeedbackAppended: {"payload_version": {}, "text": {}},
	// 证据字段（ADR-0007）：全部可选，旧事件缺省合法；retained_turn_ids 在
	// v1 摘要格式下恒为空（不保留任何完整轮次），compacted_turn_ids 与
	// through_seq 共同界定检查点替代的轮次区间。
	EventContextCompacted: {
		"payload_version": {}, "through_seq": {}, "summary": {}, "estimated_units_before": {}, "estimated_units_after": {}, "strategy": {},
		"summary_version": {}, "trigger": {}, "units_source": {}, "last_provider_prompt_tokens": {}, "compacted_turn_ids": {}, "retained_turn_ids": {},
	},
	EventVerificationStarted: {
		"payload_version": {}, "verification_id": {}, "plan_ref": {}, "last_mutation_seq": {}, "commands_fingerprint": {}, "workspace_observation": {},
	},
	EventVerificationFinished: {"payload_version": {}, "verification_id": {}, "ok": {}, "evidence_ref": {}, "verified_mutation_seq": {}, "workspace_observation": {}, "result_fingerprint": {}},
	// P3a 安全点 steering：输入经制品脱敏落盘，事件只带引用与幂等标识。
	// kind 可选区分 steer（用户补充指导）与 subagent_result（异步子终态摘要
	// 下轮交付），旧事件缺省视为 steer。
	EventSteeringInputSubmitted: {"payload_version": {}, "input_id": {}, "content_ref": {}, "kind": {}},
	EventSteeringInputConsumed:  {"payload_version": {}, "input_id": {}, "content_ref": {}, "outcome": {}, "kind": {}},
	// P3b 持久后续队列：workspace 级 queue 命名空间，RunID 哨兵 "queue"。
	EventQueueInputSubmitted: {"payload_version": {}, "input_id": {}, "content_ref": {}, "kind": {}},
	EventSubagentDelegated:   {"payload_version": {}, "child_run_id": {}, "task_digest": {}, "budget": {}},
	EventSubagentCompleted:   {"payload_version": {}, "child_run_id": {}, "outcome": {}, "usage": {}},
	EventQueueInputConsumed:  {"payload_version": {}, "input_id": {}, "content_ref": {}, "outcome": {}, "target_run_id": {}},
	EventQueueInputCancelled: {"payload_version": {}, "input_id": {}, "reason": {}},
}

// NewEvent 规范化草稿 payload，并生成带前序哈希的不可变事件。
func NewEvent(draft EventDraft, seq uint64, prevHash string) (Event, error) {
	if draft.RunID == "" {
		return Event{}, fmt.Errorf("event run id is required")
	}
	if draft.EventID == "" {
		draft.EventID = NewID("evt")
	}
	if draft.OccurredAt.IsZero() {
		draft.OccurredAt = time.Now().UTC()
	}
	if prevHash == "" {
		prevHash = GenesisHash
	}
	payload, err := normalizePayload(draft.Type, draft.Payload)
	if err != nil {
		return Event{}, err
	}
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       draft.EventID,
		RunID:         draft.RunID,
		WorkspaceHash: draft.WorkspaceHash,
		Seq:           seq,
		OccurredAt:    draft.OccurredAt.UTC(),
		Type:          draft.Type,
		TurnID:        draft.TurnID,
		OperationID:   draft.OperationID,
		AttemptID:     draft.AttemptID,
		Payload:       payload,
		PrevHash:      prevHash,
	}
	hash, err := ComputeEventHash(event)
	if err != nil {
		return Event{}, err
	}
	event.EventHash = hash
	return event, nil
}

// ComputeEventHash 按冻结的二进制字段顺序计算事件 SHA-256 哈希。
func ComputeEventHash(event Event) (string, error) {
	prevHash, err := hex.DecodeString(event.PrevHash)
	if err != nil {
		return "", fmt.Errorf("decode previous event hash: %w", err)
	}
	if len(prevHash) != sha256.Size {
		return "", fmt.Errorf("previous event hash must decode to %d bytes", sha256.Size)
	}
	var body bytes.Buffer
	body.WriteString("go-agent-event-v1\x00")
	writeUint32(&body, event.SchemaVersion)
	writeString(&body, event.EventID)
	writeString(&body, string(event.RunID))
	writeString(&body, event.WorkspaceHash)
	writeUint64(&body, event.Seq)
	writeString(&body, event.OccurredAt.UTC().Format(time.RFC3339Nano))
	writeString(&body, string(event.Type))
	writeNullable(&body, event.TurnID)
	writeNullable(&body, event.OperationID)
	writeNullable(&body, event.AttemptID)
	writeBytes(&body, event.Payload)
	writeBytes(&body, prevHash)
	sum := sha256.Sum256(body.Bytes())
	return hex.EncodeToString(sum[:]), nil
}

// VerifyChain 验证事件序号、payload schema 与前后哈希引用组成的完整链。
func VerifyChain(events []Event) error {
	previous := GenesisHash
	for index, event := range events {
		expectedSeq := uint64(index + 1)
		// 只拒绝来自更新二进制的未来 schema 版本；同代及更旧版本保持可回放，
		// 使读取端升级后仍能校验既有日志（写入端仍固定产出当前版本）。
		if event.SchemaVersion > SchemaVersion {
			return fmt.Errorf("event %d has unsupported schema version %d", event.Seq, event.SchemaVersion)
		}
		if err := validatePayload(event.Type, event.Payload); err != nil {
			return fmt.Errorf("event %d payload: %w", event.Seq, err)
		}
		if event.Seq != expectedSeq {
			return fmt.Errorf("event sequence mismatch: got %d want %d", event.Seq, expectedSeq)
		}
		if event.PrevHash != previous {
			return fmt.Errorf("event %d previous hash mismatch", event.Seq)
		}
		actual, err := ComputeEventHash(event)
		if err != nil {
			return fmt.Errorf("hash event %d: %w", event.Seq, err)
		}
		if actual != event.EventHash {
			return fmt.Errorf("event %d hash mismatch", event.Seq)
		}
		previous = event.EventHash
	}
	return nil
}

// normalizePayload 补齐版本字段并将事件 payload 规整为受限 JSON 对象。
func normalizePayload(eventType EventType, raw json.RawMessage) (json.RawMessage, error) {
	object := make(map[string]json.RawMessage)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) != 0 && !bytes.Equal(trimmed, []byte("null")) {
		if err := json.Unmarshal(trimmed, &object); err != nil {
			return nil, fmt.Errorf("event payload must be an object: %w", err)
		}
		if object == nil {
			return nil, fmt.Errorf("event payload must be an object")
		}
	}
	if _, exists := object["payload_version"]; !exists {
		object["payload_version"] = json.RawMessage("1")
	}
	encoded, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("encode event payload: %w", err)
	}
	if _, _, err := validatePayloadShape(eventType, encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

// validatePayload 拒绝未知事件种类、版本不符、schema 外字段或类型与语义
// 不合法的 payload。信封版本策略为单一 SchemaVersion + 可选字段演进
// （ADR-0007），不按 payload_version 分叉实现。
func validatePayload(eventType EventType, raw json.RawMessage) error {
	object, _, err := validatePayloadShape(eventType, raw)
	if err != nil {
		return err
	}
	return validatePayloadSemantics(eventType, raw, object)
}

// validatePayloadShape 校验事件种类、版本与封闭字段集合；NewEvent 只执行此层，
// 完整强类型语义由 ValidateDraft 与 VerifyChain 在写入前和回放前分别验证。
func validatePayloadShape(eventType EventType, raw json.RawMessage) (map[string]json.RawMessage, uint32, error) {
	allowed, known := payloadSchemas[eventType]
	if !known {
		return nil, 0, fmt.Errorf("unknown event type %q", eventType)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("payload must be an object")
		}
		return nil, 0, fmt.Errorf("decode payload: %w", err)
	}
	var version uint32
	value, present := object["payload_version"]
	// 与 VerifyChain 一致：接受当前及更旧 payload 版本，只拒绝未来版本；写入
	// 路径经 normalizePayload 固定补齐当前版本，旧版本只会出现在历史日志中。
	if !present || json.Unmarshal(value, &version) != nil || version < 1 || version > SchemaVersion {
		return nil, 0, fmt.Errorf("unsupported or missing payload_version")
	}
	for field := range object {
		if _, allowed := allowed[field]; !allowed {
			return nil, 0, fmt.Errorf("unknown field %q for %s", field, eventType)
		}
	}
	return object, version, nil
}

// validatePayloadSemantics 对事件执行强类型和业务语义校验；可选证据字段只在
// 出现时校验，以保持历史事件可回放。不按版本拆分实现。
func validatePayloadSemantics(eventType EventType, raw json.RawMessage, object map[string]json.RawMessage) error {
	switch eventType {
	case EventRunCreated:
		var payload struct {
			Task                   string             `json:"task"`
			TaskRef                json.RawMessage    `json:"task_ref"`
			Workspace              string             `json:"workspace"`
			WorkspaceHash          *string            `json:"workspace_hash"`
			ConfigRef              json.RawMessage    `json:"config_ref"`
			ConfigFingerprint      *string            `json:"config_fingerprint"`
			Build                  *BuildIdentity     `json:"build"`
			Budget                 json.RawMessage    `json:"budget"`
			SessionID              *string            `json:"session_id"`
			PrevRunID              *string            `json:"prev_run_id"`
			ParentRunID            *string            `json:"parent_run_id,omitempty"`
			PriorContextRef        json.RawMessage    `json:"prior_context_ref"`
			ProjectInstructionsRef json.RawMessage    `json:"project_instructions_ref"`
			ForkSource             *SessionForkSource `json:"fork_source"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode run_created payload semantics: %w", err)
		}
		if err := requirePayloadFields(eventType, object, "task", "workspace"); err != nil {
			return err
		}
		if strings.TrimSpace(payload.Task) == "" || strings.TrimSpace(payload.Workspace) == "" {
			return fmt.Errorf("%s task and workspace must not be empty", eventType)
		}
		if err := validateOptionalPayloadDigest(eventType, "workspace_hash", payload.WorkspaceHash); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "config_fingerprint", payload.ConfigFingerprint); err != nil {
			return err
		}
		if payload.Build != nil && payload.Build.Fingerprint != "" {
			if err := validateDigest(payload.Build.Fingerprint); err != nil {
				return fmt.Errorf("%s build fingerprint: %w", eventType, err)
			}
		}
		for _, field := range []string{"task_ref", "config_ref", "budget", "prior_context_ref", "project_instructions_ref"} {
			if err := validateOptionalPayloadObject(eventType, object, field); err != nil {
				return err
			}
		}
		// fork_source 与 prev_run_id 互斥：跨会话来源边只允许出现在新分支根，
		// 不得用跨 session 的 prev_run_id 冒充分支。
		if payload.ForkSource != nil && payload.PrevRunID != nil && strings.TrimSpace(*payload.PrevRunID) != "" {
			return fmt.Errorf("%s fork_source is mutually exclusive with prev_run_id", eventType)
		}
		if err := validateSessionForkSource(eventType, object, payload.ForkSource); err != nil {
			return err
		}
	case EventRunStarted:
		for _, field := range []string{"invocation", "invocation_id"} {
			if _, err := validateOptionalPayloadString(eventType, object, field, false); err != nil {
				return err
			}
		}
	case EventRunResumed:
		var payload struct {
			Invocation           *string `json:"invocation"`
			InvocationID         *string `json:"invocation_id"`
			FromSeq              *uint64 `json:"from_seq"`
			WorkspaceObservation *string `json:"workspace_observation"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode run_resumed payload semantics: %w", err)
		}
		if err := requirePayloadFields(eventType, object, "invocation", "from_seq", "workspace_observation"); err != nil {
			return err
		}
		if payload.Invocation == nil || strings.TrimSpace(*payload.Invocation) == "" || payload.FromSeq == nil || *payload.FromSeq == 0 {
			return fmt.Errorf("%s invocation and from_seq must be non-empty", eventType)
		}
		if err := validateOptionalPayloadDigest(eventType, "workspace_observation", payload.WorkspaceObservation); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadString(eventType, object, "invocation_id", false); err != nil {
			return err
		}
	case EventRunCancelRequested:
		cause, err := validateRequiredPayloadString(eventType, object, "cause", false)
		if err != nil {
			return err
		}
		expectedTerminal, err := validateOptionalPayloadString(eventType, object, "expected_terminal", false)
		if err != nil {
			return err
		}
		if expectedTerminal != nil && !oneOf(*expectedTerminal, "failed", "cancelled") {
			return fmt.Errorf("%s has invalid expected_terminal %q", eventType, *expectedTerminal)
		}
		if err := validateOptionalBudgetDimension(eventType, object); err != nil {
			return err
		}
		budgetDimension, err := validateOptionalPayloadString(eventType, object, "budget_dimension", false)
		if err != nil {
			return err
		}
		if expectedTerminal != nil {
			switch *expectedTerminal {
			case "cancelled":
				if cause == "budget_exhausted" || budgetDimension != nil {
					return fmt.Errorf("%s cancelled terminal must not carry budget exhaustion fields", eventType)
				}
			case "failed":
				if cause != "budget_exhausted" || budgetDimension == nil {
					return fmt.Errorf("%s failed terminal requires budget_exhausted cause and budget_dimension", eventType)
				}
			}
		}
	case EventRunFailed:
		if err := requireAnyPayloadField(eventType, object, "error", "error_class", "reason", "reason_ref", "budget_dimension"); err != nil {
			return err
		}
		for _, field := range []string{"error", "error_class"} {
			if _, err := validateOptionalPayloadString(eventType, object, field, false); err != nil {
				return err
			}
		}
		if _, err := validateOptionalPayloadString(eventType, object, "reason", true); err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "reason_ref"); err != nil {
			return err
		}
		if err := validateOptionalBudgetDimension(eventType, object); err != nil {
			return err
		}
	case EventRunCancelled:
		if _, err := validateRequiredPayloadString(eventType, object, "cause", false); err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "evidence_ref"); err != nil {
			return err
		}
	case EventTurnOpened:
		for _, field := range []string{"turn", "turn_index"} {
			if _, err := validateOptionalPayloadInteger(eventType, object, field, 0); err != nil {
				return err
			}
		}
	case EventTurnClosed:
		var payload struct {
			Outcome             *string         `json:"outcome"`
			ModelResponseRef    json.RawMessage `json:"model_response_ref"`
			OperationSignature  *string         `json:"operation_signature"`
			ProgressFingerprint *string         `json:"progress_fingerprint"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode turn_closed payload semantics: %w", err)
		}
		if payload.Outcome == nil {
			return fmt.Errorf("%s requires field %q", eventType, "outcome")
		}
		if !oneOf(*payload.Outcome, "continue", "final", "failed", "cancelled") {
			return fmt.Errorf("%s has invalid outcome %q", eventType, *payload.Outcome)
		}
		if err := validateOptionalPayloadObject(eventType, object, "model_response_ref"); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "operation_signature", payload.OperationSignature); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "progress_fingerprint", payload.ProgressFingerprint); err != nil {
			return err
		}
	case EventModelRequestStarted:
		var payload struct {
			Provider   *string         `json:"provider"`
			Model      *string         `json:"model"`
			RequestRef json.RawMessage `json:"request_ref"`
			RetryIndex *int            `json:"retry_index"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode model_request_started payload semantics: %w", err)
		}
		if payload.Provider == nil || payload.Model == nil || strings.TrimSpace(*payload.Provider) == "" || strings.TrimSpace(*payload.Model) == "" {
			return fmt.Errorf("%s provider and model are required", eventType)
		}
		if payload.RetryIndex != nil && *payload.RetryIndex < 0 {
			return fmt.Errorf("%s retry_index must not be negative", eventType)
		}
		if err := validateOptionalPayloadObject(eventType, object, "request_ref"); err != nil {
			return err
		}
	case EventModelResponseFinished:
		if err := requireAnyPayloadField(eventType, object, "text", "tool_calls", "usage", "response_ref"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadString(eventType, object, "text", true); err != nil {
			return err
		}
		if rawCalls, exists := object["tool_calls"]; exists {
			var calls []struct {
				ID        *string         `json:"id"`
				Name      *string         `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(rawCalls, &calls); err != nil || calls == nil {
				if err == nil {
					err = fmt.Errorf("value must be an array")
				}
				return fmt.Errorf("%s field %q: %w", eventType, "tool_calls", err)
			}
			seen := make(map[string]struct{}, len(calls))
			for index, call := range calls {
				if call.ID == nil || strings.TrimSpace(*call.ID) == "" || call.Name == nil || strings.TrimSpace(*call.Name) == "" {
					return fmt.Errorf("%s tool_calls[%d] requires non-empty id and name", eventType, index)
				}
				if _, duplicate := seen[*call.ID]; duplicate {
					return fmt.Errorf("%s tool_calls contains duplicate id %q", eventType, *call.ID)
				}
				seen[*call.ID] = struct{}{}
				if err := validateRawJSONObject(call.Arguments); err != nil {
					return fmt.Errorf("%s tool_calls[%d] arguments: %w", eventType, index, err)
				}
			}
		}
		if rawUsage, exists := object["usage"]; exists {
			if err := validateModelUsage(eventType, rawUsage); err != nil {
				return err
			}
		}
		if _, err := validateOptionalPayloadBool(eventType, object, "usage_estimated"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadString(eventType, object, "stop_reason", true); err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "response_ref"); err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "protocol_blocks_ref"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadInteger(eventType, object, "semantic_event_count", 0); err != nil {
			return err
		}
	case EventModelRequestFailed:
		if _, err := validateRequiredPayloadString(eventType, object, "error", false); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadString(eventType, object, "error_class", false); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadBool(eventType, object, "retryable"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadInteger(eventType, object, "semantic_event_count", 0); err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "reason_ref"); err != nil {
			return err
		}
	case EventToolCallProposed:
		if _, err := validateRequiredPayloadString(eventType, object, "call_id", false); err != nil {
			return err
		}
		if _, err := validateRequiredPayloadString(eventType, object, "tool_name", false); err != nil {
			return err
		}
		if err := validateRequiredPayloadObject(eventType, object, "arguments"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadString(eventType, object, "spec_version", false); err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "arguments_ref"); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigestField(eventType, object, "fingerprint"); err != nil {
			return err
		}
	case EventVerificationCommandProposed:
		if _, err := validateRequiredPayloadString(eventType, object, "verification_id", false); err != nil {
			return err
		}
		if _, err := validateRequiredPayloadInteger(eventType, object, "command_index", 0); err != nil {
			return err
		}
		if err := validateRequiredPayloadObject(eventType, object, "arguments"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadString(eventType, object, "tool_name", false); err != nil {
			return err
		}
		for _, field := range []string{"arguments_ref", "plan_ref"} {
			if err := validateOptionalPayloadObject(eventType, object, field); err != nil {
				return err
			}
		}
		if err := validateOptionalPayloadDigestField(eventType, object, "fingerprint"); err != nil {
			return err
		}
	case EventPolicyDecided:
		var payload struct {
			Effect      *string `json:"effect"`
			Rule        *string `json:"rule"`
			RuleID      *string `json:"rule_id"`
			Fingerprint *string `json:"fingerprint"`
			Summary     *string `json:"summary"`
			Reason      *string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode policy_decided payload semantics: %w", err)
		}
		if payload.Effect == nil {
			return fmt.Errorf("%s requires field %q", eventType, "effect")
		}
		if !oneOf(*payload.Effect, "allow", "ask", "deny") {
			return fmt.Errorf("%s has invalid effect %q", eventType, *payload.Effect)
		}
		if err := validateOptionalPayloadDigest(eventType, "fingerprint", payload.Fingerprint); err != nil {
			return err
		}
		failureCode, err := validateOptionalPayloadString(eventType, object, "failure_code", false)
		if err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "failure_data"); err != nil {
			return err
		}
		if _, exists := object["failure_data"]; exists && failureCode == nil {
			return fmt.Errorf("%s field %q requires field %q", eventType, "failure_data", "failure_code")
		}
	case EventApprovalRequested:
		if err := validateRequiredPayloadDigest(eventType, object, "fingerprint"); err != nil {
			return err
		}
		if _, err := validateRequiredPayloadString(eventType, object, "summary", false); err != nil {
			return err
		}
		for _, field := range []string{"rule", "rule_id"} {
			if _, err := validateOptionalPayloadString(eventType, object, field, true); err != nil {
				return err
			}
		}
		if err := validateOptionalPayloadDigestField(eventType, object, "workspace_observation"); err != nil {
			return err
		}
	case EventApprovalGranted:
		if err := validateRequiredPayloadDigest(eventType, object, "fingerprint"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadString(eventType, object, "actor", false); err != nil {
			return err
		}
	case EventApprovalDenied:
		if err := validateRequiredPayloadDigest(eventType, object, "fingerprint"); err != nil {
			return err
		}
		for _, field := range []string{"actor", "reason", "stop_cause"} {
			if _, err := validateOptionalPayloadString(eventType, object, field, true); err != nil {
				return err
			}
		}
	case EventOperationStarted:
		var payload struct {
			ToolName         *string         `json:"tool_name"`
			Kind             *string         `json:"kind"`
			SideEffectClass  *string         `json:"side_effect_class"`
			MutationBarrier  *bool           `json:"mutation_barrier"`
			RequestRef       json.RawMessage `json:"request_ref"`
			PlannedPath      *string         `json:"planned_path"`
			PlannedBefore    *string         `json:"planned_before"`
			PlannedAfter     *string         `json:"planned_after"`
			PlannedBeforeRef json.RawMessage `json:"planned_before_ref"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode operation_started payload semantics: %w", err)
		}
		if payload.ToolName == nil || strings.TrimSpace(*payload.ToolName) == "" {
			return fmt.Errorf("%s tool_name is required", eventType)
		}
		if payload.SideEffectClass != nil && !oneOf(*payload.SideEffectClass, "none", "workspace", "process") {
			return fmt.Errorf("%s has invalid side_effect_class %q", eventType, *payload.SideEffectClass)
		}
		for _, field := range []string{"request_ref", "planned_before_ref"} {
			if err := validateOptionalPayloadObject(eventType, object, field); err != nil {
				return err
			}
		}
		for field, value := range map[string]*string{"planned_before": payload.PlannedBefore, "planned_after": payload.PlannedAfter} {
			if value != nil && *value != "absent" {
				if err := validateOptionalPayloadDigest(eventType, field, value); err != nil {
					return err
				}
			}
		}
	case EventOperationSucceeded, EventOperationFailed, EventOperationCancelled:
		resultOK, err := validateOperationTerminalPayload(eventType, object)
		if err != nil {
			return err
		}
		if resultOK != nil {
			if eventType == EventOperationSucceeded && !*resultOK {
				return fmt.Errorf("%s result must report ok=true", eventType)
			}
			if eventType != EventOperationSucceeded && *resultOK {
				return fmt.Errorf("%s result must report ok=false", eventType)
			}
		}
		if eventType == EventOperationFailed {
			if _, err := validateOptionalPayloadString(eventType, object, "error_class", false); err != nil {
				return err
			}
			if err := validateOptionalPayloadDigestField(eventType, object, "after_observation"); err != nil {
				return err
			}
		}
		if eventType == EventOperationCancelled {
			if _, err := validateOptionalPayloadString(eventType, object, "cause", true); err != nil {
				return err
			}
			if _, err := validateOptionalPayloadBool(eventType, object, "confirmed_stopped"); err != nil {
				return err
			}
		}
	case EventOperationReconciled:
		var payload struct {
			Verdict              *string         `json:"verdict"`
			Actor                *string         `json:"actor"`
			Note                 *string         `json:"note"`
			Evidence             *string         `json:"evidence"`
			EvidenceRef          json.RawMessage `json:"evidence_ref"`
			WorkspaceObservation *string         `json:"workspace_observation"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode operation_reconciled payload semantics: %w", err)
		}
		if payload.Verdict == nil {
			return fmt.Errorf("%s requires field %q", eventType, "verdict")
		}
		if !oneOf(*payload.Verdict, "applied_success", "applied_failure", "not_applied", "abort_run") {
			return fmt.Errorf("%s has invalid verdict %q", eventType, *payload.Verdict)
		}
		if err := validateOptionalPayloadObject(eventType, object, "evidence_ref"); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "workspace_observation", payload.WorkspaceObservation); err != nil {
			return err
		}
	case EventToolResultAppended:
		if _, err := validateToolResultObject(eventType, object); err != nil {
			return err
		}
		if err := validateOptionalPayloadObject(eventType, object, "result_ref"); err != nil {
			return err
		}
		if _, err := validateOptionalPayloadBool(eventType, object, "is_error"); err != nil {
			return err
		}
	case EventAssistantMessageAppended:
		var payload struct {
			Text       *string         `json:"text"`
			MessageRef json.RawMessage `json:"message_ref"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode assistant_message_appended payload semantics: %w", err)
		}
		if payload.Text == nil {
			return fmt.Errorf("%s requires field %q", eventType, "text")
		}
		if err := validateOptionalPayloadObject(eventType, object, "message_ref"); err != nil {
			return err
		}
	case EventVerificationFeedbackAppended:
		if _, err := validateRequiredPayloadString(eventType, object, "text", false); err != nil {
			return err
		}
	case EventContextCompacted:
		var payload struct {
			ThroughSeq               *uint64   `json:"through_seq"`
			Summary                  *string   `json:"summary"`
			EstimatedUnitsBefore     *int      `json:"estimated_units_before"`
			EstimatedUnitsAfter      *int      `json:"estimated_units_after"`
			Strategy                 *string   `json:"strategy"`
			SummaryVersion           *int      `json:"summary_version"`
			Trigger                  *string   `json:"trigger"`
			UnitsSource              *string   `json:"units_source"`
			LastProviderPromptTokens *int64    `json:"last_provider_prompt_tokens"`
			CompactedTurnIDs         *[]string `json:"compacted_turn_ids"`
			RetainedTurnIDs          *[]string `json:"retained_turn_ids"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode context_compacted payload semantics: %w", err)
		}
		if payload.ThroughSeq == nil || payload.Summary == nil || payload.EstimatedUnitsBefore == nil || payload.EstimatedUnitsAfter == nil || payload.Strategy == nil {
			return fmt.Errorf("%s required checkpoint fields are missing", eventType)
		}
		if *payload.ThroughSeq == 0 || strings.TrimSpace(*payload.Summary) == "" || *payload.EstimatedUnitsBefore < 1 || *payload.EstimatedUnitsAfter < 1 || *payload.EstimatedUnitsAfter > *payload.EstimatedUnitsBefore {
			return fmt.Errorf("%s checkpoint values are invalid", eventType)
		}
		if !oneOf(*payload.Strategy, "deterministic_journal_checkpoint_v4") {
			return fmt.Errorf("%s has invalid strategy %q", eventType, *payload.Strategy)
		}
		if payload.SummaryVersion != nil && (*payload.SummaryVersion < 0 || *payload.SummaryVersion > ContextSummaryVersion) {
			return fmt.Errorf("%s summary_version %d is unsupported", eventType, *payload.SummaryVersion)
		}
		if payload.Trigger != nil && *payload.Trigger != ContextCompactionTriggerPromptEstimate {
			return fmt.Errorf("%s trigger %q is unsupported", eventType, *payload.Trigger)
		}
		if payload.UnitsSource != nil && *payload.UnitsSource != ContextUnitsSourceEstimateBytesDiv2 {
			return fmt.Errorf("%s units_source %q is unsupported", eventType, *payload.UnitsSource)
		}
		if payload.LastProviderPromptTokens != nil && *payload.LastProviderPromptTokens < 0 {
			return fmt.Errorf("%s last_provider_prompt_tokens must not be negative", eventType)
		}
		if payload.SummaryVersion != nil && *payload.SummaryVersion == ContextSummaryVersion && payload.RetainedTurnIDs != nil && len(*payload.RetainedTurnIDs) != 0 {
			return fmt.Errorf("%s summary v1 must not retain full turns", eventType)
		}
	case EventVerificationStarted:
		var payload struct {
			VerificationID       *string         `json:"verification_id"`
			PlanRef              json.RawMessage `json:"plan_ref"`
			LastMutationSeq      *uint64         `json:"last_mutation_seq"`
			CommandsFingerprint  *string         `json:"commands_fingerprint"`
			WorkspaceObservation *string         `json:"workspace_observation"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode verification_started payload semantics: %w", err)
		}
		if payload.VerificationID != nil && strings.TrimSpace(*payload.VerificationID) == "" {
			return fmt.Errorf("%s verification_id must not be empty", eventType)
		}
		if err := validateOptionalPayloadObject(eventType, object, "plan_ref"); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "commands_fingerprint", payload.CommandsFingerprint); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "workspace_observation", payload.WorkspaceObservation); err != nil {
			return err
		}
	case EventVerificationFinished:
		var payload struct {
			VerificationID       *string         `json:"verification_id"`
			OK                   *bool           `json:"ok"`
			EvidenceRef          json.RawMessage `json:"evidence_ref"`
			VerifiedMutationSeq  *uint64         `json:"verified_mutation_seq"`
			WorkspaceObservation *string         `json:"workspace_observation"`
			ResultFingerprint    *string         `json:"result_fingerprint"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode verification_finished payload semantics: %w", err)
		}
		if payload.OK == nil {
			return fmt.Errorf("%s requires boolean field %q", eventType, "ok")
		}
		if payload.VerificationID != nil && strings.TrimSpace(*payload.VerificationID) == "" {
			return fmt.Errorf("%s verification_id must not be empty", eventType)
		}
		if err := validateOptionalPayloadObject(eventType, object, "evidence_ref"); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "workspace_observation", payload.WorkspaceObservation); err != nil {
			return err
		}
		if err := validateOptionalPayloadDigest(eventType, "result_fingerprint", payload.ResultFingerprint); err != nil {
			return err
		}
	case EventRunCompleted:
		var payload struct {
			FinalResponse    *string         `json:"final_response"`
			FinalResponseRef json.RawMessage `json:"final_response_ref"`
			VerificationSeq  *uint64         `json:"verification_seq"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode run_completed payload semantics: %w", err)
		}
		if payload.FinalResponse == nil {
			return fmt.Errorf("%s requires string field %q", eventType, "final_response")
		}
		if err := validateOptionalPayloadObject(eventType, object, "final_response_ref"); err != nil {
			return err
		}
		if payload.VerificationSeq != nil && *payload.VerificationSeq == 0 {
			return fmt.Errorf("%s verification_seq must be positive when present", eventType)
		}
	case EventSubagentDelegated:
		var payload struct {
			ChildRunID *string `json:"child_run_id"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode subagent_delegated payload semantics: %w", err)
		}
		if payload.ChildRunID == nil || strings.TrimSpace(*payload.ChildRunID) == "" {
			return fmt.Errorf("%s child_run_id must not be empty", eventType)
		}
	case EventSubagentCompleted:
		var payload struct {
			ChildRunID *string `json:"child_run_id"`
			Outcome    *string `json:"outcome"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode subagent_completed payload semantics: %w", err)
		}
		if payload.ChildRunID == nil || strings.TrimSpace(*payload.ChildRunID) == "" {
			return fmt.Errorf("%s child_run_id must not be empty", eventType)
		}
		if payload.Outcome == nil || strings.TrimSpace(*payload.Outcome) == "" {
			return fmt.Errorf("%s outcome must not be empty", eventType)
		}
	case EventSteeringInputSubmitted:
		var payload struct {
			InputID    *string         `json:"input_id"`
			ContentRef json.RawMessage `json:"content_ref"`
			Kind       *string         `json:"kind"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode steering_input_submitted payload semantics: %w", err)
		}
		if payload.InputID == nil || strings.TrimSpace(*payload.InputID) == "" {
			return fmt.Errorf("%s input_id must not be empty", eventType)
		}
		if err := validateOptionalPayloadObject(eventType, object, "content_ref"); err != nil {
			return err
		}
		if payload.Kind != nil && !oneOf(*payload.Kind, "steer", "subagent_result") {
			return fmt.Errorf("%s kind %q is invalid", eventType, *payload.Kind)
		}
	case EventSteeringInputConsumed:
		var payload struct {
			InputID    *string         `json:"input_id"`
			ContentRef json.RawMessage `json:"content_ref"`
			Outcome    *string         `json:"outcome"`
			Kind       *string         `json:"kind"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode steering_input_consumed payload semantics: %w", err)
		}
		if payload.InputID == nil || strings.TrimSpace(*payload.InputID) == "" {
			return fmt.Errorf("%s input_id must not be empty", eventType)
		}
		if err := validateOptionalPayloadObject(eventType, object, "content_ref"); err != nil {
			return err
		}
		if payload.Outcome == nil || !oneOf(*payload.Outcome, "applied", "rejected") {
			return fmt.Errorf("%s outcome must be applied or rejected", eventType)
		}
		if payload.Kind != nil && !oneOf(*payload.Kind, "steer", "subagent_result") {
			return fmt.Errorf("%s kind %q is invalid", eventType, *payload.Kind)
		}
	case EventQueueInputSubmitted:
		var payload struct {
			InputID    *string         `json:"input_id"`
			ContentRef json.RawMessage `json:"content_ref"`
			Kind       *string         `json:"kind"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode queue_input_submitted payload semantics: %w", err)
		}
		if payload.InputID == nil || strings.TrimSpace(*payload.InputID) == "" {
			return fmt.Errorf("%s input_id must not be empty", eventType)
		}
		if err := validateOptionalPayloadObject(eventType, object, "content_ref"); err != nil {
			return err
		}
		if payload.Kind != nil && !oneOf(*payload.Kind, "next_task") {
			return fmt.Errorf("%s kind %q is invalid", eventType, *payload.Kind)
		}
	case EventQueueInputConsumed:
		var payload struct {
			InputID     *string         `json:"input_id"`
			ContentRef  json.RawMessage `json:"content_ref"`
			Outcome     *string         `json:"outcome"`
			TargetRunID *string         `json:"target_run_id"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode queue_input_consumed payload semantics: %w", err)
		}
		if payload.InputID == nil || strings.TrimSpace(*payload.InputID) == "" {
			return fmt.Errorf("%s input_id must not be empty", eventType)
		}
		if err := validateOptionalPayloadObject(eventType, object, "content_ref"); err != nil {
			return err
		}
		if payload.Outcome == nil || !oneOf(*payload.Outcome, "linked") {
			return fmt.Errorf("%s outcome must be linked", eventType)
		}
		if payload.TargetRunID == nil || strings.TrimSpace(*payload.TargetRunID) == "" {
			return fmt.Errorf("%s target_run_id must not be empty", eventType)
		}
	case EventQueueInputCancelled:
		var payload struct {
			InputID *string `json:"input_id"`
			Reason  *string `json:"reason"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			return fmt.Errorf("decode queue_input_cancelled payload semantics: %w", err)
		}
		if payload.InputID == nil || strings.TrimSpace(*payload.InputID) == "" {
			return fmt.Errorf("%s input_id must not be empty", eventType)
		}
		if payload.Reason == nil || strings.TrimSpace(*payload.Reason) == "" {
			return fmt.Errorf("%s reason must not be empty", eventType)
		}
	default:
		return fmt.Errorf("event type %q has no payload semantic validator", eventType)
	}
	return nil
}

// requirePayloadFields 校验当前版本必须显式出现的字段，避免零值掩盖缺失字段。
func requirePayloadFields(eventType EventType, object map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		if _, exists := object[field]; !exists {
			return fmt.Errorf("%s requires field %q", eventType, field)
		}
	}
	return nil
}

// requireAnyPayloadField 校验一组兼容字段中至少有一个被显式记录。
func requireAnyPayloadField(eventType EventType, object map[string]json.RawMessage, fields ...string) error {
	for _, field := range fields {
		if _, exists := object[field]; exists {
			return nil
		}
	}
	return fmt.Errorf("%s requires at least one semantic field", eventType)
}

// validateRequiredPayloadString 解码必填字符串，并按字段语义决定是否允许空值。
func validateRequiredPayloadString(eventType EventType, object map[string]json.RawMessage, field string, allowEmpty bool) (string, error) {
	value, err := validateOptionalPayloadString(eventType, object, field, allowEmpty)
	if err != nil {
		return "", err
	}
	if value == nil {
		return "", fmt.Errorf("%s requires field %q", eventType, field)
	}
	return *value, nil
}

// validateOptionalPayloadString 校验可选字符串的 JSON 类型与空值约束。
func validateOptionalPayloadString(eventType EventType, object map[string]json.RawMessage, field string, allowEmpty bool) (*string, error) {
	raw, exists := object[field]
	if !exists {
		return nil, nil
	}
	var value *string
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		if err == nil {
			err = fmt.Errorf("value must be a string")
		}
		return nil, fmt.Errorf("%s field %q: %w", eventType, field, err)
	}
	if !allowEmpty && strings.TrimSpace(*value) == "" {
		return nil, fmt.Errorf("%s field %q must not be empty", eventType, field)
	}
	return value, nil
}

// validateOptionalPayloadBool 校验可选布尔字段，显式 null 不视为缺省。
func validateOptionalPayloadBool(eventType EventType, object map[string]json.RawMessage, field string) (*bool, error) {
	raw, exists := object[field]
	if !exists {
		return nil, nil
	}
	var value *bool
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		if err == nil {
			err = fmt.Errorf("value must be a boolean")
		}
		return nil, fmt.Errorf("%s field %q: %w", eventType, field, err)
	}
	return value, nil
}

// validateRequiredPayloadInteger 校验必填整数及其最小允许值。
func validateRequiredPayloadInteger(eventType EventType, object map[string]json.RawMessage, field string, minimum int64) (int64, error) {
	value, err := validateOptionalPayloadInteger(eventType, object, field, minimum)
	if err != nil {
		return 0, err
	}
	if value == nil {
		return 0, fmt.Errorf("%s requires field %q", eventType, field)
	}
	return *value, nil
}

// validateOptionalPayloadInteger 校验可选整数及其最小允许值。
func validateOptionalPayloadInteger(eventType EventType, object map[string]json.RawMessage, field string, minimum int64) (*int64, error) {
	raw, exists := object[field]
	if !exists {
		return nil, nil
	}
	var value *int64
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		if err == nil {
			err = fmt.Errorf("value must be an integer")
		}
		return nil, fmt.Errorf("%s field %q: %w", eventType, field, err)
	}
	if *value < minimum {
		return nil, fmt.Errorf("%s field %q must be at least %d", eventType, field, minimum)
	}
	return value, nil
}

// validateRequiredPayloadObject 校验必填字段为非 null JSON 对象。
func validateRequiredPayloadObject(eventType EventType, object map[string]json.RawMessage, field string) error {
	if _, exists := object[field]; !exists {
		return fmt.Errorf("%s requires field %q", eventType, field)
	}
	return validateOptionalPayloadObject(eventType, object, field)
}

// validateOptionalPayloadObject 校验可选字段一旦出现就必须是非 null JSON 对象。
func validateOptionalPayloadObject(eventType EventType, object map[string]json.RawMessage, field string) error {
	raw, exists := object[field]
	if !exists {
		return nil
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(raw, &value); err != nil || value == nil {
		if err == nil {
			err = fmt.Errorf("value must be an object")
		}
		return fmt.Errorf("%s field %q: %w", eventType, field, err)
	}
	if digestRaw, exists := value["sha256"]; exists {
		var digest string
		if err := json.Unmarshal(digestRaw, &digest); err != nil {
			return fmt.Errorf("%s field %q sha256 must be a string", eventType, field)
		}
		if err := validateDigest(digest); err != nil {
			return fmt.Errorf("%s field %q: %w", eventType, field, err)
		}
	}
	return nil
}

// validateRequiredPayloadDigest 校验必填摘要字段的字符串类型与摘要格式。
func validateRequiredPayloadDigest(eventType EventType, object map[string]json.RawMessage, field string) error {
	value, err := validateRequiredPayloadString(eventType, object, field, false)
	if err != nil {
		return err
	}
	if err := validateDigest(value); err != nil {
		return fmt.Errorf("%s field %q: %w", eventType, field, err)
	}
	return nil
}

// validateOptionalPayloadDigestField 校验 map 中可选摘要字段，显式 null 非法。
func validateOptionalPayloadDigestField(eventType EventType, object map[string]json.RawMessage, field string) error {
	value, err := validateOptionalPayloadString(eventType, object, field, false)
	if err != nil || value == nil {
		return err
	}
	if err := validateDigest(*value); err != nil {
		return fmt.Errorf("%s field %q: %w", eventType, field, err)
	}
	return nil
}

// validateOptionalPayloadDigest 校验可选摘要字段；字段缺失兼容旧事件，出现时
// 必须是带 sha256 域前缀且不含空白或控制字符的字符串。
func validateOptionalPayloadDigest(eventType EventType, field string, value *string) error {
	if value == nil {
		return nil
	}
	if err := validateDigest(*value); err != nil {
		return fmt.Errorf("%s field %q: %w", eventType, field, err)
	}
	return nil
}

// validateOptionalBudgetDimension 校验预算维度的封闭枚举。
func validateOptionalBudgetDimension(eventType EventType, object map[string]json.RawMessage) error {
	value, err := validateOptionalPayloadString(eventType, object, "budget_dimension", false)
	if err != nil || value == nil {
		return err
	}
	if !oneOf(*value, "turns", "input_tokens", "output_tokens", "cost_usd", "tool_calls", "wall_time") {
		return fmt.Errorf("%s has invalid budget_dimension %q", eventType, *value)
	}
	return nil
}

// validateRawJSONObject 校验原始 JSON 为非 null 对象。
func validateRawJSONObject(raw json.RawMessage) error {
	var object map[string]json.RawMessage
	if len(raw) == 0 {
		return fmt.Errorf("value must be an object")
	}
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("value must be an object")
		}
		return err
	}
	return nil
}

// validateModelUsage 校验模型 token 计数类型、非负性与汇总一致性。
func validateModelUsage(eventType EventType, raw json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("value must be an object")
		}
		return fmt.Errorf("%s field %q: %w", eventType, "usage", err)
	}
	values := make(map[string]int64, 3)
	for _, field := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		value, err := validateOptionalPayloadInteger(eventType, object, field, 0)
		if err != nil {
			return fmt.Errorf("%s usage: %w", eventType, err)
		}
		if value != nil {
			values[field] = *value
		}
	}
	for field := range object {
		if !oneOf(field, "prompt_tokens", "completion_tokens", "total_tokens") {
			return fmt.Errorf("%s usage has unknown field %q", eventType, field)
		}
	}
	if total, exists := values["total_tokens"]; exists && total != 0 && total != values["prompt_tokens"]+values["completion_tokens"] {
		return fmt.Errorf("%s usage total_tokens must equal prompt_tokens plus completion_tokens", eventType)
	}
	return nil
}

// validateOperationTerminalPayload 校验操作终态的工具名、结果、制品引用与耗时。
func validateOperationTerminalPayload(eventType EventType, object map[string]json.RawMessage) (*bool, error) {
	if _, err := validateRequiredPayloadString(eventType, object, "tool_name", false); err != nil {
		return nil, err
	}
	var resultOK *bool
	if raw, exists := object["result"]; exists {
		var result map[string]json.RawMessage
		if err := json.Unmarshal(raw, &result); err != nil || result == nil {
			if err == nil {
				err = fmt.Errorf("value must be an object")
			}
			return nil, fmt.Errorf("%s field %q: %w", eventType, "result", err)
		}
		ok, err := validateToolResultObject(eventType, result)
		if err != nil {
			return nil, fmt.Errorf("%s field %q: %w", eventType, "result", err)
		}
		resultOK = &ok
	}
	if err := validateOptionalPayloadObject(eventType, object, "result_ref"); err != nil {
		return nil, err
	}
	if _, err := validateOptionalPayloadString(eventType, object, "result_storage_error", false); err != nil {
		return nil, err
	}
	if _, err := validateOptionalPayloadInteger(eventType, object, "duration_ms", 0); err != nil {
		return nil, err
	}
	return resultOK, nil
}

// validateToolResultObject 校验 tools.Result 的稳定字段，同时允许历史最小结果仅含 ok。
func validateToolResultObject(eventType EventType, object map[string]json.RawMessage) (bool, error) {
	ok, err := validateOptionalPayloadBool(eventType, object, "ok")
	if err != nil {
		return false, err
	}
	if ok == nil {
		return false, fmt.Errorf("%s requires boolean field %q", eventType, "ok")
	}
	if _, err := validateOptionalPayloadString(eventType, object, "code", true); err != nil {
		return false, err
	}
	if _, err := validateOptionalPayloadString(eventType, object, "preview", true); err != nil {
		return false, err
	}
	if _, err := validateOptionalPayloadBool(eventType, object, "truncated"); err != nil {
		return false, err
	}
	if _, err := validateOptionalPayloadInteger(eventType, object, "bytes", 0); err != nil {
		return false, err
	}
	if err := validateOptionalPayloadDigestField(eventType, object, "sha256"); err != nil {
		return false, err
	}
	for field := range object {
		if field == "payload_version" && eventType == EventToolResultAppended {
			continue
		}
		if !oneOf(field, "ok", "code", "data", "preview", "truncated", "bytes", "sha256", "result_ref", "is_error") {
			return false, fmt.Errorf("%s tool result has unknown field %q", eventType, field)
		}
	}
	return *ok, nil
}

// validateSessionForkSource 校验 run_created 的可选 fork_source 锚点：字段
// 一旦出现必须完整，绑定来源 run、工作区身份与事件边界摘要；与 prev_run_id
// 的互斥在调用方已判定。
func validateSessionForkSource(eventType EventType, object map[string]json.RawMessage, source *SessionForkSource) error {
	if _, exists := object["fork_source"]; !exists {
		return nil
	}
	if source == nil {
		return fmt.Errorf("%s fork_source must be an object", eventType)
	}
	if strings.TrimSpace(string(source.RunID)) == "" {
		return fmt.Errorf("%s fork_source run_id must not be empty", eventType)
	}
	if err := validateDigest(source.WorkspaceHash); err != nil {
		return fmt.Errorf("%s fork_source workspace_hash: %w", eventType, err)
	}
	if source.LastSeq == 0 {
		return fmt.Errorf("%s fork_source last_seq must be positive", eventType)
	}
	if err := validateDigest(source.LastEventHash); err != nil {
		return fmt.Errorf("%s fork_source last_event_hash: %w", eventType, err)
	}
	return nil
}

// validateDigest 接受正式十六进制摘要及历史测试/迁移使用的短安全占位值。
func validateDigest(value string) error {
	const prefix = "sha256:"
	if !strings.HasPrefix(value, prefix) || len(value) == len(prefix) {
		return fmt.Errorf("must be a non-empty sha256 digest")
	}
	for _, character := range value[len(prefix):] {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' {
			continue
		}
		return fmt.Errorf("must be a non-empty sha256 digest")
	}
	return nil
}

// oneOf 报告值是否属于封闭枚举。
func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}

// writeUint32 将 uint32 以固定大端格式写入哈希输入缓冲区。
func writeUint32(buffer *bytes.Buffer, value uint32) {
	var encoded [4]byte
	binary.BigEndian.PutUint32(encoded[:], value)
	writeBytes(buffer, encoded[:])
}

// writeUint64 将 uint64 以固定大端格式写入哈希输入缓冲区。
func writeUint64(buffer *bytes.Buffer, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	writeBytes(buffer, encoded[:])
}

// writeString 将字符串作为带长度前缀的字节序列写入缓冲区。
func writeString(buffer *bytes.Buffer, value string) { writeBytes(buffer, []byte(value)) }

// writeBytes 将字节序列及其 uint64 长度前缀写入缓冲区。
func writeBytes(buffer *bytes.Buffer, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	buffer.Write(length[:])
	buffer.Write(value)
}

// writeNullable 使用存在标记编码可空字符串派生类型。
func writeNullable[T ~string](buffer *bytes.Buffer, value *T) {
	if value == nil {
		writeBytes(buffer, []byte{0})
		return
	}
	writeBytes(buffer, append([]byte{1}, []byte(string(*value))...))
}
