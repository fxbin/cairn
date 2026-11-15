// [INPUT]: 依赖本包查询端口与复合来源键。
// [OUTPUT]: 提供演化来源、候选、冷却和精确读回。
// [POS]: 来源始终限定同一工作区和版本。
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
