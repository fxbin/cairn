// [INPUT]: 依赖 App 的持久任务、工作流和已提交 Run 事件。
// [OUTPUT]: 展示跨会话状态。转交明确选择的定义修订与审批身份。
// [POS]: 控制台薄入口。前台执行沿用运行取消与事件通道。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件接线跨会话工作负载命令。
// Author: fxbin
package tui

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

const workloadTitleWidth = 64

// workloadResultMsg 传递 App 视图。前台操作回执与对应 Run 事件按序进入通道。
type workloadResultMsg struct {
	generation  uint64
	foreground  bool
	task        *app.TaskView
	instances   []app.WorkflowStatusView
	definitions []app.WorkflowDefinition
	next        string
	err         error
}

// taskSlash 分派任务查询、执行和绑定所见修订的状态变更。
func (console *RunConsole) taskSlash(argument string) tea.Cmd {
	words := strings.Fields(argument)
	service := console.input.Service
	scope := app.TaskScope{Workspace: console.input.Workspace, StateDir: console.input.StateDir, Config: console.input.Config}
	if len(words) > 0 {
		var cmd tea.Cmd
		switch words[0] {
		case "create":
			cmd = console.createTaskSlash(scope, argument)
		case "progress", "pause", "resume", "cancel", "complete":
			cmd = console.changeTaskSlash(scope, argument)
		case "run":
		default:
			if len(words) == 1 {
				ctx := console.rootCtx
				return func() tea.Msg {
					view, err := service.GetTask(ctx, scope, app.TaskID(words[0]))
					return workloadResultMsg{task: &view, err: err}
				}
			}
		}
		if cmd != nil {
			return cmd
		}
	}
	if len(words) >= 5 && len(words) <= 6 && words[0] == "run" {
		revision, err := strconv.ParseUint(words[2], 10, 64)
		if err == nil && revision != 0 {
			id := app.TaskID(words[1])
			options := app.TaskRunOptions{Revision: revision, AssignmentID: words[3], SessionID: words[4]}
			if len(words) == 6 {
				options.PreviousRunID = domain.RunID(words[5])
			}
			return console.startWorkload("持久任务", func(ctx context.Context, onEvent app.EventSink) workloadResultMsg {
				options.OnEvent = onEvent
				view, err := service.RunTask(ctx, scope, id, options)
				return workloadResultMsg{task: &view, err: err}
			})
		}
	}
	console.appendLine(warnStyle.Render("查询 /task <任务ID>"))
	console.appendLine(dimStyle.Render("创建 /task create <目标> | <验收项> [| <其他验收项>]"))
	console.appendLine(dimStyle.Render("执行 /task run <任务ID> <修订> <分配ID> <会话ID> [前序RunID]"))
	console.appendLine(dimStyle.Render("进度 /task progress <任务ID> <修订> <摘要> [| <剩余事项> ...]（省略剩余事项即清空）"))
	console.appendLine(dimStyle.Render("控制 /task pause|resume|cancel <任务ID> <修订>"))
	console.appendLine(dimStyle.Render("验收 /task complete <任务ID> <修订> <可信RunID>"))
	return nil
}

// workflowSlash 读取定义和实例。执行类命令必须显式选择动作及身份。
func (console *RunConsole) workflowSlash(argument string) tea.Cmd {
	words := strings.Fields(argument)
	service := console.input.Service
	scope := app.WorkflowScope{Workspace: console.input.Workspace, StateDir: console.input.StateDir, Config: console.input.Config}
	root := console.rootCtx
	if len(words) == 0 || (len(words) == 1 && words[0] == "history") {
		return func() tea.Msg {
			views, err := service.WorkflowHistory(root, scope)
			return workloadResultMsg{instances: views, err: err}
		}
	}
	switch words[0] {
	case "definitions":
		if len(words) == 1 {
			return func() tea.Msg {
				definitions, err := service.ListWorkflowDefinitions(root, scope)
				return workloadResultMsg{definitions: definitions, err: err}
			}
		}
	case "status":
		if len(words) == 2 {
			return func() tea.Msg {
				view, err := service.WorkflowStatus(root, scope.Workspace, scope.StateDir, scope.Config, app.WorkflowID(words[1]))
				return workflowResult(app.WorkflowOutcome{View: view}, err)
			}
		}
	case "start":
		if len(words) == 3 {
			revision, err := strconv.ParseUint(words[2], 10, 64)
			if err == nil && revision != 0 {
				return console.startWorkload("工作流启动", func(ctx context.Context, onEvent app.EventSink) workloadResultMsg {
					return workflowResult(service.StartWorkflow(ctx, app.StartWorkflowOptions{
						Scope: scope, DefinitionID: app.WorkflowDefinitionID(words[1]), Revision: revision, OnEvent: onEvent,
					}))
				})
			}
		}
	case "resume":
		if len(words) == 2 || len(words) == 3 {
			var sequence uint64
			if len(words) == 3 {
				parsed, err := strconv.ParseUint(words[2], 10, 64)
				if err != nil || parsed == 0 {
					break
				}
				sequence = parsed
			}
			return console.startWorkload("工作流续跑", func(ctx context.Context, onEvent app.EventSink) workloadResultMsg {
				return workflowResult(service.WorkflowResume(ctx, app.WorkflowResumeOptions{
					WorkflowID: app.WorkflowID(words[1]), Workspace: scope.Workspace, StateDir: scope.StateDir, Config: scope.Config,
					Sequence: sequence, OnEvent: onEvent,
				}))
			})
		}
	case "approve", "reject", "approve-run", "reject-run":
		if cmd := console.workflowDecision(scope, words); cmd != nil {
			return cmd
		}
	case "pause", "continue", "cancel", "retry":
		if cmd := console.controlWorkflowSlash(scope, argument); cmd != nil {
			return cmd
		}
	case "reconcile":
		if cmd := console.reconcileWorkflowSlash(scope, argument); cmd != nil {
			return cmd
		}
	}
	console.appendLine(warnStyle.Render("用法 /workflow [history|definitions|status <实例ID>|start <定义ID> <修订>|resume <实例ID> [序号]]"))
	console.appendLine(dimStyle.Render("人工门 /workflow approve|reject <实例ID> <步骤ID> <序号> <原因>"))
	console.appendLine(dimStyle.Render("运行审批 /workflow approve-run|reject-run <实例ID> <步骤ID> <序号> <RunID> <操作ID> <指纹> <原因>"))
	console.appendLine(dimStyle.Render("控制 /workflow pause|continue|cancel <实例ID> <序号> <原因>"))
	console.appendLine(dimStyle.Render("返工 /workflow retry <实例ID> <起始步骤ID> <序号> <原因>"))
	console.appendLine(dimStyle.Render("对账 /workflow reconcile <实例ID> <步骤ID> <序号> <RunID> <操作ID> <判定> <原因> [| <证据>]"))
	return nil
}

// workflowDecision 转交看到的实例链头。运行审批另绑定运行、操作和指纹。
func (console *RunConsole) workflowDecision(scope app.WorkflowScope, words []string) tea.Cmd {
	runDecision := strings.HasSuffix(words[0], "-run")
	reasonIndex := 4
	if runDecision {
		reasonIndex = 7
	}
	if len(words) <= reasonIndex {
		return nil
	}
	seq, err := strconv.ParseUint(words[3], 10, 64)
	if err != nil || seq == 0 {
		return nil
	}
	options := app.WorkflowDecisionOptions{
		Scope: scope, WorkflowID: app.WorkflowID(words[1]), StepID: words[2],
		Sequence: seq, Reason: strings.Join(words[reasonIndex:], " "),
	}
	if runDecision {
		options.RunID = domain.RunID(words[4])
		options.OperationID = domain.OperationID(words[5])
		options.Fingerprint = words[6]
	}
	service := console.input.Service
	approve := strings.HasPrefix(words[0], "approve")
	return console.startWorkload("工作流审批", func(ctx context.Context, onEvent app.EventSink) workloadResultMsg {
		options.OnEvent = onEvent
		if approve {
			return workflowResult(service.ApproveStep(ctx, options))
		}
		return workflowResult(service.RejectStep(ctx, options))
	})
}

// startWorkload 复用前台取消域和 FIFO 事件通道。跨会话操作不推进当前对话链。
func (console *RunConsole) startWorkload(label string, perform func(context.Context, app.EventSink) workloadResultMsg) tea.Cmd {
	console.mode = consoleRunning
	console.beginOperation()
	console.resetRun()
	console.steering = nil
	console.lastTask = label
	console.appendLine(okStyle.Render("▶ " + label + "（Ctrl+C 请求取消）"))
	ctx, root := console.operationContext(), console.rootCtx
	generation, messages := console.operationGeneration, console.messages
	onEvent := operationEventSink(ctx, generation, messages)
	return func() tea.Msg {
		result := perform(ctx, onEvent)
		result.generation, result.foreground = generation, true
		sendOperationMessage(root, messages, result)
		return nil
	}
}

// workflowResult 保留已经创建的实例，即使推进返回等待审批或对账。
func workflowResult(outcome app.WorkflowOutcome, err error) workloadResultMsg {
	result := workloadResultMsg{next: outcome.NextAction, err: err}
	if outcome.View.WorkflowID != "" {
		result.instances = []app.WorkflowStatusView{outcome.View}
	}
	return result
}

// receiveWorkload 处理只读回执及本代前台操作结果。
func (console RunConsole) receiveWorkload(result workloadResultMsg) (tea.Model, tea.Cmd) {
	if result.foreground {
		if result.generation != console.operationGeneration {
			return console, waitForOperationMessage(console.rootCtx, console.messages)
		}
		console.finishOperation()
		if console.shutdownRequested {
			console.rootCancel()
			return console, tea.Quit
		}
		console.mode = consoleInput
	}
	if result.task != nil && result.task.ID != "" {
		console.appendTaskState(*result.task)
	}
	if result.instances != nil {
		if len(result.instances) == 0 {
			console.appendLine(dimStyle.Render("当前工作区没有工作流实例"))
		}
		for _, view := range result.instances {
			console.appendWorkflowState(view)
		}
	}
	if result.definitions != nil {
		if len(result.definitions) == 0 {
			console.appendLine(dimStyle.Render("当前工作区没有已保存的工作流定义"))
		}
		for _, definition := range result.definitions {
			console.appendLine(itemStyle.Render(fmt.Sprintf("%s · 修订 %d · %s", definition.ID, definition.Revision, firstLineClip(definition.Name, workloadTitleWidth))))
		}
	}
	if result.next != "" {
		console.appendLine(dimStyle.Render(result.next))
	}
	if result.err != nil {
		console.appendLine(warnStyle.Render(result.err.Error()))
		var waiting *ports.WaitingApprovalError
		if result.task != nil && errors.As(result.err, &waiting) {
			console.appendLine(dimStyle.Render(fmt.Sprintf("cairn resume %s --approve %s 或 --deny %s", waiting.RunID, waiting.Fingerprint, waiting.Fingerprint)))
		}
	}
	if result.foreground {
		focus := console.taskField.Focus()
		return console, tea.Batch(focus, waitForOperationMessage(console.rootCtx, console.messages))
	}
	return console, nil
}

// appendTaskState 展示任务修订及各分配的 Journal 投影。
func (console *RunConsole) appendTaskState(view app.TaskView) {
	console.appendLine(itemStyle.Render(fmt.Sprintf("任务 %s · 修订 %d · %s", view.ID, view.Revision, view.State)))
	console.appendLine(dimStyle.Render(firstLineClip(view.Definition.Goal, workloadTitleWidth)))
	if view.Progress.Summary != "" {
		console.appendLine(dimStyle.Render("进度 " + firstLineClip(view.Progress.Summary, workloadTitleWidth)))
	}
	for _, item := range view.Progress.Remaining {
		console.appendLine(dimStyle.Render("待办 " + firstLineClip(item, workloadTitleWidth)))
	}
	for _, assignment := range view.Assignments {
		phase := contract.RunPhaseLabel(assignment.Phase)
		if phase == contract.UnknownDisplayLabel {
			phase = assignment.Phase
		}
		console.appendLine(dimStyle.Render(fmt.Sprintf("分配 %s · Run %s · %s", assignment.ID, assignment.RunID, phase)))
	}
	if view.Completion != nil {
		console.appendLine(dimStyle.Render(fmt.Sprintf("验收依据 Run %s · 序号 %d", view.Completion.RunID, view.Completion.Sequence)))
	}
}

// appendWorkflowState 展示已持久化的阶段、步骤和可供明确复述的审批身份。
func (console *RunConsole) appendWorkflowState(view app.WorkflowStatusView) {
	console.appendLine(itemStyle.Render("工作流 " + view.WorkflowID))
	if view.Definition != nil {
		console.appendLine(dimStyle.Render(fmt.Sprintf("%s · 修订 %d · 序号 %d · %s", firstLineClip(view.DefinitionName, workloadTitleWidth), view.Definition.Revision, view.Sequence, view.Phase)))
	} else if view.Finished {
		console.appendLine(dimStyle.Render(contract.RunPhaseLabel(view.FinishState)))
	}
	for _, step := range view.Steps {
		console.appendLine(dimStyle.Render(fmt.Sprintf("步骤 %s · %s · %s", step.ID, step.Kind, step.State)))
	}
	if view.ResumeIndex < 0 || view.ResumeIndex >= len(view.Steps) {
		return
	}
	active := view.Steps[view.ResumeIndex]
	if pending := view.PendingReconcile; pending != nil {
		console.appendLine(warnStyle.Render(fmt.Sprintf("待对账 · Run %s · 操作 %s · %s", pending.RunID, pending.OperationID, pending.ToolName)))
		evidence := " [| <证据>]"
		if pending.SideEffectClass == string(contract.SideEffectProcess) {
			evidence = " | <证据>"
		}
		console.appendLine(dimStyle.Render(fmt.Sprintf("/workflow reconcile %s %s %d %s %s <判定> <原因>%s",
			view.WorkflowID, active.ID, view.Sequence, pending.RunID, pending.OperationID, evidence)))
	}
	if len(view.RetryTargets) != 0 {
		console.appendLine(dimStyle.Render("可返工步骤 " + strings.Join(view.RetryTargets, "、")))
		console.appendLine(dimStyle.Render(fmt.Sprintf("/workflow retry %s <起始步骤ID> %d <原因>", view.WorkflowID, view.Sequence)))
	}
	if view.Paused {
		console.appendLine(dimStyle.Render(fmt.Sprintf("/workflow continue %s %d <原因>", view.WorkflowID, view.Sequence)))
		return
	}
	if approval := view.PendingApproval; approval != nil {
		console.appendLine(warnStyle.Render("运行待审批 " + firstLineClip(approval.Summary, workloadTitleWidth)))
		console.appendLine(dimStyle.Render(fmt.Sprintf("/workflow approve-run|reject-run %s %s %d %s %s %s <原因>",
			view.WorkflowID, active.ID, view.Sequence, approval.RunID, approval.OperationID, approval.Fingerprint)))
	} else if view.Phase == "waiting_human" {
		console.appendLine(dimStyle.Render(fmt.Sprintf("/workflow approve|reject %s %s %d <原因>", view.WorkflowID, active.ID, view.Sequence)))
	}
}
