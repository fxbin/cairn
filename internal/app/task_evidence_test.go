// task_evidence_test.go 回归任务证据投影：零变更验收字段被接受，绑定漂移被拒绝。
//
// Author: fxbin
package app

import (
	"encoding/json"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/runtime/artifact"
)

func TestAcceptanceEvidenceFromRunAcceptsZeroMutationSeq(t *testing.T) {
	accepted, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	stateDir, workspaceHash := t.TempDir(), "ws"
	store, err := artifact.New(manifestArtifactRoot(stateDir, workspaceHash), 1024)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	planRef := &contract.Ref{SHA256: "sha256:" + repeatHex('a', 64), Bytes: 10, MediaType: "application/json"}
	criteria, coverage, conclusion := contract.EvaluateAcceptance(accepted, []contract.AcceptanceCheckResult{
		{CheckID: "build", Executed: true, Passed: true},
	})
	evidence := contract.AcceptanceEvidence{
		Version:          contract.AcceptanceEvidenceVersion,
		RunID:            "run-1",
		AcceptanceDigest: accepted.Digest,
		Subject:          contract.EvidenceSubject{Kind: contract.EvidenceKindWorkspace, ID: workspaceHash, Version: "sha256:obs"},
		Producer:         contract.EvidenceProducer{Kind: contract.EvidenceProducerEngine, ID: contract.EvidenceProducerID},
		Source:           contract.EvidenceSource{VerificationID: "verify-1", PlanRef: planRef, CommandsFingerprint: "sha256:fp"},
		Criteria:         criteria,
		Coverage:         coverage,
		Conclusion:       conclusion,
	}
	content, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	ref, err := store.Put(content, "application/json")
	if err != nil {
		t.Fatalf("store evidence: %v", err)
	}
	turnID := domain.NewTurnID()
	zero := uint64(0)
	events := []domain.Event{
		{RunID: "run-1", Seq: 1, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(t, struct {
			VerificationID       string        `json:"verification_id"`
			LastMutationSeq      uint64        `json:"last_mutation_seq"`
			CommandsFingerprint  string        `json:"commands_fingerprint"`
			WorkspaceObservation string        `json:"workspace_observation"`
			PlanRef              *contract.Ref `json:"plan_ref"`
		}{VerificationID: "verify-1", LastMutationSeq: 0, CommandsFingerprint: "sha256:fp", WorkspaceObservation: "sha256:obs", PlanRef: planRef})},
		{RunID: "run-1", Seq: 2, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(t, domain.VerificationFinishedPayload{
			VerificationID:       "verify-1",
			OK:                   true,
			EvidenceRef:          mustJSON(t, ref),
			VerifiedMutationSeq:  &zero,
			WorkspaceObservation: "sha256:obs",
		})},
	}
	got, err := acceptanceEvidenceFromRun(events, 2, stateDir, workspaceHash, accepted)
	if err != nil {
		t.Fatalf("acceptance evidence from run: %v", err)
	}
	if got == nil || got.Conclusion != contract.ConclusionPassed {
		t.Fatalf("zero-mutation evidence was not accepted: %+v", got)
	}
	// 缺失字段（P1 旧行为）必须被拒绝，防止读取侧放宽导致回归。
	events[1].Payload = mustJSON(t, domain.VerificationFinishedPayload{
		VerificationID:       "verify-1",
		OK:                   true,
		EvidenceRef:          mustJSON(t, ref),
		WorkspaceObservation: "sha256:obs",
	})
	if _, err := acceptanceEvidenceFromRun(events, 2, stateDir, workspaceHash, accepted); err == nil {
		t.Fatal("missing verified_mutation_seq was accepted")
	}
}

func TestAcceptanceEvidenceFromRunRejectsBindingDrift(t *testing.T) {
	accepted, err := contract.NewAcceptanceContract([]contract.AcceptanceCriterion{
		{ID: "compile", Statement: "项目可编译", Required: true, Checks: []string{"build"}},
	})
	if err != nil {
		t.Fatalf("acceptance: %v", err)
	}
	stateDir, workspaceHash := t.TempDir(), "ws"
	store, err := artifact.New(manifestArtifactRoot(stateDir, workspaceHash), 1024)
	if err != nil {
		t.Fatalf("artifact store: %v", err)
	}
	planRef := &contract.Ref{SHA256: "sha256:" + repeatHex('a', 64), Bytes: 10, MediaType: "application/json"}
	criteria, coverage, conclusion := contract.EvaluateAcceptance(accepted, []contract.AcceptanceCheckResult{
		{CheckID: "build", Executed: true, Passed: true},
	})
	evidence := contract.AcceptanceEvidence{
		Version:          contract.AcceptanceEvidenceVersion,
		RunID:            "run-1",
		AcceptanceDigest: accepted.Digest,
		Subject:          contract.EvidenceSubject{Kind: contract.EvidenceKindWorkspace, ID: workspaceHash, Version: "sha256:obs"},
		Producer:         contract.EvidenceProducer{Kind: contract.EvidenceProducerEngine, ID: contract.EvidenceProducerID},
		Source:           contract.EvidenceSource{VerificationID: "verify-1", PlanRef: planRef, CommandsFingerprint: "sha256:fp"},
		Criteria:         criteria,
		Coverage:         coverage,
		Conclusion:       conclusion,
	}
	content, err := json.Marshal(evidence)
	if err != nil {
		t.Fatalf("marshal evidence: %v", err)
	}
	ref, err := store.Put(content, "application/json")
	if err != nil {
		t.Fatalf("store evidence: %v", err)
	}
	turnID, zero := domain.NewTurnID(), uint64(0)
	buildEvents := func(startedObservation, finishedObservation, fingerprint string, plan *contract.Ref) []domain.Event {
		return []domain.Event{
			{RunID: "run-1", Seq: 1, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(t, struct {
				VerificationID       string        `json:"verification_id"`
				LastMutationSeq      uint64        `json:"last_mutation_seq"`
				CommandsFingerprint  string        `json:"commands_fingerprint"`
				WorkspaceObservation string        `json:"workspace_observation"`
				PlanRef              *contract.Ref `json:"plan_ref"`
			}{VerificationID: "verify-1", LastMutationSeq: 0, CommandsFingerprint: fingerprint, WorkspaceObservation: startedObservation, PlanRef: plan})},
			{RunID: "run-1", Seq: 2, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(t, domain.VerificationFinishedPayload{
				VerificationID: "verify-1", OK: true, EvidenceRef: mustJSON(t, ref), VerifiedMutationSeq: &zero, WorkspaceObservation: finishedObservation,
			})},
		}
	}
	cases := []struct {
		name       string
		events     []domain.Event
		wantReject bool
	}{
		{"valid", buildEvents("sha256:obs", "sha256:obs", "sha256:fp", planRef), false},
		{"observation drift", buildEvents("sha256:obs", "sha256:drift", "sha256:fp", planRef), true},
		{"fingerprint drift", buildEvents("sha256:obs", "sha256:obs", "sha256:other", planRef), true},
		{"plan ref drift", buildEvents("sha256:obs", "sha256:obs", "sha256:fp", &contract.Ref{SHA256: "sha256:" + repeatHex('b', 64)}), true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := acceptanceEvidenceFromRun(testCase.events, 2, stateDir, workspaceHash, accepted)
			if (err != nil) != testCase.wantReject {
				t.Fatalf("err = %v, wantReject %t", err, testCase.wantReject)
			}
		})
	}
}

func repeatHex(character byte, count int) string {
	result := make([]byte, count)
	for index := range result {
		result[index] = character
	}
	return string(result)
}
