/*
 * [INPUT]: 依赖workflow 历史投影与本模块只读存储入口。
 * [OUTPUT]: 提供工作流状态视图与 WorkflowStatus。
 * [POS]: 只读应用用例；不启动步骤或调度器。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 */
// 工作流只读查询用例：从工作区专属 workflow 事件链重放唯一权威投影
// （ADR-0021 片 1）。本文件不提供执行路径；执行、恢复与审批入口随片 2 接入。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
)

// ErrWorkflowNotFound 表示目标工作流链在存储中不存在。
var ErrWorkflowNotFound = errors.New("workflow not found")

// WorkflowStepView 是状态视图中单个步骤的只读投影。
type WorkflowStepView struct {
	ID         string             `json:"id"`
	Index      int                `json:"index"`
	Kind       string             `json:"kind"`
	State      string             `json:"state"`
	Input      workflow.StepIO    `json:"input,omitempty"`
	Output     workflow.StepIO    `json:"output,omitempty"`
	ErrorClass string             `json:"error_class,omitempty"`
	StartedAt  time.Time          `json:"started_at,omitempty"`
	EndedAt    time.Time          `json:"ended_at,omitempty"`
	RunID      domain.RunID       `json:"run_id,omitempty"`
	RunPhase   string             `json:"run_phase,omitempty"`
	Attempts   []workflow.Attempt `json:"attempts,omitempty"`
}

// WorkflowApprovalView 只显示已持久化的 Run 审批提议。
type WorkflowApprovalView struct {
	RunID       domain.RunID       `json:"run_id"`
	OperationID domain.OperationID `json:"operation_id"`
	Fingerprint string             `json:"fingerprint"`
	Summary     string             `json:"summary,omitempty"`
}

// WorkflowReconcileView 显示当前 Run 待对账操作的精确身份。
type WorkflowReconcileView struct {
	RunID           domain.RunID       `json:"run_id"`
	OperationID     domain.OperationID `json:"operation_id"`
	ToolName        string             `json:"tool_name"`
	SideEffectClass string             `json:"side_effect_class"`
}

// WorkflowStatusView 是工作流链重放后的对外只读状态视图。
type WorkflowStatusView struct {
	WorkflowID        string                 `json:"workflow_id"`
	WorkspaceHash     string                 `json:"workspace_hash"`
	ConfigFingerprint string                 `json:"config_fingerprint"`
	PlanSHA256        string                 `json:"plan_sha256"`
	Authorization     string                 `json:"authorization_source"`
	CreatedAt         time.Time              `json:"created_at"`
	Finished          bool                   `json:"finished"`
	FinishState       string                 `json:"finish_state,omitempty"`
	ResumeIndex       int                    `json:"resume_index"`
	RequiresReconcile []string               `json:"requires_reconcile,omitempty"`
	Steps             []WorkflowStepView     `json:"steps"`
	Definition        *WorkflowDefinitionRef `json:"definition,omitempty"`
	DefinitionName    string                 `json:"definition_name,omitempty"`
	SessionID         string                 `json:"session_id,omitempty"`
	Sequence          uint64                 `json:"sequence,omitempty"`
	Phase             string                 `json:"phase,omitempty"`
	PendingApproval   *WorkflowApprovalView  `json:"pending_approval,omitempty"`
	Paused            bool                   `json:"paused,omitempty"`
	RetryTargets      []string               `json:"retry_targets,omitempty"`
	LastRetry         *workflow.RetryPayload `json:"last_retry,omitempty"`
	FinishReason      string                 `json:"finish_reason,omitempty"`
	PendingReconcile  *WorkflowReconcileView `json:"pending_reconcile,omitempty"`
}

// WorkflowStatus 只读重放指定工作流链并返回状态视图；存储缺失、链损坏
// 分别以未找到与状态损坏错误失败关闭，不猜测任何执行状态。
func (service Service) WorkflowStatus(ctx context.Context, workspacePath, stateDir string, configuration config.Config, id workflow.ID) (WorkflowStatusView, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowStatusView{}, err
	}
	scope, hash, err := (WorkflowScope{Workspace: workspacePath, StateDir: stateDir, Config: configuration}).resolve()
	if err != nil {
		return WorkflowStatusView{}, err
	}
	store, err := openWorkflowStoreReadOnly(scope.StateDir, hash)
	if err != nil {
		return WorkflowStatusView{}, err
	}
	defer store.Close()
	execution := workflowExecution{stateDir: scope.StateDir, workspaceHash: hash, store: store}
	snapshot, err := execution.reload(ctx, id)
	if err != nil {
		return WorkflowStatusView{}, err
	}
	return execution.workflowStatusView(ctx, snapshot)
}

// workflowViewFromSnapshot 把链重放投影转换为对外只读状态视图；本函数不做
// 任何 IO，供只读查询与执行入口渲染同一口径。
func workflowViewFromSnapshot(snapshot *workflow.Snapshot) WorkflowStatusView {
	view := WorkflowStatusView{
		WorkflowID:        string(snapshot.WorkflowID),
		WorkspaceHash:     snapshot.WorkspaceHash,
		ConfigFingerprint: snapshot.ConfigFingerprint,
		PlanSHA256:        snapshot.PlanSHA256,
		Authorization:     snapshot.AuthorizationSource,
		CreatedAt:         snapshot.CreatedAt,
		Finished:          snapshot.Finished,
		FinishState:       snapshot.FinishState,
		Definition:        snapshot.Definition,
		SessionID:         snapshot.SessionID,
		ResumeIndex:       snapshot.ResumeIndex,
		RequiresReconcile: snapshot.RequiresReconcile,
		Steps:             make([]WorkflowStepView, len(snapshot.Steps)),
	}
	for index, step := range snapshot.Steps {
		view.Steps[index] = WorkflowStepView{
			ID:         step.ID,
			Index:      step.Index,
			Kind:       snapshot.Kinds[step.ID],
			State:      string(step.State),
			Input:      step.Input,
			Output:     step.Output,
			ErrorClass: step.ErrorClass,
			StartedAt:  step.StartedAt,
			EndedAt:    step.EndedAt,
		}
		if snapshot.Definition != nil {
			view.Steps[index].RunID = domain.RunID(snapshot.StepRuns[step.ID])
			view.Steps[index].Attempts = snapshot.Attempts[step.ID]
		}
	}
	return view
}
