// [INPUT]: 依赖 App 持久任务门面与桌面运行事件通道。
// [OUTPUT]: 提供任务创建、查询、显式变更和执行分配。
// [POS]: 任务版本与运行证据由 App 校验。桌面只映射请求。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接持久任务和桌面控件。
// Author: fxbin
package desktop

import (
	"context"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// TaskCreateRequest 携带交给 App 保存的目标和验收标准。Criteria 为可选的
// 结构化合同；缺省时按旧自由文本验收兼容。
type TaskCreateRequest struct {
	Workspace  string                         `json:"workspace"`
	StateDir   string                         `json:"state_dir,omitempty"`
	Goal       string                         `json:"goal"`
	Acceptance []string                       `json:"acceptance"`
	Criteria   []contract.AcceptanceCriterion `json:"criteria,omitempty"`
}

// TaskGetRequest 标识当前工作区内的持久任务。
type TaskGetRequest struct {
	Workspace string `json:"workspace"`
	StateDir  string `json:"state_dir,omitempty"`
	TaskID    string `json:"task_id"`
}

// TaskControlRequest 按客户端观察的修订提交一个显式动作。
type TaskControlRequest struct {
	Workspace     string   `json:"workspace"`
	StateDir      string   `json:"state_dir,omitempty"`
	TaskID        string   `json:"task_id"`
	Action        string   `json:"action"`
	Revision      uint64   `json:"revision"`
	Summary       string   `json:"summary,omitempty"`
	Remaining     []string `json:"remaining,omitempty"`
	RunID         string   `json:"run_id,omitempty"`
	AssignmentID  string   `json:"assignment_id,omitempty"`
	SessionID     string   `json:"session_id,omitempty"`
	PreviousRunID string   `json:"previous_run_id,omitempty"`
}

// TaskControlAck 返回变更后的任务或具有 Run 投影的任务视图。
type TaskControlAck struct {
	Task  *app.Task     `json:"task,omitempty"`
	View  *app.TaskView `json:"view,omitempty"`
	Error *ErrorBody    `json:"error,omitempty"`
}

// CreateTask 调用 App 冻结目标与验收标准。
func (api *API) CreateTask(ctx context.Context, request TaskCreateRequest) (app.Task, error) {
	scope, _, err := desktopWorkflowScope(request.Workspace, request.StateDir)
	if err != nil {
		return app.Task{}, err
	}
	return api.service.CreateTask(ctx, app.TaskScope(scope), app.TaskDefinition{Goal: request.Goal, Acceptance: request.Acceptance, Criteria: request.Criteria})
}

// GetTask 查询任务及同一工作区内已核验的分配。
func (api *API) GetTask(ctx context.Context, request TaskGetRequest) (app.TaskView, error) {
	if strings.TrimSpace(request.TaskID) == "" {
		return app.TaskView{}, errInvalid("task_id is required")
	}
	scope, _, err := desktopWorkflowScope(request.Workspace, request.StateDir)
	if err != nil {
		return app.TaskView{}, err
	}
	return api.service.GetTask(ctx, app.TaskScope(scope), app.TaskID(request.TaskID))
}

// ControlTask 将显式修改或执行分配交给 App。
func (api *API) ControlTask(ctx context.Context, req TaskControlRequest) (TaskControlAck, error) {
	if strings.TrimSpace(req.TaskID) == "" || req.Revision == 0 {
		return TaskControlAck{}, errInvalid("task_id and revision are required")
	}
	workflowScope, origin, err := desktopWorkflowScope(req.Workspace, req.StateDir)
	if err != nil {
		return TaskControlAck{}, err
	}
	scope, id := app.TaskScope(workflowScope), app.TaskID(req.TaskID)
	var task app.Task
	switch req.Action {
	case "run":
		return api.runTask(ctx, scope, id, origin, req)
	case "progress":
		task, err = api.service.UpdateTaskProgress(ctx, scope, id, req.Revision, app.TaskProgress{Summary: req.Summary, Remaining: req.Remaining})
	case "pause":
		task, err = api.service.PauseTask(ctx, scope, id, req.Revision)
	case "resume":
		task, err = api.service.ResumeTask(ctx, scope, id, req.Revision)
	case "cancel":
		task, err = api.service.CancelTask(ctx, scope, id, req.Revision)
	case "complete":
		task, err = api.service.CompleteTask(ctx, scope, id, req.Revision, domain.RunID(req.RunID))
	default:
		return TaskControlAck{}, errInvalid("unsupported task action")
	}
	if err != nil {
		body := MapError(err)
		return TaskControlAck{Error: &body}, nil
	}
	return TaskControlAck{Task: &task}, nil
}

// runTask 让任务执行随应用寿命运行，并用原事件通道转发已持久化事实。
func (api *API) runTask(ctx context.Context, scope app.TaskScope, id app.TaskID, origin string, req TaskControlRequest) (TaskControlAck, error) {
	if err := ctx.Err(); err != nil {
		return TaskControlAck{}, err
	}
	runContext, cancel, err := api.controller.runContext()
	if err != nil {
		return TaskControlAck{}, err
	}
	result := make(chan TaskControlAck, 1)
	if err := api.controller.launch(func() {
		defer cancel()
		stream := newDesktopRunStream(api.controller, runScope{origin, scope.StateDir, scope.Config}, cancel, "", req.SessionID)
		view, operationErr := api.service.RunTask(runContext, scope, id, app.TaskRunOptions{
			AssignmentID: req.AssignmentID, Revision: req.Revision, SessionID: req.SessionID,
			PreviousRunID: domain.RunID(req.PreviousRunID), OnEvent: stream.forward,
		})
		stream.close()
		ack := TaskControlAck{}
		if view.ID != "" {
			ack.View = &view
		}
		if operationErr != nil {
			body := MapError(operationErr)
			ack.Error = &body
		}
		result <- ack
	}); err != nil {
		cancel()
		return TaskControlAck{}, err
	}
	select {
	case ack := <-result:
		return ack, nil
	case <-ctx.Done():
		return TaskControlAck{}, ctx.Err()
	}
}
