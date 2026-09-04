// run.go 实现单任务运行闭环的终端主界面：任务输入、已持久化事件的实时滚
// 动摘要、在线审批（等待审批错误 → 用户决定 → 恢复）、取消与终态展示。
// 本文件只做展示与输入编排——运行、审批、取消与恢复全部复用 app.Service
// 的既有合同，事件摘要只消费已持久化事实，界面退出不改变任何事件日志。
//
// Author: fxbin
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// consoleMode 是运行主界面的模式全集：任务输入、运行中、等待审批与终态。
type consoleMode int

// 运行主界面模式；转换经事件驱动，禁止跳过运行态直接进入审批或终态。
const (
	consoleInput consoleMode = iota
	consoleRunning
	consoleApproval
	consoleDone
)

// maxConsoleLines 是事件摘要的滚动缓冲行数上限；更早的行滚出屏幕，完整事
// 实仍在事件日志中，界面不承担审计职责。
const maxConsoleLines = 18

// consoleEventBuffer 是事件通道缓冲上限；消费停滞时对 Loop 事件写入形成
// 背压而不是丢弃——界面可能落后，但不会漏展示已持久化事实。
const consoleEventBuffer = 256

// consoleInputPrompt 是任务输入框提示语。
const consoleInputPrompt = "任务描述"

// RunConsoleInput 是运行主界面的装配输入：配置与 Provider 语义由调用方加
// 载（与 CLI 共用同一配置解析链），本界面不重复装载。
type RunConsoleInput struct {
	Workspace string
	Config    config.Config
	Service   app.Service
}

// consoleEventMsg 把已持久化的领域事件送入界面事件循环。
type consoleEventMsg struct{ event domain.Event }

// runFinishedMsg 承载一次 Run 或 Resume 的最终结果；err 可能携带等待审批
// 错误，由界面转入审批模式而不是终止。
type runFinishedMsg struct {
	result *app.RunResult
	err    error
}

// RunConsole 是单任务运行闭环的 bubbletea 模型。
type RunConsole struct {
	ctx      context.Context
	cancel   context.CancelFunc
	input    RunConsoleInput
	events   chan domain.Event
	stopWait bool

	mode      consoleMode
	taskField textinput.Model

	lines        []string
	turn         int
	promptTokens int64
	outputTokens int64
	toolCalls    int

	waiting *agent.WaitingApprovalError
	result  *app.RunResult
	runErr  error
}

// NewRunConsole 构造运行主界面模型：ctx 承载取消语义（界面 Ctrl+C 在运行
// 中触发取消，Run 由 Loop 持久化取消终态后返回）。
func NewRunConsole(ctx context.Context, runInput RunConsoleInput) RunConsole {
	runCtx, cancel := context.WithCancel(ctx)
	taskField := newTextinput(consoleInputPrompt)
	console := RunConsole{
		ctx:       runCtx,
		cancel:    cancel,
		input:     runInput,
		events:    make(chan domain.Event, consoleEventBuffer),
		mode:      consoleInput,
		taskField: taskField,
	}
	console.taskField.Focus()
	return console
}

// Init 启动任务输入框与事件通道等待。
func (console RunConsole) Init() tea.Cmd {
	return tea.Batch(console.taskField.Focus(), waitForEvent(console.events))
}

// waitForEvent 阻塞等待下一条已持久化事件；通道关闭后返回 nil 消息结束
// 等待链（运行收束时由界面停止消费）。
func waitForEvent(events <-chan domain.Event) tea.Cmd {
	return func() tea.Msg {
		event, ok := <-events
		if !ok {
			return nil
		}
		return consoleEventMsg{event: event}
	}
}

// startRunCmd 发起一次运行：事件回调把已持久化事件写入通道，最终结果经
// runFinishedMsg 回到事件循环。
func startRunCmd(console RunConsole, task string) tea.Cmd {
	service := console.input.Service
	runCtx := console.ctx
	events := console.events
	onEvent := func(event domain.Event) { events <- event }
	return func() tea.Msg {
		result, err := service.Run(runCtx, app.RunOptions{
			Task: task, Workspace: console.input.Workspace, Config: console.input.Config, OnEvent: onEvent,
		})
		return runFinishedMsg{result: &result, err: err}
	}
}

// resolveApprovalCmd 以用户决定恢复运行：指纹与策略重放校验由既有 Loop 合
// 同承担，界面只传递显式决定。
func resolveApprovalCmd(console RunConsole, approve bool) tea.Cmd {
	service := console.input.Service
	runCtx := console.ctx
	events := console.events
	waiting := console.waiting
	onEvent := func(event domain.Event) { events <- event }
	return func() tea.Msg {
		result, err := service.Resume(runCtx, app.ResumeOptions{
			RunID: waiting.RunID, Workspace: console.input.Workspace, Config: console.input.Config,
			Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: approve, Reason: approvalReason(approve)},
			OnEvent:  onEvent,
		})
		return runFinishedMsg{result: &result, err: err}
	}
}

// approvalReason 是界面审批决定的固定理由文案，进入审批事件留痕。
func approvalReason(approve bool) string {
	if approve {
		return "approved in tui console"
	}
	return "denied in tui console"
}

// Update 处理主界面的按键、事件与运行结果。
func (console RunConsole) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if message, ok := msg.(tea.KeyMsg); ok {
		return console.updateKey(message)
	}
	switch typed := msg.(type) {
	case consoleEventMsg:
		console.applyEvent(typed.event)
		if console.stopWait {
			return console, nil
		}
		return console, waitForEvent(console.events)
	case runFinishedMsg:
		return console.receiveResult(typed)
	}
	return console, nil
}

// updateKey 按模式分发按键：输入态 Enter 提交任务；运行态 Ctrl+C 请求取
// 消；审批态 Enter 批准、Esc 拒绝；终态 Enter 回到输入、Ctrl+C 退出。
func (console RunConsole) updateKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch console.mode {
	case consoleInput:
		switch message.String() {
		case "ctrl+c":
			console.cancel()
			return console, tea.Quit
		case "enter":
			task := strings.TrimSpace(console.taskField.Value())
			if task == "" {
				return console, nil
			}
			console.mode = consoleRunning
			console.resetRun()
			console.appendLine(okStyle.Render("▶ 任务已提交，运行中（Ctrl+C 取消）"))
			return console, startRunCmd(console, task)
		default:
			var cmd tea.Cmd
			console.taskField, cmd = console.taskField.Update(message)
			return console, cmd
		}
	case consoleRunning:
		if message.String() == "ctrl+c" {
			console.appendLine(warnStyle.Render("⏸ 已请求取消，等待运行持久化终态…"))
			console.cancel()
		}
		return console, nil
	case consoleApproval:
		switch message.String() {
		case "enter":
			console.mode = consoleRunning
			console.appendLine(okStyle.Render("▶ 已批准，继续运行"))
			return console, resolveApprovalCmd(console, true)
		case "esc", "d":
			console.mode = consoleRunning
			console.appendLine(warnStyle.Render("▶ 已拒绝，运行将按拒绝终态收束"))
			return console, resolveApprovalCmd(console, false)
		}
		return console, nil
	case consoleDone:
		switch message.String() {
		case "ctrl+c":
			console.cancel()
			return console, tea.Quit
		case "enter":
			console.mode = consoleInput
			console.taskField.Reset()
			console.taskField.Focus()
			return console, nil
		}
		return console, nil
	}
	return console, nil
}

// receiveResult 消费一次运行或恢复的结果：等待审批转入审批模式，终态进入
// 结果页；其余错误如实展示。
func (console RunConsole) receiveResult(finished runFinishedMsg) (tea.Model, tea.Cmd) {
	var waiting *agent.WaitingApprovalError
	if errors.As(finished.err, &waiting) {
		console.mode = consoleApproval
		console.waiting = waiting
		console.appendLine(warnStyle.Render("⏸ 等待审批: " + waiting.Summary))
		console.appendLine(dimStyle.Render("  指纹 " + waiting.Fingerprint + " · Enter 批准 / Esc 拒绝"))
		return console, waitForEvent(console.events)
	}
	console.mode = consoleDone
	console.result = finished.result
	console.runErr = finished.err
	console.stopWait = true
	close(console.events)
	console.appendLine(console.verdictLine())
	return console, nil
}

// resetRun 清空上一轮运行的展示与统计，保留输入态历史行由滚动缓冲自然
// 淘汰。
func (console *RunConsole) resetRun() {
	console.lines = nil
	console.turn = 0
	console.promptTokens = 0
	console.outputTokens = 0
	console.toolCalls = 0
	console.waiting = nil
	console.result = nil
	console.runErr = nil
	console.stopWait = false
}

// applyEvent 把已持久化事件追加为摘要行并累计运行统计。
func (console *RunConsole) applyEvent(event domain.Event) {
	switch event.Type {
	case domain.EventTurnOpened:
		console.turn++
	case domain.EventToolCallProposed:
		console.toolCalls++
	case domain.EventModelResponseFinished:
		var payload struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if eventPayload(event, &payload) {
			console.promptTokens += int64(payload.Usage.PromptTokens)
			console.outputTokens += int64(payload.Usage.CompletionTokens)
		}
	}
	if line := summarizeEvent(event); line != "" {
		console.appendLine(line)
	}
}

// appendLine 追加展示行并维持滚动缓冲上限。
func (console *RunConsole) appendLine(line string) {
	console.lines = append(console.lines, line)
	if len(console.lines) > maxConsoleLines {
		console.lines = console.lines[len(console.lines)-maxConsoleLines:]
	}
}

// verdictLine 生成终态判定行：成功展示结果摘要，失败展示错误与失败分类。
func (console RunConsole) verdictLine() string {
	if console.runErr != nil {
		return errStyle.Render("● 运行失败: " + console.runErr.Error())
	}
	if console.result == nil || console.result.Result == nil {
		return errStyle.Render("● 运行结束（结果投影缺失）")
	}
	summary := console.result.Result
	verdict := okStyle.Render(fmt.Sprintf("● %s（outcome=%s", runStateText(summary.State), summary.Outcome))
	if summary.FailureClass != "" {
		verdict = errStyle.Render(fmt.Sprintf("● %s（outcome=%s，failure=%s", runStateText(summary.State), summary.Outcome, summary.FailureClass))
	}
	return verdict + "）"
}

// runStateText 把运行终态映射为中文短语。
func runStateText(state string) string {
	switch state {
	case "completed":
		return "运行完成"
	case "failed":
		return "运行失败"
	case "cancelled":
		return "已取消"
	default:
		return "运行结束（" + state + "）"
	}
}

// eventPayload 把事件 payload 解码到目标结构；解码失败返回 false，由调用
// 方按信息缺失处理而不是终止。
func eventPayload(event domain.Event, target any) bool {
	if len(event.Payload) == 0 {
		return false
	}
	return json.Unmarshal(event.Payload, target) == nil
}

// summarizeEvent 把已持久化事件投影为一行展示摘要；返回空串表示该事件静
// 默（仍计入事实流但不占屏幕）。摘要只读事件字段，不做推断。
func summarizeEvent(event domain.Event) string {
	switch event.Type {
	case domain.EventRunStarted:
		return dimStyle.Render("▶ 运行开始")
	case domain.EventTurnOpened:
		var payload struct {
			TurnIndex int `json:"turn_index"`
		}
		eventPayload(event, &payload)
		return titleStyle.Render(fmt.Sprintf("── 第 %d 轮 ──", payload.TurnIndex))
	case domain.EventToolCallProposed:
		var payload struct {
			ToolName string `json:"tool_name"`
		}
		eventPayload(event, &payload)
		return itemStyle.Render("· 工具: " + payload.ToolName)
	case domain.EventOperationSucceeded:
		var payload struct {
			DurationMs int64 `json:"duration_ms"`
		}
		eventPayload(event, &payload)
		return dimStyle.Render(fmt.Sprintf("  ✓ 完成（%dms）", payload.DurationMs))
	case domain.EventOperationFailed:
		var payload struct {
			ErrorClass string `json:"error_class"`
		}
		eventPayload(event, &payload)
		return errStyle.Render("  ✗ 工具失败: " + payload.ErrorClass)
	case domain.EventModelResponseFinished:
		var payload struct {
			StopReason string `json:"stop_reason"`
			Usage      struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		eventPayload(event, &payload)
		return dimStyle.Render(fmt.Sprintf("· 模型回复（%s · in %d / out %d tok）", payload.StopReason, payload.Usage.PromptTokens, payload.Usage.CompletionTokens))
	case domain.EventModelRequestFailed:
		var payload struct {
			ErrorClass string `json:"error_class"`
		}
		eventPayload(event, &payload)
		return errStyle.Render("⚠ 模型请求失败: " + payload.ErrorClass)
	case domain.EventApprovalRequested:
		var payload struct {
			Summary string `json:"summary"`
		}
		eventPayload(event, &payload)
		return warnStyle.Render("⏸ 审批请求: " + payload.Summary)
	case domain.EventVerificationStarted:
		return dimStyle.Render("· 验证运行…")
	case domain.EventVerificationFinished:
		var payload struct {
			OK bool `json:"ok"`
		}
		eventPayload(event, &payload)
		if payload.OK {
			return okStyle.Render("✓ 验证通过")
		}
		return errStyle.Render("✗ 验证未通过")
	case domain.EventContextCompacted:
		var payload struct {
			UnitsBefore int `json:"estimated_units_before"`
			UnitsAfter  int `json:"estimated_units_after"`
		}
		eventPayload(event, &payload)
		return dimStyle.Render(fmt.Sprintf("· 上下文压缩 %d → %d", payload.UnitsBefore, payload.UnitsAfter))
	case domain.EventRunCancelled:
		return warnStyle.Render("● 已取消")
	case domain.EventRunFailed:
		var payload struct {
			Reason string `json:"reason"`
		}
		eventPayload(event, &payload)
		return errStyle.Render("● 运行失败: " + payload.Reason)
	case domain.EventRunCompleted:
		return okStyle.Render("● 运行完成")
	default:
		return ""
	}
}

// View 渲染主界面：输入态聚焦任务框，运行态滚动事件摘要与统计行，审批态
// 展示决定入口，终态展示判定与后续动作。
func (console RunConsole) View() string {
	var sections []string
	sections = append(sections, titleStyle.Render("Cairn")+dimStyle.Render("  单任务运行"))
	sections = append(sections, strings.Join(console.lines, "\n"))
	switch console.mode {
	case consoleInput:
		sections = append(sections, "", console.taskField.View(), dimStyle.Render("Enter 提交任务 / Ctrl+C 退出"))
	case consoleRunning:
		sections = append(sections, "", console.statusLine())
	case consoleApproval:
		sections = append(sections, "", warnStyle.Render("Enter 批准 / Esc 拒绝"))
	case consoleDone:
		sections = append(sections, "")
		if console.runErr == nil && console.result != nil && console.result.Result != nil {
			if response := strings.TrimSpace(console.result.Response); response != "" {
				sections = append(sections, itemStyle.Render(finalResponsePreview(response)))
			}
		}
		sections = append(sections, dimStyle.Render("Enter 新任务 / Ctrl+C 退出"))
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...) + "\n"
}

// statusLine 生成运行中的统计行：轮次、工具调用与累计 token。
func (console RunConsole) statusLine() string {
	return dimStyle.Render(fmt.Sprintf("轮次 %d · 工具调用 %d · 累计 in %d / out %d tok · Ctrl+C 取消", console.turn, console.toolCalls, console.promptTokens, console.outputTokens))
}

// finalResponsePreview 截取最终回复的首段作为终态预览，完整回复以运行结
// 果与事件日志为准。
func finalResponsePreview(response string) string {
	firstLine := strings.SplitN(strings.TrimSpace(response), "\n", 2)[0]
	runes := []rune(firstLine)
	const previewLimit = 120
	if len(runes) > previewLimit {
		firstLine = string(runes[:previewLimit]) + "…"
	}
	return "回复: " + firstLine
}

// RunConsoleLoop 以终端模式运行单任务主界面并返回进程级结论；非 TTY 环境
// 由调用方先行拦截。
func RunConsoleLoop(ctx context.Context, runInput RunConsoleInput) error {
	program := tea.NewProgram(NewRunConsole(ctx, runInput))
	_, err := program.Run()
	return err
}
