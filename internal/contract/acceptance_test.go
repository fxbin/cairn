// acceptance_test.go 回归验收合同与证据的校验、摘要和结论公式。
//
// Author: fxbin
package contract

import (
	"strings"
	"testing"
)

func testAcceptanceCriteria() []AcceptanceCriterion {
	return []AcceptanceCriterion{
		{ID: "compile", Statement: "项目可成功编译", Required: true, Checks: []string{"build"}},
		{ID: "suite", Statement: "测试套件通过", Required: true, Checks: []string{"test", "lint"}},
		{ID: "style", Statement: "代码风格良好", Required: false, Checks: []string{"lint"}},
	}
}

func TestAcceptanceContractDigestStable(t *testing.T) {
	first, err := NewAcceptanceContract(testAcceptanceCriteria())
	if err != nil {
		t.Fatalf("new contract: %v", err)
	}
	second, err := NewAcceptanceContract(testAcceptanceCriteria())
	if err != nil {
		t.Fatalf("new contract again: %v", err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest is not stable: %q vs %q", first.Digest, second.Digest)
	}
	if !strings.HasPrefix(first.Digest, "sha256:") {
		t.Fatalf("unexpected digest format: %q", first.Digest)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	changed := testAcceptanceCriteria()
	changed[0].Statement = "项目可成功编译且无警告"
	third, err := NewAcceptanceContract(changed)
	if err != nil {
		t.Fatalf("new changed contract: %v", err)
	}
	if third.Digest == first.Digest {
		t.Fatal("different criteria produced the same digest")
	}
}

func TestAcceptanceContractRejectsInvalid(t *testing.T) {
	cases := []struct {
		name     string
		criteria []AcceptanceCriterion
	}{
		{"empty", nil},
		{"duplicate id", []AcceptanceCriterion{{ID: "a", Statement: "s", Required: true, Checks: []string{"x"}}, {ID: "a", Statement: "t", Required: false, Checks: []string{"y"}}}},
		{"no required", []AcceptanceCriterion{{ID: "a", Statement: "s", Checks: []string{"x"}}}},
		{"no checks", []AcceptanceCriterion{{ID: "a", Statement: "s", Required: true}}},
		{"bad id", []AcceptanceCriterion{{ID: "a b", Statement: "s", Required: true, Checks: []string{"x"}}}},
		{"secret statement", []AcceptanceCriterion{{ID: "a", Statement: "token=abc123", Required: true, Checks: []string{"x"}}}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if _, err := NewAcceptanceContract(testCase.criteria); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestAcceptanceContractTamperedDigest(t *testing.T) {
	contract, err := NewAcceptanceContract(testAcceptanceCriteria())
	if err != nil {
		t.Fatalf("new contract: %v", err)
	}
	contract.Digest = "sha256:deadbeef"
	if err := contract.Validate(); err == nil {
		t.Fatal("tampered digest was accepted")
	}
}

func TestEvaluateAcceptanceConclusion(t *testing.T) {
	accepted, err := NewAcceptanceContract(testAcceptanceCriteria())
	if err != nil {
		t.Fatalf("new contract: %v", err)
	}
	allPassed := []AcceptanceCheckResult{
		{CheckID: "build", Executed: true, Passed: true},
		{CheckID: "test", Executed: true, Passed: true},
		{CheckID: "lint", Executed: true, Passed: true},
	}
	criteria, coverage, conclusion := EvaluateAcceptance(accepted, allPassed)
	if conclusion != ConclusionPassed {
		t.Fatalf("expected passed conclusion, got %s", conclusion)
	}
	if coverage.Required != 2 || coverage.Covered != 2 || coverage.Passed != 2 || !coverage.Complete {
		t.Fatalf("unexpected coverage: %+v", coverage)
	}
	if len(criteria) != 3 {
		t.Fatalf("unexpected criteria count: %d", len(criteria))
	}

	missingLint := []AcceptanceCheckResult{
		{CheckID: "build", Executed: true, Passed: true},
		{CheckID: "test", Executed: true, Passed: true},
	}
	_, coverage, conclusion = EvaluateAcceptance(accepted, missingLint)
	if conclusion != ConclusionIncomplete || coverage.Complete {
		t.Fatalf("expected incomplete conclusion, got %s coverage=%+v", conclusion, coverage)
	}

	failedBuild := []AcceptanceCheckResult{
		{CheckID: "build", Executed: true, Passed: false},
		{CheckID: "test", Executed: true, Passed: true},
		{CheckID: "lint", Executed: true, Passed: true},
	}
	_, _, conclusion = EvaluateAcceptance(accepted, failedBuild)
	if conclusion != ConclusionFailed {
		t.Fatalf("expected failed conclusion, got %s", conclusion)
	}
}

func TestAcceptanceEvidenceSatisfies(t *testing.T) {
	accepted, err := NewAcceptanceContract(testAcceptanceCriteria())
	if err != nil {
		t.Fatalf("new contract: %v", err)
	}
	criteria, coverage, conclusion := EvaluateAcceptance(accepted, []AcceptanceCheckResult{
		{CheckID: "build", Executed: true, Passed: true},
		{CheckID: "test", Executed: true, Passed: true},
		{CheckID: "lint", Executed: true, Passed: true},
	})
	evidence := AcceptanceEvidence{
		Version:          AcceptanceEvidenceVersion,
		RunID:            "run-1",
		AcceptanceDigest: accepted.Digest,
		Subject:          EvidenceSubject{Kind: EvidenceKindWorkspace, ID: "sha256:ws", Version: "sha256:obs"},
		Producer:         EvidenceProducer{Kind: EvidenceProducerEngine, ID: EvidenceProducerID},
		Source:           EvidenceSource{VerificationID: "verify-1", CommandsFingerprint: "sha256:fp"},
		Criteria:         criteria,
		Coverage:         coverage,
		Conclusion:       conclusion,
	}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("validate evidence: %v", err)
	}
	if err := evidence.Satisfies(accepted, "sha256:ws", "sha256:obs"); err != nil {
		t.Fatalf("satisfies: %v", err)
	}
	if err := evidence.Satisfies(accepted, "sha256:ws", "sha256:stale"); err == nil {
		t.Fatal("stale subject version was accepted")
	}
	tampered := evidence
	tampered.Coverage.Complete = false
	if err := tampered.Validate(); err == nil {
		t.Fatal("inconsistent coverage was accepted")
	}
}
