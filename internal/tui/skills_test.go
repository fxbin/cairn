package tui

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// TestSkillSlashTaskReachesAgent 技能点名任务不能被界面当成未知命令吞掉。
func TestSkillSlashTaskReachesAgent(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	task := "/skill:review-code 检查当前修改"
	console.taskField.SetValue(task)
	if strings.Contains(console.View(), "没有匹配的命令") {
		t.Fatal("skill invocation shown as unknown command")
	}
	if handled, cmd := console.runCommand(task); handled || cmd != nil {
		t.Fatal("skill task intercepted")
	}
	model, cmd := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	next := model.(RunConsole)
	if next.mode != consoleRunning || next.lastTask != task || cmd == nil {
		t.Fatalf("skill task was not submitted: %#v", next.lastTask)
	}
}
