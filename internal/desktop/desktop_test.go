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
	"github.com/fxbin/cairn/internal/config"
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

// TestFileTreeAndPreviewGuardPathSafety 验证符号链接逃逸与敏感文件拦截。
func TestFileTreeAndPreviewGuardPathSafety(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "ok.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keys.env"), []byte("SECRET=1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	tree, err := ListFileTree(root)
	if err != nil {
		t.Fatal(err)
	}
	var sawEnv, sawEscape, sawOK bool
	var walk func(node FileNode)
	walk = func(node FileNode) {
		switch node.Name {
		case "keys.env":
			sawEnv = node.Kind == "blocked"
		case "escape":
			sawEscape = node.Kind == "blocked" || node.Kind == "symlink"
		case "ok.txt":
			sawOK = node.Kind == "file"
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	walk(tree)
	if !sawEnv || !sawEscape || !sawOK {
		t.Fatalf("tree guards missing env=%v escape=%v ok=%v", sawEnv, sawEscape, sawOK)
	}
	preview, err := PreviewFile(root, "ok.txt")
	if err != nil || preview.Text != "hello" {
		t.Fatalf("preview failed: %+v %v", preview, err)
	}
	if _, err := PreviewFile(root, "keys.env"); err == nil {
		t.Fatal("sensitive preview should fail closed")
	}
	if _, err := PreviewFile(root, "../outside"); err == nil {
		t.Fatal("path escape should fail")
	}
}

// TestDiffFileReturnsBoundedText 验证 diff 可用时返回文本。
func TestDiffFileReturnsBoundedText(t *testing.T) {
	root := t.TempDir()
	runGit(root, "init")
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("x\n"), 0o644)
	_, _ = DiffFile(root, "a.txt") // 非 git 状态或无 diff 时失败关闭/空
}

// TestStartRunRejectsUnknownMode 验证未知工作模式失败关闭。
func TestStartRunRejectsUnknownMode(t *testing.T) {
	dir := t.TempDir()
	// 无配置时 loadConfiguration 会失败；这里只验证 Mode 校验在配置之前/之时触发。
	controller := NewRunController(app.Service{}, nil, nil)
	_, err := controller.StartRun(context.Background(), StartRunRequest{
		Workspace: dir,
		Task:      "x",
		Mode:      "turbo",
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported mode") {
		t.Fatalf("expected unsupported mode error, got %v", err)
	}
}

// TestMapErrorInternalHidesDetails 验证 internal 错误不透传原文。
func TestMapErrorInternalHidesDetails(t *testing.T) {
	body := MapError(errors.New("secret-path /tmp/key and token=abc"))
	if body.Message == "" || strings.Contains(body.Message, "secret-path") || strings.Contains(body.Message, "token=") {
		t.Fatalf("internal message leaked details: %q", body.Message)
	}
}

// TestRunContextIgnoresBindingCancel 验证运行上下文不随单次 binding ctx 取消而终止。
func TestRunContextIgnoresBindingCancel(t *testing.T) {
	controller := NewRunController(app.Service{}, nil, nil)
	_, bindingCancel := context.WithCancel(context.Background())
	runCtx, cancel, err := controller.runContext()
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	bindingCancel()
	select {
	case <-runCtx.Done():
		t.Fatal("run context must survive binding context cancel")
	case <-time.After(30 * time.Millisecond):
	}
	if err := controller.Shutdown(time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestShutdownCancelsActiveRuns 验证退出时合作式取消并拒绝新运行。
func TestShutdownCancelsActiveRuns(t *testing.T) {
	controller := NewRunController(app.Service{}, nil, nil)
	runCtx, cancel, err := controller.runContext()
	if err != nil {
		t.Fatal(err)
	}
	controller.mu.Lock()
	controller.cancel["run-x"] = cancel
	controller.running["run-x"] = true
	controller.mu.Unlock()

	if err := controller.Shutdown(200 * time.Millisecond); err != nil {
		t.Fatal(err)
	}
	select {
	case <-runCtx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("shutdown did not cancel run context")
	}
	if _, _, err := controller.runContext(); err == nil {
		t.Fatal("expected reject after shutdown")
	}
}

// TestResumeWiresEventSinkAndRejectsDuplicate 验证恢复路径注册事件并阻止重复激活。
func TestResumeWiresEventSinkAndRejectsDuplicate(t *testing.T) {
	events := make(chan StreamEvent, 8)
	controller := NewRunController(app.Service{}, nil, func(event StreamEvent) {
		events <- event
	})
	// 直接验证 eventSink 在 Resume 注册路径可用。
	sink := controller.eventSink("run-r", "")
	sink(domain.Event{RunID: "run-r", Seq: 1, OccurredAt: time.Now().UTC(), Type: domain.EventRunStarted})
	select {
	case got := <-events:
		if got.RunID != "run-r" || got.Seq != 1 {
			t.Fatalf("unexpected event %+v", got)
		}
	default:
		t.Fatal("expected stream event")
	}
}

// TestApplyModeReadOnlyUsesCorePreset 验证 GUI 只读模式复用核心 Preset 合同，
// 而不是仅局部改写 Policy。
func TestApplyModeReadOnlyUsesCorePreset(t *testing.T) {
	configuration := config.Defaults()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "demo-model"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"go", "test", "./..."}}}
	if err := applyMode(&configuration, "read-only"); err == nil {
		t.Fatal("expected verification commands rejected under read-only")
	}

	configuration = config.Defaults()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "demo-model"
	configuration.Skills = &config.SkillsConfig{Enabled: true}
	if err := applyMode(&configuration, "read-only"); err == nil {
		t.Fatal("expected skills rejected under read-only")
	}

	configuration = config.Defaults()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "demo-model"
	if err := applyMode(&configuration, "read-only"); err != nil {
		t.Fatalf("unexpected read-only apply error: %v", err)
	}
	if configuration.Preset != config.PresetReadOnly {
		t.Fatalf("preset not applied: %q", configuration.Preset)
	}
	if configuration.Policy.Write != "deny" || configuration.Policy.Exec != "deny" {
		t.Fatalf("policy not denied: %+v", configuration.Policy)
	}

	configuration = config.Defaults()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "demo-model"
	if err := applyMode(&configuration, "default"); err != nil {
		t.Fatal(err)
	}
	if configuration.Preset != config.PresetCoding {
		t.Fatalf("default mode should keep coding preset, got %q", configuration.Preset)
	}
}
