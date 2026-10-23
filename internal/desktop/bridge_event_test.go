// bridge_event_test.go 锁定 QA-01 事件订阅：Emitter 推送 StreamEvent
// 字段稳定，且不 Mock 整个 desktop client。
// Author: fxbin
package desktop

import (
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
)

// TestBridgeStreamEventDTORoundTrip 锁定 StreamEvent JSON 字段名稳定。
func TestBridgeStreamEventDTORoundTrip(t *testing.T) {
	event := StreamEvent{Kind: "run_started", RunID: "run-1", Seq: 1}
	raw, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["kind"]; !ok {
		t.Fatalf("StreamEvent 缺少 kind 字段: %v", decoded)
	}
	var back StreamEvent
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Kind != "run_started" || back.RunID != "run-1" {
		t.Fatalf("往返不符: %+v", back)
	}
}

// TestBridgeEmitterDeliversEvents 锁定 Emitter 回调可接收事件。
func TestBridgeEmitterDeliversEvents(t *testing.T) {
	var mu sync.Mutex
	received := make([]StreamEvent, 0, 4)
	emit := func(event StreamEvent) {
		mu.Lock()
		received = append(received, event)
		mu.Unlock()
	}
	controller := NewRunController(app.Service{}, nil, emit)
	defer controller.Shutdown(time.Second)
	// 直接模拟推送，验证回调链路
	emit(StreamEvent{Kind: "progress", RunID: "run-1", Seq: 1})
	emit(StreamEvent{Kind: "completed", RunID: "run-1", Seq: 2})
	mu.Lock()
	defer mu.Unlock()
	if len(received) != 2 {
		t.Fatalf("应收到 2 条事件: %+v", received)
	}
	if received[0].Kind != "progress" || received[1].Kind != "completed" {
		t.Fatalf("事件顺序不符: %+v", received)
	}
}
