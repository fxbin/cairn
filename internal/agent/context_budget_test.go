package agent

import (
	"strings"
	"testing"

	"encoding/json"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

func TestCompactionSummaryLimitIncludesActualRequestOverhead(t *testing.T) {
	registry, err := tools.NewRegistry(tools.ReadFile{})
	if err != nil {
		t.Fatal(err)
	}
	for _, prior := range []string{"", "已确认的会话前情"} {
		loop := Loop{Tools: registry, ContextMaxPromptTokens: 1500, ContextMaxSummaryBytes: 1024}
		task := "inspect twice"
		size := loop.compactionSummaryLimit(task, prior)
		if size <= 0 {
			t.Fatal("fixture should leave room for a summary")
		}
		messages := loop.contextMessages(task, prior, strings.Repeat("x", size))
		if got := loop.estimatePromptTokens(messages); got > loop.ContextMaxPromptTokens {
			t.Fatalf("allocated summary produces %d tokens, limit %d", got, loop.ContextMaxPromptTokens)
		}
	}
	loop := Loop{Tools: registry, ContextMaxPromptTokens: 1}
	if got := loop.compactionSummaryLimit("task", ""); got != 0 {
		t.Fatalf("impossible persistent context allocated %d summary bytes", got)
	}
}

func TestCompactionSummaryRespectsTinyByteBudget(t *testing.T) {
	messages := []ports.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "task"}, {Role: "assistant", ToolCalls: []ports.ToolCall{{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"sample.txt"}`)}}}}
	for size := 1; size <= 128; size++ {
		if summary := summarizeMessages(messages, size); len(summary) > size {
			t.Fatalf("budget %d produced %d bytes", size, len(summary))
		}
	}
}
