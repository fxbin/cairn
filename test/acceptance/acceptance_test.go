//go:build acceptance

// 本文件是发布前验收：Docker 取消清理、审批/拒绝、取消。
// 默认构建不包含；用 go test -tags=acceptance ./test/acceptance 执行。
//
// Author: fxbin
package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/adapters/journal/memory"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// scriptedProvider 按脚本返回模型事件。
type scriptedProvider struct {
	scripts [][]ports.ModelEvent
	calls   int
}

func (provider *scriptedProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if provider.calls >= len(provider.scripts) {
		return emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"})
	}
	events := provider.scripts[provider.calls]
	provider.calls++
	for _, event := range events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

// hangingProvider 阻塞到 ctx 取消，模拟模型请求中的用户取消。
type hangingProvider struct{}

func (provider *hangingProvider) Stream(ctx context.Context, _ ports.ModelRequest, _ func(ports.ModelEvent) error) error {
	<-ctx.Done()
	return ctx.Err()
}

func startRun(t *testing.T, journal ports.Journal) domain.RunID {
	t.Helper()
	runID := domain.NewRunID()
	if _, err := journal.Append(context.Background(), 0, domain.EventDraft{
		RunID: runID, WorkspaceHash: "ws", Type: domain.EventRunCreated,
		Payload: json.RawMessage(`{"task":"acceptance","workspace":"/ws"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.Append(context.Background(), 1, domain.EventDraft{
		RunID: runID, WorkspaceHash: "ws", Type: domain.EventRunStarted,
	}); err != nil {
		t.Fatal(err)
	}
	return runID
}

func eventTypes(events []domain.Event) []domain.EventType {
	out := make([]domain.EventType, 0, len(events))
	for _, event := range events {
		out = append(out, event.Type)
	}
	return out
}

func hasEvent(events []domain.Event, want domain.EventType) bool {
	for _, event := range events {
		if event.Type == want {
			return true
		}
	}
	return false
}

// TestDockerCancellationCleanupLeavesNoContainer 验收取消后不留容器。
func TestDockerCancellationCleanupLeavesNoContainer(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	image := "cairn-agentscope-agent:test"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		t.Skip("acceptance docker image missing")
	}
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 4096, 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	tool := tools.ExecCommand{
		Workspace: workspace, Backend: tools.ExecBackendDocker,
		DockerImage: image, DockerMaxPIDs: 64,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw := json.RawMessage(`{"argv":["sleep","60"],"timeout_ms":120000}`)
	done := make(chan tools.Result, 1)
	go func() { done <- tool.Execute(ctx, raw) }()
	select {
	case result := <-done:
		if result.OK {
			t.Fatalf("cancelled docker exec unexpectedly succeeded: %#v", result)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("docker exec did not return after cancel")
	}
	list := exec.Command("docker", "container", "ls", "--all", "--quiet")
	list.Env = []string{}
	out, err := list.Output()
	if err != nil {
		t.Fatal(err)
	}
	// 不使用名称过滤：只确认本工具已走清理；若仍有 sleep 60 容器则失败
	ps := exec.Command("docker", "ps", "--all", "--format", "{{.Command}}")
	ps.Env = []string{}
	containers, _ := ps.Output()
	for _, line := range strings.Split(string(containers), "\n") {
		if strings.Contains(line, "sleep 60") {
			t.Fatalf("leftover sleep container: %s", line)
		}
	}
	_ = out
}

// TestApprovalThenResumeCompletes 验收审批通过后恢复完成。
func TestApprovalThenResumeCompletes(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	registry, err := tools.NewRegistry(patchTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{
			{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "c1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","mode":"create","content":"hello"}`)}},
			{Kind: ports.Done},
		},
		{{Kind: ports.TextDelta, Text: "approved complete"}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := startRun(t, journal)
	loop := agent.Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "accept-model", MaxTurns: 6}
	_, err = loop.Run(context.Background(), agent.RunInput{RunID: runID, Task: "approval", WorkspaceHash: "ws"})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("want WaitingApprovalError, got %v", err)
	}
	if waiting.Fingerprint == "" {
		t.Fatal("approval fingerprint required")
	}
	if _, err := loop.Resume(context.Background(), agent.RunInput{RunID: runID, Task: "approval", WorkspaceHash: "ws"}, ports.Approval{
		Fingerprint: waiting.Fingerprint, Approve: true, Reason: "cli accept",
	}); err != nil {
		t.Fatalf("resume after approve: %v", err)
	}
	events, err := journal.Load(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if !hasEvent(events, domain.EventApprovalGranted) || !hasEvent(events, domain.EventRunCompleted) {
		t.Fatalf("events=%v", eventTypes(events))
	}
	if _, err := os.Stat(root + "/a.txt"); err != nil {
		t.Fatalf("approved write missing: %v", err)
	}
}

// TestDenyLeavesNoFile 验收拒绝后无写入。
func TestDenyLeavesNoFile(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	registry, err := tools.NewRegistry(patchTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("ask", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	provider := &scriptedProvider{scripts: [][]ports.ModelEvent{
		{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "c1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"b.txt","mode":"create","content":"x"}`)}}, {Kind: ports.Done}},
	}}
	journal := memory.New()
	runID := startRun(t, journal)
	loop := agent.Loop{Journal: journal, Provider: provider, Tools: registry, Gate: gate, Model: "accept-model", MaxTurns: 4}
	_, err = loop.Run(context.Background(), agent.RunInput{RunID: runID, Task: "deny", WorkspaceHash: "ws"})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("want waiting approval, got %v", err)
	}
	_, _ = loop.Resume(context.Background(), agent.RunInput{RunID: runID, Task: "deny", WorkspaceHash: "ws"}, ports.Approval{
		Fingerprint: waiting.Fingerprint, Approve: false, Reason: "cli deny",
	})
	events, _ := journal.Load(context.Background(), runID)
	if !hasEvent(events, domain.EventApprovalDenied) {
		t.Fatalf("want approval_denied, events=%v", eventTypes(events))
	}
	if _, err := os.Stat(root + "/b.txt"); !os.IsNotExist(err) {
		t.Fatalf("denied write must not create file: %v", err)
	}
}

// TestCancelPersistsCancellation 验收取消可持久化。
func TestCancelPersistsCancellation(t *testing.T) {
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 1<<20, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	patchTool := tools.ApplyPatch{Workspace: workspace}
	registry, err := tools.NewRegistry(patchTool)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := policy.New("allow", "allow", nil, patchTool)
	if err != nil {
		t.Fatal(err)
	}
	journal := memory.New()
	runID := startRun(t, journal)
	loop := agent.Loop{Journal: journal, Provider: &hangingProvider{}, Tools: registry, Gate: gate, Model: "accept-model", MaxTurns: 4}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err = loop.Run(ctx, agent.RunInput{RunID: runID, Task: "cancel", WorkspaceHash: "ws"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	events, _ := journal.Load(context.Background(), runID)
	if !hasEvent(events, domain.EventRunCancelRequested) && !hasEvent(events, domain.EventRunCancelled) {
		t.Fatalf("want cancellation events, got %v", eventTypes(events))
	}
}
