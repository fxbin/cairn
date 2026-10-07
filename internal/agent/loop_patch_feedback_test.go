package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// TestLoopPatchRecoveryFeedbackPrecedesAnyWrite 通过真实工具与 Journal 验证三类拒绝反馈和精确重提的唯一写入。
func TestLoopPatchRecoveryFeedbackPrecedesAnyWrite(t *testing.T) {
	root := t.TempDir()
	original := []byte("if ready:\n\treturn result\n")
	target := filepath.Join(root, "file.txt")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace}, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)
	hash := "sha256:" + hex.EncodeToString(digest[:])
	mixed, err := json.Marshal(tools.PatchArguments{Path: "file.txt", Mode: "update", ExpectedSHA256: hash, Content: func() *string { value := "replacement"; return &value }(), Replacements: []tools.Replacement{{Old: "ready", New: "done"}}})
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := json.Marshal(tools.PatchArguments{Path: "file.txt", Mode: "update", ExpectedSHA256: hash, Replacements: []tools.Replacement{{Old: "if ready:\n    return result\n", New: "replacement"}}})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := json.Marshal(tools.PatchArguments{Path: "file.txt", Mode: "update", ExpectedSHA256: hash, Replacements: []tools.Replacement{{Old: string(original), New: "exact retry succeeded\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"file.txt"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "missing", Name: "apply_patch", Arguments: json.RawMessage(`{"content":"replacement"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "mixed", Name: "apply_patch", Arguments: mixed}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "mismatch", Name: "apply_patch", Arguments: mismatch}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "exact", Name: "apply_patch", Arguments: exact}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "completed exact retry"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "patch feedback")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 6}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "patch feedback", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 6 {
		t.Fatalf("模型请求数: %d", len(provider.requests))
	}
	for _, expected := range []struct {
		id   string
		code string
	}{
		{id: "missing", code: "invalid_patch_arguments"},
		{id: "mixed", code: "invalid_patch_arguments"},
		{id: "mismatch", code: "patch_text_mismatch"},
	} {
		found := false
		for _, message := range provider.requests[5].Messages {
			if message.Role != "tool" || message.ToolCallID != expected.id {
				continue
			}
			found = true
			var result tools.Result
			if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
				t.Fatal(err)
			}
			if result.OK || result.Code != expected.code || !json.Valid(result.Data) || !strings.Contains(string(result.Data), "next_action") || strings.Contains(string(result.Data), "return result") {
				t.Fatalf("模型未收到安全恢复结果: id=%s code=%s", expected.id, result.Code)
			}
			if summary := summarizeToolResult("apply_patch", message.Content); !strings.Contains(summary, expected.code) || !strings.Contains(summary, "preview=") {
				t.Fatal("摘要丢失错误分类或恢复预览")
			}
		}
		if !found {
			t.Fatalf("模型未收到 %s 的结果", expected.id)
		}
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	patchCalls := make(map[domain.OperationID]string)
	patchStarts := 0
	failures := 0
	for _, event := range events {
		if event.Type == domain.EventToolCallProposed && event.OperationID != nil {
			var proposed struct {
				CallID   string `json:"call_id"`
				ToolName string `json:"tool_name"`
			}
			if err := json.Unmarshal(event.Payload, &proposed); err != nil {
				t.Fatal(err)
			}
			if proposed.ToolName == "apply_patch" {
				patchCalls[*event.OperationID] = proposed.CallID
			}
		}
		if event.OperationID == nil || patchCalls[*event.OperationID] == "" {
			continue
		}
		if event.Type == domain.EventOperationStarted {
			patchStarts++
			if patchCalls[*event.OperationID] != "exact" {
				t.Fatal("拒绝补丁仍开始写入操作")
			}
		}
		if event.Type == domain.EventOperationFailed {
			failures++
			var payload struct {
				Result tools.Result `json:"result"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil || !json.Valid(payload.Result.Data) {
				t.Fatalf("失败 Journal 丢失结构化恢复反馈: %v", err)
			}
		}
	}
	if patchStarts != 1 || failures != 3 {
		t.Fatalf("写入/拒绝序列错误: starts=%d failures=%d", patchStarts, failures)
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != "exact retry succeeded\n" {
		t.Fatalf("精确重提未产生预期内容: %v", err)
	}
}
