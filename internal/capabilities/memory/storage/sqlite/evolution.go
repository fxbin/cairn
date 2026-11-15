// [INPUT]: 依赖来源资格、候选合同与当前事务。
// [OUTPUT]: 提供显式创建、拒绝、作废和到期。
// [POS]: 候选、冷却与审计作为一份写集提交。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 提供能力内的SQLite持久化叶子。
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

const (
	migrationVersionFive = 5
	migrationNameFive    = "005_evolution_registry_v5"
	migrationHashFive    = "cairn-schema-v5-trusted-memory-evolution-registry"
)

// v6 迁移三件套：评测注册（evaluations 表）与审计词表扩展、拒绝理由收口。
const (
	migrationVersionSix = 6
	migrationNameSix    = "006_evaluation_registry_v6"
	migrationHashSix    = "cairn-schema-v6-trusted-memory-evaluation-registry"
)

// v7 迁移三件套：晋升版本账本（promotions/activations 表）与审计词表再
// 扩展（晋升与回退两动作）。纯扩词表，存量行不受新 CHECK 约束影响。
const (
	migrationVersionSeven = 7
	migrationNameSeven    = "007_promotion_ledger_v7"
	migrationHashSeven    = "cairn-schema-v7-trusted-memory-promotion-ledger"
)

// v8 将开发期 v7 直接激活账本替换为决策、版本与 sandbox 激活三阶段模型。
const (
	migrationVersionEight = 8
	migrationNameEight    = "008_staged_promotion_activation_v8"
	migrationHashEight    = "cairn-schema-v8-trusted-memory-staged-promotion"
)

// v9 迁移三件套：召回遥测采集表。只存聚合事实（命中数/冲突数/证据不足/
// 策略与时间戳），零正文零路径零密钥。
const (
	migrationVersionNine = 9
	migrationNameNine    = "009_recall_telemetry_v9"
	migrationHashNine    = "cairn-schema-v9-trusted-memory-recall-telemetry"
)

// EvolutionInspectResult 是演化候选及其只读审计历史与评测行的投影；
// 尚未启动评测时 Evaluation 为 nil。
type EvolutionInspectResult struct {
	Candidate  memory.EvolutionCandidate
	Audit      []memory.EvolutionAuditEntry
	Evaluation *memory.Evaluation
}

// CreateEvolutionCandidate 在单一事务内写入候选与 created 审计：同 ID 重试
// 按不可变事实幂等返回；冷却键命中或事实冲突失败关闭；写入后读回比对，
// 任一失败整体回滚。允许两种初始形态：draft（评测集待定）与携带冻结指纹
// 的 ready（评测集随创建显式冻结），避免为后者单独引入二次绑定事件。
func (store *Store) CreateEvolutionCandidate(ctx context.Context, candidate memory.EvolutionCandidate, now time.Time) (memory.EvolutionCandidate, error) {
	if err := candidate.Validate(); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if candidate.Status != memory.CandidateStateDraft && candidate.Status != memory.CandidateStateReady {
		return memory.EvolutionCandidate{}, errors.New("evolution candidate must be created in draft or ready state")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("start evolution candidate creation: %w", err)
	}
	defer transaction.Rollback()
	if err := requireEvolutionSourceActive(ctx, transaction, candidate, now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	existing, found, err := lookupEvolutionCandidate(ctx, transaction, candidate.ID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if found {
		existingAudit, err := evolutionAuditFor(ctx, transaction, existing.ID)
		if err != nil {
			return memory.EvolutionCandidate{}, err
		}
		if (existing.Status == memory.CandidateStateDraft || existing.Status == memory.CandidateStateReady) && evolutionCandidateIdempotentEquivalent(existing, candidate) && evolutionAuditMatches(existingAudit, []memory.EvolutionAuditEntry{{
			CandidateID: candidate.ID,
			Action:      memory.EvolutionAuditCreated,
			OccurredAt:  existing.CreatedAt,
		}}) {
			if err := transaction.Commit(); err != nil {
				return memory.EvolutionCandidate{}, fmt.Errorf("commit idempotent evolution candidate: %w", err)
			}
			return existing, nil
		}
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: evolution candidate %s already exists in state %s", memory.ErrConflict, candidate.ID, existing.Status)
	}
	if err := ensureNoActiveCooldown(ctx, transaction, cooldownKeyOf(candidate), now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_candidates(
		id, schema_version, workspace_hash, source_derived_id, source_consolidation_id,
		target_kind, variant_spec_sha256, variant_fingerprint, baseline_fingerprint,
		eval_set_fingerprint, created_at, expires_at, status, note_digest
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ID, candidate.SchemaVersion, candidate.WorkspaceHash, candidate.Source.DerivedID, candidate.Source.ConsolidationID,
		candidate.TargetKind, candidate.VariantSpecSHA256, candidate.VariantFingerprint, candidate.BaselineFingerprint,
		nullableString(candidate.EvalSetFingerprint), candidate.CreatedAt.UTC().Format(time.RFC3339Nano),
		candidate.ExpiresAt.UTC().Format(time.RFC3339Nano), candidate.Status, nullableString(candidate.NoteDigest)); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("insert evolution candidate: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, candidate.ID, memory.EvolutionAuditCreated, "", candidate.CreatedAt); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	readBack, found, err := lookupEvolutionCandidate(ctx, transaction, candidate.ID)
	if err != nil || !found || !evolutionCandidateReadBackMatches(readBack, candidate) {
		return memory.EvolutionCandidate{}, errors.New("evolution candidate read-back verification failed")
	}
	audit, err := evolutionAuditFor(ctx, transaction, candidate.ID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if !evolutionAuditMatches(audit, []memory.EvolutionAuditEntry{{
		CandidateID: candidate.ID,
		Action:      memory.EvolutionAuditCreated,
		OccurredAt:  candidate.CreatedAt,
	}}) {
		return memory.EvolutionCandidate{}, errors.New("evolution creation audit read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("commit evolution candidate: %w", err)
	}
	return candidate, nil
}

// RejectEvolutionCandidate 将尚未启动评测的 draft/ready 候选置为 rejected，写入封闭理由审计并
// 落一条冷却记录；同候选重复拒绝幂等返回且不重复落冷却。它在事务中复核
// 来源仍 active、未过期且同工作区，防止 Store 直连写入失真终态。
func (store *Store) RejectEvolutionCandidate(ctx context.Context, candidateID, reason string, now time.Time, cooldown time.Duration) (memory.EvolutionCandidate, error) {
	if !memory.ValidEvolutionRejectionReason(reason) {
		return memory.EvolutionCandidate{}, errors.New("evolution rejection reason is invalid")
	}
	normalizedCooldown, err := memory.NormalizeCooldown(cooldown)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("start evolution rejection: %w", err)
	}
	defer transaction.Rollback()
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if !found {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	if candidate.Status == memory.CandidateStateRejected {
		if err := transaction.Commit(); err != nil {
			return memory.EvolutionCandidate{}, fmt.Errorf("commit idempotent evolution rejection: %w", err)
		}
		return candidate, nil
	}
	if candidate.Status != memory.CandidateStateDraft && candidate.Status != memory.CandidateStateReady {
		return memory.EvolutionCandidate{}, fmt.Errorf("evolution candidate %s must be draft or ready before rejection", candidateID)
	}
	if err := requireEvolutionSourceActive(ctx, transaction, candidate, now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_candidates SET status = ? WHERE id = ?", memory.CandidateStateRejected, candidateID); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("reject evolution candidate: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, candidateID, memory.EvolutionAuditRejected, reason, now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	cooldownUntil := now.Add(normalizedCooldown)
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_cooldowns(
		source_derived_id, source_consolidation_id, target_kind, variant_fingerprint, cooldown_until
	) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(source_derived_id, source_consolidation_id, target_kind, variant_fingerprint)
		DO UPDATE SET cooldown_until = excluded.cooldown_until`,
		candidate.Source.DerivedID, candidate.Source.ConsolidationID, candidate.TargetKind, candidate.VariantFingerprint,
		cooldownUntil.UTC().Format(time.RFC3339Nano)); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("record evolution cooldown: %w", err)
	}
	readBack, readBackFound, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	expected := candidate
	expected.Status = memory.CandidateStateRejected
	if err != nil || !readBackFound || !evolutionCandidateReadBackMatches(readBack, expected) {
		return memory.EvolutionCandidate{}, errors.New("evolution rejection read-back verification failed")
	}
	audit, err := evolutionAuditFor(ctx, transaction, candidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if !evolutionAuditMatches(audit, []memory.EvolutionAuditEntry{
		{CandidateID: candidateID, Action: memory.EvolutionAuditCreated, OccurredAt: candidate.CreatedAt},
		{CandidateID: candidateID, Action: memory.EvolutionAuditRejected, Reason: reason, OccurredAt: now},
	}) {
		return memory.EvolutionCandidate{}, errors.New("evolution rejection audit read-back verification failed")
	}
	var storedUntil string
	if err := transaction.QueryRowContext(ctx, `SELECT cooldown_until FROM evolution_cooldowns
		WHERE source_derived_id = ? AND source_consolidation_id = ? AND target_kind = ? AND variant_fingerprint = ?`,
		candidate.Source.DerivedID, candidate.Source.ConsolidationID, candidate.TargetKind, candidate.VariantFingerprint).Scan(&storedUntil); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("read back evolution cooldown: %w", err)
	}
	parsedUntil, err := time.Parse(time.RFC3339Nano, storedUntil)
	if err != nil || !parsedUntil.Equal(cooldownUntil) {
		return memory.EvolutionCandidate{}, errors.New("evolution cooldown read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("commit evolution rejection: %w", err)
	}
	return readBack, nil
}

// InvalidateEvolutionCandidateForInactiveSource 执行受限原子作废：同一事务内
// 先证明该候选的来源派生版本确已失效（存在但非 active，或已过保留期），
// 才允许迁移到 invalidated；来源仍活跃或行缺失都失败关闭。它是"来源被合法
// 替代导致全库校验失败"的唯一预期修复路径，调用方在提交后应执行完整 Check
// 确认恢复一致。事务末尾读回状态与审计行，任一不符整体回滚。
func (store *Store) InvalidateEvolutionCandidateForInactiveSource(ctx context.Context, candidateID string, now time.Time) (memory.EvolutionCandidate, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("start evolution invalidation: %w", err)
	}
	defer transaction.Rollback()
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if !found {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	if candidate.Status == memory.CandidateStateInvalidated {
		if err := transaction.Commit(); err != nil {
			return memory.EvolutionCandidate{}, fmt.Errorf("commit idempotent evolution invalidation: %w", err)
		}
		return candidate, nil
	}
	if candidate.Status != memory.CandidateStateDraft && candidate.Status != memory.CandidateStateReady && candidate.Status != memory.CandidateStateEvaluating {
		return memory.EvolutionCandidate{}, fmt.Errorf("evolution candidate %s must be draft, ready or evaluating before invalidation", candidateID)
	}
	if err := requireEvolutionSourceInactive(ctx, transaction, candidate, now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	var invalidatedEvaluation *memory.Evaluation
	if candidate.Status == memory.CandidateStateEvaluating {
		evaluation, exists, err := lookupEvolutionEvaluationForCandidate(ctx, transaction, candidate.ID)
		if err != nil {
			return memory.EvolutionCandidate{}, err
		}
		if !exists || evaluation.Status != memory.EvaluationStatusRunning {
			return memory.EvolutionCandidate{}, fmt.Errorf("evaluating evolution candidate %s lacks a running evaluation", candidateID)
		}
		digest, err := memory.SourceInvalidationDigest(candidate.ID, evaluation.ID)
		if err != nil {
			return memory.EvolutionCandidate{}, err
		}
		if _, err := transaction.ExecContext(ctx, `UPDATE evolution_evaluations
			SET status = ?, suite_result_digest = ?, finished_at = ? WHERE evaluation_id = ?`,
			memory.EvaluationStatusInvalid, digest, now.UTC().Format(time.RFC3339Nano), evaluation.ID); err != nil {
			return memory.EvolutionCandidate{}, fmt.Errorf("invalidate running evolution evaluation: %w", err)
		}
		evaluation.Status = memory.EvaluationStatusInvalid
		evaluation.SuiteResultDigest = digest
		evaluation.FinishedAt = stripMonotonic(now)
		invalidatedEvaluation = &evaluation
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_candidates SET status = ? WHERE id = ?", memory.CandidateStateInvalidated, candidateID); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("invalidate evolution candidate: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, candidateID, memory.EvolutionAuditInvalidated, "", now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	readBack, readBackFound, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	expected := candidate
	expected.Status = memory.CandidateStateInvalidated
	if err != nil || !readBackFound || !evolutionCandidateReadBackMatches(readBack, expected) {
		return memory.EvolutionCandidate{}, errors.New("evolution invalidation read-back verification failed")
	}
	audit, err := evolutionAuditFor(ctx, transaction, candidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	expectedAudit := []memory.EvolutionAuditEntry{{CandidateID: candidateID, Action: memory.EvolutionAuditCreated, OccurredAt: candidate.CreatedAt}}
	if invalidatedEvaluation != nil {
		expectedAudit = append(expectedAudit, memory.EvolutionAuditEntry{CandidateID: candidateID, Action: memory.EvolutionAuditEvaluationStarted, OccurredAt: invalidatedEvaluation.StartedAt})
		storedEvaluation, found, err := lookupEvolutionEvaluation(ctx, transaction, invalidatedEvaluation.ID)
		if err != nil || !found || !evolutionEvaluationRowsMatch(storedEvaluation, *invalidatedEvaluation) {
			return memory.EvolutionCandidate{}, errors.New("evolution evaluation invalidation read-back verification failed")
		}
	}
	expectedAudit = append(expectedAudit, memory.EvolutionAuditEntry{CandidateID: candidateID, Action: memory.EvolutionAuditInvalidated, OccurredAt: now})
	if !evolutionAuditMatches(audit, expectedAudit) {
		return memory.EvolutionCandidate{}, errors.New("evolution invalidation audit read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("commit evolution invalidation: %w", err)
	}
	return readBack, nil
}

// ExpireEvolutionCandidate 按合法迁移把保留期确已耗尽的候选置为 expired；
// 同一事务内先证明当前时刻已到达 ExpiresAt，再写审计并读回验证。
func (store *Store) ExpireEvolutionCandidate(ctx context.Context, candidateID string, now time.Time) (memory.EvolutionCandidate, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("start evolution expiry: %w", err)
	}
	defer transaction.Rollback()
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if !found {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	if candidate.Status == memory.CandidateStateExpired {
		if err := transaction.Commit(); err != nil {
			return memory.EvolutionCandidate{}, fmt.Errorf("commit idempotent evolution expiry: %w", err)
		}
		return candidate, nil
	}
	if candidate.Status != memory.CandidateStateDraft && candidate.Status != memory.CandidateStateReady {
		return memory.EvolutionCandidate{}, fmt.Errorf("evolution candidate %s must be draft or ready before expiry", candidateID)
	}
	if err := requireEvolutionSourceActive(ctx, transaction, candidate, now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if now.Before(candidate.ExpiresAt) {
		return memory.EvolutionCandidate{}, errors.New("evolution candidate cannot expire before its retention deadline")
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_candidates SET status = ? WHERE id = ?", memory.CandidateStateExpired, candidateID); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("expire evolution candidate: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, candidateID, memory.EvolutionAuditExpired, "", now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	readBack, readBackFound, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	expected := candidate
	expected.Status = memory.CandidateStateExpired
	if err != nil || !readBackFound || !evolutionCandidateReadBackMatches(readBack, expected) {
		return memory.EvolutionCandidate{}, errors.New("evolution expiry read-back verification failed")
	}
	audit, err := evolutionAuditFor(ctx, transaction, candidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if !evolutionAuditMatches(audit, []memory.EvolutionAuditEntry{
		{CandidateID: candidateID, Action: memory.EvolutionAuditCreated, OccurredAt: candidate.CreatedAt},
		{CandidateID: candidateID, Action: memory.EvolutionAuditExpired, OccurredAt: now},
	}) {
		return memory.EvolutionCandidate{}, errors.New("evolution expiry audit read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("commit evolution expiry: %w", err)
	}
	return readBack, nil
}

// ensureNoActiveCooldown 在创建事务内拒绝冷却期内的候选重建。同键重建在
// 正常路径已先被候选唯一键与终态不复活规则永久阻断，本检查是纵深防御：
// 防止未来终态语义被放宽后静默绕过冷却，也覆盖孤立冷却行等异常状态。
func ensureNoActiveCooldown(ctx context.Context, transaction *sql.Tx, key memory.CooldownKey, now time.Time) error {
	if err := key.Validate(); err != nil {
		return err
	}
	var until string
	err := transaction.QueryRowContext(ctx, `SELECT cooldown_until FROM evolution_cooldowns
		WHERE source_derived_id = ? AND source_consolidation_id = ? AND target_kind = ? AND variant_fingerprint = ?`,
		key.Source.DerivedID, key.Source.ConsolidationID, key.TargetKind, key.VariantFingerprint).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("query evolution cooldown: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, until)
	if err != nil {
		return fmt.Errorf("parse evolution cooldown: %w", err)
	}
	if parsed.After(now) {
		return fmt.Errorf("evolution candidate is blocked by cooldown until %s", parsed.Format(time.RFC3339))
	}
	return nil
}

// cooldownKeyOf 从候选构造冷却键。
func cooldownKeyOf(candidate memory.EvolutionCandidate) memory.CooldownKey {
	return memory.CooldownKey{Source: candidate.Source, TargetKind: candidate.TargetKind, VariantFingerprint: candidate.VariantFingerprint}
}

// insertEvolutionAudit 写入候选审计行；reason 仅拒绝时允许非空。
func insertEvolutionAudit(ctx context.Context, transaction *sql.Tx, candidateID, action, reason string, occurredAt time.Time) error {
	if _, err := transaction.ExecContext(ctx, `INSERT INTO evolution_audit_log(
		candidate_id, action, reason, occurred_at
	) VALUES (?, ?, ?, ?)`, candidateID, action, nullableString(reason), occurredAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert evolution audit: %w", err)
	}
	return nil
}
