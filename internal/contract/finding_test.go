// finding_test.go 回归 Finding 的校验、复核保护与阻塞判定。
//
// Author: fxbin
package contract

import "testing"

func proposedFinding(severity FindingSeverity) Finding {
	return Finding{
		Version: FindingVersion, ID: "finding-1", TargetID: "obj-1",
		CriterionID: "compile", Category: "correctness", Severity: severity,
		Description: "发现一个问题", Status: FindingProposed,
	}
}

func TestFindingProposedValid(t *testing.T) {
	finding := proposedFinding(SeverityMedium)
	if err := finding.Validate(); err != nil {
		t.Fatalf("validate proposed finding: %v", err)
	}
	if finding.IsBlocking() {
		t.Fatal("proposed finding must not block")
	}
}

func TestFindingNonProposedRequiresReview(t *testing.T) {
	finding := proposedFinding(SeverityHigh)
	finding.Status = FindingConfirmed
	if err := finding.Validate(); err == nil {
		t.Fatal("confirmed finding without review was accepted")
	}
	finding.Reviews = []FindingReview{{ReviewerID: "verifier-1", Outcome: FindingConfirmed, Rationale: "复核确认"}}
	if err := finding.Validate(); err != nil {
		t.Fatalf("confirmed finding with review: %v", err)
	}
	if !finding.IsBlocking() {
		t.Fatal("confirmed high-severity finding must block")
	}
}

func TestFindingHighSeverityRejectionNeedsEvidence(t *testing.T) {
	finding := proposedFinding(SeverityCritical)
	finding.Status = FindingRejected
	finding.Reviews = []FindingReview{{ReviewerID: "verifier-1", Outcome: FindingRejected}}
	if err := finding.Validate(); err == nil {
		t.Fatal("critical finding silently rejected without rationale or evidence")
	}
	finding.Reviews[0].Rationale = "已用确定性断言证明该问题不存在"
	if err := finding.Validate(); err != nil {
		t.Fatalf("rejected finding with rationale: %v", err)
	}
}

func TestFindingRejectsInvalidFields(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Finding)
	}{
		{"bad severity", func(f *Finding) { f.Severity = "urgent" }},
		{"bad status", func(f *Finding) { f.Status = "done" }},
		{"empty description", func(f *Finding) { f.Description = "" }},
		{"bad evidence ref", func(f *Finding) { f.EvidenceRefs = []Ref{{SHA256: "x"}} }},
		{"bad criterion", func(f *Finding) { f.CriterionID = "bad id" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			finding := proposedFinding(SeverityLow)
			testCase.mutate(&finding)
			if err := finding.Validate(); err == nil {
				t.Fatal("invalid finding was accepted")
			}
		})
	}
}

func TestFindingStatusMustMatchReviewOutcome(t *testing.T) {
	// 用 Confirmed 的 review 把 critical finding 标成 Rejected，绕过删除保护。
	finding := proposedFinding(SeverityCritical)
	finding.Status = FindingRejected
	finding.Reviews = []FindingReview{{ReviewerID: "v", Outcome: FindingConfirmed}}
	if err := finding.Validate(); err == nil {
		t.Fatal("rejected status with confirmed review was accepted")
	}
	// Inconclusive 同理不能伪装成 Rejected。
	finding.Reviews = []FindingReview{{ReviewerID: "v", Outcome: FindingInconclusive}}
	if err := finding.Validate(); err == nil {
		t.Fatal("rejected status with inconclusive review was accepted")
	}
}

func TestFindingResolvedRequiresReverificationEvidence(t *testing.T) {
	finding := proposedFinding(SeverityHigh)
	finding.Status = FindingResolved
	finding.Reviews = []FindingReview{{ReviewerID: "v", Outcome: FindingConfirmed, Rationale: "确认"}}
	if err := finding.Validate(); err == nil {
		t.Fatal("resolved finding without re-verification evidence was accepted")
	}
	finding.Reviews = append(finding.Reviews, FindingReview{ReviewerID: "v2", Outcome: FindingConfirmed, Rationale: "复核"})
	finding.EvidenceRefs = []Ref{{SHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}
	if err := finding.Validate(); err != nil {
		t.Fatalf("resolved finding with evidence: %v", err)
	}
	if finding.IsBlocking() {
		t.Fatal("resolved finding must not block")
	}
}

func TestConfirmedFindingIDsSorted(t *testing.T) {
	findings := []Finding{
		{Version: FindingVersion, ID: "z", TargetID: "o", Category: "c", Severity: SeverityLow, Description: "d", Status: FindingConfirmed, Reviews: []FindingReview{{ReviewerID: "v", Outcome: FindingConfirmed}}},
		{Version: FindingVersion, ID: "a", TargetID: "o", Category: "c", Severity: SeverityLow, Description: "d", Status: FindingConfirmed, Reviews: []FindingReview{{ReviewerID: "v", Outcome: FindingConfirmed}}},
	}
	ids := ConfirmedFindingIDs(findings)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "z" {
		t.Fatalf("confirmed finding ids not sorted: %v", ids)
	}
}

func TestFindingInconclusiveIsNotBlocking(t *testing.T) {
	finding := proposedFinding(SeverityHigh)
	finding.Status = FindingInconclusive
	finding.Reviews = []FindingReview{{ReviewerID: "verifier-1", Outcome: FindingInconclusive, Rationale: "证据不足，无法定论"}}
	if err := finding.Validate(); err != nil {
		t.Fatalf("validate inconclusive finding: %v", err)
	}
	if finding.IsBlocking() {
		t.Fatal("inconclusive finding must not block, but is not 'no problem' either")
	}
}
