package skills

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// historySourceFixture 只实现历史接纳方法，测试不提供执行、文件或网络入口。
type historySourceFixture struct {
	tasks    []string
	names    []string
	accepted []contract.ToolResult
	err      error
}

func (source *historySourceFixture) PrepareSkills(task string) {
	source.tasks = append(source.tasks, task)
	source.names = nil
	source.accepted = nil
}

func (source *historySourceFixture) AcceptSkillResult(name string, result contract.ToolResult) error {
	if source.err != nil {
		return source.err
	}
	if result.OK && result.Code == "ok" {
		source.names = append(source.names, name)
		source.accepted = append(source.accepted, result)
	}
	return nil
}

// historyEvent 保持真实历史载荷字段，不经其他实现转换事件。
func historyEvent(t *testing.T, kind domain.EventType, operation domain.OperationID, payload any) domain.Event {
	t.Helper()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event := domain.Event{Type: kind, Payload: encoded}
	if operation != "" {
		event.OperationID = &operation
	}
	return event
}

func historyProposal(t *testing.T, operation domain.OperationID, name string) domain.Event {
	t.Helper()
	return historyEvent(t, domain.EventToolCallProposed, operation, struct {
		ToolName string `json:"tool_name"`
	}{ToolName: name})
}

func historyReceipt(t *testing.T, operation domain.OperationID, name string, result contract.ToolResult) domain.Event {
	t.Helper()
	return historyEvent(t, domain.EventOperationSucceeded, operation, successReceipt{ToolName: name, Result: result})
}

func historyResult(t *testing.T) contract.ToolResult {
	t.Helper()
	content := contract.SkillContent{Version: contract.SkillContentVersion, Name: "review-code", Description: "核对代码", Scope: "workspace", SourceDigest: "source-identity", SHA256: "body-identity", Instructions: "完整历史正文，不授予命令或工具权限。", Resources: []contract.SkillResource{{Path: "references/rules.md", Bytes: 12, SHA256: "resource-identity"}}}
	encoded, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	return contract.ToolResult{OK: true, Code: "ok", Data: encoded, Preview: "loaded skill", Bytes: int64(len(encoded)), SHA256: "result-identity"}
}

func TestHistoryReplayPreservesSuccessfulHistoricalSnapshot(t *testing.T) {
	source := &historySourceFixture{}
	history := NewHistory(source)
	result := historyResult(t)
	const operation domain.OperationID = "skill-load"
	events := []domain.Event{
		historyEvent(t, domain.EventRunCreated, "", domain.RunCreatedPayload{Task: "/skill:review-code 核对"}),
		historyProposal(t, operation, "skill"),
		historyReceipt(t, operation, "skill", result),
		historyEvent(t, domain.EventToolResultAppended, operation, result),
	}
	if err := history.BeginReplay(events); err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if err := history.ReplayEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(source.tasks, []string{"/skill:review-code 核对"}) || !reflect.DeepEqual(source.names, []string{"skill"}) || !reflect.DeepEqual(source.accepted, []contract.ToolResult{result}) {
		t.Fatalf("historical task or complete result changed: %+v", source)
	}
}

func TestHistoryReplayRejectsMissingForgedOrOutOfOrderReceipts(t *testing.T) {
	result := historyResult(t)
	const operation domain.OperationID = "skill-load"
	for _, test := range []struct {
		name           string
		receiptName    string
		mutate         func(*contract.ToolResult)
		omit           bool
		otherOperation bool
		late           bool
		malformed      bool
	}{
		{name: "missing", omit: true},
		{name: "other-tool", receiptName: "read_file"},
		{name: "failed-receipt", mutate: func(r *contract.ToolResult) { r.OK = false }},
		{name: "non-ok-receipt", mutate: func(r *contract.ToolResult) { r.Code = "failed" }},
		{name: "forged-body", mutate: func(r *contract.ToolResult) { r.Data = json.RawMessage(`{"instructions":"forged"}`) }},
		{name: "forged-preview", mutate: func(r *contract.ToolResult) { r.Preview = "forged" }},
		{name: "forged-bytes", mutate: func(r *contract.ToolResult) { r.Bytes++ }},
		{name: "forged-digest", mutate: func(r *contract.ToolResult) { r.SHA256 = "forged" }},
		{name: "forged-truncation", mutate: func(r *contract.ToolResult) { r.Truncated = true }},
		{name: "other-operation", otherOperation: true},
		{name: "late-receipt", late: true},
		{name: "malformed-receipt", malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &historySourceFixture{}
			history := NewHistory(source)
			if err := history.BeginReplay(nil); err != nil {
				t.Fatal(err)
			}
			if err := history.ReplayEvent(historyProposal(t, operation, "skill")); err != nil {
				t.Fatal(err)
			}
			receiptResult := result
			if test.mutate != nil {
				test.mutate(&receiptResult)
			}
			name := test.receiptName
			if name == "" {
				name = "skill"
			}
			receiptOperation := operation
			if test.otherOperation {
				receiptOperation = "other-operation"
			}
			receipt := historyReceipt(t, receiptOperation, name, receiptResult)
			if test.malformed {
				receipt.Payload = json.RawMessage(`{`)
			}
			if !test.omit && !test.late {
				if err := history.ReplayEvent(receipt); err != nil {
					t.Fatal(err)
				}
			}
			err := history.ReplayEvent(historyEvent(t, domain.EventToolResultAppended, operation, result))
			if err == nil || len(source.accepted) != 0 {
				t.Fatalf("unverified skill activated: accepted=%d err=%v", len(source.accepted), err)
			}
		})
	}
}

func TestHistoryReplayFailedAndUnrelatedResultsDoNotActivate(t *testing.T) {
	result := historyResult(t)
	const operation domain.OperationID = "result-op"
	for _, test := range []struct {
		name      string
		proposal  string
		result    contract.ToolResult
		malformed bool
	}{
		{name: "failed", proposal: "skill", result: contract.ToolResult{OK: false, Code: "failed"}},
		{name: "non-ok", proposal: "skill", result: contract.ToolResult{OK: true, Code: "not-ok"}},
		{name: "missing-proposal", result: result},
		{name: "other-tool", proposal: "read_file", result: result},
		{name: "other-tool-malformed-result", proposal: "read_file", malformed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := &historySourceFixture{}
			history := NewHistory(source)
			if err := history.BeginReplay(nil); err != nil {
				t.Fatal(err)
			}
			if test.proposal != "" {
				if err := history.ReplayEvent(historyProposal(t, operation, test.proposal)); err != nil {
					t.Fatal(err)
				}
			}
			if err := history.ReplayEvent(historyReceipt(t, operation, "skill", result)); err != nil {
				t.Fatal(err)
			}
			event := historyEvent(t, domain.EventToolResultAppended, operation, test.result)
			if test.malformed {
				event.Payload = json.RawMessage(`{`)
			}
			if err := history.ReplayEvent(event); err != nil || len(source.accepted) != 0 {
				t.Fatalf("unrelated or failed result activated: accepted=%d err=%v", len(source.accepted), err)
			}
		})
	}
}

func TestHistoryBeginReplayClearsTemporaryAndActivatedState(t *testing.T) {
	source := &historySourceFixture{}
	history := NewHistory(source)
	result := historyResult(t)
	const operation domain.OperationID = "same-operation"
	created := historyEvent(t, domain.EventRunCreated, "", domain.RunCreatedPayload{Task: "first task"})
	if err := history.BeginReplay([]domain.Event{created}); err != nil {
		t.Fatal(err)
	}
	for _, event := range []domain.Event{historyProposal(t, operation, "skill"), historyReceipt(t, operation, "skill", result), historyEvent(t, domain.EventToolResultAppended, operation, result)} {
		if err := history.ReplayEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(source.accepted) != 1 {
		t.Fatal("first replay did not activate fixture")
	}
	if err := history.BeginReplay(nil); err != nil {
		t.Fatal(err)
	}
	if len(source.accepted) != 0 || !reflect.DeepEqual(source.tasks, []string{"first task", ""}) {
		t.Fatal("new replay retained prior activation or task")
	}
	if err := history.ReplayEvent(historyEvent(t, domain.EventToolResultAppended, operation, result)); err != nil || len(source.accepted) != 0 {
		t.Fatal("previous proposal survived BeginReplay")
	}
	if err := history.ReplayEvent(historyProposal(t, operation, "skill")); err != nil {
		t.Fatal(err)
	}
	if err := history.ReplayEvent(historyEvent(t, domain.EventToolResultAppended, operation, result)); err == nil || len(source.accepted) != 0 {
		t.Fatal("previous success receipt survived BeginReplay")
	}
}

func TestHistoryObserveAndFailureBoundaries(t *testing.T) {
	source := &historySourceFixture{}
	history := NewHistory(source)
	result := historyResult(t)
	if !history.HandlesToolResult("skill") || history.HandlesToolResult("skill_extra") || history.HandlesToolResult("read_file") {
		t.Fatal("unexpected handled tools")
	}
	if err := history.ObserveToolResult("read_file", result); err != nil || len(source.accepted) != 0 {
		t.Fatal("unrelated result reached source")
	}
	if err := history.ObserveToolResult("skill", result); err != nil || len(source.accepted) != 1 {
		t.Fatal("persisted skill result was not accepted")
	}
	sentinel := errors.New("source rejected historical content")
	source.err = sentinel
	if err := history.ObserveToolResult("skill", result); !errors.Is(err, sentinel) {
		t.Fatalf("source error lost: %v", err)
	}
	if err := history.BeginReplay(nil); err != nil {
		t.Fatal(err)
	}
	for _, event := range []domain.Event{historyProposal(t, "op", "skill"), historyReceipt(t, "op", "skill", result)} {
		if err := history.ReplayEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if err := history.ReplayEvent(historyEvent(t, domain.EventToolResultAppended, "op", result)); !errors.Is(err, sentinel) {
		t.Fatalf("replay source error lost: %v", err)
	}
	for _, missing := range []*History{nil, NewHistory(nil)} {
		if err := missing.BeginReplay(nil); err == nil {
			t.Fatal("missing source accepted replay")
		}
		if err := missing.ObserveToolResult("skill", result); err == nil {
			t.Fatal("missing source accepted skill")
		}
		if err := missing.ObserveToolResult("read_file", result); err != nil {
			t.Fatal("unrelated result failed")
		}
		if err := missing.ReplayEvent(domain.Event{Type: domain.EventRunCreated}); err != nil {
			t.Fatal("unrelated event failed")
		}
	}
	if err := NewHistory(source).ReplayEvent(historyProposal(t, "op", "skill")); err == nil {
		t.Fatal("replay before BeginReplay did not fail closed")
	}
	malformed := historyEvent(t, domain.EventRunCreated, "", domain.RunCreatedPayload{})
	malformed.Payload = json.RawMessage(`{`)
	if err := history.BeginReplay([]domain.Event{malformed}); err == nil || !strings.Contains(err.Error(), "decode skill task context") {
		t.Fatalf("malformed task not rejected: %v", err)
	}
	if err := history.BeginReplay(nil); err != nil {
		t.Fatal(err)
	}
	if err := history.ReplayEvent(historyProposal(t, "bad", "skill")); err != nil {
		t.Fatal(err)
	}
	malformed = historyEvent(t, domain.EventToolResultAppended, "bad", result)
	malformed.Payload = json.RawMessage(`{`)
	if err := history.ReplayEvent(malformed); err == nil || !strings.Contains(err.Error(), "decode persisted skill result") {
		t.Fatalf("malformed result not rejected: %v", err)
	}
}

// TestHistoryRejectsCanonicalInvalidResults 保持旧指纹器的失败关闭语义，即使两份
// 成功载荷都含同一歧义数据，也不能绕过 canonical 校验激活技能。
func TestHistoryRejectsCanonicalInvalidResults(t *testing.T) {
	for _, data := range []json.RawMessage{json.RawMessage(`{"name":"first","name":"second"}`), json.RawMessage(`{"value":1.5}`)} {
		source := &historySourceFixture{}
		history := NewHistory(source)
		if err := history.BeginReplay(nil); err != nil {
			t.Fatal(err)
		}
		result := historyResult(t)
		result.Data = data
		for _, event := range []domain.Event{historyProposal(t, "op", "skill"), historyReceipt(t, "op", "skill", result)} {
			if err := history.ReplayEvent(event); err != nil {
				t.Fatal(err)
			}
		}
		if err := history.ReplayEvent(historyEvent(t, domain.EventToolResultAppended, "op", result)); err == nil || len(source.accepted) != 0 {
			t.Fatal("canonical-invalid matching results activated skill")
		}
	}
}

// TestHistoryPreservesProposalDecodeBoundary 保留旧 Core 对 call_id 类型的解码
// 边界；错误提议不能仅凭名称和成功载荷被新投影接纳。
func TestHistoryPreservesProposalDecodeBoundary(t *testing.T) {
	source := &historySourceFixture{}
	history := NewHistory(source)
	if err := history.BeginReplay(nil); err != nil {
		t.Fatal(err)
	}
	proposal := historyProposal(t, "op", "skill")
	proposal.Payload = json.RawMessage(`{"call_id":123,"tool_name":"skill","arguments":{}}`)
	if err := history.ReplayEvent(proposal); err != nil {
		t.Fatal(err)
	}
	result := historyResult(t)
	for _, event := range []domain.Event{historyReceipt(t, "op", "skill", result), historyEvent(t, domain.EventToolResultAppended, "op", result)} {
		if err := history.ReplayEvent(event); err != nil {
			t.Fatal(err)
		}
	}
	if len(source.accepted) != 0 {
		t.Fatal("malformed proposal activated skill")
	}
}
