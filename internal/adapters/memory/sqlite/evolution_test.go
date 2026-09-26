package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
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
	candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
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

// TestEvolutionInvalidationClosedForMissingSourceRow 锁定 R1 语义：来源行被
// 删除属数据损坏，受限作废必须失败关闭而不是把损坏洗白为终态。
func TestEvolutionInvalidationClosedForMissingSourceRow(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_evidence WHERE derived_id = ?", candidate.Source.DerivedID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_audit_log WHERE derived_id = ?", candidate.Source.DerivedID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_candidates WHERE id = ? AND consolidation_id = ?", candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now.Add(time.Minute)); err == nil {
		t.Fatal("missing source row must fail closed instead of laundering corruption")
	}
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft {
		t.Fatalf("candidate must stay draft after refused invalidation: %#v err=%v", inspected, err)
	}
}

// TestEvolutionCheckRejectsCooldownTampering 锁定冷却截止一致性负例：
// 冷却截止早于拒绝审计时间的库必须失败关闭。
func TestEvolutionCheckRejectsCooldownTampering(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now.Add(time.Minute), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE evolution_cooldowns SET cooldown_until = ? WHERE source_derived_id = ? AND source_consolidation_id = ?",
		now.Add(-24*time.Hour).UTC().Format(time.RFC3339Nano), candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("cooldown deadline before rejection audit must fail Check")
	}
}

// TestEvolutionCreateConflictsOnDifferentTTL 锁定 TTL 差值参与幂等等价：
// 同标识但不同保留期的重建必须冲突，而不是静默吞掉保留期事实。
func TestEvolutionCreateConflictsOnDifferentTTL(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	variant := candidate
	variant.ExpiresAt = variant.CreatedAt.Add(memory.DefaultCandidateTTL + 24*time.Hour)
	if _, err := store.CreateEvolutionCandidate(context.Background(), variant, now); err == nil {
		t.Fatal("same identity with a different retention period must conflict")
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
	freezeStoreClock(t, reopened, now)
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
	for _, tampered := range []string{
		memory.CandidateStateReady,
		memory.CandidateStateEvaluating,
		memory.CandidateStateEvaluatedPassed,
		memory.CandidateStateEvaluatedFailed,
		memory.CandidateStateInconclusive,
		memory.CandidateStatePromoted,
	} {
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
	candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
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
// 零残留：触发器追加隐藏终态审计，使精确审计读回捕获不一致并整体回滚，
// 候选、审计与冷却行全部零残留。
func TestEvolutionRejectReadBackFailureLeavesNoResidue(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER append_reject_audit AFTER INSERT ON evolution_audit_log WHEN NEW.action = 'rejected' BEGIN INSERT INTO evolution_audit_log(candidate_id, action, reason, occurred_at) VALUES (NEW.candidate_id, 'invalidated', NULL, NEW.occurred_at); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now.Add(time.Minute), 0); err == nil {
		t.Fatal("extra audit must fail the exact read-back and roll back rejection")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER append_reject_audit"); err != nil {
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

// TestEvolutionCreateRejectsMissingAuditReadBack 锁定创建路径的审计读回：
// 创建审计被触发器删除时，候选写入必须整体回滚而不能留下孤立状态行。
func TestEvolutionCreateRejectsMissingAuditReadBack(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER erase_created_audit AFTER INSERT ON evolution_audit_log WHEN NEW.action = 'created' BEGIN DELETE FROM evolution_audit_log WHERE candidate_id = NEW.candidate_id AND action = 'created'; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err == nil {
		t.Fatal("missing creation audit must fail the read-back")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER erase_created_audit"); err != nil {
		t.Fatal(err)
	}
	var candidateRows, auditRows int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_candidates").Scan(&candidateRows); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_audit_log").Scan(&auditRows); err != nil {
		t.Fatal(err)
	}
	if candidateRows != 0 || auditRows != 0 {
		t.Fatalf("creation read-back failure must leave no residue: candidates=%d audits=%d", candidateRows, auditRows)
	}
}

// TestEvolutionTerminalTransitionsRejectExtraAuditReadBack 锁定作废和到期路径
// 的精确审计读回；任意额外终态审计都必须使整笔状态迁移回滚。
func TestEvolutionTerminalTransitionsRejectExtraAuditReadBack(t *testing.T) {
	tests := []struct {
		name   string
		action string
		run    func(*Store, memory.EvolutionCandidate, time.Time) error
	}{
		{
			name:   "invalidated",
			action: memory.EvolutionAuditInvalidated,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusRejected, candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
					return err
				}
				_, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now)
				return err
			},
		},
		{
			name:   "expired",
			action: memory.EvolutionAuditExpired,
			run: func(store *Store, candidate memory.EvolutionCandidate, _ time.Time) error {
				_, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
				return err
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			store, candidate, now := evolutionFixture(t)
			if testCase.name == "expired" {
				candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
			}
			if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
				t.Fatal(err)
			}
			triggerName := "append_" + testCase.name + "_audit"
			statement := "CREATE TEMP TRIGGER " + triggerName + " AFTER INSERT ON evolution_audit_log WHEN NEW.action = '" + testCase.action + "' BEGIN INSERT INTO evolution_audit_log(candidate_id, action, reason, occurred_at) VALUES (NEW.candidate_id, '" + testCase.action + "', NULL, NEW.occurred_at); END"
			if _, err := store.db.ExecContext(context.Background(), statement); err != nil {
				t.Fatal(err)
			}
			if err := testCase.run(store, candidate, now); err == nil {
				t.Fatal("extra terminal audit must fail the exact read-back")
			}
			if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER "+triggerName); err != nil {
				t.Fatal(err)
			}
			inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
			if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft || len(inspected.Audit) != 1 {
				t.Fatalf("terminal transition must leave no residue: %#v err=%v", inspected, err)
			}
		})
	}
}

// TestEvolutionInvalidationRejectsCrossWorkspaceSource 锁定受限作废的工作区
// 资格：即使来源处于非 active，跨工作区来源也不得被作废路径洗白。
func TestEvolutionInvalidationRejectsCrossWorkspaceSource(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET workspace_hash = ?, status = ? WHERE id = ? AND consolidation_id = ?", "sha256:"+evolutionHex(8), memory.DerivedStatusRejected, candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now); err == nil {
		t.Fatal("cross-workspace source must block invalidation")
	}
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateDraft || len(inspected.Audit) != 1 {
		t.Fatalf("cross-workspace refusal must leave candidate unchanged: %#v err=%v", inspected, err)
	}
}

// TestEvolutionCheckRejectsMixedTerminalAudits 锁定终态审计的互斥性：候选
// 只能拥有其自身状态对应的一条终态审计，不能混入其他终态动作。
func TestEvolutionCheckRejectsMixedTerminalAudits(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*Store, memory.EvolutionCandidate, time.Time) error
		extra   memory.EvolutionAuditEntry
	}{
		{
			name: "rejected",
			prepare: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0)
				return err
			},
			extra: memory.EvolutionAuditEntry{Action: memory.EvolutionAuditInvalidated},
		},
		{
			name: "invalidated",
			prepare: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusRejected, candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
					return err
				}
				_, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now)
				return err
			},
			extra: memory.EvolutionAuditEntry{Action: memory.EvolutionAuditExpired},
		},
		{
			name: "expired",
			prepare: func(store *Store, candidate memory.EvolutionCandidate, _ time.Time) error {
				_, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
				return err
			},
			extra: memory.EvolutionAuditEntry{Action: memory.EvolutionAuditRejected, Reason: memory.RejectReasonHumanRequest},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			store, candidate, now := evolutionFixture(t)
			if testCase.name == "expired" {
				candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
			}
			if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
				t.Fatal(err)
			}
			if err := testCase.prepare(store, candidate, now); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.ExecContext(context.Background(), "INSERT INTO evolution_audit_log(candidate_id, action, reason, occurred_at) VALUES (?, ?, ?, ?)", candidate.ID, testCase.extra.Action, nullableString(testCase.extra.Reason), candidate.ExpiresAt.UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			if err := checkEvolutionInvariantsAt(context.Background(), store.db, candidate.ExpiresAt); err == nil {
				t.Fatal("mixed terminal audit must fail evolution invariant check")
			}
			if err := store.Check(context.Background()); err == nil {
				t.Fatal("raw mixed terminal audit must fail Store.Check")
			}
		})
	}
}

// TestEvolutionCheckRejectsEarlyExpiryAudit 锁定到期时序：过期审计不得早于
// 候选自身的保留期截止，避免篡改数据绕过显式到期资格。
func TestEvolutionCheckRejectsEarlyExpiryAudit(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE evolution_audit_log SET occurred_at = ? WHERE candidate_id = ? AND action = ?", candidate.ExpiresAt.Add(-time.Nanosecond).UTC().Format(time.RFC3339Nano), candidate.ID, memory.EvolutionAuditExpired); err != nil {
		t.Fatal(err)
	}
	if err := checkEvolutionInvariantsAt(context.Background(), store.db, candidate.ExpiresAt); err == nil {
		t.Fatal("expiry audit before deadline must fail evolution invariant check")
	}
}

// TestEvolutionCheckRejectsInvalidatedCandidateWithActiveSource 锁定作废终态
// 的持续来源资格：被篡改为活跃且未过期的来源不能继续支撑 invalidated 状态。
func TestEvolutionCheckRejectsInvalidatedCandidateWithActiveSource(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusRejected, candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ?, expires_at = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusActive, candidate.ExpiresAt.Add(time.Hour).UTC().Format(time.RFC3339Nano), candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if err := checkEvolutionInvariantsAt(context.Background(), store.db, candidate.ExpiresAt); err == nil {
		t.Fatal("invalidated candidate with an active source must fail evolution invariant check")
	}
}

// TestEvolutionCheckRejectsTerminalCandidateWithMissingSource 锁定终态候选
// 的来源完整性：来源缺失始终是损坏，不能因候选已 rejected 而被忽略。
func TestEvolutionCheckRejectsTerminalCandidateWithMissingSource(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_evidence WHERE derived_id = ? AND consolidation_id = ?", candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_audit_log WHERE derived_id = ? AND consolidation_id = ?", candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM derived_candidates WHERE id = ? AND consolidation_id = ?", candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if err := checkEvolutionInvariantsAt(context.Background(), store.db, now); err == nil {
		t.Fatal("terminal candidate with a missing source must fail evolution invariant check")
	}
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

// TestEvolutionDirectWritesRequireQualifiedSource 锁定三条 draft 写路径的事务
// 来源资格：即使绕过应用层直连 Store，缺失、跨工作区、inactive 或过期来源
// 都不得产生候选状态、审计或冷却残留。
func TestEvolutionDirectWritesRequireQualifiedSource(t *testing.T) {
	actions := []struct {
		name string
		run  func(*Store, memory.EvolutionCandidate, time.Time) error
	}{
		{
			name: "create",
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.CreateEvolutionCandidate(context.Background(), candidate, now)
				return err
			},
		},
		{
			name: "reject",
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0)
				return err
			},
		},
		{
			name: "expire",
			run: func(store *Store, candidate memory.EvolutionCandidate, _ time.Time) error {
				_, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
				return err
			},
		},
	}
	mutations := []evolutionSourceMutation{evolutionSourceMissing, evolutionSourceCross, evolutionSourceInactive, evolutionSourceExpired}
	for _, action := range actions {
		for _, mutation := range mutations {
			t.Run(action.name+"_"+string(mutation), func(t *testing.T) {
				store, candidate, now := evolutionFixture(t)
				if action.name == "expire" {
					candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
				}
				if action.name != "create" {
					if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
						t.Fatal(err)
					}
				}
				mutateEvolutionSource(t, store, candidate, now, mutation)
				if err := action.run(store, candidate, now); err == nil {
					t.Fatalf("%s source must block direct %s", mutation, action.name)
				}
				if action.name == "create" {
					var candidateRows, auditRows int
					if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_candidates").Scan(&candidateRows); err != nil {
						t.Fatal(err)
					}
					if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_audit_log").Scan(&auditRows); err != nil {
						t.Fatal(err)
					}
					if candidateRows != 0 || auditRows != 0 {
						t.Fatalf("refused create must leave no residue: candidates=%d audits=%d", candidateRows, auditRows)
					}
					return
				}
				assertEvolutionDraftOnly(t, store, candidate.ID)
				if evolutionCooldownRowCount(t, store) != 0 {
					t.Fatal("refused terminal write must not persist cooldown")
				}
			})
		}
	}
}

// TestEvolutionInvalidationRequiresInactiveQualifiedSource 锁定受限作废的反向
// 资格：只有同工作区且 inactive 或已过期的 exact source 才能把 draft 关闭。
func TestEvolutionInvalidationRequiresInactiveQualifiedSource(t *testing.T) {
	tests := []struct {
		name       string
		mutation   evolutionSourceMutation
		shouldPass bool
	}{
		{name: "active", shouldPass: false},
		{name: string(evolutionSourceMissing), mutation: evolutionSourceMissing, shouldPass: false},
		{name: string(evolutionSourceCross), mutation: evolutionSourceCross, shouldPass: false},
		{name: string(evolutionSourceInactive), mutation: evolutionSourceInactive, shouldPass: true},
		{name: string(evolutionSourceExpired), mutation: evolutionSourceExpired, shouldPass: true},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			store, candidate, now := evolutionFixture(t)
			if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
				t.Fatal(err)
			}
			if testCase.mutation != "" {
				mutateEvolutionSource(t, store, candidate, now, testCase.mutation)
			}
			invalidated, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now)
			if testCase.shouldPass {
				if err != nil || invalidated.Status != memory.CandidateStateInvalidated {
					t.Fatalf("qualified inactive source invalidation = %#v err=%v", invalidated, err)
				}
				return
			}
			if err == nil {
				t.Fatal("unqualified source must block invalidation")
			}
			assertEvolutionDraftOnly(t, store, candidate.ID)
		})
	}
}

// TestEvolutionReadyCandidateAcceptsManualTerminalActions 锁定 M4-2 启用后的恢复
// 边界：尚未启动评测的 ready 候选可以被拒绝、到期或因来源失效作废。
func TestEvolutionReadyCandidateAcceptsManualTerminalActions(t *testing.T) {
	tests := []struct {
		name     string
		run      func(*Store, memory.EvolutionCandidate, time.Time) error
		expiry   bool
		inactive bool
	}{
		{
			name: "ready_reject",
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0)
				return err
			},
		},
		{
			name:   "ready_expire",
			expiry: true,
			run: func(store *Store, candidate memory.EvolutionCandidate, _ time.Time) error {
				_, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
				return err
			},
		},
		{
			name:     "ready_invalidate",
			inactive: true,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now)
				return err
			},
		},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			store, candidate, now := evolutionFixture(t)
			if testCase.expiry {
				candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
			}
			candidate.Status = memory.CandidateStateReady
			candidate.EvalSetFingerprint = evalSetFrozen
			if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
				t.Fatal(err)
			}
			if testCase.inactive {
				mutateEvolutionSource(t, store, candidate, now, evolutionSourceInactive)
			}
			if err := testCase.run(store, candidate, now); err != nil {
				t.Fatalf("ready terminal action failed: %v", err)
			}
			rawCandidate, found, err := lookupEvolutionCandidate(context.Background(), store.db, candidate.ID)
			if err != nil || !found || !memory.IsTerminalCandidateState(rawCandidate.Status) {
				t.Fatalf("ready action must close the candidate: candidate=%#v found=%t err=%v", rawCandidate, found, err)
			}
		})
	}
}

// TestEvolutionWriteReadBackRejectsCandidateMutation 为四条写路径注入状态与
// 等幅时间篡改。任何逐字段候选读回失配都必须在提交前整体回滚。
func TestEvolutionWriteReadBackRejectsCandidateMutation(t *testing.T) {
	tests := []struct {
		name   string
		action string
		run    func(*Store, memory.EvolutionCandidate, time.Time) error
	}{
		{
			name:   "create",
			action: memory.EvolutionAuditCreated,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.CreateEvolutionCandidate(context.Background(), candidate, now)
				return err
			},
		},
		{
			name:   "reject",
			action: memory.EvolutionAuditRejected,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0)
				return err
			},
		},
		{
			name:   "invalidated",
			action: memory.EvolutionAuditInvalidated,
			run: func(store *Store, candidate memory.EvolutionCandidate, now time.Time) error {
				_, err := store.InvalidateEvolutionCandidateForInactiveSource(context.Background(), candidate.ID, now)
				return err
			},
		},
		{
			name:   "expired",
			action: memory.EvolutionAuditExpired,
			run: func(store *Store, candidate memory.EvolutionCandidate, _ time.Time) error {
				_, err := store.ExpireEvolutionCandidate(context.Background(), candidate.ID, candidate.ExpiresAt)
				return err
			},
		},
	}
	for _, testCase := range tests {
		for _, mutation := range []string{"status", "time"} {
			t.Run(testCase.name+"_"+mutation, func(t *testing.T) {
				store, candidate, now := evolutionFixture(t)
				if testCase.name == "expired" {
					candidate = evolutionCandidateBeforeSourceExpiry(candidate, now)
				}
				if testCase.name != "create" {
					if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
						t.Fatal(err)
					}
				}
				if testCase.name == "invalidated" {
					mutateEvolutionSource(t, store, candidate, now, evolutionSourceInactive)
				}
				triggerName := "mutate_" + testCase.name + "_" + mutation
				statement := evolutionCandidateMutationTrigger(triggerName, testCase.name, testCase.action, mutation, candidate)
				if _, err := store.db.ExecContext(context.Background(), statement); err != nil {
					t.Fatal(err)
				}
				if err := testCase.run(store, candidate, now); err == nil {
					t.Fatal("candidate mutation must fail exact read-back")
				}
				if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER "+triggerName); err != nil {
					t.Fatal(err)
				}
				if testCase.name == "create" {
					var candidateRows, auditRows int
					if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_candidates").Scan(&candidateRows); err != nil {
						t.Fatal(err)
					}
					if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM evolution_audit_log").Scan(&auditRows); err != nil {
						t.Fatal(err)
					}
					if candidateRows != 0 || auditRows != 0 {
						t.Fatalf("create mutation must leave no residue: candidates=%d audits=%d", candidateRows, auditRows)
					}
					return
				}
				assertEvolutionDraftOnly(t, store, candidate.ID)
				if evolutionCooldownRowCount(t, store) != 0 {
					t.Fatal("failed transition must not persist cooldown")
				}
			})
		}
	}
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

// TestEvolutionRejectReadBackRejectsCooldownMutation 锁定拒绝写集的最后一项：
// 冷却行被触发器改写后，候选、审计与冷却必须作为单一事务全部回滚。
func TestEvolutionRejectReadBackRejectsCooldownMutation(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER mutate_reject_cooldown AFTER INSERT ON evolution_cooldowns BEGIN UPDATE evolution_cooldowns SET cooldown_until = NEW.cooldown_until || '.tampered' WHERE source_derived_id = NEW.source_derived_id AND source_consolidation_id = NEW.source_consolidation_id AND target_kind = NEW.target_kind AND variant_fingerprint = NEW.variant_fingerprint; END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RejectEvolutionCandidate(context.Background(), candidate.ID, memory.RejectReasonHumanRequest, now, 0); err == nil {
		t.Fatal("cooldown mutation must fail exact read-back")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER mutate_reject_cooldown"); err != nil {
		t.Fatal(err)
	}
	assertEvolutionDraftOnly(t, store, candidate.ID)
	if evolutionCooldownRowCount(t, store) != 0 {
		t.Fatal("cooldown read-back failure must leave no cooldown residue")
	}
}
