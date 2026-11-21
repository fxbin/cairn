// verifier_findings_test.go 回归组合门面使用真实制品库校验证据。
//
// Author: fxbin
package app

import (
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

func TestValidateVerifierFindingsUsesRealArtifactStore(t *testing.T) {
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
	result := VerifierResult{VerifierRunID: domain.NewRunID(), Findings: []contract.Finding{realFinding, forgedFinding}}
	validated, err := ValidateVerifierFindings(result, stateDir, workspaceHash, "validator-1")
	if err != nil {
		t.Fatalf("validate verifier findings: %v", err)
	}
	// 真实证据的发现被确认，伪造证据的发现只能 Inconclusive。
	if len(validated.Confirmed) != 1 || validated.Confirmed[0].ID != "finding-1" {
		t.Fatalf("confirmed = %+v", validated.Confirmed)
	}
	if len(validated.Inconclusive) != 1 || validated.Inconclusive[0].ID != "finding-2" {
		t.Fatalf("inconclusive = %+v", validated.Inconclusive)
	}
	if len(validated.BlockingIDs) != 1 || validated.BlockingIDs[0] != "finding-1" {
		t.Fatalf("blocking ids = %v", validated.BlockingIDs)
	}
}

func TestValidateVerifierFindingsEmpty(t *testing.T) {
	validated, err := ValidateVerifierFindings(VerifierResult{}, t.TempDir(), "ws", "validator-1")
	if err != nil {
		t.Fatalf("validate empty findings: %v", err)
	}
	if len(validated.Confirmed) != 0 || len(validated.BlockingIDs) != 0 {
		t.Fatal("empty verifier output must not produce confirmed findings")
	}
}
