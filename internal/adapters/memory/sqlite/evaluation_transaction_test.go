package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

// readyCandidateDigest 系列是评测事务测试共用的形态合法摘要。
const (
	evalCandidateFingerprint = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	evalSetFrozen            = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	suiteResultFrozen        = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// readyEvolutionFixture 创建一个携带冻结评测指纹、处于 ready 态的真实候选。
func readyEvolutionFixture(t *testing.T) (*Store, memory.EvolutionCandidate, time.Time) {
	t.Helper()
	store, candidate, now := evolutionFixture(t)
	candidate.Status = memory.CandidateStateReady
	candidate.EvalSetFingerprint = evalSetFrozen
	if _, err := store.CreateEvolutionCandidate(context.Background(), candidate, now); err != nil {
		t.Fatalf("create ready candidate: %v", err)
	}
	return store, candidate, now
}

// newEvaluationFor 为候选派生合法 running 评测行。
func newEvaluationFor(candidate memory.EvolutionCandidate) (memory.Evaluation, error) {
	id, err := memory.EvaluationID(candidate.ID, evalSetFrozen, evalCandidateFingerprint, evalCandidateFingerprint, "cairn-comparator-v1")
	if err != nil {
		return memory.Evaluation{}, err
	}
	return memory.Evaluation{
		ID: id, SchemaVersion: 1, CandidateID: candidate.ID,
		EvalSetFingerprint:        evalSetFrozen,
		BaselineConfigFingerprint: evalCandidateFingerprint, VariantConfigFingerprint: evalCandidateFingerprint,
		ComparatorVersion: "cairn-comparator-v1", Status: memory.EvaluationStatusRunning,
	}, nil
}

// TestEvaluationLifecycleHappyPath 锁定完整生命周期：ready 候选启动 →
// evaluating+running 行+started 审计（Check 通过）→ 通过结论 → 对应候选终态
// +passed 行+尾部审计（Check 通过）→ 结论幂等重放无副作用。
func TestEvaluationLifecycleHappyPath(t *testing.T) {
	store, candidate, now := readyEvolutionFixture(t)
	ctx := context.Background()
	evaluation, err := newEvaluationFor(candidate)
	if err != nil {
		t.Fatal(err)
	}
	started, err := store.StartEvaluation(ctx, candidate.ID, evaluation, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("start evaluation: %v", err)
	}
	if started.Status != memory.EvaluationStatusRunning || !started.StartedAt.Equal(stripMonotonic(now.Add(time.Minute))) {
		t.Fatalf("started row mismatch: %#v", started)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("check after start: %v", err)
	}

	concludedAt := now.Add(2 * time.Hour)
	concluded, err := store.FinishEvaluation(ctx, evaluation.ID, memory.EvaluationStatusPassed, suiteResultFrozen, concludedAt)
	if err != nil {
		t.Fatalf("finish evaluation: %v", err)
	}
	if concluded.Status != memory.EvaluationStatusPassed || concluded.SuiteResultDigest != suiteResultFrozen || concluded.FinishedAt.IsZero() {
		t.Fatalf("concluded row mismatch: %#v", concluded)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("check after conclusion: %v", err)
	}

	replayed, err := store.FinishEvaluation(ctx, evaluation.ID, memory.EvaluationStatusPassed, suiteResultFrozen, now.Add(3*time.Hour))
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	if !replayed.FinishedAt.Equal(concluded.FinishedAt) {
		t.Fatal("idempotent replay must not move the finish timestamp")
	}
	if _, err := store.StartEvaluation(ctx, candidate.ID, evaluation, now); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("starting a concluded candidate must conflict, got %v", err)
	}
}

// TestStartEvaluationRejectsBrokenPrerequisites 覆盖启动前置：draft 候选拒绝、
// 来源失效拒绝；两种情况下都不产生任何评测行与审计行。
func TestStartEvaluationRejectsBrokenPrerequisites(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	ctx := context.Background()
	evaluation, err := newEvaluationFor(candidate)
	if err != nil {
		t.Fatal(err)
	}
	evaluation.CandidateID = candidate.ID
	if _, err := store.StartEvaluation(ctx, candidate.ID, evaluation, now); err == nil {
		t.Fatal("draft candidate must not start an evaluation")
	}
	var rows int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM evolution_evaluations").Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rejected start must leave no evaluation rows: %d %v", rows, err)
	}
	var audits int
	if err := store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM evolution_audit_log WHERE action = 'evaluation_started'").Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("rejected start must leave no evaluation audit: %d %v", audits, err)
	}
}

// TestFinishEvaluationRollsBackOnAuditTampering 直接注入一条隐藏终态审计，
// 让结论写集的读回失配：事务必须整体回滚，评测行保持 running 且候选未动。
func TestFinishEvaluationRollsBackOnAuditTampering(t *testing.T) {
	store, candidate, now := readyEvolutionFixture(t)
	ctx := context.Background()
	evaluation, err := newEvaluationFor(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.StartEvaluation(ctx, candidate.ID, evaluation, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO evolution_audit_log (candidate_id, action, reason, occurred_at) VALUES (?, 'expired', NULL, ?)`,
		candidate.ID, now.Add(time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.FinishEvaluation(ctx, evaluation.ID, memory.EvaluationStatusPassed, suiteResultFrozen, now.Add(2*time.Hour)); err == nil {
		t.Fatal("conclusion under tampered audit history must fail")
	}
	row, err := store.LookupEvolutionEvaluationByCandidate(ctx, candidate.ID)
	if err != nil || row.Status != memory.EvaluationStatusRunning {
		t.Fatalf("rolled-back conclusion must keep the row running: %#v %v", row, err)
	}
	var status string
	if err := store.db.QueryRowContext(ctx, "SELECT status FROM evolution_candidates WHERE id = ?", candidate.ID).Scan(&status); err != nil || status != memory.CandidateStateEvaluating {
		t.Fatalf("candidate must stay evaluating after rollback: %s %v", status, err)
	}
}

// TestInvalidConclusionMapsToInvalidationAndCheckPasses 验证 invalid 结论经
// 作废词表闭合候选，且带作废评测行的候选仍能通过全库不变量检查。
func TestInvalidConclusionMapsToInvalidationAndCheckPasses(t *testing.T) {
	store, candidate, now := readyEvolutionFixture(t)
	ctx := context.Background()
	evaluation, err := newEvaluationFor(candidate)
	if err != nil {
		t.Fatal(err)
	}
	started, err := store.StartEvaluation(ctx, candidate.ID, evaluation, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	concluded, err := store.FinishEvaluation(ctx, started.ID, memory.EvaluationStatusInvalid, suiteResultFrozen, now.Add(2*time.Hour))
	if err != nil {
		t.Fatalf("invalid conclusion: %v", err)
	}
	if concluded.Status != memory.EvaluationStatusInvalid {
		t.Fatalf("row status = %q", concluded.Status)
	}
	stored, err := store.InspectEvolutionCandidate(ctx, candidate.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Candidate.Status != memory.CandidateStateInvalidated {
		t.Fatalf("candidate status after invalid conclusion = %q", stored.Candidate.Status)
	}
	if stored.Evaluation == nil || stored.Evaluation.ID != started.ID {
		t.Fatalf("inspect must attach the evaluation row: %#v", stored.Evaluation)
	}
	if err := store.Check(ctx); err != nil {
		t.Fatalf("check with invalidated-plus-invalid-evaluation pair: %v", err)
	}
}
