// [INPUT]: 依赖本包查询端口与派生版本键。
// [OUTPUT]: 提供派生读取、资格、历史解释与读回。
// [POS]: 读回校验不创造新的来源事实。
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
	"reflect"

	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

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
