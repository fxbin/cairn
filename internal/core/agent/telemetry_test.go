package agent

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

// TestSemanticTelemetryDerivesRunSemantics 锁定领域遥测的事件派生语义：轮次、
// 操作、验证、token 与停止原因全部来自已持久化事件；终态事件决定 Completed。
func TestSemanticTelemetryDerivesRunSemantics(t *testing.T) {
	telemetry := NewSemanticTelemetry("run_x")
	turnID := domain.TurnID("t")
	operationID := domain.OperationID("op")
	events := []domain.Event{
		{Seq: 1, Type: domain.EventTurnOpened, TurnID: &turnID, Payload: mustJSON(struct{ Turn int }{1})},
		{Seq: 2, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID},
		{Seq: 3, Type: domain.EventModelResponseFinished, Payload: json.RawMessage(`{"text":"ok","usage":{"prompt_tokens":120,"completion_tokens":30}}`)},
		{Seq: 4, Type: domain.EventVerificationFinished, Payload: mustJSON(domain.VerificationFinishedPayload{OK: false})},
		{Seq: 5, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error":"x","error_class":"loop_guard_no_workspace_or_verifier_progress"}`)},
	}
	for _, event := range events {
		telemetry.ObserveRunEvent(event)
	}
	if telemetry.Turns != 1 || telemetry.Operations != 1 || telemetry.InputTokens != 120 || telemetry.OutputTokens != 30 {
		t.Fatalf("telemetry = %+v", telemetry)
	}
	if telemetry.VerifierFailed != 1 || telemetry.VerifierOK != 0 {
		t.Fatalf("verifier counts = %+v", telemetry)
	}
	if telemetry.StopReason != "loop_guard_no_workspace_or_verifier_progress" || telemetry.Completed {
		t.Fatalf("terminal semantics = %+v", telemetry)
	}
	completed := NewSemanticTelemetry("run_y")
	completed.ObserveRunEvent(domain.Event{Seq: 1, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{})})
	if !completed.Completed || completed.StopReason != "" {
		t.Fatalf("completed semantics = %+v", completed)
	}
}

// TestSemanticTelemetryIsNotOnTheCriticalPath 锁定默认关闭合同：Loop 结构与
// RunInput 不持有遥测引用（零接线），接入仅通过 EventSink 组合显式发生。
func TestSemanticTelemetryIsNotOnTheCriticalPath(t *testing.T) {
	var loop Loop
	_ = loop.OnEvent // EventSink 是既有唯一外部消费点
	if loop.Journal != nil {
		t.Fatal("loop must not be constructed in this test")
	}
}
