package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
		"session_id": {}, "prev_run_id": {}, "prior_context_ref": {},
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
	EventModelResponseFinished: {"payload_version": {}, "text": {}, "tool_calls": {}, "usage": {}, "usage_estimated": {}, "stop_reason": {}, "response_ref": {}, "semantic_event_count": {}},
	EventModelRequestFailed:    {"payload_version": {}, "error": {}, "error_class": {}, "retryable": {}, "semantic_event_count": {}, "reason_ref": {}},
	EventToolCallProposed:      {"payload_version": {}, "call_id": {}, "tool_name": {}, "spec_version": {}, "arguments": {}, "arguments_ref": {}, "fingerprint": {}},
	EventVerificationCommandProposed: {
		"payload_version": {}, "verification_id": {}, "command_index": {}, "tool_name": {}, "arguments": {}, "arguments_ref": {}, "fingerprint": {}, "plan_ref": {},
	},
	EventPolicyDecided:     {"payload_version": {}, "effect": {}, "rule": {}, "rule_id": {}, "fingerprint": {}, "summary": {}, "reason": {}},
	EventApprovalRequested: {"payload_version": {}, "fingerprint": {}, "summary": {}, "rule": {}, "rule_id": {}, "workspace_observation": {}},
	EventApprovalGranted:   {"payload_version": {}, "fingerprint": {}, "actor": {}},
	EventApprovalDenied:    {"payload_version": {}, "fingerprint": {}, "actor": {}, "reason": {}, "stop_cause": {}},
	EventOperationStarted: {
		"payload_version": {}, "tool_name": {}, "kind": {}, "side_effect_class": {}, "mutation_barrier": {}, "request_ref": {}, "planned_path": {}, "planned_before": {}, "planned_after": {},
	},
	EventOperationSucceeded: {"payload_version": {}, "tool_name": {}, "result": {}, "result_ref": {}, "result_storage_error": {}, "duration_ms": {}},
	EventOperationFailed:    {"payload_version": {}, "tool_name": {}, "result": {}, "result_ref": {}, "result_storage_error": {}, "error_class": {}, "after_observation": {}, "duration_ms": {}},
	EventOperationCancelled: {"payload_version": {}, "tool_name": {}, "result": {}, "result_ref": {}, "result_storage_error": {}, "cause": {}, "confirmed_stopped": {}},
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
	if err := validatePayload(eventType, encoded); err != nil {
		return nil, err
	}
	return encoded, nil
}

// validatePayload 拒绝未知事件种类、版本不符或 schema 外的 payload 字段。
func validatePayload(eventType EventType, raw json.RawMessage) error {
	allowed, known := payloadSchemas[eventType]
	if !known {
		return fmt.Errorf("unknown event type %q", eventType)
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&object); err != nil || object == nil {
		if err == nil {
			err = fmt.Errorf("payload must be an object")
		}
		return fmt.Errorf("decode payload: %w", err)
	}
	var version uint32
	value, present := object["payload_version"]
	// 与 VerifyChain 一致：接受当前及更旧 payload 版本，只拒绝未来版本；写入
	// 路径经 normalizePayload 固定补齐当前版本，旧版本只会出现在历史日志中。
	if !present || json.Unmarshal(value, &version) != nil || version < 1 || version > SchemaVersion {
		return fmt.Errorf("unsupported or missing payload_version")
	}
	for field := range object {
		if _, allowed := allowed[field]; !allowed {
			return fmt.Errorf("unknown field %q for %s", field, eventType)
		}
	}
	return nil
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
