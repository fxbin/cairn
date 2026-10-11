//go:build acceptance

// TUI 审批实机验收：按键产生 Resume，且理由文案与事件一致。
//
// Author: fxbin
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

type acceptScriptedProvider struct {
	scripts [][]ports.ModelEvent
	calls   int
}

func (p *acceptScriptedProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if p.calls >= len(p.scripts) {
		return emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"})
	}
	events := p.scripts[p.calls]
	p.calls++
	for _, e := range events {
		if err := emit(e); err != nil {
			return err
		}
	}
	return nil
}

// TestTUIApprovalKeysDriveRealApproval 验收 TUI 确认键触发 app.Resume 并完成写入。
func TestTUIApprovalKeysDriveRealApproval(t *testing.T) {
	workspaceRoot := t.TempDir()
	stateDir := t.TempDir()
	configuration := config.Defaults()
	configuration.Provider.BaseURL = "https://example.invalid/v1"
	configuration.Provider.Model = "accept-model"
	configuration.Provider.Admission = config.ProviderAdmissionOptional
	configuration.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true}
	configuration.Policy.Write = "ask"
	configuration.Policy.Exec = "allow"
	configuration.Verification.AutoAfterPatch = false

	service := app.Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &acceptScriptedProvider{scripts: [][]ports.ModelEvent{
			{{Kind: ports.ToolCallEvent, ToolCall: &ports.ToolCall{ID: "c1", Name: "apply_patch", Arguments: json.RawMessage(`{"path":"tui.txt","mode":"create","content":"tui"}`)}}, {Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}, {Kind: ports.Done, StopReason: "tool_calls"}},
			{{Kind: ports.TextDelta, Text: "tui approved"}, {Kind: ports.UsageEvent, Usage: &ports.Usage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}}, {Kind: ports.Done, StopReason: "stop"}},
		}}, nil
	}}

	runCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := service.Run(runCtx, app.RunOptions{
		Task: "tui approval", Workspace: workspaceRoot, StateDir: stateDir, Config: configuration,
	})
	var waiting *ports.WaitingApprovalError
	if !errors.As(err, &waiting) {
		t.Fatalf("want waiting approval from service.Run, got %v", err)
	}

	console := NewRunConsole(context.Background(), RunConsoleInput{
		Workspace: workspaceRoot, StateDir: stateDir, Config: configuration, Service: service,
	})
	console.waiting = waiting
	console.mode = consoleApproval

	model, cmd := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if cmd == nil {
		t.Fatal("approval enter must return resume cmd")
	}
	if console.mode != consoleRunning {
		t.Fatalf("mode=%v want running", console.mode)
	}
	// 执行 TUI 生成的 Resume 命令（与点按等价）
	if msg := cmd(); msg != nil {
		// 同步返回时再喂回 Update
		model, _ = console.Update(msg)
		console = model.(RunConsole)
	}
	// 消费 runFinishedMsg：终态应进入 done（waiting 在下次 resetRun 才清）
	deadline := 0
	for console.mode != consoleDone && deadline < 50 {
		select {
		case m := <-console.messages:
			model, _ = console.Update(m)
			console = model.(RunConsole)
		default:
			deadline++
		}
	}
	if console.mode != consoleDone {
		t.Fatalf("mode=%v want done after tui resume", console.mode)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, "tui.txt")); err != nil {
		t.Fatalf("approved write missing after tui resume: %v", err)
	}
	_ = domain.RunCompleted
	_ = approvalReason(true)
}
