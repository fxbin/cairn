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
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textarea"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
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
	console.updateSteeringReceipt(event)
	switch event.Type {
	case domain.EventTurnOpened:
		console.turn++
		console.turnToolCalls = 0
		console.turnExecutedTools = 0
		console.turnPromptTokens = 0
		console.turnOutputTokens = 0
		console.turnOpenedAt = event.OccurredAt
	case domain.EventToolCallProposed:
		console.toolCalls++
		console.turnToolCalls++
	case domain.EventOperationStarted:
		console.turnExecutedTools++
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
			console.turnPromptTokens += int64(payload.Usage.PromptTokens)
			console.turnOutputTokens += int64(payload.Usage.CompletionTokens)
		}
	case domain.EventSubagentDelegated, domain.EventSubagentCompleted:
		console.populateSubagentView(event)
	case domain.EventTurnClosed:
		console.appendLine(console.turnSummaryLine(event))
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

// statusLine 生成运行中的统计行：轮次、工具调用、累计 token 与上下文水位。
func (console RunConsole) statusLine() string {
	turns := fmt.Sprintf("轮次 %d", console.turn)
	if budget := console.maxTurnsBudget(); budget > 0 {
		turns = fmt.Sprintf("轮次 %d/%d", console.turn, budget)
	}
	hint := "Esc 取消 · 输入即排队 · Ctrl+O 事件"
	line := fmt.Sprintf("%s · 工具调用 %d · 累计 in %d / out %d tok", turns, console.toolCalls, console.promptTokens, console.outputTokens)
	if watermark := console.contextWatermark(); watermark != "" {
		line += " · " + watermark
	}
	return dimStyle.Render(line + " · " + hint)
}

// appendContextAlerts 在事件流中插入上下文告警行。
func (console *RunConsole) appendContextAlerts() {
	for _, alert := range console.contextAlerts() {
		console.appendLine(alert)
	}
}

// contextAlerts 返回当前上下文用量触发的告警行（无则空）。
func (console RunConsole) contextAlerts() []string {
	limit := console.input.Config.Context.MaxPromptTokens
	if limit <= 0 {
		return nil
	}
	used := int(console.promptTokens)
	percent := used * 100 / limit
	switch {
	case percent >= 90:
		return []string{warnStyle.Render(fmt.Sprintf("⚠ 上下文已用 %d%%，即将触发自动压缩或预算上限", percent))}
	case percent >= 70:
		return []string{warnStyle.Render(fmt.Sprintf("! 上下文已用 %d%%，建议 /compact 查看检查点", percent))}
	}
	return nil
}

// contextWatermark 按 max_prompt_tokens 计算上下文用量百分比水位。
func (console RunConsole) contextWatermark() string {
	limit := console.input.Config.Context.MaxPromptTokens
	if limit <= 0 {
		return ""
	}
	used := int(console.promptTokens)
	percent := used * 100 / limit
	marker := ""
	switch {
	case percent >= 90:
		marker = "‼"
	case percent >= 70:
		marker = "!"
	}
	return fmt.Sprintf("上下文 %d%% %s", percent, marker)
}

// turnSummaryLine 生成回合收束执行摘要：结果、工具次数、token 与墙钟耗时。
// 耗时取本回合 turn_opened→turn_closed 的事件 OccurredAt 差值，缺失时为 0，
// 不推断未持久化的时间。
func (console RunConsole) turnSummaryLine(event domain.Event) string {
	var payload struct {
		Outcome string `json:"outcome"`
	}
	eventPayload(event, &payload)
	outcome := payload.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	duration := time.Duration(0)
	if !console.turnOpenedAt.IsZero() && !event.OccurredAt.IsZero() && !event.OccurredAt.Before(console.turnOpenedAt) {
		duration = event.OccurredAt.Sub(console.turnOpenedAt)
	}
	return dimStyle.Render(fmt.Sprintf("▸ 第 %d 轮收束(%s) · 工具 %d（提议）/ 执行 %d · in %d / out %d tok · %s",
		console.turn, outcome, console.turnToolCalls, console.turnExecutedTools, console.turnPromptTokens, console.turnOutputTokens,
		formatDuration(duration)))
}

// formatDuration 把回合耗时格式化为秒级可读文本，避免引入完整时钟依赖。
func formatDuration(duration time.Duration) string {
	milliseconds := duration.Milliseconds()
	if milliseconds < 1000 {
		return fmt.Sprintf("%dms", milliseconds)
	}
	return fmt.Sprintf("%.1fs", float64(milliseconds)/1000)
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
