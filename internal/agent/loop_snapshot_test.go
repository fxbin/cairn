package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// snapshotFixture 搭建带制品库的补丁执行环境：预置文件、工作区、策略与注册表。
func snapshotFixture(t *testing.T, initial map[string]string, writeMode string) (root string, loop *Loop, journal *memory.Journal, store *artifact.Store) {
	t.Helper()
	root = t.TempDir()
	for path, content := range initial {
		if err := os.WriteFile(filepath.Join(root, path), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	workspace, err := tools.NewWorkspace(root, nil, 1024, 100, 4096)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New(writeMode, "allow", nil, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	registry, err := tools.NewRegistry(tools.ListFiles{Workspace: workspace}, tools.ReadFile{Workspace: workspace}, tools.SearchText{Workspace: workspace}, tools.ApplyPatch{Workspace: workspace})
	if err != nil {
		t.Fatal(err)
	}
	store, err = artifact.New(filepath.Join(t.TempDir(), "artifacts"), 1024)
	if err != nil {
		t.Fatal(err)
	}
	journal = memory.New()
	loop = &Loop{Journal: journal, Tools: registry, Gate: gate, Model: "fake", MaxTurns: 2, Artifacts: store}
	return root, loop, journal, store
}

// startedBeforeRef 从事件流提取 apply_patch operation_started 的 before 快照引用。
func startedBeforeRef(t *testing.T, journal *memory.Journal, runID domain.RunID) *artifact.Ref {
	t.Helper()
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type != domain.EventOperationStarted {
			continue
		}
		var payload struct {
			ToolName         string        `json:"tool_name"`
			PlannedBeforeRef *artifact.Ref `json:"planned_before_ref"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ToolName == "apply_patch" {
			return payload.PlannedBeforeRef
		}
	}
	return nil
}

// TestLoopSnapshotsBeforeContentForUpdate 锁定 before 快照主合同：update 执行前
// 的现文件内容被无损制品化并引用于 operation_started，制品读回等于原文。
func TestLoopSnapshotsBeforeContentForUpdate(t *testing.T) {
	_, loop, journal, store := snapshotFixture(t, map[string]string{"note.txt": "original body"}, "allow")
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "update note")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-update", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"note.txt","mode":"update","expected_sha256":"` + fileHash(t, "original body") + `","replacements":[{"old":"original","new":"replaced"}]}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "updated"}, {Kind: ports.Done}},
	}}
	loop.Provider = provider
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "update note", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	reference := startedBeforeRef(t, journal, runID)
	if reference == nil {
		t.Fatal("operation_started is missing planned_before_ref for update")
	}
	content, err := store.Open(*reference)
	if err != nil || string(content) != "original body" {
		t.Fatalf("before snapshot=%q err=%v", content, err)
	}
	if reference.SHA256 != fileHash(t, "original body") {
		t.Fatalf("snapshot digest %s is not the original content digest", reference.SHA256)
	}
}

// TestLoopSnapshotsBeforeContentForDelete 锁定 delete 同样携带 before 快照。
func TestLoopSnapshotsBeforeContentForDelete(t *testing.T) {
	root, loop, journal, store := snapshotFixture(t, map[string]string{"gone.txt": "vanishing"}, "allow")
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "delete gone")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-delete", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"gone.txt","mode":"delete","expected_sha256":"` + fileHash(t, "vanishing") + `"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "deleted"}, {Kind: ports.Done}},
	}}
	loop.Provider = provider
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "delete gone", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	reference := startedBeforeRef(t, journal, runID)
	if reference == nil {
		t.Fatal("operation_started is missing planned_before_ref for delete")
	}
	content, err := store.Open(*reference)
	if err != nil || string(content) != "vanishing" {
		t.Fatalf("before snapshot=%q err=%v", content, err)
	}
	if _, err := os.Stat(filepath.Join(root, "gone.txt")); !os.IsNotExist(err) {
		t.Fatalf("delete did not remove target: %v", err)
	}
}

// TestLoopCreatePatchHasNoBeforeRef 锁定 create 无旧内容、不产生快照引用。
func TestLoopCreatePatchHasNoBeforeRef(t *testing.T) {
	_, loop, journal, _ := snapshotFixture(t, nil, "allow")
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "create fresh")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-create", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"fresh.txt","mode":"create","content":"fresh"}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "created"}, {Kind: ports.Done}},
	}}
	loop.Provider = provider
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "create fresh", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if reference := startedBeforeRef(t, journal, runID); reference != nil {
		t.Fatalf("create must not carry planned_before_ref: %+v", reference)
	}
}

// TestLoopSkipsSnapshotWhenRedactionWouldCorruptContent 锁定无损判定：现内容
// 命中脱敏模式时绝不产生快照引用（脱敏副本不能冒充回退源），补丁本身照常执行。
func TestLoopSkipsSnapshotWhenRedactionWouldCorruptContent(t *testing.T) {
	const body = "token = sk-abcdefgh12345678"
	_, loop, journal, _ := snapshotFixture(t, map[string]string{"secret.txt": body}, "allow")
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "update secret")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-secret", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"secret.txt","mode":"update","expected_sha256":"` + fileHash(t, body) + `","replacements":[{"old":"token","new":"value"}]}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "updated"}, {Kind: ports.Done}},
	}}
	loop.Provider = provider
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "update secret", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if reference := startedBeforeRef(t, journal, runID); reference != nil {
		t.Fatalf("redacted snapshot must not be referenced: %+v", reference)
	}
}

// TestLoopSnapshotsBeforeOnApprovalResume 锁定审批批准后的恢复执行路径同样
// 生成 before 快照（operation_started 由 resume 分支写入）。
func TestLoopSnapshotsBeforeOnApprovalResume(t *testing.T) {
	_, loop, journal, store := snapshotFixture(t, map[string]string{"approved.txt": "awaiting approval"}, "ask")
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "update after approval")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-ask", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"approved.txt","mode":"update","expected_sha256":"` + fileHash(t, "awaiting approval") + `","replacements":[{"old":"awaiting","new":"granted"}]}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "resumed after approval"}, {Kind: ports.Done}},
	}}
	loop.Provider = provider
	_, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "update after approval", WorkspaceHash: "workspace"})
	var waiting *WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("run must pause for approval: %v", err)
	}
	if _, err := loop.Resume(context.Background(), RunInput{RunID: runID, Task: "update after approval", WorkspaceHash: "workspace"}, Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	reference := startedBeforeRef(t, journal, runID)
	if reference == nil {
		t.Fatal("approval-resumed operation_started is missing planned_before_ref")
	}
	content, err := store.Open(*reference)
	if err != nil || string(content) != "awaiting approval" {
		t.Fatalf("before snapshot=%q err=%v", content, err)
	}
}

// TestLoopWorksWithoutArtifactStoreForSnapshot 锁定制品库缺席时补丁照常执行且
// 不产生快照引用（该次改写如实不可回退）。
func TestLoopWorksWithoutArtifactStoreForSnapshot(t *testing.T) {
	_, loop, journal, _ := snapshotFixture(t, map[string]string{"bare.txt": "plain"}, "allow")
	loop.Artifacts = nil
	runID := domain.NewRunID()
	appendInitialRunningEvents(t, journal, runID, "update bare")
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-bare", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"bare.txt","mode":"update","expected_sha256":"` + fileHash(t, "plain") + `","replacements":[{"old":"plain","new":"fancy"}]}`)}}, {Kind: ports.Done}},
		{{Kind: ports.TextDelta, Text: "done"}, {Kind: ports.Done}},
	}}
	loop.Provider = provider
	if _, err := loop.Run(context.Background(), RunInput{RunID: runID, Task: "update bare", WorkspaceHash: "workspace"}); err != nil {
		t.Fatal(err)
	}
	if reference := startedBeforeRef(t, journal, runID); reference != nil {
		t.Fatalf("no snapshot without artifact store: %+v", reference)
	}
}

// fileHash 计算测试内容的 sha256 引用形态（与工具层摘要前缀一致）。
func fileHash(t *testing.T, content string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(digest[:])
}
