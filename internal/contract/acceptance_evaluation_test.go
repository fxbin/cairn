// acceptance_evaluation_test.go 回归多级验收结论的保守判定。
//
// Author: fxbin
package contract

import "testing"

func evalAcceptance(t *testing.T) AcceptanceContract {
	t.Helper()
	accepted, err := NewAcceptanceContract([]AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
		{ID: "docs", Statement: "文档完整", Required: false, Checks: []string{"docs"}},
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	return accepted
}

func evalEvidence(t *testing.T, accepted AcceptanceContract, status string) *AcceptanceEvidence {
	t.Helper()
	criteria := []CriterionEvidence{
		{CriterionID: "compile", Required: true, Status: status, Checks: []CheckEvidence{{CheckID: "build", Status: status}}},
		{CriterionID: "docs", Required: false, Status: status, Checks: []CheckEvidence{{CheckID: "docs", Status: status}}},
	}
	// 必需标准 1 条；通过时覆盖与通过数均为 1，失败时覆盖 1 通过 0。
	coverage := AcceptanceCoverage{Required: 1, Covered: 1, Passed: 1, Complete: true}
	conclusion := ConclusionPassed
	if status == CheckFailed {
		coverage.Passed = 0
		conclusion = ConclusionFailed
	}
	return &AcceptanceEvidence{
		Version: AcceptanceEvidenceVersion, RunID: "run-1", AcceptanceDigest: accepted.Digest,
		Subject:  EvidenceSubject{Kind: EvidenceKindWorkspace, ID: "ws", Version: "sha256:obs"},
		Producer: EvidenceProducer{Kind: EvidenceProducerEngine, ID: EvidenceProducerID},
		Source:   EvidenceSource{VerificationID: "v1", CommandsFingerprint: "sha256:fp"},
		Criteria: criteria, Coverage: coverage, Conclusion: conclusion,
	}
}

func TestEvaluateAcceptanceVerdictPassed(t *testing.T) {
	accepted := evalAcceptance(t)
	input := EvaluationInput{
		Acceptance: accepted,
		Coverage:   CoverageSummary{Total: 2, Required: 1, Covered: 1, Complete: true},
		Evidence:   evalEvidence(t, accepted, CheckPassed),
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Verdict != VerdictPassed {
		t.Fatalf("verdict = %s, want passed", evaluation.Verdict)
	}
	if !AutoAcceptAllowed(evaluation, "frozen-auto-accept-token") {
		t.Fatal("passed evaluation with authorization should allow auto-accept")
	}
	if AutoAcceptAllowed(evaluation, "") {
		t.Fatal("auto-accept without authorization must be rejected")
	}
}

func TestEvaluateAcceptanceVerdictUnknownNotPassed(t *testing.T) {
	accepted := evalAcceptance(t)
	// 无证据 → Unknown，且覆盖不完整。
	input := EvaluationInput{
		Acceptance: accepted,
		Coverage:   CoverageSummary{Total: 2, Required: 1, Covered: 0, Complete: false, UncoveredRequired: []string{"compile"}},
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Verdict != VerdictUnknown {
		t.Fatalf("verdict = %s, want unknown", evaluation.Verdict)
	}
	if AutoAcceptAllowed(evaluation, "frozen-auto-accept-token") {
		t.Fatal("unknown verdict must never auto-accept")
	}
}

func TestEvaluateAcceptanceVerdictFailedByFinding(t *testing.T) {
	accepted := evalAcceptance(t)
	finding := Finding{
		Version: FindingVersion, ID: "finding-1", TargetID: "obj-1", CriterionID: "compile",
		Category: "correctness", Severity: SeverityHigh, Description: "发现确定缺陷",
		Status:  FindingConfirmed,
		Reviews: []FindingReview{{ReviewerID: "verifier-1", Outcome: FindingConfirmed, Rationale: "复核确认"}},
	}
	input := EvaluationInput{
		Acceptance: accepted,
		Coverage:   CoverageSummary{Total: 2, Required: 1, Covered: 1, Complete: true},
		Evidence:   evalEvidence(t, accepted, CheckPassed),
		Findings:   []Finding{finding},
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Verdict != VerdictFailed {
		t.Fatalf("verdict = %s, want failed", evaluation.Verdict)
	}
	if len(evaluation.BlockingFindings) != 1 || evaluation.BlockingFindings[0] != "finding-1" {
		t.Fatalf("blocking findings = %v", evaluation.BlockingFindings)
	}
}

func TestEvaluateAcceptanceVerdictWaivedNotPassed(t *testing.T) {
	accepted := evalAcceptance(t)
	input := EvaluationInput{
		Acceptance: accepted,
		Coverage:   CoverageSummary{Total: 2, Required: 1, Covered: 0, Complete: false, UncoveredRequired: []string{"compile"}},
		Waivers:    []AcceptanceWaiver{{CriterionID: "compile", Reason: "用户明确豁免", Authorization: "user-approved"}},
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Verdict == VerdictPassed {
		t.Fatal("waived verdict must not equal passed")
	}
	if AutoAcceptAllowed(evaluation, "frozen-auto-accept-token") {
		t.Fatal("waived verdict must never auto-accept")
	}
}

func TestEvaluateAcceptanceVerdictNotApplicable(t *testing.T) {
	accepted := evalAcceptance(t)
	input := EvaluationInput{
		Acceptance: accepted,
		Coverage:   CoverageSummary{Total: 2, Required: 1, Covered: 0, Complete: false, UncoveredRequired: []string{"compile"}},
		NotApplicable: []NotApplicableBasis{{CriterionID: "compile", Reason: "依据冻结规则不适用",
			BasisRef: Ref{SHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}},
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Verdict != VerdictNotApplicable {
		t.Fatalf("verdict = %s, want not_applicable", evaluation.Verdict)
	}
}

func TestEvaluateAcceptanceVerdictCheckFailedIsFailed(t *testing.T) {
	accepted := evalAcceptance(t)
	input := EvaluationInput{
		Acceptance: accepted,
		Coverage:   CoverageSummary{Total: 2, Required: 1, Covered: 1, Complete: true},
		Evidence:   evalEvidence(t, accepted, CheckFailed),
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	// CheckFailed 是已确认不符合，须判 Failed，而不是笼统 Unknown。
	if evaluation.Verdict != VerdictFailed {
		t.Fatalf("verdict = %s, want failed", evaluation.Verdict)
	}
}

// 全局证据结论为 Failed 时，即便逐标准状态看起来通过，整体也不得 Passed。
// 用自洽的失败证据（存在失败的标准）构造，断言整体 Failed 而非 Unknown。
func TestEvaluateAcceptanceVerdictGlobalConclusionFailed(t *testing.T) {
	accepted := evalAcceptance(t)
	input := EvaluationInput{
		Acceptance: accepted,
		Coverage:   CoverageSummary{Total: 2, Required: 1, Covered: 1, Complete: true},
		Evidence:   evalEvidence(t, accepted, CheckFailed),
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Verdict != VerdictFailed {
		t.Fatalf("verdict = %s, want failed", evaluation.Verdict)
	}
	if AutoAcceptAllowed(evaluation, "frozen-auto-accept-token") {
		t.Fatal("failed evidence must never auto-accept")
	}
}

func TestAutoAcceptRejectsForgedEvaluation(t *testing.T) {
	// 零值/伪造 evaluation 即使带授权也不得自动验收。
	if AutoAcceptAllowed(AcceptanceEvaluation{Verdict: VerdictPassed}, "frozen-auto-accept-token") {
		t.Fatal("forged zero-value evaluation was auto-accepted")
	}
	// 合法结构但合同摘要格式错误也要拒绝。
	evaluation := AcceptanceEvaluation{
		Version: AcceptanceEvaluationVersion, AcceptanceDigest: "sha256:x",
		Verdict:  VerdictPassed,
		Criteria: []CriterionVerdict{{CriterionID: "compile", Required: true, Verdict: VerdictPassed}},
	}
	if AutoAcceptAllowed(evaluation, "frozen-auto-accept-token") {
		t.Fatal("evaluation with invalid digest format was auto-accepted")
	}
}

func TestWaiverWithBlockingFindingStillFail(t *testing.T) {
	accepted := evalAcceptance(t)
	finding := Finding{
		Version: FindingVersion, ID: "finding-1", TargetID: "obj-1", CriterionID: "compile",
		Category: "correctness", Severity: SeverityHigh, Description: "发现确定缺陷",
		Status:  FindingConfirmed,
		Reviews: []FindingReview{{ReviewerID: "v", Outcome: FindingConfirmed, Rationale: "确认"}},
	}
	input := EvaluationInput{
		Acceptance: accepted, Coverage: CoverageSummary{Total: 2, Required: 1, Complete: true},
		Waivers:  []AcceptanceWaiver{{CriterionID: "compile", Reason: "豁免", Authorization: "user-approved"}},
		Findings: []Finding{finding},
	}
	evaluation, err := EvaluateAcceptanceVerdict(input)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if evaluation.Verdict != VerdictFailed {
		t.Fatalf("waiver must not mask blocking finding: verdict = %s", evaluation.Verdict)
	}
}

func TestEvaluateAcceptanceVerdictRejectsMalformedInputs(t *testing.T) {
	accepted := evalAcceptance(t)
	cases := []struct {
		name   string
		mutate func(*EvaluationInput)
	}{
		{"stale evidence", func(in *EvaluationInput) {
			in.Evidence = evalEvidence(t, accepted, CheckPassed)
			in.Evidence.AcceptanceDigest = "sha256:stale"
		}},
		{"waiver no authorization", func(in *EvaluationInput) { in.Waivers = []AcceptanceWaiver{{CriterionID: "compile", Reason: "r"}} }},
		{"waiver unknown criterion", func(in *EvaluationInput) {
			in.Waivers = []AcceptanceWaiver{{CriterionID: "missing", Reason: "r", Authorization: "a"}}
		}},
		{"not-applicable bad ref", func(in *EvaluationInput) {
			in.NotApplicable = []NotApplicableBasis{{CriterionID: "compile", Reason: "r", BasisRef: Ref{SHA256: "x"}}}
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := EvaluationInput{Acceptance: accepted}
			testCase.mutate(&input)
			if _, err := EvaluateAcceptanceVerdict(input); err == nil {
				t.Fatal("malformed evaluation input was accepted")
			}
		})
	}
}
