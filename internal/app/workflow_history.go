// [INPUT]: 依赖工作流实例重放、Run Journal 和冻结制品。
// [OUTPUT]: 提供权威实例列表、步骤历史和活动审批投影。
// [POS]: 只读应用用例。丢失的历史不由界面或缓存推断。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件投影版本化工作流状态与执行历史。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/ports"
)

// WorkflowHistory 列出工作区的权威实例和尝试历史。
func (Service) WorkflowHistory(ctx context.Context, scope WorkflowScope) ([]WorkflowStatusView, error) {
	scope, hash, err := scope.resolve()
	if err != nil {
		return nil, err
	}
	store, err := openWorkflowStoreReadOnly(scope.StateDir, hash)
	if errors.Is(err, ErrWorkflowNotFound) {
		return []WorkflowStatusView{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer store.Close()
	ids, err := store.ListInstances(ctx, hash)
	if err != nil {
		return nil, err
	}
	execution := workflowExecution{stateDir: scope.StateDir, workspaceHash: hash, store: store}
	views := make([]WorkflowStatusView, 0, len(ids))
	for _, id := range ids {
		snapshot, err := execution.reload(ctx, id)
		if err != nil {
			return nil, err
		}
		view, err := execution.workflowStatusView(ctx, snapshot)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, nil
}

// workflowStatusView 核验冻结定义和 Run 历史后给出当前阶段。
func (execution workflowExecution) workflowStatusView(ctx context.Context, snapshot *workflow.Snapshot) (WorkflowStatusView, error) {
	view := workflowViewFromSnapshot(snapshot)
	if snapshot.Definition == nil {
		return view, nil
	}
	definition := execution.definition
	if definition == nil {
		loaded, err := verificationBoundDefinition(execution.stateDir, execution.workspaceHash, snapshot)
		if err != nil {
			return WorkflowStatusView{}, err
		}
		definition = &loaded
	}
	view.DefinitionName, view.Sequence = definition.Name, snapshot.LastSeq
	view.Paused, view.LastRetry, view.FinishReason = snapshot.Paused, snapshot.LastRetry, snapshot.FinishReason
	if !snapshot.Paused {
		view.RetryTargets = snapshot.RetryTargets()
	}
	view.Phase = "ready"
	if err := execution.projectWorkflowAttempts(ctx, snapshot, &view); err != nil {
		return WorkflowStatusView{}, err
	}
	switch {
	case view.Finished:
		view.Phase = view.FinishState
	case view.PendingReconcile != nil:
		view.Phase = "waiting_reconcile"
	case view.Paused:
		view.Phase = "paused"
	case view.ResumeIndex < 0:
		view.Phase = "settling"
	default:
		step := view.Steps[view.ResumeIndex]
		switch {
		case step.State == string(workflow.StepPending) && step.Kind == workflow.StepKindAccept:
			view.Phase = "waiting_human"
		case step.State == string(workflow.StepFailed):
			view.Phase = "retry_required"
		case step.RunPhase != "":
			view.Phase = step.RunPhase
		}
	}
	return view, nil
}

// projectWorkflowAttempts 核验已绑定的 Run、终态锚点和产物引用。
func (execution workflowExecution) projectWorkflowAttempts(ctx context.Context, snapshot *workflow.Snapshot, view *WorkflowStatusView) error {
	var journal ports.Journal
	for index, step := range view.Steps {
		for _, attempt := range step.Attempts {
			if attempt.RunID == "" {
				continue
			}
			current := index == view.ResumeIndex && step.RunID == attempt.RunID && step.State == string(workflow.StepRunning)
			if journal == nil {
				opened, err := openReadOnlyJournalContext(ctx, execution.stateDir, execution.workspaceHash)
				if err != nil {
					if current && errors.Is(err, os.ErrNotExist) && errors.Is(missingWorkflowAttempt(attempt), ErrWorkflowAttemptUnconfirmed) {
						view.Steps[index].RunPhase = "binding_unconfirmed"
						continue
					}
					return fmt.Errorf("%w: open workflow run history: %w", ErrStateCorrupt, err)
				}
				defer opened.Close()
				journal = opened
			}
			run, events, err := readWorkflowAttempt(ctx, journal, execution.workspaceHash, snapshot.SessionID, attempt)
			if err != nil {
				if current && errors.Is(err, ErrWorkflowAttemptUnconfirmed) {
					view.Steps[index].RunPhase = "binding_unconfirmed"
					continue
				}
				return fmt.Errorf("%w: workflow run history: %w", ErrStateCorrupt, err)
			}
			if step.Kind == workflow.StepKindVerify && attempt.State == workflow.StepSucceeded {
				if err := checkRunArtifactReferences(events, execution.stateDir, execution.workspaceHash); err != nil {
					return fmt.Errorf("%w: workflow verification artifacts: %w", ErrStateCorrupt, err)
				}
				result, err := runResultFromEvents(events, run, execution.stateDir, execution.workspaceHash)
				if err != nil {
					return fmt.Errorf("%w: invalid workflow verification result: %w", ErrStateCorrupt, err)
				}
				if !result.Evidence.Trusted {
					return fmt.Errorf("%w: successful workflow verification has no trusted run evidence", ErrStateCorrupt)
				}
			}
			if err := execution.checkWorkflowAttemptOutput(attempt); err != nil {
				return err
			}
			if step.RunID == attempt.RunID {
				view.Steps[index].RunPhase = run.Phase()
				if index == view.ResumeIndex && run.Phase() == "waiting_reconcile" && run.PendingOperation != nil {
					pending := run.PendingOperation
					view.PendingReconcile = &WorkflowReconcileView{
						RunID: run.ID, OperationID: pending.ID, ToolName: pending.ToolName, SideEffectClass: pending.SideEffectClass,
					}
				}
				if index == view.ResumeIndex && run.PendingApproval != nil {
					pending := run.PendingApproval
					view.PendingApproval = &WorkflowApprovalView{
						RunID: run.ID, OperationID: pending.OperationID, Fingerprint: pending.Fingerprint, Summary: pending.Summary,
					}
				}
			}
		}
	}
	return nil
}

// checkWorkflowAttemptOutput 核对输出引用只能属于本尝试的 Run。
func (execution workflowExecution) checkWorkflowAttemptOutput(attempt workflow.Attempt) error {
	if attempt.Output.Ref == "" {
		return nil
	}
	output, err := execution.readWorkflowDependencies([]workflow.Dependency{{Output: attempt.Output}})
	if err != nil {
		return err
	}
	if output[0].RunID != attempt.RunID {
		return fmt.Errorf("%w: workflow output run mismatch", ErrStateCorrupt)
	}
	return nil
}

// versionedWorkflowOutcome 在中断返回时也保留可读取的实例身份与恢复状态。
func (execution workflowExecution) versionedWorkflowOutcome(ctx context.Context, id workflow.ID, hint string, driveErr error) (WorkflowOutcome, error) {
	observe, cancel := context.WithTimeout(context.WithoutCancel(ctx), workflowCommitTimeout)
	defer cancel()
	snapshot, err := execution.reload(observe, id)
	if err != nil {
		return WorkflowOutcome{}, errors.Join(driveErr, err)
	}
	view, err := execution.workflowStatusView(observe, snapshot)
	if err != nil {
		return workflowOutcomeFor(snapshot, hint), errors.Join(driveErr, err)
	}
	return WorkflowOutcome{View: view, NextAction: hint}, driveErr
}
