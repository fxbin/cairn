// verifier_findings_test.go 回归真实制品引用不足以证明独立审查提出的缺陷。
//
// Author: fxbin
package app

import (
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

func TestValidateVerifierFindingsRequiresCorroboration(t *testing.T) {
	stateDir, workspaceHash := t.TempDir(), "ws"
	store, err := artifact.New(manifestArtifactRoot(stateDir, workspaceHash), 1024)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	// 真实写入一份证据制品，取真实摘要。
	ref, err := store.Put([]byte(`{"evidence":"real"}`), "application/json")
	if err != nil {
		t.Fatalf("store evidence: %v", err)
	}
	realFinding := proposedFindingWith("f-real", contract.SeverityHigh, "obj:1", []contract.Ref{ref})
	realFinding.ID = "finding-1"
	forgedFinding := proposedFindingWith("f-fake", contract.SeverityHigh, "obj:2", []contract.Ref{
		{SHA256: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	})
	forgedFinding.ID = "finding-2"
	unrelated, err := store.Put([]byte("other run's artifact"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	unrelatedFinding := proposedFindingWith("finding-3", contract.SeverityMedium, "obj:3", []contract.Ref{unrelated})
	result := VerifierResult{
		VerifierRunID: domain.NewRunID(),
		Findings:      []contract.Finding{realFinding, forgedFinding, unrelatedFinding},
		target:        verifierContext{TargetRunID: "obj-1", Artifacts: []verifierArtifact{{Ref: ref}}},
	}
	validated, err := ValidateVerifierFindings(result, "validator-1")
	if err != nil {
		t.Fatalf("validate verifier findings: %v", err)
	}
	if len(validated.Confirmed) != 0 || len(validated.BlockingIDs) != 0 {
		t.Fatalf("artifact existence confirmed a defect: %+v", validated)
	}
	if len(validated.Inconclusive) != 3 || validated.Inconclusive[0].ID != "finding-1" || validated.Inconclusive[1].ID != "finding-2" || validated.Inconclusive[2].ID != "finding-3" {
		t.Fatalf("inconclusive = %+v", validated.Inconclusive)
	}
	if got := validated.Inconclusive[0].EvidenceRefs; len(got) != 1 || got[0].SHA256 != ref.SHA256 {
		t.Fatalf("original investigation evidence was lost: %+v", got)
	}
}

func TestValidateVerifierFindingsEmpty(t *testing.T) {
	validated, err := ValidateVerifierFindings(VerifierResult{}, "validator-1")
	if err != nil {
		t.Fatalf("validate empty findings: %v", err)
	}
	if len(validated.Confirmed) != 0 || len(validated.BlockingIDs) != 0 {
		t.Fatal("empty verifier output must not produce confirmed findings")
	}
}
