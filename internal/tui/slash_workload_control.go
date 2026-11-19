// [INPUT]: 依赖 App 任务生命周期、工作流控制和证据判定入口。
// [OUTPUT]: 将控制台的显式修订、链头与操作身份传给 App。
// [POS]: 命令适配层。任务状态、返工与对账由 App 判定。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件接线持久任务和工作流的控制命令。
// Author: fxbin
package tui

import (
	"context"
	"strconv"
	"strings"
	"unicode"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
)

// createTaskSlash 将目标与显式验收项交给任务门面。
func (console *RunConsole) createTaskSlash(scope app.TaskScope, argument string) tea.Cmd {
	_, text := splitWorkloadPrefix(argument, 1)
	fields := workloadTextFields(text)
	if len(fields) < 2 || fields[0] == "" || !nonEmptyWorkloadItems(fields[1:]) {
		return nil
	}
	service := console.input.Service
	definition := app.TaskDefinition{Goal: fields[0], Acceptance: fields[1:]}
	return console.startWorkload("创建持久任务", func(ctx context.Context, _ app.EventSink) workloadResultMsg {
		task, err := service.CreateTask(ctx, scope, definition)
		return workloadResultMsg{task: &app.TaskView{Task: task}, err: err}
	})
}

// changeTaskSlash 为每次变更传入用户所见修订，验收另指定 Run。
func (console *RunConsole) changeTaskSlash(scope app.TaskScope, argument string) tea.Cmd {
	prefix, text := splitWorkloadPrefix(argument, 3)
	if len(prefix) != 3 {
		return nil
	}
	revision, err := strconv.ParseUint(prefix[2], 10, 64)
	if err != nil || revision == 0 {
		return nil
	}
	action, id := prefix[0], app.TaskID(prefix[1])
	var progress app.TaskProgress
	var runID domain.RunID
	switch action {
	case "progress":
		fields := workloadTextFields(text)
		if !nonEmptyWorkloadItems(fields[1:]) {
			return nil
		}
		progress = app.TaskProgress{Summary: fields[0], Remaining: fields[1:]}
	case "complete":
		words := strings.Fields(text)
		if len(words) != 1 {
			return nil
		}
		runID = domain.RunID(words[0])
	default:
		if text != "" {
			return nil
		}
	}
	service := console.input.Service
	return console.startWorkload("更新持久任务", func(ctx context.Context, _ app.EventSink) workloadResultMsg {
		var result app.Task
		var err error
		switch action {
		case "progress":
			result, err = service.UpdateTaskProgress(ctx, scope, id, revision, progress)
		case "pause":
			result, err = service.PauseTask(ctx, scope, id, revision)
		case "resume":
			result, err = service.ResumeTask(ctx, scope, id, revision)
		case "cancel":
			result, err = service.CancelTask(ctx, scope, id, revision)
		case "complete":
			result, err = service.CompleteTask(ctx, scope, id, revision, runID)
		}
		return workloadResultMsg{task: &app.TaskView{Task: result}, err: err}
	})
}

// controlWorkflowSlash 将实例控制决定绑定链头，返工另绑定步骤。
func (console *RunConsole) controlWorkflowSlash(scope app.WorkflowScope, argument string) tea.Cmd {
	prefix, reason := splitWorkloadPrefix(argument, 3)
	if len(prefix) != 3 {
		return nil
	}
	action := prefix[0]
	var stepID string
	if action == "retry" {
		prefix, reason = splitWorkloadPrefix(argument, 4)
		if len(prefix) != 4 {
			return nil
		}
		stepID = prefix[2]
	}
	sequence, err := strconv.ParseUint(prefix[len(prefix)-1], 10, 64)
	if err != nil || sequence == 0 || reason == "" {
		return nil
	}
	options := app.WorkflowControlOptions{
		Scope: scope, WorkflowID: app.WorkflowID(prefix[1]), Sequence: sequence, Reason: reason,
	}
	service := console.input.Service
	return console.startWorkload("工作流控制", func(ctx context.Context, onEvent app.EventSink) workloadResultMsg {
		options.OnEvent = onEvent
		var outcome app.WorkflowOutcome
		var err error
		switch action {
		case "pause":
			outcome, err = service.PauseWorkflow(ctx, options)
		case "continue":
			outcome, err = service.ResumeWorkflow(ctx, options)
		case "cancel":
			outcome, err = service.CancelWorkflow(ctx, options)
		case "retry":
			outcome, err = service.RetryStep(ctx, app.WorkflowRetryOptions{WorkflowControlOptions: options, StepID: stepID})
		}
		return workflowResult(outcome, err)
	})
}

// reconcileWorkflowSlash 转交显式对账判定，绑定实例、步骤、Run 和操作。
func (console *RunConsole) reconcileWorkflowSlash(scope app.WorkflowScope, argument string) tea.Cmd {
	prefix, text := splitWorkloadPrefix(argument, 7)
	if len(prefix) != 7 {
		return nil
	}
	sequence, err := strconv.ParseUint(prefix[3], 10, 64)
	fields := workloadTextFields(text)
	if err != nil || sequence == 0 || fields[0] == "" || len(fields) > 2 {
		return nil
	}
	options := app.WorkflowReconcileOptions{
		WorkflowControlOptions: app.WorkflowControlOptions{
			Scope: scope, WorkflowID: app.WorkflowID(prefix[1]), Sequence: sequence, Reason: fields[0],
		},
		StepID: prefix[2], RunID: domain.RunID(prefix[4]), OperationID: domain.OperationID(prefix[5]), Verdict: prefix[6],
	}
	if len(fields) == 2 {
		options.Evidence = fields[1]
	}
	service := console.input.Service
	return console.startWorkload("工作流对账", func(ctx context.Context, onEvent app.EventSink) workloadResultMsg {
		options.OnEvent = onEvent
		return workflowResult(service.ReconcileWorkflowStep(ctx, options))
	})
}

// splitWorkloadPrefix 提取固定的识别字段。剩余文字保留内部空格与换行。
func splitWorkloadPrefix(argument string, count int) ([]string, string) {
	prefix := make([]string, 0, count)
	text := strings.TrimSpace(argument)
	for len(prefix) < count && text != "" {
		next := strings.IndexFunc(text, unicode.IsSpace)
		if next < 0 {
			return append(prefix, text), ""
		}
		prefix = append(prefix, text[:next])
		text = strings.TrimSpace(text[next:])
	}
	return prefix, text
}

// workloadTextFields 裁剪分隔项的边缘空白。空首项保留给进度摘要。
func workloadTextFields(text string) []string {
	fields := strings.Split(text, "|")
	for index, field := range fields {
		fields[index] = strings.TrimSpace(field)
	}
	return fields
}

// nonEmptyWorkloadItems 检查用户明确提供的验收项和待办项。
func nonEmptyWorkloadItems(items []string) bool {
	for _, item := range items {
		if item == "" {
			return false
		}
	}
	return true
}
