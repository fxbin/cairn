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
	"sort"
	"time"

	"github.com/fxbin/cairn/internal/memory"
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

// validatePlannedConsolidation 校验计划自洽：写集摘要与输入一致、冻结支持度
// 满足合同下限、派生数量受上限约束；每条派生候选都必须属于本计划并通过合同
// 校验。源候选的存储态与可重算事实由同一 SQLite 事务中的后续校验负责。
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
	if !sortedUnique(plan.InputIDs) {
		return errors.New("consolidation input ids must be sorted and unique")
	}
	if plan.FactAnchor.IsZero() {
		return errors.New("consolidation requires a derived fact anchor")
	}
	if plan.MinPatternSupport < memory.PatternMinSupport || plan.MinHypothesisSupport < memory.HypothesisMinSupport {
		return errors.New("consolidation frozen support is below the minimum")
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
		if !derived.CreatedAt.Equal(plan.FactAnchor) || !derived.CreatedAt.Equal(first.CreatedAt) || derived.ExpiresAt.Sub(derived.CreatedAt) != first.ExpiresAt.Sub(first.CreatedAt) {
			return fmt.Errorf("derived candidate %d retention anchor differs within one consolidation", index)
		}
	}
	return nil
}

// validatePlanAgainstStoredInputs 在当前 SQLite 事务内从事实源候选表重新加载
// 计划的所有输入，验证它们仍属于同一工作区、active 且未过期，并以计划冻结
// 的 TTL 与最小支持度重算全部派生事实后作字节级比对。
func validatePlanAgainstStoredInputs(ctx context.Context, transaction *sql.Tx, plan memory.PlannedConsolidation, now time.Time) error {
	if err := validatePlannedConsolidation(plan); err != nil {
		return err
	}
	inputs := memory.ConsolidationInputs{
		WorkspaceHash: plan.WorkspaceHash,
		Candidates:    make([]memory.Candidate, 0, len(plan.InputIDs)),
	}
	for _, candidateID := range plan.InputIDs {
		candidate, found, err := lookupCandidate(ctx, transaction, candidateID)
		if err != nil {
			return fmt.Errorf("load input candidate %s: %w", candidateID, err)
		}
		if !found {
			return fmt.Errorf("input candidate %s: %w", candidateID, memory.ErrNotFound)
		}
		candidate.Evidence, err = evidenceFor(ctx, transaction, candidate.ID)
		if err != nil {
			return fmt.Errorf("load input candidate %s evidence: %w", candidateID, err)
		}
		if err := candidate.Validate(); err != nil {
			return fmt.Errorf("validate input candidate %s: %w", candidateID, err)
		}
		if candidate.WorkspaceHash != plan.WorkspaceHash {
			return fmt.Errorf("input candidate %s belongs to a different workspace", candidateID)
		}
		if candidate.Status != memory.StatusActive {
			return fmt.Errorf("input candidate %s is not active", candidateID)
		}
		if !candidate.ExpiresAt.After(now) {
			return fmt.Errorf("input candidate %s is expired", candidateID)
		}
		inputs.Candidates = append(inputs.Candidates, candidate)
	}
	for _, derived := range plan.Derived {
		if err := validateDerivedEvidenceSubset(derived, plan.InputIDs); err != nil {
			return err
		}
	}
	recomputed, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{
		Now:                  now,
		TTL:                  plan.Derived[0].ExpiresAt.Sub(plan.Derived[0].CreatedAt),
		MinPatternSupport:    plan.MinPatternSupport,
		MinHypothesisSupport: plan.MinHypothesisSupport,
	})
	if err != nil {
		return fmt.Errorf("recompute consolidation facts: %w", err)
	}
	for _, candidate := range recomputed {
		if candidate.Strategy == plan.Strategy {
			if !plannedFactsEquivalent(candidate, plan) {
				return errors.New("planned consolidation does not match recomputed facts")
			}
			return nil
		}
	}
	return errors.New("planned consolidation strategy is not reproducible from inputs")
}

// validateDerivedEvidenceSubset 要求派生候选的每个间接证据都声明在计划输入
// 集内，避免计划以未重算的外部候选伪造支持度。
func validateDerivedEvidenceSubset(derived memory.DerivedCandidate, inputIDs []string) error {
	allowed := make(map[string]struct{}, len(inputIDs))
	for _, candidateID := range inputIDs {
		allowed[candidateID] = struct{}{}
	}
	for _, reference := range derived.Evidence {
		if _, exists := allowed[reference.CandidateID]; !exists {
			return fmt.Errorf("derived candidate %s evidence is outside plan inputs", derived.ID)
		}
	}
	return nil
}

// plannedFactsEquivalent 比较计划的所有冻结事实；实际提交时间不属于计划，
// 因为它必须由写入事务的真实 UTC 时钟生成并持久化。
func plannedFactsEquivalent(left, right memory.PlannedConsolidation) bool {
	return left.ConsolidationID == right.ConsolidationID && left.SchemaVersion == right.SchemaVersion &&
		left.Strategy == right.Strategy && left.WorkspaceHash == right.WorkspaceHash &&
		left.InputsDigest == right.InputsDigest && reflect.DeepEqual(left.InputIDs, right.InputIDs) &&
		left.FactAnchor.Equal(right.FactAnchor) && left.MinPatternSupport == right.MinPatternSupport &&
		left.MinHypothesisSupport == right.MinHypothesisSupport && reflect.DeepEqual(left.Derived, right.Derived)
}

// sortedUnique 判断输入标识是否按稳定字典序有序且没有重复，避免摘要的集合语义
// 被重复 ID 掩盖而绕过同事务逐项加载。
func sortedUnique(values []string) bool {
	if len(values) == 0 {
		return false
	}
	return sort.SliceIsSorted(values, func(left, right int) bool {
		return values[left] < values[right]
	}) && !hasDuplicate(values)
}

// hasDuplicate 在已经排序的输入标识中检查相邻重复项。
func hasDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index-1] == values[index] {
			return true
		}
	}
	return false
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
	if err := scanner.Scan(&stored.ID, &stored.SchemaVersion, &stored.Strategy, &stored.WorkspaceHash, &stored.InputsDigest, &inputIDs, &stored.InputCount, &committedAt, &ttlSeconds, &stored.MinPatternSupport, &stored.MinHypothesisSupport, &stored.Status); err != nil {
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
	if supersedesConsolidation.Valid {
		candidate.SupersedesConsolidationID = supersedesConsolidation.String
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

// activeDerivedVersion 返回指定派生候选当前可操作的唯一版本行；除数据库状态为
// active 外，派生自身和所有来源候选也必须未过期且仍为 active，避免把历史到期
// 或显式失效的事实用于拒绝等写入操作。
func activeDerivedVersion(ctx context.Context, source queryerRows, derivedID string, now time.Time) (memory.DerivedCandidate, bool, error) {
	now = now.UTC()
	rows, err := source.QueryContext(ctx, derivedSelectBase+" WHERE id = ? AND status = ? AND expires_at > ?", derivedID, memory.DerivedStatusActive, now.Format(time.RFC3339Nano))
	if err != nil {
		return memory.DerivedCandidate{}, false, fmt.Errorf("query active derived candidate: %w", err)
	}
	var found *memory.DerivedCandidate
	for rows.Next() {
		candidate, err := scanDerived(rows)
		if err != nil {
			rows.Close()
			return memory.DerivedCandidate{}, false, err
		}
		if found != nil {
			rows.Close()
			return memory.DerivedCandidate{}, false, errors.New("derived candidate has multiple active versions")
		}
		found = &candidate
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return memory.DerivedCandidate{}, false, fmt.Errorf("iterate active derived candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return memory.DerivedCandidate{}, false, fmt.Errorf("close active derived candidates: %w", err)
	}
	if found == nil {
		return memory.DerivedCandidate{}, false, nil
	}
	found.Evidence, err = derivedEvidenceFor(ctx, source, found.ID, found.ConsolidationID)
	if err != nil {
		return memory.DerivedCandidate{}, false, err
	}
	current, err := derivedCurrent(ctx, source, *found, now)
	if err != nil {
		return memory.DerivedCandidate{}, false, err
	}
	if !current {
		return memory.DerivedCandidate{}, false, nil
	}
	return *found, true, nil
}

// latestDerivedVersion 返回派生候选的最新版本行；版本先后以 SQLite 解析后的真实
// committed_at 为第一顺序，避免变长 RFC3339Nano 文本的字节序歧义，并以同一事务
// 写入的 consolidated 审计自增标识作稳定次序，绝不以 consolidation ID 哈希决定
// 版本新旧。不存在时 found 为 false。
func latestDerivedVersion(ctx context.Context, source queryer, derivedID string) (memory.DerivedCandidate, bool, error) {
	candidate, err := scanDerived(source.QueryRowContext(ctx, derivedSelectBase+` WHERE id = ?
		ORDER BY
			julianday((SELECT committed_at FROM consolidations WHERE consolidations.id = derived_candidates.consolidation_id)) DESC,
			(SELECT id FROM derived_audit_log
				WHERE derived_audit_log.derived_id = derived_candidates.id
					AND derived_audit_log.consolidation_id = derived_candidates.consolidation_id
					AND derived_audit_log.action = ?) DESC
		LIMIT 1`, derivedID, memory.DerivedAuditConsolidated))
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
	if existing.Strategy != plan.Strategy || existing.WorkspaceHash != plan.WorkspaceHash || existing.InputsDigest != plan.InputsDigest || existing.InputCount != len(plan.InputIDs) ||
		existing.DerivedTTL != plan.Derived[0].ExpiresAt.Sub(plan.Derived[0].CreatedAt) || existing.MinPatternSupport != plan.MinPatternSupport || existing.MinHypothesisSupport != plan.MinHypothesisSupport {
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
func readBackPlan(ctx context.Context, transaction *sql.Tx, plan memory.PlannedConsolidation, committedAt time.Time) (bool, error) {
	stored, found, err := lookupConsolidation(ctx, transaction, plan.ConsolidationID)
	if err != nil || !found {
		return false, err
	}
	if stored.Strategy != plan.Strategy || stored.WorkspaceHash != plan.WorkspaceHash || stored.InputsDigest != plan.InputsDigest ||
		stored.InputCount != len(plan.InputIDs) || !reflect.DeepEqual(stored.InputIDs, plan.InputIDs) || !stored.CommittedAt.Equal(committedAt) ||
		stored.Status != memory.ConsolidationStatusCommitted || stored.SchemaVersion != plan.SchemaVersion ||
		stored.DerivedTTL != plan.Derived[0].ExpiresAt.Sub(plan.Derived[0].CreatedAt) ||
		stored.MinPatternSupport != plan.MinPatternSupport || stored.MinHypothesisSupport != plan.MinHypothesisSupport {
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
		audit, err := derivedAuditFor(ctx, transaction, stored.ID, stored.ConsolidationID)
		if err != nil {
			return false, err
		}
		if len(audit) != 1 || audit[0].Action != memory.DerivedAuditConsolidated || audit[0].Reason != "" || !audit[0].OccurredAt.Equal(committedAt) {
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
		if !sortedUnique(stored.InputIDs) || stored.MinPatternSupport < memory.PatternMinSupport || stored.MinHypothesisSupport < memory.HypothesisMinSupport {
			return fmt.Errorf("consolidation %s has invalid frozen parameters", stored.ID)
		}
		consolidationByID[stored.ID] = stored
	}
	derived, err := storeListDerived(ctx, source)
	if err != nil {
		return err
	}
	versionByKey := make(map[DerivedVersionKey]memory.DerivedCandidate, len(derived))
	for _, candidate := range derived {
		key := DerivedVersionKey{ID: candidate.ID, ConsolidationID: candidate.ConsolidationID}
		if _, exists := versionByKey[key]; exists {
			return fmt.Errorf("derived candidate version %s/%s is duplicated", key.ID, key.ConsolidationID)
		}
		versionByKey[key] = candidate
	}
	activeCounts := make(map[string]int)
	successorCounts := make(map[DerivedVersionKey]int)
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
		if candidate.SupersedesID != "" {
			predecessorKey := DerivedVersionKey{ID: candidate.SupersedesID, ConsolidationID: candidate.SupersedesConsolidationID}
			predecessor, found := versionByKey[predecessorKey]
			if !found || (predecessorKey.ID == candidate.ID && predecessorKey.ConsolidationID == candidate.ConsolidationID) ||
				predecessor.Status != memory.DerivedStatusSuperseded || predecessor.WorkspaceHash != candidate.WorkspaceHash ||
				predecessor.Kind != candidate.Kind || predecessor.ScopeDigest != candidate.ScopeDigest {
				return fmt.Errorf("derived candidate %s has an invalid predecessor version key", candidate.ID)
			}
			successorCounts[predecessorKey]++
		}
		audit, err := derivedAuditFor(ctx, source, candidate.ID, candidate.ConsolidationID)
		if err != nil {
			return err
		}
		consolidated, rejected, superseded := 0, 0, 0
		stored := consolidationByID[candidate.ConsolidationID]
		for _, entry := range audit {
			if entry.OccurredAt.Before(candidate.CreatedAt) {
				return fmt.Errorf("derived candidate %s has an invalid audit timeline", candidate.ID)
			}
			switch entry.Action {
			case memory.DerivedAuditConsolidated:
				if !entry.OccurredAt.Equal(stored.CommittedAt) || entry.Reason != "" {
					return fmt.Errorf("derived candidate %s has an invalid consolidation audit", candidate.ID)
				}
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
		}
	}
	for id, count := range activeCounts {
		if count > 1 {
			return fmt.Errorf("derived candidate %s has %d active versions", id, count)
		}
	}
	for _, candidate := range derived {
		if candidate.Status == memory.DerivedStatusSuperseded && successorCounts[DerivedVersionKey{ID: candidate.ID, ConsolidationID: candidate.ConsolidationID}] != 1 {
			return fmt.Errorf("superseded derived candidate %s must have exactly one successor version", candidate.ID)
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
		recomputed, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: stored.CommittedAt, TTL: stored.DerivedTTL, MinPatternSupport: stored.MinPatternSupport, MinHypothesisSupport: stored.MinHypothesisSupport})
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
	Current   bool
	Eligible  bool
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
	current, err := derivedCurrent(ctx, store.db, version, store.currentUTC())
	if err != nil {
		return DerivedInspectResult{}, err
	}
	audit, err := derivedAuditFor(ctx, store.db, version.ID, version.ConsolidationID)
	if err != nil {
		return DerivedInspectResult{}, err
	}
	entries := make([]DerivedAuditEntry, 0, len(audit))
	for _, entry := range audit {
		entries = append(entries, DerivedAuditEntry{Action: entry.Action, Reason: entry.Reason, OccurredAt: entry.OccurredAt})
	}
	return DerivedInspectResult{Candidate: version, Audit: entries, Current: current, Eligible: current}, nil
}

// derivedCurrent 判断派生版本在给定 UTC 时刻能否作为当前事实：版本自身及其每个
// 来源候选都必须为 active 且尚未过期。自然到期返回 false 而不是把历史记录当作
// 损坏；缺失或跨工作区来源仍按完整性错误失败关闭。
func derivedCurrent(ctx context.Context, source queryerRows, candidate memory.DerivedCandidate, now time.Time) (bool, error) {
	if candidate.Status != memory.DerivedStatusActive || !candidate.ExpiresAt.After(now) {
		return false, nil
	}
	for _, reference := range candidate.Evidence {
		sourceCandidate, found, err := lookupCandidate(ctx, source, reference.CandidateID)
		if err != nil {
			return false, err
		}
		if !found {
			return false, fmt.Errorf("derived candidate %s references a missing source %s", candidate.ID, reference.CandidateID)
		}
		if sourceCandidate.WorkspaceHash != candidate.WorkspaceHash {
			return false, fmt.Errorf("derived candidate %s evidence crosses workspaces", candidate.ID)
		}
		if sourceCandidate.Status != memory.StatusActive || !sourceCandidate.ExpiresAt.After(now) {
			return false, nil
		}
	}
	return true, nil
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
