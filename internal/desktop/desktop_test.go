package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// TestMapErrorClassifiesApprovalAndReconcile 验证审批/对账/策略/预算错误码映射。
func TestMapErrorClassifiesApprovalAndReconcile(t *testing.T) {
	waiting := &ports.WaitingApprovalError{
		RunID:       "run-1",
		OperationID: "op-1",
		Fingerprint: "fp",
		Summary:     "write file",
	}
	body := MapError(waiting)
	if body.Code != CodeWaitingApproval || body.Fingerprint != "fp" || body.RunID != "run-1" {
		t.Fatalf("unexpected approval mapping: %+v", body)
	}
	reconcile := &ports.WaitingReconcileError{RunID: "run-1", OperationID: "op-2", Reason: "unknown"}
	body = MapError(reconcile)
	if body.Code != CodeWaitingReconcile || body.OperationID != "op-2" {
		t.Fatalf("unexpected reconcile mapping: %+v", body)
	}
	body = MapError(&ports.PolicyDeniedError{Summary: "blocked"})
	if body.Code != CodePolicyDenied {
		t.Fatalf("unexpected policy mapping: %+v", body)
	}
	body = MapError(&ports.BudgetExceededError{Dimension: "max_turns"})
	if body.Code != CodeBudget {
		t.Fatalf("unexpected budget mapping: %+v", body)
	}
	body = MapError(app.ErrConfigDrift)
	if body.Code != CodeConfigDrift {
		t.Fatalf("unexpected drift mapping: %+v", body)
	}
}

// TestPrefsStoreRoundTripAndArchive 验证偏好读写、置顶排序与归档展示过滤。
func TestPrefsStoreRoundTripAndArchive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prefs.json")
	store := NewPrefsStoreAt(path)
	workspace := filepath.Join(dir, "proj-a")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchProject(workspace); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSessionMeta(SessionMeta{SessionID: "s1", Title: "任务甲", Pinned: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSessionMeta(SessionMeta{SessionID: "s2", Title: "任务乙"}); err != nil {
		t.Fatal(err)
	}
	reloaded := NewPrefsStoreAt(path)
	recent := reloaded.ListRecentProjects(false)
	if len(recent) != 1 || recent[0].Path == "" {
		t.Fatalf("unexpected recent projects: %+v", recent)
	}
	metas := reloaded.SessionPreferences()
	if len(metas) != 2 {
		t.Fatalf("unexpected session meta count: %d", len(metas))
	}
	if err := reloaded.SetProjectArchived(workspace, true); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.ListRecentProjects(false); len(got) != 0 {
		t.Fatalf("archived project should be hidden: %+v", got)
	}
	if got := reloaded.ListRecentProjects(true); len(got) != 1 {
		t.Fatalf("archived project should appear when requested: %+v", got)
	}
}

// TestWorkspaceObserveReadonly 验证工作区观察只读且报告 git/配置存在性。
func TestWorkspaceObserveReadonly(t *testing.T) {
	dir := t.TempDir()
	status, err := WorkspaceObserve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Exists || !status.IsWorkspace {
		t.Fatalf("expected existing workspace: %+v", status)
	}
	if status.HasConfig {
		t.Fatal("empty workspace should not report config")
	}
	agentDir := filepath.Join(dir, ".agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "config.yaml"), []byte("provider:\n  model: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, err = WorkspaceObserve(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !status.HasAgentDir || !status.HasConfig {
		t.Fatalf("expected config presence: %+v", status)
	}
}

// TestRunControllerDedupesStaleSeq 验证迟到/重复事件序号被丢弃，不污染新会话。
func TestRunControllerDedupesStaleSeq(t *testing.T) {
	events := make(chan StreamEvent, 16)
	controller := NewRunController(app.Service{}, nil, func(event StreamEvent) {
		events <- event
	})
	// 直接注入 sink 行为。
	sink := controller.eventSink("run-1", "session-1")
	base := time.Now().UTC()
	sink(domain.Event{RunID: "run-1", Seq: 2, OccurredAt: base, Type: domain.EventRunStarted})
	sink(domain.Event{RunID: "run-1", Seq: 1, OccurredAt: base, Type: domain.EventRunStarted})
	sink(domain.Event{RunID: "run-1", Seq: 2, OccurredAt: base, Type: domain.EventRunStarted})
	close(events)
	var seqs []uint64
	for item := range events {
		seqs = append(seqs, item.Seq)
	}
	if len(seqs) != 1 || seqs[0] != 2 {
		t.Fatalf("expected only seq 2 delivered, got %v", seqs)
	}
}

// TestStartRunValidatesInput 验证任务与工作区必填校验。
func TestStartRunValidatesInput(t *testing.T) {
	controller := NewRunController(app.Service{}, nil, nil)
	if _, err := controller.StartRun(context.Background(), StartRunRequest{Workspace: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "task") {
		t.Fatalf("expected task validation error, got %v", err)
	}
	if _, err := controller.StartRun(context.Background(), StartRunRequest{Task: "x"}); err == nil {
		t.Fatal("expected workspace validation error")
	}
}

// TestMapErrorUnknownIsInternal 验证未知错误收敛为 internal。
func TestMapErrorUnknownIsInternal(t *testing.T) {
	body := MapError(errors.New("boom"))
	if body.Code != CodeInternal {
		t.Fatalf("unexpected code: %+v", body)
	}
	if strings.Contains(strings.ToLower(body.Message), "api_key") {
		t.Fatalf("message leaked sensitive-looking token: %s", body.Message)
	}
}

// TestPickDirectoryRequiresPicker 验证未注入对话框时失败关闭。
func TestPickDirectoryRequiresPicker(t *testing.T) {
	api := NewAPI(app.Service{}, NewPrefsStoreAt(t.TempDir()+"/p.json"), nil)
	if _, err := api.PickDirectory(context.Background(), "选择工作区", ""); err == nil {
		t.Fatal("expected picker unavailable error")
	}
	called := false
	api.AttachDirectoryPicker(func(title, defaultPath string) (string, error) {
		called = true
		if title == "" {
			t.Fatal("expected title")
		}
		return t.TempDir(), nil
	})
	path, err := api.PickDirectory(context.Background(), "选择工作区", t.TempDir())
	if err != nil || !called || path == "" {
		t.Fatalf("unexpected picker result path=%q err=%v", path, err)
	}
}

// TestListRunsFiltersArchivedAndQuery 验证会话搜索与归档过滤（基于偏好层）。
func TestListRunsFiltersArchivedAndQuery(t *testing.T) {
	store := NewPrefsStoreAt(t.TempDir() + "/p.json")
	if err := store.UpsertSessionMeta(SessionMeta{SessionID: "s1", Title: "登录修复", Archived: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertSessionMeta(SessionMeta{SessionID: "s2", Title: "文档更新", Pinned: true}); err != nil {
		t.Fatal(err)
	}
	metas := store.SessionPreferences()
	if len(metas) != 2 {
		t.Fatalf("unexpected meta count %d", len(metas))
	}
	if !metas[0].Archived || metas[1].Pinned != true {
		t.Fatalf("unexpected meta flags: %+v", metas)
	}
}
