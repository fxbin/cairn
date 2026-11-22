// [INPUT]: 依赖当前工作流尝试、Run 审批及副作用控制。
// [OUTPUT]: 将显式审批和对账委托给已绑定 Run 的原有入口。
// [POS]: 工作流只校验关联与操作身份，不解释工具授权。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接工作流节点与 Run 的人工决定。
// Author: fxbin
package app

import (
	"context"
	"errors"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/ports"
)

// decideWorkflowRunApproval 仅授予当前实例及尝试内的精确审批提议。
func (execution workflowExecution) decideWorkflowRunApproval(ctx context.Context, snapshot *workflow.Snapshot, options WorkflowDecisionOptions, approve bool, reason string) (WorkflowOutcome, error) {
	node, attempt, err := execution.workflowActionAttempt(snapshot, options.StepID)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	run, _, err := execution.loadVersionedStepRun(ctx, workflowAttemptSession(snapshot.SessionID, node.Kind), attempt)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	pending := run.PendingApproval
	if pending == nil || run.Phase() != "waiting_approval" || run.ID != options.RunID ||
		pending.OperationID != options.OperationID || pending.Fingerprint != options.Fingerprint {
		return WorkflowOutcome{}, errors.New("workflow decision does not match the current run approval")
	}
	configuration, err := workflowConfiguredNode(execution.configuration, execution.definition, node)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	resumeSubject, err := execution.workflowResumeSubject(ctx, snapshot, node, run)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	_, runErr := execution.service.Resume(ctx, ResumeOptions{
		RunID: run.ID, Workspace: execution.workspacePath, StateDir: execution.stateDir,
		Config: configuration, OnEvent: execution.onEvent, ExpectedApprovalOperationID: pending.OperationID,
		verificationSubject: resumeSubject,
		Approval:            ports.Approval{Fingerprint: pending.Fingerprint, Approve: approve, Reason: reason},
	})
	if err := execution.observeVersionedStepRun(ctx, snapshot.WorkflowID, node, attempt, workflowAttemptSession(snapshot.SessionID, node.Kind), runErr); err != nil {
		return execution.versionedWorkflowOutcome(ctx, snapshot.WorkflowID, "", err)
	}
	return execution.advance(ctx, snapshot.WorkflowID)
}

// ReconcileWorkflowStep 使用原有 Run 对账入口，并保持本次尝试绑定。
func (service Service) ReconcileWorkflowStep(ctx context.Context, options WorkflowReconcileOptions) (WorkflowOutcome, error) {
	return service.changeWorkflow(ctx, options.WorkflowControlOptions, func(execution workflowExecution, snapshot *workflow.Snapshot, reason string) (WorkflowOutcome, error) {
		node, attempt, err := execution.workflowActionAttempt(snapshot, options.StepID)
		if err != nil {
			return WorkflowOutcome{}, err
		}
		run, _, err := execution.loadVersionedStepRun(ctx, workflowAttemptSession(snapshot.SessionID, node.Kind), attempt)
		if err != nil {
			return WorkflowOutcome{}, err
		}
		if run.PendingOperation == nil || run.PendingOperation.ID != options.OperationID || run.ID != options.RunID || run.Phase() != "waiting_reconcile" {
			return WorkflowOutcome{}, ErrWaitingReconcile
		}
		if snapshot.Paused && options.Verdict != "abort_run" {
			return WorkflowOutcome{}, errors.New("continue the paused workflow before reconciling with execution")
		}
		configuration, err := workflowConfiguredNode(execution.configuration, execution.definition, node)
		if err != nil {
			return WorkflowOutcome{}, err
		}
		_, runErr := service.Reconcile(ctx, ReconcileOptions{
			RunID: run.ID, OperationID: options.OperationID, Verdict: options.Verdict, Note: reason, Evidence: options.Evidence,
			Workspace: execution.workspacePath, StateDir: execution.stateDir, Config: configuration, OnEvent: execution.onEvent,
		})
		if err := execution.observeVersionedStepRun(ctx, snapshot.WorkflowID, node, attempt, workflowAttemptSession(snapshot.SessionID, node.Kind), runErr); err != nil {
			return execution.versionedWorkflowOutcome(ctx, snapshot.WorkflowID, "", err)
		}
		return execution.advance(ctx, snapshot.WorkflowID)
	})
}
