// finding_validation_test.go 回归 Finding 程序核验的保守规则与去重。
//
// Author: fxbin
package app

import (
	"errors"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

func proposedFindingWith(id string, severity contract.FindingSeverity, location string, evidence []contract.Ref) contract.Finding {
	return contract.Finding{
		Version: contract.FindingVersion, ID: id, TargetID: "obj-1", Category: "correctness",
		Severity: severity, Description: "发现一个问题", Status: contract.FindingProposed,
		Location: location, EvidenceRefs: evidence,
	}
}

func validRef() contract.Ref {
	return contract.Ref{SHA256: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
}

// acceptKnownEvidence 只接受 validRef，模拟真实制品库校验；其余摘要视为伪造。
func acceptKnownEvidence(ref contract.Ref) error {
	if ref.SHA256 == validRef().SHA256 {
		return nil
	}
	return errors.New("artifact not found")
}

func TestValidateFindingConfirmsWithEvidence(t *testing.T) {
	finding := proposedFindingWith("f-1", contract.SeverityHigh, "obj-1:10", []contract.Ref{validRef()})
	validation, err := ValidateFinding(finding, "validator-1", acceptKnownEvidence)
	if err != nil {
		t.Fatalf("validate finding: %v", err)
	}
	if validation.Finding.Status != contract.FindingConfirmed {
		t.Fatalf("status = %s, want confirmed", validation.Finding.Status)
	}
	if len(validation.Finding.Reviews) != 1 || validation.Finding.Reviews[0].Outcome != contract.FindingConfirmed {
		t.Fatal("confirmation review missing")
	}
}

func TestValidateFindingRejectsForgedEvidence(t *testing.T) {
	// 格式合法但制品库不存在的摘要不得确认高风险发现。
	forged := contract.Ref{SHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	finding := proposedFindingWith("f-1", contract.SeverityCritical, "obj-1:10", []contract.Ref{forged})
	validation, err := ValidateFinding(finding, "validator-1", acceptKnownEvidence)
	if err != nil {
		t.Fatalf("validate finding: %v", err)
	}
	if validation.Finding.Status != contract.FindingInconclusive {
		t.Fatalf("status = %s, want inconclusive for forged evidence", validation.Finding.Status)
	}
	// 没有制品校验器时也不得确认。
	noVerifier, err := ValidateFinding(proposedFindingWith("f-2", contract.SeverityHigh, "loc", []contract.Ref{validRef()}), "validator-1", nil)
	if err != nil {
		t.Fatalf("validate finding without verifier: %v", err)
	}
	if noVerifier.Finding.Status != contract.FindingInconclusive {
		t.Fatal("finding confirmed without any evidence verifier")
	}
}

func TestValidateFindingInconclusiveWithoutEvidence(t *testing.T) {
	// 高风险无证据不得确认，也不得当作没问题。
	finding := proposedFindingWith("f-1", contract.SeverityCritical, "obj-1:10", nil)
	validation, err := ValidateFinding(finding, "validator-1", acceptKnownEvidence)
	if err != nil {
		t.Fatalf("validate finding: %v", err)
	}
	if validation.Finding.Status != contract.FindingInconclusive {
		t.Fatalf("status = %s, want inconclusive", validation.Finding.Status)
	}
	if validation.Finding.Description != "发现一个问题" {
		t.Fatal("original finding was lost")
	}
}

func TestValidateFindingLocationIsNotEvidence(t *testing.T) {
	for _, severity := range []contract.FindingSeverity{contract.SeverityLow, contract.SeverityMedium, contract.SeverityHigh, contract.SeverityCritical} {
		t.Run(string(severity), func(t *testing.T) {
			finding := proposedFindingWith("f-1", severity, "obj-1:10", nil)
			validation, err := ValidateFinding(finding, "validator-1", acceptKnownEvidence)
			if err != nil {
				t.Fatal(err)
			}
			if validation.Finding.Status != contract.FindingInconclusive || len(BlockingFindingIDs([]contract.Finding{validation.Finding})) != 0 {
				t.Fatalf("location without evidence produced a confirmed finding: %+v", validation)
			}
		})
	}
}

func TestRejectFindingRequiresBasisForHighRisk(t *testing.T) {
	finding := proposedFindingWith("f-1", contract.SeverityHigh, "obj-1:10", []contract.Ref{validRef()})
	if _, err := RejectFinding(finding, "validator-1", "", nil); err == nil {
		t.Fatal("high-severity rejection without basis was accepted")
	}
	validation, err := RejectFinding(finding, "validator-1", "用确定性断言证明该问题不存在", nil)
	if err != nil {
		t.Fatalf("reject finding: %v", err)
	}
	if validation.Finding.Status != contract.FindingRejected {
		t.Fatalf("status = %s, want rejected", validation.Finding.Status)
	}
	if validation.Finding.Description != "发现一个问题" || validation.Finding.Reviews[0].Rationale == "" {
		t.Fatal("rejection did not preserve original finding and rationale")
	}
}

func TestDeduplicateAndValidate(t *testing.T) {
	findings := []contract.Finding{
		proposedFindingWith("f-2", contract.SeverityHigh, "obj:2", []contract.Ref{validRef()}),
		proposedFindingWith("f-1", contract.SeverityHigh, "obj:1", []contract.Ref{validRef()}),
		proposedFindingWith("f-2", contract.SeverityHigh, "obj:dup", nil), // 重复项
		proposedFindingWith("f-3", contract.SeverityCritical, "obj:3", nil),
	}
	confirmed, inconclusive, rejected, err := DeduplicateAndValidate(findings, "validator-1", acceptKnownEvidence)
	if err != nil {
		t.Fatalf("deduplicate: %v", err)
	}
	if len(confirmed) != 2 || confirmed[0].ID != "f-1" || confirmed[1].ID != "f-2" {
		t.Fatalf("confirmed = %+v", confirmed)
	}
	if len(inconclusive) != 1 || inconclusive[0].ID != "f-3" {
		t.Fatalf("inconclusive = %+v", inconclusive)
	}
	if len(rejected) != 0 {
		t.Fatalf("rejected = %+v", rejected)
	}
}

func TestDeduplicateMergesDuplicateEvidence(t *testing.T) {
	// 重复项携带另一份真实证据，必须合并进保留项而非丢弃。
	forged := contract.Ref{SHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}
	first := proposedFindingWith("f-1", contract.SeverityHigh, "obj:1", []contract.Ref{validRef()})
	duplicate := proposedFindingWith("f-1", contract.SeverityHigh, "obj:1", []contract.Ref{forged})
	confirmed, _, _, err := DeduplicateAndValidate([]contract.Finding{first, duplicate}, "validator-1", acceptKnownEvidence)
	if err != nil {
		t.Fatalf("deduplicate: %v", err)
	}
	if len(confirmed) != 1 {
		t.Fatalf("confirmed = %d, want 1", len(confirmed))
	}
	// 合并后含 2 条证据（1 真实 + 1 伪造），原始与重复证据都保留。
	if len(confirmed[0].EvidenceRefs) != 2 {
		t.Fatalf("merged evidence refs = %d, want 2", len(confirmed[0].EvidenceRefs))
	}
}

func TestBlockingFindingIDs(t *testing.T) {
	findings := []contract.Finding{
		proposedFindingWith("f-2", contract.SeverityHigh, "obj:2", []contract.Ref{validRef()}),
		proposedFindingWith("f-1", contract.SeverityMedium, "obj:1", []contract.Ref{validRef()}),
	}
	confirmed, _, _, err := DeduplicateAndValidate(findings, "validator-1", acceptKnownEvidence)
	if err != nil {
		t.Fatalf("deduplicate: %v", err)
	}
	ids := BlockingFindingIDs(confirmed)
	if len(ids) != 2 || ids[0] != "f-1" || ids[1] != "f-2" {
		t.Fatalf("blocking ids = %v", ids)
	}
}
