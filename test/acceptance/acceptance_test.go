//go:build acceptance

// 发布前验收：Docker 取消清理、审批/拒绝、取消链、TUI 审批接线。
// go test -tags=acceptance ./test/acceptance
//
// Author: fxbin
package acceptance

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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

func requireDocker(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("docker")
	if err != nil {
		t.Skip("docker not available")
	}
	image := "cairn-agentscope-agent:test"
	if err := exec.Command(path, "image", "inspect", image).Run(); err != nil {
		t.Skip("acceptance docker image missing")
	}
	return image
}

func dockerOutput(t *testing.T, args ...string) (string, error) {
	t.Helper()
	command := exec.Command("docker", args...)
	command.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin"}
	out, err := command.CombinedOutput()
	return string(out), err
}

// TestDockerCancellationCleanupLeavesNoContainer 验收取消后清理到 destroy，
// 且以本次容器身份为准；查询错误失败关闭。
func TestDockerCancellationCleanupLeavesNoContainer(t *testing.T) {
	image := requireDocker(t)
	root := t.TempDir()
	workspace, err := tools.NewWorkspace(root, nil, 4096, 64, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	before, err := dockerOutput(t, "ps", "--all", "--quiet", "--filter", "ancestor="+image)
	if err != nil {
		t.Fatalf("docker ps before: %v", err)
	}
	beforeIDs := map[string]struct{}{}
	for _, line := range strings.Split(strings.TrimSpace(before), "\n") {
		if line != "" {
			beforeIDs[line] = struct{}{}
		}
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

	// 等待新容器出现，再取消
	var newIDs []string
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		out, err := dockerOutput(t, "ps", "--all", "--quiet", "--filter", "ancestor="+image)
		if err != nil {
			t.Fatalf("docker ps during: %v", err)
		}
		newIDs = nil
		for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
			if line == "" {
				continue
			}
			if _, old := beforeIDs[line]; !old {
				newIDs = append(newIDs, line)
			}
		}
		if len(newIDs) > 0 {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if len(newIDs) == 0 {
		t.Fatal("docker container never appeared for this exec")
	}
	cancel()

	select {
	case result := <-done:
		if result.OK {
			t.Fatalf("cancelled exec must not succeed: %#v", result)
		}
		if result.Code != "cancelled" {
			t.Fatalf("want code=cancelled, got %q preview=%q", result.Code, result.Preview)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("exec did not return after cancel")
	}

	// 每个本次新 CID 必须已销毁
	for _, id := range newIDs {
		out, err := dockerOutput(t, "inspect", "--format", "{{.State.Status}}", id)
		if err != nil {
			if strings.Contains(out, "No such object") || strings.Contains(out, "no such") {
				continue
			}
			t.Fatalf("docker inspect %s: %v %s", id, err, out)
		}
		status := strings.TrimSpace(out)
		if status != "removed" && status != "" {
			if status == "running" || status == "created" || status == "restarting" {
				t.Fatalf("container %s still %s after cancel cleanup", id, status)
			}
			// destroyed 后 inspect 失败；若仍可查到非终态则失败
			if status != "exited" && status != "dead" {
				t.Fatalf("container %s unexpected state %s", id, status)
			}
		}
	}
	final, err := dockerOutput(t, "ps", "--all", "--quiet", "--filter", "ancestor="+image)
	if err != nil {
		t.Fatalf("docker ps after: %v", err)
	}
	for _, id := range newIDs {
		if strings.Contains(final, id) {
			t.Fatalf("container %s still listed after cleanup: %s", id, final)
		}
	}
}

func approvalLoop(t *testing.T) (*agent.Loop, ports.Journal, domain.RunID, *tools.ApplyPatch, string) {
	t.Helper()
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
	journal := memory.New()
	runID := startRun(t, journal)
	loop := &agent.Loop{
		Journal: journal, Provider: &scriptedProvider{scripts: [][]ports.ModelEvent{
			{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "c1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"a.txt","mode":"create","content":"hello"}`)}}, {Kind: ports.Done}},
			{{Kind: ports.TextDelta, Text: "approved complete"}, {Kind: ports.Done}},
		}},
		Tools: registry, Gate: gate, Model: "accept-model", MaxTurns: 6,
	}
	return loop, journal, runID, &patchTool, root
}

// TestApprovalThenResumeCompletes 验收批准后恢复完成且文件落地。
func TestApprovalThenResumeCompletes(t *testing.T) {
	loop, journal, runID, _, root := approvalLoop(t)
	_, err := loop.Run(context.Background(), agent.RunInput{RunID: runID, Task: "approval", WorkspaceHash: "ws"})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) || waiting.Fingerprint == "" {
		t.Fatalf("want WaitingApprovalError with fingerprint, got %v", err)
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
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Fatalf("approved write missing: %v", err)
	}
}

// TestDenyLeavesNoFile 验收拒绝后无写入且落 approval_denied。
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
	journal := memory.New()
	runID := startRun(t, journal)
	loop := agent.Loop{
		Journal: journal,
		Provider: &scriptedProvider{scripts: [][]ports.ModelEvent{
			{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "c1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"b.txt","mode":"create","content":"x"}`)}}, {Kind: ports.Done}},
		}},
		Tools: registry, Gate: gate, Model: "accept-model", MaxTurns: 4,
	}
	_, err = loop.Run(context.Background(), agent.RunInput{RunID: runID, Task: "deny", WorkspaceHash: "ws"})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("want waiting approval, got %v", err)
	}
	if _, err := loop.Resume(context.Background(), agent.RunInput{RunID: runID, Task: "deny", WorkspaceHash: "ws"}, ports.Approval{
		Fingerprint: waiting.Fingerprint, Approve: false, Reason: "cli deny",
	}); err != nil {
		var denied *ports.PolicyDeniedError
		if !errors.As(err, &denied) {
			t.Fatalf("resume deny must be PolicyDeniedError, got %v", err)
		}
	}
	events, _ := journal.Load(context.Background(), runID)
	if !hasEvent(events, domain.EventApprovalDenied) {
		t.Fatalf("want approval_denied, events=%v", eventTypes(events))
	}
	if _, err := os.Stat(filepath.Join(root, "b.txt")); !os.IsNotExist(err) {
		t.Fatalf("denied write must not create file: %v", err)
	}
}

// TestCancelPersistsFullCancellationChain 验收取消链完整：requested + cancelled + 终态。
func TestCancelPersistsFullCancellationChain(t *testing.T) {
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
	loop := agent.Loop{
		Journal: journal, Provider: &hangingProvider{}, Tools: registry, Gate: gate,
		Model: "accept-model", MaxTurns: 4,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err = loop.Run(ctx, agent.RunInput{RunID: runID, Task: "cancel", WorkspaceHash: "ws"})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	events, loadErr := journal.Load(context.Background(), runID)
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if !hasEvent(events, domain.EventRunCancelRequested) {
		t.Fatalf("want run_cancel_requested, events=%v", eventTypes(events))
	}
	if !hasEvent(events, domain.EventRunCancelled) {
		t.Fatalf("want run_cancelled, events=%v", eventTypes(events))
	}
	projection, err := domain.Replay(events)
	if err != nil {
		t.Fatal(err)
	}
	if projection.State != domain.RunCancelled {
		t.Fatalf("want cancelled terminal, got %s events=%v", projection.State, eventTypes(events))
	}
}
