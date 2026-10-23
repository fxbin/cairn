// send_message_test 锁定消息校验、未装配失败关闭与成功投递。
//
// Author: fxbin
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestSendMessageValid 锁定合法消息调用 Deliver 并返回 ok。
func TestSendMessageValid(t *testing.T) {
	var gotTo, gotContent string
	tool := SendMessage{Deliver: func(_ context.Context, to, content, _ string) error {
		gotTo, gotContent = to, content
		return nil
	}}
	raw := json.RawMessage(`{"to_run_id":"run-child","content":"继续盘点","reason":"follow-up"}`)
	result := tool.Execute(context.Background(), raw)
	if !result.OK || result.Code != "ok" {
		t.Fatalf("合法消息应成功: %+v", result)
	}
	if gotTo != "run-child" || gotContent != "继续盘点" {
		t.Fatalf("Deliver 参数不符: %q %q", gotTo, gotContent)
	}
	var payload SendMessageResult
	if err := json.Unmarshal(result.Data, &payload); err != nil || payload.ToRunID != "run-child" {
		t.Fatalf("载荷不符: %v %+v", err, payload)
	}
}

// TestSendMessageRejectsInvalid 锁定空目标、空内容、超限与未装配拒绝。
func TestSendMessageRejectsInvalid(t *testing.T) {
	wired := SendMessage{Deliver: func(context.Context, string, string, string) error { return nil }}
	cases := []string{
		`{}`,
		`{"to_run_id":"run-c","content":""}`,
		`{"to_run_id":"","content":"x"}`,
		`{"to_run_id":"run-c","content":"` + strings.Repeat("a", SendMessageMaxBytes+1) + `"}`,
	}
	for _, raw := range cases {
		if wired.Execute(context.Background(), json.RawMessage(raw)).OK {
			t.Fatalf("应拒绝 %s", raw)
		}
	}
	if (SendMessage{}).Execute(context.Background(), json.RawMessage(`{"to_run_id":"run-c","content":"x"}`)).OK {
		t.Fatal("未装配 Deliver 应失败关闭")
	}
	failing := SendMessage{Deliver: func(context.Context, string, string, string) error {
		return errors.New("target missing")
	}}
	result := failing.Execute(context.Background(), json.RawMessage(`{"to_run_id":"run-c","content":"x"}`))
	if result.OK || result.Code != "deliver_failed" {
		t.Fatalf("投递失败应 deliver_failed: %+v", result)
	}
}
