package tui

import (
	"context"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"testing"
)

func TestReviewQueuePersistenceRestoresOnlyUnwrittenSuffix(t *testing.T) {
	input := RunConsoleInput{Workspace: t.TempDir(), StateDir: t.TempDir(), Config: config.Defaults(), Service: app.Service{}}
	console := NewRunConsole(context.Background(), input)
	defer console.rootCancel()
	console.mode = consoleDone
	console.queue = []string{"persisted", "", "remaining"}
	model, command := console.Update(tea.KeyMsg{Type: tea.KeyCtrlQ})
	if command == nil {
		t.Fatal("missing persistence command")
	}
	after := model.(RunConsole)
	// 模拟转存期间到达的新输入，失败后的旧后缀必须排在其前面。
	after.queue = append(after.queue, "new")
	model, _ = after.Update(command())
	after = model.(RunConsole)
	if len(after.queue) != 3 || after.queue[0] != "" || after.queue[1] != "remaining" || after.queue[2] != "new" {
		t.Fatalf("unwritten input lost or duplicated: %q", after.queue)
	}
	items, err := input.Service.ListQueue(context.Background(), input.Workspace, input.StateDir, input.Config)
	if err != nil || len(items) != 1 {
		t.Fatalf("already persisted prefix should be unique: %+v %v", items, err)
	}
}
