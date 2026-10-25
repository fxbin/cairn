package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// updateThenFinalProvider 先提交一个 update 补丁再收尾，模拟真实链上任务的
// 工作区写入。
type updateThenFinalProvider struct {
	calls    int
	expected string
	oldText  string
	newText  string
	path     string
}

func (provider *updateThenFinalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if provider.calls == 1 {
		arguments, err := json.Marshal(map[string]any{
			"path": provider.path, "mode": "update", "expected_sha256": provider.expected,
			"replacements": []map[string]string{{"old": provider.oldText, "new": provider.newText}},
		})
		if err != nil {
			return err
		}
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-update", Name: "apply_patch", Arguments: arguments}}); err != nil {
			return err
		}
	} else if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "patched and done"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// runApprovedUpdateTask 以 ask 写策略跑一个 update 任务：先暂停审批、批准后
// 完成，返回链上 run 与服务句柄。
func runApprovedUpdateTask(t *testing.T, workspace, stateDir string, path, before, after string) (domain.RunID, Service, config.Config) {
	t.Helper()
	provider := &updateThenFinalProvider{expected: testContentHash(t, before), oldText: before, newText: after, path: path}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "ask"
	_, err := service.Run(context.Background(), RunOptions{Task: "update " + path, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err == nil {
		t.Fatal("ask policy must pause before the patch")
	}
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("run must wait for approval: %v", err)
	}
	finished, err := service.Resume(context.Background(), ResumeOptions{RunID: waiting.RunID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Result == nil || finished.Result.State != string(domain.RunCompleted) {
		t.Fatalf("finished=%#v", finished.Result)
	}
	return finished.ID, service, configuration
}

// testContentHash 计算测试内容的 sha256 引用形态。
func testContentHash(t *testing.T, content string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// TestRewindRejectsInvalidBuildIdentityBeforePersistence 锁定回退入口同样在 Boot
// 前验证编译期身份，不得因目标 run 查询而创建任何状态文件。
func TestRewindRejectsInvalidBuildIdentityBeforePersistence(t *testing.T) {
	injectInvalidBuildIdentity(t)
	providerCalled := false
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		providerCalled = true
		return finalProvider{}, nil
	}}
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, err := service.Rewind(context.Background(), RewindOptions{
		Workspace: workspace,
		StateDir:  stateDir,
		Config:    withTestProvider(config.Defaults()),
		Runs:      []domain.RunID{domain.NewRunID()},
	})
	if err == nil || !strings.Contains(err.Error(), "invalid current build identity") {
		t.Fatalf("rewind error = %v", err)
	}
	if providerCalled {
		t.Fatal("provider was constructed before build identity validation")
	}
	assertStateDirEmpty(t, stateDir)
}

// TestRewindRestoresFileFromBeforeArtifact 锁定回退主合同：update 反放经审批
// 恢复 before 内容，回退 run 完成且走完整事件链。
func TestRewindRestoresFileFromBeforeArtifact(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	const path = "note.txt"
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runID, service, configuration := runApprovedUpdateTask(t, workspace, stateDir, path, "original", "updated")
	if content, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(content) != "updated" {
		t.Fatalf("note.txt=%q err=%v", content, err)
	}
	_, err := service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{runID}})
	if err == nil {
		t.Fatal("rewind must pause for approval")
	}
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) || waiting.RunID == runID {
		t.Fatalf("rewind must wait for its own approval: %v", err)
	}
	finished, err := service.Resume(context.Background(), ResumeOptions{RunID: waiting.RunID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Result == nil || finished.Result.State != string(domain.RunCompleted) {
		t.Fatalf("rewind result=%#v", finished.Result)
	}
	if content, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(content) != "original" {
		t.Fatalf("note.txt=%q err=%v（应恢复为 before 内容）", content, err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, waiting.RunID, false)
	if err != nil || inspected.Projection.State != domain.RunCompleted {
		t.Fatalf("rewind run projection=%+v err=%v", inspected.Projection, err)
	}
}

// TestRewindContinuesByIdempotentReentry 锁定崩溃后续跑：等待审批的回退 run
// 由再次 Rewind 携带审批决定收束。
func TestRewindContinuesByIdempotentReentry(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	const path = "note.txt"
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runID, service, configuration := runApprovedUpdateTask(t, workspace, stateDir, path, "original", "updated")
	options := RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{runID}}
	_, first := service.Rewind(context.Background(), options)
	var waiting *ports.WaitingApprovalError
	if !errors.As(first, &waiting) {
		t.Fatalf("rewind must pause: %v", first)
	}
	options.Approval = ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "reentry"}
	finished, err := service.Rewind(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if finished.Result == nil || finished.Result.State != string(domain.RunCompleted) {
		t.Fatalf("reentry result=%#v", finished.Result)
	}
	if content, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(content) != "original" {
		t.Fatalf("note.txt=%q err=%v", content, err)
	}
}

// countingFinalProvider 记录模型流调用次数；回退恢复路径必须始终保持为零。
type countingFinalProvider struct {
	calls int
}

func (provider *countingFinalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "unexpected model call"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// putActiveRewindRun 写入一个等待恢复的回退 run；frozen 为空时使用当前 v2
// 冻结格式，非空时原样存储以覆盖损坏或旧格式场景。
func putActiveRewindRun(t *testing.T, workspace, stateDir string, configuration config.Config, runs []domain.RunID, frozen []byte, pending bool) (domain.RunID, domain.OperationID, int) {
	t.Helper()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := openArtifactStore(stateDir, workspaceHash, configuration.Output)
	if err != nil {
		t.Fatal(err)
	}
	var configRef artifact.Ref
	if len(frozen) == 0 {
		configRef = putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
	} else {
		configRef, err = artifacts.Put(frozen, "application/json")
		if err != nil {
			t.Fatal(err)
		}
	}
	planRef, err := artifacts.Put(artifact.MustEncodeRedactedJSON(rewindPlanArtifact{Version: rewindPlanVersion, Kind: rewindPlanKind, Runs: runs}), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	runID := domain.NewRunID()
	drafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: artifact.MustEncodeRedactedJSON(struct {
			domain.RunCreatedPayload
			ConfigRef artifact.Ref `json:"config_ref"`
			TaskRef   artifact.Ref `json:"task_ref"`
		}{RunCreatedPayload: domain.RunCreatedPayload{Task: "active rewind", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint()}, ConfigRef: configRef, TaskRef: planRef})},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted},
	}
	var operationID domain.OperationID
	if pending {
		turnID := domain.NewTurnID()
		operationID = domain.NewOperationID()
		drafts = append(drafts,
			domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID},
			domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"rewind-pending","tool_name":"apply_patch","arguments":{"path":"note.txt","mode":"create","content":"x"}}`)},
			domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"sha256:approved"}`)},
			domain.EventDraft{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)},
		)
	}
	for index, draft := range drafts {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
	return runID, operationID, len(drafts)
}

// TestRewindEnforcesRequiredAdmissionAcrossEntryPoints 锁定新建、Resume 与
// Reconcile 回退入口在创建事件或继续脚本化工具计划前都经过 required 准入门。
func TestRewindEnforcesRequiredAdmissionAcrossEntryPoints(t *testing.T) {
	t.Run("new rewind", func(t *testing.T) {
		workspace, stateDir := t.TempDir(), t.TempDir()
		configuration := withTestProvider(config.Defaults())
		configuration.Provider.Admission = config.ProviderAdmissionRequired
		provider := &countingFinalProvider{}
		service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
		if _, err := service.Rewind(context.Background(), RewindOptions{
			Workspace: workspace,
			StateDir:  stateDir,
			Config:    configuration,
			Runs:      []domain.RunID{domain.NewRunID()},
		}); err == nil || !strings.Contains(err.Error(), "admission gate") {
			t.Fatalf("rewind without admission decision error = %v", err)
		}
		if provider.calls != 0 {
			t.Fatalf("provider stream calls=%d, want 0", provider.calls)
		}
		_, workspaceHash, err := workspaceFor(workspace, configuration)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := openReadOnlyJournal(stateDir, workspaceHash)
		if err != nil {
			t.Fatal(err)
		}
		defer journal.Close()
		active, err := journal.NonTerminalRuns(context.Background(), workspaceHash)
		if err != nil {
			t.Fatal(err)
		}
		if len(active) != 0 {
			t.Fatalf("failed rewind created active runs: %+v", active)
		}
	})

	for _, testCase := range []struct {
		name      string
		pending   bool
		reconcile bool
	}{
		{name: "resume", pending: false},
		{name: "reconcile", pending: true, reconcile: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace, stateDir := t.TempDir(), t.TempDir()
			configuration := withTestProvider(config.Defaults())
			configuration.Provider.Admission = config.ProviderAdmissionRequired
			targetRuns := []domain.RunID{domain.NewRunID()}
			runID, operationID, initialEvents := putActiveRewindRun(t, workspace, stateDir, configuration, targetRuns, nil, testCase.pending)
			provider := &countingFinalProvider{}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
			var runErr error
			if testCase.reconcile {
				_, runErr = service.Reconcile(context.Background(), ReconcileOptions{
					RunID: runID, OperationID: operationID, Verdict: "not_applied", Note: "not applied",
					Workspace: workspace, StateDir: stateDir, Config: configuration,
				})
			} else {
				_, runErr = service.Resume(context.Background(), ResumeOptions{
					RunID: runID, Workspace: workspace, StateDir: stateDir, Config: configuration,
				})
			}
			if runErr == nil || !strings.Contains(runErr.Error(), "admission gate") {
				t.Fatalf("%s without admission decision error = %v", testCase.name, runErr)
			}
			if provider.calls != 0 {
				t.Fatalf("provider stream calls=%d, want 0", provider.calls)
			}
			_, workspaceHash, err := workspaceFor(workspace, configuration)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := openReadOnlyJournal(stateDir, workspaceHash)
			if err != nil {
				t.Fatal(err)
			}
			defer journal.Close()
			events, err := journal.Load(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != initialEvents {
				t.Fatalf("failed %s appended events: %d", testCase.name, len(events))
			}
		})
	}
}

// TestRewindReentryRejectsLegacyFrozenConfig 锁定 Rewind 幂等重入在任何工具
// 推导和执行前拒绝旧版直接序列化 Config 的冻结制品。
func TestRewindReentryRejectsLegacyFrozenConfig(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	targetRuns := []domain.RunID{domain.NewRunID()}
	legacyFrozen, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	runID, _, initialEvents := putActiveRewindRun(t, workspace, stateDir, configuration, targetRuns, legacyFrozen, false)
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &countingFinalProvider{}, nil }}
	if _, err := service.Rewind(context.Background(), RewindOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: targetRuns,
	}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("legacy frozen config reentry error = %v", err)
	}
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openReadOnlyJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != initialEvents {
		t.Fatalf("failed reentry appended events: %d", len(events))
	}
}

// TestRewindReconcileRespectsAppliedVerdict 锁定回退副作用悬挂后的恢复语义：
// applied_success 跳过已生效调用，not_applied 则重新提议并在审批后执行；两条
// 路径都不得进入模型循环。
func TestRewindReconcileRespectsAppliedVerdict(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		verdict string
	}{
		{name: "applied_success skips execution", verdict: "applied_success"},
		{name: "not_applied retries execution", verdict: "not_applied"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			workspace, stateDir := t.TempDir(), t.TempDir()
			const path = "note.txt"
			if err := os.WriteFile(filepath.Join(workspace, path), []byte("original"), 0o600); err != nil {
				t.Fatal(err)
			}
			sourceRunID, _, configuration := runApprovedUpdateTask(t, workspace, stateDir, path, "original", "updated")
			configuration.Policy.Write = "ask"
			provider := &countingFinalProvider{}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
			_, rewindErr := service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{sourceRunID}})
			var waiting *ports.WaitingApprovalError
			if !errors.As(rewindErr, &waiting) {
				t.Fatalf("rewind must pause before operation: %v", rewindErr)
			}
			_, workspaceHash, err := workspaceFor(workspace, configuration)
			if err != nil {
				t.Fatal(err)
			}
			journal, err := openJournal(stateDir, workspaceHash)
			if err != nil {
				t.Fatal(err)
			}
			events, err := journal.Load(context.Background(), waiting.RunID)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := domain.Replay(events)
			if err != nil || projection.CurrentTurn == nil {
				t.Fatalf("rewind projection=%+v err=%v", projection, err)
			}
			turnID := *projection.CurrentTurn
			operationID := waiting.OperationID
			if _, err := journal.Append(context.Background(), projection.LastSeq, domain.EventDraft{RunID: waiting.RunID, WorkspaceHash: workspaceHash, Type: domain.EventApprovalGranted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"fingerprint":"` + waiting.Fingerprint + `","actor":"test"}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(context.Background(), projection.LastSeq+1, domain.EventDraft{RunID: waiting.RunID, WorkspaceHash: workspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","fingerprint":"` + waiting.Fingerprint + `"}`)}); err != nil {
				t.Fatal(err)
			}
			if _, err := journal.Append(context.Background(), projection.LastSeq+2, domain.EventDraft{RunID: waiting.RunID, WorkspaceHash: workspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"note.txt","planned_before":"` + testContentHash(t, "updated") + `","planned_after":"` + testContentHash(t, "original") + `"}`)}); err != nil {
				t.Fatal(err)
			}
			if err := journal.Close(); err != nil {
				t.Fatal(err)
			}
			if testCase.verdict == "applied_success" {
				if err := os.WriteFile(filepath.Join(workspace, path), []byte("original"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			result, reconcileErr := service.Reconcile(context.Background(), ReconcileOptions{RunID: waiting.RunID, OperationID: operationID, Verdict: testCase.verdict, Note: "test reconciliation", Workspace: workspace, StateDir: stateDir, Config: configuration})
			if testCase.verdict == "not_applied" {
				if !errors.As(reconcileErr, &waiting) {
					t.Fatalf("not_applied must request approval for retry: %v", reconcileErr)
				}
				result, reconcileErr = service.Resume(context.Background(), ResumeOptions{RunID: waiting.RunID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "retry"}})
			}
			if reconcileErr != nil || result.Result == nil || result.Result.State != string(domain.RunCompleted) {
				t.Fatalf("result=%#v err=%v", result, reconcileErr)
			}
			if provider.calls != 0 {
				t.Fatalf("provider stream calls=%d, want 0", provider.calls)
			}
			if content, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(content) != "original" {
				t.Fatalf("note.txt=%q err=%v", content, err)
			}
			inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, waiting.RunID, true)
			if err != nil {
				t.Fatal(err)
			}
			proposals := 0
			for _, event := range inspected.Events {
				if event.Type == domain.EventToolCallProposed {
					proposals++
				}
			}
			wantProposals := 1
			if testCase.verdict == "not_applied" {
				wantProposals = 2
			}
			if proposals != wantProposals {
				t.Fatalf("tool proposals=%d, want %d", proposals, wantProposals)
			}
		})
	}
}

// TestRewindDeletesCreatedByReversal 锁定 create 反放：原创建的文件被审批后
// 删除，无需任何 before 制品。
func TestRewindDeletesCreatedByReversal(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	provider := &createThenFinalProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	configuration := withTestProvider(config.Defaults())
	configuration.Policy.Write = "ask"
	result, err := service.Run(context.Background(), RunOptions{Task: "create fresh", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err == nil {
		t.Fatal("ask policy must pause")
	}
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("run must wait: %v", err)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: waiting.RunID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "created.txt")); err != nil {
		t.Fatalf("created.txt should exist: %v", err)
	}
	_, rewindErr := service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{result.ID}})
	var rewindWaiting *ports.WaitingApprovalError
	if !errors.As(rewindErr, &rewindWaiting) {
		t.Fatalf("rewind must pause: %v", rewindErr)
	}
	if _, err := service.Resume(context.Background(), ResumeOptions{RunID: rewindWaiting.RunID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: rewindWaiting.Fingerprint, Approve: true, Reason: "test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, "created.txt")); !os.IsNotExist(err) {
		t.Fatalf("created.txt should be removed by rewind: %v", err)
	}
}

// createThenFinalProvider 提交一个 create 补丁再收尾。
type createThenFinalProvider struct{ calls int }

func (provider *createThenFinalProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	provider.calls++
	if provider.calls == 1 {
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "patch-create", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"created.txt","mode":"create","content":"created"}`)}}); err != nil {
			return err
		}
	} else if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "created"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// TestRewindRefusesWithoutBeforeSnapshot 锁定旧运行失败关闭：无 before 制品
// 的 update 不可回退，拒绝发生在创建任何回退 run 之前。
func TestRewindRefusesWithoutBeforeSnapshot(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	const beforeBody = "legacy original"
	const afterBody = "legacy applied"
	if err := os.WriteFile(filepath.Join(workspace, "note.txt"), []byte(afterBody), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := withTestProvider(config.Defaults())
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	runID := domain.NewRunID()
	turnID := domain.NewTurnID()
	operationID := domain.NewOperationID()
	legacyDrafts := []domain.EventDraft{
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated, Payload: json.RawMessage(`{"task":"legacy update","workspace":"` + workspace + `"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted, Payload: json.RawMessage(`{"invocation":"invoke_legacy"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID, Payload: json.RawMessage(`{"turn":1}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"call_id":"legacy-1","tool_name":"apply_patch","arguments":{"path":"note.txt","mode":"update","expected_sha256":"` + testContentHash(t, beforeBody) + `","replacements":[{"old":"` + beforeBody + `","new":"` + afterBody + `"}]}}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"effect":"allow","rule":"workspace_write","fingerprint":"sha256:legacy"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"note.txt","planned_before":"` + testContentHash(t, beforeBody) + `","planned_after":"` + testContentHash(t, afterBody) + `"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventOperationSucceeded, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"tool_name":"apply_patch","result":{"ok":true,"code":"ok","preview":"updated note.txt"}}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: json.RawMessage(`{"ok":true,"code":"ok","preview":"updated note.txt"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: json.RawMessage(`{"verification_id":"verify-legacy","last_mutation_seq":6,"commands_fingerprint":"sha256:0000000000000000000000000000000000000000000000000000000000000000","workspace_observation":"sha256:0000000000000000000000000000000000000000000000000000000000000000"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: json.RawMessage(`{"verification_id":"verify-legacy","ok":true}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: json.RawMessage(`{"outcome":"final"}`)},
		{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCompleted, Payload: json.RawMessage(`{"final_response":"done"}`)},
	}
	for index, draft := range legacyDrafts {
		if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	_, err = service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{runID}})
	if err == nil || !strings.Contains(err.Error(), "before 快照") {
		t.Fatalf("legacy run must be refused honestly: %v", err)
	}
	readOnly, readErr := openReadOnlyJournal(stateDir, workspaceHash)
	if readErr != nil {
		t.Fatal(readErr)
	}
	defer readOnly.Close()
	active, activeErr := readOnly.NonTerminalRuns(context.Background(), workspaceHash)
	if activeErr != nil || len(active) != 0 {
		t.Fatalf("refused rewind must not leave an active run: %v %v", active, activeErr)
	}
}

// TestRewindRefusesOnWorkspaceDrift 锁定漂移失败关闭：链外篡改文件后回退拒
// 绝执行，且不留下非终态 run。
func TestRewindRefusesOnWorkspaceDrift(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	const path = "note.txt"
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runID, service, configuration := runApprovedUpdateTask(t, workspace, stateDir, path, "original", "updated")
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{runID}})
	if err == nil || !strings.Contains(err.Error(), "不符") {
		t.Fatalf("drifted workspace must be refused: %v", err)
	}
	if content, readErr := os.ReadFile(filepath.Join(workspace, path)); readErr != nil || string(content) != "tampered" {
		t.Fatalf("refused rewind must not touch files: %q %v", content, readErr)
	}
	_, workspaceHash, hashErr := workspaceFor(workspace, configuration)
	if hashErr != nil {
		t.Fatal(hashErr)
	}
	readOnly, readErr := openReadOnlyJournal(stateDir, workspaceHash)
	if readErr != nil {
		t.Fatal(readErr)
	}
	defer readOnly.Close()
	active, activeErr := readOnly.NonTerminalRuns(context.Background(), workspaceHash)
	if activeErr != nil || len(active) != 0 {
		t.Fatalf("refused rewind must not leave an active run: %v %v", active, activeErr)
	}
}

// TestRewindRejectsEmptySelection 锁定空选择与无补丁链段的诚实反馈。
func TestRewindRejectsEmptySelection(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	configuration := withTestProvider(config.Defaults())
	if _, err := service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil {
		t.Fatal("empty run list must be rejected")
	}
	result, err := service.Run(context.Background(), RunOptions{Task: "no patches", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{result.ID}})
	if err == nil || !strings.Contains(err.Error(), "无需回退") {
		t.Fatalf("patch-free run must be reported as nothing to rewind: %v", err)
	}
}

// TestRewindReversesSequentialPatchesOnSameFile 锁定逆序模拟链：同一文件跨
// 任务连续两次改写，一次回退逐项反放，最终恢复到链前内容（后项反放的替换
// 基线来自磁盘，前项来自 before 制品）。
func TestRewindReversesSequentialPatchesOnSameFile(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	const path = "note.txt"
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	runOne, service, configuration := runApprovedUpdateTask(t, workspace, stateDir, path, "v1", "v2")
	runTwo, _, _ := runApprovedUpdateTask(t, workspace, stateDir, path, "v2", "v3")
	if content, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(content) != "v3" {
		t.Fatalf("note.txt=%q err=%v", content, err)
	}
	_, err := service.Rewind(context.Background(), RewindOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Runs: []domain.RunID{runOne, runTwo}})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("first reversal must pause: %v", err)
	}
	for round := 0; round < 2; round++ {
		finished, resumeErr := service.Resume(context.Background(), ResumeOptions{RunID: waiting.RunID, Workspace: workspace, StateDir: stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: true, Reason: "test"}})
		if resumeErr == nil {
			if finished.Result == nil || finished.Result.State != string(domain.RunCompleted) {
				t.Fatalf("round %d finished=%#v", round, finished.Result)
			}
			break
		}
		if !errors.As(resumeErr, &waiting) {
			t.Fatalf("round %d unexpected error: %v", round, resumeErr)
		}
	}
	if content, err := os.ReadFile(filepath.Join(workspace, path)); err != nil || string(content) != "v1" {
		t.Fatalf("note.txt=%q err=%v（应恢复到链前 v1）", content, err)
	}
}

// TestReadRunArtifactVerifiesContent 锁定只读制品解引用：内容与引用一致可
// 读回，篡改摘要失败关闭。
func TestReadRunArtifactVerifiesContent(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	const path = "note.txt"
	if err := os.WriteFile(filepath.Join(workspace, path), []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	runID, service, configuration := runApprovedUpdateTask(t, workspace, stateDir, path, "original", "updated")
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	readOnly, err := openReadOnlyJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	events, err := readOnly.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	var reference *artifact.Ref
	for _, event := range events {
		if event.Type != domain.EventOperationStarted {
			continue
		}
		var payload struct {
			PlannedBeforeRef *artifact.Ref `json:"planned_before_ref"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.PlannedBeforeRef != nil {
			reference = payload.PlannedBeforeRef
			break
		}
	}
	if reference == nil {
		t.Fatal("run must carry a planned_before_ref for the update")
	}
	content, err := service.ReadRunArtifact(workspace, stateDir, configuration, *reference)
	if err != nil || string(content) != "original" {
		t.Fatalf("artifact=%q err=%v", content, err)
	}
	tampered := *reference
	tampered.SHA256 = "sha256:" + strings.Repeat("0", 64)
	if _, err := service.ReadRunArtifact(workspace, stateDir, configuration, tampered); err == nil {
		t.Fatal("tampered digest must fail closed")
	}
}
