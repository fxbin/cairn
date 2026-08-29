package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

// passedCandidateFixture 依既有评测事务 fixture 装配一个已通过评测的候选：
// ready 建档 → started → passed，返回候选与评测行。
func passedCandidateFixture(t *testing.T, evalSetFingerprint, suiteDigest string) (*Store, memory.EvolutionCandidate, memory.Evaluation, time.Time) {
	t.Helper()
	store, candidate, now := readyEvolutionFixtureWithSet(t, evalSetFingerprint)
	ctx := context.Background()
	started, err := newEvaluationForBound(candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	started, err = store.StartEvaluation(ctx, candidate.ID, started, evaluationBindingFor(candidate), now)
	if err != nil {
		t.Fatalf("start evaluation: %v", err)
	}
	concluded, err := store.FinishEvaluation(ctx, started.ID, memory.EvaluationStatusPassed, suiteDigest, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("conclude evaluation: %v", err)
	}
	return store, candidate, concluded, now
}

// readyEvolutionFixtureWithSet 以指定冻结评测指纹装配 ready 候选。
func readyEvolutionFixtureWithSet(t *testing.T, evalSetFingerprint string) (*Store, memory.EvolutionCandidate, time.Time) {
	t.Helper()
	store, candidate, now := evolutionFixture(t)
	candidate.Status = memory.CandidateStateReady
	candidate.EvalSetFingerprint = evalSetFingerprint
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatalf("create ready candidate: %v", err)
	}
	return store, candidate, now
}

// newEvaluationForBound 为候选派生合法 running 评测行（含时间锚）。
func newEvaluationForBound(candidate memory.EvolutionCandidate, now time.Time) (memory.Evaluation, error) {
	evaluation, err := newEvaluationFor(candidate)
	if err != nil {
		return memory.Evaluation{}, err
	}
	evaluation.StartedAt = now
	return evaluation, nil
}

// promotionRequestFor 依候选与评测事实组装结构合法的晋升请求。
func promotionRequestFor(candidate memory.EvolutionCandidate, evaluation memory.Evaluation, version int) memory.Promotion {
	variant := memory.CompactionParamsVariant{
		SchemaVersion: memory.EvolutionSchemaVersion, MaxPromptTokens: 50_000, MaxSummaryBytes: 16_384,
		InventoryBudgetPercent: 25, ThresholdReservePercent: 20,
	}
	return memory.Promotion{
		SchemaVersion: 1, CandidateID: candidate.ID, TargetKind: memory.TargetKindContextCompactionParams,
		Version: version, Variant: variant, VariantFingerprint: candidate.VariantFingerprint, VariantSpecSHA256: candidate.VariantSpecSHA256,
		BaselineFingerprint: candidate.BaselineFingerprint, EvalSetFingerprint: evaluation.EvalSetFingerprint,
		SuiteResultDigest: evaluation.SuiteResultDigest, ComparatorVersion: evaluation.ComparatorVersion,
		Conclusion: memory.EvaluationStatusPassed, ConclusionTrigger: "mode_b_efficiency",
		Approver: "fxbin", Status: memory.PromotionStatusActive, CreatedAt: time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
	}
}

// promotionIDFor 是确定性晋升身份的测试直通。
func promotionIDFor(t *testing.T, candidate memory.EvolutionCandidate, version int) string {
	t.Helper()
	id, err := memory.PromotionID(candidate.ID, candidate.VariantFingerprint, version)
	if err != nil {
		t.Fatalf("promotion id: %v", err)
	}
	return id
}

// approvalFor 是审批指纹的测试直通。
func approvalFor(t *testing.T, request memory.Promotion) string {
	t.Helper()
	fingerprint, err := memory.PromotionApprovalFingerprint(request.CandidateID, request.VariantFingerprint, request.EvalSetFingerprint, request.SuiteResultDigest, request.TargetKind, request.Version)
	if err != nil {
		t.Fatalf("approval: %v", err)
	}
	return fingerprint
}

// TestPromoteCandidateHappyPathAndGuards 覆盖晋升事务核心面：指纹不匹配、版
// 本跳跃与重复晋升各按冲突失败关闭且候选保持原态；合法路径落账、推进激活
// 指针并通过全库检查。
func TestPromoteCandidateHappyPathAndGuards(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	ctx := context.Background()
	request := promotionRequestFor(candidate, evaluation, 1)
	request.ID = promotionIDFor(t, candidate, 1)
	approval := approvalFor(t, request)

	if _, err := store.PromoteCandidate(ctx, request, "sha256:"+evolutionHex(64), request.CreatedAt); err == nil {
		t.Fatal("wrong approval fingerprint must fail closed")
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateEvaluatedPassed {
		t.Fatalf("failed promotion must leave the candidate evaluated_passed: %v", err)
	}
	skipped := request
	skipped.Version = 2
	skipped.ID = promotionIDFor(t, candidate, 2)
	skippedApproval := approvalFor(t, skipped)
	if _, err := store.PromoteCandidate(ctx, skipped, skippedApproval, request.CreatedAt); err == nil {
		t.Fatal("version skip must fail closed")
	}

	promoted, err := store.PromoteCandidate(ctx, request, approval, request.CreatedAt)
	if err != nil {
		t.Fatalf("legitimate promotion: %v", err)
	}
	if promoted.Status != memory.PromotionStatusActive || promoted.Version != 1 || promoted.RollbackTargetID != "" {
		t.Fatalf("promotion row drifted: %#v", promoted)
	}
	activation, found, err := store.ActivationFor(memory.TargetKindContextCompactionParams)
	if err != nil || !found || activation.PromotionID != promoted.ID {
		t.Fatalf("activation must reference the fresh promotion: %+v %v", activation, err)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("post-promotion full check: %v", err)
	}
	if _, err := store.PromoteCandidate(ctx, request, approval, request.CreatedAt.Add(time.Minute)); err == nil {
		t.Fatal("re-promoting the same candidate must conflict")
	}
}

// TestPromoteCandidateRequiresPassedEvaluation 验证 failed 结论候选在晋升
// 入口即被拒绝，账本零残留。
func TestPromoteCandidateRequiresPassedEvaluation(t *testing.T) {
	store, candidate, now := readyEvolutionFixture(t)
	candidate.EvalSetFingerprint = evalSetFrozen
	ctx := context.Background()
	started, err := newEvaluationForBound(candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	started, err = store.StartEvaluation(ctx, candidate.ID, started, evaluationBindingFor(candidate), now)
	if err != nil {
		t.Fatal(err)
	}
	concluded, err := store.FinishEvaluation(ctx, started.ID, memory.EvaluationStatusFailed, suiteResultFrozen, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	request := promotionRequestFor(candidate, concluded, 1)
	request.Conclusion = memory.EvaluationStatusPassed
	request.SuiteResultDigest = suiteResultFrozen
	request.EvalSetFingerprint = evalSetFrozen
	request.ID = promotionIDFor(t, candidate, 1)
	if err := request.Validate(); err != nil {
		t.Fatalf("fixture request must be structurally valid: %v", err)
	}
	if _, err := store.PromoteCandidate(ctx, request, approvalFor(t, request), request.CreatedAt); err == nil {
		t.Fatal("promoting a failed candidate must fail closed")
	}
	if promotions, err := store.ListPromotions(memory.TargetKindContextCompactionParams); err != nil || len(promotions) != 0 {
		t.Fatalf("rejected promotion must leave no ledger rows: %v", err)
	}
}

// TestRollbackPromotionRestoresBaseline 验证回退事务：首版回退清空激活指针
// （回产品基线）、版本置 rolled_back、无活跃版本时重复回退冲突、全库检查
// 通过。
func TestRollbackPromotionRestoresBaseline(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	ctx := context.Background()
	request := promotionRequestFor(candidate, evaluation, 1)
	request.ID = promotionIDFor(t, candidate, 1)
	if _, err := store.PromoteCandidate(ctx, request, approvalFor(t, request), request.CreatedAt); err != nil {
		t.Fatalf("promote before rollback: %v", err)
	}
	rollbackAt := request.CreatedAt.Add(time.Hour)
	rolled, activation, err := store.RollbackPromotion(ctx, memory.TargetKindContextCompactionParams, "human_request", rollbackAt)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolled.Status != memory.PromotionStatusRolledBack || !rolled.RolledBackAt.Equal(rollbackAt) {
		t.Fatalf("rolled row drifted: %#v", rolled)
	}
	if activation.PromotionID != "" {
		t.Fatalf("activation must reset to baseline: %+v", activation)
	}
	if _, found, err := store.ActivationFor(memory.TargetKindContextCompactionParams); err != nil || found {
		t.Fatalf("activation row must be gone: %v", err)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("post-rollback full check: %v", err)
	}
	if _, _, err := store.RollbackPromotion(ctx, memory.TargetKindContextCompactionParams, "human_request", rollbackAt.Add(time.Minute)); err == nil {
		t.Fatal("rollback without an active promotion must conflict")
	}
}

// TestPromotionInvariantCatchesTampering 验证账本不变量的防篡改面：直接
// SQL 篡改版本状态后全库检查失败关闭。
func TestPromotionInvariantCatchesTampering(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	ctx := context.Background()
	request := promotionRequestFor(candidate, evaluation, 1)
	request.ID = promotionIDFor(t, candidate, 1)
	promoted, err := store.PromoteCandidate(ctx, request, approvalFor(t, request), request.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE evolution_promotions SET version = 5 WHERE promotion_id = ?", promoted.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(ctx); err == nil {
		t.Fatal("version tampering must fail the full check")
	}
}
