package agent

import (
	"strings"
	"testing"

	"context"
	"encoding/json"
	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
	"reflect"
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
		if summary := summarizeMessagesWithInventoryBudget(messages, size, 25); len(summary) > size {
			t.Fatalf("budget %d produced %d bytes", size, len(summary))
		}
	}
}

func TestCompactionDoesNotExpandPersistentOnlyContext(t *testing.T) {
	registry, err := tools.NewRegistry(tools.ReadFile{})
	if err != nil {
		t.Fatal(err)
	}
	for _, delta := range []int{10, -1} {
		journal := memory.New()
		runID := domain.NewRunID()
		appendInitialRunningEvents(t, journal, runID, "task")
		events, err := journal.Load(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		projection, err := domain.Replay(events)
		if err != nil {
			t.Fatal(err)
		}
		loop := Loop{Journal: journal, Tools: registry, ContextAutoCompact: true}
		messages := loop.contextMessages("task", "", "")
		loop.ContextMaxPromptTokens = loop.estimatePromptTokens(messages) + delta
		rebuilt, err := loop.compactContextIfNeeded(context.Background(), &projection, RunInput{RunID: runID, Task: "task", WorkspaceHash: "workspace"}, messages)
		if delta > 0 {
			if err != nil || !reflect.DeepEqual(rebuilt, messages) {
				t.Fatalf("fitting persistent context should not be compacted: %v", err)
			}
		} else if err == nil {
			t.Fatal("oversized persistent context must fail closed")
		}
		after, err := journal.Load(context.Background(), runID)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(events) {
			t.Fatal("wrote a checkpoint without compactable history")
		}
	}
}
