package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

// consolidationStoreFixture 打开临时 Memory Store 并写入两条同事实输入候选。
func consolidationStoreFixture(t *testing.T) (*Store, memory.ConsolidationInputs, time.Time) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	workspaceHash := "sha256:" + consolidationHex(1)
	taskDigest := "sha256:" + consolidationHex(2)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []memory.Candidate{
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_a", now.Add(-2*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_b", now.Add(-1*time.Hour)),
	}}
	for _, candidate := range inputs.Candidates {
		if _, err := store.Capture(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
	}
	return store, inputs, now
}

// consolidationBatchFixture 打开含两个任务摘要、同一失败类别的输入集合，确保
// 一次确定性归纳同时产生 hypothesis 与 pattern 两个策略计划。
func consolidationBatchFixture(t *testing.T) (*Store, memory.ConsolidationInputs, time.Time) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	workspaceHash := "sha256:" + consolidationHex(1)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []memory.Candidate{
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(2), "run_a", now.Add(-4*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(2), "run_b", now.Add(-3*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(4), "run_c", now.Add(-2*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(4), "run_d", now.Add(-time.Hour)),
	}}
	for _, candidate := range inputs.Candidates {
		if _, err := store.Capture(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
	}
	return store, inputs, now
}

// storePath 返回测试 Store 当前主数据库的绝对路径，供关闭后重开耐久性断言使用。
func storePath(t *testing.T, store *Store) string {
	t.Helper()
	var sequence int
	var name, path string
	if err := store.db.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	return path
}

// consolidationInputCandidate 构造一条合法 run_outcome 输入候选。
func consolidationInputCandidate(t *testing.T, workspaceHash, taskDigest, runID string, createdAt time.Time) memory.Candidate {
	t.Helper()
	evidence := []memory.EventEvidenceRef{{RunID: runID, EventSeq: 3, EventHash: "sha256:" + consolidationHex(3), EventType: "run_failed"}}
	candidate, err := memory.NewRunOutcomeCandidate(workspaceHash, runID, taskDigest, "failed", "budget_turns", false, createdAt, memory.DefaultTTL, evidence, "")
	if err != nil {
		t.Fatalf("input candidate %s: %v", runID, err)
	}
	return candidate
}

// consolidationHex 生成测试用 sha256 形式十六进制摘要。
func consolidationHex(seed int) string {
	const hexChars = "0123456789abcdef"
	result := make([]byte, 0, 64)
	for index := 0; index < 64; index++ {
		result = append(result, hexChars[(index+seed)%len(hexChars)])
	}
	return string(result)
}

// TestConsolidateCommitsPersistsAndRecomputes 锁定 M3 退出门的基线：事务提交、
// 重开库后审计耐久、Check 按输入重算派生事实一致。
func TestConsolidateCommitsPersistsAndRecomputes(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	committed := 0
	for _, plan := range plans {
		result, err := store.Consolidate(context.Background(), plan)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.SupersededVersion) != 0 {
			t.Fatalf("first consolidation must not supersede: %#v", result.SupersededVersion)
		}
		committed++
	}
	if committed == 0 {
		t.Fatal("expected at least one committed consolidation")
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("post-consolidation check: %v", err)
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(derived) == 0 {
		t.Fatal("derived candidates missing after consolidation")
	}
	for _, candidate := range derived {
		if candidate.Status != memory.DerivedStatusActive {
			t.Fatalf("fresh derived candidate status = %s", candidate.Status)
		}
	}
}

// TestConsolidateIsIdempotentOnRetry 锁定幂等：同一计划重试返回等价结果且不
// 产生重复版本或重复审计。
func TestConsolidateIsIdempotentOnRetry(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		again, err := store.Consolidate(context.Background(), plan)
		if err != nil {
			t.Fatalf("idempotent retry: %v", err)
		}
		if len(again.SupersededVersion) != 0 {
			t.Fatalf("retry must not supersede: %#v", again.SupersededVersion)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	perID := make(map[string]int)
	for _, candidate := range derived {
		perID[candidate.ID]++
	}
	for id, count := range perID {
		if count != 1 {
			t.Fatalf("derived candidate %s has %d versions after retry", id, count)
		}
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestConsolidateExistingPlanIsIdempotentAfterInputsExpire 锁定恢复边界：已完整
// 提交的同一计划即使其历史输入后来过期，仍按已存事实幂等返回，且不会新增
// consolidation、派生版本、间接证据或审计；从未提交的新计划仍由过期校验拒绝。
func TestConsolidateExistingPlanIsIdempotentAfterInputsExpire(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	plan := plans[0]
	if _, err := store.Consolidate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	var beforeConsolidations, beforeDerived, beforeEvidence, beforeAudit int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM consolidations").Scan(&beforeConsolidations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_candidates").Scan(&beforeDerived); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_evidence").Scan(&beforeEvidence); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_audit_log").Scan(&beforeAudit); err != nil {
		t.Fatal(err)
	}
	expiredAt := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET expires_at = ?", expiredAt); err != nil {
		t.Fatal(err)
	}
	again, err := store.Consolidate(context.Background(), plan)
	if err != nil {
		t.Fatalf("expired historical inputs must not break idempotent recovery: %v", err)
	}
	if again.Plan.ConsolidationID != plan.ConsolidationID || len(again.SupersededVersion) != 0 {
		t.Fatalf("idempotent recovery = %#v", again)
	}
	var afterConsolidations, afterDerived, afterEvidence, afterAudit int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM consolidations").Scan(&afterConsolidations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_candidates").Scan(&afterDerived); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_evidence").Scan(&afterEvidence); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_audit_log").Scan(&afterAudit); err != nil {
		t.Fatal(err)
	}
	if beforeConsolidations != afterConsolidations || beforeDerived != afterDerived || beforeEvidence != afterEvidence || beforeAudit != afterAudit {
		t.Fatalf("idempotent recovery changed rows: before=(%d,%d,%d,%d) after=(%d,%d,%d,%d)", beforeConsolidations, beforeDerived, beforeEvidence, beforeAudit, afterConsolidations, afterDerived, afterEvidence, afterAudit)
	}
}

// TestConsolidateSupersedesPreviousScopeVersion 锁定版本替代：输入集合扩大后
// 新一轮巩固把同 scope 旧版本置 superseded，替代链与审计完整。
func TestConsolidateSupersedesPreviousScopeVersion(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	first, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(first) == 0 {
		t.Fatalf("first plans = %#v err = %v", first, err)
	}
	for _, plan := range first {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	extra := consolidationInputCandidate(t, inputs.WorkspaceHash, inputs.Candidates[0].TaskPromptDigest, "run_c", now)
	if _, err := store.Capture(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	expanded := memory.ConsolidationInputs{WorkspaceHash: inputs.WorkspaceHash, Candidates: append(append([]memory.Candidate(nil), inputs.Candidates...), extra)}
	second, err := memory.DeriveConsolidations(expanded, memory.ConsolidationOptions{Now: now.Add(time.Hour)})
	if err != nil || len(second) == 0 {
		t.Fatalf("second plans = %#v err = %v", second, err)
	}
	for _, plan := range second {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active := make(map[string]int)
	superseded := make(map[string]int)
	for _, candidate := range derived {
		switch candidate.Status {
		case memory.DerivedStatusActive:
			active[candidate.ID]++
		case memory.DerivedStatusSuperseded:
			superseded[candidate.ID]++
		}
	}
	if len(active) == 0 {
		t.Fatal("no active derived version after superseding round")
	}
	for id := range active {
		if active[id] != 1 {
			t.Fatalf("derived candidate %s has %d active versions", id, active[id])
		}
	}
	if len(superseded) == 0 {
		t.Fatal("expected at least one superseded version after the second round")
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestRejectDerivedIsIdempotentAndClosed 锁定显式拒绝：封闭理由、幂等重拒与
// 非法理由失败关闭。
func TestRejectDerivedIsIdempotentAndClosed(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil || len(derived) == 0 {
		t.Fatalf("derived = %#v err = %v", derived, err)
	}
	target := derived[0]
	if _, err := store.RejectDerived(context.Background(), target.ID, "because_i_say_so", now); err == nil {
		t.Fatal("invalid rejection reason must fail closed")
	}
	key, err := store.RejectDerived(context.Background(), target.ID, memory.RejectHumanRequest, now)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != target.ID {
		t.Fatalf("rejected key = %#v", key)
	}
	repeat, err := store.RejectDerived(context.Background(), target.ID, memory.RejectHumanRequest, now.Add(time.Minute))
	if err != nil || repeat != key {
		t.Fatalf("idempotent rejection = %#v err = %v", repeat, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestConsolidateRollsBackFullyOnFailure 锁定两层失败关闭：计划自洽校验在事务
// 前拒绝损坏计划；临时触发器在事务中途注入失败后，巩固与派生零残留。
func TestConsolidateRollsBackFullyOnFailure(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	corrupted := plans[0]
	corrupted.Derived = append(corrupted.Derived, memory.DerivedCandidate{ID: "pat_foreignbody", SchemaVersion: memory.ConsolidationSchemaVersion, Kind: memory.KindPattern, ConsolidationID: "cons_other"})
	if _, err := store.Consolidate(context.Background(), corrupted); err == nil {
		t.Fatal("inconsistent plan must fail closed")
	}
	assertNoConsolidationResidue(t, store)
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER fail_derived_evidence BEFORE INSERT ON derived_evidence BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err == nil {
		t.Fatal("mid-transaction failure must roll back the consolidation")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER fail_derived_evidence"); err != nil {
		t.Fatal(err)
	}
	assertNoConsolidationResidue(t, store)
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatalf("retry after rollback must succeed: %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestConsolidateRejectsInputsThatDriftBeforeWrite 锁定事务内输入复核：计划的
// 证据不得越出 InputIDs，候选在计划和写入之间若撤销或过期，也必须拒绝且
// 不产生 consolidation、derived、evidence 或 audit 残留。
func TestConsolidateRejectsInputsThatDriftBeforeWrite(t *testing.T) {
	t.Run("evidence outside inputs", func(t *testing.T) {
		store, inputs, now := consolidationStoreFixture(t)
		plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
		if err != nil || len(plans) == 0 {
			t.Fatalf("plans = %#v err = %v", plans, err)
		}
		plan := plans[0]
		outsideID := memory.CandidateID(inputs.WorkspaceHash, "run_outside")
		plan.Derived[0].Evidence[0].CandidateID = outsideID
		sort.Slice(plan.Derived[0].Evidence, func(left, right int) bool {
			return plan.Derived[0].Evidence[left].CandidateID < plan.Derived[0].Evidence[right].CandidateID
		})
		if _, err := store.Consolidate(context.Background(), plan); err == nil {
			t.Fatal("evidence outside input IDs must fail closed")
		}
		assertNoConsolidationResidue(t, store)
	})
	t.Run("revoked input", func(t *testing.T) {
		store, inputs, now := consolidationStoreFixture(t)
		plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
		if err != nil || len(plans) == 0 {
			t.Fatalf("plans = %#v err = %v", plans, err)
		}
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET status = ? WHERE id = ?", memory.StatusRevoked, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Consolidate(context.Background(), plans[0]); err == nil {
			t.Fatal("revoked input must fail closed")
		}
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET status = ? WHERE id = ?", memory.StatusActive, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		assertNoConsolidationResidue(t, store)
	})
	t.Run("expired input", func(t *testing.T) {
		store, inputs, now := consolidationStoreFixture(t)
		plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
		if err != nil || len(plans) == 0 {
			t.Fatalf("plans = %#v err = %v", plans, err)
		}
		expiredAt := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET expires_at = ? WHERE id = ?", expiredAt, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Consolidate(context.Background(), plans[0]); err == nil {
			t.Fatal("expired input must fail closed")
		}
		restoredAt := inputs.Candidates[0].ExpiresAt.UTC().Format(time.RFC3339Nano)
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET expires_at = ? WHERE id = ?", restoredAt, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		assertNoConsolidationResidue(t, store)
	})
}

// TestConsolidateBatchRollsBackEarlierPlan 锁定同一应用调用的多策略原子性：
// 第二个策略触发器失败时，第一个策略写入的所有行也必须随事务整体回滚。
func TestConsolidateBatchRollsBackEarlierPlan(t *testing.T) {
	store, inputs, now := consolidationBatchFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	if plans[0].Strategy != memory.StrategyFailureClassRecurrence || plans[1].Strategy != memory.StrategyTaskOutcomeStability {
		t.Fatalf("unexpected strategy order: %#v", plans)
	}
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER fail_second_consolidation
		BEFORE INSERT ON derived_candidates WHEN NEW.kind = 'pattern'
		BEGIN SELECT RAISE(ABORT, 'injected second-plan failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsolidateBatch(context.Background(), plans); err == nil {
		t.Fatal("second plan failure must roll back the full batch")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER fail_second_consolidation"); err != nil {
		t.Fatal(err)
	}
	assertNoConsolidationResidue(t, store)
}

// TestConsolidateSeparatesFactAnchorFromCommitAuditTime 锁定来源事实时间与真实
// 写入时间的职责分离：输入历史时间只锚定派生候选，consolidations.committed_at
// 和 consolidated 审计必须等于当前事务时间且不能回填该锚点。
func TestConsolidateSeparatesFactAnchorFromCommitAuditTime(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatal(err)
	}
	consolidations, err := store.ListConsolidations(context.Background())
	if err != nil || len(consolidations) != 1 {
		t.Fatalf("consolidations = %#v err = %v", consolidations, err)
	}
	stored := consolidations[0]
	if !stored.CommittedAt.After(plans[0].FactAnchor) || stored.CommittedAt.Equal(plans[0].FactAnchor) {
		t.Fatalf("committed at = %s fact anchor = %s", stored.CommittedAt, plans[0].FactAnchor)
	}
	inspected, err := store.InspectDerived(context.Background(), plans[0].Derived[0].ID)
	if err != nil || len(inspected.Audit) != 1 {
		t.Fatalf("derived audit = %#v err = %v", inspected.Audit, err)
	}
	if !inspected.Audit[0].OccurredAt.Equal(stored.CommittedAt) || inspected.Audit[0].OccurredAt.Equal(plans[0].FactAnchor) {
		t.Fatalf("audit time = %s committed = %s anchor = %s", inspected.Audit[0].OccurredAt, stored.CommittedAt, plans[0].FactAnchor)
	}
}

// TestConsolidatePersistsFrozenSupportForRecompute 锁定非默认最小支持度随计划
// 入库；重开后 Check 必须使用冻结值重算，不能退回编译期默认阈值。
func TestConsolidatePersistsFrozenSupportForRecompute(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	extra := consolidationInputCandidate(t, inputs.WorkspaceHash, inputs.Candidates[0].TaskPromptDigest, "run_c", now)
	if _, err := store.Capture(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	inputs.Candidates = append(inputs.Candidates, extra)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now.Add(time.Hour), MinPatternSupport: 3, MinHypothesisSupport: 3})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatal(err)
	}
	consolidations, err := store.ListConsolidations(context.Background())
	if err != nil || len(consolidations) != 1 {
		t.Fatalf("consolidations = %#v err = %v", consolidations, err)
	}
	if consolidations[0].MinPatternSupport != 3 || consolidations[0].MinHypothesisSupport != 3 {
		t.Fatalf("frozen supports = %#v", consolidations[0])
	}
	path := storePath(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Check(context.Background()); err != nil {
		t.Fatalf("recompute with frozen supports: %v", err)
	}
}

// TestCheckDetectsTTLTampering 锁定保留期篡改：直接改写派生候选过期时间后，
// 重算校验必须失败关闭而不是信任存储 TTL。
func TestCheckDetectsTTLTampering(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET expires_at = ? WHERE id = (SELECT id FROM derived_candidates LIMIT 1)",
		now.Add(memory.MaxTTL).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("tampered retention window must fail the recompute check")
	}
}

// TestConsolidateRejectsEmptyDerivedPlan 锁定空派生计划的失败关闭：
// 不因访问首个派生候选而 panic，返回封闭错误且数据库零残留。
func TestConsolidateRejectsEmptyDerivedPlan(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	empty := plans[0]
	empty.Derived = nil
	if _, err := store.Consolidate(context.Background(), empty); err == nil {
		t.Fatal("empty derived plan must fail closed")
	}
	assertNoConsolidationResidue(t, store)
}

// assertNoConsolidationResidue 断言数据库没有任何巩固或派生残留。
func assertNoConsolidationResidue(t *testing.T, store *Store) {
	t.Helper()
	consolidations, err := store.ListConsolidations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(consolidations) != 0 {
		t.Fatalf("rolled back consolidation rows = %d", len(consolidations))
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(derived) != 0 {
		t.Fatalf("rolled back derived rows = %d", len(derived))
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestCheckDetectsPayloadTampering 锁定重算校验：直接篡改派生载荷后 Check
// 必须失败关闭，而不是信任存储文本。
func TestCheckDetectsPayloadTampering(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET payload_json = ? WHERE id = (SELECT id FROM derived_candidates LIMIT 1)",
		`{"task_prompt_digest":"sha256:`+consolidationHex(9)+`","outcome":"completed","support":2,"trusted_verification":true,"first_created_at":"2026-08-25T10:00:00Z","last_created_at":"2026-08-25T11:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("tampered derived payload must fail the recompute check")
	}
}

// TestMigrationUpgradesV1Database 锁定链式迁移：v1 库打开后升级到 v2 并通过
// 校验；只读打开 v1 库失败关闭（单向兼容）。
func TestMigrationUpgradesV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrationVersion {
		t.Fatalf("migrated user_version = %d want %d", version, migrationVersion)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if err := readOnly.Check(context.Background()); err != nil {
		t.Fatalf("read-only check after migration: %v", err)
	}
}

// TestConsolidationDurableAcrossReopen 锁定审计耐久：关闭并重开数据库后，
// 巩固行、派生版本与审计不变量完整。
func TestConsolidationDurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHash := "sha256:" + consolidationHex(1)
	taskDigest := "sha256:" + consolidationHex(2)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	inputs := memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []memory.Candidate{
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_a", now.Add(-time.Hour)),
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_b", now),
	}}
	for _, candidate := range inputs.Candidates {
		if _, err := store.Capture(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
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
		t.Fatalf("durable audit check: %v", err)
	}
	consolidations, err := reopened.ListConsolidations(context.Background())
	if err != nil || len(consolidations) != len(plans) {
		t.Fatalf("durable consolidations = %#v err = %v", consolidations, err)
	}
}

// TestConflictOnSameConsolidationDifferentFacts 锁定冲突失败关闭：同一巩固
// 标识携带不同派生事实时拒绝而不是覆盖。
func TestConflictOnSameConsolidationDifferentFacts(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	plan := plans[0]
	if _, err := store.Consolidate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	variant := plan
	for index := range variant.Derived {
		variant.Derived[index].ExpiresAt = variant.Derived[index].ExpiresAt.Add(time.Hour)
	}
	if _, err := store.Consolidate(context.Background(), variant); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("conflicting retry must fail with ErrConflict, got %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}
