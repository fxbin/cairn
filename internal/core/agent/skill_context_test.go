// [INPUT]: 真实 Loop、Policy 和同一技能能力实例。
// [OUTPUT]: 验证历史正文、冷恢复与观察器前置门禁。
// [POS]: Core 对技能装配与权限合同的回归。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/capabilities/skills"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/policy"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// newSkillLoopTest 组装真实工具、Policy、Journal 与替身 Provider，不需要外部服务。
func newSkillLoopTest(t *testing.T, workspacePath, skillsPath string, provider ports.Provider, journal *memory.Journal, exec *countingExecTool) (*Loop, *skills.Skill) {
	t.Helper()
	workspace, err := tools.NewWorkspace(workspacePath, nil, 16*1024, 100, 64*1024)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.DiscoverSkills([]skills.SkillRoot{{Path: skillsPath, Scope: "workspace"}}, func(root string) (skills.PackageView, error) {
		return tools.NewWorkspace(root, nil, skills.SkillMaxResourceBytes, skills.SkillMaxFiles, skills.SkillMaxPackageBytes)
	})
	if err != nil {
		t.Fatal(err)
	}
	skillTool := &skills.Skill{Catalog: catalog}
	registry, err := tools.NewRegistry(skillTool, tools.ReadFile{Workspace: workspace}, exec)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "ask", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	return &Loop{Journal: journal, Provider: provider, Tools: registry, ContextProviders: []ContextProvider{skills.NewContext(skillTool)}, ResultObservers: []ToolResultObserver{skills.NewHistory(skillTool)}, Gate: gate, Model: "fake", MaxTurns: 4}, skillTool
}

// skillLoopSource 创建包含工具声明的技能，权限声明必须保持为无效授权。
func skillLoopSource(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	directory := filepath.Join(root, "review-code")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "SKILL.md"), []byte("---\nname: review-code\ndescription: 检查代码\nallowed-tools: Bash(*)\n---\nPINNED_SKILL_ORIGINAL：先核对再执行。"), 0600); err != nil {
		t.Fatal(err)
	}
	return root
}

// TestSkillColdResumeKeepsJournalBodyAndRequiresExecApproval 真实审批暂停后删去源文件，冷恢复仍只读历史说明且命令只执行一次。
func TestSkillColdResumeKeepsJournalBodyAndRequiresExecApproval(t *testing.T) {
	workspace, root := t.TempDir(), skillLoopSource(t)
	journal := memory.New()
	runID := domain.NewRunID()
	task := "/skill:review-code 检查代码"
	appendInitialRunningEvents(t, journal, runID, task)
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "load", Name: "skill", Arguments: json.RawMessage(`{"name":"review-code"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "exec", Name: "exec_command", Arguments: json.RawMessage(`{"argv":["true"]}`)}}, {Kind: ports.Done}},
	}}
	exec := &countingExecTool{}
	loop, _ := newSkillLoopTest(t, workspace, root, provider, journal, exec)
	input := RunInput{RunID: runID, Task: task, WorkspaceHash: "workspace"}
	_, err := loop.Run(context.Background(), input)
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) || exec.calls != 0 {
		t.Fatalf("skill permission bypass: calls=%d err=%v", exec.calls, err)
	}
	if len(provider.requests) != 2 || !requestContainsSkill(provider.requests[1], "PINNED_SKILL_ORIGINAL") {
		t.Fatal("loaded instructions absent from next model request")
	}
	if err := os.RemoveAll(filepath.Join(root, "review-code")); err != nil {
		t.Fatal(err)
	}
	nextProvider := &scriptedProvider{scripts: [][]ports.ModelEvent{{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}}}}
	nextExec := &countingExecTool{}
	next, nextSkill := newSkillLoopTest(t, workspace, root, nextProvider, journal, nextExec)
	if _, err := next.Resume(context.Background(), input, ports.Approval{Approve: true, Fingerprint: waiting.Fingerprint}); err != nil {
		t.Fatal(err)
	}
	if nextExec.calls != 1 || len(nextProvider.requests) != 1 || !requestContainsSkill(nextProvider.requests[0], "PINNED_SKILL_ORIGINAL") {
		t.Fatal("cold resume lost skill instructions or replayed work")
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, domain.EventOperationStarted) != 2 {
		t.Fatal("cold resume reloaded skill source")
	}
	withoutReceipt := []domain.Event{}
	for _, event := range events {
		if event.Type == domain.EventOperationSucceeded {
			var receipt struct {
				ToolName string `json:"tool_name"`
			}
			_ = json.Unmarshal(event.Payload, &receipt)
			if receipt.ToolName == "skill" {
				continue
			}
		}
		withoutReceipt = append(withoutReceipt, event)
	}
	if err := next.restoreResultProjections(withoutReceipt); err == nil || len(nextSkill.ActiveSkills()) != 0 {
		t.Fatal("skill activated without matching success receipt")
	}
}

// TestSkillInstructionsSurviveCompaction 原始读取正文被压缩后，已加载说明仍完整保留且计入请求预算。
func TestSkillInstructionsSurviveCompaction(t *testing.T) {
	workspace, root := t.TempDir(), skillLoopSource(t)
	if err := os.WriteFile(filepath.Join(workspace, "large.txt"), []byte(strings.Repeat("verbose evidence ", 800)), 0600); err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID := domain.NewRunID()
	task := "review with skill"
	appendInitialRunningEvents(t, journal, runID, task)
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "load", Name: "skill", Arguments: json.RawMessage(`{"name":"review-code"}`)}}, {Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "read", Name: "read_file", Arguments: json.RawMessage(`{"path":"large.txt","limit":14000}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}},
	}}
	loop, _ := newSkillLoopTest(t, workspace, root, provider, journal, &countingExecTool{})
	loop.ContextAutoCompact = true
	loop.ContextMaxPromptTokens = 4000
	loop.ContextMaxSummaryBytes = 512
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: task, WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if countEvent(events, domain.EventContextCompacted) == 0 {
		t.Fatal("fixture did not exercise compaction")
	}
	request := provider.requests[1]
	if !requestContainsSkill(request, "PINNED_SKILL_ORIGINAL") || loop.estimatePromptTokens(request.Messages) > loop.ContextMaxPromptTokens {
		t.Fatal("compaction lost instructions or exceeded context budget")
	}
	occurrences := 0
	for _, message := range request.Messages {
		occurrences += strings.Count(message.Content, "PINNED_SKILL_ORIGINAL")
	}
	if occurrences != 1 {
		t.Fatalf("instructions duplicated %d times", occurrences)
	}
}

// requestContainsSkill 只在模型实际收到的消息中查找标记，不从本地投影推断成功。
func requestContainsSkill(request ports.ModelRequest, marker string) bool {
	for _, message := range request.Messages {
		if strings.Contains(message.Content, marker) {
			return true
		}
	}
	return false
}

// TestRegisteredSkillRequiresHistoryBeforeProvider 缺失历史观察器时，注册技能不得请求模型或激活正文。
func TestRegisteredSkillRequiresHistoryBeforeProvider(t *testing.T) {
	journal := memory.New()
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "review with skill")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "load", Name: "skill", Arguments: json.RawMessage(`{"name":"review-code"}`)}}, {Kind: ports.Done}},
	}}
	loop, skillTool := newSkillLoopTest(t, t.TempDir(), skillLoopSource(t), provider, journal, &countingExecTool{})
	loop.ResultObservers = nil
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "review with skill", WorkspaceHash: "workspace"}); err == nil {
		t.Fatal("registered skill accepted missing history observer")
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(provider.requests) != 0 || countEvent(events, domain.EventOperationStarted) != 0 || len(skillTool.ActiveSkills()) != 0 {
		t.Fatal("skill execution began before history wiring was accepted")
	}
}
