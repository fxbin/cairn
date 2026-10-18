// session_fork_test 覆盖跨会话分支的会话树、来源校验与可持久恢复续链：A→B→C
// 后从 B 建 D，原链不变；重启后能分别续 C、D；损坏/跨工作区/未终结/循环
// 来源全部失败关闭。
//
// Author: fxbin
package app

import (
	"context"
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

// TestSessionForkBranchFromMiddleKeepsOriginalChain 锁定 A→B→C 后从 B 创建 D：
// 原链不变，D 来源可追踪，树上两条分支并存。
func TestSessionForkBranchFromMiddleKeepsOriginalChain(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	runA, err := service.Run(context.Background(), RunOptions{Task: "A", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: "session-chain"})
	if err != nil {
		t.Fatal(err)
	}
	runB, err := service.Run(context.Background(), RunOptions{Task: "B", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: "session-chain", PrevRunID: runA.ID})
	if err != nil {
		t.Fatal(err)
	}
	runC, err := service.Run(context.Background(), RunOptions{Task: "C", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: "session-chain", PrevRunID: runB.ID})
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if anchor.Source.RunID != runB.ID || anchor.SourceSessionID == "" {
		t.Fatalf("anchor = %#v", anchor)
	}
	runD, err := service.Run(context.Background(), RunOptions{
		Task: "D", Workspace: workspace, StateDir: stateDir, Config: configuration, ForkSource: &anchor.Source,
	})
	if err != nil {
		t.Fatal(err)
	}
	if runD.ID == runB.ID || runD.ID == runC.ID {
		t.Fatalf("fork created run %s", runD.ID)
	}
	items, err := service.ListRuns(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 4 {
		t.Fatalf("history count = %d, want 4", len(items))
	}
	byID := map[domain.RunID]RunHistoryItem{}
	for _, item := range items {
		byID[item.ID] = item
	}
	if byID[runC.ID].PrevRunID != runB.ID {
		t.Fatalf("run C prev = %q, want %q", byID[runC.ID].PrevRunID, runB.ID)
	}
	if byID[runB.ID].PrevRunID != runA.ID {
		t.Fatalf("run B prev = %q, want %q", byID[runB.ID].PrevRunID, runA.ID)
	}
	if byID[runD.ID].ForkSource == nil || byID[runD.ID].ForkSource.RunID != runB.ID {
		t.Fatalf("run D fork source = %#v", byID[runD.ID].ForkSource)
	}
	if byID[runD.ID].PrevRunID != "" {
		t.Fatalf("run D must not carry prev_run_id, got %q", byID[runD.ID].PrevRunID)
	}
	if byID[runD.ID].SessionID == byID[runA.ID].SessionID {
		t.Fatalf("fork must open a new session, got %q", byID[runD.ID].SessionID)
	}
	tree, err := service.ListSessionTree(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(tree.Roots) != 2 {
		t.Fatalf("tree roots = %d, want 2: %#v", len(tree.Roots), tree.Roots)
	}
	if tree.Nodes[runD.ID].ForkSource == nil || tree.Nodes[runD.ID].ForkSource.RunID != runB.ID {
		t.Fatalf("tree node D = %#v", tree.Nodes[runD.ID])
	}
	if len(tree.Nodes[runB.ID].Children) != 1 || tree.Nodes[runB.ID].Children[0] != runC.ID {
		t.Fatalf("tree B children = %#v", tree.Nodes[runB.ID].Children)
	}
}

// TestSessionForkRestartContinuesBothBranches 锁定关闭并重开日志后能分别从
// 原链尾 C 与分支尾 D 续链，且互不干扰。
func TestSessionForkRestartContinuesBothBranches(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	runA, err := service.Run(context.Background(), RunOptions{Task: "A", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: "session-restart"})
	if err != nil {
		t.Fatal(err)
	}
	sessionA := "session-restart"
	runB, err := service.Run(context.Background(), RunOptions{Task: "B", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: sessionA, PrevRunID: runA.ID})
	if err != nil {
		t.Fatal(err)
	}
	runC, err := service.Run(context.Background(), RunOptions{Task: "C", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: sessionA, PrevRunID: runB.ID})
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runB.ID)
	if err != nil {
		t.Fatal(err)
	}
	runD, err := service.Run(context.Background(), RunOptions{Task: "D", Workspace: workspace, StateDir: stateDir, Config: configuration, ForkSource: &anchor.Source})
	if err != nil {
		t.Fatal(err)
	}
	sessionD := sessionOfRun(t, service, workspace, stateDir, configuration, runD.ID)
	if sessionD == sessionA {
		t.Fatalf("fork reused session %q", sessionD)
	}

	// 模拟重启：服务只读重开日志，分别解析 C、D 的唯一链尾。
	continueC, err := service.ResolveSessionContinuation(context.Background(), workspace, stateDir, configuration, runC.ID)
	if err != nil {
		t.Fatal(err)
	}
	if continueC.SessionID != sessionA || continueC.PrevRunID != runC.ID {
		t.Fatalf("continue C = %#v", continueC)
	}
	continueD, err := service.ResolveSessionContinuation(context.Background(), workspace, stateDir, configuration, runD.ID)
	if err != nil {
		t.Fatal(err)
	}
	if continueD.SessionID != sessionD || continueD.PrevRunID != runD.ID {
		t.Fatalf("continue D = %#v", continueD)
	}
	runE, err := service.Run(context.Background(), RunOptions{Task: "E", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: continueC.SessionID, PrevRunID: continueC.PrevRunID, RequireSessionTail: true})
	if err != nil {
		t.Fatal(err)
	}
	runF, err := service.Run(context.Background(), RunOptions{Task: "F", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: continueD.SessionID, PrevRunID: continueD.PrevRunID, RequireSessionTail: true})
	if err != nil {
		t.Fatal(err)
	}
	items, err := service.ListRuns(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[domain.RunID]RunHistoryItem{}
	for _, item := range items {
		byID[item.ID] = item
	}
	if byID[runE.ID].SessionID != sessionA || byID[runE.ID].PrevRunID != runC.ID {
		t.Fatalf("run E = %#v", byID[runE.ID])
	}
	if byID[runF.ID].SessionID != sessionD || byID[runF.ID].PrevRunID != runD.ID {
		t.Fatalf("run F = %#v", byID[runF.ID])
	}
}

// TestSessionForkRejectsInvalidSources 锁定缺失、未终结、跨工作区、锚点不符
// 与复用来源会话全部失败关闭，不创建新 run。
func TestSessionForkRejectsInvalidSources(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	runA, err := service.Run(context.Background(), RunOptions{Task: "A", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: "session-invalid-src"})
	if err != nil {
		t.Fatal(err)
	}
	sessionA := "session-invalid-src"

	t.Run("缺失来源", func(t *testing.T) {
		_, err := service.Run(context.Background(), RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration, ForkSource: &domain.SessionForkSource{
			RunID: "run-missing", WorkspaceHash: mustWorkspaceHash(t, workspace, configuration), LastSeq: 1, LastEventHash: "sha256:none",
		}})
		if !errors.Is(err, ErrSessionContinuation) {
			t.Fatalf("err = %v, want ErrSessionContinuation", err)
		}
	})

	t.Run("锚点不符", func(t *testing.T) {
		_, err := service.Run(context.Background(), RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration, ForkSource: &domain.SessionForkSource{
			RunID: runA.ID, WorkspaceHash: mustWorkspaceHash(t, workspace, configuration), LastSeq: 99, LastEventHash: "sha256:stale",
		}})
		if !errors.Is(err, ErrSessionContinuation) {
			t.Fatalf("err = %v, want ErrSessionContinuation", err)
		}
	})

	t.Run("跨工作区", func(t *testing.T) {
		_, err := service.Run(context.Background(), RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration, ForkSource: &domain.SessionForkSource{
			RunID: runA.ID, WorkspaceHash: "sha256:foreign", LastSeq: 3, LastEventHash: "sha256:abc",
		}})
		if !errors.Is(err, ErrSessionContinuation) {
			t.Fatalf("err = %v, want ErrSessionContinuation", err)
		}
	})

	t.Run("复用来源会话", func(t *testing.T) {
		anchor, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runA.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Run(context.Background(), RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: sessionA, ForkSource: &anchor.Source})
		if !errors.Is(err, ErrSessionContinuation) {
			t.Fatalf("err = %v, want ErrSessionContinuation", err)
		}
	})

	t.Run("与 prev_run_id 互斥", func(t *testing.T) {
		anchor, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runA.ID)
		if err != nil {
			t.Fatal(err)
		}
		_, err = service.Run(context.Background(), RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration, PrevRunID: runA.ID, ForkSource: &anchor.Source})
		if !errors.Is(err, ErrSessionContinuation) {
			t.Fatalf("err = %v, want ErrSessionContinuation", err)
		}
	})

	t.Run("RequireSessionTail 缺前驱失败关闭", func(t *testing.T) {
		_, err := service.Run(context.Background(), RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration, RequireSessionTail: true})
		if !errors.Is(err, ErrSessionContinuation) {
			t.Fatalf("err = %v, want ErrSessionContinuation", err)
		}
	})

	// 未终结来源：手工写入 created-only run 后拒绝。
	t.Run("未终结来源", func(t *testing.T) {
		_, workspaceHash, err := workspaceFor(workspace, configuration)
		if err != nil {
			t.Fatal(err)
		}
		journal, err := openJournal(stateDir, workspaceHash)
		if err != nil {
			t.Fatal(err)
		}
		configRef := putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
		runningID := domain.NewRunID()
		if _, err := journal.Append(context.Background(), 0, domain.EventDraft{
			RunID: runningID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated,
			Payload: artifact.EncodeRedactedJSON(struct {
				domain.RunCreatedPayload
				ConfigRef artifact.Ref `json:"config_ref"`
			}{RunCreatedPayload: domain.RunCreatedPayload{Task: "running", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint(), SessionID: "session-live"}, ConfigRef: configRef}),
		}); err != nil {
			t.Fatal(err)
		}
		if err := journal.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runningID); !errors.Is(err, ErrSessionContinuation) {
			t.Fatalf("fork from non-terminal error = %v, want ErrSessionContinuation", err)
		}
	})
}

// TestSessionHistoryContinuationRejectsSameSessionFork 锁定同 session 分叉后
// 唯一链尾续链仍拒绝歧义，但显式 fork 到新 session 仍是出路。
func TestSessionHistoryContinuationRejectsSameSessionFork(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	runA, err := service.Run(context.Background(), RunOptions{Task: "A", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: "session-forked"})
	if err != nil {
		t.Fatal(err)
	}
	sessionA := "session-forked"
	runB, err := service.Run(context.Background(), RunOptions{Task: "B", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: sessionA, PrevRunID: runA.ID})
	if err != nil {
		t.Fatal(err)
	}
	// 同 session 内再从 A 分叉出 C（旧 rewind 语义）。
	runC, err := service.Run(context.Background(), RunOptions{Task: "C", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: sessionA, PrevRunID: runA.ID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveSessionContinuation(context.Background(), workspace, stateDir, configuration, runB.ID); !errors.Is(err, ErrSessionContinuation) {
		t.Fatalf("continuation on forked session error = %v, want ErrSessionContinuation", err)
	}
	// 显式从分叉后的终态节点开新 session 仍是出路。
	anchor, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runC.ID)
	if err != nil {
		t.Fatal(err)
	}
	runD, err := service.Run(context.Background(), RunOptions{Task: "D", Workspace: workspace, StateDir: stateDir, Config: configuration, ForkSource: &anchor.Source})
	if err != nil {
		t.Fatal(err)
	}
	items, err := service.ListRuns(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range items {
		if item.ID == runD.ID {
			found = true
			if item.ForkSource == nil || item.ForkSource.RunID != runC.ID {
				t.Fatalf("run D source = %#v", item.ForkSource)
			}
		}
	}
	if !found {
		t.Fatal("forked run missing from history")
	}
}

// TestSessionForkRejectsCorruptSourceArtifacts 锁定来源制品缺失或被篡改时
// 按状态损坏失败关闭，不创建分支。
func TestSessionForkRejectsCorruptSourceArtifacts(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	runA, err := service.Run(context.Background(), RunOptions{Task: "A", Workspace: workspace, StateDir: stateDir, Config: configuration, SessionID: "session-artifact"})
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, runA.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	var configRef *artifact.Ref
	for _, event := range inspected.Events {
		var payload struct {
			ConfigRef *artifact.Ref `json:"config_ref"`
		}
		if event.Type == domain.EventRunCreated && json.Unmarshal(event.Payload, &payload) == nil {
			configRef = payload.ConfigRef
			break
		}
	}
	if configRef == nil {
		t.Fatal("run_created missing config_ref")
	}
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.TrimPrefix(configRef.SHA256, "sha256:")
	artifactPath := filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts", "sha256", digest[:2], digest)
	if err := os.Remove(artifactPath); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runA.ID); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("fork from corrupt source error = %v, want ErrStateCorrupt", err)
	}
}

// TestSessionForkRejectsCyclicAncestry 锁定来源祖先图成环时按状态损坏失败
// 关闭，不进入分支创建。
func TestSessionForkRejectsCyclicAncestry(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, workspaceHash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := openJournal(stateDir, workspaceHash)
	if err != nil {
		t.Fatal(err)
	}
	configRef := putTestFrozenRunConfig(t, stateDir, workspaceHash, configuration)
	runX, runY := domain.NewRunID(), domain.NewRunID()
	appendCycleMember := func(runID domain.RunID, previous domain.RunID) {
		t.Helper()
		drafts := []domain.EventDraft{
			{
				RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunCreated,
				Payload: artifact.EncodeRedactedJSON(struct {
					domain.RunCreatedPayload
					ConfigRef artifact.Ref `json:"config_ref"`
				}{RunCreatedPayload: domain.RunCreatedPayload{
					Task: "cycle", Workspace: workspace, ConfigFingerprint: configuration.Fingerprint(),
					SessionID: "session-cycle", PrevRunID: previous,
				}, ConfigRef: configRef}),
			},
			{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunStarted},
			{RunID: runID, WorkspaceHash: workspaceHash, Type: domain.EventRunFailed, Payload: json.RawMessage(`{"error":"cycle fixture"}`)},
		}
		for index, draft := range drafts {
			if _, err := journal.Append(context.Background(), uint64(index), draft); err != nil {
				t.Fatal(err)
			}
		}
	}
	// 直接写入互相指认的前驱，绕过创建期校验，模拟损坏日志中的环。
	appendCycleMember(runX, runY)
	appendCycleMember(runY, runX)
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ResolveSessionFork(context.Background(), workspace, stateDir, configuration, runX); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("fork from cyclic ancestry error = %v, want ErrStateCorrupt", err)
	}
}

// sessionOfRun 读取运行投影的会话标识，供测试组装续链输入。
func sessionOfRun(t *testing.T, service Service, workspace, stateDir string, configuration config.Config, runID domain.RunID) string {
	t.Helper()
	items, err := service.ListRuns(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.ID == runID {
			if item.SessionID == "" {
				t.Fatalf("run %s has no session identity", runID)
			}
			return item.SessionID
		}
	}
	t.Fatalf("run %s not found", runID)
	return ""
}

// mustWorkspaceHash 解析当前工作区身份摘要。
func mustWorkspaceHash(t *testing.T, workspace string, configuration config.Config) string {
	t.Helper()
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}
