package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// scriptedCalls 构造两步反放风格的应用补丁调用：先建新文件、再改既有文件。
func scriptedCalls(updateExpected string) []ports.ToolCall {
	return []ports.ToolCall{
		{ID: "rewind-create-1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"fresh.txt","mode":"create","content":"fresh"}`)},
		{ID: "rewind-update-2", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"note.txt","mode":"update","expected_sha256":"` + updateExpected + `","replacements":[{"old":"original","new":"restored"}]}`)},
	}
}

// TestLoopScriptedPlanExecutesAndCompletes 锁定脚本化计划主合同：不经模型执
// 行全部调用，事件链完整，验证按零命令语义收束并完成运行。
func TestLoopScriptedPlanExecutesAndCompletes(t *testing.T) {
	root, loop, journal, _ := snapshotFixture(t, map[string]string{"note.txt": "original"}, "allow")
	loop.Provider = &scriptedProvider{scripts: [][]ports.ModelEvent{{}}}
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "scripted plan")
	result, err := loop.RunScriptedToolPlan(context.Background(), RunInput{RunID: runID, WorkspaceHash: "workspace"}, ports.Approval{}, scriptedCalls(fileHash(t, "original")))
	if err != nil || result.RunID != runID {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Response != automaticVerificationResponse {
		t.Fatalf("response=%q", result.Response)
	}
	if content, err := os.ReadFile(filepath.Join(root, "fresh.txt")); err != nil || string(content) != "fresh" {
		t.Fatalf("fresh.txt=%q err=%v", content, err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(content) != "restored" {
		t.Fatalf("note.txt=%q err=%v", content, err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct {
		eventType domain.EventType
		count     int
	}{
		{domain.EventToolCallProposed, 2},
		{domain.EventOperationStarted, 2},
		{domain.EventOperationSucceeded, 2},
		{domain.EventToolResultAppended, 2},
		{domain.EventRunCompleted, 1},
	} {
		if countEvent(events, want.eventType) != want.count {
			t.Fatalf("%s count=%d want %d", want.eventType, countEvent(events, want.eventType), want.count)
		}
	}
	if _, err := domain.Replay(events); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

// TestLoopScriptedPlanPausesPerApprovalAndResumes 锁定逐项审批合同：ask 策略
// 下每个调用独立等待审批，携带正确指纹再次进入即续跑剩余计划。
func TestLoopScriptedPlanPausesPerApprovalAndResumes(t *testing.T) {
	root, loop, journal, _ := snapshotFixture(t, map[string]string{"note.txt": "original"}, "ask")
	loop.Provider = &scriptedProvider{scripts: [][]ports.ModelEvent{{}}}
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "scripted approvals")
	calls := scriptedCalls(fileHash(t, "original"))
	input := RunInput{RunID: runID, WorkspaceHash: "workspace"}
	_, first := loop.RunScriptedToolPlan(context.Background(), input, ports.Approval{}, calls)
	var waiting *ports.WaitingApprovalError
	if !errors.As(first, &waiting) {
		t.Fatalf("first call must pause for approval: %v", first)
	}
	_, second := loop.RunScriptedToolPlan(context.Background(), input, ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "test"}, calls)
	var waitingAgain *ports.WaitingApprovalError
	if !errors.As(second, &waitingAgain) {
		t.Fatalf("second call must pause for its own approval: %v", second)
	}
	if _, err := loop.RunScriptedToolPlan(context.Background(), input, ports.Approval{Fingerprint: waitingAgain.Fingerprint, Approve: true, Reason: "test"}, calls); err != nil {
		t.Fatalf("final continuation: %v", err)
	}
	if content, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(content) != "restored" {
		t.Fatalf("note.txt=%q err=%v", content, err)
	}
	events, _ := journal.Load(context.Background(), runID)
	if countEvent(events, domain.EventApprovalGranted) != 2 || countEvent(events, domain.EventRunCompleted) != 1 {
		t.Fatalf("approvals=%d completed=%d", countEvent(events, domain.EventApprovalGranted), countEvent(events, domain.EventRunCompleted))
	}
	if _, err := domain.Replay(events); err != nil {
		t.Fatalf("replay: %v", err)
	}
}

// TestLoopScriptedPlanApprovalDenialFailsRun 锁定拒绝语义：审批拒绝终态化运
// 行，剩余计划不再执行。
func TestLoopScriptedPlanApprovalDenialFailsRun(t *testing.T) {
	root, loop, journal, _ := snapshotFixture(t, map[string]string{"note.txt": "original"}, "ask")
	loop.Provider = &scriptedProvider{scripts: [][]ports.ModelEvent{{}}}
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "scripted denial")
	calls := scriptedCalls(fileHash(t, "original"))
	input := RunInput{RunID: runID, WorkspaceHash: "workspace"}
	_, first := loop.RunScriptedToolPlan(context.Background(), input, ports.Approval{}, calls)
	var waiting *ports.WaitingApprovalError
	if !errors.As(first, &waiting) {
		t.Fatalf("must pause for approval: %v", first)
	}
	_, err := loop.RunScriptedToolPlan(context.Background(), input, ports.Approval{Fingerprint: waiting.Fingerprint, Approve: false, Reason: "rejected"}, calls)
	var denied *ports.PolicyDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("denial must fail the run: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "fresh.txt")); !os.IsNotExist(statErr) {
		t.Fatal("denied plan must not create files")
	}
	events, _ := journal.Load(context.Background(), runID)
	projection, replayErr := domain.Replay(events)
	if replayErr != nil || projection.State != domain.RunFailed {
		t.Fatalf("projection=%+v replayErr=%v", projection, replayErr)
	}
}

// TestLoopScriptedPlanHealsMissingToolResult 锁定中断恢复窗口：已终态但缺结
// 果事件的调用由终态载荷确定性补齐，随后正常收束。
func TestLoopScriptedPlanHealsMissingToolResult(t *testing.T) {
	const beforeBody = "heal original"
	const afterBody = "heal applied"
	root, loop, journal, _ := snapshotFixture(t, map[string]string{"note.txt": afterBody}, "allow")
	loop.Provider = &scriptedProvider{scripts: [][]ports.ModelEvent{{}}}
	runID := domain.NewRunID()
	ctx := context.Background()
	if _, err := journal.Append(ctx, 0, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"heal","workspace":"/workspace"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(ctx, 1, domain.EventDraft{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventRunStarted, Payload: json.RawMessage(`{"invocation":"invoke_heal"}`)}); err != nil {
		t.Fatal(err)
	}
	turnID := domain.NewTurnID()
	operationID := domain.NewOperationID()
	healDrafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventTurnOpened, TurnID: &turnID, Payload: json.RawMessage(`{"turn":1}`)},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"rewind_heal_1","tool_name":"apply_patch","arguments":{"path":"note.txt","mode":"update","expected_sha256":"` + fileHash(t, beforeBody) + `","replacements":[{"old":"` + beforeBody + `","new":"` + afterBody + `"}]}}`)},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","rule":"workspace_write","fingerprint":"sha256:heal"}`)},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"note.txt","planned_before":"` + fileHash(t, beforeBody) + `","planned_after":"` + fileHash(t, afterBody) + `"}`)},
		{RunID: runID, WorkspaceHash: "workspace", Type: domain.EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","result":{"ok":true,"code":"ok","preview":"updated note.txt"}}`)},
	}
	for index, draft := range healDrafts {
		if _, err := journal.Append(ctx, uint64(index+2), draft); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loop.RunScriptedToolPlan(ctx, RunInput{RunID: runID, WorkspaceHash: "workspace"}, ports.Approval{}, []ports.ToolCall{{ID: "rewind_heal_1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"note.txt","mode":"update","expected_sha256":"` + fileHash(t, beforeBody) + `","replacements":[{"old":"` + beforeBody + `","new":"` + afterBody + `"}]}`)}}); err != nil {
		t.Fatal(err)
	}
	events, _ := journal.Load(ctx, runID)
	if countEvent(events, domain.EventToolResultAppended) != 1 {
		t.Fatalf("healed tool results=%d", countEvent(events, domain.EventToolResultAppended))
	}
	if countEvent(events, domain.EventRunCompleted) != 1 {
		t.Fatal("healed run must complete")
	}
	if _, err := domain.Replay(events); err != nil {
		t.Fatalf("replay: %v", err)
	}
	// 该调用已执行过（事件声称 before→after 已生效），续跑只补结果不重复
	// 执行：文件保持事件声称的终态。
	if content, err := os.ReadFile(filepath.Join(root, "note.txt")); err != nil || string(content) != afterBody {
		t.Fatalf("note.txt=%q err=%v", content, err)
	}
}
