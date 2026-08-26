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

// TestEvolutionTransitionsClosedForFuturePhases 锁定本切片外迁移失败关闭：
// 无法从 draft 直接进入 ready/evaluating/promoted。
func TestEvolutionTransitionsClosedForFuturePhases(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.transitionEvolutionCandidate(context.Background(), candidate.ID, memory.CandidateStateReady, memory.EvolutionAuditCreated, now); err == nil {
		t.Fatal("draft -> ready must be closed in this slice")
	}
	if _, err := store.transitionEvolutionCandidate(context.Background(), candidate.ID, memory.CandidateStatePromoted, memory.EvolutionAuditCreated, now); err == nil {
		t.Fatal("draft -> promoted must be closed in this slice")
	}
	if _, err := store.transitionEvolutionCandidate(context.Background(), candidate.ID, memory.CandidateStateInvalidated, memory.EvolutionAuditInvalidated, now); err != nil {
		t.Fatalf("draft -> invalidated must be permitted: %v", err)
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
	if _, err := store.InvalidateEvolutionCandidate(context.Background(), candidate.ID, now); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("explicit invalidation must restore consistency: %v", err)
	}
}
