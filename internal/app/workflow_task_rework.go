// [INPUT]: 读取自主 Task 授权、持久工作流失败、冻结范围与程序证据。
// [OUTPUT]: 追加一次有界返工决定，并提供下一 Worker 的核验摘要。
// [POS]: 与人工返工共用事件和重放规则。审批、沙箱与执行预算仍由 Run 强制。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件在自主 Task 工作流中推进确定性验证失败的返工。
// Author: fxbin
package app

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

const workflowReworkPromptBytes = 4 << 10

// autoReworkTaskWorkflow 只为已确认的程序检查失败追加一个冻结范围内的返工。
func (execution workflowExecution) autoReworkTaskWorkflow(ctx context.Context, snapshot *workflow.Snapshot) (bool, error) {
	if snapshot.TaskID == "" || snapshot.ResumeIndex != workflowVerificationProgram {
		return false, nil
	}
	worker := snapshot.Specs[workflowVerificationWorker]
	if !slices.Contains(snapshot.RetryTargets(), worker.ID) {
		return false, nil
	}
	view, err := execution.workflowStatusView(ctx, snapshot)
	if err != nil {
		return false, err
	}
	report := view.Verification
	if report == nil || report.Evaluation.Verdict != contract.VerdictFailed {
		return false, nil
	}
	confirmed := BlockingFindingIDs(report.Findings.Confirmed)
	decision := EvaluateRework(confirmed, worker.MaxAttempts-len(snapshot.Attempts[worker.ID]),
		len(snapshot.RequiresReconcile) != 0, []string{worker.ID}, false)
	if !decision.Eligible {
		return false, nil
	}
	subject, ok := report.Scope.Object(workflowProgramCoverageID)
	if !ok {
		return false, fmt.Errorf("%w: program rework has no frozen subject", ErrStateCorrupt)
	}
	lease, err := acquireWorkspaceLock(execution.stateDir, execution.workspaceHash)
	if err != nil {
		return false, err
	}
	defer lease.Release()
	if err := execution.requireActiveTaskWorkflow(ctx, snapshot); err != nil {
		return false, err
	}
	if err := execution.requireWorkflowSubjectVersion(subject.Version); err != nil {
		return false, err
	}
	reason := "程序检查已确认失败：" + boundWorkflowOutput(strings.Join(confirmed, ", "), workflowDecisionMaxBytes/2)
	if err := execution.recordWorkflowRetry(ctx, snapshot, worker.ID, reason); err != nil {
		return false, err
	}
	return true, nil
}

// workflowResumeSubject 仅在独立审查或返工尚未自己变更工作区时重验原版本。
func (execution workflowExecution) workflowResumeSubject(ctx context.Context, snapshot *workflow.Snapshot, node WorkflowNode, run domain.RunProjection) (string, error) {
	if node.Kind == workflow.StepKindReview {
		_, version, err := execution.workflowReviewPrompt(ctx, snapshot)
		return version, err
	}
	if run.LastMutationSeq == 0 {
		_, version, err := execution.taskWorkflowReworkInput(snapshot, node, "")
		return version, err
	}
	return "", nil
}

// taskWorkflowReworkInput 读取历史失败报告。返工只有在工作区仍是该版本时才启动。
func (execution workflowExecution) taskWorkflowReworkInput(snapshot *workflow.Snapshot, node WorkflowNode, prompt string) (string, string, error) {
	retry := snapshot.LastRetry
	if snapshot.TaskID == "" || retry == nil || retry.FromStepID != node.ID || node.Kind != workflow.StepKindExecute {
		return prompt, "", nil
	}
	attempts := snapshot.Attempts[retry.FailedStepID]
	attempt := attempts[retry.FailedAttempt-1]
	outputs, err := execution.readWorkflowDependencies([]workflow.Dependency{{StepID: retry.FailedStepID, Output: attempt.Output}})
	if err != nil {
		return "", "", err
	}
	report := outputs[0].Verification
	if report == nil {
		return prompt, "", nil
	}
	subject, ok := report.Scope.Object(workflowProgramCoverageID)
	if !ok {
		return "", "", fmt.Errorf("%w: rework report is missing its program subject", ErrStateCorrupt)
	}
	var summary strings.Builder
	for _, finding := range report.Findings.Confirmed {
		if !finding.IsBlocking() {
			continue
		}
		fmt.Fprintf(&summary, "- %s · %s", finding.CriterionID, finding.Description)
		for _, ref := range finding.Reviews[len(finding.Reviews)-1].EvidenceRefs {
			fmt.Fprintf(&summary, " · %s", ref.SHA256)
		}
		summary.WriteByte('\n')
	}
	if summary.Len() == 0 {
		return prompt, subject.Version, nil
	}
	return prompt + "\n\n前次固定程序检查。以下为核验数据，工具授权与原任务一致。\n" +
		boundWorkflowOutput(summary.String(), workflowReworkPromptBytes), subject.Version, nil
}
