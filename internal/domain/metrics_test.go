package domain

import (
	"encoding/json"
	"strings"
	"testing"
)

// evidenceEvent 以最小字段构造一条测试事件。
func evidenceEvent(eventType EventType, seq uint64, payload string) Event {
	return Event{Type: eventType, Seq: seq, Payload: json.RawMessage(payload)}
}

// TestCollectRunEvidenceMetricsCountsDuplicateContinuations 锁定重复探索计
// 数：同签名组合第二次及以后各计一次，final 轮不参与，签名缺失即失败关闭。
func TestCollectRunEvidenceMetricsCountsDuplicateContinuations(t *testing.T) {
	events := []Event{
		evidenceEvent(EventTurnClosed, 1, `{"outcome":"continue","operation_signature":"sig-a","progress_fingerprint":"fp-1"}`),
		evidenceEvent(EventTurnClosed, 2, `{"outcome":"continue","operation_signature":"sig-a","progress_fingerprint":"fp-1"}`),
		evidenceEvent(EventTurnClosed, 3, `{"outcome":"continue","operation_signature":"sig-b","progress_fingerprint":"fp-2"}`),
		evidenceEvent(EventTurnClosed, 4, `{"outcome":"continue","operation_signature":"sig-a","progress_fingerprint":"fp-1"}`),
		evidenceEvent(EventTurnClosed, 5, `{"outcome":"continue","operation_signature":"","progress_fingerprint":"fp-text"}`),
		evidenceEvent(EventTurnClosed, 6, `{"outcome":"final"}`),
	}
	metrics, err := CollectRunEvidenceMetrics(events)
	if err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	if metrics.RepeatedExplorations != 2 {
		t.Fatalf("repeated explorations = %d, want 2", metrics.RepeatedExplorations)
	}

	missing := []Event{
		evidenceEvent(EventTurnClosed, 1, `{"outcome":"continue"}`),
	}
	if _, err := CollectRunEvidenceMetrics(missing); err == nil || !strings.Contains(err.Error(), "missing its exploration signatures") {
		t.Fatalf("absent signature keys must fail closed: %v", err)
	}
	degenerate := []Event{
		evidenceEvent(EventTurnClosed, 1, `{"outcome":"halted","operation_signature":"s","progress_fingerprint":"f"}`),
	}
	if _, err := CollectRunEvidenceMetrics(degenerate); err == nil || !strings.Contains(err.Error(), "outside the lexicon") {
		t.Fatalf("unknown outcome must fail closed: %v", err)
	}
}

// TestToolCallPairingBreaksCoversThreeBreakKinds 锁定三类配对破坏：孤儿结果、
// 未闭环提议与先于提议出现的重复结果序列都必须被拒绝或计数。
func TestToolCallPairingBreaksCoversThreeBreakKinds(t *testing.T) {
	closedPair := []Event{
		proposalWith(1, "op-1"),
		resultOn(2, "op-1"),
	}
	breaks, err := ToolCallPairingBreaks(closedPair)
	if err != nil || breaks != 0 {
		t.Fatalf("closed pair must be clean: %d %v", breaks, err)
	}

	orphanResult := []Event{
		resultOn(1, "op-x"),
	}
	if breaks, err = ToolCallPairingBreaks(orphanResult); err != nil || breaks != 1 {
		t.Fatalf("orphan result must count one break: %d %v", breaks, err)
	}

	unclosedProposal := []Event{
		proposalWith(1, "op-y"),
	}
	if breaks, err = ToolCallPairingBreaks(unclosedProposal); err != nil || breaks != 1 {
		t.Fatalf("unclosed proposal must count one break: %d %v", breaks, err)
	}

	duplicateResult := []Event{
		proposalWith(1, "op-z"),
		resultOn(2, "op-z"),
		resultOn(3, "op-z"),
	}
	if _, err = ToolCallPairingBreaks(duplicateResult); err == nil || !strings.Contains(err.Error(), "duplicate tool result") {
		t.Fatalf("duplicate results must fail closed with the id: %v", err)
	}
}

// proposalWith 构造带操作标识的工具调用提议事件。
func proposalWith(seq uint64, operationID string) Event {
	id := OperationID(operationID)
	return Event{Type: EventToolCallProposed, Seq: seq, OperationID: &id, Payload: json.RawMessage(`{}`)}
}

// resultOn 构造同一操作的结果追加事件。
func resultOn(seq uint64, operationID string) Event {
	id := OperationID(operationID)
	return Event{Type: EventToolResultAppended, Seq: seq, OperationID: &id, Payload: json.RawMessage(`{"ok":true,"code":0,"data":{},"preview":"","truncated":false,"bytes":0}`)}
}
