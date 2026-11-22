// [INPUT]: 程序检查结果、制品引用与已复核发现夹具。
// [OUTPUT]: 验证工作流报告的覆盖、结论与证据绑定。
// [POS]: 覆盖报告纯投影，不构造运行环境或访问存储。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package app

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

func TestWorkflowVerificationReportDistinguishesMissingReviewAndUnresolvedFinding(t *testing.T) {
	input := workflowVerificationReportFixture(t)
	report, err := buildWorkflowVerificationReport(input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Evaluation.Verdict != contract.VerdictPassed {
		t.Fatalf("complete evidence got %s, want passed", report.Evaluation.Verdict)
	}
	summary, err := report.Coverage.Summary(report.Scope)
	if err != nil || !summary.Complete {
		t.Fatalf("coverage did not bind to the frozen scope: %+v, %v", summary, err)
	}
	if report.Coverage.Entries[0].EvidenceRef.SHA256 != input.ProgramRef.SHA256 ||
		report.Coverage.Entries[1].EvidenceRef.SHA256 != input.ReviewRef.SHA256 {
		t.Fatal("report is missing verification artifact references")
	}
	if report.Coverage.Entries[0].EvidenceRef.TruncatedPreview != "" ||
		report.Coverage.Entries[1].EvidenceRef.TruncatedPreview != "" {
		t.Fatal("report retained raw artifact previews")
	}

	missing := input
	missing.ReviewRunID, missing.ReviewRef = "", nil
	withoutReview, err := buildWorkflowVerificationReport(missing)
	if err != nil {
		t.Fatal(err)
	}
	if withoutReview.Evaluation.Verdict != contract.VerdictUnknown ||
		withoutReview.Coverage.Entries[1].Status != contract.CoveragePending {
		t.Fatalf("missing review was credited: %+v", withoutReview)
	}

	input.Findings.Inconclusive = []contract.Finding{{
		Version: contract.FindingVersion, ID: "unresolved", TargetID: string(input.WorkerRunID),
		CriterionID: "compile", Category: "correctness", Severity: contract.SeverityHigh,
		Description: "候选问题需要复核", Status: contract.FindingInconclusive,
		Reviews: []contract.FindingReview{{ReviewerID: "host", Outcome: contract.FindingInconclusive}},
	}}
	input.Findings.BlockingIDs = []string{"unresolved"}
	withFinding, err := buildWorkflowVerificationReport(input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Scope.Digest != withFinding.Scope.Digest || report.Coverage.Digest != withFinding.Coverage.Digest {
		t.Fatal("finding changed frozen coverage facts")
	}
	evaluation := withFinding.Evaluation
	if evaluation.Verdict != contract.VerdictUnknown || evaluation.Criteria[0].Verdict != contract.VerdictUnknown ||
		evaluation.Criteria[1].Verdict != contract.VerdictPassed || !strings.Contains(evaluation.Rationale, "人工复核") {
		t.Fatalf("unresolved review did not hold the affected criterion: %+v", evaluation)
	}
	if len(evaluation.BlockingFindings) != 0 || len(withFinding.Findings.BlockingIDs) != 0 {
		t.Fatal("unresolved finding became a confirmed blocker")
	}
}

func TestWorkflowVerificationReportSeparatesFailedCheckAndInfrastructureError(t *testing.T) {
	input := workflowVerificationReportFixture(t)
	input.ReviewRunID, input.ReviewRef = "", nil
	input.ProgramError = "program failed"
	input.ProgramEvidence, input.ProgramRef = nil, nil
	report, err := buildWorkflowVerificationReport(input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Evaluation.Verdict != contract.VerdictUnknown || report.Coverage.Entries[0].Status != contract.CoverageFailed {
		t.Fatalf("program error without evidence was misclassified: %+v", report)
	}

	deterministic := workflowVerificationReportFixture(t)
	deterministic.ReviewRunID, deterministic.ReviewRef = "", nil
	deterministic.ProgramError = "check failed"
	deterministic.ProgramEvidence.Criteria, deterministic.ProgramEvidence.Coverage, deterministic.ProgramEvidence.Conclusion =
		contract.EvaluateAcceptance(deterministic.Acceptance, []contract.AcceptanceCheckResult{{CheckID: "build", Executed: true}})
	report, err = buildWorkflowVerificationReport(deterministic)
	if err != nil {
		t.Fatal(err)
	}
	if report.Evaluation.Verdict != contract.VerdictFailed || report.Evaluation.Criteria[0].Verdict != contract.VerdictFailed ||
		report.Evaluation.Criteria[1].Verdict != contract.VerdictUnknown || report.Coverage.Entries[0].Status != contract.CoverageFailed {
		t.Fatalf("deterministic failure or coverage gap was lost: %+v", report)
	}
}

func TestWorkflowVerificationReportRequiresEvidenceForConfirmedFindings(t *testing.T) {
	input := workflowVerificationReportFixture(t)
	input.Findings.Confirmed = []contract.Finding{{
		Version: contract.FindingVersion, ID: "confirmed", TargetID: string(input.WorkerRunID),
		CriterionID: "behavior", Category: "correctness", Severity: contract.SeverityHigh,
		Description: "确定性复核发现问题", Status: contract.FindingConfirmed,
		Reviews: []contract.FindingReview{{ReviewerID: "host", Outcome: contract.FindingConfirmed}},
	}}
	if _, err := buildWorkflowVerificationReport(input); err == nil {
		t.Fatal("report accepted a confirmation without evidence")
	}

	evidence := *input.ReviewRef
	finding := &input.Findings.Confirmed[0]
	finding.EvidenceRefs = []contract.Ref{evidence}
	finding.Reviews[0].EvidenceRefs = []contract.Ref{evidence}
	report, err := buildWorkflowVerificationReport(input)
	if err != nil {
		t.Fatal(err)
	}
	if report.Evaluation.Verdict != contract.VerdictFailed || report.Evaluation.Criteria[1].Verdict != contract.VerdictFailed ||
		len(report.Findings.BlockingIDs) != 1 || report.Findings.BlockingIDs[0] != finding.ID {
		t.Fatalf("confirmed finding did not block acceptance: %+v", report.Evaluation)
	}
}

func TestWorkflowVerificationReportRejectsUnboundProgramEvidence(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*workflowVerificationInputs)
	}{
		{"run", func(input *workflowVerificationInputs) { input.ProgramEvidence.RunID = string(input.WorkerRunID) }},
		{"contract", func(input *workflowVerificationInputs) { input.ProgramEvidence.AcceptanceDigest = "foreign" }},
		{"workspace version", func(input *workflowVerificationInputs) { input.ProgramEvidence.Subject.Version = "stale" }},
		{"missing reference", func(input *workflowVerificationInputs) { input.ProgramRef = nil }},
		{"wrong check", func(input *workflowVerificationInputs) {
			input.ProgramEvidence.Criteria[0].Checks[0].CheckID = "different"
		}},
		{"inconsistent status", func(input *workflowVerificationInputs) {
			input.ProgramEvidence.Criteria[0].Checks[0].Status = contract.CheckFailed
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := workflowVerificationReportFixture(t)
			test.change(&input)
			if _, err := buildWorkflowVerificationReport(input); err == nil {
				t.Fatal("report accepted invalid evidence")
			}
		})
	}
}

func workflowVerificationReportFixture(t *testing.T) workflowVerificationInputs {
	t.Helper()
	acceptance, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
		{ID: "behavior", Statement: "功能测试通过", Required: true, Checks: []string{"test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	version := "sha256:" + strings.Repeat("a", 64)
	programRef := &contract.Ref{SHA256: "sha256:" + strings.Repeat("b", 64), Bytes: 10, MediaType: "application/json", TruncatedPreview: "raw preview"}
	reviewRef := &contract.Ref{SHA256: "sha256:" + strings.Repeat("c", 64), Bytes: 10, MediaType: "application/json", TruncatedPreview: "raw preview"}
	criteria, coverage, conclusion := contract.EvaluateAcceptance(acceptance, []contract.AcceptanceCheckResult{
		{CheckID: "build", Executed: true, Passed: true},
		{CheckID: "test", Executed: true, Passed: true},
	})
	return workflowVerificationInputs{
		Acceptance: acceptance, WorkerRunID: "run-worker", ProgramRunID: "run-program", ReviewRunID: "run-review",
		WorkspaceVersion: version, ProgramRef: programRef, ReviewRef: reviewRef,
		ProgramEvidence: &contract.AcceptanceEvidence{
			Version: contract.AcceptanceEvidenceVersion, RunID: "run-program", AcceptanceDigest: acceptance.Digest,
			Subject:  contract.EvidenceSubject{Kind: contract.EvidenceKindWorkspace, ID: "workspace", Version: version},
			Producer: contract.EvidenceProducer{Kind: contract.EvidenceProducerEngine, ID: contract.EvidenceProducerID},
			Source:   contract.EvidenceSource{VerificationID: "verify-program", CommandsFingerprint: "commands"},
			Criteria: criteria, Coverage: coverage, Conclusion: conclusion,
		},
	}
}
