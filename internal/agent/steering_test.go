// steering_test 覆盖安全点 steering：幂等消费、正文入制品、冷恢复不重复注入。
//
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// TestSteeringConsumedAtSafetyPointOnce 锁定安全点消费一次、幂等跳过重复 ID。
func TestSteeringConsumedAtSafetyPointOnce(t *testing.T) {
	journal := memory.New()
	store, err := artifact.New(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	loop := &Loop{Journal: journal, Artifacts: store, steeringConsumed: map[string]struct{}{}}
	projection := &domain.RunProjection{State: domain.RunRunning, ID: "run-1", WorkspaceHash: "h"}
	messages := []ports.Message{}
	bridge := NewSteeringBridge()
	loop.AdoptSteering(bridge)
	if err := bridge.Submit("in-1", "请优先修复 parser 的空指针"); err != nil {
		t.Fatal(err)
	}
	if err := bridge.Submit("in-1", "重复投递应幂等"); err != nil {
		t.Fatal(err)
	}
	updated, err := loop.consumeSteeringAtSafetyPoint(context.Background(), projection, RunInput{RunID: "run-1", WorkspaceHash: "h"}, messages)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated) != 1 || !strings.Contains(updated[0].Content, "parser") {
		t.Fatalf("messages = %#v", updated)
	}
	// 再次安全点：无新输入，不重复注入。
	updated2, err := loop.consumeSteeringAtSafetyPoint(context.Background(), projection, RunInput{RunID: "run-1", WorkspaceHash: "h"}, updated)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated2) != 1 {
		t.Fatalf("duplicate injection: %#v", updated2)
	}
	events, err := journal.Load(context.Background(), "run-1")
	if err != nil {
		t.Fatal(err)
	}
	var submitted, consumed int
	for _, event := range events {
		switch event.Type {
		case domain.EventSteeringInputSubmitted:
			submitted++
		case domain.EventSteeringInputConsumed:
			consumed++
		}
	}
	if submitted != 1 || consumed != 1 {
		t.Fatalf("submitted=%d consumed=%d events=%d", submitted, consumed, len(events))
	}
}

// TestSteeringColdRecoverySkipsConsumed 锁定冷恢复按事件重建已消费集合。
func TestSteeringColdRecoverySkipsConsumed(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventSteeringInputSubmitted, Payload: json.RawMessage(`{"input_id":"in-9","kind":"steer"}`)},
		{Type: domain.EventSteeringInputConsumed, Payload: json.RawMessage(`{"input_id":"in-9","outcome":"applied"}`)},
	}
	consumed := steeringConsumedFromEvents(events)
	if _, ok := consumed["in-9"]; !ok {
		t.Fatal("consumed id must be restored from events")
	}
}
