// Package sqlite 将受限的可信记忆控制面持久化为独立 SQLite 数据库；本文件
// 补充 M4 的演化候选 registry：幂等创建、封闭状态迁移、冷却与审计。
//
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/memory"
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

// InspectEvolutionCandidate 返回候选与审计历史；不存在返回 ErrNotFound。
func (store *Store) InspectEvolutionCandidate(ctx context.Context, candidateID string) (EvolutionInspectResult, error) {
	candidate, found, err := lookupEvolutionCandidate(ctx, store.db, candidateID)
	if err != nil {
		return EvolutionInspectResult{}, err
	}
	if !found {
		return EvolutionInspectResult{}, memory.ErrNotFound
	}
	if err := candidate.Validate(); err != nil {
		return EvolutionInspectResult{}, fmt.Errorf("validate evolution candidate: %w", err)
	}
	audit, err := evolutionAuditFor(ctx, store.db, candidateID)
	if err != nil {
		return EvolutionInspectResult{}, err
	}
	result := EvolutionInspectResult{Candidate: candidate, Audit: audit}
	evaluation, evalErr := store.LookupEvolutionEvaluationByCandidate(ctx, candidateID)
	if evalErr == nil {
		row := evaluation
		result.Evaluation = &row
	} else if !errors.Is(evalErr, memory.ErrNotFound) {
		return EvolutionInspectResult{}, evalErr
	}
	return result, nil
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

const evolutionCandidateSelect = `SELECT id, schema_version, workspace_hash, source_derived_id, source_consolidation_id,
	target_kind, variant_spec_sha256, variant_fingerprint, baseline_fingerprint, eval_set_fingerprint,
	created_at, expires_at, status, note_digest FROM evolution_candidates`

// lookupEvolutionCandidate 按标识查找候选；不存在不是错误。
func lookupEvolutionCandidate(ctx context.Context, source queryer, candidateID string) (memory.EvolutionCandidate, bool, error) {
	candidate, err := scanEvolutionCandidate(source.QueryRowContext(ctx, evolutionCandidateSelect+" WHERE id = ?", candidateID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.EvolutionCandidate{}, false, nil
	}
	if err != nil {
		return memory.EvolutionCandidate{}, false, err
	}
	return candidate, true, nil
}

// scanEvolutionCandidate 从 SQL 行还原候选并解析严格 UTC 时间戳。
func scanEvolutionCandidate(scanner interface{ Scan(...any) error }) (memory.EvolutionCandidate, error) {
	var candidate memory.EvolutionCandidate
	var evalSetFingerprint, noteDigest sql.NullString
	var createdAt, expiresAt string
	if err := scanner.Scan(&candidate.ID, &candidate.SchemaVersion, &candidate.WorkspaceHash, &candidate.Source.DerivedID, &candidate.Source.ConsolidationID,
		&candidate.TargetKind, &candidate.VariantSpecSHA256, &candidate.VariantFingerprint, &candidate.BaselineFingerprint, &evalSetFingerprint,
		&createdAt, &expiresAt, &candidate.Status, &noteDigest); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	var err error
	candidate.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("parse evolution created_at: %w", err)
	}
	candidate.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("parse evolution expires_at: %w", err)
	}
	if evalSetFingerprint.Valid {
		candidate.EvalSetFingerprint = evalSetFingerprint.String
	}
	if noteDigest.Valid {
		candidate.NoteDigest = noteDigest.String
	}
	return candidate, nil
}

// evolutionAuditFor 返回候选审计历史的稳定插入序。
func evolutionAuditFor(ctx context.Context, source queryerRows, candidateID string) ([]memory.EvolutionAuditEntry, error) {
	rows, err := source.QueryContext(ctx, "SELECT candidate_id, action, reason, occurred_at FROM evolution_audit_log WHERE candidate_id = ? ORDER BY id ASC", candidateID)
	if err != nil {
		return nil, fmt.Errorf("query evolution audit: %w", err)
	}
	defer rows.Close()
	var entries []memory.EvolutionAuditEntry
	for rows.Next() {
		var entry memory.EvolutionAuditEntry
		var reason sql.NullString
		var occurredAt string
		if err := rows.Scan(&entry.CandidateID, &entry.Action, &reason, &occurredAt); err != nil {
			return nil, fmt.Errorf("scan evolution audit: %w", err)
		}
		entry.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse evolution audit time: %w", err)
		}
		if reason.Valid {
			entry.Reason = reason.String
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evolution audit: %w", err)
	}
	return entries, nil
}

// evolutionAuditMatches 按条数、顺序及全部闭合字段比较审计读回值；状态写入
// 只能同时带有创建审计和本次唯一终态审计，避免隐藏审计被静默提交。
func evolutionAuditMatches(actual, expected []memory.EvolutionAuditEntry) bool {
	if len(actual) != len(expected) {
		return false
	}
	for index := range expected {
		if actual[index].CandidateID != expected[index].CandidateID || actual[index].Action != expected[index].Action || actual[index].Reason != expected[index].Reason || !actual[index].OccurredAt.Equal(expected[index].OccurredAt) {
			return false
		}
	}
	return true
}

// evolutionCandidateIdempotentEquivalent 比较候选的身份与闭合事实。状态与时间列不参与：
// 创建时刻由首次写入的时钟锚定，同 ID 重试必然携带不同时钟，若参与比较会
// 把幂等重试误判为事实冲突。
func evolutionCandidateIdempotentEquivalent(left, right memory.EvolutionCandidate) bool {
	if left.ExpiresAt.Sub(left.CreatedAt) != right.ExpiresAt.Sub(right.CreatedAt) {
		return false
	}
	return left.ID == right.ID && left.SchemaVersion == right.SchemaVersion && left.WorkspaceHash == right.WorkspaceHash &&
		left.Source == right.Source && left.TargetKind == right.TargetKind && left.VariantSpecSHA256 == right.VariantSpecSHA256 &&
		left.VariantFingerprint == right.VariantFingerprint && left.BaselineFingerprint == right.BaselineFingerprint &&
		left.EvalSetFingerprint == right.EvalSetFingerprint && left.NoteDigest == right.NoteDigest
}

// evolutionCandidateReadBackMatches 比较一次写入后应持久化的完整候选值。
// 它刻意不复用幂等等价：状态和两个绝对时间也必须逐字段一致，防止触发器
// 在 SQL 写入后静默改写候选而仍被事务提交。
func evolutionCandidateReadBackMatches(actual, expected memory.EvolutionCandidate) bool {
	return evolutionCandidateIdempotentEquivalent(actual, expected) && actual.Status == expected.Status &&
		actual.CreatedAt.Equal(expected.CreatedAt) && actual.ExpiresAt.Equal(expected.ExpiresAt)
}

// checkEvolutionInvariants 验证演化候选的结构不变量：审计时间线、状态与
// 审计一致、rejected 与冷却绑定、来源派生版本存在且同工作区，以及非终态
// 候选的来源仍为 active（否则必须已迁移到 invalidated）。到期判定必须使用
// 调用方注入的同一时钟，禁止在此回退到墙钟。
func checkEvolutionInvariants(ctx context.Context, source queryerRows, checkNow time.Time) error {
	return checkEvolutionInvariantsAt(ctx, source, checkNow)
}

// checkEvolutionInvariantsAt 以给定时刻执行演化不变量校验，供测试冻结时钟。
func checkEvolutionInvariantsAt(ctx context.Context, source queryerRows, checkNow time.Time) error {
	candidates, err := listEvolutionCandidatesOn(ctx, source)
	if err != nil {
		return err
	}
	cooldowns, err := listEvolutionCooldowns(ctx, source)
	if err != nil {
		return err
	}
	for _, candidate := range candidates {
		audit, err := evolutionAuditFor(ctx, source, candidate.ID)
		if err != nil {
			return err
		}
		created, rejected, invalidated, expired := 0, 0, 0, 0
		evaluationStarted, evaluationPassed, evaluationFailed, evaluationInconclusive := 0, 0, 0, 0
		promotedAudits, sandboxActivated, promotionRolledBack := 0, 0, 0
		for _, entry := range audit {
			if entry.CandidateID != candidate.ID || entry.OccurredAt.Before(candidate.CreatedAt) {
				return fmt.Errorf("evolution candidate %s has an invalid audit timeline", candidate.ID)
			}
			switch entry.Action {
			case memory.EvolutionAuditCreated:
				if entry.Reason != "" || !entry.OccurredAt.Equal(candidate.CreatedAt) {
					return fmt.Errorf("evolution candidate %s has an invalid creation audit", candidate.ID)
				}
				created++
			case memory.EvolutionAuditRejected:
				if !memory.ValidEvolutionRejectionReason(entry.Reason) {
					return fmt.Errorf("evolution candidate %s has an invalid rejection audit", candidate.ID)
				}
				rejected++
			case memory.EvolutionAuditInvalidated:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid invalidation audit", candidate.ID)
				}
				invalidated++
			case memory.EvolutionAuditExpired:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid expiry audit", candidate.ID)
				}
				expired++
			case memory.EvolutionAuditEvaluationStarted:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation start audit", candidate.ID)
				}
				evaluationStarted++
			case memory.EvolutionAuditEvaluatedPassed:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation pass audit", candidate.ID)
				}
				evaluationPassed++
			case memory.EvolutionAuditEvaluatedFailed:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation fail audit", candidate.ID)
				}
				evaluationFailed++
			case memory.EvolutionAuditEvaluationInconclusive:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid evaluation inconclusive audit", candidate.ID)
				}
				evaluationInconclusive++
			case memory.EvolutionAuditPromoted:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid promotion audit", candidate.ID)
				}
				promotedAudits++
			case memory.EvolutionAuditSandboxActivated:
				if entry.Reason != "" {
					return fmt.Errorf("evolution candidate %s has an invalid sandbox activation audit", candidate.ID)
				}
				sandboxActivated++
			case memory.EvolutionAuditPromotionRolledBack:
				if !memory.ValidEvolutionRejectionReason(entry.Reason) {
					return fmt.Errorf("evolution candidate %s has an invalid rollback audit", candidate.ID)
				}
				promotionRolledBack++
			default:
				return fmt.Errorf("evolution candidate %s has an unknown audit action", candidate.ID)
			}
		}
		if created != 1 {
			return fmt.Errorf("evolution candidate %s must have exactly one creation audit", candidate.ID)
		}
		sourceVersion, err := evolutionSourceForCandidate(ctx, source, candidate)
		if err != nil {
			return err
		}
		evaluationRow, hasEvaluation, err := lookupEvolutionEvaluationForCandidate(ctx, source, candidate.ID)
		if err != nil {
			return err
		}
		terminalEvaluationAudits := evaluationPassed + evaluationFailed + evaluationInconclusive
		switch candidate.Status {
		case memory.CandidateStateDraft:
			// draft 只允许 created 审计与活跃来源；评测生命周期尚未开始。
			if rejected != 0 || invalidated != 0 || expired != 0 || evaluationStarted != 0 || terminalEvaluationAudits != 0 {
				return fmt.Errorf("draft evolution candidate %s has terminal audit", candidate.ID)
			}
			if sourceVersion.Status != memory.DerivedStatusActive {
				return fmt.Errorf("draft evolution candidate %s has an inactive source version", candidate.ID)
			}
			if !sourceVersion.ExpiresAt.After(checkNow) {
				return fmt.Errorf("draft evolution candidate %s has an expired source version", candidate.ID)
			}
		case memory.CandidateStateReady:
			// ready 表示冻结输入就绪但评测未启动：禁止一切终态与评测审计，
			// 冻结指纹必须随登记写入。
			if rejected != 0 || invalidated != 0 || expired != 0 || evaluationStarted != 0 || terminalEvaluationAudits != 0 {
				return fmt.Errorf("ready evolution candidate %s has terminal or evaluation audit", candidate.ID)
			}
			if sourceVersion.Status != memory.DerivedStatusActive {
				return fmt.Errorf("ready evolution candidate %s has an inactive source version", candidate.ID)
			}
			if !sourceVersion.ExpiresAt.After(checkNow) {
				return fmt.Errorf("ready evolution candidate %s has an expired source version", candidate.ID)
			}
		case memory.CandidateStateEvaluating:
			if evaluationStarted != 1 || terminalEvaluationAudits != 0 || rejected != 0 || invalidated != 0 || expired != 0 {
				return fmt.Errorf("evaluating evolution candidate %s has invalid audit state", candidate.ID)
			}
			if !hasEvaluation || evaluationRow.Status != memory.EvaluationStatusRunning {
				return fmt.Errorf("evaluating evolution candidate %s lacks a running evaluation row", candidate.ID)
			}
			if sourceVersion.Status != memory.DerivedStatusActive {
				return fmt.Errorf("evaluating evolution candidate %s has an inactive source version", candidate.ID)
			}
			if !sourceVersion.ExpiresAt.After(checkNow) {
				return fmt.Errorf("evaluating evolution candidate %s has an expired source version", candidate.ID)
			}
		case memory.CandidateStateEvaluatedPassed, memory.CandidateStateEvaluatedFailed, memory.CandidateStateInconclusive:
			expectedConclusion := 0
			rowStatus := ""
			if candidate.Status == memory.CandidateStateEvaluatedPassed {
				expectedConclusion, rowStatus = evaluationPassed, memory.EvaluationStatusPassed
			} else if candidate.Status == memory.CandidateStateEvaluatedFailed {
				expectedConclusion, rowStatus = evaluationFailed, memory.EvaluationStatusFailed
			} else {
				expectedConclusion, rowStatus = evaluationInconclusive, memory.EvaluationStatusInconclusive
			}
			if evaluationStarted != 1 || expectedConclusion != 1 || terminalEvaluationAudits != 1 || rejected != 0 || invalidated != 0 || expired != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("%s evolution candidate %s has invalid evaluation audit state", candidate.Status, candidate.ID)
			}
			if !hasEvaluation || evaluationRow.Status != rowStatus {
				return fmt.Errorf("%s evolution candidate %s lacks a matching evaluation row", candidate.Status, candidate.ID)
			}
		case memory.CandidateStateRejected:
			if evaluationStarted != 0 || terminalEvaluationAudits != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("rejected evolution candidate %s carries evaluation or promotion audits", candidate.ID)
			}
			if rejected != 1 || invalidated != 0 || expired != 0 {
				return fmt.Errorf("rejected evolution candidate %s has invalid audit state", candidate.ID)
			}
			until, exists := cooldowns[cooldownKeyOf(candidate)]
			if !exists {
				return fmt.Errorf("rejected evolution candidate %s lacks a cooldown record", candidate.ID)
			}
			var rejectTime time.Time
			for _, entry := range audit {
				if entry.Action == memory.EvolutionAuditRejected {
					rejectTime = entry.OccurredAt
				}
			}
			if rejectTime.IsZero() || until.Before(rejectTime) {
				return fmt.Errorf("rejected evolution candidate %s has an incoherent cooldown deadline", candidate.ID)
			}
		case memory.CandidateStateInvalidated:
			if rejected != 0 || expired != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("invalidated evolution candidate %s has invalid audit state", candidate.ID)
			}
			if hasEvaluation && evaluationRow.Status != memory.EvaluationStatusInvalid {
				return fmt.Errorf("invalidated evolution candidate %s carries a non-invalid evaluation row", candidate.ID)
			}
			if hasEvaluation && evaluationRow.Status == memory.EvaluationStatusInvalid && evaluationStarted == 1 && invalidated == 1 && evaluationPassed+evaluationFailed+evaluationInconclusive == 0 {
				// 评测安全作废路径：隔离或证据合同破坏令结论为 invalid，
				// 候选随之 invalidated；来源活性不再是必要条件。
			} else {
				// 来源失效驱动的受限作废：来源必须已经不处于可用状态。
				if invalidated != 1 || evaluationStarted != 0 || terminalEvaluationAudits != 0 {
					return fmt.Errorf("invalidated evolution candidate %s has invalid audit state", candidate.ID)
				}
				if sourceVersion.Status == memory.DerivedStatusActive && sourceVersion.ExpiresAt.After(checkNow) {
					return fmt.Errorf("invalidated evolution candidate %s has an active source version", candidate.ID)
				}
			}
		case memory.CandidateStateExpired:
			if rejected != 0 || invalidated != 0 || expired != 1 || evaluationStarted != 0 || terminalEvaluationAudits != 0 || promotedAudits != 0 || sandboxActivated != 0 || promotionRolledBack != 0 {
				return fmt.Errorf("expired evolution candidate %s has invalid audit state", candidate.ID)
			}
			if hasEvaluation {
				return fmt.Errorf("expired evolution candidate %s carries an evaluation row", candidate.ID)
			}
			for _, entry := range audit {
				if entry.Action == memory.EvolutionAuditExpired && entry.OccurredAt.Before(candidate.ExpiresAt) {
					return fmt.Errorf("expired evolution candidate %s has an early expiry audit", candidate.ID)
				}
			}
		case memory.CandidateStatePromoted:
			if evaluationStarted != 1 || evaluationPassed != 1 || terminalEvaluationAudits != 1 || promotedAudits != 1 || rejected != 0 || invalidated != 0 || expired != 0 || sandboxActivated > 1 || promotionRolledBack > 1 || promotionRolledBack > sandboxActivated {
				return fmt.Errorf("promoted evolution candidate %s has invalid audit state", candidate.ID)
			}
			if !hasEvaluation || evaluationRow.Status != memory.EvaluationStatusPassed {
				return fmt.Errorf("promoted evolution candidate %s lacks a passed evaluation row", candidate.ID)
			}
			promotionRow, hasPromotion, err := lookupEvolutionPromotionForCandidate(ctx, source, candidate.ID)
			if err != nil {
				return err
			}
			if !hasPromotion {
				return fmt.Errorf("promoted evolution candidate %s lacks a promotion ledger row", candidate.ID)
			}
			expectedStatus := memory.PromotionStatusPromotedInactive
			if sandboxActivated == 1 {
				expectedStatus = memory.PromotionStatusSandboxActivated
			}
			if promotionRolledBack == 1 {
				expectedStatus = memory.PromotionStatusRevoked
			}
			if promotionRow.Status != expectedStatus {
				return fmt.Errorf("promoted evolution candidate %s has a promotion status that diverges from its audit history", candidate.ID)
			}
		default:
			return fmt.Errorf("evolution candidate %s has state %q that is not implemented in this slice", candidate.ID, candidate.Status)
		}
	}
	return nil
}

// evolutionSourceForCandidate 精确读取候选来源并验证工作区归属；来源缺失或跨
// 工作区均属于损坏，不能由终态迁移或完整性检查忽略。
func evolutionSourceForCandidate(ctx context.Context, source queryer, candidate memory.EvolutionCandidate) (memory.DerivedCandidate, error) {
	sourceVersion, foundVersion, err := lookupDerivedVersionExact(ctx, source, candidate.Source.DerivedID, candidate.Source.ConsolidationID)
	if err != nil || !foundVersion {
		return memory.DerivedCandidate{}, fmt.Errorf("evolution candidate %s references a missing source version: %v", candidate.ID, err)
	}
	if sourceVersion.WorkspaceHash != candidate.WorkspaceHash {
		return memory.DerivedCandidate{}, fmt.Errorf("evolution candidate %s source crosses workspaces", candidate.ID)
	}
	return sourceVersion, nil
}

// requireEvolutionSourceActive 复核 draft 候选写入所需的来源资格：精确版本
// 必须同工作区、active 且未过期。创建、拒绝和显式到期都以此为提交前条件。
func requireEvolutionSourceActive(ctx context.Context, source queryer, candidate memory.EvolutionCandidate, now time.Time) error {
	sourceVersion, err := evolutionSourceForCandidate(ctx, source, candidate)
	if err != nil {
		return err
	}
	if sourceVersion.Status != memory.DerivedStatusActive {
		return fmt.Errorf("evolution candidate %s has an inactive source version", candidate.ID)
	}
	if !sourceVersion.ExpiresAt.After(now) {
		return fmt.Errorf("evolution candidate %s has an expired source version", candidate.ID)
	}
	return nil
}

// requireEvolutionSourceInactive 复核受限作废的反向来源资格：精确版本必须
// 存在且同工作区，并且已经不是 active 或已过期；活跃且未过期的来源不能被
// 作废路径洗白。
func requireEvolutionSourceInactive(ctx context.Context, source queryer, candidate memory.EvolutionCandidate, now time.Time) error {
	sourceVersion, err := evolutionSourceForCandidate(ctx, source, candidate)
	if err != nil {
		return err
	}
	if sourceVersion.Status == memory.DerivedStatusActive && sourceVersion.ExpiresAt.After(now) {
		return errors.New("evolution candidate cannot be invalidated while its source version is still active")
	}
	return nil
}

// listEvolutionCandidatesOn 在事务或只读连接上列出全部演化候选。
func listEvolutionCandidatesOn(ctx context.Context, source queryerRows) ([]memory.EvolutionCandidate, error) {
	rows, err := source.QueryContext(ctx, evolutionCandidateSelect+" ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("list evolution candidates: %w", err)
	}
	var candidates []memory.EvolutionCandidate
	for rows.Next() {
		candidate, err := scanEvolutionCandidate(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		if err := candidate.Validate(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("validate evolution candidate: %w", err)
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate evolution candidates: %w", err)
	}
	return candidates, rows.Close()
}

// listEvolutionCooldowns 列出全部冷却记录并按键索引。
func listEvolutionCooldowns(ctx context.Context, source queryerRows) (map[memory.CooldownKey]time.Time, error) {
	rows, err := source.QueryContext(ctx, `SELECT source_derived_id, source_consolidation_id, target_kind, variant_fingerprint, cooldown_until FROM evolution_cooldowns`)
	if err != nil {
		return nil, fmt.Errorf("list evolution cooldowns: %w", err)
	}
	defer rows.Close()
	result := make(map[memory.CooldownKey]time.Time)
	for rows.Next() {
		var key memory.CooldownKey
		var until string
		if err := rows.Scan(&key.Source.DerivedID, &key.Source.ConsolidationID, &key.TargetKind, &key.VariantFingerprint, &until); err != nil {
			return nil, fmt.Errorf("scan evolution cooldown: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, until)
		if err != nil {
			return nil, fmt.Errorf("parse evolution cooldown: %w", err)
		}
		if err := key.Validate(); err != nil {
			return nil, fmt.Errorf("validate evolution cooldown: %w", err)
		}
		result[key] = parsed
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate evolution cooldowns: %w", err)
	}
	return result, nil
}

// lookupDerivedVersionExact 按复合键精确定位一条派生候选版本行。
func lookupDerivedVersionExact(ctx context.Context, source queryer, derivedID, consolidationID string) (memory.DerivedCandidate, bool, error) {
	candidate, err := scanDerived(source.QueryRowContext(ctx, derivedSelectBase+" WHERE id = ? AND consolidation_id = ?", derivedID, consolidationID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.DerivedCandidate{}, false, nil
	}
	if err != nil {
		return memory.DerivedCandidate{}, false, err
	}
	return candidate, true, nil
}
