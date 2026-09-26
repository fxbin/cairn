package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

// passedCandidateFixture 装配一个已通过冻结评测的候选及其评测行。
func passedCandidateFixture(t *testing.T, evalSetFingerprint, suiteDigest string) (*Store, memory.EvolutionCandidate, memory.Evaluation, time.Time) {
	t.Helper()
	store, candidate, now := readyEvolutionFixtureWithSet(t, evalSetFingerprint)
	started, err := newEvaluationForBound(candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	started, err = store.StartEvaluation(context.Background(), candidate.ID, started, evaluationBindingFor(candidate), now)
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := store.FinishEvaluation(context.Background(), started.ID, memory.EvaluationStatusPassed, suiteDigest, now.Add(2*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return store, candidate, evaluation, now
}

// readyEvolutionFixtureWithSet 以指定评测集指纹创建 ready 候选。
func readyEvolutionFixtureWithSet(t *testing.T, evalSetFingerprint string) (*Store, memory.EvolutionCandidate, time.Time) {
	t.Helper()
	store, candidate, now := evolutionFixture(t)
	candidate.Status, candidate.EvalSetFingerprint = memory.CandidateStateReady, evalSetFingerprint
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatal(err)
	}
	return store, candidate, now
}

// newEvaluationForBound 让评测启动时间与测试事务时钟一致。
func newEvaluationForBound(candidate memory.EvolutionCandidate, now time.Time) (memory.Evaluation, error) {
	evaluation, err := newEvaluationFor(candidate)
	if err != nil {
		return memory.Evaluation{}, err
	}
	evaluation.StartedAt = now
	return evaluation, nil
}

// promotionRequestFor 组装尚未批准的版本请求；Store 会在事务内写入审批指纹
// 与决策身份，测试只固定一次性 nonce 和可重派生版本身份。
func promotionRequestFor(t *testing.T, candidate memory.EvolutionCandidate, evaluation memory.Evaluation, version int) memory.Promotion {
	t.Helper()
	variant := memory.CompactionParamsVariant{
		SchemaVersion: memory.EvolutionSchemaVersion, MaxPromptTokens: 50_000, MaxSummaryBytes: 16_384,
		InventoryBudgetPercent: 25, ThresholdReservePercent: 20,
	}
	id, err := memory.PromotionID(memory.TargetKindContextCompactionParams, version)
	if err != nil {
		t.Fatal(err)
	}
	return memory.Promotion{
		ID: id, SchemaVersion: memory.PromotionSchemaVersion, CandidateID: candidate.ID, EvaluationID: evaluation.ID,
		TargetKind: memory.TargetKindContextCompactionParams, Version: version, Variant: variant,
		VariantFingerprint: candidate.VariantFingerprint, VariantSpecSHA256: candidate.VariantSpecSHA256,
		BaselineFingerprint: candidate.BaselineFingerprint, EvalSetFingerprint: evaluation.EvalSetFingerprint,
		SuiteResultDigest: evaluation.SuiteResultDigest, ComparatorVersion: evaluation.ComparatorVersion,
		Conclusion: memory.EvaluationStatusPassed, ConclusionTrigger: "mode_b_efficiency",
		ApprovalNonce: "sha256:" + evolutionHex(97+version), Approver: "fxbin",
		Status: memory.PromotionStatusPromotedInactive, CreatedAt: time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC),
	}
}

// approvalFor 计算操作者必须人工核对的完整决策摘要。
func approvalFor(t *testing.T, request memory.Promotion) string {
	t.Helper()
	fingerprint, err := memory.PromotionApprovalFingerprint(request)
	if err != nil {
		t.Fatal(err)
	}
	return fingerprint
}

// promoteForTest 执行批准决策与 inactive 版本落账。
func promoteForTest(t *testing.T, store *Store, request memory.Promotion) memory.Promotion {
	t.Helper()
	promotion, err := store.PromoteCandidate(context.Background(), request, approvalFor(t, request), request.CreatedAt)
	if err != nil {
		t.Fatal(err)
	}
	return promotion
}

// TestPromoteCandidateCreatesInactiveVersion 验证人工批准只产生决策与 inactive
// 版本，必须经第二个显式动作才能触及 sandbox 指针。
func TestPromoteCandidateCreatesInactiveVersion(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	promotion := promoteForTest(t, store, promotionRequestFor(t, candidate, evaluation, 1))
	if promotion.Status != memory.PromotionStatusPromotedInactive || promotion.DecisionID == "" {
		t.Fatalf("promotion must remain inactive after decision: %#v", promotion)
	}
	if _, found, err := store.ActivationFor(memory.TargetKindContextCompactionParams); err != nil || found {
		t.Fatalf("decision must not activate sandbox: %v", err)
	}
	if promotions, err := store.ListPromotions(candidate.TargetKind); err != nil || len(promotions) != 1 || promotions[0].ID != promotion.ID {
		t.Fatalf("version listing must remain readable on the single connection: %#v %v", promotions, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("inactive decision/version must satisfy Check: %v", err)
	}
}

// TestSandboxActivationAndRollbackRequireSeparateActions 验证 sandbox 激活与
// 回退各有独立事务；空理由必须在写入前失败，不能产生提交后损坏。
func TestSandboxActivationAndRollbackRequireSeparateActions(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	promotion := promoteForTest(t, store, promotionRequestFor(t, candidate, evaluation, 1))
	activated, activation, err := store.ActivatePromotion(context.Background(), promotion.TargetKind, promotion.ID, promotion.CreatedAt.Add(time.Minute))
	if err != nil || activated.Status != memory.PromotionStatusSandboxActivated || activation.Environment != memory.SandboxEnvironment {
		t.Fatalf("explicit activation failed: %#v %#v %v", activated, activation, err)
	}
	if _, _, err := store.RollbackPromotion(context.Background(), promotion.TargetKind, "", promotion.CreatedAt.Add(2*time.Minute)); err == nil {
		t.Fatal("empty rollback reason must fail before mutation")
	}
	if current, found, err := store.ActivationFor(promotion.TargetKind); err != nil || !found || current.PromotionID != promotion.ID {
		t.Fatalf("failed rollback must preserve sandbox pointer: %#v %v", current, err)
	}
	rolled, baseline, err := store.RollbackPromotion(context.Background(), promotion.TargetKind, memory.RejectReasonHumanRequest, promotion.CreatedAt.Add(3*time.Minute))
	if err != nil || rolled.Status != memory.PromotionStatusRevoked || baseline.PromotionID != "" {
		t.Fatalf("rollback must revoke version and restore baseline: %#v %#v %v", rolled, baseline, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("rollback must leave a self-consistent store: %v", err)
	}
}

// TestPromotionRevalidatesSource 验证评测通过后来源失效时，晋升事务不留下
// 决策、版本、激活或审计残留。
func TestPromotionRevalidatesSource(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusRejected, candidate.Source.DerivedID, candidate.Source.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	request := promotionRequestFor(t, candidate, evaluation, 1)
	if _, err := store.PromoteCandidate(context.Background(), request, approvalFor(t, request), request.CreatedAt); err == nil {
		t.Fatal("inactive source must reject promotion")
	}
	if promotions, err := store.ListPromotions(candidate.TargetKind); err != nil || len(promotions) != 0 {
		t.Fatalf("source failure must leave no version residue: %#v %v", promotions, err)
	}
}

// TestApprovalFingerprintBindsRollbackAndNonce 验证回退目标或 nonce 改变后，
// 已复制的批准摘要不能被重放。
func TestApprovalFingerprintBindsRollbackAndNonce(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	request := promotionRequestFor(t, candidate, evaluation, 1)
	approval := approvalFor(t, request)
	request.ApprovalNonce = "sha256:" + evolutionHex(99)
	if _, err := store.PromoteCandidate(context.Background(), request, approval, request.CreatedAt); err == nil {
		t.Fatal("changed nonce must invalidate the copied approval")
	}
	inspected, err := store.InspectEvolutionCandidate(context.Background(), candidate.ID)
	if err != nil || inspected.Candidate.Status != memory.CandidateStateEvaluatedPassed {
		t.Fatalf("approval mismatch must keep candidate unchanged: %#v %v", inspected.Candidate, err)
	}
}

// TestPromotionCheckCrossValidatesEvidenceTuple 验证形态合法的证据字段篡改
// 仍会被全库 Check 检出。
func TestPromotionCheckCrossValidatesEvidenceTuple(t *testing.T) {
	store, candidate, evaluation, _ := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	promotion := promoteForTest(t, store, promotionRequestFor(t, candidate, evaluation, 1))
	if _, err := store.db.ExecContext(context.Background(), "UPDATE evolution_promotion_decisions SET baseline_fingerprint = ? WHERE decision_id = ?", "sha256:"+evolutionHex(99), promotion.DecisionID); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("tampered evidence tuple must fail Check")
	}
}

// TestSandboxRollbackRestoresApprovedPreviousVersion 验证激活前固定的 previous
// version 是回退唯一目标，不能按当前最新版本推断。
func TestSandboxRollbackRestoresApprovedPreviousVersion(t *testing.T) {
	store, firstCandidate, firstEvaluation, now := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	first := promoteForTest(t, store, promotionRequestFor(t, firstCandidate, firstEvaluation, 1))
	if _, _, err := store.ActivatePromotion(context.Background(), first.TargetKind, first.ID, now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	secondCandidate, secondEvaluation := nextVariantPassedCandidate(t, store, firstCandidate, now, 51_000)
	secondRequest := promotionRequestFor(t, secondCandidate, secondEvaluation, 2)
	secondRequest.RollbackTargetID = first.ID
	second := promoteForTest(t, store, secondRequest)
	if _, _, err := store.ActivatePromotion(context.Background(), second.TargetKind, second.ID, now.Add(9*time.Minute)); err != nil {
		t.Fatal(err)
	}
	rolled, activation, err := store.RollbackPromotion(context.Background(), second.TargetKind, memory.RejectReasonHumanRequest, now.Add(10*time.Minute))
	if err != nil || rolled.Status != memory.PromotionStatusRevoked || activation.PromotionID != first.ID {
		t.Fatalf("rollback must restore the approved previous version: %#v %#v %v", rolled, activation, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// secondPassedCandidate 在同一库内追加第二个通过评测的候选供版本链测试。
func nextVariantPassedCandidate(t *testing.T, store *Store, template memory.EvolutionCandidate, now time.Time, promptTokens int) (memory.EvolutionCandidate, memory.Evaluation) {
	t.Helper()
	variant := memory.CompactionParamsVariant{SchemaVersion: memory.EvolutionSchemaVersion, MaxPromptTokens: promptTokens, MaxSummaryBytes: 16_384, InventoryBudgetPercent: 25, ThresholdReservePercent: 20}
	variantFingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, variant)
	if err != nil {
		t.Fatal(err)
	}
	variantDigest, err := memory.VariantSpecDigest(variant)
	if err != nil {
		t.Fatal(err)
	}
	candidate := template
	candidate.ID = memory.EvolutionCandidateID(template.WorkspaceHash, template.Source, memory.TargetKindContextCompactionParams, variantFingerprint)
	candidate.VariantFingerprint, candidate.VariantSpecSHA256 = variantFingerprint, variantDigest
	candidate.CreatedAt, candidate.ExpiresAt = now.Add(4*time.Minute), now.Add(memory.DefaultCandidateTTL)
	candidate.Status, candidate.EvalSetFingerprint = memory.CandidateStateReady, evalSetFrozen
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, candidate.CreatedAt); err != nil {
		t.Fatal(err)
	}
	started, err := newEvaluationForBound(candidate, now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	started, err = store.StartEvaluation(context.Background(), candidate.ID, started, evaluationBindingFor(candidate), now.Add(5*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	evaluation, err := store.FinishEvaluation(context.Background(), started.ID, memory.EvaluationStatusPassed, suiteResultFrozen, now.Add(6*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return candidate, evaluation
}

// TestThreeTierChainRollbackNeverDeadEnds 锁定三层版本链的回退语义：逐级回
// 退恢复各层决策固定的审批上下文（previous=被恢复版本的回退目标），全库
// 检查在每步后保持通过，不存在不可回退或需手工修库的死路。
func TestThreeTierChainRollbackNeverDeadEnds(t *testing.T) {
	store, c1, e1, now := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	ctx := context.Background()
	v1 := promoteAndActivateForTest(t, store, promotionRequestFor(t, c1, e1, 1))
	c2, e2 := nextVariantPassedCandidate(t, store, c1, now, 51_000)
	request2 := promotionRequestFor(t, c2, e2, 2)
	request2.RollbackTargetID = v1.ID
	v2 := promoteAndActivateForTest(t, store, request2)
	c3, e3 := nextVariantPassedCandidate(t, store, c2, now, 52_000)
	request3 := promotionRequestFor(t, c3, e3, 3)
	request3.RollbackTargetID = v2.ID
	v3 := promoteAndActivateForTest(t, store, request3)

	// 回退 v3：指针恢复 v2 且 previous=v2 决策的回退目标 v1。
	if _, activation, err := store.RollbackPromotion(ctx, v3.TargetKind, "human_request", v3.CreatedAt.Add(2*time.Minute)); err != nil {
		t.Fatalf("rollback v3: %v", err)
	} else if activation.PromotionID != v2.ID || activation.PreviousPromotionID != v1.ID {
		t.Fatalf("v3 rollback restored an inconsistent pointer: %+v", activation)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("check after v3 rollback: %v", err)
	}
	// 回退 v2：指针恢复 v1 且 previous=v1 决策的空目标（首版）。
	if _, activation, err := store.RollbackPromotion(ctx, v2.TargetKind, "human_request", v2.CreatedAt.Add(3*time.Minute)); err != nil {
		t.Fatalf("rollback v2 must stay available: %v", err)
	} else if activation.PromotionID != v1.ID || activation.PreviousPromotionID != "" {
		t.Fatalf("v2 rollback restored an inconsistent pointer: %+v", activation)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("check after v2 rollback: %v", err)
	}
	// 回退 v1：清空指针回产品基线。
	if _, activation, err := store.RollbackPromotion(ctx, v1.TargetKind, "human_request", v1.CreatedAt.Add(4*time.Minute)); err != nil {
		t.Fatalf("rollback v1 must reach baseline: %v", err)
	} else if activation.PromotionID != "" {
		t.Fatalf("v1 rollback must reset to baseline: %+v", activation)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("check after v1 rollback: %v", err)
	}
}

// promoteAndActivateForTest 是晋升加沙箱激活的连击封装；激活时刻锚定晋升行
// 的落账时刻，保证候选审计时间线单调。
func promoteAndActivateForTest(t *testing.T, store *Store, request memory.Promotion) memory.Promotion {
	t.Helper()
	promoted := promoteForTest(t, store, request)
	if _, _, err := store.ActivatePromotion(context.Background(), promoted.TargetKind, promoted.ID, promoted.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatalf("activate %s: %v", promoted.ID, err)
	}
	return promoted
}

// TestMigrationEightFailsClosedOnLegacyLedger 守卫 v8 迁移的失败关闭面：v7
// 账本非空时 Open 拒绝升级且版本停留在 7；空库升级路径由全部既有测试的
// 完整迁移链覆盖。
func TestMigrationEightFailsClosedOnLegacyLedger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v7-ledger.db")
	store, seedCandidate, seedEvaluation, _ := passedCandidateFixtureAt(t, path, evalSetFrozen, suiteResultFrozen)
	decision := memory.Promotion{
		ID: "sha256:" + evolutionHex(40), SchemaVersion: memory.PromotionSchemaVersion,
		CandidateID: seedCandidate.ID, EvaluationID: seedEvaluation.ID,
		TargetKind:         memory.TargetKindContextCompactionParams,
		VariantFingerprint: "sha256:" + evolutionHex(42), VariantSpecSHA256: "sha256:" + evolutionHex(43),
		BaselineFingerprint: "sha256:" + evolutionHex(44), EvalSetFingerprint: "sha256:" + evolutionHex(45),
		SuiteResultDigest: "sha256:" + evolutionHex(46), ComparatorVersion: "test",
		ConclusionTrigger: "mode_b_efficiency", Approver: "fxbin",
		ApprovalFingerprint: "sha256:" + evolutionHex(47), ApprovalNonce: "sha256:" + evolutionHex(48),
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO evolution_promotion_decisions(
		decision_id, schema_version, candidate_id, evaluation_id, target_kind, variant_fingerprint, variant_spec_sha256,
		baseline_fingerprint, eval_set_fingerprint, suite_result_digest, comparator_version, conclusion_trigger,
		metrics_json, approval_fingerprint, approval_nonce, approver, decided_at
	) VALUES (?, 1, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '{}', ?, ?, 'fxbin', ?)`,
		decision.ID, decision.CandidateID, decision.EvaluationID, decision.TargetKind,
		decision.VariantFingerprint, decision.VariantSpecSHA256, decision.BaselineFingerprint,
		decision.EvalSetFingerprint, decision.SuiteResultDigest, decision.ComparatorVersion,
		decision.ConclusionTrigger, decision.ApprovalFingerprint, decision.ApprovalNonce,
		time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed legacy ledger row: %v", err)
	}
	if _, err := store.db.ExecContext(context.Background(), `INSERT INTO evolution_promotions(
		promotion_id, schema_version, decision_id, target_kind, version, variant_json, status, created_at
	) VALUES (?, 1, ?, ?, 1, '{}', 'promoted_inactive', ?)`,
		"sha256:"+evolutionHex(37), decision.ID, memory.TargetKindContextCompactionParams, time.Date(2026, 8, 29, 9, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("seed legacy promotion row: %v", err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM schema_migrations WHERE version = ?", migrationVersionEight); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), fmt.Sprintf("PRAGMA user_version = %d", migrationVersionSeven)); err != nil {
		t.Fatal(err)
	}
	store.Close()

	if _, err := Open(path); err == nil {
		t.Fatal("opening a v7 store carrying a legacy promotion ledger must fail closed")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	var version int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrationVersionSeven {
		t.Fatalf("failed migration must leave user_version at %d, got %d", migrationVersionSeven, version)
	}
}

// passedCandidateFixtureAt 在指定路径建库并装配已通过评测的候选。
func passedCandidateFixtureAt(t *testing.T, path, evalSetFingerprint, suiteDigest string) (*Store, memory.EvolutionCandidate, memory.Evaluation, time.Time) {
	t.Helper()
	opened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = opened.Close()
	store, candidate, now := evolutionFixtureOn(t, path)
	candidate.Status = memory.CandidateStateReady
	candidate.EvalSetFingerprint = evalSetFingerprint
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatalf("create ready candidate: %v", err)
	}
	started, err := newEvaluationForBound(candidate, now)
	if err != nil {
		t.Fatal(err)
	}
	started, err = store.StartEvaluation(context.Background(), candidate.ID, started, evaluationBindingFor(candidate), now)
	if err != nil {
		t.Fatalf("start evaluation: %v", err)
	}
	concluded, err := store.FinishEvaluation(context.Background(), started.ID, memory.EvaluationStatusPassed, suiteDigest, now.Add(2*time.Minute))
	if err != nil {
		t.Fatalf("conclude evaluation: %v", err)
	}
	return store, candidate, concluded, now
}

// TestTelemetrySnapshotAggregatesLedgers 锁定聚合快照与手工账目对账：候选
// 状态分布、评测分布、晋升计数、回退耗时序列与召回遥测聚合全部一致。
func TestTelemetrySnapshotAggregatesLedgers(t *testing.T) {
	store, candidate, evaluation, now := passedCandidateFixture(t, evalSetFrozen, suiteResultFrozen)
	ctx := context.Background()
	request := promotionRequestFor(t, candidate, evaluation, 1)
	promoted := promoteForTest(t, store, request)
	if _, _, err := store.ActivatePromotion(ctx, promoted.TargetKind, promoted.ID, request.CreatedAt.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.RollbackPromotion(ctx, promoted.TargetKind, "human_request", request.CreatedAt.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRecallTelemetry(ctx, RecallTelemetryRecord{OccurredAt: now, CandidatesReturned: 2, Conflicts: 1, PlanStrategy: "exact"}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordRecallTelemetry(ctx, RecallTelemetryRecord{OccurredAt: now, CandidatesReturned: 0, EvidenceInsufficient: true, PlanStrategy: "exact"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.TelemetrySnapshot(ctx)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snapshot.Evolution.CandidatesByStatus["promoted"] != 1 || snapshot.Evolution.CandidatesByStatus["ready"] != 0 {
		t.Fatalf("candidate distribution drifted: %+v", snapshot.Evolution.CandidatesByStatus)
	}
	if snapshot.Evolution.EvaluationsByStatus["passed"] != 1 {
		t.Fatalf("evaluation distribution drifted: %+v", snapshot.Evolution.EvaluationsByStatus)
	}
	if snapshot.Evolution.PromotionsRevoked != 1 || snapshot.Evolution.PromotionsActive != 0 {
		t.Fatalf("promotion counts drifted: %+v", snapshot.Evolution)
	}
	if len(snapshot.Evolution.RollbackDurationsMS) != 1 || snapshot.Evolution.RollbackDurationsMS[0] != 2*60_000 {
		t.Fatalf("rollback duration drifted: %+v", snapshot.Evolution.RollbackDurationsMS)
	}
	if snapshot.Recall.TotalCalls != 2 || snapshot.Recall.AverageCandidates != 1.0 ||
		snapshot.Recall.ConflictRate != 0.5 || snapshot.Recall.EvidenceInsufficientRate != 0.5 {
		t.Fatalf("recall aggregate drifted: %+v", snapshot.Recall)
	}
	if snapshot.Consolidation.ConsolidationsCommitted == 0 {
		t.Fatal("consolidation count must reflect fixture history")
	}
}
