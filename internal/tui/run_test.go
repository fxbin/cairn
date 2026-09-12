// run_test 覆盖单任务运行主界面的纯函数与状态机：事件摘要投影、运行统
// 计累计、滚动缓冲上限、审批模式转换、终态判定与回复预览。真实运行与恢
// 复语义属 app.Service 既有测试范围，此处用状态机冒烟锁定界面编排。
//
// Author: fxbin
package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/modelcatalog"
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

// TestApprovalProposalPreview 锁定审批参数预览：从事件尾部提取最近工具提
// 议，apply_patch 展示模式/路径/内容行，exec 展示 argv，内容截断带总数。
func TestApprovalProposalPreview(t *testing.T) {
	events := []domain.Event{
		{Type: domain.EventRunCreated},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"read_file","arguments":{"path":"a.go"}}`)},
		{Type: domain.EventModelResponseFinished},
		{Type: domain.EventToolCallProposed, Payload: json.RawMessage(`{"tool_name":"apply_patch","arguments":{"path":"main.go","mode":"update","expected_sha256":"abcdef1234567890","content":"line1\nline2\nline3"}}`)},
	}
	lines := approvalProposalPreview(events)
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"apply_patch update", "main.go", "line1", "line3"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("apply_patch 预览应包含 %q:\n%s", want, joined)
		}
	}

	long := strings.Repeat("row\n", 20)
	events[len(events)-1].Payload = json.RawMessage(`{"tool_name":"apply_patch","arguments":{"path":"x.go","mode":"create","expected_sha256":"s","content":"` + strings.ReplaceAll(long, "\n", `\n`) + `"}}`)
	lines = approvalProposalPreview(events)
	joined = strings.Join(lines, "\n")
	if !strings.Contains(joined, "共 21 行") {
		t.Fatalf("内容预览应标注总行数:\n%s", joined)
	}

	events[len(events)-1].Payload = json.RawMessage(`{"tool_name":"exec_command","arguments":{"argv":["go","test","./..."]}}`)
	lines = approvalProposalPreview(events)
	if !strings.Contains(strings.Join(lines, "\n"), "go test ./...") {
		t.Fatalf("exec 预览应展示 argv:\n%s", strings.Join(lines, "\n"))
	}
}

// TestTranscriptViewerToggleAndScroll 锁定事件查看器：无事件不进入、有事件
// Ctrl+O 进入并落到底部窗口、↑↓ 滚动、Esc 返回原模式。
func TestTranscriptViewerToggleAndScroll(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{})
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	console = model.(RunConsole)
	if console.mode != consoleInput {
		t.Fatal("无事件时 Ctrl+O 不应进入查看器")
	}
	console.mode = consoleRunning
	for index := 0; index < transcriptWindowHeight+5; index++ {
		console.applyEvent(eventWithPayload(domain.EventTurnOpened, `{"turn_index":1}`))
	}
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyCtrlO})
	console = model.(RunConsole)
	if console.mode != consoleTranscript || console.transcriptReturn != consoleRunning {
		t.Fatalf("运行中 Ctrl+O 应进入查看器并记住来源，得到 mode=%d", console.mode)
	}
	if console.transcriptCursor != 5 {
		t.Fatalf("进入时应落到底部窗口起点 5，得到 %d", console.transcriptCursor)
	}
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyUp})
	console = model.(RunConsole)
	if console.transcriptCursor != 4 {
		t.Fatalf("上移应滚动，得到 %d", console.transcriptCursor)
	}
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEsc})
	console = model.(RunConsole)
	if console.mode != consoleRunning {
		t.Fatal("Esc 应返回来源模式")
	}
}

// TestMaxTurnsBudgetFromResolution 锁定轮次水位读取 Resolved Plan。
func TestMaxTurnsBudgetFromResolution(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	if console.maxTurnsBudget() != 0 {
		t.Fatal("无 Resolution 时不应展示上限")
	}
	console.input.Config.Resolution = []config.ResolutionEntry{{Field: "budget.max_turns", Value: "48"}}
	if got := console.maxTurnsBudget(); got != 48 {
		t.Fatalf("应读取 resolved 轮次预算 48，得到 %d", got)
	}
}

// TestSlashCommands 锁定命令基线：help/new/status/cost 就地反馈、未知命令
// 提示、quit 请求退出、非斜杠输入不拦截。
func TestSlashCommands(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.input.Config.Provider.Model = "test-model"

	if handled, cmd := console.runCommand("普通任务"); handled || cmd != nil {
		t.Fatal("非斜杠输入不应按命令处理")
	}
	console.mode = consoleInput
	console.taskField.SetValue("/help")
	model, cmd := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleInput || len(console.lines) == 0 {
		t.Fatal("/help 应就地反馈且不进入运行")
	}
	if strings.Contains(strings.Join(console.lines, "\n"), "/new") != true {
		t.Fatal("/help 输出应包含命令清单")
	}

	console.taskField.SetValue("/status")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if !strings.Contains(strings.Join(console.lines, "\n"), "test-model") {
		t.Fatal("/status 应展示模型")
	}

	console.turn = 3
	console.promptTokens = 100
	console.taskField.SetValue("/cost")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if !strings.Contains(strings.Join(console.lines, "\n"), "in 100") {
		t.Fatal("/cost 应展示累计")
	}

	console.lines = nil
	console.taskField.SetValue("/new")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if len(console.lines) != 1 || console.turn != 0 || console.sessionPromptTokens != 100 {
		t.Fatalf("/new 应清屏并保留会话累计，lines=%d turn=%d session=%d", len(console.lines), console.turn, console.sessionPromptTokens)
	}

	console.taskField.SetValue("/quit")
	model, cmd = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if cmd == nil {
		t.Fatal("/quit 应请求退出")
	}
}

// TestModelCommandEmbedsWizard 锁定 /model 嵌入向导：进入向导模式、按键转
// 发、退出命令翻译为关闭消息并热重载配置。
func TestModelCommandEmbedsWizard(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.mode = consoleInput
	console.taskField.SetValue("/model")
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleModelWizard || console.wizard == nil {
		t.Fatal("/model 应进入嵌入的配置向导")
	}
	console.wizard.catalog = &modelcatalog.Catalog{Providers: []modelcatalog.ProviderInfo{
		{ID: "deepseek", Name: "DeepSeek", ModelCount: 3},
		{ID: "openai", Name: "OpenAI", ModelCount: 47},
	}}
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyDown})
	console = model.(RunConsole)
	if console.wizard.providerCursor != 1 {
		t.Fatalf("按键应转发给向导，得到 cursor=%d", console.wizard.providerCursor)
	}

	reloadCalled := false
	console.input.ReloadConfig = func() (config.Config, error) {
		reloadCalled = true
		return config.Config{}, nil
	}
	console.wizard.outcome = &app.SetupOutcome{ConfigPath: "x"}
	model, _ = console.Update(wizardClosedMsg{})
	console = model.(RunConsole)
	if console.mode != consoleInput || console.wizard != nil {
		t.Fatal("向导关闭消息应退出向导模式")
	}
	if !reloadCalled {
		t.Fatal("提交成功应触发热重载回调")
	}
}

// TestModelCommandStartsWizardInit 锁定嵌入向导的启动命令：/model 必须把向
// 导的 Init 命令（目录装载与指示启动）交给程序，否则目录永远停在装载中。
func TestModelCommandStartsWizardInit(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.mode = consoleInput
	console.taskField.SetValue("/model")
	model, cmd := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleModelWizard || cmd == nil {
		t.Fatal("/model 应返回向导的 Init 命令驱动目录装载")
	}
	if console.wizard.catalogLoading {
		_ = cmd
	}
}

// TestCommandSuggestionsAndCompletion 锁定命令候选与补全：斜杠前缀过滤候
// 选、唯一前缀补全完整命令、多匹配补全公共前缀、零匹配为空。
func TestCommandSuggestionsAndCompletion(t *testing.T) {
	if got := commandSuggestions("/mo"); len(got) != 1 || !strings.Contains(got[0], "/model") {
		t.Fatalf("/mo 应唯一命中 /model，得到 %v", got)
	}
	if got := commandSuggestions("/zzz"); len(got) != 0 {
		t.Fatalf("零匹配应为空，得到 %v", got)
	}
	if got := completeCommand("/st"); got != "/status" {
		t.Fatalf("/st 应补全到 /status，得到 %q", got)
	}
	if got := completeCommand("/s"); got != "" {
		t.Fatalf("/s 多匹配无更长公共前缀应不动，得到 %q", got)
	}
	if got := completeCommand("/c"); got != "/cost" {
		t.Fatalf("/c 应补全到 /cost，得到 %q", got)
	}
	if got := completeCommand("/"); got != "" {
		t.Fatalf("/ 多匹配无更长公共前缀应不动，得到 %q", got)
	}
	if got := completeCommand(""); got != "" {
		t.Fatal("空输入不应补全")
	}
	// 清单与分发一致性：每个登记命令都必须被分发器接受
	for _, command := range consoleCommands {
		console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
		if handled, _ := console.runCommand(command.name); !handled {
			t.Fatalf("清单命令 %s 未被分发器接受（清单与 switch 漂移）", command.name)
		}
	}
}

// TestReceiveResultLoadsApprovalDetail 锁定审批集成：进入审批模式必须加载
// 参数预览（此前曾在重构中丢失而纯函数测试无法发现）。
func TestReceiveResultLoadsApprovalDetail(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Defaults()})
	console.mode = consoleRunning
	model, _ := console.receiveResult(runFinishedMsg{err: &agent.WaitingApprovalError{RunID: "run-missing", Fingerprint: "f", Summary: "s"}})
	console = model.(RunConsole)
	if console.mode != consoleApproval {
		t.Fatal("应进入审批模式")
	}
	if len(console.approvalDetail) == 0 {
		t.Fatal("审批参数预览必须加载（不可用时也应有标注行）")
	}
}

// TestSessionChainCarriesPriorRun 锁定 TUI 会话模式：首个任务开链、终态记
// 链尾、后续任务携带会话与前运行标识、/new 断链。
func TestSessionChainCarriesPriorRun(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.mode = consoleInput
	console.taskField.SetValue("第一个任务")
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.sessionID == "" {
		t.Fatal("首个任务应生成会话标识开链")
	}

	console.mode = consoleRunning
	model, _ = console.receiveResult(runFinishedMsg{result: &app.RunResult{ID: "run-1", Response: "ok", Result: nil}})
	console = model.(RunConsole)
	if console.sessionPrev != "run-1" || len(console.sessionRuns) != 1 {
		t.Fatalf("终态应记录链尾，得到 prev=%q runs=%v", console.sessionPrev, console.sessionRuns)
	}

	console.mode = consoleInput
	console.taskField.SetValue("第二个任务")
	model, cmd := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleRunning || cmd == nil {
		t.Fatal("第二任务应正常发起")
	}
	if console.sessionPrev != "run-1" {
		t.Fatal("第二任务应携带前一运行标识")
	}
	if len(console.lines) == 0 || !containsLine(console.lines, "前情") {
		t.Fatalf("续链任务应提示携带前情: %v", console.lines)
	}

	console.mode = consoleInput
	console.taskField.SetValue("/new")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.sessionID != "" || console.sessionPrev != "" || console.sessionRuns != nil {
		t.Fatal("/new 应断开会话链")
	}
}

// containsLine 判断展示行集合是否含关键词。
func containsLine(lines []string, keyword string) bool {
	for _, line := range lines {
		if strings.Contains(line, keyword) {
			return true
		}
	}
	return false
}

// TestExpandTaskReferences 锁定 @ 引用展开：合法引用附加内容块、去重、越
// 界与缺失失败关闭、无引用原文返回、超长截断标注。
func TestExpandTaskReferences(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "note.txt"), []byte("hello note"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "big.txt"), bytes.Repeat([]byte("x"), referencePreviewLimit+100), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "out"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "out"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	workspace, err := workspaceForRoot(root)
	if err != nil {
		t.Fatal(err)
	}

	expanded, err := expandTaskReferences("看看 @note.txt 的内容", workspace)
	if err != nil || !strings.Contains(expanded, "hello note") || !strings.Contains(expanded, "看看") {
		t.Fatalf("合法引用应展开: %q err=%v", expanded, err)
	}
	if expanded, err := expandTaskReferences("重复 @note.txt 与 @note.txt", workspace); err != nil || strings.Count(expanded, "[引用文件") != 1 {
		t.Fatalf("重复引用应去重: %q err=%v", expanded, err)
	}
	if expanded, err := expandTaskReferences("邮箱 a@b.com 不算引用", workspace); err != nil || expanded != "邮箱 a@b.com 不算引用" {
		t.Fatalf("词内 @ 不应被识别为引用: %q err=%v", expanded, err)
	}
	if _, err := expandTaskReferences("越界 @../etc/passwd", workspace); err == nil {
		t.Fatal("越界引用应失败关闭")
	}
	if _, err := expandTaskReferences("缺失 @nope.txt", workspace); err == nil {
		t.Fatal("缺失引用应失败关闭")
	}
	expanded, err = expandTaskReferences("大文件 @big.txt", workspace)
	if err != nil || !strings.Contains(expanded, "已截断") || !strings.Contains(expanded, "read_file") {
		t.Fatalf("超长应截断并提示: err=%v", err)
	}
	if expanded, err := expandTaskReferences("普通任务", workspace); err != nil || expanded != "普通任务" {
		t.Fatal("无引用应原文返回")
	}
}

// TestCycleThemeRotatesAndFailsClosed 锁定主题切换：循环推进全部内置主题、
// 越界回落默认、当前标签一致。
func TestCycleThemeRotatesAndFailsClosed(t *testing.T) {
	applyTheme(0)
	seen := map[string]bool{}
	for index := 0; index < len(builtinThemes); index++ {
		message := cycleTheme()
		seen[currentThemeLabel()] = true
		if message == "" {
			t.Fatal("切换应返回提示")
		}
	}
	if len(seen) != len(builtinThemes) {
		t.Fatalf("一轮切换应遍历全部主题，见到 %v", seen)
	}
	applyTheme(len(builtinThemes) + 5)
	if activeThemeIndex != 0 {
		t.Fatalf("越界序号应回落默认主题，得到 %d", activeThemeIndex)
	}
}

// TestEditorClosedRefillsInput 锁定外部编辑器回填：输入态收消息回填，错误
// 如实提示。
func TestEditorClosedRefillsInput(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.mode = consoleInput
	model, _ := console.Update(editorClosedMsg{content: "编辑后的任务"})
	console = model.(RunConsole)
	if console.taskField.Value() != "编辑后的任务" {
		t.Fatalf("应回填输入框，得到 %q", console.taskField.Value())
	}
	console.lines = nil
	model, _ = console.Update(editorClosedMsg{err: errors.New("exit 1")})
	console = model.(RunConsole)
	if len(console.lines) == 0 || !containsLine(console.lines, "外部编辑器") {
		t.Fatalf("编辑器失败应如实提示: %v", console.lines)
	}
}

// TestRewindConversationChain 锁定会话级回退：无参列出、合法序号回退续链点
// 并截断链、越界拒绝、回退后新任务携带指回的前运行。
func TestRewindConversationChain(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.sessionID = "session-x"
	console.sessionRuns = []sessionRunEntry{
		{id: "run-1", verdict: "完成", summary: "创建文件"},
		{id: "run-2", verdict: "失败", summary: "修改文件"},
		{id: "run-3", verdict: "完成", summary: "再修一次"},
	}
	console.sessionPrev = "run-3"

	console.mode = consoleInput
	console.taskField.SetValue("/rewind")
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if !containsLine(console.lines, "仅回退对话不动工作区") || !containsLine(console.lines, "修改文件") {
		t.Fatalf("无参应列出链与语义说明: %v", console.lines)
	}

	console.taskField.SetValue("/rewind 1")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.sessionPrev != "run-1" || len(console.sessionRuns) != 1 {
		t.Fatalf("回退应指回 run-1 并截断链，得到 prev=%q runs=%d", console.sessionPrev, len(console.sessionRuns))
	}
	if !containsLine(console.lines, "工作区未变动") {
		t.Fatal("回退提示必须明示工作区未变动")
	}

	console.taskField.SetValue("/rewind 9")
	console.mode = consoleInput
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.sessionPrev != "run-1" || !containsLine(console.lines, "越界") {
		t.Fatal("越界序号应拒绝")
	}
}

// TestRewindFilesCommandDispatch 锁定 /rewind-files 的输入态分发：空链提示、
// 列表、越界与末节点守卫、有效选点进入运行态并保持回退标记。
func TestRewindFilesCommandDispatch(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.mode = consoleInput
	console.taskField.SetValue("/rewind-files\r")
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if !strings.Contains(strings.Join(console.lines, "\n"), "无可回退的会话链") {
		t.Fatal("空链应提示无可回退")
	}

	console.sessionID = "session_test"
	console.sessionRuns = []sessionRunEntry{
		{id: "run_first", verdict: "运行完成", summary: "first"},
		{id: "run_second", verdict: "运行完成", summary: "second"},
	}
	console.lines = nil
	console.taskField.SetValue("/rewind-files\r")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if !strings.Contains(strings.Join(console.lines, "\n"), "run_second") {
		t.Fatal("无参调用应列出会话链节点")
	}

	console.lines = nil
	console.taskField.SetValue("/rewind-files 3\r")
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if !strings.Contains(strings.Join(console.lines, "\n"), "序号越界") {
		t.Fatal("越界序号应被拒绝")
	}

	console.lines = nil
	console.taskField.SetValue("/rewind-files 1\r")
	model, cmd := console.Update(tea.KeyMsg{Type: tea.KeyEnter})
	console = model.(RunConsole)
	if console.mode != consoleRunning || console.rewinding == nil {
		t.Fatalf("有效选点应进入运行态并保持回退标记: mode=%d rewinding=%v", console.mode, console.rewinding)
	}
	if cmd == nil {
		t.Fatal("有效选点应发起回退命令")
	}
}

// TestReceiveRewindResultTransitions 锁定回退结果的状态机：审批暂停保持回退
// 标记并进入审批模式；终态清标记并展示反放计数。
func TestReceiveRewindResultTransitions(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.mode = consoleRunning
	console.rewinding = &rewindState{}
	waiting := &agent.WaitingApprovalError{RunID: "run_rewind", Fingerprint: "sha256:f", Summary: "update note.txt"}
	model, cmd := console.Update(rewindFinishedMsg{err: waiting})
	console = model.(RunConsole)
	if console.mode != consoleApproval || console.rewinding == nil {
		t.Fatalf("审批暂停应保持回退标记: mode=%d rewinding=%v", console.mode, console.rewinding)
	}
	if cmd == nil {
		t.Fatal("审批暂停应继续消费事件通道")
	}
	if !strings.Contains(strings.Join(console.lines, "\n"), "回退等待审批") {
		t.Fatal("审批暂停应展示回退语义提示")
	}

	console.rewinding = &rewindState{applied: 2}
	model, _ = console.Update(rewindFinishedMsg{result: &app.RunResult{ID: "run_rewind"}})
	console = model.(RunConsole)
	if console.mode != consoleDone || console.rewinding != nil {
		t.Fatalf("终态应清回退标记: mode=%d rewinding=%v", console.mode, console.rewinding)
	}
	if !strings.Contains(strings.Join(console.lines, "\n"), "反放 2 个补丁") {
		t.Fatal("终态应展示反放计数")
	}
}

// TestApprovalDiffKeyRouting 锁定审批态差异键路由：v 进入差异视图、Esc 返回
// 审批态；差异不可用时就地提示且不改变审批决定入口。
func TestApprovalDiffKeyRouting(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	console.mode = consoleApproval
	console.waiting = &agent.WaitingApprovalError{RunID: "run_missing", OperationID: "op_missing", Fingerprint: "sha256:f", Summary: "update note.txt"}
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	console = model.(RunConsole)
	if console.mode != consoleApproval || !strings.Contains(strings.Join(console.lines, "\n"), "差异不可用") {
		t.Fatalf("差异不可用应留在审批态并提示: mode=%d lines=%v", console.mode, console.lines)
	}
}

// TestTranscriptDiffKeyRouting 锁定事件查看器差异键路由：补丁提议行渲染差
// 异，非提议行如实提示，Esc 返回查看器。
func TestTranscriptDiffKeyRouting(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	operation := domain.OperationID("op_diff")
	proposal := eventWithPayload(domain.EventToolCallProposed, `{"call_id":"c1","tool_name":"apply_patch","arguments":{"path":"fresh.txt","mode":"create","content":"created\n"}}`)
	proposal.OperationID = &operation
	started := eventWithPayload(domain.EventOperationStarted, `{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"fresh.txt","planned_before":"absent","planned_after":"sha256:x"}`)
	started.OperationID = &operation
	console.transcript = []domain.Event{eventWithPayload(domain.EventTurnOpened, `{"turn":1}`), proposal, started}
	console.transcriptCursor = 1
	console.mode = consoleTranscript
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	console = model.(RunConsole)
	if console.mode != consoleDiff || len(console.diffLines) == 0 {
		t.Fatalf("补丁提议应进入差异视图: mode=%d lines=%d", console.mode, len(console.diffLines))
	}
	rendered := strings.Join(console.diffLines, "\n")
	if !strings.Contains(rendered, "补丁 create · fresh.txt") {
		t.Fatalf("差异头缺失: %s", rendered)
	}
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyEsc})
	console = model.(RunConsole)
	if console.mode != consoleTranscript {
		t.Fatalf("Esc 应返回事件查看器: mode=%d", console.mode)
	}
	console.transcriptCursor = 0
	model, _ = console.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	console = model.(RunConsole)
	if console.mode != consoleDiff || !strings.Contains(strings.Join(console.diffLines, "\n"), "不是工具提议") {
		t.Fatalf("非提议行应如实提示: %v", console.diffLines)
	}
}

// TestTranscriptDiffAnnotatesUnappliedPatch 锁定回看诚实性：未生效补丁的差
// 异头部标注终态，明示其为提议推演而非既成事实。
func TestTranscriptDiffAnnotatesUnappliedPatch(t *testing.T) {
	console := NewRunConsole(context.Background(), RunConsoleInput{Config: config.Config{}})
	operation := domain.OperationID("op_failed")
	proposal := eventWithPayload(domain.EventToolCallProposed, `{"call_id":"c2","tool_name":"apply_patch","arguments":{"path":"note.txt","mode":"update","replacements":[{"old":"a","new":"b"}]}}`)
	proposal.OperationID = &operation
	started := eventWithPayload(domain.EventOperationStarted, `{"tool_name":"apply_patch","side_effect_class":"workspace","planned_path":"note.txt","planned_before":"sha256:b","planned_after":"sha256:c"}`)
	started.OperationID = &operation
	failed := eventWithPayload(domain.EventOperationFailed, `{"tool_name":"apply_patch","error_class":"io"}`)
	failed.OperationID = &operation
	console.transcript = []domain.Event{proposal, started, failed}
	console.transcriptCursor = 0
	console.mode = consoleTranscript
	model, _ := console.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("v")})
	console = model.(RunConsole)
	rendered := strings.Join(console.diffLines, "\n")
	if !strings.Contains(rendered, "该补丁未生效（operation_failed）") {
		t.Fatalf("unapplied annotation missing: %s", rendered)
	}
}
