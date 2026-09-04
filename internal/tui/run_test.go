// run_test 覆盖单任务运行主界面的纯函数与状态机：事件摘要投影、运行统
// 计累计、滚动缓冲上限、审批模式转换、终态判定与回复预览。真实运行与恢
// 复语义属 app.Service 既有测试范围，此处用状态机冒烟锁定界面编排。
//
// Author: fxbin
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
)

// eventWithPayload 构造带 JSON payload 的测试事件。
func eventWithPayload(eventType domain.EventType, payload string) domain.Event {
	event := domain.Event{Type: eventType}
	if payload != "" {
		event.Payload = json.RawMessage(payload)
	}
	return event
}

// TestSummarizeEventKeyLines 锁定关键事件的展示摘要与静默事件。
func TestSummarizeEventKeyLines(t *testing.T) {
	cases := []struct {
		event    domain.Event
		contains string
	}{
		{eventWithPayload(domain.EventTurnOpened, `{"turn_index":3}`), "第 3 轮"},
		{eventWithPayload(domain.EventToolCallProposed, `{"tool_name":"read_file"}`), "read_file"},
		{eventWithPayload(domain.EventVerificationFinished, `{"ok":true}`), "验证通过"},
		{eventWithPayload(domain.EventVerificationFinished, `{"ok":false}`), "验证未通过"},
		{eventWithPayload(domain.EventRunFailed, `{"reason":"budget_turns"}`), "budget_turns"},
		{eventWithPayload(domain.EventModelResponseFinished, `{"stop_reason":"stop","usage":{"prompt_tokens":100,"completion_tokens":20}}`), "in 100 / out 20"},
	}
	for _, testCase := range cases {
		line := summarizeEvent(testCase.event)
		if line == "" || !strings.Contains(line, testCase.contains) {
			t.Fatalf("事件 %s 摘要应包含 %q，得到 %q", testCase.event.Type, testCase.contains, line)
		}
	}
	if line := summarizeEvent(eventWithPayload(domain.EventOperationStarted, `{}`)); line != "" {
		t.Fatalf("operation_started 应静默，得到 %q", line)
	}
}

// TestApplyEventAccumulatesStats 锁定轮次、工具与 token 统计累计。
func TestApplyEventAccumulatesStats(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	console.applyEvent(eventWithPayload(domain.EventTurnOpened, `{"turn_index":1}`))
	console.applyEvent(eventWithPayload(domain.EventTurnOpened, `{"turn_index":2}`))
	console.applyEvent(eventWithPayload(domain.EventToolCallProposed, `{"tool_name":"read_file"}`))
	console.applyEvent(eventWithPayload(domain.EventModelResponseFinished, `{"usage":{"prompt_tokens":100,"completion_tokens":20}}`))
	console.applyEvent(eventWithPayload(domain.EventModelResponseFinished, `{"usage":{"prompt_tokens":50,"completion_tokens":10}}`))
	if console.turn != 2 || console.toolCalls != 1 {
		t.Fatalf("统计不符: turn=%d toolCalls=%d", console.turn, console.toolCalls)
	}
	if console.promptTokens != 150 || console.outputTokens != 30 {
		t.Fatalf("token 统计不符: in=%d out=%d", console.promptTokens, console.outputTokens)
	}
}

// TestAppendLineCapsBuffer 锁定滚动缓冲上限淘汰最旧行。
func TestAppendLineCapsBuffer(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	for index := 0; index < maxConsoleLines+10; index++ {
		console.appendLine(string(rune('a' + index%26)))
	}
	if len(console.lines) != maxConsoleLines {
		t.Fatalf("缓冲应保持 %d 行，得到 %d", maxConsoleLines, len(console.lines))
	}
}

// TestReceiveResultApprovalTransition 锁定等待审批错误的模式转换与提示。
func TestReceiveResultApprovalTransition(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	console.mode = consoleRunning
	waiting := &agent.WaitingApprovalError{RunID: "run-x", Fingerprint: "sha256:abc", Summary: "写入 main.go"}
	model, _ := console.receiveResult(runFinishedMsg{err: waiting})
	console = model.(RunConsole)
	if console.mode != consoleApproval || console.waiting != waiting {
		t.Fatalf("等待审批应转入审批模式，得到 mode=%d", console.mode)
	}
}

// TestReceiveResultDoneTransition 锁定终态转换与通道收束。
func TestReceiveResultDoneTransition(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	console.mode = consoleRunning
	model, _ := console.receiveResult(runFinishedMsg{result: &app.RunResult{Response: "done", Result: nil}})
	console = model.(RunConsole)
	if console.mode != consoleDone || !console.stopWait {
		t.Fatalf("运行结束应进入终态并停止等待，得到 mode=%d stopWait=%v", console.mode, console.stopWait)
	}
}

// TestRunConsoleRestartAfterDone 锁定终态回车回到输入态可发起新任务。
func TestRunConsoleRestartAfterDone(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	console.mode = consoleDone
	console.lines = append(console.lines, "旧运行")
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleInput {
		t.Fatalf("终态回车应回到输入态，得到 %d", console.mode)
	}
	console.taskField.SetValue("下一个任务")
	model, cmd := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleRunning || cmd == nil {
		t.Fatalf("输入态回车应发起运行，得到 mode=%d cmd=%v", console.mode, cmd)
	}
	if len(console.lines) != 1 || strings.Contains(console.lines[0], "旧运行") {
		t.Fatalf("新任务应重置运行展示，得到 %v", console.lines)
	}
}

// TestVerdictLineCarriesOutcome 锁定终态判定行携带结果分类。
func TestVerdictLineCarriesOutcome(t *testing.T) {
	failed := RunConsole{runErr: errors.New("budget exhausted")}
	if line := failed.verdictLine(); !strings.Contains(line, "budget exhausted") {
		t.Fatalf("失败判定应携带错误: %q", line)
	}
}

// TestFinalResponsePreviewTruncates 锁定终态回复预览按字符截断且不切断多
// 字节字符。
func TestFinalResponsePreviewTruncates(t *testing.T) {
	long := strings.Repeat("字", 200)
	preview := finalResponsePreview(long + "\n第二行")
	if runes := []rune(preview); len(runes) > 130 {
		t.Fatalf("预览应按字符截断: %d 字符", len(runes))
	}
	if strings.Contains(preview, "第二行") {
		t.Fatalf("预览应只含首行: %q", preview)
	}
	for _, character := range preview {
		if character == 0xFFFD {
			t.Fatal("预览含替换符（多字节字符被字节截断）")
		}
	}
}

// TestQueueInputSurvivesRunBoundary 锁定跨运行输入队列：运行中输入入队、
// 终态回车预填队首、Tab 丢弃队首——运行中的用户输入永不丢失。
func TestQueueInputSurvivesRunBoundary(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	console.mode = consoleRunning
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'继'}})
	console = model.(RunConsole)
	if !console.queuing {
		t.Fatal("运行中输入应进入排队输入态")
	}
	console.queueField.SetValue("继续上一步")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if len(console.queue) != 1 || console.queue[0] != "继续上一步" {
		t.Fatalf("回车应入队，得到 %v", console.queue)
	}
	if console.queueField.Value() != "" {
		t.Fatal("入队后排队框应清空")
	}
	model, _ = console.Update(runFinishedMsg{result: &app.RunResult{}})
	console = model.(RunConsole)
	if console.mode != consoleDone {
		t.Fatal("运行结束应进入终态")
	}
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleInput || console.taskField.Value() != "继续上一步" {
		t.Fatalf("终态回车应预填队首任务，得到 mode=%d value=%q", console.mode, console.taskField.Value())
	}
	if len(console.queue) != 0 {
		t.Fatalf("预填后队列应弹出，得到 %v", console.queue)
	}
}
