package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// checkpointFixture 使用真实读取/补丁工具装配隔离工作区，原始任务保持独立消息。
func checkpointFixture(t *testing.T, scripts [][]ports.ModelEvent, enabled bool, task string) (*Loop, RunInput, *memory.Journal, *scriptedProvider, string) {
	t.Helper()
	root := t.TempDir()
	content := []byte("line1\nline2\nline3\nline4\nline5\nline6\nbefore\n")
	target := filepath.Join(root, "target.txt")
	if err := os.WriteFile(target, content, 0o600); err != nil {
		t.Fatal(err)
	}
	workspace, err := tools.NewWorkspace(root, nil, 4096, 100, 4096)
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
	journal := memory.New()
	runID := domain.NewRunID()
	input := RunInput{RunID: runID, Task: task, WorkspaceHash: "workspace"}
	appendInitialRunningEvents(t, journal, runID, input.Task)
	provider := &scriptedProvider{scripts: scripts}
	loop := &Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: len(scripts), DecisionCheckpointEnabled: enabled, DecisionCheckpointMinTurns: 1}
	return loop, input, journal, provider, target
}

// checkpointReadScript 构造一轮明确的只读提议，运行时回执仍由真实工具产生。
func checkpointReadScript(id, arguments string) []ports.ModelEvent {
	return []ports.ModelEvent{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: id, Name: "read_file", Arguments: json.RawMessage(arguments)}}, {Kind: ports.Done}}
}

// countCheckpointMessages 只计直接插入的检查点，避免将原始任务或普通说明误算为反馈。
func countCheckpointMessages(messages []ports.Message) int {
	count := 0
	for _, message := range messages {
		if message.Role == "user" && strings.HasPrefix(message.Content, decisionCheckpointPrefix) {
			count++
		}
	}
	return count
}

// TestLoopDecisionCheckpointUsesSuccessfulReadOnlyFeedback 覆盖真实触发、默认关闭及读失败后不虚构证据。
func TestLoopDecisionCheckpointUsesSuccessfulReadOnlyFeedback(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			scripts := [][]ports.ModelEvent{
				checkpointReadScript("missing", `{"path":"missing.txt"}`),
				checkpointReadScript("read", `{"path":"target.txt"}`),
				checkpointReadScript("read-again", `{"path":"target.txt","offset":6,"limit":12}`),
				{{Kind: ports.TextDelta, Text: "只读分析完成"}, {Kind: ports.Done}},
			}
			loop, input, journal, provider, _ := checkpointFixture(t, scripts, enabled, "只读分析目标文件，保持只读")
			if _, err := loop.Run(context.Background(), input); err != nil {
				t.Fatal(err)
			}
			if countCheckpointMessages(provider.requests[1].Messages) != 0 {
				t.Fatal("失败读取触发了基于已读事实的检查点")
			}
			want := 0
			if enabled {
				want = 1
			}
			if countCheckpointMessages(provider.requests[2].Messages) != want || countCheckpointMessages(provider.requests[3].Messages) != want {
				t.Fatal("显式开启或一次去重失效")
			}
			events, err := journal.Load(context.Background(), input.RunID)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == domain.EventToolCallProposed && strings.Contains(string(event.Payload), "apply_patch") {
					t.Fatal("只读任务因检查点提议了写入")
				}
			}
			for _, message := range provider.requests[3].Messages {
				if strings.HasPrefix(message.Content, decisionCheckpointPrefix) && !strings.Contains(message.Content, "只读") {
					t.Fatal("反馈丢失只读范围")
				}
			}
		})
	}
}

// checkpointGapJournal 模拟已关闭轮次后、反馈持久化之前的失败，不修改已经落盘的事实。
type checkpointGapJournal struct {
	ports.Journal
	rejected bool
}

// Append 仅拒绝一次检查点写入，恢复时交还底层 Journal 并重建投影。
func (journal *checkpointGapJournal) Append(ctx context.Context, seq uint64, draft domain.EventDraft) (domain.Event, error) {
	if draft.Type == domain.EventVerificationFeedbackAppended && !journal.rejected {
		journal.rejected = true
		return domain.Event{}, errors.New("injected checkpoint append failure")
	}
	return journal.Journal.Append(ctx, seq, draft)
}

// TestLoopDecisionCheckpointCatchesUpBeforeResumeRequest 关闭边界缺少反馈时，恢复后的首个请求即收到检查点。
func TestLoopDecisionCheckpointCatchesUpBeforeResumeRequest(t *testing.T) {
	scripts := [][]ports.ModelEvent{checkpointReadScript("read", `{"path":"target.txt"}`), {{Kind: ports.TextDelta, Text: "分析完成"}, {Kind: ports.Done}}}
	loop, input, journal, provider, _ := checkpointFixture(t, scripts, true, "只读分析目标文件，保持只读")
	gap := &checkpointGapJournal{Journal: journal}
	loop.Journal = gap
	if _, err := loop.Run(context.Background(), input); err == nil || !gap.rejected {
		t.Fatal("未复现关闭后反馈缺口")
	}
	resumeProvider := &scriptedProvider{scripts: scripts[1:]}
	resume := *loop
	resume.Journal = journal
	resume.Provider = resumeProvider
	if _, err := resume.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 1 || len(resumeProvider.requests) != 1 || countCheckpointMessages(resumeProvider.requests[0].Messages) != 1 {
		t.Fatal("恢复后的第一个请求未补齐一次反馈")
	}
	events, err := journal.Load(context.Background(), input.RunID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.Type == domain.EventVerificationFeedbackAppended {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("持久化反馈数=%d", count)
	}
}

// TestLoopDecisionCheckpointPreservesLateExactPatch 不把探索提醒变成首补丁期限或提前终态。
func TestLoopDecisionCheckpointPreservesLateExactPatch(t *testing.T) {
	scripts := [][]ports.ModelEvent{}
	for turn := range 7 {
		scripts = append(scripts, checkpointReadScript(fmt.Sprint(turn), fmt.Sprintf(`{"path":"target.txt","offset":%d,"limit":8}`, turn*6)))
	}
	original := []byte("line1\nline2\nline3\nline4\nline5\nline6\nbefore\n")
	hash := sha256.Sum256(original)
	arguments, err := json.Marshal(tools.PatchArguments{Path: "target.txt", Mode: "update", ExpectedSHA256: "sha256:" + hex.EncodeToString(hash[:]), Replacements: []tools.Replacement{{Old: "before\n", New: "after\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	scripts = append(scripts, []ports.ModelEvent{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch", Name: "apply_patch", Arguments: arguments}}, {Kind: ports.Done}}, []ports.ModelEvent{{Kind: ports.TextDelta, Text: "修改完成"}, {Kind: ports.Done}})
	loop, input, journal, provider, target := checkpointFixture(t, scripts, true, "根据目标文件将 before 改成 after")
	loop.DecisionCheckpointMinTurns = 4
	if _, err := loop.Run(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil || !strings.HasSuffix(string(content), "after\n") || len(provider.requests) != 9 {
		t.Fatalf("成功探索路径被提前中止: %v", err)
	}
	events, err := journal.Load(context.Background(), input.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.Replay(events); err != nil {
		t.Fatal(err)
	}
}

// TestLoopDecisionCheckpointRestoresLegacyCompactedFeedback 冷打开 SQLite 后，旧摘要丢正文不会丢提醒或重复写事件。
func TestLoopDecisionCheckpointRestoresLegacyCompactedFeedback(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(fmt.Sprint(retained), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal.db")
			journal, err := sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			runID, closedTurn, feedbackTurn := domain.NewRunID(), domain.NewTurnID(), domain.NewTurnID()
			const task = "只读分析目标文件，保持只读"
			summary := "旧摘要只保留了读取事实"
			if retained {
				summary = decisionCheckpointSummary
			}
			drafts := []domain.EventDraft{
				{Type: domain.EventRunCreated, Payload: mustJSON(domain.RunCreatedPayload{Task: task, Workspace: "/workspace"})},
				{Type: domain.EventRunStarted},
				{Type: domain.EventTurnOpened, TurnID: &closedTurn},
				{Type: domain.EventTurnClosed, TurnID: &closedTurn, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})},
				{Type: domain.EventVerificationFeedbackAppended, TurnID: &feedbackTurn, Payload: json.RawMessage(`{"text":"[决策检查点] 历史收敛提醒"}`)},
				{Type: domain.EventContextCompacted, Payload: mustJSON(domain.ContextCompactedPayload{ThroughSeq: 5, Summary: summary, EstimatedTokensBefore: 8000, EstimatedTokensAfter: 100, Strategy: contextCompactionStrategy})},
			}
			for index, draft := range drafts {
				draft.RunID = runID
				draft.WorkspaceHash = "workspace"
				if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
					_ = journal.Close()
					t.Fatal(err)
				}
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			journal, err = sqlite.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			workspace, err := tools.NewWorkspace(t.TempDir(), nil, 4096, 100, 4096)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := tools.NewRegistry(tools.ReadFile{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			gate, err := policy.New("deny", "deny", nil, tools.ApplyPatch{Workspace: workspace})
			if err != nil {
				t.Fatal(err)
			}
			provider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "只读分析完成"}, {Kind: ports.Done}}}}
			loop := Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2, ContextAutoCompact: true, ContextMaxPromptTokens: 8000, ContextMaxSummaryBytes: 1024, DecisionCheckpointEnabled: true, DecisionCheckpointMinTurns: 1}
			if _, err := loop.Resume(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, ports.Approval{}); err != nil {
				t.Fatal(err)
			}
			if len(provider.requests) != 1 {
				t.Fatal("恢复请求数不正确")
			}
			count := 0
			for _, message := range provider.requests[0].Messages {
				count += strings.Count(message.Content, decisionCheckpointSummary)
			}
			if count != 1 {
				t.Fatalf("首个请求中的提醒数=%d", count)
			}
			events, err := journal.Load(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			feedbackCount := 0
			for _, event := range events {
				if event.Type == domain.EventVerificationFeedbackAppended {
					feedbackCount++
				}
			}
			if feedbackCount != 1 {
				t.Fatal("恢复写了第二个反馈事件")
			}
		})
	}
}
