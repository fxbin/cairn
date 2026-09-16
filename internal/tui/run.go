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
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

// consoleMode 是运行主界面的模式全集：任务输入、运行中、等待审批、终态、
// 事件查看、差异查看与配置向导。
type consoleMode int

// 运行主界面模式；转换经事件驱动，禁止跳过运行态直接进入审批或终态。
const (
	consoleInput consoleMode = iota
	consoleRunning
	consoleApproval
	consoleDone
	consoleTranscript
	consoleModelWizard
	consoleDiff
)

// maxConsoleLines 是事件摘要的滚动缓冲行数上限；更早的行滚出屏幕，完整事
// 实仍在事件日志中，界面不承担审计职责。
const maxConsoleLines = 18

// consoleEventBuffer 是事件通道缓冲上限；消费停滞时对 Loop 事件写入形成
// 背压而不是丢弃——界面可能落后，但不会漏展示已持久化事实。
const consoleEventBuffer = 256

// consoleInputPrompt 是任务输入框提示语。
const consoleInputPrompt = "任务描述"

// transcriptEventLimit 是查看器保留的事件数上限；更早事件仍完整存在于
// 事件日志，超出部分从内存查看器淘汰。
const transcriptEventLimit = 5000

// transcriptWindowHeight 是查看器同屏行数。
const transcriptWindowHeight = 15

// RunConsoleInput 是运行主界面的装配输入：配置与 Provider 语义由调用方加
// 载（与 CLI 共用同一配置解析链），本界面不重复装载。
type RunConsoleInput struct {
	Workspace string
	Config    config.Config
	Service   app.Service
	// ReloadConfig 在向导提交新配置后由调用方提供热重载（重新走同一配置
	// 解析链）；缺省时新配置对后续 cairn 进程生效，当前界面如实提示。
	ReloadConfig func() (config.Config, error)
}

// consoleEventMsg 把已持久化的领域事件及其操作世代送入界面事件循环。
type consoleEventMsg struct {
	generation uint64
	event      domain.Event
}

// wizardClosedMsg 表示嵌入的配置向导请求收束（完成或取消），由主界面退
// 出向导模式并按提交结果决定是否热重载。
type wizardClosedMsg struct{}

// wizardRelayMsg 把嵌入向导异步命令的结果绑定到创建它的实例世代；主界面
// 仅向当前实例转发匹配世代的消息，避免旧目录或提交结果污染重开的向导。
type wizardRelayMsg struct {
	generation uint64
	message    tea.Msg
}

// consoleShutdownMsg 表示调用方已请求进程级收束；运行中必须先等待服务写入
// 终态，空闲或审批等待时可直接退出。
type consoleShutdownMsg struct{}

// runFinishedMsg 承载一次 Run 或 Resume 的最终结果及操作世代；err 可能携带
// 等待审批错误，由界面转入审批模式而不是终止。
type runFinishedMsg struct {
	generation uint64
	result     *app.RunResult
	err        error
}

// sessionHistoryLoadedMsg 承载当前工作区的只读持久化历史；消息只驱动展示，
// 不修改当前进程的会话续链或回退范围。
type sessionHistoryLoadedMsg struct {
	items []app.RunHistoryItem
	err   error
}

// sessionContinuationLoadedMsg 承载历史链尾解析结果；成功时只设置下一次
// Run 的会话锚点，不恢复旧进程可回退列表。
type sessionContinuationLoadedMsg struct {
	continuation app.SessionContinuation
	requestID    uint64
	err          error
}

// sessionRunEntry 是会话链内的单个运行记录：运行标识与终态结论，供
// /sessions 展示当前链。
type sessionRunEntry struct {
	id      domain.RunID
	verdict string
	summary string
}

// rewindState 标记主界面正在执行工作区回退：审批续跑走 Resume 但结果按回退
// 语义呈现，回退 run 不进入会话对话链；applied 累计本轮回退中反放成功的补
// 丁数（来自已持久化事件，不是界面推测）。
type rewindState struct {
	applied int
}

// RunConsole 是单任务运行闭环的 bubbletea 模型。rootCtx 覆盖控制台消息泵
// 生命周期，operationParent 接收调用方取消，operationCtx 仅覆盖当前 Run、
// Resume 或 Rewind，避免单次取消污染后续操作。
type RunConsole struct {
	rootCtx             context.Context
	rootCancel          context.CancelFunc
	operationParent     context.Context
	operationCtx        context.Context
	operationCancel     context.CancelFunc
	operationGeneration uint64
	input               RunConsoleInput
	messages            chan tea.Msg
	stopWait            bool
	shutdownRequested   bool

	mode       consoleMode
	taskField  textarea.Model
	queueField textarea.Model
	queuing    bool
	queue      []string

	lines            []string
	transcript       []domain.Event
	transcriptCursor int
	transcriptReturn consoleMode
	diffLines        []string
	diffCursor       int
	diffReturn       consoleMode
	turn             int
	promptTokens     int64
	outputTokens     int64
	toolCalls        int

	sessionPromptTokens int64
	sessionOutputTokens int64
	// 会话链状态（ADR-0018）：首个任务生成会话标识开链，后续任务自动以
	// 前一运行为前置续链；/new 断链后回到无前情状态。
	sessionID           string
	sessionPrev         domain.RunID
	sessionRuns         []sessionRunEntry
	sessionTailRequired bool
	continuationRequest uint64
	lastTask            string
	wizard              *ConfigWizard
	wizardGeneration    uint64
	waiting             *agent.WaitingApprovalError
	approvalDetail      []string
	result              *app.RunResult
	runErr              error
	rewinding           *rewindState
}

// NewRunConsole 构造运行主界面模型：内部根 context 承载消息泵生命周期，
// 调用方 context 承载外部取消；每次 Run、Resume 或 Rewind 另建可取消
// context，运行中 Ctrl+C 不影响后续操作，外部取消后仍可消费持久化终态。
func NewRunConsole(ctx context.Context, runInput RunConsoleInput) RunConsole {
	rootCtx, rootCancel := context.WithCancel(context.Background())
	console := RunConsole{
		rootCtx:         rootCtx,
		rootCancel:      rootCancel,
		operationParent: ctx,
		input:           runInput,
		messages:        make(chan tea.Msg, consoleEventBuffer),
		mode:            consoleInput,
		taskField:       newTaskArea(consoleInputPrompt),
		queueField:      newTaskArea("排队输入"),
	}
	console.taskField.Focus()
	return console
}

// newTaskArea 构造多行任务输入区：Enter 留给外层做提交，换行由 Ctrl+J 或
// Shift+Enter 承担（对齐同类工具的跨终端兜底约定），宽高随终端约束。
func newTaskArea(prompt string) textarea.Model {
	area := textarea.New()
	area.Placeholder = prompt
	area.Prompt = ""
	area.ShowLineNumbers = false
	area.CharLimit = 0
	area.SetHeight(3)
	area.SetWidth(72)
	area.KeyMap.InsertNewline.SetKeys("ctrl+j", "shift+enter")
	return area
}

// Init 启动任务输入框、内部消息等待与一次性的调用方取消观察。
func (console RunConsole) Init() tea.Cmd {
	return tea.Batch(
		console.taskField.Focus(),
		waitForOperationMessage(console.rootCtx, console.messages),
		waitForConsoleShutdown(console.rootCtx, console.operationParent),
	)
}

// waitForConsoleShutdown 把调用方取消转换为界面消息，使运行态先等待持久化
// 终态再退出；控制台自身关闭时直接收束观察者，不遗留阻塞 goroutine。
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
		items, err := service.ListRuns(ctx, workspace, "", configuration)
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
		continuation, err := service.ResolveSessionContinuation(ctx, workspace, "", configuration, runID)
		return sessionContinuationLoadedMsg{continuation: continuation, requestID: requestID, err: err}
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
			Task: task, Workspace: console.input.Workspace, Config: console.input.Config, OnEvent: onEvent,
			SessionID: console.sessionID, PrevRunID: console.sessionPrev, RequireSessionTail: console.sessionTailRequired,
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
			RunID: waiting.RunID, Workspace: console.input.Workspace, Config: console.input.Config,
			Approval: agent.Approval{Fingerprint: waiting.Fingerprint, Approve: approve, Reason: approvalReason(approve)},
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
			Workspace: console.input.Workspace, Config: console.input.Config,
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
func (console RunConsole) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if message, ok := msg.(tea.KeyMsg); ok {
		return console.updateKey(message)
	}
	if edited, isEditor := msg.(editorClosedMsg); isEditor {
		if console.mode == consoleInput {
			if edited.err != nil {
				console.appendLine(warnStyle.Render("外部编辑器执行失败: " + edited.err.Error()))
			} else {
				console.taskField.SetValue(edited.content)
				console.appendLine(dimStyle.Render("已从外部编辑器回填任务输入"))
			}
		}
		return console, nil
	}
	if loaded, isSessionHistory := msg.(sessionHistoryLoadedMsg); isSessionHistory {
		console.showSessionHistory(loaded)
		return console, nil
	}
	if loaded, isContinuation := msg.(sessionContinuationLoadedMsg); isContinuation {
		if loaded.requestID != console.continuationRequest || console.mode != consoleInput {
			return console, nil
		}
		if loaded.err != nil {
			console.appendLine(errStyle.Render("历史会话续链失败: " + loaded.err.Error()))
			return console, nil
		}
		console.sessionID = loaded.continuation.SessionID
		console.sessionPrev = loaded.continuation.PrevRunID
		console.sessionRuns = nil
		console.sessionTailRequired = true
		console.appendLine(okStyle.Render(fmt.Sprintf("已选择历史会话 %s 的链尾 %s；下一任务将携带其前情", console.sessionID, console.sessionPrev)))
		console.appendLine(dimStyle.Render("历史运行保持只读，不进入 /rewind 或 /rewind-files 范围"))
		return console, nil
	}
	if relayed, ok := msg.(wizardRelayMsg); ok {
		if relayed.generation != console.wizardGeneration || console.mode != consoleModelWizard || console.wizard == nil {
			return console, nil
		}
		msg = relayed.message
		if _, quit := msg.(tea.QuitMsg); quit {
			msg = wizardClosedMsg{}
		}
	}
	if _, closed := msg.(wizardClosedMsg); closed {
		exitConsole := console.shutdownRequested
		console.exitWizard()
		if exitConsole {
			return console, tea.Quit
		}
		return console, nil
	}
	if _, shuttingDown := msg.(consoleShutdownMsg); shuttingDown {
		console.shutdownRequested = true
		console.cancelOperation()
		if console.mode == consoleRunning {
			return console, nil
		}
		if console.mode == consoleModelWizard && console.wizard != nil && console.wizard.step == stepCommitting {
			console.rootCancel()
			return console, nil
		}
		console.rootCancel()
		return console, tea.Quit
	}
	if console.mode == consoleModelWizard {
		switch msg.(type) {
		case consoleEventMsg, runFinishedMsg, rewindFinishedMsg:
		default:
			model, cmd := console.relayWizardMsg(msg)
			updated := model.(RunConsole)
			if updated.shutdownRequested && updated.wizard != nil && updated.wizard.step != stepCommitting {
				return updated, tea.Quit
			}
			return updated, cmd
		}
	}
	switch typed := msg.(type) {
	case consoleEventMsg:
		if typed.generation == console.operationGeneration {
			console.applyEvent(typed.event)
		}
		return console, waitForOperationMessage(console.rootCtx, console.messages)
	case runFinishedMsg:
		if typed.generation != console.operationGeneration {
			return console, waitForOperationMessage(console.rootCtx, console.messages)
		}
		model, _ := console.receiveResult(typed)
		updated := model.(RunConsole)
		if updated.shutdownRequested {
			updated.rootCancel()
			return updated, tea.Quit
		}
		return updated, waitForOperationMessage(updated.rootCtx, updated.messages)
	case rewindFinishedMsg:
		if typed.generation != console.operationGeneration {
			return console, waitForOperationMessage(console.rootCtx, console.messages)
		}
		model, _ := console.receiveRewindResult(typed)
		updated := model.(RunConsole)
		if updated.shutdownRequested {
			updated.rootCancel()
			return updated, tea.Quit
		}
		return updated, waitForOperationMessage(updated.rootCtx, updated.messages)
	}
	return console, nil
}

// updateKey 按模式分发按键：输入态 Enter 提交任务；运行态 Ctrl+C 请求取
// 消；审批态 Enter 批准、Esc 拒绝；终态 Enter 回到输入、Ctrl+C 退出。
func (console RunConsole) updateKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	if message.String() == "ctrl+o" && console.mode != consoleApproval && console.mode != consoleTranscript {
		if len(console.transcript) == 0 {
			return console, nil
		}
		console.transcriptReturn = console.mode
		console.transcriptCursor = len(console.transcript) - transcriptWindowHeight
		if console.transcriptCursor < 0 {
			console.transcriptCursor = 0
		}
		console.mode = consoleTranscript
		return console, nil
	}
	switch console.mode {
	case consoleTranscript:
		switch message.String() {
		case "ctrl+c", "esc":
			console.mode = console.transcriptReturn
			return console, nil
		case "up", "k":
			if console.transcriptCursor > 0 {
				console.transcriptCursor--
			}
		case "down", "j":
			if console.transcriptCursor < len(console.transcript)-1 {
				console.transcriptCursor++
			}
		case "v":
			return console.openTranscriptDiff()
		}
		return console, nil
	case consoleDiff:
		switch message.String() {
		case "ctrl+c", "esc", "q":
			console.mode = console.diffReturn
			return console, nil
		case "up", "k":
			if console.diffCursor > 0 {
				console.diffCursor--
			}
		case "down", "j":
			if console.diffCursor < len(console.diffLines)-1 {
				console.diffCursor++
			}
		}
		return console, nil
	case consoleInput:
		switch message.String() {
		case "ctrl+c":
			console.rootCancel()
			return console, tea.Quit
		case "tab":
			current := strings.TrimSpace(console.taskField.Value())
			if strings.HasPrefix(current, "/") {
				if completed := completeCommand(current); completed != "" {
					console.taskField.SetValue(completed)
				}
			}
			return console, nil
		case "enter":
			task := strings.TrimSpace(console.taskField.Value())
			if task == "" {
				return console, nil
			}
			console.continuationRequest++
			if handled, cmd := console.runCommand(task); handled {
				console.taskField.Reset()
				return console, cmd
			}
			expanded, expandErr := expandTaskReferences(task, console.referenceWorkspace())
			if expandErr != nil {
				console.appendLine(errStyle.Render("✗ " + expandErr.Error()))
				return console, nil
			}
			if expanded != task {
				console.appendLine(dimStyle.Render(fmt.Sprintf("已展开 %d 处文件引用进任务文本", len(referencePattern.FindAllStringSubmatch(task, -1)))))
				task = expanded
			}
			console.mode = consoleRunning
			console.beginOperation()
			console.lastTask = task
			carryingPrior := false
			if console.sessionID == "" {
				console.sessionID = domain.NewID("session")
			} else if console.sessionPrev != "" {
				carryingPrior = true
			}
			console.resetRun()
			if carryingPrior {
				console.appendLine(dimStyle.Render("已携带上一任务前情（/new 可断开会话）"))
			}
			console.appendLine(okStyle.Render("▶ 任务已提交，运行中（Ctrl+C 取消）"))
			return console, startRunCmd(console, task)
		default:
			var cmd tea.Cmd
			console.taskField, cmd = console.taskField.Update(message)
			return console, cmd
		}
	case consoleRunning:
		switch message.String() {
		case "ctrl+c", "esc":
			console.appendLine(warnStyle.Render("⏸ 已请求取消，等待运行持久化终态…"))
			console.cancelOperation()
			return console, nil
		case "enter":
			if !console.queuing {
				return console, nil
			}
			if text := strings.TrimSpace(console.queueField.Value()); text != "" {
				console.queue = append(console.queue, text)
				console.appendLine(dimStyle.Render(fmt.Sprintf("✎ 已排队 %d 条（运行结束后 Enter 逐条预填）", len(console.queue))))
			}
			console.queueField.Reset()
			return console, nil
		default:
			if !console.queuing {
				console.queuing = true
			}
			var cmd tea.Cmd
			console.queueField, cmd = console.queueField.Update(message)
			return console, cmd
		}
	case consoleApproval:
		switch message.String() {
		case "enter":
			console.beginOperation()
			console.mode = consoleRunning
			console.appendLine(okStyle.Render("▶ 已批准，继续运行"))
			return console, resolveApprovalCmd(console, true)
		case "esc", "d":
			console.beginOperation()
			console.mode = consoleRunning
			console.appendLine(warnStyle.Render("▶ 已拒绝，运行将按拒绝终态收束"))
			return console, resolveApprovalCmd(console, false)
		case "v":
			return console.openApprovalDiff()
		}
		return console, nil
	case consoleModelWizard:
		return console.relayWizardKey(message)
	case consoleDone:
		switch message.String() {
		case "ctrl+c":
			console.rootCancel()
			return console, tea.Quit
		case "enter":
			console.mode = consoleInput
			console.taskField.Reset()
			console.taskField.Focus()
			if len(console.queue) > 0 {
				console.taskField.SetValue(console.queue[0])
				console.queue = console.queue[1:]
			}
			return console, nil
		case "tab":
			if len(console.queue) > 0 {
				console.queue = console.queue[1:]
				console.appendLine(dimStyle.Render("已丢弃队首排队输入"))
			}
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
	console.finishOperation()
	if errors.As(finished.err, &waiting) {
		console.mode = consoleApproval
		console.waiting = waiting
		console.approvalDetail = console.loadApprovalDetail(waiting)
		console.appendLine(warnStyle.Render("⏸ 等待审批: " + waiting.Summary))
		console.appendLine(dimStyle.Render("  指纹 " + waiting.Fingerprint + " · Enter 批准 / Esc 拒绝 / v 差异"))
		return console, noMessageCmd
	}
	console.mode = consoleDone
	console.result = finished.result
	console.runErr = finished.err
	console.stopWait = true
	if console.sessionID != "" && finished.result != nil && finished.result.ID != "" {
		console.sessionPrev = finished.result.ID
		console.sessionRuns = append(console.sessionRuns, sessionRunEntry{id: finished.result.ID, verdict: console.verdictText(), summary: firstLineClip(console.lastTask, 48)})
	}
	console.appendLine(console.verdictLine())
	if console.result != nil && console.result.ID != "" {
		console.appendLine(dimStyle.Render("run " + string(console.result.ID) + "（cairn inspect 查证）"))
	}
	return console, nil
}

// receiveRewindResult 消费一次工作区回退（或其审批续跑）的结果：等待审批转
// 入审批模式；终态展示回退结论。回退 run 不进入会话对话链，对话前情不变。
func (console RunConsole) receiveRewindResult(finished rewindFinishedMsg) (tea.Model, tea.Cmd) {
	var waiting *agent.WaitingApprovalError
	console.finishOperation()
	if errors.As(finished.err, &waiting) {
		console.mode = consoleApproval
		console.waiting = waiting
		console.approvalDetail = console.loadApprovalDetail(waiting)
		console.appendLine(warnStyle.Render("⏸ 回退等待审批: " + waiting.Summary))
		console.appendLine(dimStyle.Render("  指纹 " + waiting.Fingerprint + " · Enter 批准 / Esc 拒绝（拒绝即中止回退）/ v 差异"))
		return console, noMessageCmd
	}
	console.mode = consoleDone
	console.result = finished.result
	console.runErr = finished.err
	console.stopWait = true
	applied := 0
	if console.rewinding != nil {
		applied = console.rewinding.applied
	}
	console.rewinding = nil
	if finished.err != nil {
		console.appendLine(errStyle.Render(fmt.Sprintf("● 工作区回退中止: 界面已观察到 %d 次补丁操作成功；实际断点以事件日志为准: %v", applied, finished.err)))
		console.appendLine(dimStyle.Render("整批回退非原子；请 inspect 回退 run 查证，悬挂操作按提示使用 resume --resolve 对账"))
	} else {
		console.appendLine(okStyle.Render(fmt.Sprintf("● 工作区回退完成（界面已观察到反放 %d 个补丁操作；逐项持久化；对话链与前情未变）", applied)))
	}
	if console.result != nil && console.result.ID != "" {
		console.appendLine(dimStyle.Render("回退 run " + string(console.result.ID) + "（cairn inspect 查证）"))
	}
	return console, nil
}

// beginOperation 为一次 Run、Resume 或 Rewind 创建独立取消域；调用方必须
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

// cancelOperation 只取消当前运行操作，不终止控制台及历史、向导等后续能力。
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
	console.waiting = nil
	console.result = nil
	console.runErr = nil
	console.rewinding = nil
	console.diffLines = nil
	console.diffCursor = 0
	console.diffReturn = consoleInput
	console.stopWait = false
}

// relayWizardKey 把按键转发给嵌入的配置向导并桥接其命令：非退出命令（如
// 光标闪烁）透传给程序驱动，退出命令翻译为向导关闭消息，由主界面消费。
func (console RunConsole) relayWizardKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	return console.relayWizardMsg(message)
}

// relayWizardMsg 把任意消息（按键、spinner 节拍、输入框闪烁等）转发给嵌
// 入向导并桥接其命令；consoleEventMsg 属于主界面运行流不进入向导。
func (console RunConsole) relayWizardMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	if console.wizard == nil {
		console.mode = consoleInput
		return console, nil
	}
	model, cmd := console.wizard.Update(msg)
	wizard, ok := model.(ConfigWizard)
	if !ok {
		console.mode = consoleInput
		return console, nil
	}
	console.wizard = &wizard
	if cmd == nil {
		return console, nil
	}
	return console, relayWizardCmd(cmd, console.wizardGeneration)
}

// relayWizardCmd 递归包装嵌入向导命令及 Batch 子命令，使所有异步结果携带
// 创建它们的向导实例世代；nil 结果保持 nil，由 Bubble Tea 忽略。
func relayWizardCmd(cmd tea.Cmd, generation uint64) tea.Cmd {
	return func() tea.Msg {
		message := cmd()
		if message == nil {
			return nil
		}
		if batch, ok := message.(tea.BatchMsg); ok {
			wrapped := make(tea.BatchMsg, 0, len(batch))
			for _, child := range batch {
				wrapped = append(wrapped, relayWizardCmd(child, generation))
			}
			return wrapped
		}
		return wizardRelayMsg{generation: generation, message: message}
	}
}

// exitWizard 退出配置向导回到输入态：提交成功时经调用方回调热重载配置
// 使后续任务立即生效，重载失败如实提示（新配置对下次进程仍生效）；取消
// 或失败时不改变现有配置。
func (console *RunConsole) exitWizard() {
	console.mode = consoleInput
	submitted := console.wizard != nil && console.wizard.outcome != nil
	console.wizard = nil
	console.lines = nil
	if submitted && console.input.ReloadConfig != nil {
		if reloaded, err := console.input.ReloadConfig(); err == nil {
			console.input.Config = reloaded
			console.appendLine(okStyle.Render("配置已更新，后续任务即用新模型"))
			return
		} else {
			console.appendLine(warnStyle.Render("配置已写入但当前界面重载失败: " + err.Error()))
			return
		}
	}
	if submitted {
		console.appendLine(dimStyle.Render("配置已写入，重启或下次运行生效"))
		return
	}
	console.appendLine(dimStyle.Render("已退出配置向导（配置未修改）"))
}

// applyEvent 把已持久化事件追加为摘要行并累计运行统计。
func (console *RunConsole) applyEvent(event domain.Event) {
	console.transcript = append(console.transcript, event)
	if len(console.transcript) > transcriptEventLimit {
		console.transcript = console.transcript[len(console.transcript)-transcriptEventLimit:]
	}
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
	case domain.EventOperationSucceeded:
		if console.rewinding != nil {
			var payload struct {
				ToolName string `json:"tool_name"`
			}
			if eventPayload(event, &payload) && payload.ToolName == "apply_patch" {
				console.rewinding.applied++
			}
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

// loadApprovalDetail 在审批暂停时从事件日志提取最近一次工具提议的参数预
// 览：apply_patch 展示写入模式、目标路径与内容/替换摘要，exec 展示 argv，
// 其余工具截断参数原文。预览是已持久化事实的只读投影，不可用时如实标注
// 而不阻塞审批。
func (console RunConsole) loadApprovalDetail(waiting *agent.WaitingApprovalError) []string {
	workspace := console.input.Workspace
	if workspace == "" {
		if current, err := os.Getwd(); err == nil {
			workspace = current
		}
	}
	inspected, err := console.input.Service.Inspect(console.rootCtx, workspace, "", console.input.Config, waiting.RunID, true)
	if err != nil {
		return []string{dimStyle.Render("（参数预览不可用: " + err.Error() + "）")}
	}
	return approvalProposalPreview(inspected.Events)
}

// approvalProposalPreview 从事件流尾部提取最近一次工具提议的参数预览行；
// 无提议事件时返回空切片。
func approvalProposalPreview(events []domain.Event) []string {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload struct {
			ToolName  string          `json:"tool_name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if !eventPayload(event, &payload) {
			return nil
		}
		return proposalLines(payload.ToolName, payload.Arguments)
	}
	return nil
}

// openApprovalDiff 在审批态打开待审批补丁的差异视图；差异不可用时就地提示
// 并留在审批态，不阻塞审批决定。
func (console RunConsole) openApprovalDiff() (tea.Model, tea.Cmd) {
	lines, err := console.patchApprovalDiff()
	if err != nil {
		console.appendLine(warnStyle.Render("差异不可用: " + err.Error()))
		return console, nil
	}
	console.diffLines = lines
	console.diffCursor = 0
	console.diffReturn = consoleApproval
	console.mode = consoleDiff
	return console, nil
}

// patchApprovalDiff 现算待审批补丁的前后全文：审批发生在快照生成之前，磁盘
// 现内容即 before，由 ApplyPatch.Plan 以零写入方式给出确定性改写计划。
func (console RunConsole) patchApprovalDiff() ([]string, error) {
	if console.waiting == nil {
		return nil, errors.New("当前无待审批操作")
	}
	inspected, err := console.input.Service.Inspect(console.rootCtx, workspaceRootOrDefault(console.input.Workspace), "", console.input.Config, console.waiting.RunID, true)
	if err != nil {
		return nil, err
	}
	toolName, arguments, found := proposalArgumentsForOperation(inspected.Events, console.waiting.OperationID)
	if !found {
		return nil, errors.New("未找到待审批操作的提议参数")
	}
	if toolName != "apply_patch" {
		return nil, errors.New("该审批不是补丁写入，无差异可看")
	}
	workspace, err := console.previewWorkspace()
	if err != nil {
		return nil, err
	}
	plan, err := tools.ApplyPatch{Workspace: workspace}.Plan(arguments)
	if err != nil {
		return nil, err
	}
	return renderPlannedPatchDiff(plan, arguments), nil
}

// previewWorkspace 构造补丁差异预览用的工作区视图：与 Boot 装配同一 ignore
// 与读取上限配置来源，保证 Plan 的边界判定与策略侧一致。
func (console RunConsole) previewWorkspace() (tools.Workspace, error) {
	configuration := console.input.Config
	return tools.NewWorkspace(workspaceRootOrDefault(console.input.Workspace), configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
}

// proposalArgumentsForOperation 从事件流提取指定操作的工具提议身份与参数。
func proposalArgumentsForOperation(events []domain.Event, operationID domain.OperationID) (string, json.RawMessage, bool) {
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed || event.OperationID == nil || *event.OperationID != operationID {
			continue
		}
		var payload struct {
			ToolName  string          `json:"tool_name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return "", nil, false
		}
		return payload.ToolName, payload.Arguments, payload.ToolName != "" && len(payload.Arguments) != 0
	}
	return "", nil, false
}

// openTranscriptDiff 在事件查看器中打开光标所在补丁提议的差异视图。
func (console RunConsole) openTranscriptDiff() (tea.Model, tea.Cmd) {
	console.diffLines = console.transcriptPatchDiff()
	console.diffCursor = 0
	console.diffReturn = consoleTranscript
	console.mode = consoleDiff
	return console, nil
}

// transcriptPatchDiff 渲染光标所在补丁提议的差异：before 优先取同一操作
// operation_started 的 planned_before_ref 制品（只读解引用并校验哈希），缺省
// 时按参数如实降级——create 仍可完整渲染，update 只显示无定位替换块。
func (console RunConsole) transcriptPatchDiff() []string {
	if len(console.transcript) == 0 || console.transcriptCursor >= len(console.transcript) {
		return []string{warnStyle.Render("（无事件可看）")}
	}
	event := console.transcript[console.transcriptCursor]
	if event.Type != domain.EventToolCallProposed {
		return []string{warnStyle.Render("（光标行不是工具提议；v 仅对补丁提议有效）")}
	}
	toolName, arguments, found := "", json.RawMessage(nil), false
	if event.OperationID != nil {
		toolName, arguments, found = proposalArgumentsForOperation(console.transcript, *event.OperationID)
	}
	if !found {
		return []string{warnStyle.Render("（提议参数不可解码，无法生成差异）")}
	}
	if toolName != "apply_patch" {
		return []string{warnStyle.Render("（该提议是 " + toolName + "，无补丁差异）")}
	}
	before, beforeKnown := console.beforeSnapshotFor(event)
	lines := renderPatchDiff(arguments, before, beforeKnown)
	// 未生效补丁的差异是提议参数的改写推演而非既成事实，头部如实标注终态。
	if outcome, terminal := operationOutcomeFor(console.transcript, event); terminal && outcome != domain.EventOperationSucceeded && outcome != domain.EventOperationReconciled {
		note := "（该补丁未生效（" + string(outcome) + "），以下为提议参数的改写推演）"
		if len(lines) > 0 {
			lines = append([]string{lines[0], warnStyle.Render(note)}, lines[1:]...)
		} else {
			lines = []string{warnStyle.Render(note)}
		}
	}
	return lines
}

// operationOutcomeFor 在事件查看器内查找同一操作的终态事件；未到终态（等待
// 审批、悬挂）返回 terminal=false。
func operationOutcomeFor(events []domain.Event, proposal domain.Event) (domain.EventType, bool) {
	if proposal.OperationID == nil {
		return "", false
	}
	for _, event := range events {
		if event.OperationID == nil || *event.OperationID != *proposal.OperationID {
			continue
		}
		switch event.Type {
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			return event.Type, true
		}
	}
	return "", false
}

// beforeSnapshotFor 在事件查看器内查找同一操作 operation_started 的 before
// 快照制品并只读解引用；无制品（旧运行或脱敏跳过）或读取失败均如实返回不
// 可用。
func (console RunConsole) beforeSnapshotFor(proposal domain.Event) ([]byte, bool) {
	if proposal.OperationID == nil {
		return nil, false
	}
	for _, event := range console.transcript {
		if event.Type != domain.EventOperationStarted || event.OperationID == nil || *event.OperationID != *proposal.OperationID {
			continue
		}
		var payload struct {
			PlannedBeforeRef *artifact.Ref `json:"planned_before_ref"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.PlannedBeforeRef == nil {
			return nil, false
		}
		content, err := console.input.Service.ReadRunArtifact(workspaceRootOrDefault(console.input.Workspace), "", console.input.Config, *payload.PlannedBeforeRef)
		if err != nil {
			return nil, false
		}
		return content, true
	}
	return nil, false
}

// proposalLines 按工具类型把提议参数投影为预览行；内容预览截断到固定行数
// 与字符宽度，完整参数以事件日志与制品为准。
func proposalLines(toolName string, arguments json.RawMessage) []string {
	const previewLines = 8
	const previewWidth = 72
	switch toolName {
	case "apply_patch":
		var patch struct {
			Path         string `json:"path"`
			Mode         string `json:"mode"`
			ExpectedSHA  string `json:"expected_sha256"`
			Content      string `json:"content"`
			Replacements []struct {
				Old string `json:"old"`
				New string `json:"new"`
			} `json:"replacements"`
		}
		if !jsonPiece(arguments, &patch) {
			return []string{dimStyle.Render("（apply_patch 参数不可解码）")}
		}
		lines := []string{itemStyle.Render(fmt.Sprintf("  apply_patch %s · %s · 基线 %s", patch.Mode, patch.Path, shortDigest(patch.ExpectedSHA)))}
		if patch.Content != "" {
			lines = append(lines, dimStyle.Render("  将写入内容预览:"))
			lines = append(lines, previewText(patch.Content, previewLines, previewWidth)...)
		}
		for _, replacement := range patch.Replacements {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  替换: %s → %s", clip(replacement.Old, previewWidth), clip(replacement.New, previewWidth))))
		}
		return lines
	case "exec_command":
		var command struct {
			Argv []string `json:"argv"`
		}
		if !jsonPiece(arguments, &command) {
			return []string{dimStyle.Render("（exec_command 参数不可解码）")}
		}
		return []string{itemStyle.Render("  exec_command argv: " + strings.Join(command.Argv, " "))}
	default:
		if len(arguments) == 0 {
			return nil
		}
		return []string{itemStyle.Render("  " + clip(string(arguments), previewWidth))}
	}
}

// jsonPiece 解码提议参数片段；空参数返回 false。
func jsonPiece(raw json.RawMessage, target any) bool {
	if len(raw) == 0 {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

// previewText 把多行文本投影为带缩进的预览行并截断行数。
func previewText(text string, maxLines, width int) []string {
	rows := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(rows) > maxLines {
		rows = append(rows[:maxLines], fmt.Sprintf("  …（共 %d 行）", len(strings.Split(text, "\n"))))
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, dimStyle.Render("  | "+clip(row, width)))
	}
	return lines
}

// clip 截断单行文本到字符宽度。
func clip(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	return string(runes[:width]) + "…"
}

// shortDigest 缩短十六进制摘要便于展示。
func shortDigest(digest string) string {
	const digestPrefix = 12
	runes := []rune(digest)
	if len(runes) <= digestPrefix {
		return digest
	}
	return string(runes[:digestPrefix]) + "…"
}

// verdictText 返回不带样式的终态结论短语，供链记录使用。
func (console RunConsole) verdictText() string {
	if console.runErr != nil {
		return "失败"
	}
	if console.result == nil || console.result.Result == nil {
		return "结束（投影缺失）"
	}
	return runStateText(console.result.Result.State)
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

// renderTranscriptLine 把单条事件投影为查看器行：序号、类型与轮次归属，
// 只呈现结构事实不含 payload 全文。
func renderTranscriptLine(event domain.Event) string {
	return fmt.Sprintf("  %4d  %-28s turn=%d", event.Seq, string(event.Type), event.TurnID)
}

// maxTurnsBudget 从 Resolved Execution Plan 读取轮次预算；缺失或不可解析
// 返回 0 表示不展示上限。
func (console RunConsole) maxTurnsBudget() int {
	for _, entry := range console.input.Config.Resolution {
		if entry.Field == "budget.max_turns" {
			value, err := strconv.Atoi(entry.Value)
			if err == nil && value > 0 {
				return value
			}
			return 0
		}
	}
	return 0
}

// View 渲染主界面：输入态聚焦任务框，运行态滚动事件摘要与统计行，审批态
// 展示决定入口，终态展示判定与后续动作。
func (console RunConsole) View() string {
	var sections []string
	sections = append(sections, titleStyle.Render("Cairn")+dimStyle.Render("  单任务运行"))
	sections = append(sections, strings.Join(console.lines, "\n"))
	switch console.mode {
	case consoleInput:
		if len(console.lines) == 0 {
			sections = append(sections, console.welcomeBlock())
		}
		if current := strings.TrimSpace(console.taskField.Value()); strings.HasPrefix(current, "/") {
			if suggestions := commandSuggestions(current); len(suggestions) > 0 {
				sections = append(sections, "", strings.Join(suggestions, "\n"))
			} else {
				sections = append(sections, "", warnStyle.Render("没有匹配的命令（/help 查看全部）"))
			}
		}
		sections = append(sections, "", console.taskField.View(), dimStyle.Render("Enter 执行/提交 · Tab 补全命令 · Ctrl+J 换行 · Ctrl+O 事件 · Ctrl+C 退出"))
	case consoleRunning:
		if console.queuing {
			sections = append(sections, "", console.queueField.View(), dimStyle.Render("Enter 入队（运行结束后预填）· Ctrl+J 换行"))
		}
		sections = append(sections, "", console.statusLine())
	case consoleApproval:
		sections = append(sections, strings.Join(console.approvalDetail, "\n"))
		sections = append(sections, "", warnStyle.Render("Enter 批准 / Esc 拒绝 / v 差异"))
	case consoleModelWizard:
		if console.wizard != nil {
			sections = append(sections, console.wizard.View())
		}
	case consoleTranscript:
		sections = []string{titleStyle.Render("事件查看器") + dimStyle.Render(fmt.Sprintf("  %d 条已持久化事件 · ↑↓ 滚动 / v 补丁差异 / Esc 返回", len(console.transcript)))}
		end := console.transcriptCursor + transcriptWindowHeight
		if end > len(console.transcript) {
			end = len(console.transcript)
		}
		for index := console.transcriptCursor; index < end; index++ {
			sections = append(sections, renderTranscriptLine(console.transcript[index]))
		}
	case consoleDiff:
		sections = []string{titleStyle.Render("补丁差异") + dimStyle.Render(fmt.Sprintf("  %d 行 · ↑↓ 滚动 / Esc 返回", len(console.diffLines)))}
		end := console.diffCursor + diffWindowHeight
		if end > len(console.diffLines) {
			end = len(console.diffLines)
		}
		for index := console.diffCursor; index < end; index++ {
			sections = append(sections, "  "+console.diffLines[index])
		}
	case consoleDone:
		sections = append(sections, "")
		if console.runErr == nil && console.result != nil && console.result.Result != nil {
			if response := strings.TrimSpace(console.result.Response); response != "" {
				sections = append(sections, itemStyle.Render(finalResponsePreview(response)))
			}
		}
		queueHint := "Enter 新任务 / Ctrl+C 退出"
		if len(console.queue) > 0 {
			queueHint = fmt.Sprintf("Enter 预填排队任务（%d 条）· Tab 丢弃队首 / Ctrl+C 退出", len(console.queue))
		}
		sections = append(sections, dimStyle.Render(queueHint))
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...) + "\n"
}

// welcomeBlock 生成空态启动信息区：模型与端点、工作区、轮次预算与审计
// 提示——全部来自已加载配置的只读投影，让用户打开即知"我在用什么、在哪
// 干活、从哪查证"。
func (console RunConsole) welcomeBlock() string {
	turns := "未配置"
	if budget := console.maxTurnsBudget(); budget > 0 {
		turns = strconv.Itoa(budget)
	}
	model := console.input.Config.Provider.Model
	if model == "" {
		model = "未配置模型"
	}
	lines := []string{
		itemStyle.Render(fmt.Sprintf("模型 %s · %s", model, hostOf(console.input.Config.Provider.BaseURL))),
		dimStyle.Render(fmt.Sprintf("工作区 %s · 轮次预算 %s", console.statusWorkspace(), turns)),
		dimStyle.Render("运行全程事件可审计（cairn inspect <run-id> 查证）· 密钥仅环境与本机凭据文件"),
		"",
		dimStyle.Render("输入任务开始 · /help 查看命令"),
		"",
	}
	return strings.Join(lines, "\n")
}

// hostOf 从端点地址提取主机名用于展示；解析失败返回原串截断。
func hostOf(baseURL string) string {
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	if len(baseURL) > 40 {
		return baseURL[:40] + "…"
	}
	return baseURL
}

// consoleCommands 是主界面的命令清单：候选提示、Tab 补全与分发校验共用
// 本表，新增命令必须在此登记。
var consoleCommands = []struct {
	name        string
	description string
}{
	{"/help", "命令与按键清单"},
	{"/model", "模型配置向导（提交后热生效）"},
	{"/new", "新任务清屏（会话累计保留）"},
	{"/status", "模型、端点与预算"},
	{"/sessions", "当前会话链与工作区历史"},
	{"/history", "从历史会话唯一链尾继续"},
	{"/rewind", "回退会话对话链"},
	{"/rewind-files", "回退工作区补丁（撤销所选任务起）"},
	{"/cost", "本任务与会话累计 token"},
	{"/theme", "切换主题"},
	{"/editor", "外部编辑器编辑当前输入"},
	{"/quit", "退出"},
}

// commandSuggestions 按输入前缀匹配命令清单，返回展示行；空前缀返回全部。
func commandSuggestions(text string) []string {
	var lines []string
	for _, command := range consoleCommands {
		if text == "" || strings.HasPrefix(command.name, text) {
			lines = append(lines, itemStyle.Render(command.name)+dimStyle.Render("  "+command.description))
		}
	}
	return lines
}

// completeCommand 返回前缀匹配的补全结果：唯一匹配补全到完整命令，多
// 匹配补全到公共前缀（比输入长时），零匹配返回空串——与 readline 的
// Tab 行为一致。
func completeCommand(text string) string {
	if text == "" {
		return ""
	}
	common := ""
	count := 0
	for _, command := range consoleCommands {
		if !strings.HasPrefix(command.name, text) {
			continue
		}
		count++
		if count == 1 {
			common = command.name
		} else {
			common = commonPrefix(common, command.name)
		}
	}
	if count == 0 || len(common) <= len(text) {
		return ""
	}
	return common
}

// commonPrefix 返回两串的最长公共前缀。
func commonPrefix(left, right string) string {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return left[:index]
		}
	}
	return left[:limit]
}

// appendActiveSession 展示仅存在于当前进程的活跃链；该链仍是 /rewind 与
// /rewind-files 的唯一可操作范围。
func (console *RunConsole) appendActiveSession() {
	lines := []string{itemStyle.Render("当前进程会话链")}
	if console.sessionID == "" {
		lines = append(lines, dimStyle.Render("  无活跃会话（提交首个任务自动开链）"))
		console.appendLine(strings.Join(lines, "\n"))
		return
	}
	lines = append(lines, dimStyle.Render(fmt.Sprintf("  会话 %s · 已链 %d 个任务", console.sessionID, len(console.sessionRuns))))
	for index, entry := range console.sessionRuns {
		lines = append(lines, dimStyle.Render(fmt.Sprintf("  %d. %s · %s", index+1, entry.id, entry.verdict)))
	}
	console.appendLine(strings.Join(lines, "\n"))
}

// showSessionHistory 展示应用层完整回放后的当前工作区历史；历史记录保持只读，
// 不写回 sessionID、sessionPrev 或 sessionRuns，因而不会扩大续链或回退范围。
func (console *RunConsole) showSessionHistory(loaded sessionHistoryLoadedMsg) {
	lines := []string{itemStyle.Render("当前工作区持久化历史（只读）")}
	if loaded.err != nil {
		lines = append(lines, errStyle.Render("  读取失败: "+loaded.err.Error()))
		console.appendLine(strings.Join(lines, "\n"))
		return
	}
	if len(loaded.items) == 0 {
		lines = append(lines, dimStyle.Render("  暂无历史运行"))
		console.appendLine(strings.Join(lines, "\n"))
		return
	}
	for index, item := range loaded.items {
		session := item.SessionID
		if session == "" {
			session = "未分组"
		}
		task := firstLineClip(item.Task, 48)
		lines = append(lines, dimStyle.Render(fmt.Sprintf("  %d. %s · %s · 会话 %s · %s", index+1, item.ID, item.Phase, session, task)))
	}
	console.appendLine(strings.Join(lines, "\n"))
}

// runCommand 处理输入态的斜杠命令：大部分为纯界面操作，/sessions 异步调用
// 只读应用层历史能力；返回 handled=false 表示输入不是命令，按普通任务处理。
func (console *RunConsole) runCommand(task string) (bool, tea.Cmd) {
	if !strings.HasPrefix(task, "/") {
		return false, nil
	}
	name, remainder, _ := strings.Cut(strings.TrimSpace(task), " ")
	switch name {
	case "/help", "/?":
		console.appendLine(dimStyle.Render("命令: /help 帮助 · /model 模型配置 · /new 断链清屏 · /sessions 会话链 · /history <run-id> 历史续链 · /rewind 回退对话 · /rewind-files 回退工作区 · /status 配置与预算 · /cost 会话累计 · /editor 外部编辑 · /theme 主题 · /quit 退出"))
		console.appendLine(dimStyle.Render("按键: Enter 提交 · Ctrl+J 换行 · Ctrl+O 事件 · v 审批/事件差异 · Esc/Ctrl+C 取消 · 运行中输入即排队"))
	case "/new":
		console.sessionPromptTokens += console.promptTokens
		console.sessionOutputTokens += console.outputTokens
		console.lines = nil
		console.transcript = nil
		console.turn, console.toolCalls = 0, 0
		console.promptTokens, console.outputTokens = 0, 0
		if console.sessionID != "" {
			console.sessionID = ""
			console.sessionPrev = ""
			console.sessionRuns = nil
			console.sessionTailRequired = false
			console.appendLine(dimStyle.Render("已断开会话链并清屏（新任务不带前情；完整历史在事件日志）"))
			return true, nil
		}
		console.appendLine(dimStyle.Render("已清屏（会话累计保留，完整历史在事件日志）"))
	case "/status":
		turns := "未配置"
		if budget := console.maxTurnsBudget(); budget > 0 {
			turns = strconv.Itoa(budget)
		}
		console.appendLine(itemStyle.Render(fmt.Sprintf("模型 %s · 端点 %s", console.input.Config.Provider.Model, console.input.Config.Provider.BaseURL)))
		console.appendLine(dimStyle.Render(fmt.Sprintf("轮次预算 %s · 工作区 %s", turns, console.statusWorkspace())))
	case "/model":
		console.wizardGeneration++
		wizard := NewConfigWizard(console.rootCtx, console.nextSetupInput())
		console.wizard = &wizard
		console.mode = consoleModelWizard
		console.appendLine(dimStyle.Render("进入模型配置向导（Ctrl+C 取消返回）"))
		return true, relayWizardCmd(wizard.Init(), console.wizardGeneration)
	case "/sessions":
		console.appendActiveSession()
		console.appendLine(dimStyle.Render("正在读取当前工作区持久化历史…"))
		return true, loadSessionHistoryCmd(*console)
	case "/history":
		runID := domain.RunID(strings.TrimSpace(remainder))
		if runID == "" {
			console.appendLine(warnStyle.Render("用法: /history <run-id>（先用 /sessions 查看历史）"))
			return true, nil
		}
		console.appendLine(dimStyle.Render("正在验证历史会话链尾…"))
		return true, loadSessionContinuationCmd(*console, runID, console.continuationRequest)
	case "/editor":
		return true, console.openEditorCmd()
	case "/rewind":
		argument := strings.TrimSpace(remainder)
		if argument == "" {
			if console.sessionID == "" || len(console.sessionRuns) == 0 {
				console.appendLine(warnStyle.Render("当前无可回退的会话链"))
				return true, nil
			}
			console.appendLine(itemStyle.Render("会话链（/rewind <序号> 从该任务之后重来，仅回退对话不动工作区）:"))
			for index, entry := range console.sessionRuns {
				console.appendLine(dimStyle.Render(fmt.Sprintf("  %d. %s · %s · %s", index+1, entry.id, entry.verdict, entry.summary)))
			}
			return true, nil
		}
		index, convErr := strconv.Atoi(argument)
		if convErr != nil || index < 1 || index > len(console.sessionRuns) {
			console.appendLine(warnStyle.Render("序号越界（/rewind 列出可用节点）"))
			return true, nil
		}
		entry := console.sessionRuns[index-1]
		console.sessionPrev = entry.id
		console.sessionRuns = console.sessionRuns[:index]
		console.sessionTailRequired = false
		console.appendLine(okStyle.Render(fmt.Sprintf("已回退对话到第 %d 个任务之后（%s）；下一任务从该点前情继续，工作区未变动", index, entry.summary)))
		return true, nil
	case "/rewind-files":
		argument := strings.TrimSpace(remainder)
		if argument == "" {
			if console.sessionID == "" || len(console.sessionRuns) == 0 {
				console.appendLine(warnStyle.Render("当前无可回退的会话链"))
				return true, nil
			}
			console.appendLine(itemStyle.Render("会话链（/rewind-files <序号> 反放该任务起由 Cairn 记录且可恢复的补丁；整批非原子，逐项经策略判定并在必要时审批，不撤销命令或链外副作用）:"))
			for index, entry := range console.sessionRuns {
				console.appendLine(dimStyle.Render(fmt.Sprintf("  %d. %s · %s · %s", index+1, entry.id, entry.verdict, entry.summary)))
			}
			return true, nil
		}
		index, convErr := strconv.Atoi(argument)
		if convErr != nil || index < 1 || index > len(console.sessionRuns) {
			console.appendLine(warnStyle.Render("序号越界（/rewind-files 列出可用节点）"))
			return true, nil
		}
		runs := make([]domain.RunID, 0, len(console.sessionRuns)-index+1)
		for _, entry := range console.sessionRuns[index-1:] {
			runs = append(runs, entry.id)
		}
		console.mode = consoleRunning
		console.beginOperation()
		console.rewinding = &rewindState{}
		console.appendLine(okStyle.Render(fmt.Sprintf("▶ 工作区回退中：撤销第 %d 个任务起的 %d 个任务的补丁（Ctrl+C 取消；逐项经策略判定并在必要时审批；整批非原子）", index, len(runs))))
		return true, rewindFilesCmd(*console, runs)
	case "/theme":
		console.appendLine(dimStyle.Render(cycleTheme() + "（会话内生效）"))
		return true, nil
	case "/cost":
		console.appendLine(dimStyle.Render(fmt.Sprintf("本任务 in %d / out %d tok · 会话累计 in %d / out %d tok（金额以事件与制品为准）",
			console.promptTokens, console.outputTokens, console.sessionPromptTokens+console.promptTokens, console.sessionOutputTokens+console.outputTokens)))
	case "/quit", "/exit":
		console.rootCancel()
		return true, tea.Quit
	default:
		console.appendLine(warnStyle.Render("未知命令 " + name + "（/help 查看清单）" + remainder))
	}
	return true, nil
}

// referenceWorkspace 构造 @ 引用解析用的工作区视图（按输入工作区或当前目
// 录），构造失败返回零值工作区并由解析层对首个引用失败关闭。
func (console RunConsole) referenceWorkspace() tools.Workspace {
	workspace, err := workspaceForRoot(workspaceRootOrDefault(console.input.Workspace))
	if err != nil {
		return tools.Workspace{}
	}
	return workspace
}

// nextSetupInput 构造向导输入：写入目标跟随当前实际生效的配置位置（用
// 户级存在即用户级，否则工作区），避免改配置改到未生效的那份上。
func (console RunConsole) nextSetupInput() app.SetupInput {
	workspace := console.input.Workspace
	if workspace == "" {
		if current, err := os.Getwd(); err == nil {
			workspace = current
		}
	}
	input := app.SetupInput{
		Workspace: workspace, CatalogURL: "", CatalogCacheDir: "",
		Admission: console.input.Config.Provider.Admission,
	}
	if userPath, err := config.UserConfigPath(); err == nil {
		if resolved := config.ResolveDefaultPath(workspace); resolved == userPath {
			input.ScopeUserConfig = true
		}
	}
	return input
}

// editorClosedMsg 承载外部编辑器收束后的输入内容与执行错误。
type editorClosedMsg struct {
	content string
	err     error
}

// openEditorCmd 把当前输入送外部编辑器（VISUAL 优先于 EDITOR）：临时文件
// 写入、挂起界面执行编辑、恢复后读回内容并清理临时文件。未配置编辑器时
// 就地提示。
func (console RunConsole) openEditorCmd() tea.Cmd {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		console.appendLine(warnStyle.Render("未配置 VISUAL/EDITOR 环境变量，无法打开外部编辑器"))
		return nil
	}
	temporary, err := os.CreateTemp("", "cairn-task-*.md")
	if err != nil {
		console.appendLine(warnStyle.Render("创建编辑临时文件失败: " + err.Error()))
		return nil
	}
	path := temporary.Name()
	if _, err := temporary.WriteString(console.taskField.Value()); err != nil {
		temporary.Close()
		os.Remove(path)
		console.appendLine(warnStyle.Render("写入编辑临时文件失败: " + err.Error()))
		return nil
	}
	temporary.Close()
	return tea.ExecProcess(exec.Command(editor, path), func(err error) tea.Msg {
		content, readErr := os.ReadFile(path)
		os.Remove(path)
		if readErr != nil {
			return editorClosedMsg{err: readErr}
		}
		return editorClosedMsg{content: string(content), err: err}
	})
}

// firstLineClip 取文本首行并按字符截断，作为链节点的任务摘要。
func firstLineClip(text string, limit int) string {
	first := strings.SplitN(strings.TrimSpace(text), "\n", 2)[0]
	runes := []rune(first)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return first
}

// statusWorkspace 返回展示用工作区标识；空工作区显示当前目录语义。
func (console RunConsole) statusWorkspace() string {
	if console.input.Workspace == "" {
		return "当前目录"
	}
	return console.input.Workspace
}

// statusLine 生成运行中的统计行：轮次、工具调用与累计 token。
func (console RunConsole) statusLine() string {
	turns := fmt.Sprintf("轮次 %d", console.turn)
	if budget := console.maxTurnsBudget(); budget > 0 {
		turns = fmt.Sprintf("轮次 %d/%d", console.turn, budget)
	}
	hint := "Esc 取消 · 输入即排队 · Ctrl+O 事件"
	return dimStyle.Render(fmt.Sprintf("%s · 工具调用 %d · 累计 in %d / out %d tok · %s", turns, console.toolCalls, console.promptTokens, console.outputTokens, hint))
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
