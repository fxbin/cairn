// [INPUT]: 冻结验收合同、已绑定的程序与独立验证事实。
// [OUTPUT]: 可持久化的验证范围、覆盖清单、发现与验收结论。
// [POS]: 从已核验的 Run 证据纯投影报告，不写 Journal 或授予权限。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件组合工作流的验收报告。
// Author: fxbin
package app

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

const (
	workflowProgramCoverageID = "program"
	workflowReviewCoverageID  = "review"
	workflowRulesetVersion    = "v1"
)

// WorkflowVerificationReport 汇总同一工作区版本的验证证据和验收结论。
type WorkflowVerificationReport struct {
	WorkerRunID  domain.RunID                  `json:"worker_run_id"`
	ProgramRunID domain.RunID                  `json:"program_run_id,omitempty"`
	ReviewRunID  domain.RunID                  `json:"review_run_id,omitempty"`
	Scope        contract.VerificationScope    `json:"scope"`
	Coverage     contract.CoverageManifest     `json:"coverage"`
	Findings     ValidatedFindings             `json:"findings"`
	Evaluation   contract.AcceptanceEvaluation `json:"evaluation"`
}

// workflowVerificationInputs 只接收应用层已核对来源的事实及制品引用。
type workflowVerificationInputs struct {
	Acceptance       contract.AcceptanceContract
	WorkerRunID      domain.RunID
	ProgramRunID     domain.RunID
	ReviewRunID      domain.RunID
	WorkspaceVersion string
	ProgramEvidence  *contract.AcceptanceEvidence
	ProgramRef       *contract.Ref
	ReviewRef        *contract.Ref
	ProgramError     string
	ReviewError      string
	Findings         ValidatedFindings
}

// buildWorkflowVerificationReport 保留缺失覆盖及未定论发现，复用合同验收判定。
func buildWorkflowVerificationReport(input workflowVerificationInputs) (WorkflowVerificationReport, error) {
	if input.WorkerRunID == "" || input.WorkerRunID == input.ProgramRunID || input.WorkerRunID == input.ReviewRunID ||
		(input.ProgramRunID != "" && input.ProgramRunID == input.ReviewRunID) {
		return WorkflowVerificationReport{}, errors.New("verification report requires distinct worker, program and review runs")
	}
	criteria := make([]string, 0, len(input.Acceptance.Criteria))
	for _, criterion := range input.Acceptance.Criteria {
		criteria = append(criteria, criterion.ID)
	}
	scope, err := contract.NewVerificationScope(input.Acceptance, []contract.VerificationObject{
		{ID: workflowProgramCoverageID, Kind: "program", Version: input.WorkspaceVersion, Criteria: criteria, Ruleset: "acceptance-commands", RulesetVersion: workflowRulesetVersion},
		{ID: workflowReviewCoverageID, Kind: "review", Version: input.WorkspaceVersion, Criteria: criteria, Ruleset: "independent-review", RulesetVersion: workflowRulesetVersion},
	})
	if err != nil {
		return WorkflowVerificationReport{}, err
	}
	if err := checkWorkflowProgramEvidence(input); err != nil {
		return WorkflowVerificationReport{}, err
	}
	if input.ReviewRef != nil && (input.ReviewRunID == "" || input.ReviewError != "") {
		return WorkflowVerificationReport{}, errors.New("independent review evidence has no successful review run")
	}
	findings, err := workflowReportFindings(input, criteria)
	if err != nil {
		return WorkflowVerificationReport{}, err
	}
	coverage, err := workflowReportCoverage(scope, input)
	if err != nil {
		return WorkflowVerificationReport{}, err
	}
	summary, err := coverage.Summary(scope)
	if err != nil {
		return WorkflowVerificationReport{}, err
	}
	evaluation, err := contract.EvaluateAcceptanceVerdict(contract.EvaluationInput{
		Acceptance: input.Acceptance, Coverage: summary, Evidence: input.ProgramEvidence, Findings: findings,
	})
	if err != nil {
		return WorkflowVerificationReport{}, err
	}
	markWorkflowInconclusive(&evaluation, input.Findings.Inconclusive)
	validated := input.Findings
	validated.BlockingIDs = slices.Clone(evaluation.BlockingFindings)
	return WorkflowVerificationReport{
		WorkerRunID: input.WorkerRunID, ProgramRunID: input.ProgramRunID, ReviewRunID: input.ReviewRunID,
		Scope: scope, Coverage: coverage, Findings: validated, Evaluation: evaluation,
	}, nil
}

// checkWorkflowProgramEvidence 核对 Run、合同及工作区版本。失败可保留确定性证据。
func checkWorkflowProgramEvidence(input workflowVerificationInputs) error {
	evidence := input.ProgramEvidence
	if evidence == nil {
		if input.ProgramRef != nil {
			return errors.New("program evidence reference is missing its validated evidence")
		}
		return nil
	}
	if input.ProgramRef == nil || input.ProgramRunID == "" || evidence.RunID != string(input.ProgramRunID) ||
		evidence.AcceptanceDigest != input.Acceptance.Digest || evidence.Subject.Version != input.WorkspaceVersion {
		return errors.New("program evidence does not match its run, contract or workspace version")
	}
	if err := evidence.Validate(); err != nil {
		return fmt.Errorf("program evidence: %w", err)
	}
	if input.ProgramError != "" && evidence.Conclusion == contract.ConclusionPassed {
		return errors.New("failed program verification cannot supply passed evidence")
	}
	// 按合同重算标准状态，阻止检查明细与标准结论互相矛盾。
	checks := make(map[string]contract.AcceptanceCheckResult)
	for _, criterion := range evidence.Criteria {
		for _, check := range criterion.Checks {
			result := contract.AcceptanceCheckResult{
				CheckID: check.CheckID, Executed: check.Status != contract.CheckNotExecuted, Passed: check.Status == contract.CheckPassed,
			}
			if previous, found := checks[check.CheckID]; found && previous != result {
				return errors.New("program evidence has conflicting results for the same check")
			}
			checks[check.CheckID] = result
		}
	}
	results := make([]contract.AcceptanceCheckResult, 0, len(checks))
	for _, result := range checks {
		results = append(results, result)
	}
	expected, _, _ := contract.EvaluateAcceptance(input.Acceptance, results)
	if !slices.EqualFunc(evidence.Criteria, expected, func(actual, want contract.CriterionEvidence) bool {
		return actual.CriterionID == want.CriterionID && actual.Required == want.Required &&
			actual.Status == want.Status && slices.Equal(actual.Checks, want.Checks)
	}) {
		return errors.New("program evidence checks do not match the acceptance criteria")
	}
	return nil
}

// workflowReportFindings 绑定发现的目标和标准，只允许独立的复核结论。
func workflowReportFindings(input workflowVerificationInputs, criteria []string) ([]contract.Finding, error) {
	groups := []struct {
		status contract.FindingStatus
		items  []contract.Finding
	}{
		{status: contract.FindingConfirmed, items: input.Findings.Confirmed},
		{status: contract.FindingInconclusive, items: input.Findings.Inconclusive},
		{status: contract.FindingRejected, items: input.Findings.Rejected},
	}
	var findings []contract.Finding
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, finding := range group.items {
			if err := finding.Validate(); err != nil {
				return nil, fmt.Errorf("verification report finding %q: %w", finding.ID, err)
			}
			_, duplicate := seen[finding.ID]
			target := domain.RunID(finding.TargetID)
			if finding.Status != group.status || duplicate || (target != input.WorkerRunID && target != input.ProgramRunID) ||
				(finding.CriterionID != "" && !slices.Contains(criteria, finding.CriterionID)) {
				return nil, fmt.Errorf("verification report finding %q is duplicated or has an invalid binding", finding.ID)
			}
			if finding.Status == contract.FindingConfirmed && len(finding.Reviews[len(finding.Reviews)-1].EvidenceRefs) == 0 {
				return nil, fmt.Errorf("verification report finding %q has no confirmed evidence", finding.ID)
			}
			seen[finding.ID] = struct{}{}
			findings = append(findings, finding)
		}
	}
	return findings, nil
}

// workflowReportCoverage 将两个执行阶段投影到冻结对象，执行错误不伪造覆盖。
func workflowReportCoverage(scope contract.VerificationScope, input workflowVerificationInputs) (contract.CoverageManifest, error) {
	manifest, err := contract.NewCoverageManifest(scope)
	if err != nil {
		return contract.CoverageManifest{}, err
	}
	programRef := input.ProgramRef
	programError := input.ProgramError
	if input.ProgramEvidence != nil && !input.ProgramEvidence.Coverage.Complete {
		programRef = nil
		programError = "程序验证覆盖不完整"
	}
	stages := []struct {
		id            string
		runID         domain.RunID
		ref           *contract.Ref
		failed        bool
		failureReason string
	}{
		{workflowProgramCoverageID, input.ProgramRunID, programRef, programError != "", "程序验证缺少完整执行证据"},
		{workflowReviewCoverageID, input.ReviewRunID, input.ReviewRef, input.ReviewError != "", "独立审查缺少可信执行证据"},
	}
	updates := make([]contract.CoverageUpdate, 0, len(stages)*3)
	for _, stage := range stages {
		updates = append(updates, contract.CoverageUpdate{ObjectID: stage.id, Status: contract.CoveragePending})
		if stage.runID == "" && !stage.failed {
			continue
		}
		if stage.runID == "" {
			updates = append(updates, contract.CoverageUpdate{ObjectID: stage.id, Status: contract.CoverageSkipped, Reason: stage.failureReason})
			continue
		}
		updates = append(updates, contract.CoverageUpdate{ObjectID: stage.id, Status: contract.CoverageRunning})
		switch {
		case stage.ref != nil:
			ref := *stage.ref
			ref.TruncatedPreview = ""
			updates = append(updates, contract.CoverageUpdate{
				ObjectID: stage.id, Status: contract.CoverageCompleted, EvidenceRef: &ref,
				SubjectVersion: input.WorkspaceVersion, VerifierID: string(stage.runID), RulesetVersion: workflowRulesetVersion,
			})
		case stage.failed:
			updates = append(updates, contract.CoverageUpdate{ObjectID: stage.id, Status: contract.CoverageFailed, Reason: stage.failureReason})
		}
	}
	return manifest.ApplyBound(scope, updates)
}

// markWorkflowInconclusive 只降级受未定论发现影响的标准，保留确定性失败。
func markWorkflowInconclusive(evaluation *contract.AcceptanceEvaluation, findings []contract.Finding) {
	if len(findings) == 0 {
		return
	}
	const reason = "独立审查存在未定论发现，需要人工复核"
	affected := make(map[string]bool, len(findings))
	for _, finding := range findings {
		affected[finding.CriterionID] = true
	}
	for index := range evaluation.Criteria {
		criterion := &evaluation.Criteria[index]
		if affected[""] || affected[criterion.CriterionID] {
			switch criterion.Verdict {
			case contract.VerdictPassed, contract.VerdictUnknown:
				criterion.Verdict = contract.VerdictUnknown
				criterion.Reason = reason
			}
		}
	}
	if evaluation.Verdict == contract.VerdictPassed {
		evaluation.Verdict = contract.VerdictUnknown
	}
	if strings.TrimSpace(evaluation.Rationale) == "" {
		evaluation.Rationale = reason
	}
}
