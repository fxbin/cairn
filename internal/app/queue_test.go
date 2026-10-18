// queue_test 覆盖持久后续队列：三态投影、幂等确认、取消与非法顺序失败关闭。
//
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// TestQueueProjectionThreeStates 锁定 pending → linked/cancelled 三态与顺序约束。
func TestQueueProjectionThreeStates(t *testing.T) {
	mustQueueEvent := func(t *testing.T, seq uint64, prev string, eventType domain.EventType, payload string) domain.Event {
		t.Helper()
		event, err := domain.NewEvent(domain.EventDraft{
			RunID: domain.QueueRunID, WorkspaceHash: "wh", Type: eventType, Payload: json.RawMessage(payload),
		}, seq, prev)
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	e1 := mustQueueEvent(t, 1, domain.GenesisHash, domain.EventQueueInputSubmitted, `{"input_id":"q1","content_ref":{"sha256":"sha256:a"}}`)
	e2 := mustQueueEvent(t, 2, e1.EventHash, domain.EventQueueInputSubmitted, `{"input_id":"q2","content_ref":{"sha256":"sha256:b"}}`)
	e3 := mustQueueEvent(t, 3, e2.EventHash, domain.EventQueueInputConsumed, `{"input_id":"q1","outcome":"linked","target_run_id":"run-q1"}`)
	e4 := mustQueueEvent(t, 4, e3.EventHash, domain.EventQueueInputCancelled, `{"input_id":"q2","reason":"user_request"}`)
	events := []domain.Event{e1, e2, e3, e4}
	projection, err := domain.ProjectQueue(events)
	if err != nil {
		t.Fatal(err)
	}
	if projection.Items["q1"].State != domain.QueueItemLinked || projection.Items["q1"].TargetRunID != "run-q1" {
		t.Fatalf("q1 = %#v", projection.Items["q1"])
	}
	if projection.Items["q2"].State != domain.QueueItemCancelled || projection.Items["q2"].Reason != "user_request" {
		t.Fatalf("q2 = %#v", projection.Items["q2"])
	}
}

// TestQueueProjectionRejectsIllegalOrder 锁定未提交先消费、重复终结失败关闭。
func TestQueueProjectionRejectsIllegalOrder(t *testing.T) {
	mustQueueEvent := func(t *testing.T, seq uint64, prev string, eventType domain.EventType, payload string) domain.Event {
		t.Helper()
		event, err := domain.NewEvent(domain.EventDraft{
			RunID: domain.QueueRunID, WorkspaceHash: "wh", Type: eventType, Payload: json.RawMessage(payload),
		}, seq, prev)
		if err != nil {
			t.Fatal(err)
		}
		return event
	}
	e1 := mustQueueEvent(t, 1, domain.GenesisHash, domain.EventQueueInputConsumed, `{"input_id":"x","outcome":"linked","target_run_id":"r"}`)
	if _, err := domain.ProjectQueue([]domain.Event{e1}); err == nil {
		t.Fatal("consume before submit must fail")
	}
	s1 := mustQueueEvent(t, 1, domain.GenesisHash, domain.EventQueueInputSubmitted, `{"input_id":"x"}`)
	c1 := mustQueueEvent(t, 2, s1.EventHash, domain.EventQueueInputCancelled, `{"input_id":"x","reason":"r"}`)
	c2 := mustQueueEvent(t, 3, c1.EventHash, domain.EventQueueInputConsumed, `{"input_id":"x","outcome":"linked","target_run_id":"r"}`)
	if _, err := domain.ProjectQueue([]domain.Event{s1, c1, c2}); err == nil {
		t.Fatal("resolve twice must fail")
	}
}

// TestQueueRejectsRunChainMixing 锁定队列事件不得混入 run 链。
func TestQueueRejectsRunChainMixing(t *testing.T) {
	projection := domain.RunProjection{}
	err := projection.Apply(domain.Event{Seq: 1, RunID: "run-1", Type: domain.EventQueueInputSubmitted, Payload: json.RawMessage(`{"input_id":"x"}`)})
	if err == nil || !strings.Contains(err.Error(), "must not appear") {
		t.Fatalf("err = %v", err)
	}
}

// TestSubmitAndCancelQueueInput 锁定 App 层提交与取消写入 queue 命名空间。
func TestSubmitAndCancelQueueInput(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	item, err := service.SubmitQueueInput(context.Background(), QueueSubmitOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		InputID: "q-1", Content: "下一任务：补测试",
	})
	if err != nil {
		t.Fatal(err)
	}
	if item.State != domain.QueueItemPending {
		t.Fatalf("item = %#v", item)
	}
	// 重复提交幂等拒绝。
	if _, err := service.SubmitQueueInput(context.Background(), QueueSubmitOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		InputID: "q-1", Content: "dup",
	}); err == nil {
		t.Fatal("duplicate submit must fail")
	}
	cancelled, err := service.CancelQueueInput(context.Background(), QueueCancelOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		InputID: "q-1", Reason: "user_request",
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != domain.QueueItemCancelled {
		t.Fatalf("cancelled = %#v", cancelled)
	}
	views, err := service.ListQueue(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].State != domain.QueueItemCancelled {
		t.Fatalf("views = %#v", views)
	}
}

// TestConfirmQueueInputLinksNewRun 锁定确认启动：consumed{linked} 与新 run 关联。
func TestConfirmQueueInputLinksNewRun(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	if _, err := service.SubmitQueueInput(context.Background(), QueueSubmitOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		InputID: "q-run", Content: "从队列启动的任务",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := service.ConfirmQueueInput(context.Background(), QueueConfirmOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration, InputID: "q-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ID == "" {
		t.Fatal("confirm must create a run")
	}
	views, err := service.ListQueue(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, view := range views {
		if view.InputID == "q-run" {
			found = true
			if view.State != domain.QueueItemLinked || view.TargetRunID != result.ID {
				t.Fatalf("view = %#v result=%s", view, result.ID)
			}
		}
	}
	if !found {
		t.Fatalf("views = %#v", views)
	}
	// 幂等：重复确认不产生第二个 run。
	again, err := service.ConfirmQueueInput(context.Background(), QueueConfirmOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration, InputID: "q-run",
	})
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != result.ID {
		t.Fatalf("idempotent confirm = %s want %s", again.ID, result.ID)
	}
}
