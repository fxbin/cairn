// [INPUT]: 依赖 App 实例门面和运行事件。旧模板由定义适配器导入。
// [OUTPUT]: 提供实例启动、控制回执与统一桌面事件。
// [POS]: 控制器拥有桌面任务寿命。实例步骤和授权状态由 App 推进。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件将统一工作流门面接入桌面事件通道。
// Author: fxbin
package desktop

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// WorkflowRunAck 返回实例身份与创建时的持久视图。预演不创建实例。
type WorkflowRunAck struct {
	WorkflowID string                  `json:"workflow_id"`
	RunIDs     []string                `json:"run_ids,omitempty"`
	SessionID  string                  `json:"session_id,omitempty"`
	State      string                  `json:"state"`
	Status     *app.WorkflowStatusView `json:"status,omitempty"`
}

// WorkflowControlRequest 绑定操作者实际看到的实例、链头与待决操作。
type WorkflowControlRequest struct {
	Workspace   string `json:"workspace"`
	StateDir    string `json:"state_dir,omitempty"`
	WorkflowID  string `json:"workflow_id"`
	Action      string `json:"action"`
	Sequence    uint64 `json:"sequence"`
	StepID      string `json:"step_id,omitempty"`
	RunID       string `json:"run_id,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Fingerprint string `json:"fingerprint,omitempty"`
	Reason      string `json:"reason,omitempty"`
	Verdict     string `json:"verdict,omitempty"`
	Evidence    string `json:"evidence,omitempty"`
}

// WorkflowControlAck 在操作暂停或失败时保留已持久化视图与稳定错误码。
type WorkflowControlAck struct {
	View       app.WorkflowStatusView `json:"view"`
	NextAction string                 `json:"next_action,omitempty"`
	Error      *ErrorBody             `json:"error,omitempty"`
}

// RunWorkflow 兼容旧控制器调用。首个 Run 标识来自已持久化创建事件。
func (controller *RunController) RunWorkflow(ctx context.Context, workspace, workflowID string, dryRun bool) (WorkflowRunAck, error) {
	return controller.startWorkflow(ctx, WorkflowStartRequest{Workspace: workspace, DefinitionID: workflowID, DryRun: dryRun}, true)
}

// startWorkflow 只创建持久化实例，再后台调用通用恢复入口。
func (controller *RunController) startWorkflow(ctx context.Context, request WorkflowStartRequest, compatibility bool) (WorkflowRunAck, error) {
	scope, origin, err := desktopWorkflowScope(request.Workspace, request.StateDir)
	if err != nil {
		if compatibility && request.DryRun {
			return controller.previewUnconfiguredLegacyWorkflow(ctx, request.Workspace, request.DefinitionID)
		}
		return WorkflowRunAck{}, err
	}
	if compatibility {
		latest, err := controller.service.ReadWorkflowDefinition(ctx, scope, app.WorkflowDefinitionID(request.DefinitionID), 0)
		if err == nil {
			request.Revision = latest.Revision
		} else if !errors.Is(err, app.ErrWorkflowDefinitionNotFound) {
			return WorkflowRunAck{}, err
		}
	}
	definition, err := controller.selectedWorkflowDefinition(ctx, scope, request)
	if err != nil {
		return WorkflowRunAck{}, err
	}
	options := app.StartWorkflowOptions{Scope: scope, DefinitionID: definition.ID, Revision: definition.Revision}
	if request.DryRun {
		if options.Revision > 0 {
			if err := controller.service.CheckWorkflowDefinition(ctx, options); err != nil {
				return WorkflowRunAck{}, err
			}
		}
		return WorkflowRunAck{WorkflowID: string(definition.ID), State: "dry_run_ok"}, nil
	}
	runContext, cancel, err := controller.runContext()
	if err != nil {
		return WorkflowRunAck{}, err
	}
	view, err := controller.service.CreateWorkflowInstance(ctx, options)
	if err != nil {
		cancel()
		return WorkflowRunAck{}, err
	}
	ack := WorkflowRunAck{WorkflowID: view.WorkflowID, SessionID: view.SessionID, State: view.Phase, Status: &view}
	firstRun := make(chan string, 1)
	status := make(chan WorkflowControlAck, 1)
	if err := controller.launch(func() {
		defer cancel()
		stream := newDesktopRunStream(controller, runScope{origin, scope.StateDir, scope.Config}, cancel, view.WorkflowID, view.SessionID)
		if compatibility {
			stream.created = func(runID string) {
				controller.mirrorWorkflowRunOverride(runContext, scope, view.WorkflowID, definition, runID)
				select {
				case firstRun <- runID:
				default:
				}
			}
		}
		outcome, runErr := controller.service.WorkflowResume(runContext, app.WorkflowResumeOptions{
			WorkflowID: app.WorkflowID(view.WorkflowID), Workspace: scope.Workspace, StateDir: scope.StateDir,
			Config: scope.Config, Sequence: view.Sequence, OnEvent: stream.forward,
		})
		stream.close()
		status <- controller.publishWorkflowOutcome(runContext, scope, origin, view.WorkflowID, outcome, runErr)
	}); err != nil {
		cancel()
		return ack, err
	}
	if !compatibility {
		return ack, nil
	}
	select {
	case runID := <-firstRun:
		ack.RunIDs = []string{runID}
	case result := <-status:
		if result.View.WorkflowID != "" {
			ack.Status, ack.State = &result.View, result.View.Phase
		}
		select {
		case runID := <-firstRun:
			ack.RunIDs = []string{runID}
		default:
		}
		if len(ack.RunIDs) == 0 && result.Error != nil {
			return ack, errInvalid(result.Error.Message)
		}
	case <-ctx.Done():
		return ack, ctx.Err()
	}
	return ack, nil
}

// mirrorWorkflowRunOverride 由真实步骤绑定投影旧界面兼容的 Run 覆盖。
func (controller *RunController) mirrorWorkflowRunOverride(ctx context.Context, scope app.WorkflowScope, workflowID string, definition app.WorkflowDefinition, runID string) {
	if controller.prefs == nil {
		return
	}
	view, err := controller.service.WorkflowStatus(ctx, scope.Workspace, scope.StateDir, scope.Config, app.WorkflowID(workflowID))
	if err != nil {
		return
	}
	for _, step := range view.Steps {
		if string(step.RunID) != runID || step.Index < 0 || step.Index >= len(definition.Nodes) {
			continue
		}
		node := definition.Nodes[step.Index]
		mode := ""
		if node.ReadOnly {
			mode = "read-only"
		}
		_ = controller.prefs.SaveRunOverride(RunOverride{RunID: runID, Model: node.Model, Mode: mode})
		return
	}
}

// ControlWorkflow 把显式决定交给 App。后台控制连接随桌面寿命结束。
func (api *API) ControlWorkflow(ctx context.Context, request WorkflowControlRequest) (WorkflowControlAck, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowControlAck{}, err
	}
	if strings.TrimSpace(request.WorkflowID) == "" || request.Sequence == 0 {
		return WorkflowControlAck{}, errInvalid("workflow_id and sequence are required")
	}
	switch request.Action {
	case "advance", "pause", "resume", "cancel", "retry", "approve", "reject", "reconcile":
	default:
		return WorkflowControlAck{}, errInvalid("unsupported workflow action")
	}
	scope, origin, err := desktopWorkflowScope(request.Workspace, request.StateDir)
	if err != nil {
		return WorkflowControlAck{}, err
	}
	view, err := api.service.WorkflowStatus(ctx, scope.Workspace, scope.StateDir, scope.Config, app.WorkflowID(request.WorkflowID))
	if err != nil {
		return WorkflowControlAck{}, err
	}
	runContext, cancel, err := api.controller.runContext()
	if err != nil {
		return WorkflowControlAck{}, err
	}
	result := make(chan WorkflowControlAck, 1)
	if err := api.controller.launch(func() {
		defer cancel()
		stream := newDesktopRunStream(api.controller, runScope{origin, scope.StateDir, scope.Config}, cancel, view.WorkflowID, view.SessionID)
		outcome, operationErr := api.applyWorkflowControl(runContext, scope, request, stream.forward)
		stream.close()
		result <- api.controller.publishWorkflowOutcome(runContext, scope, origin, request.WorkflowID, outcome, operationErr)
	}); err != nil {
		cancel()
		return WorkflowControlAck{}, err
	}
	select {
	case response := <-result:
		return response, nil
	case <-ctx.Done():
		return WorkflowControlAck{}, ctx.Err()
	}
}

// applyWorkflowControl 映射封闭动作词表。链头和审批身份由 App 在锁内核验。
func (api *API) applyWorkflowControl(ctx context.Context, scope app.WorkflowScope, req WorkflowControlRequest, sink app.EventSink) (app.WorkflowOutcome, error) {
	control := app.WorkflowControlOptions{Scope: scope, WorkflowID: app.WorkflowID(req.WorkflowID), Sequence: req.Sequence, Reason: req.Reason, OnEvent: sink}
	switch req.Action {
	case "advance":
		return api.service.WorkflowResume(ctx, app.WorkflowResumeOptions{
			WorkflowID: control.WorkflowID, Workspace: scope.Workspace, StateDir: scope.StateDir,
			Config: scope.Config, Sequence: req.Sequence, OnEvent: sink,
		})
	case "pause":
		return api.service.PauseWorkflow(ctx, control)
	case "resume":
		return api.service.ResumeWorkflow(ctx, control)
	case "cancel":
		return api.service.CancelWorkflow(ctx, control)
	case "retry":
		return api.service.RetryStep(ctx, app.WorkflowRetryOptions{WorkflowControlOptions: control, StepID: req.StepID})
	case "approve", "reject":
		decision := app.WorkflowDecisionOptions{
			Scope: scope, WorkflowID: control.WorkflowID, StepID: req.StepID, Sequence: req.Sequence, Reason: req.Reason,
			RunID: domain.RunID(req.RunID), OperationID: domainOperationID(req.OperationID), Fingerprint: req.Fingerprint, OnEvent: sink,
		}
		if req.Action == "approve" {
			return api.service.ApproveStep(ctx, decision)
		}
		return api.service.RejectStep(ctx, decision)
	case "reconcile":
		return api.service.ReconcileWorkflowStep(ctx, app.WorkflowReconcileOptions{
			WorkflowControlOptions: control, StepID: req.StepID, RunID: domain.RunID(req.RunID), OperationID: domainOperationID(req.OperationID),
			Verdict: req.Verdict, Evidence: req.Evidence,
		})
	default:
		return app.WorkflowOutcome{}, errInvalid("unsupported workflow action")
	}
}

// publishWorkflowOutcome 投送最新持久事实。失败路径尝试重新读取，避免旧视图覆盖新状态。
func (controller *RunController) publishWorkflowOutcome(ctx context.Context, scope app.WorkflowScope, origin, id string, outcome app.WorkflowOutcome, operationErr error) WorkflowControlAck {
	view := outcome.View
	if view.WorkflowID == "" {
		const statusReadTimeout = 2 * time.Second
		readContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), statusReadTimeout)
		defer cancel()
		current, err := controller.service.WorkflowStatus(readContext, scope.Workspace, scope.StateDir, scope.Config, app.WorkflowID(id))
		if err == nil {
			view = current
		}
	}
	event := StreamEvent{Workspace: origin, Kind: "workflow.status", WorkflowID: id, SessionID: view.SessionID, NextAction: outcome.NextAction}
	if view.WorkflowID != "" {
		event.Workflow = &view
	}
	var body *ErrorBody
	if operationErr != nil {
		mapped := MapError(operationErr)
		body = &mapped
		event.Error = body
	}
	controller.push(event)
	if body != nil && body.RunID != "" {
		kind := ""
		switch body.Code {
		case CodeWaitingApproval:
			kind = "run.waiting_approval"
		case CodeWaitingReconcile:
			kind = "run.waiting_reconcile"
		}
		if kind != "" {
			controller.push(StreamEvent{Workspace: origin, Kind: kind, RunID: body.RunID, WorkflowID: id, SessionID: view.SessionID, Error: body})
		}
	}
	return WorkflowControlAck{View: view, NextAction: outcome.NextAction, Error: body}
}

// desktopRunStream 仅转发 App 落盘后的事件。它跟踪当前调用的取消句柄。
type desktopRunStream struct {
	controller *RunController
	scope      runScope
	cancel     context.CancelFunc
	workflowID string
	sessionID  string
	sink       app.EventSink
	created    func(string)
	mu         sync.Mutex
	registered map[string]bool
	completed  []string
	closed     bool
}

// newDesktopRunStream 建立一次 App 调用的运行通知桥。
func newDesktopRunStream(controller *RunController, scope runScope, cancel context.CancelFunc, workflowID, sessionID string) *desktopRunStream {
	return &desktopRunStream{
		controller: controller, scope: scope, cancel: cancel, workflowID: workflowID, sessionID: sessionID,
		sink: controller.eventSink("", sessionID, scope.Workspace), registered: make(map[string]bool),
	}
}

// forward 在创建、恢复或脚本操作启动后登记取消。终态来自 Journal。
func (stream *desktopRunStream) forward(event domain.Event) {
	runID := string(event.RunID)
	stream.mu.Lock()
	if stream.closed {
		stream.mu.Unlock()
		return
	}
	if event.Type == domain.EventRunCreated || event.Type == domain.EventRunResumed || event.Type == domain.EventOperationStarted {
		if !stream.registered[runID] && stream.controller.registerRun(runID, stream.sessionID, stream.cancel, nil) == nil {
			stream.registered[runID] = true
		}
	}
	stream.mu.Unlock()
	if event.Type == domain.EventRunCreated && stream.created != nil {
		stream.created(runID)
	}
	stream.sink(event)

	var state, response string
	switch event.Type {
	case domain.EventRunCompleted:
		state = string(domain.RunCompleted)
		var payload domain.RunCompletedPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			response = payload.FinalResponse
		}
		stream.mu.Lock()
		stream.completed = append(stream.completed, runID)
		stream.mu.Unlock()
	case domain.EventRunFailed:
		state = string(domain.RunFailed)
	case domain.EventRunCancelled:
		state = string(domain.RunCancelled)
	default:
		return
	}
	view := RunStatusView{
		RunID: runID, SessionID: stream.sessionID, State: state, Phase: state,
		StateLabel: contract.RunPhaseLabel(state), PhaseLabel: contract.RunPhaseLabel(state), ResponsePreview: previewResponse(response),
	}
	stream.controller.push(StreamEvent{
		Workspace: stream.scope.Workspace, Kind: "run.finished", WorkflowID: stream.workflowID, RunID: runID, SessionID: stream.sessionID, Status: &view,
	})
	stream.mu.Lock()
	if stream.registered[runID] {
		delete(stream.registered, runID)
		stream.controller.unregisterRun(runID)
	}
	stream.mu.Unlock()
}

// close 释放调用结束后的桌面登记。完成钩子仍归原工作区和状态目录。
func (stream *desktopRunStream) close() {
	stream.mu.Lock()
	registered, completed := stream.registered, stream.completed
	stream.registered, stream.completed, stream.closed = nil, nil, true
	stream.mu.Unlock()
	for runID := range registered {
		stream.controller.unregisterRun(runID)
	}
	for _, runID := range completed {
		stream.controller.dispatchHooks("run_finished", runID, stream.scope)
	}
}
