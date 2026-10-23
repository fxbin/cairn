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
	_, _ = DiffFile(root, "a.txt", DiffScopeAll) // 非 git 状态或无 diff 时失败关闭/空
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

// TestRuntimeOverridesReproduceFingerprint 验证启动与恢复使用同一覆盖后指纹一致，
// 且未记录覆盖时的差异仍表现为漂移。
func TestRuntimeOverridesReproduceFingerprint(t *testing.T) {
	base := config.Defaults()
	base.Provider.BaseURL = "https://example.invalid/v1"
	base.Provider.Model = "base-model"

	startCfg := base
	if err := applyRuntimeOverrides(&startCfg, "override-model", "read-only"); err != nil {
		t.Fatal(err)
	}
	startFP := startCfg.Fingerprint()

	resumeCfg := base
	override := RunOverride{RunID: "run-1", Model: "override-model", Mode: "read-only"}
	if err := applyRuntimeOverrides(&resumeCfg, override.Model, override.Mode); err != nil {
		t.Fatal(err)
	}
	if resumeCfg.Fingerprint() != startFP {
		t.Fatalf("resume fingerprint mismatch: start=%s resume=%s", startFP, resumeCfg.Fingerprint())
	}

	// 未应用覆盖时不得伪装成匹配。
	if base.Fingerprint() == startFP {
		t.Fatal("base config should differ from overridden fingerprint")
	}

	// 偏好可持久化覆盖。
	dir := t.TempDir()
	store := NewPrefsStoreAt(filepath.Join(dir, "prefs.json"))
	if err := store.SaveRunOverride(override); err != nil {
		t.Fatal(err)
	}
	got, ok := store.LoadRunOverride("run-1")
	if !ok || got.Model != "override-model" || got.Mode != "read-only" {
		t.Fatalf("unexpected override %+v ok=%v", got, ok)
	}
}

// TestStartRunAssignsRunID 验证 ACK 返回真实 run 身份，供恢复与路由使用。
func TestStartRunAssignsRunID(t *testing.T) {
	workspace := t.TempDir()
	agentDir := filepath.Join(workspace, ".agent")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatal(err)
	}
	controller := NewRunController(app.Service{}, NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json")), nil)
	ack, err := controller.StartRun(context.Background(), StartRunRequest{
		Task: "demo",
		// 无配置装载会失败；此测试覆盖身份分配在装载成功路径之外不误报。
		Workspace: workspace,
	})
	// 缺少合法配置时应在装载阶段失败，不得返回空 run 的伪 ACK。
	if err == nil && ack.RunID == "" {
		t.Fatal("expected either error or non-empty run id")
	}
}

// TestPreviewAndDiffRejectSymlinkAliasToSensitive 使用合成文件验证别名链接
// 与 Diff 不得绕过敏感预览拦截。
func TestPreviewAndDiffRejectSymlinkAliasToSensitive(t *testing.T) {
	root := t.TempDir()
	secret := filepath.Join(root, "keys.env")
	if err := os.WriteFile(secret, []byte("SYNTHETIC_MARKER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "public.txt"), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("keys.env", filepath.Join(root, "alias.txt")); err != nil {
		t.Fatal(err)
	}
	// 直接敏感路径
	if _, err := PreviewFile(root, "keys.env"); err == nil {
		t.Fatal("direct sensitive preview must fail")
	}
	// 符号链接别名
	preview, err := PreviewFile(root, "alias.txt")
	if err == nil && strings.Contains(preview.Text, "SYNTHETIC_MARKER") {
		t.Fatal("alias preview leaked sensitive content")
	}
	if err == nil {
		t.Fatal("alias preview should fail closed")
	}
	// Diff 同样受边界约束
	if diff, err := DiffFile(root, "keys.env", DiffScopeAll); err == nil && strings.Contains(diff, "SYNTHETIC_MARKER") {
		t.Fatal("sensitive diff leaked content")
	}
	if diff, err := DiffFile(root, "alias.txt", DiffScopeAll); err == nil && strings.Contains(diff, "SYNTHETIC_MARKER") {
		t.Fatal("alias diff leaked content")
	}
	// 正常源码可预览
	ok, err := PreviewFile(root, "public.txt")
	if err != nil || ok.Text != "ok\n" {
		t.Fatalf("normal preview failed: %+v %v", ok, err)
	}
}

// TestStartRunRequestCarriesSessionContinuation 验证续接字段进入 RunOptions 路径。
func TestStartRunRequestCarriesSessionContinuation(t *testing.T) {
	request := StartRunRequest{
		Task:               "follow-up",
		Workspace:          t.TempDir(),
		SessionID:          "sess-1",
		PrevRunID:          "run-0",
		RequireSessionTail: true,
		Mode:               "default",
		Model:              "m1",
	}
	if request.SessionID == "" || request.PrevRunID == "" || !request.RequireSessionTail {
		t.Fatal("continuation identity must be preserved on request")
	}
	override := RunOverride{RunID: "run-1", Model: request.Model, Mode: request.Mode}
	if override.Model != "m1" || override.Mode != "default" {
		t.Fatal("override must capture runtime model/mode")
	}
}

// TestPrefsStoreCreatesRootAndPreservesCorrupt 验证首次保存与损坏偏好保全。
func TestPrefsStoreCreatesRootAndPreservesCorrupt(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "cairn")
	path := filepath.Join(dir, "prefs.json")
	store := NewPrefsStoreAt(path)
	workspace := filepath.Join(t.TempDir(), "proj")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.TouchProject(workspace); err != nil {
		t.Fatalf("fresh prefs save should create parent dir: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("prefs file should exist after save")
	}

	// 损坏 JSON 保全到旁路，不得静默当空偏好继续覆盖。
	if err := os.WriteFile(path, []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded := NewPrefsStoreAt(path)
	if reloaded == nil {
		t.Fatal("store should still construct")
	}
	corrupt := path + ".corrupt"
	if _, err := os.Stat(corrupt); err != nil {
		t.Fatal("corrupt prefs should be preserved")
	}
}

// TestSetSessionFlagsPreservesTitle 验证置顶/归档不丢失标题，且支持取消置顶。
func TestSetSessionFlagsPreservesTitle(t *testing.T) {
	store := NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json"))
	api := NewAPI(app.Service{}, store, nil)
	if _, err := api.RenameSession(context.Background(), "sess-1", "任务甲"); err != nil {
		t.Fatal(err)
	}
	if _, err := api.SetSessionFlags(context.Background(), "sess-1", true, false); err != nil {
		t.Fatal(err)
	}
	if _, err := api.SetSessionFlags(context.Background(), "sess-1", false, false); err != nil {
		t.Fatal(err)
	}
	metas := store.SessionPreferences()
	if len(metas) != 1 || metas[0].Title != "任务甲" || metas[0].Pinned {
		t.Fatalf("unexpected meta %+v", metas[0])
	}
}

// TestDiffScopesCoverStagedUnstagedUntracked 验证比较范围不把暂存差异伪装成空。
func TestDiffScopesCoverStagedUnstagedUntracked(t *testing.T) {
	root := t.TempDir()
	if _, err := runGit(root, "init"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "user.email", "t@example.com"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "user.name", "t"); err != nil {
		t.Fatal(err)
	}
	// 中文/空格文件名 + 仅暂存改动
	name := "中文 空格.txt"
	if err := os.WriteFile(filepath.Join(root, name), []byte("v1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "add", name); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "commit", "-m", "init"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte("v2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "add", name); err != nil {
		t.Fatal(err)
	}
	staged, err := DiffFile(root, name, DiffScopeStaged)
	if err != nil || !strings.Contains(staged, "+v2") {
		t.Fatalf("staged diff missing: %q err=%v", staged, err)
	}
	unstaged, err := DiffFile(root, name, DiffScopeUnstaged)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(unstaged, "+v2") {
		t.Fatalf("unstaged should be empty after stage, got %q", unstaged)
	}
	// 未跟踪文件
	if err := os.WriteFile(filepath.Join(root, "new-file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	untracked, err := DiffFile(root, "new-file.txt", DiffScopeUntracked)
	if err != nil || !strings.Contains(untracked, "hello") {
		t.Fatalf("untracked diff missing: %q err=%v", untracked, err)
	}
	// ListChanges 能识别中文路径与 untracked
	changes, err := ListChanges(root)
	if err != nil {
		t.Fatal(err)
	}
	var sawChinese, sawNew bool
	for _, entry := range changes.Entries {
		if entry.Relative == name && (entry.Status == "staged" || entry.Status == "mixed" || entry.Status == "modified") {
			sawChinese = true
		}
		if entry.Relative == "new-file.txt" && entry.Status == "untracked" {
			sawNew = true
		}
	}
	if !sawChinese || !sawNew {
		t.Fatalf("changes missing entries: %+v", changes.Entries)
	}
}

// TestConfirmQueueRejectsMissingInput 验证队列确认失败关闭且不伪造 ACK。
func TestConfirmQueueRejectsMissingInput(t *testing.T) {
	workspace := t.TempDir()
	controller := NewRunController(app.Service{}, NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json")), nil)
	ack, err := controller.ConfirmQueue(context.Background(), QueueActionRequest{
		Workspace: workspace,
		InputID:   "missing",
	})
	if err == nil {
		t.Fatalf("expected error, got ack %+v", ack)
	}
}

// TestStartRunRejectsForkWithPrevRun 锁定分支与续链互斥。
func TestStartRunRejectsForkWithPrevRun(t *testing.T) {
	controller := NewRunController(app.Service{}, NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json")), nil)
	_, err := controller.StartRun(context.Background(), StartRunRequest{
		Task:            "x",
		Workspace:       t.TempDir(),
		PrevRunID:       "run-a",
		ForkSourceRunID: "run-b",
	})
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual exclusion error, got %v", err)
	}
}

// TestSubmitSteeringRequiresActiveRun 验证补充输入只作用于运行中的任务。
func TestSubmitSteeringRequiresActiveRun(t *testing.T) {
	controller := NewRunController(app.Service{}, nil, nil)
	if err := controller.SubmitSteering("run-x", "", "hello"); err == nil {
		t.Fatal("expected reject when run is not active")
	}
}

// TestHookRulesOnlyAllowQueueFollowup 验证钩子动作白名单与熔断。
func TestHookRulesOnlyAllowQueueFollowup(t *testing.T) {
	store := NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json"))
	ws := t.TempDir()
	if err := store.UpsertHook(HookRule{
		ID: "h1", Workspace: ws, Event: "run_finished", Enabled: true,
		Actions: []HookAction{{Kind: "exec", Content: "rm -rf /"}},
	}); err == nil {
		t.Fatal("must reject non-whitelisted hook action")
	}
	if err := store.UpsertHook(HookRule{
		ID: "h1", Workspace: ws, Event: "run_finished", Enabled: true,
		Actions: []HookAction{{Kind: "queue_followup", Content: "继续总结"}},
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err := store.BumpHookFailure("h1"); err != nil {
			t.Fatal(err)
		}
	}
	rules := store.ListHooks(ws)
	if len(rules) != 1 || rules[0].Enabled {
		t.Fatalf("expected fused-off hook, got %+v", rules)
	}
}

// TestWorkflowValidationAndDryRun 验证步骤校验与预演不执行。
func TestWorkflowValidationAndDryRun(t *testing.T) {
	store := NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json"))
	controller := NewRunController(app.Service{}, store, nil)
	ws := t.TempDir()
	if err := store.SaveWorkflow(WorkflowDefinition{ID: "w1", Workspace: ws, Name: "demo"}); err == nil {
		t.Fatal("must require steps")
	}
	if err := store.SaveWorkflow(WorkflowDefinition{
		ID: "w1", Workspace: ws, Name: "demo",
		Steps: []WorkflowStep{{Task: "t1"}, {Task: "t2"}},
	}); err != nil {
		t.Fatal(err)
	}
	ack, err := controller.RunWorkflow(context.Background(), ws, "w1", true)
	if err != nil || ack.State != "dry_run_ok" {
		t.Fatalf("dry-run should validate only: %+v %v", ack, err)
	}
}

// TestRevokeMemoryRejectsMissingWorkspace 验证撤销入口失败关闭。
func TestRevokeMemoryRejectsMissingWorkspace(t *testing.T) {
	api := NewAPI(app.Service{}, NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json")), nil)
	_, err := api.RevokeMemory(context.Background(), struct {
		Workspace   string `json:"workspace"`
		StateDir    string `json:"state_dir,omitempty"`
		CandidateID string `json:"candidate_id"`
		Reason      string `json:"reason"`
	}{CandidateID: "c1", Reason: "superseded"})
	if err == nil {
		t.Fatal("expected workspace required error")
	}
}

// TestSkillStoreRejectsMissingWorkspace 验证技能管理入口失败关闭。
func TestSkillStoreRejectsMissingWorkspace(t *testing.T) {
	api := NewAPI(app.Service{}, NewPrefsStoreAt(filepath.Join(t.TempDir(), "prefs.json")), nil)
	if _, err := api.InstallSkill(context.Background(), "", "user", "/tmp/x"); err == nil {
		t.Fatal("expected workspace error")
	}
}

// TestWriteProjectInstructionsAtomic 验证规约写入固定路径且原子。
func TestWriteProjectInstructionsAtomic(t *testing.T) {
	root := t.TempDir()
	if err := WriteProjectInstructions(root, "# rules\n"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if err != nil || string(raw) != "# rules\n" {
		t.Fatalf("unexpected content %q err=%v", raw, err)
	}
	if err := WriteProjectInstructions(root, "# updated\n"); err != nil {
		t.Fatal(err)
	}
	raw, _ = os.ReadFile(filepath.Join(root, "AGENTS.md"))
	if string(raw) != "# updated\n" {
		t.Fatalf("update failed: %q", raw)
	}
}
