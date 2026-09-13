package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// TestSummarizeMessagesDropsSecretLikeExecArguments 锁定审计 P1
// (model-tool-argv-secret-persistence) 验收第 2 条的压缩层合同：探索清单对
// exec_command 只保留 argv 首词，敏感命令行参数不得进入 context_compacted
// 摘要，也就无法随检查点进入后续 Provider 请求。
func TestSummarizeMessagesDropsSecretLikeExecArguments(t *testing.T) {
	encoded, err := json.Marshal(tools.Success(map[string]any{"preview": "command finished"}, "command finished", 16, "", false))
	if err != nil {
		t.Fatal(err)
	}
	messages := []ports.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "legacy_exec", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["curl","--password=sh0rt","-H","X-Token: abc123xyz","https://example.invalid"]}`)}}},
		{Role: "tool", ToolCallID: "legacy_exec", Content: string(encoded)},
		{Role: "assistant", Content: "已执行探测命令，基于已有发现推进。"},
	}
	summary := summarizeMessages(messages, 2_048)
	if !strings.Contains(summary, "curl") {
		t.Fatalf("inventory must keep the executable for exploration memory: %q", summary)
	}
	for _, leaked := range []string{"password", "sh0rt", "X-Token", "abc123xyz", "example.invalid"} {
		if strings.Contains(summary, leaked) {
			t.Fatalf("checkpoint leaked secret-like exec argument %q: %q", leaked, summary)
		}
	}
}

// TestMessagesFromEventsReplayKeepsLegacySecretOutOfCheckpoint 覆盖修复前遗留
// journal 的恢复路径：model_response_finished 中的敏感 argv 会如实重放进
// assistant 消息（journal 是事实源），但触发压缩后检查点只能暴露 argv 首词，
// 凭据不得进入 context_compacted 摘要或下一次 Provider 请求。
func TestMessagesFromEventsReplayKeepsLegacySecretOutOfCheckpoint(t *testing.T) {
	operation := domain.OperationID("op-leak")
	events := []domain.Event{
		{Seq: 1, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"probe legacy","workspace":"/w"}`)},
		{Seq: 2, Type: domain.EventModelResponseFinished, Payload: json.RawMessage(`{"tool_calls":[{"id":"leak","name":"exec_command","arguments":{"argv":["curl","--password=sh0rt"]}}]}`)},
		{Seq: 3, Type: domain.EventToolCallProposed, OperationID: &operation, Payload: json.RawMessage(`{"call_id":"leak","tool_name":"exec_command","arguments":{"argv":["curl","--password=sh0rt"]}}`)},
		{Seq: 4, Type: domain.EventToolResultAppended, OperationID: &operation, Payload: json.RawMessage(`{"ok":true,"code":"ok","preview":"command finished"}`)},
	}
	loop := Loop{}
	rebuilt, err := loop.messagesFromEvents(events, "fallback")
	if err != nil {
		t.Fatal(err)
	}
	var replayedRaw bool
	for _, message := range rebuilt {
		for _, call := range message.ToolCalls {
			if strings.Contains(string(call.Arguments), "sh0rt") {
				replayedRaw = true
			}
		}
	}
	if !replayedRaw {
		t.Fatalf("journal replay must faithfully carry legacy facts; test fixture broken: %#v", rebuilt)
	}
	summary := summarizeMessages(rebuilt, 2_048)
	if !strings.Contains(summary, "curl") {
		t.Fatalf("checkpoint inventory must keep the executable: %q", summary)
	}
	for _, leaked := range []string{"password", "sh0rt"} {
		if strings.Contains(summary, leaked) {
			t.Fatalf("checkpoint leaked legacy secret %q after replay: %q", leaked, summary)
		}
	}
}
