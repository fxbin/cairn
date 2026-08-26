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
	"reflect"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

const (
	migrationVersionFive = 5
	migrationNameFive    = "005_evolution_registry_v5"
	migrationHashFive    = "cairn-schema-v5-trusted-memory-evolution-registry"
)

// EvolutionInspectResult 是演化候选及其只读审计历史的投影。
type EvolutionInspectResult struct {
	Candidate memory.EvolutionCandidate
	Audit     []memory.EvolutionAuditEntry
}

// CreateEvolutionCandidate 在单一事务内写入候选与 created 审计：同 ID 重试
// 按不可变事实幂等返回；冷却键命中或事实冲突失败关闭；写入后读回比对，
// 任一失败整体回滚。
func (store *Store) CreateEvolutionCandidate(ctx context.Context, candidate memory.EvolutionCandidate, now time.Time) (memory.EvolutionCandidate, error) {
	if err := candidate.Validate(); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if candidate.Status != memory.CandidateStateDraft {
		return memory.EvolutionCandidate{}, errors.New("evolution candidate must be created in draft state")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("start evolution candidate creation: %w", err)
	}
	defer transaction.Rollback()
	existing, found, err := lookupEvolutionCandidate(ctx, transaction, candidate.ID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if found {
		existingAudit, err := evolutionAuditFor(ctx, transaction, existing.ID)
		if err != nil {
			return memory.EvolutionCandidate{}, err
		}
		if existing.Status == memory.CandidateStateDraft && evolutionCandidateEquivalent(existing, candidate) && len(existingAudit) >= 1 && existingAudit[0].Action == memory.EvolutionAuditCreated {
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
	if err != nil || !found || !evolutionCandidateEquivalent(readBack, candidate) {
		return memory.EvolutionCandidate{}, errors.New("evolution candidate read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("commit evolution candidate: %w", err)
	}
	return candidate, nil
}

// RejectEvolutionCandidate 将非终态候选按合法迁移置为 rejected，写入封闭理由
// 审计并落一条冷却记录；同候选重复拒绝幂等返回且不重复落冷却。
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
	if err := memory.ValidCandidateTransition(candidate.Status, memory.CandidateStateRejected); err != nil {
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
	candidate.Status = memory.CandidateStateRejected
	if err := transaction.Commit(); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("commit evolution rejection: %w", err)
	}
	return candidate, nil
}

// InvalidateEvolutionCandidate 按合法迁移把候选置为来源失效终态并写审计。
func (store *Store) InvalidateEvolutionCandidate(ctx context.Context, candidateID string, now time.Time) (memory.EvolutionCandidate, error) {
	return store.transitionEvolutionCandidate(ctx, candidateID, memory.CandidateStateInvalidated, memory.EvolutionAuditInvalidated, now)
}

// ExpireEvolutionCandidate 按合法迁移把候选置为保留期耗尽终态并写审计。
func (store *Store) ExpireEvolutionCandidate(ctx context.Context, candidateID string, now time.Time) (memory.EvolutionCandidate, error) {
	return store.transitionEvolutionCandidate(ctx, candidateID, memory.CandidateStateExpired, memory.EvolutionAuditExpired, now)
}

// transitionEvolutionCandidate 执行无理由终态迁移的共用事务路径：幂等、
// 合法迁移校验、审计与读回一致后提交。
func (store *Store) transitionEvolutionCandidate(ctx context.Context, candidateID, targetState, auditAction string, now time.Time) (memory.EvolutionCandidate, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("start evolution transition: %w", err)
	}
	defer transaction.Rollback()
	candidate, found, err := lookupEvolutionCandidate(ctx, transaction, candidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if !found {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	if candidate.Status == targetState {
		if err := transaction.Commit(); err != nil {
			return memory.EvolutionCandidate{}, fmt.Errorf("commit idempotent evolution transition: %w", err)
		}
		return candidate, nil
	}
	if err := memory.ValidCandidateTransition(candidate.Status, targetState); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE evolution_candidates SET status = ? WHERE id = ?", targetState, candidateID); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("transition evolution candidate: %w", err)
	}
	if err := insertEvolutionAudit(ctx, transaction, candidateID, auditAction, "", now); err != nil {
		return memory.EvolutionCandidate{}, err
	}
	candidate.Status = targetState
	if err := transaction.Commit(); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("commit evolution transition: %w", err)
	}
	return candidate, nil
}

// ListEvolutionCandidates 返回全部演化候选供诊断与校验；不做状态过滤。
func (store *Store) ListEvolutionCandidates(ctx context.Context) ([]memory.EvolutionCandidate, error) {
	rows, err := store.db.QueryContext(ctx, evolutionCandidateSelect+" ORDER BY id ASC")
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
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate evolution candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close evolution candidate rows: %w", err)
	}
	for index := range candidates {
		if err := candidates[index].Validate(); err != nil {
			return nil, fmt.Errorf("validate evolution candidate: %w", err)
		}
	}
	return candidates, nil
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
	return EvolutionInspectResult{Candidate: candidate, Audit: audit}, nil
}

// ActiveEvolutionCooldown 在给定时刻返回冷却键的剩余冷却截止；未命中返回零值。
func (store *Store) ActiveEvolutionCooldown(ctx context.Context, key memory.CooldownKey, now time.Time) (time.Time, error) {
	if err := key.Validate(); err != nil {
		return time.Time{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	var until string
	err := store.db.QueryRowContext(ctx, `SELECT cooldown_until FROM evolution_cooldowns
		WHERE source_derived_id = ? AND source_consolidation_id = ? AND target_kind = ? AND variant_fingerprint = ?`,
		key.Source.DerivedID, key.Source.ConsolidationID, key.TargetKind, key.VariantFingerprint).Scan(&until)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("query evolution cooldown: %w", err)
	}
	parsed, err := time.Parse(time.RFC3339Nano, until)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse evolution cooldown: %w", err)
	}
	if !parsed.After(now) {
		return time.Time{}, nil
	}
	return parsed, nil
}

// ensureNoActiveCooldown 在创建事务内拒绝冷却期内的候选重建。
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

// evolutionCandidateEquivalent 比较候选的全部不可变事实；状态列不参与。
func evolutionCandidateEquivalent(left, right memory.EvolutionCandidate) bool {
	return left.ID == right.ID && left.SchemaVersion == right.SchemaVersion && left.WorkspaceHash == right.WorkspaceHash &&
		left.Source == right.Source && left.TargetKind == right.TargetKind && left.VariantSpecSHA256 == right.VariantSpecSHA256 &&
		left.VariantFingerprint == right.VariantFingerprint && left.BaselineFingerprint == right.BaselineFingerprint &&
		left.EvalSetFingerprint == right.EvalSetFingerprint && left.CreatedAt.Equal(right.CreatedAt) &&
		left.ExpiresAt.Equal(right.ExpiresAt) && left.NoteDigest == right.NoteDigest &&
		reflect.DeepEqual(left.ExpiresAt, right.ExpiresAt)
}

// checkEvolutionInvariants 验证演化候选的结构不变量：审计时间线、状态与
// 审计一致、rejected 与冷却绑定、来源派生版本存在且同工作区，以及非终态
// 候选的来源仍为 active（否则必须已迁移到 invalidated）。
func checkEvolutionInvariants(ctx context.Context, source queryerRows) error {
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
			default:
				return fmt.Errorf("evolution candidate %s has an unknown audit action", candidate.ID)
			}
		}
		if created != 1 {
			return fmt.Errorf("evolution candidate %s must have exactly one creation audit", candidate.ID)
		}
		switch candidate.Status {
		case memory.CandidateStateDraft, memory.CandidateStateReady, memory.CandidateStateEvaluating,
			memory.CandidateStateEvaluatedPassed, memory.CandidateStateEvaluatedFailed, memory.CandidateStateInconclusive:
			if rejected != 0 || invalidated != 0 || expired != 0 {
				return fmt.Errorf("non-terminal evolution candidate %s has terminal audit", candidate.ID)
			}
			sourceVersion, foundVersion, err := lookupDerivedVersionExact(ctx, source, candidate.Source.DerivedID, candidate.Source.ConsolidationID)
			if err != nil || !foundVersion {
				return fmt.Errorf("non-terminal evolution candidate %s references a missing source version: %v", candidate.ID, err)
			}
			if sourceVersion.Status != memory.DerivedStatusActive {
				return fmt.Errorf("non-terminal evolution candidate %s has an inactive source version", candidate.ID)
			}
			if sourceVersion.WorkspaceHash != candidate.WorkspaceHash {
				return fmt.Errorf("evolution candidate %s source crosses workspaces", candidate.ID)
			}
		case memory.CandidateStateRejected:
			if rejected != 1 {
				return fmt.Errorf("rejected evolution candidate %s has invalid audit state", candidate.ID)
			}
			until, exists := cooldowns[cooldownKeyOf(candidate)]
			if !exists {
				return fmt.Errorf("rejected evolution candidate %s lacks a cooldown record", candidate.ID)
			}
			_ = until
		case memory.CandidateStateInvalidated:
			if invalidated != 1 {
				return fmt.Errorf("invalidated evolution candidate %s has invalid audit state", candidate.ID)
			}
		case memory.CandidateStateExpired:
			if expired != 1 {
				return fmt.Errorf("expired evolution candidate %s has invalid audit state", candidate.ID)
			}
		case memory.CandidateStatePromoted:
			return fmt.Errorf("evolution candidate %s is promoted outside the permitted slice", candidate.ID)
		}
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
