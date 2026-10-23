// subagent_test 锁定父子委派载荷的校验与可回放编码：空标识、自派发、
// 空任务拒绝；摘要稳定；完成载荷拒绝空 outcome。
//
// Author: fxbin
package app

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
)

// TestValidateDelegateRejectsInvalid 锁定非法委派失败关闭。
func TestValidateDelegateRejectsInvalid(t *testing.T) {
	cases := []DelegateOptions{
		{ParentRunID: "", ChildRunID: "c1", Task: "t"},
		{ParentRunID: "p1", ChildRunID: "", Task: "t"},
		{ParentRunID: "p1", ChildRunID: "p1", Task: "t"},
		{ParentRunID: "p1", ChildRunID: "c1", Task: "  "},
	}
	for _, options := range cases {
		if err := ValidateDelegate(options); !errors.Is(err, ErrInvalidDelegation) {
			t.Fatalf("期望 ErrInvalidDelegation，得到 %v（%+v）", err, options)
		}
	}
	if err := ValidateDelegate(DelegateOptions{ParentRunID: "p1", ChildRunID: "c1", Task: "调研接口"}); err != nil {
		t.Fatalf("合法委派不应失败: %v", err)
	}
}

// TestTaskDigestStable 锁定任务摘要对同一输入稳定且带 sha256 前缀。
func TestTaskDigestStable(t *testing.T) {
	first := TaskDigest("调查模块边界")
	second := TaskDigest("调查模块边界")
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("摘要不稳定或前缀错误: %q / %q", first, second)
	}
	if first == TaskDigest("调查模块边界 ") {
		t.Fatal("不同任务不应产生相同摘要")
	}
}

// TestEncodeDelegatedRoundTrip 锁定委派载荷可回放字段完整。
func TestEncodeDelegatedRoundTrip(t *testing.T) {
	raw, err := EncodeDelegated(DelegateOptions{
		ParentRunID: domain.RunID("run-parent"),
		ChildRunID:  domain.RunID("run-child"),
		Task:        "只读盘点工具面",
		Budget:      SubagentBudget{MaxTurns: 3, MaxTokens: 4096},
	})
	if err != nil {
		t.Fatalf("编码委派失败: %v", err)
	}
	var payload DelegatedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解码委派失败: %v", err)
	}
	if payload.ChildRunID != "run-child" {
		t.Fatalf("child_run_id 不符: %+v", payload)
	}
	if payload.TaskDigest != TaskDigest("只读盘点工具面") {
		t.Fatalf("task_digest 不符: %q", payload.TaskDigest)
	}
	if payload.Budget.MaxTurns != 3 || payload.Budget.MaxTokens != 4096 {
		t.Fatalf("budget 不符: %+v", payload.Budget)
	}
	if _, err := EncodeDelegated(DelegateOptions{ParentRunID: "p", ChildRunID: "p", Task: "t"}); !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("自派发应拒绝: %v", err)
	}
}

// TestEncodeCompletedRequiresOutcome 锁定完成载荷必填字段与往返。
func TestEncodeCompletedRequiresOutcome(t *testing.T) {
	if _, err := EncodeCompleted("run-child", "", nil); !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("空 outcome 应拒绝: %v", err)
	}
	if _, err := EncodeCompleted("", "ok", nil); !errors.Is(err, ErrInvalidDelegation) {
		t.Fatalf("空 child_run_id 应拒绝: %v", err)
	}
	raw, err := EncodeCompleted("run-child", "ok", json.RawMessage(`{"prompt_tokens":10}`))
	if err != nil {
		t.Fatalf("编码完成失败: %v", err)
	}
	var payload CompletedPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("解码完成失败: %v", err)
	}
	if payload.ChildRunID != "run-child" || payload.Outcome != "ok" {
		t.Fatalf("完成载荷不符: %+v", payload)
	}
}
