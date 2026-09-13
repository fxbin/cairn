// session_test 覆盖会话前情卡的确定性投影与链装配：卡片各段投影、终态与
// 验证结论如实标注，前置运行的事件经内存日志装载后沿用其会话标识延续链。
//
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
)

// priorRunEvents 构造一个带写变更与验证的完整前置运行事件序列。
func priorRunEvents(t *testing.T, runID domain.RunID, sessionID string) []domain.Event {
	t.Helper()
	created, err := json.Marshal(domain.RunCreatedPayload{Task: "创建 note.txt", SessionID: sessionID})
	if err != nil {
		t.Fatal(err)
	}
	return []domain.Event{
		{RunID: runID, Seq: 1, Type: domain.EventRunCreated, Payload: created},
		{RunID: runID, Seq: 2, Type: domain.EventRunStarted, Payload: json.RawMessage(`{"invocation":"i"}`)},
		{RunID: runID, Seq: 3, Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"apply_patch","arguments":{"path":"note.txt","mode":"create","content":"hi"}}`)},
		{RunID: runID, Seq: 4, Type: domain.EventVerificationFinished, Payload: json.RawMessage(`{"ok":true}`)},
		{RunID: runID, Seq: 5, Type: domain.EventRunCompleted, Payload: json.RawMessage(`{"final_response":"已创建并验证 note.txt"}`)},
	}
}

// TestPriorContextCardProjectsDeterministicCard 锁定前情卡投影：任务、终态、
// 变更文件、验证与最终回复各段齐全且文本确定。
func TestPriorContextCardProjectsDeterministicCard(t *testing.T) {
	events := priorRunEvents(t, "run-prior", "session-x")
	card := PriorContextCard(events)
	for _, want := range []string{"任务: 创建 note.txt", "终态: 完成", "变更文件: note.txt", "验证: 通过", "已创建并验证 note.txt"} {
		if !strings.Contains(card, want) {
			t.Fatalf("前情卡缺少 %q:\n%s", want, card)
		}
	}
	failed := append(priorRunEvents(t, "run-prior", "s"), domain.Event{Seq: 6, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error_class":"budget_turns"}`)})
	// run_failed 与 run_completed 并存时取最后出现的终态语义：投影按事件顺序，
	// 后写入的 run_failed 覆盖终态段。
	card = PriorContextCard(failed)
	if !strings.Contains(card, "失败（budget_turns）") {
		t.Fatalf("失败终态应带失败分类:\n%s", card)
	}
}

// TestResolveSessionContextContinuesChain 锁定链装配：前置运行事件可装载时
// 沿用其会话标识，前情卡制品化且引用成对返回。
func TestResolveSessionContextContinuesChain(t *testing.T) {
	journal := memory.New()
	events := priorRunEvents(t, "run-prior", "session-x")
	for _, event := range events {
		if _, err := journal.Append(context.Background(), uint64(event.Seq-1), domain.EventDraft{RunID: event.RunID, WorkspaceHash: "h", Type: event.Type, Payload: event.Payload}); err != nil {
			t.Fatal(err)
		}
	}
	store, err := artifact.New(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{}
	sessionID, priorContext, ref, err := service.resolveSessionContext(context.Background(), journal, store, RunOptions{PrevRunID: "run-prior"})
	if err != nil {
		t.Fatal(err)
	}
	if sessionID != "session-x" {
		t.Fatalf("应沿用前置运行的会话标识，得到 %q", sessionID)
	}
	if priorContext == "" || ref == nil {
		t.Fatal("前情卡文本与制品引用应成对返回")
	}
	stored, err := store.Open(*ref)
	if err != nil || !strings.Contains(string(stored), "创建 note.txt") {
		t.Fatalf("前情卡制品应可读回: err=%v", err)
	}
}

// TestResolveSessionContextWithoutPrevRun 锁定无会话输入时零变化。
func TestResolveSessionContextWithoutPrevRun(t *testing.T) {
	service := Service{}
	sessionID, priorContext, ref, err := service.resolveSessionContext(context.Background(), nil, nil, RunOptions{})
	if err != nil || sessionID != "" || priorContext != "" || ref != nil {
		t.Fatalf("无会话输入应原样返回空值: session=%q prior=%q ref=%v err=%v", sessionID, priorContext, ref, err)
	}
}

// TestResolveSessionContextPersistsExplicitTextAndRestoresReference 锁定显式文本与
// 仅引用两条入口都以同一内容寻址制品为事实源。
func TestResolveSessionContextPersistsExplicitTextAndRestoresReference(t *testing.T) {
	store, err := artifact.New(t.TempDir(), 1024)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{}
	_, text, ref, err := service.resolveSessionContext(context.Background(), nil, store, RunOptions{PriorContext: "已验证的显式前情", PriorContextRef: &artifact.Ref{SHA256: "sha256:stale"}})
	if err != nil {
		t.Fatal(err)
	}
	if ref == nil || text != "已验证的显式前情" || ref.SHA256 == "sha256:stale" {
		t.Fatalf("显式文本未生成规范引用: text=%q ref=%#v", text, ref)
	}
	_, restored, restoredRef, err := service.resolveSessionContext(context.Background(), nil, store, RunOptions{PriorContextRef: ref})
	if err != nil {
		t.Fatal(err)
	}
	if restored != text || restoredRef == nil || *restoredRef != *ref {
		t.Fatalf("仅引用未恢复原文: text=%q ref=%#v", restored, restoredRef)
	}
}
