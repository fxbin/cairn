package agent

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// inventoryReadMessages 构造配对回执；覆盖范围只来自回执元数据，正文不参与清单。
func inventoryReadMessages(t *testing.T, id, path, hash string, offset, limit, total int64) []ports.Message {
	t.Helper()
	arguments, err := json.Marshal(struct {
		Path string `json:"path"`
	}{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	result := tools.Success(struct {
		Path       string `json:"path"`
		Offset     int64  `json:"offset"`
		Limit      int64  `json:"limit"`
		TotalBytes int64  `json:"total_bytes"`
		Content    string `json:"content"`
	}{Path: path, Offset: offset, Limit: limit, TotalBytes: total, Content: "SOURCE_BODY_MUST_NOT_BE_COPIED"}, "safe preview", total, hash, false)
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return []ports.Message{
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: id, Name: "read_file", Arguments: arguments}}},
		{Role: "tool", ToolCallID: id, Content: string(raw)},
	}
}

// TestExplorationInventoryUsesSuccessfulReadRanges 拒绝虚构已读证据，按文件版本合并实际范围。
func TestExplorationInventoryUsesSuccessfulReadRanges(t *testing.T) {
	hashA := "sha256:" + strings.Repeat("a", 64)
	hashB := "sha256:" + strings.Repeat("b", 64)
	messages := []ports.Message{
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "denied", Name: "read_file", Arguments: json.RawMessage(`{"path":"denied.go"}`)}}},
		{Role: "tool", ToolCallID: "denied", Content: `{"ok":false,"code":"policy_denied"}`},
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "unanswered", Name: "read_file", Arguments: json.RawMessage(`{"path":"unanswered.go"}`)}}},
	}
	for index, item := range []struct {
		hash          string
		offset, limit int64
	}{{hashA, 0, 10}, {hashA, 5, 10}, {hashA, 25, 5}, {hashB, 10, 2}} {
		messages = append(messages, inventoryReadMessages(t, fmt.Sprint(index), "target.go", item.hash, item.offset, item.limit, 30)...)
	}
	inventory := explorationInventory(messages, 4096)
	for _, required := range []string{hashA, hashB, "[0,15)", "[25,30)", "[10,12)"} {
		if !strings.Contains(inventory, required) {
			t.Fatalf("缺少实际版本/范围 %q: %s", required, inventory)
		}
	}
	for _, forbidden := range []string{"denied.go", "unanswered.go", "SOURCE_BODY_MUST_NOT_BE_COPIED", "[0,30)"} {
		if strings.Contains(inventory, forbidden) {
			t.Fatalf("清单虚构或复制证据 %q: %s", forbidden, inventory)
		}
	}
}

// TestExplorationInventoryKeepsSearchScope 不将同一路径的不同查询、正则模式压成同一事实。
func TestExplorationInventoryKeepsSearchScope(t *testing.T) {
	messages := []ports.Message{}
	queries := []string{
		`{"path":"src","query":"first","regex":false}`,
		`{"path":"src","query":"first","regex":true}`,
		`{"path":"src","query":"second","glob":"*.go"}`,
		fmt.Sprintf(`{"path":"src","query":"%s-x"}`, strings.Repeat("prefix", 40)),
		fmt.Sprintf(`{"path":"src","query":"%s-y"}`, strings.Repeat("prefix", 40)),
	}
	for index, raw := range queries {
		id := fmt.Sprint(index)
		messages = append(messages,
			ports.Message{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: id, Name: "search_text", Arguments: json.RawMessage(raw)}}},
			ports.Message{Role: "tool", ToolCallID: id, Content: `{"ok":true,"code":"ok","data":{"matches":[]}}`},
		)
	}
	inventory := explorationInventory(messages, 2048)
	for _, required := range []string{`query="first"`, `query="second"`, "regex=false", "regex=true", `glob="*.go"`} {
		if !strings.Contains(inventory, required) {
			t.Fatalf("丢失搜索条件 %q: %s", required, inventory)
		}
	}
	if strings.Count(inventory, "search_text") != len(queries) {
		t.Fatalf("不同搜索被合并: %s", inventory)
	}
}

// TestExplorationInventoryPrioritizesRecentEvidence 清单不足以容纳所有历史时保留近期事实并明确省略。
func TestExplorationInventoryPrioritizesRecentEvidence(t *testing.T) {
	messages := []ports.Message{}
	for index := range 30 {
		messages = append(messages, inventoryReadMessages(t, fmt.Sprint(index), fmt.Sprintf("older-file-%02d.go", index), "", 0, 10, 10)...)
	}
	messages = append(messages, inventoryReadMessages(t, "latest", "latest-target.go", "", 30, 10, 100)...)
	inventory := explorationInventory(messages, 512)
	if len(inventory) > 512 || !strings.Contains(inventory, "latest-target.go") || !strings.Contains(inventory, "[30,40)") || !strings.Contains(inventory, "…") {
		t.Fatalf("预算或近期证据丢失: %s", inventory)
	}
	if inventory != explorationInventory(messages, 512) {
		t.Fatal("同一历史的清单不确定")
	}
}

// TestCompactionRetainsScopedDecisionCheckpoint 保留已触发的收敛提醒，不让近期泛读挤掉权限约束。
func TestCompactionRetainsScopedDecisionCheckpoint(t *testing.T) {
	messages := []ports.Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "仅阅读和分析"},
		{Role: "user", Content: decisionCheckpointPrompt},
	}
	for index := range 20 {
		messages = append(messages, inventoryReadMessages(t, fmt.Sprint(index), "target.go", "", int64(index), 10, 100)...)
	}
	summary := summarizeMessagesWithInventoryBudget(messages, 1024, 25)
	if len(summary) > 1024 || !strings.Contains(summary, decisionCheckpointPrefix) || !strings.Contains(summary, "不授权写入") || !strings.Contains(summary, "只读") {
		t.Fatalf("收敛提醒或任务范围丢失: %s", summary)
	}
}

// TestCheckpointRetainsPatchRecoveryMetadata 保留可执行的白名单恢复字段，拒绝将未知原文复制到摘要。
func TestCheckpointRetainsPatchRecoveryMetadata(t *testing.T) {
	raw := `{"ok":false,"code":"patch_text_mismatch","preview":"exact match failed","data":{"path":"target.go","replacement_index":2,"actual_occurrences":0,"expected_occurrences":1,"next_action":"copy the exact original block; read only the needed range","old":"SOURCE_BODY_MUST_NOT_BE_COPIED","new":"CANDIDATE_MUST_NOT_BE_COPIED"}}`
	summary := summarizeToolResult("apply_patch", raw)
	for _, required := range []string{"replacement_index=2", "actual_occurrences=0", "expected_occurrences=1", "read only the needed range"} {
		if !strings.Contains(summary, required) {
			t.Fatalf("恢复字段丢失 %q: %s", required, summary)
		}
	}
	for _, forbidden := range []string{"SOURCE_BODY_MUST_NOT_BE_COPIED", "CANDIDATE_MUST_NOT_BE_COPIED"} {
		if strings.Contains(summary, forbidden) {
			t.Fatal("恢复摘要复制了原文")
		}
	}
}

// TestExplorationInventoryRedactsSearchBeforeClipping 新保留的查询文本必须先完整脱敏，避免截断后留下密钥片段。
func TestExplorationInventoryRedactsSearchBeforeClipping(t *testing.T) {
	secret := "sk-abcdefghijklmnopqrstuvwxyz1234567890"
	arguments, err := json.Marshal(struct {
		Path  string `json:"path"`
		Query string `json:"query"`
	}{Path: "src", Query: "api_key=" + secret + " " + strings.Repeat("padding", 100)})
	if err != nil {
		t.Fatal(err)
	}
	messages := []ports.Message{
		{Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "search", Name: "search_text", Arguments: arguments}}},
		{Role: "tool", ToolCallID: "search", Content: `{"ok":true,"code":"ok","data":{"matches":[]}}`},
	}
	inventory := explorationInventory(messages, 512)
	if !strings.Contains(inventory, "query=") || strings.Contains(inventory, "abcdefghijklmnopqrstuvwxyz") || len(inventory) > 512 {
		t.Fatalf("查询脱敏或边界失效: %s", inventory)
	}
}
