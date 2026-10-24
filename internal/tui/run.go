// run.go 实现单任务运行闭环的终端主界面：任务输入、已持久化事件的实时滚
// 动摘要、在线审批（等待审批错误 → 用户决定 → 恢复）、取消与终态展示。
// 本文件只做展示与输入编排——运行、审批、取消与恢复全部复用 app.Service
// 的既有合同，事件摘要只消费已持久化事实，界面退出不改变任何事件日志。
//
// Author: fxbin
package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
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
	StateDir  string
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

// sessionTreeLoadedMsg 承载只读会话树投影；只驱动展示与分支选择，不写工作区。
type sessionTreeLoadedMsg struct {
	tree app.SessionTree
	err  error
}

// sessionForkLoadedMsg 承载分支来源校验结果；成功时只记录来源锚点，首次
// 新任务才创建分支，不在选择阶段留下幽灵运行。
type sessionForkLoadedMsg struct {
	anchor    app.SessionForkAnchor
	sourceID  domain.RunID
	requestID uint64
	err       error
}

// sessionRunEntry 是会话链内的单个运行记录：运行标识与终态结论，供
// /sessions 展示当前链。
type sessionRunEntry struct {
	id      domain.RunID
	verdict string
	summary string
}

// steeringReceipt 跟踪一次安全点 steering 的界面回执状态。
type steeringReceipt struct {
	id    string
	state string // 待接收 / 已接收 / 已生效 / 无法应用
}

// queueListLoadedMsg 承载只读队列投影结果。
type queueListLoadedMsg struct {
	items []app.QueueItemView
	err   error
}

// queueActionDoneMsg 承载入队/确认/取消的结果回执。
type queueActionDoneMsg struct {
	action    string
	item      app.QueueItemView
	runID     domain.RunID
	message   string
	remaining []string
	err       error
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
	// diffRequest 隔离异步工作区状态查询的新旧回执。
	diffRequest         uint64
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
	// lastBatchPlan 是最近一次模型工具批次的并行诊断投影。
	lastBatchPlan *agent.BatchPlan
	// subagentViews 是子运行展示投影。
	subagentViews []SubagentView
	// phaseTracker 记录活跃/等待用户阶段。
	phaseTracker *agent.UIPhaseTracker
	promptTokens int64
	outputTokens int64
	toolCalls    int
	// 回合级执行统计：turn_closed 摘要行展示本轮工具次数与 token，耗时取事件时间差。
	turnToolCalls     int
	turnExecutedTools int
	turnPromptTokens  int64
	turnOutputTokens  int64
	turnOpenedAt      time.Time
	queuePersisting   bool

	sessionPromptTokens int64
	sessionOutputTokens int64
	// 会话链状态（ADR-0018）：首个任务生成会话标识开链，后续任务自动以
	// 前一运行为前置续链；/new 断链后回到无前情状态。
	sessionID           string
	sessionPrev         domain.RunID
	sessionRuns         []sessionRunEntry
	sessionTailRequired bool
	continuationRequest uint64
	// sessionForkSource 是已选定的跨会话分支来源锚点；下一任务创建新会话
	// 根运行，与 sessionPrev 互斥。选择本身不创建运行。
	sessionForkSource *domain.SessionForkSource
	// steering 是安全点 steering 投递桥；运行期间 /steer 经此投递补充指导。
	steering *agent.SteeringBridge
	// steeringStatus 跟踪本进程已投递 steering 的四态回执。
	steeringStatus   []steeringReceipt
	forkRequest      uint64
	lastTask         string
	wizard           *ConfigWizard
	wizardGeneration uint64
	waiting          *ports.WaitingApprovalError
	approvalDetail   []string
	result           *app.RunResult
	runErr           error
	rewinding        *rewindState
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
		console.sessionForkSource = nil
		console.appendLine(okStyle.Render(fmt.Sprintf("已选择历史会话 %s 的链尾 %s；下一任务将携带其前情", console.sessionID, console.sessionPrev)))
		console.appendLine(dimStyle.Render("历史运行保持只读，不进入 /rewind 或 /rewind-files 范围"))
		console.forkRequest++
		return console, nil
	}
	if loaded, isTree := msg.(sessionTreeLoadedMsg); isTree {
		console.showSessionTree(loaded)
		return console, nil
	}
	if loaded, isFork := msg.(sessionForkLoadedMsg); isFork {
		if loaded.requestID != console.forkRequest || console.mode != consoleInput {
			return console, nil
		}
		if loaded.err != nil {
			console.appendLine(errStyle.Render("分支来源校验失败: " + loaded.err.Error()))
			return console, nil
		}
		source := loaded.anchor.Source
		console.sessionForkSource = &source
		console.sessionID = domain.NewID("session")
		console.sessionPrev = ""
		console.sessionRuns = nil
		console.sessionTailRequired = false
		console.appendLine(okStyle.Render(fmt.Sprintf("已选择分支来源 %s（会话 %s）；下一任务将创建新会话分支", source.RunID, loaded.anchor.SourceSessionID)))
		console.appendLine(dimStyle.Render("分支不继承历史审批；选择时不修改文件；/rewind-files 不作用于历史分支"))
		return console, nil
	}
	if loaded, isQueueList := msg.(queueListLoadedMsg); isQueueList {
		console.showQueueList(loaded)
		return console, nil
	}
	if done, isQueueAction := msg.(queueActionDoneMsg); isQueueAction {
		if done.action == "persisted" {
			console.queuePersisting = false
			console.queue = append(done.remaining, console.queue...)
		}
		if done.err != nil {
			console.appendLine(errStyle.Render("队列操作失败: " + done.err.Error()))
			return console, nil
		}
		switch done.action {
		case "add":
			console.appendLine(okStyle.Render(fmt.Sprintf("已入队 %s（待确认；/queue confirm %s 启动）", done.item.InputID, done.item.InputID)))
		case "confirm":
			console.appendLine(okStyle.Render(fmt.Sprintf("已确认启动 %s → run %s", done.item.InputID, done.runID)))
		case "cancel":
			console.appendLine(okStyle.Render(fmt.Sprintf("已取消 %s（%s）", done.item.InputID, done.item.Reason)))
		case "persisted":
			console.appendLine(okStyle.Render("已转存到持久队列（/queue 查看；/queue confirm 启动）"))
		}
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
		case consoleEventMsg, runFinishedMsg, rewindFinishedMsg, gitStatusMsg:
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
	case gitStatusMsg:
		if typed.request != console.diffRequest || console.rootCtx.Err() != nil {
			return console, nil
		}
		if typed.err != nil {
			console.appendLine(warnStyle.Render(typed.err.Error()))
		} else {
			console.appendLine(dimStyle.Render(strings.Join(typed.lines, "\n")))
		}
		return console, nil
	case doctorDoneMsg:
		for _, line := range typed.lines {
			console.appendLine(line)
		}
		return console, nil
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
			console.forkRequest++
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
			// 安全点 steering：每次运行独立投递桥，运行结束后可继续观察回执。
			console.steering = agent.NewSteeringBridge()
			console.steeringStatus = nil
			carryingPrior := false
			carryingFork := console.sessionForkSource != nil
			if console.sessionID == "" {
				console.sessionID = domain.NewID("session")
			} else if console.sessionPrev != "" {
				carryingPrior = true
			}
			console.resetRun()
			if carryingFork {
				console.appendLine(dimStyle.Render("已选择分支来源，本任务将创建新会话分支（/new 可取消）"))
			} else if carryingPrior {
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
			text := strings.TrimSpace(console.queueField.Value())
			console.queueField.Reset()
			if text == "" {
				return console, nil
			}
			if text == "/steer" || strings.HasPrefix(text, "/steer ") {
				return console.submitSteering(text)
			}
			if !console.queuing {
				return console, nil
			}
			console.queue = append(console.queue, text)
			console.appendLine(dimStyle.Render(fmt.Sprintf("✎ 已排队 %d 条（运行结束后 Enter 逐条预填）", len(console.queue))))
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
				console.appendLine(dimStyle.Render("已丢弃队首排队输入 · Ctrl+Q 转存持久队列"))
			}
			return console, nil
		case "ctrl+q":
			if console.queuePersisting {
				console.appendLine(dimStyle.Render("队列转存尚未完成"))
				return console, nil
			}
			// 运行结束后把内存排队一次性转为持久队列，避免只靠手动预填。
			if len(console.queue) == 0 {
				console.appendLine(dimStyle.Render("没有可转存的排队输入"))
				return console, nil
			}
			items := append([]string(nil), console.queue...)
			console.queue = nil
			console.queuePersisting = true
			console.appendLine(dimStyle.Render(fmt.Sprintf("正在转存 %d 条到持久队列…", len(items))))
			return console, persistQueueCmd(console, items)
		}
		return console, nil
	}
	return console, nil
}

// persistQueueCmd 把运行结束后仍留在内存的排队输入批量写入持久队列。
// 每条独立 input_id；确认前不自动启动为新 run。
func (console RunConsole) receiveResult(finished runFinishedMsg) (tea.Model, tea.Cmd) {
	var waiting *ports.WaitingApprovalError
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
		// 只有非空且精确匹配当前会话的运行才推进链尾；队列等无会话运行不得续链。
		if finished.result.SessionID != "" && finished.result.SessionID == console.sessionID {
			console.sessionPrev = finished.result.ID
			console.sessionRuns = append(console.sessionRuns, sessionRunEntry{id: finished.result.ID, verdict: console.verdictText(), summary: firstLineClip(console.lastTask, 48)})
			// 分支根运行创建后转为同会话续链；来源锚点只作用于首次新任务。
			console.sessionForkSource = nil
			console.sessionTailRequired = false
		}
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
	var waiting *ports.WaitingApprovalError
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

// relayWizardKey 把按键转发给嵌入的配置向导并桥接其命令：非退出命令（如
// 光标闪烁）透传给程序驱动，退出命令翻译为向导关闭消息，由主界面消费。
func RunConsoleLoop(ctx context.Context, runInput RunConsoleInput) error {
	program := tea.NewProgram(NewRunConsole(ctx, runInput))
	_, err := program.Run()
	return err
}
