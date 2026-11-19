// [INPUT]: 依赖 App 运行入口、Bubble Tea 消息与操作上下文。
// [OUTPUT]: 提供运行命令、异步查询及前台操作取消域。
// [POS]: App 结果按事件顺序回到主控制台。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 运行控制面 tea.Cmd 构造。
// Author: fxbin
package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

func waitForConsoleShutdown(rootCtx, operationParent context.Context) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-rootCtx.Done():
			return nil
		case <-operationParent.Done():
			return consoleShutdownMsg{}
		}
	}
}

// waitForOperationMessage 按 FIFO 读取运行事件与终态；根 context 取消时退出，
// 避免控制台关闭后遗留阻塞等待者。
func waitForOperationMessage(ctx context.Context, messages <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		select {
		case <-ctx.Done():
			return nil
		case message := <-messages:
			return message
		}
	}
}

// loadSessionHistoryCmd 异步读取当前工作区历史，避免 SQLite 查询阻塞界面事件循环。
func loadSessionHistoryCmd(console RunConsole) tea.Cmd {
	service := console.input.Service
	ctx := console.rootCtx
	workspace := console.input.Workspace
	configuration := console.input.Config
	return func() tea.Msg {
		items, err := service.ListRuns(ctx, workspace, console.input.StateDir, configuration)
		return sessionHistoryLoadedMsg{items: items, err: err}
	}
}

// loadSessionContinuationCmd 异步验证历史运行的完整会话链与唯一终态链尾。
func loadSessionContinuationCmd(console RunConsole, runID domain.RunID, requestID uint64) tea.Cmd {
	service := console.input.Service
	ctx := console.rootCtx
	workspace := console.input.Workspace
	configuration := console.input.Config
	return func() tea.Msg {
		continuation, err := service.ResolveSessionContinuation(ctx, workspace, console.input.StateDir, configuration, runID)
		return sessionContinuationLoadedMsg{continuation: continuation, requestID: requestID, err: err}
	}
}

// loadSessionTreeCmd 异步读取只读会话树投影，供 /tree 展示与 /fork 选择。
func loadSessionTreeCmd(console RunConsole) tea.Cmd {
	service := console.input.Service
	ctx := console.rootCtx
	workspace := console.input.Workspace
	configuration := console.input.Config
	return func() tea.Msg {
		tree, err := service.ListSessionTree(ctx, workspace, console.input.StateDir, configuration)
		return sessionTreeLoadedMsg{tree: tree, err: err}
	}
}

// loadSessionForkCmd 异步校验分支来源并返回创建锚点；不创建运行。
func loadSessionForkCmd(console RunConsole, runID domain.RunID, requestID uint64) tea.Cmd {
	service := console.input.Service
	ctx := console.rootCtx
	workspace := console.input.Workspace
	configuration := console.input.Config
	return func() tea.Msg {
		anchor, err := service.ResolveSessionFork(ctx, workspace, console.input.StateDir, configuration, runID)
		return sessionForkLoadedMsg{anchor: anchor, sourceID: runID, requestID: requestID, err: err}
	}
}

// startRunCmd 发起一次运行：事件回调把已持久化事件写入通道，最终结果经
// runFinishedMsg 回到事件循环。
func startRunCmd(console RunConsole, task string) tea.Cmd {
	service := console.input.Service
	runCtx := console.operationContext()
	generation := console.operationGeneration
	messages := console.messages
	onEvent := operationEventSink(runCtx, generation, messages)
	return func() tea.Msg {
		result, err := service.Run(runCtx, app.RunOptions{
			Task: task, Workspace: console.input.Workspace, StateDir: console.input.StateDir, Config: console.input.Config, OnEvent: onEvent,
			SessionID: console.sessionID, PrevRunID: console.sessionPrev, RequireSessionTail: console.sessionTailRequired,
			ForkSource: console.sessionForkSource,
			Steering:   console.steering,
		})
		sendOperationMessage(console.rootCtx, messages, runFinishedMsg{generation: generation, result: &result, err: err})
		return nil
	}
}

// resolveApprovalCmd 以用户决定恢复运行：指纹与策略重放校验由既有 Loop 合
// 同承担，界面只传递显式决定；工作区回退的审批续跑同样经 Resume，但结果按
// 回退语义呈现。
func resolveApprovalCmd(console RunConsole, approve bool) tea.Cmd {
	service := console.input.Service
	runCtx := console.operationContext()
	generation := console.operationGeneration
	messages := console.messages
	waiting := console.waiting
	rewinding := console.rewinding
	onEvent := operationEventSink(runCtx, generation, messages)
	return func() tea.Msg {
		result, err := service.Resume(runCtx, app.ResumeOptions{
			RunID: waiting.RunID, Workspace: console.input.Workspace, StateDir: console.input.StateDir, Config: console.input.Config,
			Approval: ports.Approval{Fingerprint: waiting.Fingerprint, Approve: approve, Reason: approvalReason(approve)},
			OnEvent:  onEvent,
		})
		if rewinding != nil {
			sendOperationMessage(console.rootCtx, messages, rewindFinishedMsg{generation: generation, result: &result, err: err})
			return nil
		}
		sendOperationMessage(console.rootCtx, messages, runFinishedMsg{generation: generation, result: &result, err: err})
		return nil
	}
}

// rewindFilesCmd 发起工作区回退：逐项反放选定节点之后的已生效补丁，事件流
// 与审批复用运行通道，最终结果按回退语义收束。
func rewindFilesCmd(console RunConsole, runs []domain.RunID) tea.Cmd {
	service := console.input.Service
	runCtx := console.operationContext()
	generation := console.operationGeneration
	messages := console.messages
	onEvent := operationEventSink(runCtx, generation, messages)
	return func() tea.Msg {
		result, err := service.Rewind(runCtx, app.RewindOptions{
			Workspace: console.input.Workspace, StateDir: console.input.StateDir, Config: console.input.Config,
			Runs: runs, OnEvent: onEvent,
		})
		sendOperationMessage(console.rootCtx, messages, rewindFinishedMsg{generation: generation, result: &result, err: err})
		return nil
	}
}

// rewindFinishedMsg 承载一次工作区回退（或其审批续跑）的最终结果及操作世代。
type rewindFinishedMsg struct {
	generation uint64
	result     *app.RunResult
	err        error
}

// operationEventSink 把已持久化事件绑定到产生它的操作世代；操作取消时停止
// 等待界面缓冲，避免同步事件回调阻塞服务收束。完整事实仍保存在 Journal。
func operationEventSink(ctx context.Context, generation uint64, messages chan<- tea.Msg) func(domain.Event) {
	return func(event domain.Event) {
		sendOperationMessage(ctx, messages, consoleEventMsg{generation: generation, event: event})
	}
}

// sendOperationMessage 按产生顺序投递事件与终态，并在生命周期取消时解除背压。
func sendOperationMessage(ctx context.Context, messages chan<- tea.Msg, message tea.Msg) {
	select {
	case messages <- message:
	case <-ctx.Done():
	}
}

// noMessageCmd 满足状态转换需要返回命令的界面合同，但不建立第二个事件消费者。
func noMessageCmd() tea.Msg { return nil }

// approvalReason 是界面审批决定的固定理由文案，进入审批事件留痕。
func approvalReason(approve bool) string {
	if approve {
		return "approved in tui console"
	}
	return "denied in tui console"
}

// Update 处理主界面的按键、事件与运行结果。
func persistQueueCmd(console RunConsole, items []string) tea.Cmd {
	return func() tea.Msg {
		service := console.input.Service
		ctx := console.rootCtx
		workspace := console.input.Workspace
		stateDir := console.input.StateDir
		configuration := console.input.Config
		for index, content := range items {
			inputID := domain.NewID("queue")
			_, err := service.SubmitQueueInput(ctx, app.QueueSubmitOptions{
				Workspace: workspace, StateDir: stateDir, Config: configuration,
				InputID: inputID, Content: content,
			})
			if err != nil {
				return queueActionDoneMsg{action: "persisted", remaining: append([]string(nil), items[index:]...), err: err}
			}
		}
		return queueActionDoneMsg{action: "persisted"}
	}
}

// receiveResult 消费一次运行或恢复的结果：等待审批转入审批模式，终态进入
// 结果页；其余错误如实展示。

// beginOperation 为一次前台 App 操作创建独立取消域；调用方必须
// 在启动异步命令前调用，已有操作会先释放。操作继承调用方取消源，但消息泵
// 保持独立，确保外部取消后仍能按 FIFO 消费已持久化事件与最终结果。
func (console *RunConsole) beginOperation() {
	console.finishOperation()
	console.operationGeneration++
	console.operationCtx, console.operationCancel = context.WithCancel(console.operationParent)
}

// operationContext 返回当前操作 context；未建立操作时返回根 context，供
// 异常状态失败关闭而不制造脱离控制台生命周期的后台工作。
func (console RunConsole) operationContext() context.Context {
	if console.operationCtx != nil {
		return console.operationCtx
	}
	return console.rootCtx
}

// cancelOperation 只取消当前前台操作，不终止控制台及历史、向导等后续能力。
func (console RunConsole) cancelOperation() {
	if console.operationCancel != nil {
		console.operationCancel()
	}
}

// finishOperation 释放当前操作 context，并清除引用以便下一轮建立全新取消域。
func (console *RunConsole) finishOperation() {
	if console.operationCancel != nil {
		console.operationCancel()
	}
	console.operationCtx = nil
	console.operationCancel = nil
}

// resetRun 清空上一轮运行的展示与统计，保留输入态历史行由滚动缓冲自然
// 淘汰。
func (console *RunConsole) resetRun() {
	console.sessionPromptTokens += console.promptTokens
	console.sessionOutputTokens += console.outputTokens
	console.lines = nil
	console.transcript = nil
	console.transcriptCursor = 0
	console.turn = 0
	console.promptTokens = 0
	console.outputTokens = 0
	console.toolCalls = 0
	console.turnToolCalls, console.turnExecutedTools = 0, 0
	console.turnPromptTokens, console.turnOutputTokens = 0, 0
	console.turnOpenedAt = time.Time{}
	console.waiting = nil
	console.result = nil
	console.runErr = nil
	console.rewinding = nil
	console.diffLines = nil
	console.diffCursor = 0
	console.diffReturn = consoleInput
	console.stopWait = false
}
