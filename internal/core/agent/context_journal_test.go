package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

func TestRepeatedCompactionRebuildsFlatJournalHistory(t *testing.T) {
	root := t.TempDir()
	provider := &scriptedProvider{}
	const rounds = 10
	for index := 0; index < rounds; index++ {
		name := fmt.Sprintf("sample-%02d.txt", index)
		if err := os.WriteFile(filepath.Join(root, name), []byte(strings.Repeat("padding-", 8000)+"RAW_BODY_SENTINEL"), 0o600); err != nil {
			t.Fatal(err)
		}
		arguments, err := json.Marshal(readFilePathForCompactionTest{Path: name})
		if err != nil {
			t.Fatal(err)
		}
		provider.scripts = append(provider.scripts, []ports.ModelEvent{
			{Kind: ports.TextDelta, Text: fmt.Sprintf("decision-%02d", index)},
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: fmt.Sprintf("call-%02d", index), Name: "read_file", Arguments: arguments}},
			{Kind: ports.Done},
		})
	}
	provider.scripts = append(provider.scripts, []ports.ModelEvent{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}})
	workspace, err := tools.NewWorkspace(root, nil, 128*1024, 100, 256*1024)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID := domain.NewRunID()
	const task = "original task stays outside summary"
	const prior = "session context stays outside summary"
	appendInitialRunningEvents(t, journal, runID, task)
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: rounds + 1, ContextAutoCompact: true, ContextMaxPromptTokens: 3000, ContextMaxSummaryBytes: 2048}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: task, WorkspaceHash: "workspace", PriorContext: prior}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for index, event := range events {
		if event.Type != domain.EventContextCompacted {
			continue
		}
		count++
		var checkpoint domain.ContextCompactedPayload
		if err := json.Unmarshal(event.Payload, &checkpoint); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(checkpoint.Summary, "先前检查点") {
			t.Fatalf("checkpoint %d recursively embeds old summary", count)
		}
		for _, forbidden := range []string{task, prior, "RAW_BODY_SENTINEL"} {
			if strings.Contains(checkpoint.Summary, forbidden) {
				t.Fatalf("checkpoint %d contains excluded content", count)
			}
		}
		if !strings.Contains(checkpoint.Summary, "byte_range=") {
			t.Fatalf("checkpoint %d lost read location", count)
		}
		if len(checkpoint.Summary) > loop.ContextMaxSummaryBytes {
			t.Fatalf("checkpoint %d exceeds budget", count)
		}
		if !strings.Contains(checkpoint.Summary, "sample-00.txt") || !strings.Contains(checkpoint.Summary, fmt.Sprintf("sample-%02d.txt", count-1)) || !strings.Contains(checkpoint.Summary, fmt.Sprintf("decision-%02d", count-1)) {
			t.Fatalf("checkpoint %d lost early inventory or latest decision: %s", count, checkpoint.Summary)
		}
		replayLoop := Loop{PriorContext: prior}
		rebuilt, err := replayLoop.messagesFromEvents(events[:index+1], task)
		if err != nil {
			t.Fatal(err)
		}
		// 每次压缩后的下一请求必须与只读恢复完全一致。
		if !reflect.DeepEqual(rebuilt, provider.requests[count].Messages) {
			t.Fatalf("checkpoint %d differs from replay", count)
		}
	}
	if count != rounds {
		t.Fatalf("compactions=%d want %d", count, rounds)
	}
}

type readFilePathForCompactionTest struct {
	Path string `json:"path"`
}

func TestJournalCompactionKeepsTextBeforeFailedVerification(t *testing.T) {
	workspace, err := tools.NewWorkspace(t.TempDir(), nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	verifier := &flakyVerificationTool{}
	registry, err := tools.NewRegistry(tools.ApplyPatch{Workspace: workspace}, verifier)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	const decision = "CHECKPOINT_DECISION_ANCHOR"
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "create", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"sample.txt","mode":"create","content":"change"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: decision + strings.Repeat("detail-", 2000)}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "verify")
	loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 3, ContextAutoCompact: true, ContextMaxPromptTokens: 3000, ContextMaxSummaryBytes: 2048, Verification: []VerificationCommand{{ID: "check", Arguments: json.RawMessage(`{"argv":["verify"]}`)}}}
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "verify", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 3 || verifier.calls != 2 {
		t.Fatalf("requests=%d verification calls=%d", len(provider.requests), verifier.calls)
	}
	var summary string
	for _, message := range provider.requests[2].Messages {
		if strings.HasPrefix(message.Content, contextCheckpointPrefix) {
			summary = message.Content
		}
	}
	if !strings.Contains(summary, decision) || !strings.Contains(summary, "Verification command check failed") {
		t.Fatalf("checkpoint lost decision or failed verification: %s", summary)
	}
}
