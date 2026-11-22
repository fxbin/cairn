// [INPUT]: 依赖实例链头、返工规则与 Run 控制用例。
// [OUTPUT]: 提供持久暂停、继续、取消、返工及对账入口。
// [POS]: 变更绑定实例链头。Run 对账、终止和批准沿用原应用入口。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件装配版本化工作流的控制用例。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/domain"
)

// WorkflowControlOptions 锚定操作者所见的实例链头及原因。
type WorkflowControlOptions struct {
	Scope      WorkflowScope
	WorkflowID workflow.ID
	Sequence   uint64
	Reason     string
	OnEvent    EventSink
}

// WorkflowRetryOptions 选择冻结策略允许的返工起点。
type WorkflowRetryOptions struct {
	WorkflowControlOptions
	StepID string
}

// WorkflowReconcileOptions 将对账决定绑定当前步骤和操作。
type WorkflowReconcileOptions struct {
	WorkflowControlOptions
	StepID      string
	RunID       domain.RunID
	OperationID domain.OperationID
	Verdict     string
	Evidence    string
}

// PauseWorkflow 持久暂停后续推进。已有 Run 仍归原控制面。
func (service Service) PauseWorkflow(ctx context.Context, options WorkflowControlOptions) (WorkflowOutcome, error) {
	return service.changeWorkflow(ctx, options, func(execution workflowExecution, snapshot *workflow.Snapshot, reason string) (WorkflowOutcome, error) {
		if snapshot.Paused || snapshot.ResumeIndex < 0 {
			return WorkflowOutcome{}, errors.New("workflow cannot be paused in its current state")
		}
		_, err := execution.appendDraftAt(ctx, options.WorkflowID, snapshot.LastSeq, workflow.EventWorkflowPaused, workflow.ControlPayload{Reason: reason})
		return execution.versionedWorkflowOutcome(ctx, options.WorkflowID, "", err)
	})
}

// ResumeWorkflow 以显式理由解除持久暂停，再继续实例。
func (service Service) ResumeWorkflow(ctx context.Context, options WorkflowControlOptions) (WorkflowOutcome, error) {
	return service.changeWorkflow(ctx, options, func(execution workflowExecution, snapshot *workflow.Snapshot, reason string) (WorkflowOutcome, error) {
		if !snapshot.Paused {
			return WorkflowOutcome{}, errors.New("workflow is not paused")
		}
		if _, err := execution.appendDraftAt(ctx, options.WorkflowID, snapshot.LastSeq, workflow.EventWorkflowContinued, workflow.ControlPayload{Reason: reason}); err != nil {
			return WorkflowOutcome{}, err
		}
		return execution.advance(ctx, options.WorkflowID)
	})
}

// CancelWorkflow 结算或停止绑定 Run，再终结工作流。已有不确定操作先对账。
func (service Service) CancelWorkflow(ctx context.Context, options WorkflowControlOptions) (WorkflowOutcome, error) {
	return service.changeWorkflow(ctx, options, func(execution workflowExecution, snapshot *workflow.Snapshot, reason string) (WorkflowOutcome, error) {
		id := options.WorkflowID
		index := snapshot.ResumeIndex
		if index < 0 {
			return WorkflowOutcome{}, ErrWorkflowFinished
		}
		step := snapshot.Steps[index]
		if step.State == workflow.StepRunning || step.State == workflow.StepUncertain {
			if err := execution.closeWorkflowAttempt(ctx, snapshot, reason); err != nil {
				return execution.versionedWorkflowOutcome(ctx, id, "", err)
			}
			var err error
			snapshot, err = execution.reload(ctx, id)
			if err != nil {
				return WorkflowOutcome{}, err
			}
			index = snapshot.ResumeIndex
		}
		state := workflow.RunStateCancelled
		if index < 0 {
			state = workflow.RunStateCompleted
		}
		_, err := execution.appendDraftAt(ctx, id, snapshot.LastSeq, workflow.EventRunFinished,
			workflow.RunFinishedPayload{State: state, ResumeIndex: index, Reason: reason})
		return execution.versionedWorkflowOutcome(ctx, id, "", err)
	})
}

// RetryStep 先提交失败尝试及返工起点，再驱动有界的新尝试。
func (service Service) RetryStep(ctx context.Context, options WorkflowRetryOptions) (WorkflowOutcome, error) {
	return service.changeWorkflow(ctx, options.WorkflowControlOptions, func(execution workflowExecution, snapshot *workflow.Snapshot, reason string) (WorkflowOutcome, error) {
		if snapshot.Paused || !slices.Contains(snapshot.RetryTargets(), options.StepID) {
			return WorkflowOutcome{}, errors.New("workflow retry target is not authorized by the frozen plan")
		}
		failed := snapshot.Steps[snapshot.ResumeIndex].ID
		if _, err := execution.appendDraftAt(ctx, options.WorkflowID, snapshot.LastSeq, workflow.EventRetryAuthorized, workflow.RetryPayload{
			FailedStepID: failed, FailedAttempt: len(snapshot.Attempts[failed]), FromStepID: options.StepID, Reason: reason,
		}); err != nil {
			return WorkflowOutcome{}, err
		}
		return execution.advance(ctx, options.WorkflowID)
	})
}

// changeWorkflow 在实例互斥区内重新核对客户端链头。
func (service Service) changeWorkflow(ctx context.Context, options WorkflowControlOptions, action func(workflowExecution, *workflow.Snapshot, string) (WorkflowOutcome, error)) (WorkflowOutcome, error) {
	reason, err := validWorkflowReason(options.Reason)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	execution, snapshot, release, err := service.openWorkflowMutation(ctx, options.Scope, options.WorkflowID, options.OnEvent)
	if err != nil {
		return WorkflowOutcome{}, err
	}
	defer release()
	if snapshot.LastSeq != options.Sequence {
		return WorkflowOutcome{}, workflow.ErrConflict
	}
	return action(execution, snapshot, reason)
}

// validWorkflowReason 规范化明确原因，并限制链上文本大小。
func validWorkflowReason(input string) (string, error) {
	reason := strings.TrimSpace(input)
	if reason == "" || len(reason) > workflowDecisionMaxBytes || !utf8.ValidString(reason) {
		return "", errors.New("workflow control requires a bounded reason")
	}
	return reason, nil
}

// workflowActionAttempt 要求动作绑定当前仍需结算的运行尝试。
func (execution workflowExecution) workflowActionAttempt(snapshot *workflow.Snapshot, stepID string) (WorkflowNode, workflow.Attempt, error) {
	index := snapshot.ResumeIndex
	if index < 0 {
		return WorkflowNode{}, workflow.Attempt{}, ErrWorkflowFinished
	}
	step := snapshot.Steps[index]
	if step.ID != stepID || (step.State != workflow.StepRunning && step.State != workflow.StepUncertain) {
		return WorkflowNode{}, workflow.Attempt{}, errors.New("workflow action requires the current running step")
	}
	attempts := snapshot.Attempts[stepID]
	return execution.definition.Nodes[index], attempts[len(attempts)-1], nil
}

// closeWorkflowAttempt 使用既有应急终止，避免触发模型或工具。
func (execution workflowExecution) closeWorkflowAttempt(ctx context.Context, snapshot *workflow.Snapshot, reason string) error {
	step := snapshot.Steps[snapshot.ResumeIndex]
	node, attempt, err := execution.workflowActionAttempt(snapshot, step.ID)
	if err != nil {
		return err
	}
	run, events, err := execution.loadVersionedStepRun(ctx, workflowAttemptSession(snapshot.SessionID, node.Kind), attempt)
	if err != nil {
		return err
	}
	if run.Terminal() {
		return execution.settleVersionedAttempt(ctx, snapshot.WorkflowID, node, attempt, run, events, nil)
	}
	if run.PendingOperation != nil {
		return fmt.Errorf("%w: reconcile operation %s before cancelling", ErrWaitingReconcile, run.PendingOperation.ID)
	}
	configuration, err := workflowConfiguredNode(execution.configuration, execution.definition, node)
	if err != nil {
		return err
	}
	_, runErr := execution.service.Abort(ctx, AbortOptions{
		RunID: run.ID, Reason: reason, Workspace: execution.workspacePath,
		StateDir: execution.stateDir, Config: configuration, OnEvent: execution.onEvent,
	})
	return execution.observeVersionedStepRun(ctx, snapshot.WorkflowID, node, attempt, workflowAttemptSession(snapshot.SessionID, node.Kind), runErr)
}
