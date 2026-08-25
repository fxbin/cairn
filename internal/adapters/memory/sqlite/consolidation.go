// Package sqlite 将受限的可信记忆控制面持久化为独立 SQLite 数据库；本文件
// 补充 M3 的版本化巩固事务、派生候选拒绝与只读不变量校验。
//
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

const (
	migrationVersionTwo      = 2
	migrationNameTwo         = "002_consolidation_v2"
	migrationHashTwo         = "cairn-schema-v2-trusted-memory-consolidation"
	consolidationSelectBase  = `SELECT id, schema_version, strategy, workspace_hash, inputs_digest, input_ids, input_count, committed_at, derived_ttl_seconds, status FROM consolidations`
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
	ID            string
	SchemaVersion int
	Strategy      string
	WorkspaceHash string
	InputsDigest  string
	InputIDs      []string
	InputCount    int
	CommittedAt   time.Time
	DerivedTTL    time.Duration
	Status        string
}

// Consolidate 在单一 SQLite 事务内提交一份巩固计划：写入前逐项校验计划
// 自洽（含每条派生候选属于本计划的巩固标识），写入后逐行读回比对，任一
// 失败整体回滚。同一巩固标识的重试按派生事实等价性幂等返回；同 scope 的
// 旧 active 版本在同一事务内置为 superseded 并记录审计。
func (store *Store) Consolidate(ctx context.Context, plan memory.PlannedConsolidation) (ConsolidationResult, error) {
	if err := validatePlannedConsolidation(plan); err != nil {
		return ConsolidationResult{}, err
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return ConsolidationResult{}, fmt.Errorf("start memory consolidation: %w", err)
	}
	defer transaction.Rollback()
	existing, found, err := lookupConsolidation(ctx, transaction, plan.ConsolidationID)
	if err != nil {
		return ConsolidationResult{}, err
	}
	if found {
		equal, err := storedPlanEquivalent(ctx, transaction, existing, plan)
		if err != nil {
			return ConsolidationResult{}, err
		}
		if !equal {
			return ConsolidationResult{}, fmt.Errorf("%w: consolidation %s already committed with different facts", memory.ErrConflict, plan.ConsolidationID)
		}
		if err := transaction.Commit(); err != nil {
			return ConsolidationResult{}, fmt.Errorf("commit idempotent memory consolidation: %w", err)
		}
		return ConsolidationResult{Plan: plan}, nil
	}
	result := ConsolidationResult{Plan: plan, SupersededVersion: []DerivedVersionKey{}}
	inputIDsJSON, err := json.Marshal(plan.InputIDs)
	if err != nil {
		return ConsolidationResult{}, fmt.Errorf("encode consolidation inputs: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO consolidations(
		id, schema_version, strategy, workspace_hash, inputs_digest, input_ids, input_count, committed_at, derived_ttl_seconds, status
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		plan.ConsolidationID, plan.SchemaVersion, plan.Strategy, plan.WorkspaceHash, plan.InputsDigest, string(inputIDsJSON), len(plan.InputIDs),
		plan.CommittedAt.UTC().Format(time.RFC3339Nano), int64(plan.Derived[0].ExpiresAt.Sub(plan.Derived[0].CreatedAt)/time.Second), memory.ConsolidationStatusCommitted); err != nil {
		return ConsolidationResult{}, fmt.Errorf("insert memory consolidation: %w", err)
	}
	for _, derived := range plan.Derived {
		previous, superseded, err := supersedeActiveDerived(ctx, transaction, derived, plan.ConsolidationID)
		if err != nil {
			return ConsolidationResult{}, err
		}
		if superseded {
			result.SupersededVersion = append(result.SupersededVersion, previous)
		}
		version := derived
		version.SupersedesID = previous.ID
		if err := insertDerived(ctx, transaction, version, plan.ConsolidationID, previous.ConsolidationID); err != nil {
			return ConsolidationResult{}, err
		}
	}
	readBack, err := readBackPlan(ctx, transaction, plan)
	if err != nil {
		return ConsolidationResult{}, err
	}
	if !readBack {
		return ConsolidationResult{}, errors.New("memory consolidation read-back verification failed")
	}
	if err := transaction.Commit(); err != nil {
		return ConsolidationResult{}, fmt.Errorf("commit memory consolidation: %w", err)
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
		occurredAt = time.Now().UTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return DerivedVersionKey{}, fmt.Errorf("start derived rejection: %w", err)
	}
	defer transaction.Rollback()
	version, found, err := activeDerivedVersion(ctx, transaction, derivedID)
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

// validatePlannedConsolidation 校验计划自洽：写集摘要与输入一致、派生数量受
// 上限约束、每条派生候选属于本计划并通过合同校验（含审查要求的巩固标识
// 交叉校验），时间锚点不得早于任一输入候选。
func validatePlannedConsolidation(plan memory.PlannedConsolidation) error {
	if plan.SchemaVersion != memory.ConsolidationSchemaVersion {
		return fmt.Errorf("unsupported consolidation schema version %d", plan.SchemaVersion)
	}
	if plan.Strategy != memory.StrategyTaskOutcomeStability && plan.Strategy != memory.StrategyFailureClassRecurrence {
		return fmt.Errorf("unsupported consolidation strategy %q", plan.Strategy)
	}
	if plan.ConsolidationID != memory.ConsolidationID(plan.Strategy, plan.WorkspaceHash, plan.InputsDigest) {
		return errors.New("consolidation id does not match strategy, workspace and inputs digest")
	}
	if memory.InputsDigest(plan.InputIDs) != plan.InputsDigest {
		return errors.New("consolidation inputs digest does not match input ids")
	}
	if plan.CommittedAt.IsZero() {
		return errors.New("consolidation requires a committed-at anchor")
	}
	if len(plan.Derived) == 0 {
		return errors.New("consolidation requires at least one derived candidate")
	}
	if err := validatePlanRetention(plan); err != nil {
		return err
	}
	if len(plan.Derived) > maxDerivedPerConsolidate {
		return fmt.Errorf("consolidation derives %d candidates exceeding the cap %d", len(plan.Derived), maxDerivedPerConsolidate)
	}
	for index, derived := range plan.Derived {
		if derived.ConsolidationID != plan.ConsolidationID {
			return fmt.Errorf("derived candidate %d belongs to a different consolidation", index)
		}
		if derived.WorkspaceHash != plan.WorkspaceHash {
			return fmt.Errorf("derived candidate %d belongs to a different workspace", index)
		}
		if err := derived.Validate(); err != nil {
			return fmt.Errorf("derived candidate %d: %w", index, err)
		}
	}
	return nil
}

// validatePlanRetention 要求计划内全部派生候选共享同一创建锚点与保留期，
// 使巩固事务可以按单一 TTL 值封存并在验证时重算。
func validatePlanRetention(plan memory.PlannedConsolidation) error {
	first := plan.Derived[0]
	for index, derived := range plan.Derived {
		if !derived.CreatedAt.Equal(first.CreatedAt) || derived.ExpiresAt.Sub(derived.CreatedAt) != first.ExpiresAt.Sub(first.CreatedAt) {
			return fmt.Errorf("derived candidate %d retention anchor differs within one consolidation", index)
		}
	}
	return nil
}

// supersedeActiveDerived 将同 scope 的旧 active 版本置为 superseded 并返回其
// 版本键；没有旧版本时返回零值与 false。
func supersedeActiveDerived(ctx context.Context, transaction *sql.Tx, derived memory.DerivedCandidate, consolidationID string) (DerivedVersionKey, bool, error) {
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
	supersededAt := time.Now().UTC()
	if _, err := transaction.ExecContext(ctx, "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?",
		memory.DerivedStatusSuperseded, previous.ID, previous.ConsolidationID); err != nil {
		return DerivedVersionKey{}, false, fmt.Errorf("supersede derived candidate: %w", err)
	}
	if err := insertDerivedAudit(ctx, transaction, previous.ID, previous.ConsolidationID, memory.DerivedAuditSuperseded, "", supersededAt); err != nil {
		return DerivedVersionKey{}, false, err
	}
	return DerivedVersionKey{ID: previous.ID, ConsolidationID: previous.ConsolidationID}, true, nil
}

// insertDerived 写入派生候选版本行、间接证据与 consolidated 审计。
func insertDerived(ctx context.Context, transaction *sql.Tx, derived memory.DerivedCandidate, consolidationID, supersedesConsolidationID string) error {
	if _, err := transaction.ExecContext(ctx, `INSERT INTO derived_candidates(
		id, schema_version, kind, consolidation_id, workspace_hash, scope_digest, payload_json,
		created_at, expires_at, status, supersedes_id, supersedes_consolidation_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		derived.ID, derived.SchemaVersion, derived.Kind, consolidationID, derived.WorkspaceHash, derived.ScopeDigest, string(derived.Payload),
		derived.CreatedAt.UTC().Format(time.RFC3339Nano), derived.ExpiresAt.UTC().Format(time.RFC3339Nano),
		memory.DerivedStatusActive, nullableString(derived.SupersedesID), nullableString(supersedesConsolidationID)); err != nil {
		return fmt.Errorf("insert derived candidate: %w", err)
	}
	for ordinal, reference := range derived.Evidence {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO derived_evidence(
			derived_id, consolidation_id, ordinal, source_candidate_id
		) VALUES (?, ?, ?, ?)`, derived.ID, consolidationID, ordinal, reference.CandidateID); err != nil {
			return fmt.Errorf("insert derived evidence: %w", err)
		}
	}
	return insertDerivedAudit(ctx, transaction, derived.ID, consolidationID, memory.DerivedAuditConsolidated, "", derived.CreatedAt)
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

// lookupConsolidation 按标识查找巩固事务行；不存在不是错误。
func lookupConsolidation(ctx context.Context, source queryer, consolidationID string) (StoredConsolidation, bool, error) {
	stored, err := scanConsolidation(source.QueryRowContext(ctx, consolidationSelectBase+" WHERE id = ?", consolidationID))
	if errors.Is(err, sql.ErrNoRows) {
		return StoredConsolidation{}, false, nil
	}
	if err != nil {
		return StoredConsolidation{}, false, err
	}
	return stored, true, nil
}

// scanConsolidation 从 SQL 行还原巩固事务投影并解析输入标识列表。
func scanConsolidation(scanner interface{ Scan(...any) error }) (StoredConsolidation, error) {
	var stored StoredConsolidation
	var inputIDs string
	var committedAt string
	var ttlSeconds int64
	if err := scanner.Scan(&stored.ID, &stored.SchemaVersion, &stored.Strategy, &stored.WorkspaceHash, &stored.InputsDigest, &inputIDs, &stored.InputCount, &committedAt, &ttlSeconds, &stored.Status); err != nil {
		return StoredConsolidation{}, err
	}
	stored.DerivedTTL = time.Duration(ttlSeconds) * time.Second
	if err := json.Unmarshal([]byte(inputIDs), &stored.InputIDs); err != nil {
		return StoredConsolidation{}, fmt.Errorf("parse consolidation input ids: %w", err)
	}
	var err error
	stored.CommittedAt, err = time.Parse(time.RFC3339Nano, committedAt)
	if err != nil {
		return StoredConsolidation{}, fmt.Errorf("parse consolidation committed_at: %w", err)
	}
	return stored, nil
}

// scanDerived 从 SQL 行还原派生候选版本行。
func scanDerived(scanner interface{ Scan(...any) error }) (memory.DerivedCandidate, error) {
	var candidate memory.DerivedCandidate
	var payload string
	var supersedesID, supersedesConsolidation sql.NullString
	var createdAt, expiresAt string
	if err := scanner.Scan(&candidate.ID, &candidate.SchemaVersion, &candidate.Kind, &candidate.ConsolidationID, &candidate.WorkspaceHash, &candidate.ScopeDigest, &payload, &createdAt, &expiresAt, &candidate.Status, &supersedesID, &supersedesConsolidation); err != nil {
		return memory.DerivedCandidate{}, err
	}
	var err error
	candidate.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return memory.DerivedCandidate{}, fmt.Errorf("parse derived created_at: %w", err)
	}
	candidate.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return memory.DerivedCandidate{}, fmt.Errorf("parse derived expires_at: %w", err)
	}
	candidate.Payload = json.RawMessage(payload)
	if supersedesID.Valid {
		candidate.SupersedesID = supersedesID.String
	}
	return candidate, nil
}

// derivedEvidenceFor 返回派生候选版本行的间接证据引用。
func derivedEvidenceFor(ctx context.Context, source queryerRows, derivedID, consolidationID string) ([]memory.SourceCandidateRef, error) {
	rows, err := source.QueryContext(ctx, `SELECT source_candidate_id FROM derived_evidence
		WHERE derived_id = ? AND consolidation_id = ? ORDER BY ordinal ASC`, derivedID, consolidationID)
	if err != nil {
		return nil, fmt.Errorf("query derived evidence: %w", err)
	}
	defer rows.Close()
	var references []memory.SourceCandidateRef
	for rows.Next() {
		var reference memory.SourceCandidateRef
		if err := rows.Scan(&reference.CandidateID); err != nil {
			return nil, fmt.Errorf("scan derived evidence: %w", err)
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate derived evidence: %w", err)
	}
	return references, nil
}

// activeDerivedVersion 返回指定派生候选当前 active 的唯一版本行。
func activeDerivedVersion(ctx context.Context, source queryerRows, derivedID string) (memory.DerivedCandidate, bool, error) {
	rows, err := source.QueryContext(ctx, derivedSelectBase+" WHERE id = ? AND status = ?", derivedID, memory.DerivedStatusActive)
	if err != nil {
		return memory.DerivedCandidate{}, false, fmt.Errorf("query active derived candidate: %w", err)
	}
	defer rows.Close()
	var found *memory.DerivedCandidate
	for rows.Next() {
		candidate, err := scanDerived(rows)
		if err != nil {
			return memory.DerivedCandidate{}, false, err
		}
		if found != nil {
			return memory.DerivedCandidate{}, false, errors.New("derived candidate has multiple active versions")
		}
		found = &candidate
	}
	if err := rows.Err(); err != nil {
		return memory.DerivedCandidate{}, false, fmt.Errorf("iterate active derived candidates: %w", err)
	}
	if found == nil {
		return memory.DerivedCandidate{}, false, nil
	}
	found.Evidence, err = derivedEvidenceFor(ctx, source, found.ID, found.ConsolidationID)
	if err != nil {
		return memory.DerivedCandidate{}, false, err
	}
	return *found, true, nil
}

// latestDerivedVersion 返回派生候选的最新版本行；不存在时 found 为 false。
func latestDerivedVersion(ctx context.Context, source queryer, derivedID string) (memory.DerivedCandidate, bool, error) {
	candidate, err := scanDerived(source.QueryRowContext(ctx, derivedSelectBase+" WHERE id = ? ORDER BY created_at DESC, consolidation_id DESC LIMIT 1", derivedID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.DerivedCandidate{}, false, nil
	}
	if err != nil {
		return memory.DerivedCandidate{}, false, err
	}
	return candidate, true, nil
}

// storedPlanEquivalent 判断既有巩固事务行与新计划是否派生等价：策略、写集、
// 输入与全部派生候选（载荷字节与证据）完全一致。
func storedPlanEquivalent(ctx context.Context, transaction *sql.Tx, existing StoredConsolidation, plan memory.PlannedConsolidation) (bool, error) {
	if existing.Strategy != plan.Strategy || existing.WorkspaceHash != plan.WorkspaceHash || existing.InputsDigest != plan.InputsDigest || existing.InputCount != len(plan.InputIDs) {
		return false, nil
	}
	if !reflect.DeepEqual(existing.InputIDs, plan.InputIDs) {
		return false, nil
	}
	storedDerived, err := derivedForConsolidation(ctx, transaction, plan.ConsolidationID)
	if err != nil {
		return false, err
	}
	if len(storedDerived) != len(plan.Derived) {
		return false, nil
	}
	byID := make(map[string]memory.DerivedCandidate, len(storedDerived))
	for _, candidate := range storedDerived {
		byID[candidate.ID] = candidate
	}
	for _, candidate := range plan.Derived {
		stored, exists := byID[candidate.ID]
		if !exists {
			return false, nil
		}
		stored.Status = memory.DerivedStatusActive
		if !derivedEquivalent(stored, candidate) {
			return false, nil
		}
	}
	return true, nil
}

// derivedForConsolidation 返回一次巩固事务写入的全部派生候选版本行。
func derivedForConsolidation(ctx context.Context, source queryerRows, consolidationID string) ([]memory.DerivedCandidate, error) {
	rows, err := source.QueryContext(ctx, derivedSelectBase+" WHERE consolidation_id = ? ORDER BY id ASC", consolidationID)
	if err != nil {
		return nil, fmt.Errorf("query consolidation derived candidates: %w", err)
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
		return nil, fmt.Errorf("iterate consolidation derived candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close consolidation derived candidates: %w", err)
	}
	for index := range derived {
		candidate := &derived[index]
		candidate.Evidence, err = derivedEvidenceFor(ctx, source, candidate.ID, candidate.ConsolidationID)
		if err != nil {
			return nil, err
		}
	}
	return derived, nil
}

// derivedEquivalent 比较两个派生候选的不可变事实；状态与替代指针属于版本
// 生命周期，不参与等价判定。
func derivedEquivalent(left, right memory.DerivedCandidate) bool {
	return left.ID == right.ID && left.SchemaVersion == right.SchemaVersion && left.Kind == right.Kind && left.ConsolidationID == right.ConsolidationID &&
		left.WorkspaceHash == right.WorkspaceHash && left.ScopeDigest == right.ScopeDigest && string(left.Payload) == string(right.Payload) &&
		left.CreatedAt.Equal(right.CreatedAt) && left.ExpiresAt.Equal(right.ExpiresAt) && reflect.DeepEqual(left.Evidence, right.Evidence)
}

// readBackPlan 在事务提交前读回全部写入行并与计划逐字节比对，任一不一致
// 都会让整个事务回滚，不留半成品。
func readBackPlan(ctx context.Context, transaction *sql.Tx, plan memory.PlannedConsolidation) (bool, error) {
	stored, found, err := lookupConsolidation(ctx, transaction, plan.ConsolidationID)
	if err != nil || !found {
		return false, err
	}
	if stored.Strategy != plan.Strategy || stored.WorkspaceHash != plan.WorkspaceHash || stored.InputsDigest != plan.InputsDigest ||
		stored.InputCount != len(plan.InputIDs) || !reflect.DeepEqual(stored.InputIDs, plan.InputIDs) || !stored.CommittedAt.Equal(plan.CommittedAt) ||
		stored.Status != memory.ConsolidationStatusCommitted || stored.SchemaVersion != plan.SchemaVersion ||
		stored.DerivedTTL != plan.Derived[0].ExpiresAt.Sub(plan.Derived[0].CreatedAt) {
		return false, nil
	}
	derived, err := derivedForConsolidation(ctx, transaction, plan.ConsolidationID)
	if err != nil {
		return false, err
	}
	if len(derived) != len(plan.Derived) {
		return false, nil
	}
	byID := make(map[string]memory.DerivedCandidate, len(derived))
	for _, candidate := range derived {
		byID[candidate.ID] = candidate
	}
	for _, candidate := range plan.Derived {
		stored, exists := byID[candidate.ID]
		if !exists || !derivedEquivalent(stored, candidate) || stored.Status != memory.DerivedStatusActive {
			return false, nil
		}
	}
	return true, nil
}

// checkDerivedInvariants 验证派生候选与巩固事务的结构不变量：外键、载荷、
// 同 id 唯一 active、替代链、审计时间线，以及按输入重算的派生事实一致性。
func checkDerivedInvariants(ctx context.Context, source queryerRows) error {
	consolidations, err := storeListConsolidations(ctx, source)
	if err != nil {
		return err
	}
	consolidationByID := make(map[string]StoredConsolidation, len(consolidations))
	for _, stored := range consolidations {
		if stored.SchemaVersion != memory.ConsolidationSchemaVersion || stored.Status != memory.ConsolidationStatusCommitted {
			return fmt.Errorf("consolidation %s has an unsupported schema or status", stored.ID)
		}
		if stored.ID != memory.ConsolidationID(stored.Strategy, stored.WorkspaceHash, stored.InputsDigest) || memory.InputsDigest(stored.InputIDs) != stored.InputsDigest || len(stored.InputIDs) != stored.InputCount {
			return fmt.Errorf("consolidation %s does not match its declared inputs", stored.ID)
		}
		consolidationByID[stored.ID] = stored
	}
	derived, err := storeListDerived(ctx, source)
	if err != nil {
		return err
	}
	activeCounts := make(map[string]int)
	successorCounts := make(map[string]int)
	for _, candidate := range derived {
		if _, exists := consolidationByID[candidate.ConsolidationID]; !exists {
			return fmt.Errorf("derived candidate %s references a missing consolidation", candidate.ID)
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("validate derived candidate: %w", err)
		}
		for _, reference := range candidate.Evidence {
			sourceCandidate, found, err := lookupCandidate(ctx, source, reference.CandidateID)
			if err != nil || !found {
				return fmt.Errorf("derived candidate %s references missing source %s: %v", candidate.ID, reference.CandidateID, err)
			}
			if sourceCandidate.WorkspaceHash != candidate.WorkspaceHash {
				return fmt.Errorf("derived candidate %s evidence crosses workspaces", candidate.ID)
			}
		}
		if candidate.Status == memory.DerivedStatusActive {
			activeCounts[candidate.ID]++
		}
		audit, err := derivedAuditFor(ctx, source, candidate.ID, candidate.ConsolidationID)
		if err != nil {
			return err
		}
		consolidated, rejected, superseded := 0, 0, 0
		for _, entry := range audit {
			if entry.OccurredAt.Before(candidate.CreatedAt) {
				return fmt.Errorf("derived candidate %s has an invalid audit timeline", candidate.ID)
			}
			switch entry.Action {
			case memory.DerivedAuditConsolidated:
				consolidated++
			case memory.DerivedAuditRejected:
				if !memory.ValidRejectionReason(entry.Reason) {
					return fmt.Errorf("derived candidate %s has an invalid rejection audit", candidate.ID)
				}
				rejected++
			case memory.DerivedAuditSuperseded:
				superseded++
			default:
				return fmt.Errorf("derived candidate %s has an unknown audit action", candidate.ID)
			}
		}
		if consolidated != 1 {
			return fmt.Errorf("derived candidate %s must have exactly one consolidation audit", candidate.ID)
		}
		switch candidate.Status {
		case memory.DerivedStatusActive:
			if rejected != 0 || superseded != 0 {
				return fmt.Errorf("active derived candidate %s has terminal audit", candidate.ID)
			}
		case memory.DerivedStatusRejected:
			if rejected != 1 || superseded != 0 {
				return fmt.Errorf("rejected derived candidate %s has invalid audit state", candidate.ID)
			}
		case memory.DerivedStatusSuperseded:
			if rejected != 0 || superseded != 1 {
				return fmt.Errorf("superseded derived candidate %s has invalid audit state", candidate.ID)
			}
			successorCounts[candidate.ID]++
		}
	}
	for id, count := range activeCounts {
		if count > 1 {
			return fmt.Errorf("derived candidate %s has %d active versions", id, count)
		}
	}
	for _, candidate := range derived {
		if candidate.Status == memory.DerivedStatusSuperseded && successorCounts[candidate.ID] == 0 {
			return fmt.Errorf("superseded derived candidate %s has no successor version", candidate.ID)
		}
	}
	return checkDerivedFactsRecompute(ctx, source, consolidations)
}

// checkDerivedFactsRecompute 按每个巩固事务声明的输入候选重算确定性归纳，
// 并与已派生事实比对；存储的载荷一旦与源事实漂移即失败关闭。
func checkDerivedFactsRecompute(ctx context.Context, source queryerRows, consolidations []StoredConsolidation) error {
	for _, stored := range consolidations {
		inputs := memory.ConsolidationInputs{WorkspaceHash: stored.WorkspaceHash, Candidates: make([]memory.Candidate, 0, len(stored.InputIDs))}
		for _, candidateID := range stored.InputIDs {
			candidate, found, err := lookupCandidate(ctx, source, candidateID)
			if err == nil && !found {
				err = memory.ErrNotFound
			}
			if err != nil {
				return fmt.Errorf("consolidation %s input candidate %s is missing: %v", stored.ID, candidateID, err)
			}
			candidate.Evidence, err = evidenceFor(ctx, source, candidate.ID)
			if err != nil {
				return err
			}
			inputs.Candidates = append(inputs.Candidates, candidate)
		}
		anchor, err := earliestDerivedAnchor(ctx, source, stored.ID)
		if err != nil {
			return err
		}
		recomputed, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: anchor, TTL: stored.DerivedTTL, MinPatternSupport: memory.PatternMinSupport, MinHypothesisSupport: memory.HypothesisMinSupport})
		if err != nil {
			return fmt.Errorf("recompute consolidation %s: %w", stored.ID, err)
		}
		derived, err := derivedForConsolidation(ctx, source, stored.ID)
		if err != nil {
			return err
		}
		expected := make([]memory.DerivedCandidate, 0)
		for _, plan := range recomputed {
			if plan.Strategy == stored.Strategy {
				expected = append(expected, plan.Derived...)
			}
		}
		if len(expected) != len(derived) {
			return fmt.Errorf("consolidation %s derived %d candidates but facts recompute to %d", stored.ID, len(derived), len(expected))
		}
		expectedByID := make(map[string]memory.DerivedCandidate, len(expected))
		for _, candidate := range expected {
			expectedByID[candidate.ID] = candidate
		}
		for _, candidate := range derived {
			expectedCandidate, exists := expectedByID[candidate.ID]
			if !exists {
				return fmt.Errorf("consolidation %s derived candidate %s is not reproducible from inputs", stored.ID, candidate.ID)
			}
			if expectedCandidate.Kind != candidate.Kind || expectedCandidate.ScopeDigest != candidate.ScopeDigest ||
				expectedCandidate.WorkspaceHash != candidate.WorkspaceHash || string(expectedCandidate.Payload) != string(candidate.Payload) ||
				!expectedCandidate.CreatedAt.Equal(candidate.CreatedAt) || !expectedCandidate.ExpiresAt.Equal(candidate.ExpiresAt) ||
				!reflect.DeepEqual(expectedCandidate.Evidence, candidate.Evidence) {
				return fmt.Errorf("consolidation %s derived candidate %s does not match recomputed facts", stored.ID, candidate.ID)
			}
		}
	}
	return nil
}

// earliestDerivedAnchor 返回一次巩固事务派生行的创建锚点；计划级保留期校验
// 保证同一事务内锚点唯一，因此任取一行即为该锚点，缺失派生行返回错误。
func earliestDerivedAnchor(ctx context.Context, source queryerRows, consolidationID string) (time.Time, error) {
	derived, err := derivedForConsolidation(ctx, source, consolidationID)
	if err != nil {
		return time.Time{}, err
	}
	if len(derived) == 0 {
		return time.Time{}, fmt.Errorf("consolidation %s has no derived rows", consolidationID)
	}
	return derived[0].CreatedAt, nil
}

// derivedAuditFor 返回派生候选版本行的审计历史（含 reason）。
func derivedAuditFor(ctx context.Context, source queryerRows, derivedID, consolidationID string) ([]DerivedAuditEntry, error) {
	rows, err := source.QueryContext(ctx, `SELECT action, reason, occurred_at FROM derived_audit_log
		WHERE derived_id = ? AND consolidation_id = ? ORDER BY id ASC`, derivedID, consolidationID)
	if err != nil {
		return nil, fmt.Errorf("query derived audit: %w", err)
	}
	defer rows.Close()
	var entries []DerivedAuditEntry
	for rows.Next() {
		var entry DerivedAuditEntry
		var reason sql.NullString
		var occurredAt string
		if err := rows.Scan(&entry.Action, &reason, &occurredAt); err != nil {
			return nil, fmt.Errorf("scan derived audit: %w", err)
		}
		if reason.Valid {
			entry.Reason = reason.String
		}
		entry.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse derived audit time: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate derived audit: %w", err)
	}
	return entries, nil
}

// storeListConsolidations 在事务或只读连接上列出全部巩固事务行。
func storeListConsolidations(ctx context.Context, source queryerRows) ([]StoredConsolidation, error) {
	rows, err := source.QueryContext(ctx, consolidationSelectBase+" ORDER BY id ASC")
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

// storeListDerived 在事务或只读连接上列出全部派生候选版本行。
func storeListDerived(ctx context.Context, source queryerRows) ([]memory.DerivedCandidate, error) {
	rows, err := source.QueryContext(ctx, derivedSelectBase+" ORDER BY id ASC, consolidation_id ASC")
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
		candidate.Evidence, err = derivedEvidenceFor(ctx, source, candidate.ID, candidate.ConsolidationID)
		if err != nil {
			return nil, err
		}
	}
	return derived, nil
}

// DerivedAuditEntry 是派生候选版本的一行审计投影；字段与 derived_audit_log 一致。
type DerivedAuditEntry struct {
	Action     string
	Reason     string
	OccurredAt time.Time
}

// DerivedInspectResult 是派生候选的只读解释投影：最新版本行及其审计历史。
type DerivedInspectResult struct {
	Candidate memory.DerivedCandidate
	Audit     []DerivedAuditEntry
}

// InspectDerived 返回派生候选最新版本的候选与审计历史；不存在时返回 ErrNotFound。
// 读取走只读连接或可写连接，均不改变任何状态。
func (store *Store) InspectDerived(ctx context.Context, derivedID string) (DerivedInspectResult, error) {
	version, found, err := latestDerivedVersion(ctx, store.db, derivedID)
	if err != nil {
		return DerivedInspectResult{}, err
	}
	if !found {
		return DerivedInspectResult{}, memory.ErrNotFound
	}
	version.Evidence, err = derivedEvidenceFor(ctx, store.db, version.ID, version.ConsolidationID)
	if err != nil {
		return DerivedInspectResult{}, err
	}
	if err := version.Validate(); err != nil {
		return DerivedInspectResult{}, fmt.Errorf("validate derived candidate: %w", err)
	}
	audit, err := derivedAuditFor(ctx, store.db, version.ID, version.ConsolidationID)
	if err != nil {
		return DerivedInspectResult{}, err
	}
	entries := make([]DerivedAuditEntry, 0, len(audit))
	for _, entry := range audit {
		entries = append(entries, DerivedAuditEntry{Action: entry.Action, Reason: entry.Reason, OccurredAt: entry.OccurredAt})
	}
	return DerivedInspectResult{Candidate: version, Audit: entries}, nil
}
