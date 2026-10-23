// subagent_emit_test 锁定委派事件草稿的类型、归属与载荷可解析性。
//
// Author: fxbin
package app

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// TestDelegatedEventDraftBuildsParentEvent 锁定草稿写在父 run 且类型正确。
func TestDelegatedEventDraftBuildsParentEvent(t *testing.T) {
	at := time.Unix(1700, 0).UTC()
	draft, err := DelegatedEventDraft("run-parent", "ws-hash", DelegateOptions{
		ParentRunID: "run-parent",
		ChildRunID:  "run-child",
		Task:        "盘点工具面",
		Budget:      SubagentBudget{MaxTurns: 2},
	}, at)
	if err != nil {
		t.Fatalf("组装委派草稿失败: %v", err)
	}
	if draft.RunID != "run-parent" || draft.Type != domain.EventSubagentDelegated {
		t.Fatalf("草稿归属或类型不符: %+v", draft)
	}
	if draft.WorkspaceHash != "ws-hash" || !draft.OccurredAt.Equal(at) {
		t.Fatalf("工作区或时间不符: %+v", draft)
	}
	var payload DelegatedPayload
	if err := json.Unmarshal(draft.Payload, &payload); err != nil || payload.ChildRunID != "run-child" {
		t.Fatalf("载荷不可回放: %v %+v", err, payload)
	}
	if _, err := DelegatedEventDraft("", "ws", DelegateOptions{ParentRunID: "p", ChildRunID: "c", Task: "t"}, at); !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("缺父 run 应拒绝: %v", err)
	}
}

// TestCompletedEventDraftBuildsParentEvent 锁定完成草稿字段与拒绝路径。
func TestCompletedEventDraftBuildsParentEvent(t *testing.T) {
	at := time.Unix(1701, 0).UTC()
	draft, err := CompletedEventDraft("run-parent", "ws-hash", "run-child", "ok", json.RawMessage(`{"prompt_tokens":3}`), at)
	if err != nil {
		t.Fatalf("组装完成草稿失败: %v", err)
	}
	if draft.RunID != "run-parent" || draft.Type != domain.EventSubagentCompleted {
		t.Fatalf("完成草稿归属或类型不符: %+v", draft)
	}
	var payload CompletedPayload
	if err := json.Unmarshal(draft.Payload, &payload); err != nil {
		t.Fatalf("解码完成载荷失败: %v", err)
	}
	if payload.ChildRunID != "run-child" || payload.Outcome != "ok" {
		t.Fatalf("完成载荷不符: %+v", payload)
	}
	if _, err := CompletedEventDraft("run-parent", "ws", "run-child", "", nil, at); !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("空 outcome 应拒绝: %v", err)
	}
}
