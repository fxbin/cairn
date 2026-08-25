package memory

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/contract"
)

const recallTestDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestBuildRecallPlanClosesFieldsAndFailsOnInsufficientScope(t *testing.T) {
	plan, err := BuildRecallPlan(RecallQuery{WorkspaceHash: recallTestDigest})
	if err != nil || plan.StopReason != RecallStopEvidenceInsufficient || plan.Strategy != RecallStrategySQLiteExact {
		t.Fatalf("plan=%#v err=%v", plan, err)
	}
	from := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	until := from.Add(time.Hour)
	normalized, err := (RecallQuery{
		WorkspaceHash:       recallTestDigest,
		Outcomes:            []string{"failed", "completed", "failed"},
		FailureClasses:      []string{"agent", "agent"},
		CreatedFrom:         &from,
		CreatedUntil:        &until,
		TrustedVerification: TrustedVerificationAny,
		Precision:           RecallPrecisionCompact,
	}).Normalize()
	if err != nil || normalized.Limit != DefaultCompactRecallLimit || len(normalized.Outcomes) != 2 || normalized.Outcomes[0] != "completed" {
		t.Fatalf("normalized=%#v err=%v", normalized, err)
	}
	if _, err := (RecallQuery{WorkspaceHash: recallTestDigest, FailureClasses: []string{"unbounded"}}).Normalize(); err == nil {
		t.Fatal("unbounded failure class unexpectedly accepted")
	}
	if _, err := (RecallQuery{WorkspaceHash: recallTestDigest, FailureClasses: []string{contract.FailureLoopGuardRepeatedOperationFailure}}).Normalize(); err != nil {
		t.Fatalf("known terminal failure class rejected: %v", err)
	}
	if _, err := (RecallQuery{WorkspaceHash: recallTestDigest, Outcomes: []string{"failed"}, Precision: RecallPrecisionCompact, Limit: MaxCompactRecallLimit + 1}).Normalize(); err == nil {
		t.Fatal("compact limit above its bound unexpectedly accepted")
	}
}

func TestEvidenceLedgerProjectsPrecisionConflictsAndGapsDeterministically(t *testing.T) {
	plan, err := BuildRecallPlan(RecallQuery{WorkspaceHash: recallTestDigest, Outcomes: []string{"completed"}, Precision: RecallPrecisionCompact})
	if err != nil {
		t.Fatal(err)
	}
	timestamp := time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC)
	evidence := []LedgerEvidence{
		{Source: LedgerSourceEventJournal, EventSeq: 3, EventHash: recallTestDigest, EventType: "run_completed", EventTime: timestamp},
		{Source: LedgerSourceArtifactReference, EventSeq: 3, EventHash: recallTestDigest, EventType: "run_completed", ArtifactSHA256: recallTestDigest, EventTime: timestamp},
	}
	entries := []LedgerEntry{
		{CandidateID: "mem_a", TaskPromptDigest: recallTestDigest, Outcome: "completed", Evidence: evidence, EventReferenceCount: CountEventReferences(evidence), VerificationTimeStatus: EvidenceSlotUnavailable},
		{CandidateID: "mem_b", TaskPromptDigest: recallTestDigest, Outcome: "failed", Evidence: evidence, EventReferenceCount: CountEventReferences(evidence), VerificationTimeStatus: EvidenceSlotUnavailable},
	}
	ledger := NewEvidenceLedger(plan, entries)
	compactJSON, err := json.Marshal(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if ledger.Status != LedgerStatusSufficient || len(ledger.Entries[0].Evidence) != 0 || ledger.Entries[0].EventReferenceCount != 1 || len(ledger.Conflicts) != 1 || len(ledger.Conflicts[0].Evidence) != 0 || ledger.Conflicts[0].Resolved || len(ledger.Gaps) == 0 || bytes.Contains(compactJSON, []byte("event_hash")) || bytes.Contains(compactJSON, []byte("artifact_sha256")) || bytes.Contains(compactJSON, []byte("event_time")) {
		t.Fatalf("compact ledger=%#v", ledger)
	}
	plan.NormalizedRequest.Precision = RecallPrecisionTrace
	trace := NewEvidenceLedger(plan, entries)
	if len(trace.Entries[0].Evidence) != len(evidence) || len(trace.Conflicts) != 1 || len(trace.Conflicts[0].Evidence) == 0 || trace.Entries[0].EventReferenceCount != 1 {
		t.Fatalf("trace ledger=%#v", trace)
	}
}
