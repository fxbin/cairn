// 队列、Steering 与会话树展示。
// Author: fxbin
package tui

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
)

func (console *RunConsole) queueCommand(remainder string) (bool, tea.Cmd) {
	sub, args, _ := strings.Cut(strings.TrimSpace(remainder), " ")
	args = strings.TrimSpace(args)
	service := console.input.Service
	ctx := console.rootCtx
	workspace := console.input.Workspace
	stateDir := console.input.StateDir
	configuration := console.input.Config
	switch sub {
	case "", "list":
		console.appendLine(dimStyle.Render("正在读取队列…"))
		return true, func() tea.Msg {
			items, err := service.ListQueue(ctx, workspace, stateDir, configuration)
			return queueListLoadedMsg{items: items, err: err}
		}
	case "clear":
		console.appendLine(dimStyle.Render("正在清空队列…"))
		return true, func() tea.Msg {
			items, listErr := service.ListQueue(ctx, workspace, stateDir, configuration)
			if listErr != nil {
				return queueListLoadedMsg{err: listErr}
			}
			var lastErr error
			cleared := 0
			for _, item := range items {
				if _, err := service.CancelQueueInput(ctx, app.QueueCancelOptions{
					Workspace: workspace,
					StateDir:  stateDir,
					Config:    configuration,
					InputID:   item.InputID,
					Reason:    "cleared by /queue clear",
				}); err == nil {
					cleared++
				} else {
					lastErr = err
				}
			}
			remaining, _ := service.ListQueue(ctx, workspace, stateDir, configuration)
			if lastErr != nil && cleared == 0 {
				return queueListLoadedMsg{items: remaining, err: lastErr}
			}
			console.appendLine(okStyle.Render(fmt.Sprintf("已清空 %d 个队列项", cleared)))
			return queueListLoadedMsg{items: remaining}
		}
	case "add":
		if args == "" {
			console.appendLine(warnStyle.Render("用法: /queue add <下一任务>（持久入队，确认后才启动）"))
			return true, nil
		}
		inputID := domain.NewID("queue")
		console.appendLine(dimStyle.Render("正在入队…"))
		return true, func() tea.Msg {
			item, err := service.SubmitQueueInput(ctx, app.QueueSubmitOptions{
				Workspace: workspace, StateDir: stateDir, Config: configuration,
				InputID: inputID, Content: args,
			})
			return queueActionDoneMsg{action: "add", item: item, err: err}
		}
	case "confirm":
		if args == "" {
			console.appendLine(warnStyle.Render("用法: /queue confirm <input-id>（确认后启动为新 run）"))
			return true, nil
		}
		console.beginOperation()
		console.mode = consoleRunning
		console.appendLine(okStyle.Render(fmt.Sprintf("▶ 队列任务 %s 启动中（Ctrl+C 取消）", args)))
		return true, queueConfirmRunCmd(*console, args)
	case "cancel":
		id, reason, _ := strings.Cut(args, " ")
		id = strings.TrimSpace(id)
		reason = strings.TrimSpace(reason)
		if id == "" {
			console.appendLine(warnStyle.Render("用法: /queue cancel <input-id> [原因]"))
			return true, nil
		}
		if reason == "" {
			reason = "user_request"
		}
		return true, func() tea.Msg {
			item, err := service.CancelQueueInput(ctx, app.QueueCancelOptions{
				Workspace: workspace, StateDir: stateDir, Config: configuration,
				InputID: id, Reason: reason,
			})
			return queueActionDoneMsg{action: "cancel", item: item, err: err}
		}
	default:
		console.appendLine(warnStyle.Render("用法: /queue [list] | /queue add <任务> | /queue confirm <id> | /queue cancel <id> [原因]"))
		return true, nil
	}
}

// showQueueList 展示只读队列投影；索引可重建，事实源是 Journal。
func (console *RunConsole) showQueueList(loaded queueListLoadedMsg) {
	lines := []string{itemStyle.Render("后续任务队列（Journal 投影）")}
	if loaded.err != nil {
		lines = append(lines, errStyle.Render("  读取失败: "+loaded.err.Error()))
		console.appendLine(strings.Join(lines, "\n"))
		return
	}
	if len(loaded.items) == 0 {
		lines = append(lines, dimStyle.Render("  队列为空（/queue add <任务> 入队）"))
		console.appendLine(strings.Join(lines, "\n"))
		return
	}
	for _, item := range loaded.items {
		detail := string(item.State)
		if item.TargetRunID != "" {
			detail += " → " + string(item.TargetRunID)
		}
		if item.Reason != "" {
			detail += "（" + item.Reason + "）"
		}
		lines = append(lines, dimStyle.Render(fmt.Sprintf("  %s · %s", item.InputID, detail)))
	}
	console.appendLine(strings.Join(lines, "\n"))
}

// queueConfirmRunCmd 把队列确认后的目标 run 接入正常运行生命周期：操作
// 上下文、事件泵、审批等待、取消与结果收束复用 startRunCmd 通道。
func queueConfirmRunCmd(console RunConsole, inputID string) tea.Cmd {
	service := console.input.Service
	runCtx := console.operationContext()
	generation := console.operationGeneration
	messages := console.messages
	onEvent := operationEventSink(runCtx, generation, messages)
	workspace := console.input.Workspace
	stateDir := console.input.StateDir
	configuration := console.input.Config
	steering := console.steering
	return func() tea.Msg {
		result, err := service.ConfirmQueueInputWithEvents(runCtx, app.QueueConfirmOptions{
			Workspace: workspace, StateDir: stateDir, Config: configuration, InputID: inputID,
		}, onEvent, steering)
		sendOperationMessage(console.rootCtx, messages, queueActionDoneMsg{action: "confirm", item: app.QueueItemView{InputID: inputID, State: domain.QueueItemLinked, TargetRunID: result.ID}, runID: result.ID, err: err})
		sendOperationMessage(console.rootCtx, messages, runFinishedMsg{generation: generation, result: &result, err: err})
		return nil
	}
}

// submitSteering 投递一次安全点 steering：/steer <文本> 补充当前任务指导。
// 界面立即显示「待接收」；Journal 持久化后由事件流推进为已接收/已生效。
func (console RunConsole) submitSteering(command string) (tea.Model, tea.Cmd) {
	if console.steering == nil {
		console.appendLine(warnStyle.Render("当前没有接受补充输入的运行"))
		return console, nil
	}
	_, remainder, _ := strings.Cut(command, " ")
	content := strings.TrimSpace(remainder)
	if content == "" {
		console.appendLine(warnStyle.Render("用法: /steer <补充指导>（区分：排队输入是下一任务）"))
		return console, nil
	}
	inputID := domain.NewID("steer")
	if err := console.steering.Submit(inputID, content); err != nil {
		console.appendLine(errStyle.Render("补充输入投递失败: " + err.Error()))
		return console, nil
	}
	console.steeringStatus = append(console.steeringStatus, steeringReceipt{id: inputID, state: "待接收"})
	console.appendLine(okStyle.Render(fmt.Sprintf("✎ 补充当前任务已投递（%s）· 状态: 待接收", inputID)))
	console.appendLine(dimStyle.Render("安全点消费后生效；不改变本 run 的冻结权限/预算"))
	return console, nil
}

// updateSteeringReceipt 按已持久化事件推进 steering 四态；只有 Journal 事实
// 才能推进为已接收/已生效/无法应用。
func (console *RunConsole) updateSteeringReceipt(event domain.Event) {
	var inputID, outcome string
	switch event.Type {
	case domain.EventSteeringInputSubmitted:
		var payload struct {
			InputID string `json:"input_id"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return
		}
		inputID = payload.InputID
		outcome = "已接收"
	case domain.EventSteeringInputConsumed:
		var payload struct {
			InputID string `json:"input_id"`
			Outcome string `json:"outcome"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return
		}
		inputID = payload.InputID
		if payload.Outcome == "applied" {
			outcome = "已生效"
		} else {
			outcome = "无法应用"
		}
	default:
		return
	}
	for index := range console.steeringStatus {
		if console.steeringStatus[index].id == inputID {
			console.steeringStatus[index].state = outcome
			return
		}
	}
	console.steeringStatus = append(console.steeringStatus, steeringReceipt{id: inputID, state: outcome})
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
		relation := ""
		if item.PrevRunID != "" {
			relation = " ← " + string(item.PrevRunID)
		}
		if item.ForkSource != nil {
			relation = " ⇢ 分支自 " + string(item.ForkSource.RunID)
		}
		task := firstLineClip(item.Task, 48)
		lines = append(lines, dimStyle.Render(fmt.Sprintf("  %d. %s · %s · 会话 %s · %s%s", index+1, item.ID, item.Phase, session, task, relation)))
	}
	console.appendLine(strings.Join(lines, "\n"))
}

// showSessionTree 展示只读会话树投影；跨 session 的 fork_source 以分支标记
// 呈现，同 session 分叉如实展示，不静默选支。
func (console *RunConsole) showSessionTree(loaded sessionTreeLoadedMsg) {
	lines := []string{itemStyle.Render("会话树（只读投影，事实源为事件日志）")}
	if loaded.err != nil {
		lines = append(lines, errStyle.Render("  读取失败: "+loaded.err.Error()))
		console.appendLine(strings.Join(lines, "\n"))
		return
	}
	if len(loaded.tree.Roots) == 0 {
		lines = append(lines, dimStyle.Render("  暂无会话"))
		console.appendLine(strings.Join(lines, "\n"))
		return
	}
	visited := make(map[domain.RunID]bool)
	for _, root := range loaded.tree.Roots {
		console.appendSessionTreeNode(&lines, loaded.tree, root, "  ", visited)
	}
	console.appendLine(strings.Join(lines, "\n"))
}

// appendSessionTreeNode 递归追加树节点行；visited 防止损坏图导致无限递归。
func (console *RunConsole) appendSessionTreeNode(lines *[]string, tree app.SessionTree, id domain.RunID, prefix string, visited map[domain.RunID]bool) {
	if visited[id] {
		*lines = append(*lines, dimStyle.Render(prefix+string(id)+" …（环）"))
		return
	}
	visited[id] = true
	node, exists := tree.Nodes[id]
	if !exists {
		*lines = append(*lines, warnStyle.Render(prefix+string(id)+" （缺失）"))
		return
	}
	marker := ""
	if node.ForkSource != nil {
		marker = fmt.Sprintf(" [分支自 %s]", node.ForkSource.RunID)
	}
	*lines = append(*lines, dimStyle.Render(fmt.Sprintf("%s%s · %s · %s%s", prefix, node.ID, node.Phase, firstLineClip(node.Task, 40), marker)))
	for _, child := range node.Children {
		console.appendSessionTreeNode(lines, tree, child, prefix+"  ", visited)
	}
}

// runCommand 处理输入态的斜杠命令：大部分为纯界面操作，/sessions 异步调用
// 只读应用层历史能力；返回 handled=false 表示输入不是命令，按普通任务处理。
func (console *RunConsole) runCommand(task string) (bool, tea.Cmd) {
	if !strings.HasPrefix(task, "/") || strings.HasPrefix(task, "/skill:") {
		return false, nil
	}
	name, remainder, _ := strings.Cut(strings.TrimSpace(task), " ")
	switch name {
	case "/help", "/?":
		// 命令清单来自 consoleCommands：/help 与候选提示、Tab 补全共用同一张表，
		// 不再手写第二份，避免新增命令后帮助里看不到。
		for _, line := range helpCommandLines() {
			console.appendLine(dimStyle.Render(line))
		}
		console.appendLine(dimStyle.Render("按键: Enter 提交 · Ctrl+J 换行 · Ctrl+O 事件 · v 审批/事件差异 · Esc/Ctrl+C 取消 · 运行中输入即排队 · 结束后 Ctrl+Q 转存持久队列"))
		console.appendLine(dimStyle.Render("运行中: /steer <文本> 补充当前任务（安全点生效）· 其余输入排队为下一任务"))
		console.appendLine(dimStyle.Render("队列: /queue add <任务> 持久入队 · /queue confirm <id> 确认启动 · /queue cancel <id> · /queue 列表"))
		console.appendLine(dimStyle.Render("技能: 启用 skills.enabled 后，以 $名称 或 /skill:名称 加任务说明点名；cairn skills 管理本地技能"))
	case "/new":
		console.sessionPromptTokens += console.promptTokens
		console.sessionOutputTokens += console.outputTokens
		console.lines = nil
		console.transcript = nil
		console.turn, console.toolCalls = 0, 0
		console.turnToolCalls, console.turnExecutedTools = 0, 0
		console.turnPromptTokens, console.turnOutputTokens = 0, 0
		console.turnOpenedAt = time.Time{}
		console.promptTokens, console.outputTokens = 0, 0
		console.forkRequest++
		if console.sessionID != "" || console.sessionForkSource != nil {
			console.sessionID = ""
			console.sessionPrev = ""
			console.sessionRuns = nil
			console.sessionTailRequired = false
			console.sessionForkSource = nil
			console.appendLine(dimStyle.Render("已断开会话链并清屏（新任务不带前情；完整历史在事件日志）"))
			return true, nil
		}
		console.appendLine(dimStyle.Render("已清屏（会话累计保留，完整历史在事件日志）"))
	case "/status":
		console.appendBatchPlan()
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
	case "/tree":
		console.appendLine(dimStyle.Render("正在读取会话树…"))
		return true, loadSessionTreeCmd(*console)
	case "/history":
		runID := domain.RunID(strings.TrimSpace(remainder))
		if runID == "" {
			console.appendLine(warnStyle.Render("用法: /history <run-id>（先用 /tree 或 /sessions 查看历史）"))
			return true, nil
		}
		console.appendLine(dimStyle.Render("正在验证历史会话链尾…"))
		return true, loadSessionContinuationCmd(*console, runID, console.continuationRequest)
	case "/fork":
		runID := domain.RunID(strings.TrimSpace(remainder))
		if runID == "" {
			console.appendLine(warnStyle.Render("用法: /fork <run-id>（先用 /tree 查看可分支的终态 run；选定后下一任务创建新会话分支）"))
			return true, nil
		}
		console.forkRequest++
		console.appendLine(dimStyle.Render("正在校验分支来源…"))
		return true, loadSessionForkCmd(*console, runID, console.forkRequest)
	case "/steer":
		if console.mode == consoleInput {
			console.appendLine(warnStyle.Render("/steer 在运行中使用：/steer <文本> 补充当前任务指导（区分排队输入=下一任务）"))
			return true, nil
		}
		return true, nil
	case "/queue":
		return console.queueCommand(remainder)
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
	case "/init":
		console.initProjectInstructionsLines(strings.TrimSpace(remainder))
		return true, nil
	case "/compact":
		console.appendCompactStatus()
	case "/clear":
		console.lines = nil
		console.appendLine(dimStyle.Render("已清屏（会话链、统计与事件日志保留）"))
	case "/context":
		console.appendContextBudget()
	case "/doctor":
		console.appendSafetyEvents()
		console.appendSecurityPolicy()
		console.appendHeartbeat()
		return true, console.doctorCmd()
	case "/export":
		console.exportTranscript(strings.TrimSpace(remainder))
	case "/plan":
		console.appendPlan()
	case "/review":
		console.appendReview()
	case "/diff":
		console.appendLine(dimStyle.Render("正在读取工作区变更…"))
		return true, console.diffCmd()
	case "/approvals":
		console.appendApprovals()
	case "/mention":
		console.appendMention()
	case "/config":
		console.appendConfig()
	case "/models":
		console.appendModels(strings.TrimSpace(remainder))
	case "/tools":
		console.appendTools()
	case "/undo":
		console.appendUndo(strings.TrimSpace(remainder))
	case "/search":
		console.appendSearch(strings.TrimSpace(remainder))
	case "/trust":
		console.appendTrust(strings.TrimSpace(remainder))
	case "/usage":
		if strings.TrimSpace(remainder) == "fix" {
			console.applyUsageFix()
		} else {
			console.appendUsage()
			console.appendUsagePhases()
		}
	case "/subagents":
		if strings.TrimSpace(remainder) != "" {
			console.appendSubagentDetail(strings.TrimSpace(remainder))
		} else {
			console.appendSubagents()
		}
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
