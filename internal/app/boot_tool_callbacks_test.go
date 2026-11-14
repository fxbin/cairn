package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/runtime/memory"
	"github.com/fxbin/cairn/internal/runtime/tools"
)

// callbackFixture 为每个工作区创建可验证记忆来源，避免只用空账本掩盖串区。
type callbackFixture struct {
	workspace string
	stateDir  string
	cfg       config.Config
	candidate memory.Candidate
}

// newCallbackFixture 创建终态来源及其记忆，保留真实 Journal 与制品验证路径。
func newCallbackFixture(t *testing.T) callbackFixture {
	t.Helper()
	f := callbackFixture{workspace: t.TempDir(), stateDir: t.TempDir(), cfg: withTestProvider(config.Defaults())}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	run, err := service.Run(context.Background(), RunOptions{Task: "callback fixture", Workspace: f.workspace, StateDir: f.stateDir, Config: f.cfg})
	if err != nil {
		t.Fatal(err)
	}
	f.candidate, err = service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: f.workspace, StateDir: f.stateDir, Config: f.cfg})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// appendCallbackTarget 创建独立且可回放的运行中消息目标；不修改记忆来源。
func appendCallbackTarget(t *testing.T, boot *Boot, stateDir string, id domain.RunID) {
	t.Helper()
	configRef := putTestFrozenRunConfig(t, stateDir, boot.WorkspaceHash, boot.Config)
	for index, draft := range []domain.EventDraft{
		{Type: domain.EventRunCreated, Payload: contract.MustEncodeRedactedJSON(struct {
			Task              string       `json:"task"`
			Workspace         string       `json:"workspace"`
			ConfigRef         contract.Ref `json:"config_ref"`
			ConfigFingerprint string       `json:"config_fingerprint"`
		}{Task: "message target", Workspace: boot.Workspace.Root, ConfigRef: configRef, ConfigFingerprint: boot.Config.Fingerprint()})},
		{Type: domain.EventRunStarted},
	} {
		draft.RunID = id
		draft.WorkspaceHash = boot.WorkspaceHash
		if _, err := boot.Journal.Append(context.Background(), uint64(index), draft); err != nil {
			t.Fatal(err)
		}
	}
}

// assertCallbackRecall 验证账本只包含当前工作区来源，模型额外字段不能改变范围。
func assertCallbackRecall(registry *tools.Registry, f callbackFixture, hash string) error {
	result := registry.Execute(context.Background(), "memory_recall", json.RawMessage(`{"outcomes":["completed"],"workspace_hash":"other-workspace"}`))
	if !result.OK {
		return fmt.Errorf("recall failed: %+v", result)
	}
	var ledger memory.EvidenceLedger
	if err := json.Unmarshal(result.Data, &ledger); err != nil {
		return err
	}
	if ledger.Plan.NormalizedRequest.WorkspaceHash != hash || len(ledger.Entries) != 1 || ledger.Entries[0].CandidateID != f.candidate.ID {
		return fmt.Errorf("recall crossed workspace: %+v", ledger)
	}
	return nil
}

// assertCallbackMessages 检查消息只出现在本 Journal，且保持哈希链和可回放性。
func assertCallbackMessages(t *testing.T, boot *Boot, id domain.RunID, marker string, count int) {
	t.Helper()
	events, err := boot.Journal.Load(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := domain.Replay(events); err != nil {
		t.Fatal(err)
	}
	if len(events) != count+2 {
		t.Fatalf("events=%d want=%d", len(events), count+2)
	}
	for _, event := range events[2:] {
		var payload struct {
			ContentRef contract.Ref `json:"content_ref"`
		}
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		content, err := boot.Artifacts.Open(payload.ContentRef)
		if err != nil {
			t.Fatal(err)
		}
		if event.WorkspaceHash != boot.WorkspaceHash || string(content) != marker {
			t.Fatalf("crossed journal: %+v", event)
		}
	}
}

// TestBootCallbacksIsolateConcurrentWorkspaces 锁定同进程并发、多工作区、相同目标标识的资源隔离。
func TestBootCallbacksIsolateConcurrentWorkspaces(t *testing.T) {
	fixtures := []callbackFixture{newCallbackFixture(t), newCallbackFixture(t)}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	boots := make([]*Boot, len(fixtures))
	const target domain.RunID = "run_shared_callback_target"
	for i, f := range fixtures {
		var err error
		boots[i], err = service.assembleBoot(f.workspace, f.stateDir, f.cfg, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer boots[i].Close()
		appendCallbackTarget(t, boots[i], f.stateDir, target)
		frozen, err := FrozenToolRegistryFingerprint(f.cfg.Preset)
		if err != nil {
			t.Fatal(err)
		}
		actual, err := boots[i].Registry.Fingerprint()
		if err != nil || actual != frozen {
			t.Fatalf("registry identity changed: %v", err)
		}
	}
	if result := (tools.MemoryRecall{}).Execute(context.Background(), json.RawMessage(`{}`)); result.OK || result.Code != "memory_recall_unavailable" {
		t.Fatalf("bare recall inherited boot: %+v", result)
	}
	if result := (tools.SendMessage{}).Execute(context.Background(), json.RawMessage(`{"to_run_id":"x","content":"x"}`)); result.OK || result.Code != "not_wired" {
		t.Fatalf("bare message inherited boot: %+v", result)
	}
	var wg sync.WaitGroup
	failures := make(chan error, len(boots))
	const iterations = 4
	for i, boot := range boots {
		wg.Add(1)
		go func(i int, boot *Boot) {
			defer wg.Done()
			for n := 0; n < iterations; n++ {
				if err := assertCallbackRecall(boot.Registry, fixtures[i], boot.WorkspaceHash); err != nil {
					failures <- err
					return
				}
				raw := json.RawMessage(fmt.Sprintf(`{"to_run_id":%q,"content":%q}`, target, fmt.Sprint(i)))
				if result := boot.Registry.Execute(context.Background(), "send_message", raw); !result.OK {
					failures <- fmt.Errorf("message: %+v", result)
					return
				}
			}
		}(i, boot)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	for i, boot := range boots {
		assertCallbackMessages(t, boot, target, fmt.Sprint(i), iterations)
	}
}

// TestBootCallbacksKeepParentAfterChildClose 验证子 Boot 关闭后父工具仍只使用父资源。
func TestBootCallbacksKeepParentAfterChildClose(t *testing.T) {
	f := newCallbackFixture(t)
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	parent, err := service.assembleBoot(f.workspace, f.stateDir, f.cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	const target domain.RunID = "run_parent_callback_target"
	appendCallbackTarget(t, parent, f.stateDir, target)
	child, err := service.assembleBootWithLock(f.workspace, f.stateDir, f.cfg, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertCallbackRecall(child.Registry, f, child.WorkspaceHash); err != nil {
		t.Fatal(err)
	}
	if err := child.Close(); err != nil {
		t.Fatal(err)
	}
	if err := assertCallbackRecall(parent.Registry, f, parent.WorkspaceHash); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(fmt.Sprintf(`{"to_run_id":%q,"content":"parent"}`, target))
	if result := parent.Registry.Execute(context.Background(), "send_message", raw); !result.OK {
		t.Fatalf("parent used closed child journal: %+v", result)
	}
	if result := child.Registry.Execute(context.Background(), "send_message", raw); result.OK {
		t.Fatal("closed child journal unexpectedly remained usable")
	}
	assertCallbackMessages(t, parent, target, "parent", 1)
}

// callbackRecoveryProvider 在冷恢复后依次调用召回、投递并检查真实工具回执。
type callbackRecoveryProvider struct {
	next        int
	target      domain.RunID
	candidateID string
	hash        string
}

// Stream 检查每个工具回执；召回结果必须来自恢复入口自己的 Boot。
func (provider *callbackRecoveryProvider) Stream(_ context.Context, request ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	for _, message := range request.Messages {
		if message.Role != "tool" {
			continue
		}
		var result contract.ToolResult
		if err := json.Unmarshal([]byte(message.Content), &result); err != nil {
			return err
		}
		if !result.OK && result.Code != "reconciled_not_applied" {
			return fmt.Errorf("tool failed after recovery: %s", message.Content)
		}
		if strings.Contains(string(result.Data), `"normalized_request"`) {
			var ledger memory.EvidenceLedger
			if err := json.Unmarshal(result.Data, &ledger); err != nil {
				return err
			}
			if ledger.Plan.NormalizedRequest.WorkspaceHash != provider.hash || len(ledger.Entries) != 1 || ledger.Entries[0].CandidateID != provider.candidateID {
				return errors.New("recovery recalled another workspace")
			}
		}
	}
	calls := []ports.ToolCall{
		{ID: "recall", Name: "memory_recall", Arguments: json.RawMessage(`{"outcomes":["completed"]}`)},
		{ID: "message", Name: "send_message", Arguments: json.RawMessage(fmt.Sprintf(`{"to_run_id":%q,"content":"recovery"}`, provider.target))},
	}
	if provider.next < len(calls) {
		call := calls[provider.next]
		provider.next++
		if err := emit(ports.ModelEvent{Kind: ports.ToolCallEvent, ToolCall: &call}); err != nil {
			return err
		}
	} else if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "callbacks recovered"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// TestBootCallbacksColdResumeAndReconcile 覆盖审批冷恢复和悬置写操作对账后继续执行；不重试原副作用。
func TestBootCallbacksColdResumeAndReconcile(t *testing.T) {
	for _, mode := range []string{"resume", "reconcile"} {
		t.Run(mode, func(t *testing.T) {
			f := newCallbackFixture(t)
			const target domain.RunID = "run_recovery_callback_target"
			initialService := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &patchThenFinalProvider{}, nil }}
			run, err := initialService.Run(context.Background(), RunOptions{Task: "pause before callbacks", Workspace: f.workspace, StateDir: f.stateDir, Config: f.cfg})
			var waiting *ports.WaitingApprovalError
			if !errors.As(err, &waiting) {
				t.Fatalf("write bypassed approval: %v", err)
			}
			boot, err := (Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}).assembleBoot(f.workspace, f.stateDir, f.cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			appendCallbackTarget(t, boot, f.stateDir, target)
			events, err := boot.Journal.Load(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			projection, err := domain.Replay(events)
			if err != nil {
				t.Fatal(err)
			}
			operation := projection.PendingApproval.OperationID
			if mode == "reconcile" {
				seq := projection.LastSeq
				for _, draft := range []domain.EventDraft{
					{Type: domain.EventApprovalGranted, Payload: json.RawMessage(fmt.Sprintf(`{"fingerprint":%q,"actor":"user"}`, waiting.Fingerprint))},
					{Type: domain.EventPolicyDecided, Payload: json.RawMessage(fmt.Sprintf(`{"effect":"allow","fingerprint":%q}`, waiting.Fingerprint))},
					{Type: domain.EventOperationStarted, Payload: json.RawMessage(`{"tool_name":"apply_patch","side_effect_class":"workspace"}`)},
				} {
					draft.RunID = run.ID
					draft.WorkspaceHash = boot.WorkspaceHash
					draft.TurnID = projection.CurrentTurn
					draft.OperationID = &operation
					if _, err := boot.Journal.Append(context.Background(), seq, draft); err != nil {
						t.Fatal(err)
					}
					seq++
				}
			}
			hash := boot.WorkspaceHash
			if err := boot.Close(); err != nil {
				t.Fatal(err)
			}
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
				return &callbackRecoveryProvider{target: target, candidateID: f.candidate.ID, hash: hash}, nil
			}}
			var result RunResult
			if mode == "resume" {
				result, err = service.Resume(context.Background(), ResumeOptions{RunID: run.ID, Workspace: f.workspace, StateDir: f.stateDir, Config: f.cfg, Approval: ports.Approval{Approve: true, Fingerprint: waiting.Fingerprint}})
			} else {
				result, err = service.Reconcile(context.Background(), ReconcileOptions{RunID: run.ID, OperationID: operation, Verdict: "not_applied", Note: "before write", Workspace: f.workspace, StateDir: f.stateDir, Config: f.cfg})
			}
			if err != nil || result.Result == nil || result.Result.State != string(domain.RunCompleted) || result.Response != "callbacks recovered" {
				t.Fatalf("recovery=%+v err=%v", result, err)
			}
			_, statErr := os.Stat(filepath.Join(f.workspace, "created.txt"))
			if mode == "reconcile" && !os.IsNotExist(statErr) {
				t.Fatalf("reconciliation retried write: %v", statErr)
			}
			if mode == "resume" && statErr != nil {
				t.Fatalf("approved write absent: %v", statErr)
			}
			inspected, err := service.Inspect(context.Background(), f.workspace, f.stateDir, f.cfg, run.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			patchStarts := 0
			for _, event := range inspected.Events {
				if event.Type == domain.EventOperationStarted && strings.Contains(string(event.Payload), "apply_patch") {
					patchStarts++
				}
			}
			if patchStarts != 1 {
				t.Fatalf("original write started %d times", patchStarts)
			}
			checkBoot, err := service.assembleBoot(f.workspace, f.stateDir, f.cfg, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer checkBoot.Close()
			assertCallbackMessages(t, checkBoot, target, "recovery", 1)
		})
	}
}
