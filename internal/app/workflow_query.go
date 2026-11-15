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
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/config"
)

// ErrWorkflowNotFound 表示目标工作流链在存储中不存在。
var ErrWorkflowNotFound = errors.New("workflow not found")

// WorkflowStepView 是状态视图中单个步骤的只读投影。
type WorkflowStepView struct {
	ID         string          `json:"id"`
	Index      int             `json:"index"`
	Kind       string          `json:"kind"`
	State      string          `json:"state"`
	Input      workflow.StepIO `json:"input,omitempty"`
	Output     workflow.StepIO `json:"output,omitempty"`
	ErrorClass string          `json:"error_class,omitempty"`
	StartedAt  time.Time       `json:"started_at,omitempty"`
	EndedAt    time.Time       `json:"ended_at,omitempty"`
}

// WorkflowStatusView 是工作流链重放后的对外只读状态视图。
type WorkflowStatusView struct {
	WorkflowID        string             `json:"workflow_id"`
	WorkspaceHash     string             `json:"workspace_hash"`
	ConfigFingerprint string             `json:"config_fingerprint"`
	PlanSHA256        string             `json:"plan_sha256"`
	Authorization     string             `json:"authorization_source"`
	CreatedAt         time.Time          `json:"created_at"`
	Finished          bool               `json:"finished"`
	FinishState       string             `json:"finish_state,omitempty"`
	ResumeIndex       int                `json:"resume_index"`
	RequiresReconcile []string           `json:"requires_reconcile,omitempty"`
	Steps             []WorkflowStepView `json:"steps"`
}

// WorkflowStatus 只读重放指定工作流链并返回状态视图；存储缺失、链损坏
// 分别以未找到与状态损坏错误失败关闭，不猜测任何执行状态。
func (service Service) WorkflowStatus(ctx context.Context, workspacePath, stateDir string, configuration config.Config, id workflow.ID) (WorkflowStatusView, error) {
	if err := ctx.Err(); err != nil {
		return WorkflowStatusView{}, err
	}
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return WorkflowStatusView{}, err
	}
	store, err := openWorkflowStoreReadOnly(stateDir, hash)
	if err != nil {
		return WorkflowStatusView{}, err
	}
	defer store.Close()
	events, err := store.Load(ctx, id)
	if err != nil {
		return WorkflowStatusView{}, err
	}
	if len(events) == 0 {
		return WorkflowStatusView{}, fmt.Errorf("%w: %s", ErrWorkflowNotFound, id)
	}
	snapshot, err := workflow.Project(events)
	if err != nil {
		return WorkflowStatusView{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return workflowViewFromSnapshot(snapshot), nil
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
	}
	return view
}
