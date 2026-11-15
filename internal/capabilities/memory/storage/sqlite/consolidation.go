// [INPUT]: 依赖父能力巩固计划与当前事务。
// [OUTPUT]: 提供批量巩固、派生替代和来源级联撤销。
// [POS]: 整个计划在单事务内提交或回滚。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 提供能力内的SQLite持久化叶子。
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

const (
	migrationVersionTwo      = 2
	migrationNameTwo         = "002_consolidation_v2"
	migrationHashTwo         = "cairn-schema-v2-trusted-memory-consolidation"
	migrationVersionThree    = 3
	migrationNameThree       = "003_consolidation_frozen_supports_v3"
	migrationHashThree       = "cairn-schema-v3-trusted-memory-consolidation-frozen-supports"
	migrationVersionFour     = 4
	migrationNameFour        = "004_derived_lifecycle_v4"
	migrationHashFour        = "cairn-schema-v4-trusted-memory-derived-lifecycle"
	consolidationSelectBase  = `SELECT id, schema_version, strategy, workspace_hash, inputs_digest, input_ids, input_count, committed_at, derived_ttl_seconds, min_pattern_support, min_hypothesis_support, status FROM consolidations`
	derivedSelectBase        = `SELECT id, schema_version, kind, consolidation_id, workspace_hash, scope_digest, payload_json, created_at, expires_at, status, supersedes_id, supersedes_consolidation_id FROM derived_candidates`
	maxDerivedPerConsolidate = 100
)

// ConsolidationResult 是一次巩固事务的返回投影：计划本身与被本事务替代的
// 旧版本标识列表；替代只改变派生候选状态，不删除任何历史行。
type ConsolidationResult struct {
	Plan              memory.PlannedConsolidation
	SupersededVersion []DerivedVersionKey
}

// DerivedVersionKey 唯一定位一条派生候选的版本行。
type DerivedVersionKey struct {
	ID              string
	ConsolidationID string
}

// StoredConsolidation 是 consolidations 表行的只读投影。
type StoredConsolidation struct {
	ID                   string
	SchemaVersion        int
	Strategy             string
	WorkspaceHash        string
	InputsDigest         string
	InputIDs             []string
	InputCount           int
	CommittedAt          time.Time
	DerivedTTL           time.Duration
	MinPatternSupport    int
	MinHypothesisSupport int
	Status               string
}

// Consolidate 在单一 SQLite 事务内提交一份巩固计划。它保留兼容入口，实际
// 调用批量原子接口，因而源输入校验、重算、写入与读回验证全部处于同一事务。
func (store *Store) Consolidate(ctx context.Context, plan memory.PlannedConsolidation) (ConsolidationResult, error) {
	results, err := store.ConsolidateBatch(ctx, []memory.PlannedConsolidation{plan})
	if err != nil {
		return ConsolidationResult{}, err
	}
	if len(results) != 1 {
		return ConsolidationResult{}, errors.New("single consolidation did not return exactly one result")
	}
	return results[0], nil
}

// ConsolidateBatch 将同一次应用巩固调用的全部计划作为唯一 SQLite 事务提交。
// 既有且完全等价的计划在结构校验后直接幂等返回；任一新计划的源候选、冻结
// 参数或写入校验失败都会回滚整个批次，绝不留下部分 consolidation、derived
// candidate、evidence 或 audit 行。
func (store *Store) ConsolidateBatch(ctx context.Context, plans []memory.PlannedConsolidation) ([]ConsolidationResult, error) {
	if len(plans) == 0 {
		return []ConsolidationResult{}, nil
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("start memory consolidation batch: %w", err)
	}
	defer transaction.Rollback()
	committedAt := store.currentUTC()
	results := make([]ConsolidationResult, len(plans))
	newPlans := make([]bool, len(plans))
	for index, plan := range plans {
		if err := validatePlannedConsolidation(plan); err != nil {
			return nil, fmt.Errorf("validate consolidation plan %d structure: %w", index, err)
		}
		existing, found, err := lookupConsolidation(ctx, transaction, plan.ConsolidationID)
		if err != nil {
			return nil, err
		}
		if found {
			equal, err := storedPlanEquivalent(ctx, transaction, existing, plan)
			if err != nil {
				return nil, err
			}
			if !equal {
				return nil, fmt.Errorf("%w: consolidation %s already committed with different facts", memory.ErrConflict, plan.ConsolidationID)
			}
			results[index] = ConsolidationResult{Plan: plan, SupersededVersion: []DerivedVersionKey{}}
			continue
		}
		if err := validatePlanAgainstStoredInputs(ctx, transaction, plan, committedAt); err != nil {
			return nil, fmt.Errorf("validate consolidation plan %d: %w", index, err)
		}
		newPlans[index] = true
	}
	for index, plan := range plans {
		if !newPlans[index] {
			continue
		}
		result, err := persistConsolidationPlan(ctx, transaction, plan, committedAt)
		if err != nil {
			return nil, err
		}
		results[index] = result
	}
	if err := transaction.Commit(); err != nil {
		return nil, fmt.Errorf("commit memory consolidation batch: %w", err)
	}
	return results, nil
}

// persistConsolidationPlan 写入已经在当前事务内完成源候选验证与重算比对的计划，
// 并以事务的真实 UTC 时间记录提交和 consolidated 审计。
func persistConsolidationPlan(ctx context.Context, transaction *sql.Tx, plan memory.PlannedConsolidation, committedAt time.Time) (ConsolidationResult, error) {
	result := ConsolidationResult{Plan: plan, SupersededVersion: []DerivedVersionKey{}}
	inputIDsJSON, err := json.Marshal(plan.InputIDs)
	if err != nil {
		return ConsolidationResult{}, fmt.Errorf("encode consolidation inputs: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO consolidations(
		id, schema_version, strategy, workspace_hash, inputs_digest, input_ids, input_count, committed_at, derived_ttl_seconds,
		min_pattern_support, min_hypothesis_support, status
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.ConsolidationID, plan.SchemaVersion, plan.Strategy, plan.WorkspaceHash, plan.InputsDigest, string(inputIDsJSON), len(plan.InputIDs),
		committedAt.UTC().Format(time.RFC3339Nano), int64(plan.Derived[0].ExpiresAt.Sub(plan.Derived[0].CreatedAt)/time.Second),
		plan.MinPatternSupport, plan.MinHypothesisSupport, memory.ConsolidationStatusCommitted); err != nil {
		return ConsolidationResult{}, fmt.Errorf("insert memory consolidation: %w", err)
	}
	for _, derived := range plan.Derived {
		previous, superseded, err := supersedeActiveDerived(ctx, transaction, derived, plan.ConsolidationID, committedAt)
		if err != nil {
			return ConsolidationResult{}, err
		}
		if superseded {
			result.SupersededVersion = append(result.SupersededVersion, previous)
		}
		version := derived
		version.SupersedesID = previous.ID
		version.SupersedesConsolidationID = previous.ConsolidationID
		if err := insertDerived(ctx, transaction, version, plan.ConsolidationID, committedAt); err != nil {
			return ConsolidationResult{}, err
		}
	}
	readBack, err := readBackPlan(ctx, transaction, plan, committedAt)
	if err != nil {
		return ConsolidationResult{}, err
	}
	if !readBack {
		return ConsolidationResult{}, errors.New("memory consolidation read-back verification failed")
	}
	return result, nil
}

// RejectDerived 将当前 active 的派生候选版本置为 rejected，并以封闭理由写入
// 审计；重复拒绝幂等返回，不产生额外审计行。
func (store *Store) RejectDerived(ctx context.Context, derivedID, reason string, occurredAt time.Time) (DerivedVersionKey, error) {
	if !memory.ValidRejectionReason(reason) {
		return DerivedVersionKey{}, errors.New("derived candidate rejection reason is invalid")
	}
	if occurredAt.IsZero() {
		occurredAt = store.currentUTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return DerivedVersionKey{}, fmt.Errorf("start derived rejection: %w", err)
	}
	defer transaction.Rollback()
	version, found, err := activeDerivedVersion(ctx, transaction, derivedID, store.currentUTC())
	if err != nil {
		return DerivedVersionKey{}, err
	}
	if !found {
		latest, latestFound, latestErr := latestDerivedVersion(ctx, transaction, derivedID)
		if latestErr != nil {
			return DerivedVersionKey{}, latestErr
		}
		if !latestFound {
			return DerivedVersionKey{}, memory.ErrNotFound
		}
		if latest.Status != memory.DerivedStatusRejected {
			return DerivedVersionKey{}, errors.New("only active derived candidates can be rejected")
		}
		if err := transaction.Commit(); err != nil {
			return DerivedVersionKey{}, fmt.Errorf("commit idempotent derived rejection: %w", err)
		}
		return DerivedVersionKey{ID: latest.ID, ConsolidationID: latest.ConsolidationID}, nil
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?",
		memory.DerivedStatusRejected, version.ID, version.ConsolidationID); err != nil {
		return DerivedVersionKey{}, fmt.Errorf("reject derived candidate: %w", err)
	}
	if err := insertDerivedAudit(ctx, transaction, version.ID, version.ConsolidationID, memory.DerivedAuditRejected, reason, occurredAt); err != nil {
		return DerivedVersionKey{}, err
	}
	if err := transaction.Commit(); err != nil {
		return DerivedVersionKey{}, fmt.Errorf("commit derived rejection: %w", err)
	}
	return DerivedVersionKey{ID: version.ID, ConsolidationID: version.ConsolidationID}, nil
}

// ListDerived 返回全部派生候选版本行，供 doctor 与跨库验证使用；不做状态或
// 过期过滤，保证审计与替代链可被完整检查。
func (store *Store) ListDerived(ctx context.Context) ([]memory.DerivedCandidate, error) {
	rows, err := store.db.QueryContext(ctx, derivedSelectBase+" ORDER BY id ASC, consolidation_id ASC")
	if err != nil {
		return nil, fmt.Errorf("list derived candidates: %w", err)
	}
	var derived []memory.DerivedCandidate
	for rows.Next() {
		candidate, err := scanDerived(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		derived = append(derived, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate derived candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close derived candidate rows: %w", err)
	}
	for index := range derived {
		candidate := &derived[index]
		candidate.Evidence, err = derivedEvidenceFor(ctx, store.db, candidate.ID, candidate.ConsolidationID)
		if err != nil {
			return nil, err
		}
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("validate derived candidate: %w", err)
		}
	}
	return derived, nil
}

// ListConsolidations 返回全部巩固事务行，供验证与诊断使用。
func (store *Store) ListConsolidations(ctx context.Context) ([]StoredConsolidation, error) {
	rows, err := store.db.QueryContext(ctx, consolidationSelectBase+" ORDER BY id ASC")
	if err != nil {
		return nil, fmt.Errorf("list consolidations: %w", err)
	}
	defer rows.Close()
	var consolidations []StoredConsolidation
	for rows.Next() {
		stored, err := scanConsolidation(rows)
		if err != nil {
			return nil, err
		}
		consolidations = append(consolidations, stored)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate consolidations: %w", err)
	}
	return consolidations, nil
}

// supersedeActiveDerived 为同一派生标识关闭数据库状态仍为 active 的历史版本，
// 以便写入其真实后继并维持唯一 active 索引；它不将过期版本视为当前事实。审计
// 时间由当前提交事务传入，绝不复用任何输入或派生事实锚点。
func supersedeActiveDerived(ctx context.Context, transaction *sql.Tx, derived memory.DerivedCandidate, consolidationID string, occurredAt time.Time) (DerivedVersionKey, bool, error) {
	rows, err := transaction.QueryContext(ctx, derivedSelectBase+" WHERE id = ? AND status = ?", derived.ID, memory.DerivedStatusActive)
	if err != nil {
		return DerivedVersionKey{}, false, fmt.Errorf("query active derived version: %w", err)
	}
	var previous *memory.DerivedCandidate
	for rows.Next() {
		candidate, err := scanDerived(rows)
		if err != nil {
			rows.Close()
			return DerivedVersionKey{}, false, err
		}
		previous = &candidate
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DerivedVersionKey{}, false, fmt.Errorf("iterate active derived versions: %w", err)
	}
	if err := rows.Close(); err != nil {
		return DerivedVersionKey{}, false, fmt.Errorf("close active derived versions: %w", err)
	}
	if previous == nil {
		return DerivedVersionKey{}, false, nil
	}
	if previous.ConsolidationID == consolidationID {
		return DerivedVersionKey{}, false, errors.New("derived candidate already active within the same consolidation")
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?",
		memory.DerivedStatusSuperseded, previous.ID, previous.ConsolidationID); err != nil {
		return DerivedVersionKey{}, false, fmt.Errorf("supersede derived candidate: %w", err)
	}
	if err := insertDerivedAudit(ctx, transaction, previous.ID, previous.ConsolidationID, memory.DerivedAuditSuperseded, "", occurredAt); err != nil {
		return DerivedVersionKey{}, false, err
	}
	return DerivedVersionKey{ID: previous.ID, ConsolidationID: previous.ConsolidationID}, true, nil
}

// insertDerived 写入派生候选版本行、间接证据与 consolidated 审计。候选的
// CreatedAt 保持来源事实锚点，完整 predecessor key 从候选自身取得，而审计使用
// 当前事务的真实提交时间。
func insertDerived(ctx context.Context, transaction *sql.Tx, derived memory.DerivedCandidate, consolidationID string, committedAt time.Time) error {
	if _, err := transaction.ExecContext(ctx, `INSERT INTO derived_candidates(
		id, schema_version, kind, consolidation_id, workspace_hash, scope_digest, payload_json,
		created_at, expires_at, status, supersedes_id, supersedes_consolidation_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		derived.ID, derived.SchemaVersion, derived.Kind, consolidationID, derived.WorkspaceHash, derived.ScopeDigest, string(derived.Payload),
		derived.CreatedAt.UTC().Format(time.RFC3339Nano), derived.ExpiresAt.UTC().Format(time.RFC3339Nano),
		memory.DerivedStatusActive, nullableString(derived.SupersedesID), nullableString(derived.SupersedesConsolidationID)); err != nil {
		return fmt.Errorf("insert derived candidate: %w", err)
	}
	for ordinal, reference := range derived.Evidence {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO derived_evidence(
			derived_id, consolidation_id, ordinal, source_candidate_id
		) VALUES (?, ?, ?, ?)`, derived.ID, consolidationID, ordinal, reference.CandidateID); err != nil {
			return fmt.Errorf("insert derived evidence: %w", err)
		}
	}
	return insertDerivedAudit(ctx, transaction, derived.ID, consolidationID, memory.DerivedAuditConsolidated, "", committedAt)
}

// insertDerivedAudit 写入派生候选的封闭审计行；reason 仅拒绝时允许非空。
func insertDerivedAudit(ctx context.Context, transaction *sql.Tx, derivedID, consolidationID, action, reason string, occurredAt time.Time) error {
	if _, err := transaction.ExecContext(ctx, `INSERT INTO derived_audit_log(
		derived_id, consolidation_id, action, reason, occurred_at
	) VALUES (?, ?, ?, ?, ?)`, derivedID, consolidationID, action, nullableString(reason), occurredAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert derived audit: %w", err)
	}
	return nil
}

// rejectDependentDerived 在来源候选被显式撤销或替代前的同一 SQLite 事务中，关闭
// 所有当前可操作且引用该来源的派生版本。到期版本或已含失效来源的历史版本不会
// 被当作当前事实；每个关闭版本仅写一次 insufficient_evidence 审计，历史候选、
// 证据和原有 consolidation 永不删除。
func rejectDependentDerived(ctx context.Context, transaction *sql.Tx, sourceCandidateID string, occurredAt time.Time) error {
	rows, err := transaction.QueryContext(ctx, `SELECT derived_candidates.id, derived_candidates.consolidation_id, derived_candidates.created_at
		FROM derived_candidates
		JOIN derived_evidence ON derived_evidence.derived_id = derived_candidates.id
			AND derived_evidence.consolidation_id = derived_candidates.consolidation_id
		WHERE derived_evidence.source_candidate_id = ? AND derived_candidates.status = ?
			AND derived_candidates.expires_at > ?
			AND NOT EXISTS (
				SELECT 1 FROM derived_evidence AS all_evidence
				JOIN candidates AS source_candidate ON source_candidate.id = all_evidence.source_candidate_id
				WHERE all_evidence.derived_id = derived_candidates.id
					AND all_evidence.consolidation_id = derived_candidates.consolidation_id
					AND (source_candidate.status != ? OR source_candidate.expires_at <= ?)
			)
		ORDER BY derived_candidates.id ASC, derived_candidates.consolidation_id ASC`,
		sourceCandidateID, memory.DerivedStatusActive, occurredAt.UTC().Format(time.RFC3339Nano),
		memory.StatusActive, occurredAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("query source-dependent derived candidates: %w", err)
	}
	type dependentVersion struct {
		key       DerivedVersionKey
		createdAt time.Time
	}
	versions := make([]dependentVersion, 0)
	for rows.Next() {
		var version dependentVersion
		var createdAt string
		if err := rows.Scan(&version.key.ID, &version.key.ConsolidationID, &createdAt); err != nil {
			rows.Close()
			return fmt.Errorf("scan source-dependent derived candidate: %w", err)
		}
		version.createdAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			rows.Close()
			return fmt.Errorf("parse source-dependent derived creation time: %w", err)
		}
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("iterate source-dependent derived candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close source-dependent derived candidates: %w", err)
	}
	for _, version := range versions {
		closedAt := occurredAt
		if closedAt.Before(version.createdAt) {
			closedAt = version.createdAt
		}
		updated, err := transaction.ExecContext(ctx, `UPDATE derived_candidates SET status = ?
			WHERE id = ? AND consolidation_id = ? AND status = ?`, memory.DerivedStatusRejected, version.key.ID, version.key.ConsolidationID, memory.DerivedStatusActive)
		if err != nil {
			return fmt.Errorf("close source-dependent derived candidate: %w", err)
		}
		affected, err := updated.RowsAffected()
		if err != nil {
			return fmt.Errorf("count source-dependent derived closure: %w", err)
		}
		if affected != 1 {
			return errors.New("source-dependent derived candidate changed during closure")
		}
		if err := insertDerivedAudit(ctx, transaction, version.key.ID, version.key.ConsolidationID, memory.DerivedAuditRejected, memory.RejectInsufficientEvidence, closedAt); err != nil {
			return err
		}
	}
	return nil
}
