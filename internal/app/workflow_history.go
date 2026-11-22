// [INPUT]: 依赖工作流实例重放、Run Journal 和冻结制品。
// [OUTPUT]: 提供权威实例列表、步骤历史和活动审批投影。
// [POS]: 只读应用用例。丢失的历史不由界面或缓存推断。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件投影版本化工作流状态与执行历史。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
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
	if definition.Verification != nil {
		view.VerificationCriteria = contract.CloneAcceptanceCriteria(definition.Verification.Criteria)
	}
	view.Paused, view.LastRetry, view.FinishReason = snapshot.Paused, snapshot.LastRetry, snapshot.FinishReason
	if !snapshot.Paused {
		view.RetryTargets = snapshot.RetryTargets()
	}
	view.Phase = "ready"
	if err := execution.projectWorkflowAttempts(ctx, snapshot, definition, &view); err != nil {
		return WorkflowStatusView{}, err
	}
	taskState, err := execution.workflowTaskState(ctx, snapshot, definition, view)
	if err != nil {
		return WorkflowStatusView{}, err
	}
	view.TaskState = taskState
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
func (execution workflowExecution) projectWorkflowAttempts(ctx context.Context, snapshot *workflow.Snapshot, definition *WorkflowDefinition, view *WorkflowStatusView) error {
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
			run, events, err := readWorkflowAttempt(ctx, journal, execution.workspaceHash, workflowAttemptSession(snapshot.SessionID, step.Kind), attempt)
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
			report, err := execution.checkWorkflowAttemptOutput(ctx, journal, snapshot, definition, step.Kind, attempt)
			if err != nil {
				return err
			}
			if step.RunID == attempt.RunID {
				if report != nil {
					view.Verification = report
				}
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

// checkWorkflowAttemptOutput 核对输出引用与报告内覆盖证据。
func (execution workflowExecution) checkWorkflowAttemptOutput(ctx context.Context, journal ports.Journal, snapshot *workflow.Snapshot, definition *WorkflowDefinition, kind string, attempt workflow.Attempt) (*WorkflowVerificationReport, error) {
	if attempt.Output.Ref == "" {
		return nil, nil
	}
	outputs, err := execution.readWorkflowDependencies([]workflow.Dependency{{Output: attempt.Output}})
	if err != nil {
		return nil, err
	}
	output := outputs[0]
	if output.RunID != attempt.RunID {
		return nil, fmt.Errorf("%w: workflow output run mismatch", ErrStateCorrupt)
	}
	report := output.Verification
	if report == nil {
		return nil, nil
	}
	if definition.Verification == nil || (kind != workflow.StepKindVerify && kind != workflow.StepKindReview) ||
		report.Coverage.ValidateAgainst(report.Scope) != nil || report.Evaluation.Validate() != nil ||
		report.Evaluation.AcceptanceDigest != report.Scope.AcceptanceDigest || report.Scope.AcceptanceDigest != definition.Verification.Digest {
		return nil, fmt.Errorf("%w: workflow attempt verification report does not match its anchors", ErrStateCorrupt)
	}
	worker := precedingWorkflowAttempt(snapshot, definition.Nodes[workflowVerificationWorker].ID, attempt.StartedSeq)
	program := attempt
	if kind == workflow.StepKindReview {
		program = precedingWorkflowAttempt(snapshot, definition.Nodes[workflowVerificationProgram].ID, attempt.StartedSeq)
	}
	if worker.RunID == "" || report.WorkerRunID != worker.RunID || report.ProgramRunID != program.RunID ||
		kind == workflow.StepKindReview && report.ReviewRunID != attempt.RunID ||
		kind == workflow.StepKindVerify && report.ReviewRunID != "" {
		return nil, fmt.Errorf("%w: verification report refers to the wrong attempts", ErrStateCorrupt)
	}
	workerRun, workerEvents, err := readWorkflowAttempt(ctx, journal, execution.workspaceHash, snapshot.SessionID, worker)
	if err != nil {
		return nil, err
	}
	version, err := workflowObservedRunVersion(workerRun, workerEvents)
	if err != nil {
		return nil, err
	}
	if err := execution.checkWorkflowReportCoverage(ctx, journal, snapshot.SessionID, program, attempt, report, version); err != nil {
		return nil, err
	}
	if len(report.Findings.Confirmed) > 0 {
		run, events, err := readWorkflowAttempt(ctx, journal, execution.workspaceHash, snapshot.SessionID, program)
		if err != nil {
			return nil, err
		}
		confirmed, err := execution.workflowProgramFailureFindings(run, events, workflowVerificationInputs{
			Acceptance: *definition.Verification, WorkerRunID: worker.RunID, ProgramRunID: program.RunID, WorkspaceVersion: version,
		})
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(confirmed, report.Findings.Confirmed) {
			return nil, fmt.Errorf("%w: confirmed findings differ from program evidence", ErrStateCorrupt)
		}
	}
	return report, nil
}

// precedingWorkflowAttempt 返回报告来源阶段在当时最近的尝试。
func precedingWorkflowAttempt(snapshot *workflow.Snapshot, stepID string, before uint64) workflow.Attempt {
	attempts := snapshot.Attempts[stepID]
	for index := len(attempts) - 1; index >= 0; index-- {
		if attempts[index].StartedSeq < before {
			return attempts[index]
		}
	}
	return workflow.Attempt{}
}

// checkWorkflowReportCoverage 校验证据制品与来源 Run 的终态或验证事件。
func (execution workflowExecution) checkWorkflowReportCoverage(ctx context.Context, journal ports.Journal, sessionID string, program, current workflow.Attempt, report *WorkflowVerificationReport, version string) error {
	root := manifestArtifactRoot(execution.stateDir, execution.workspaceHash)
	for _, object := range report.Scope.Objects {
		if object.Version != version {
			return fmt.Errorf("%w: verification scope references the wrong workspace version", ErrStateCorrupt)
		}
	}
	for _, entry := range report.Coverage.Entries {
		if entry.EvidenceRef == nil {
			continue
		}
		content, err := readArtifactContent(root, *entry.EvidenceRef)
		if err != nil {
			return fmt.Errorf("%w: workflow coverage evidence: %w", ErrStateCorrupt, err)
		}
		switch entry.ObjectID {
		case workflowProgramCoverageID:
			run, events, err := readWorkflowAttempt(ctx, journal, execution.workspaceHash, sessionID, program)
			if err != nil {
				return err
			}
			var finish domain.VerificationFinishedPayload
			var ref contract.Ref
			if !run.VerificationSucceeded || run.LastVerificationSeq == 0 ||
				json.Unmarshal(events[run.LastVerificationSeq-1].Payload, &finish) != nil ||
				json.Unmarshal(finish.EvidenceRef, &ref) != nil || ref.SHA256 != entry.EvidenceRef.SHA256 {
				return fmt.Errorf("%w: program evidence has no matching verification event", ErrStateCorrupt)
			}
		case workflowReviewCoverageID:
			var receipt workflowReviewReceipt
			if json.Unmarshal(content, &receipt) != nil || current.Terminal == nil ||
				receipt.Terminal != *current.Terminal || receipt.RunID != report.ReviewRunID ||
				receipt.SubjectVersion != version || !reflect.DeepEqual(receipt.Findings, report.Findings) {
				return fmt.Errorf("%w: review evidence has no matching terminal run", ErrStateCorrupt)
			}
		default:
			return fmt.Errorf("%w: unknown workflow coverage object", ErrStateCorrupt)
		}
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
