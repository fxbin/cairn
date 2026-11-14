package mcp

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// historySource 仅记录目录投影；额外外部入口用于断言历史回放没有执行或连接。
type historySource struct {
	resets        int
	accepted      []string
	results       []contract.ToolResult
	active        bool
	externalCalls int
	acceptErr     error
}

func (source *historySource) ResetMCPProjection() {
	source.resets++
	source.active = false
	source.accepted = nil
	source.results = nil
}
func (source *historySource) AcceptMCPResult(name string, result contract.ToolResult) error {
	if source.acceptErr != nil {
		return source.acceptErr
	}
	source.accepted = append(source.accepted, name)
	source.results = append(source.results, result)
	if (name == "mcp_discover" || name == "tool_search") && result.OK {
		source.active = true
	}
	return nil
}
func (source *historySource) Connect() { source.externalCalls++ }
func (source *historySource) Execute() { source.externalCalls++ }

// encodeHistoryPayload 生成真实 Journal 事件载荷，编码失败立即停止测试。
func encodeHistoryPayload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// historyEvents 保持提议、成功终态和工具回执的原始写入顺序。
func historyEvents(t *testing.T, name string, result contract.ToolResult) []domain.Event {
	t.Helper()
	operation := domain.NewOperationID()
	return []domain.Event{
		{Type: domain.EventToolCallProposed, OperationID: &operation, Payload: encodeHistoryPayload(t, proposalPayload{CallID: "call", ToolName: name, Arguments: json.RawMessage(`{}`)})},
		{Type: domain.EventOperationSucceeded, OperationID: &operation, Payload: encodeHistoryPayload(t, successReceipt{ToolName: name, Result: result})},
		{Type: domain.EventToolResultAppended, OperationID: &operation, Payload: encodeHistoryPayload(t, result)},
	}
}

// replayHistory 按调用方合同先清空再逐事件推进，保留失败发生的位置。
func replayHistory(history *History, events []domain.Event) error {
	if err := history.BeginReplay(events); err != nil {
		return err
	}
	for _, event := range events {
		if err := history.ReplayEvent(event); err != nil {
			return err
		}
	}
	return nil
}

// TestHistoryHandlesMCPNamesAndForwardsReceipts 锁定名称路由及显式实时回执转发。
func TestHistoryHandlesMCPNamesAndForwardsReceipts(t *testing.T) {
	source := &historySource{}
	history := NewHistory(source)
	for _, name := range []string{"mcp_discover", "tool_search", "mcp__docs__lookup", contract.MCPQuiesceTool} {
		if !history.HandlesToolResult(name) {
			t.Fatalf("MCP name unhandled: %s", name)
		}
		result := contract.ToolFailure("denied", "not executed")
		if err := history.ObserveToolResult(name, result); err != nil {
			t.Fatal(err)
		}
		if source.accepted[len(source.accepted)-1] != name || !reflect.DeepEqual(source.results[len(source.results)-1], result) {
			t.Fatal("live receipt changed")
		}
	}
	for _, name := range []string{"", "skill", "read_file", "mcp_discover_other", "tool_search_other", "mcp_docs"} {
		if history.HandlesToolResult(name) {
			t.Fatalf("unrelated tool handled: %s", name)
		}
	}
	sentinel := errors.New("reject catalog")
	source.acceptErr = sentinel
	if err := history.ObserveToolResult("mcp_discover", contract.ToolSuccess(struct{}{}, "", 0, "", false)); !errors.Is(err, sentinel) {
		t.Fatalf("source error lost: %v", err)
	}
	if source.externalCalls != 0 {
		t.Fatal("observer issued an external request")
	}
}

// TestHistoryReplaysOnlyMatchingSuccessfulHelpers 覆盖两种目录帮助工具和规范化 JSON 一致性。
func TestHistoryReplaysOnlyMatchingSuccessfulHelpers(t *testing.T) {
	for _, name := range []string{"mcp_discover", "tool_search"} {
		t.Run(name, func(t *testing.T) {
			source := &historySource{active: true}
			history := NewHistory(source)
			result := contract.ToolResult{OK: true, Code: "ok", Data: json.RawMessage(`{"a":1,"b":2}`), Preview: "catalog", Bytes: 13, SHA256: "sha256:catalog"}
			events := historyEvents(t, name, result)
			reordered := result
			reordered.Data = json.RawMessage(`{ "b":2, "a":1 }`)
			events[2].Payload = encodeHistoryPayload(t, reordered)
			if err := history.BeginReplay(events); err != nil {
				t.Fatal(err)
			}
			if source.active || len(source.accepted) != 0 || source.resets != 1 {
				t.Fatal("BeginReplay consumed history or retained old catalog")
			}
			for i, event := range events {
				if err := history.ReplayEvent(event); err != nil {
					t.Fatal(err)
				}
				if i < 2 && source.active {
					t.Fatal("catalog activated before matching tool result")
				}
			}
			if !source.active || !reflect.DeepEqual(source.accepted, []string{name}) {
				t.Fatalf("catalog missing: %+v", source)
			}
			if source.externalCalls != 0 {
				t.Fatal("history replay issued an external request")
			}
		})
	}
}

// TestHistoryRejectsMissingForgedOrFailedReceipts 锁定操作身份、工具名与完整结果一致边界。
func TestHistoryRejectsMissingForgedOrFailedReceipts(t *testing.T) {
	result := contract.ToolSuccess(struct {
		Count int `json:"count"`
	}{1}, "catalog", 12, "sha256:one", false)
	tests := []struct {
		name    string
		change  func([]domain.Event) []domain.Event
		message string
	}{
		{"missing success", func(events []domain.Event) []domain.Event { return []domain.Event{events[0], events[2]} }, "no successful operation receipt"},
		{"failed terminal", func(events []domain.Event) []domain.Event {
			events[1].Type = domain.EventOperationFailed
			return events
		}, "no successful operation receipt"},
		{"other operation", func(events []domain.Event) []domain.Event {
			other := domain.NewOperationID()
			events[1].OperationID = &other
			return events
		}, "no successful operation receipt"},
		{"other tool", func(events []domain.Event) []domain.Event {
			events[1].Payload = encodeHistoryPayload(t, successReceipt{ToolName: "tool_search", Result: result})
			return events
		}, "no successful operation receipt"},
		{"failed result in success", func(events []domain.Event) []domain.Event {
			events[1].Payload = encodeHistoryPayload(t, successReceipt{ToolName: "mcp_discover", Result: contract.ToolFailure("denied", "no catalog")})
			return events
		}, "no successful operation receipt"},
		{"non-ok success code", func(events []domain.Event) []domain.Event {
			forged := result
			forged.Code = "other"
			events[1].Payload = encodeHistoryPayload(t, successReceipt{ToolName: "mcp_discover", Result: forged})
			return events
		}, "no successful operation receipt"},
		{"malformed success", func(events []domain.Event) []domain.Event {
			events[1].Payload = json.RawMessage(`{"result":`)
			return events
		}, "no successful operation receipt"},
		{"malformed appended", func(events []domain.Event) []domain.Event {
			events[2].Payload = json.RawMessage(`{"ok":`)
			return events
		}, "decode persisted MCP result failed"},
	}
	for _, field := range []string{"data", "preview", "bytes", "sha256", "truncated"} {
		tests = append(tests, struct {
			name    string
			change  func([]domain.Event) []domain.Event
			message string
		}{"changed " + field, func(events []domain.Event) []domain.Event {
			forged := result
			switch field {
			case "data":
				forged.Data = json.RawMessage(`{"count":2}`)
			case "preview":
				forged.Preview = "other"
			case "bytes":
				forged.Bytes++
			case "sha256":
				forged.SHA256 = "sha256:other"
			case "truncated":
				forged.Truncated = true
			}
			events[2].Payload = encodeHistoryPayload(t, forged)
			return events
		}, "does not match its operation receipt"})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := &historySource{active: true}
			history := NewHistory(source)
			err := replayHistory(history, test.change(historyEvents(t, "mcp_discover", result)))
			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Fatalf("err=%v want=%s", err, test.message)
			}
			if source.active || len(source.accepted) != 0 || source.externalCalls != 0 {
				t.Fatalf("invalid receipt granted catalog or external call: %+v", source)
			}
		})
	}
}

// TestHistoryFailedAndUnrelatedResultsCannotGrantCatalog 保持失败结果、动态调用和回收历史的原有非授予行为。
func TestHistoryFailedAndUnrelatedResultsCannotGrantCatalog(t *testing.T) {
	success := contract.ToolSuccess(struct{}{}, "catalog", 0, "", false)
	for _, name := range []string{"mcp__docs__lookup", contract.MCPQuiesceTool, "read_file"} {
		t.Run(name, func(t *testing.T) {
			events := historyEvents(t, name, success)
			// 非目录调用不要求新增成功操作回执，也不解析其工具结果为目录证据。
			events = append(events[:1], events[2])
			events[1].Payload = json.RawMessage(`not JSON`)
			source := &historySource{}
			if err := replayHistory(NewHistory(source), events); err != nil {
				t.Fatal(err)
			}
			if source.active || len(source.accepted) != 0 || source.externalCalls != 0 {
				t.Fatal("non-helper history granted catalog or executed")
			}
		})
	}
	for _, result := range []contract.ToolResult{contract.ToolFailure("denied", "denied"), {OK: true, Code: "not_ok"}} {
		events := historyEvents(t, "mcp_discover", result)
		events = append(events[:1], events[2])
		source := &historySource{}
		if err := replayHistory(NewHistory(source), events); err != nil {
			t.Fatal(err)
		}
		if source.active || len(source.accepted) != 0 {
			t.Fatal("failed appended result granted catalog")
		}
	}
}

// TestHistoryRepeatedReplayClearsCatalogAndTemporaryEvidence 防止已有目录、提议或成功映射污染下次恢复。
func TestHistoryRepeatedReplayClearsCatalogAndTemporaryEvidence(t *testing.T) {
	source := &historySource{}
	history := NewHistory(source)
	result := contract.ToolSuccess(struct{}{}, "catalog", 0, "", false)
	events := historyEvents(t, "mcp_discover", result)
	if err := replayHistory(history, events); err != nil || !source.active {
		t.Fatalf("initial replay: %v", err)
	}
	if err := replayHistory(history, events[2:]); err != nil {
		t.Fatal(err)
	}
	if source.active || len(source.accepted) != 0 {
		t.Fatal("old proposal survived BeginReplay")
	}
	if err := history.BeginReplay(nil); err != nil {
		t.Fatal(err)
	}
	if err := history.ReplayEvent(events[0]); err != nil {
		t.Fatal(err)
	}
	if err := history.ReplayEvent(events[2]); err == nil {
		t.Fatal("old successful receipt survived BeginReplay")
	}
	if source.active || len(source.accepted) != 0 {
		t.Fatal("stale receipt granted catalog")
	}
	if err := replayHistory(history, events); err != nil || !source.active {
		t.Fatalf("rebuild after reset failed: %v", err)
	}
	if source.resets != 4 || source.externalCalls != 0 {
		t.Fatalf("resets=%d external=%d", source.resets, source.externalCalls)
	}
}

// TestHistoryPreservesProposalDecodeAndModelEventBoundaries 不放宽旧提议字段解码，不消费模型请求事件。
func TestHistoryPreservesProposalDecodeAndModelEventBoundaries(t *testing.T) {
	source := &historySource{}
	history := NewHistory(source)
	result := contract.ToolSuccess(struct{}{}, "catalog", 0, "", false)
	events := historyEvents(t, "mcp_discover", result)
	events[0].Payload = json.RawMessage(`{"call_id":12,"tool_name":"mcp_discover","arguments":{}}`)
	if err := replayHistory(history, events); err != nil {
		t.Fatal(err)
	}
	if source.active || len(source.accepted) != 0 {
		t.Fatal("invalid proposal shape granted catalog")
	}
	if err := history.ReplayEvent(domain.Event{Type: domain.EventModelRequestStarted, Payload: json.RawMessage(`not JSON`)}); err != nil {
		t.Fatal(err)
	}
	if source.active || len(source.accepted) != 0 {
		t.Fatal("model request consumed catalog history")
	}
	events = historyEvents(t, "tool_search", result)
	sentinel := errors.New("catalog decode failed")
	source.acceptErr = sentinel
	if err := replayHistory(history, events); !errors.Is(err, sentinel) {
		t.Fatalf("replay lost source error: %v", err)
	}
}

// TestHistoryMissingSourceFailsClosed 锁定显式装配要求，不从隐藏全局能力恢复目录。
func TestHistoryMissingSourceFailsClosed(t *testing.T) {
	for _, history := range []*History{nil, NewHistory(nil)} {
		if err := history.BeginReplay(nil); err == nil {
			t.Fatal("missing source began replay")
		}
		if err := history.ReplayEvent(domain.Event{}); err == nil {
			t.Fatal("missing source consumed history")
		}
		if err := history.ObserveToolResult("mcp_discover", contract.ToolSuccess(struct{}{}, "", 0, "", false)); err == nil {
			t.Fatal("missing source accepted result")
		}
	}
}
