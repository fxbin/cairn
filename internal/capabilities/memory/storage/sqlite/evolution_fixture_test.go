// [INPUT]: 依赖真实SQLite与父能力结构化合同。
// [OUTPUT]: 提供演化来源、冻结时钟与定向篡改夹具。
// [POS]: 能力存储回归，保留原全部安全与恢复断言。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package sqlite

import (
	"context"

	"testing"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

const testEvolutionCandidateExpiryOffset = time.Hour

// evolutionFixture 在临时库中准备一条 active 的 M3 派生版本，作为演化候选来源。
func evolutionFixture(t *testing.T) (*Store, memory.EvolutionCandidate, time.Time) {
	t.Helper()
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.ConsolidateBatch(context.Background(), []memory.PlannedConsolidation{plan}); err != nil {
			t.Fatal(err)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil || len(derived) == 0 {
		t.Fatalf("derived = %#v err = %v", derived, err)
	}
	source := memory.EvolutionSourceRef{DerivedID: derived[0].ID, ConsolidationID: derived[0].ConsolidationID}
	variant := memory.CompactionParamsVariant{
		SchemaVersion:           memory.EvolutionSchemaVersion,
		MaxPromptTokens:         50_000,
		MaxSummaryBytes:         16_384,
		InventoryBudgetPercent:  25,
		ThresholdReservePercent: 20,
	}
	variantFingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, variant)
	if err != nil {
		t.Fatal(err)
	}
	variantDigest, err := memory.VariantSpecDigest(variant)
	if err != nil {
		t.Fatal(err)
	}
	candidate := memory.EvolutionCandidate{
		ID:                  memory.EvolutionCandidateID(derived[0].WorkspaceHash, source, memory.TargetKindContextCompactionParams, variantFingerprint),
		SchemaVersion:       memory.EvolutionSchemaVersion,
		WorkspaceHash:       derived[0].WorkspaceHash,
		Source:              source,
		TargetKind:          memory.TargetKindContextCompactionParams,
		VariantSpecSHA256:   variantDigest,
		VariantFingerprint:  variantFingerprint,
		BaselineFingerprint: "sha256:" + evolutionHex(6),
		CreatedAt:           now,
		ExpiresAt:           now.Add(memory.DefaultCandidateTTL),
		Status:              memory.CandidateStateDraft,
	}
	return store, candidate, now
}

func evolutionCandidateBeforeSourceExpiry(candidate memory.EvolutionCandidate, now time.Time) memory.EvolutionCandidate {
	candidate.ExpiresAt = now.Add(testEvolutionCandidateExpiryOffset)
	return candidate
}

// evolutionHex 生成测试用 sha256 形式十六进制摘要。
func evolutionHex(seed int) string {
	const hexChars = "0123456789abcdef"
	result := make([]byte, 0, 64)
	for index := 0; index < 64; index++ {
		result = append(result, hexChars[(index+seed)%len(hexChars)])
	}
	return string(result)
}

// evolutionFixtureOn 在指定路径上构建演化候选夹具。
func evolutionFixtureOn(t *testing.T, path string) (*Store, memory.EvolutionCandidate, time.Time) {
	t.Helper()
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHash := "sha256:" + evolutionHex(1)
	taskDigest := "sha256:" + evolutionHex(2)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	freezeStoreClock(t, store, now)
	firstEvidence := []memory.EventEvidenceRef{{RunID: "run_a", EventSeq: 3, EventHash: "sha256:" + evolutionHex(3), EventType: "run_failed"}}
	secondEvidence := []memory.EventEvidenceRef{{RunID: "run_b", EventSeq: 5, EventHash: "sha256:" + evolutionHex(4), EventType: "run_failed"}}
	first, err := memory.NewRunOutcomeCandidate(workspaceHash, "run_a", taskDigest, "failed", "budget_turns", false, now.Add(-time.Hour), memory.DefaultTTL, firstEvidence, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := memory.NewRunOutcomeCandidate(workspaceHash, "run_b", taskDigest, "failed", "budget_turns", false, now, memory.DefaultTTL, secondEvidence, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []memory.Candidate{first, second} {
		if _, err := store.Capture(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := memory.DeriveConsolidations(memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []memory.Candidate{first, second}}, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.ConsolidateBatch(context.Background(), []memory.PlannedConsolidation{plan}); err != nil {
			t.Fatal(err)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil || len(derived) == 0 {
		t.Fatalf("derived = %#v err = %v", derived, err)
	}
	source := memory.EvolutionSourceRef{DerivedID: derived[0].ID, ConsolidationID: derived[0].ConsolidationID}
	variant := memory.CompactionParamsVariant{SchemaVersion: memory.EvolutionSchemaVersion, MaxPromptTokens: 50_000, MaxSummaryBytes: 16_384, InventoryBudgetPercent: 25, ThresholdReservePercent: 20}
	variantFingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, variant)
	if err != nil {
		t.Fatal(err)
	}
	variantDigest, err := memory.VariantSpecDigest(variant)
	if err != nil {
		t.Fatal(err)
	}
	candidate := memory.EvolutionCandidate{
		ID:                  memory.EvolutionCandidateID(workspaceHash, source, memory.TargetKindContextCompactionParams, variantFingerprint),
		SchemaVersion:       memory.EvolutionSchemaVersion,
		WorkspaceHash:       workspaceHash,
		Source:              source,
		TargetKind:          memory.TargetKindContextCompactionParams,
		VariantSpecSHA256:   variantDigest,
		VariantFingerprint:  variantFingerprint,
		BaselineFingerprint: "sha256:" + evolutionHex(6),
		CreatedAt:           now,
		ExpiresAt:           now.Add(memory.DefaultCandidateTTL),
		Status:              memory.CandidateStateDraft,
	}
	return store, candidate, now
}

type evolutionSourceMutation string

const (
	evolutionSourceMissing  evolutionSourceMutation = "missing"
	evolutionSourceCross    evolutionSourceMutation = "cross_workspace"
	evolutionSourceInactive evolutionSourceMutation = "inactive"
	evolutionSourceExpired  evolutionSourceMutation = "expired"
)

func mutateEvolutionSource(t *testing.T, store *Store, candidate memory.EvolutionCandidate, now time.Time, mutation evolutionSourceMutation) {
	t.Helper()
	switch mutation {
	case evolutionSourceMissing:
		if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_evidence WHERE derived_id = ? AND consolidation_id = ?", candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_audit_log WHERE derived_id = ? AND consolidation_id = ?", candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_candidates WHERE id = ? AND consolidation_id = ?", candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
			t.Fatal(err)
		}
	case evolutionSourceCross:
		if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET workspace_hash = ? WHERE id = ? AND consolidation_id = ?", "sha256:"+evolutionHex(8), candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
			t.Fatal(err)
		}
	case evolutionSourceInactive:
		if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusRejected, candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
			t.Fatal(err)
		}
	case evolutionSourceExpired:
		if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET expires_at = ? WHERE id = ? AND consolidation_id = ?", now.UTC().Format(time.RFC3339Nano), candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown source mutation %q", mutation)
	}
}

func assertEvolutionDraftOnly(t *testing.T, store *Store, candidateID string) {
	t.Helper()
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidateID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft || len(inspected.Audit) != 1 || inspected.Audit[0].Action != memory.EvolutionAuditCreated {
		t.Fatalf("candidate must remain draft with only creation audit: %#v err=%v", inspected, err)
	}
}

func evolutionCooldownRowCount(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_cooldowns").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func evolutionCandidateMutationTrigger(name, actionName, action, mutation string, candidate memory.EvolutionCandidate) string {
	if actionName == "create" {
		if mutation == "status" {
			return "CREATE TEMP TRIGGER " + name + " AFTER INSERT ON evolution_candidates BEGIN UPDATE evolution_candidates SET status = 'ready' WHERE id = NEW.id; END"
		}
		return "CREATE TEMP TRIGGER " + name + " AFTER INSERT ON evolution_candidates BEGIN UPDATE evolution_candidates SET created_at = '" + candidate.CreatedAt.Add(time.Minute).UTC().Format(time.RFC3339Nano) + "', expires_at = '" + candidate.ExpiresAt.Add(time.Minute).UTC().Format(time.RFC3339Nano) + "' WHERE id = NEW.id; END"
	}
	if mutation == "status" {
		return "CREATE TEMP TRIGGER " + name + " AFTER UPDATE OF status ON evolution_candidates WHEN NEW.status = '" + action + "' BEGIN UPDATE evolution_candidates SET status = 'ready' WHERE id = NEW.id; END"
	}
	return "CREATE TEMP TRIGGER " + name + " AFTER UPDATE OF status ON evolution_candidates WHEN NEW.status = '" + action + "' BEGIN UPDATE evolution_candidates SET created_at = '" + candidate.CreatedAt.Add(time.Minute).UTC().Format(time.RFC3339Nano) + "', expires_at = '" + candidate.ExpiresAt.Add(time.Minute).UTC().Format(time.RFC3339Nano) + "' WHERE id = NEW.id; END"
}
