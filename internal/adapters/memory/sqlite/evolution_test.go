package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

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

// TestEvolutionOrphanCooldownBlocksCreation 直接钉住冷却门的纵深防御语义：
// 即使候选唯一键与终态不复活规则被未来放宽，孤立冷却行仍必须阻断同键重建。
func TestEvolutionOrphanCooldownBlocksCreation(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	fingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, memory.CompactionParamsVariant{
		SchemaVersion: memory.EvolutionSchemaVersion, MaxPromptTokens: 50_000, MaxSummaryBytes: 16_384,
		InventoryBudgetPercent: 25, ThresholdReservePercent: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO evolution_cooldowns(
		source_derived_id, source_consolidation_id, target_kind, variant_fingerprint, cooldown_until
	) VALUES (?, ?, ?, ?, ?)`,
		candidate.Source.DerivedID, candidate.Source.ConsolidationID, candidate.TargetKind, fingerprint,
		now.Add(memory.DefaultCooldown).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err == nil {
		t.Fatal("orphan cooldown row must block creation via the cooldown gate")
	}
}

// TestEvolutionExpireTransitionPermitsAndCloses 锁定过期迁移：draft 可显式过期、
// 幂等重入、过期后审计与 Check 全部一致。
func TestEvolutionExpireTransitionPermitsAndCloses(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	expired, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
	if err != nil || expired.Status != memory.CandidateStateExpired {
		t.Fatalf("expire = %#v err = %v", expired, err)
	}
	repeat, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt.Add(time.Minute))
	if err != nil || repeat.Status != memory.CandidateStateExpired {
		t.Fatalf("idempotent expire = %#v err = %v", repeat, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
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

// TestEvolutionCandidateCreateIsIdempotentAndDurable 锁定创建幂等、审计耐久
// 与迁移升级：重试返回同一候选，重开库后 Check 全部通过。
func TestEvolutionCandidateCreateIsIdempotentAndDurable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, candidate, now := evolutionFixtureOn(t, path)
	created, err := store.CreateEvolutionCandidate(context.Background(), candidate, now)
	if err != nil || created.ID != candidate.ID {
		t.Fatalf("create = %#v err = %v", created, err)
	}
	again, err := store.CreateEvolutionCandidate(context.Background(), candidate, now.Add(time.Minute))
	if err != nil || again.ID != created.ID {
		t.Fatalf("idempotent create = %#v err = %v", again, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Check(context.Background()); err != nil {
		t.Fatalf("durable check: %v", err)
	}
	inspected, err := reopened.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || len(inspected.Audit) != 1 || inspected.Audit[0].Action != memory.EvolutionAuditCreated {
		t.Fatalf("inspect = %#v err = %v", inspected, err)
	}
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

// TestEvolutionRejectBlocksRecreationAndIsIdempotent 锁定拒绝写冷却、冷却期
// 内重建失败关闭与重复拒绝幂等。
func TestEvolutionRejectBlocksRecreationAndIsIdempotent(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	rejected, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0)
	if err != nil || rejected.Status != memory.CandidateStateRejected {
		t.Fatalf("reject = %#v err = %v", rejected, err)
	}
	repeat, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonInsufficientEvidence, now.Add(time.Minute), 0)
	if err != nil || repeat.ID != rejected.ID {
		t.Fatalf("idempotent reject = %#v err = %v", repeat, err)
	}
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now.Add(time.Minute)); err == nil {
		t.Fatal("cooldown must block recreation")
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEvolutionTransitionsClosedForFuturePhases 锁定本切片外状态失败关闭：
// 直接把状态篡改为 ready 或 promoted 后 Check 必须拒绝，而非接受未知状态。
func TestEvolutionTransitionsClosedForFuturePhases(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	for _, tampered := range []string{memory.CandidateStateReady, memory.CandidateStateEvaluating, memory.CandidateStateEvaluatedPassed, memory.CandidateStatePromoted} {
		if _, err := store.db.ExecContext(context.Background(), "UPDATE evolution_candidates SET status = ? WHERE id = ?", tampered, candidate.ID); err != nil {
			t.Fatal(err)
		}
		if err := store.Check(context.Background()); err == nil {
			t.Fatalf("tampered status %s must fail Check", tampered)
		}
		if _, err := store.db.ExecContext(context.Background(), "UPDATE evolution_candidates SET status = ? WHERE id = ?", memory.CandidateStateDraft, candidate.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEvolutionInvalidationRequiresInactiveSource 锁定作废资格条件：
// 来源仍活跃时受限作废失败关闭且状态不变。
func TestEvolutionInvalidationRequiresInactiveSource(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now.Add(time.Minute)); err == nil {
		t.Fatal("active source must block invalidation")
	}
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft {
		t.Fatalf("candidate must stay draft after refused invalidation: %#v err=%v", inspected, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEvolutionExpireRequiresDeadlineReached 锁定过期资格条件：
// 未到保留期截止的过期迁移失败关闭且状态不变。
func TestEvolutionExpireRequiresDeadlineReached(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt.Add(-time.Second)); err == nil {
		t.Fatal("premature expiry must fail closed")
	}
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft {
		t.Fatalf("candidate must stay draft after refused expiry: %#v err=%v", inspected, err)
	}
	if _, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt); err != nil {
		t.Fatalf("expiry at deadline must succeed: %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEvolutionRejectReadBackFailureLeavesNoResidue 锁定拒绝路径的读回
// 零残留：审计读回比对被触发器注入失败后，状态与冷却全部回滚。
func TestEvolutionRejectReadBackFailureLeavesNoResidue(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER duplicate_reject_audit BEFORE INSERT ON evolution_audit_log WHEN NEW.action = 'rejected' BEGIN INSERT INTO evolution_audit_log(candidate_id, action, reason, occurred_at) VALUES (NEW.candidate_id, NEW.action, NEW.reason, NEW.occurred_at); SELECT RAISE(ABORT, 'injected duplicate'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now.Add(time.Minute), 0); err == nil {
		t.Fatal("read-back mismatch must roll back rejection")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER duplicate_reject_audit"); err != nil {
		t.Fatal(err)
	}
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft || len(inspected.Audit) != 1 {
		t.Fatalf("rejection must leave no residue: %#v err=%v", inspected, err)
	}
	var cooldownRows int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_cooldowns").Scan(&cooldownRows); err != nil {
		t.Fatal(err)
	}
	if cooldownRows != 0 {
		t.Fatalf("rejected rollback must not persist cooldown rows, got %d", cooldownRows)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEvolutionCreateRollsBackOnMidFailure 锁定创建事务原子性：审计插入被
// 触发器注入失败后候选与冷却零残留。
func TestEvolutionCreateRollsBackOnMidFailure(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER fail_evolution_audit BEFORE INSERT ON evolution_audit_log BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err == nil {
		t.Fatal("mid-transaction failure must roll back creation")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER fail_evolution_audit"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatalf("retry after rollback must succeed: %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestEvolutionCheckDetectsInactiveSource 锁定来源失效检测：非终态候选的来源
// 版本被替代后，Check 失败关闭直至候选显式作废。
func TestEvolutionCheckDetectsInactiveSource(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 通过第二轮巩固（新增一条同任务候选）让旧派生版本被合法替代，
	// 而不是绕过审计直接改状态。
	all, err := store.ListAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	extraEvidence := []memory.EventEvidenceRef{{RunID: "run_c", EventSeq: 7, EventHash: "sha256:" + evolutionHex(5), EventType: "run_failed"}}
	extra, err := memory.NewRunOutcomeCandidate(candidate.WorkspaceHash, "run_c", all[0].TaskPromptDigest, "failed", "budget_turns", false, now, memory.DefaultTTL, extraEvidence, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Capture(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	expanded := append(append([]memory.Candidate(nil), all...), extra)
	plans, err := memory.DeriveConsolidations(memory.ConsolidationInputs{WorkspaceHash: candidate.WorkspaceHash, Candidates: expanded}, memory.ConsolidationOptions{Now: now})
	if err != nil {
		t.Fatal(err)
	}
	for _, plan := range plans {
		if _, err := store.ConsolidateBatch(context.Background(), []memory.PlannedConsolidation{plan}); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("inactive source with non-terminal candidate must fail Check")
	}
	if _, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("explicit invalidation must restore consistency: %v", err)
	}
}
