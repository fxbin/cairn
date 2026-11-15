// [INPUT]: 依赖候选合同与本包连接。
// [OUTPUT]: 提供候选捕获、召回、撤销与审计校验。
// [POS]: 候选与来源关系在同一事务内闭合。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 提供能力内的SQLite持久化叶子。
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"reflect"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

// Capture 原子写入候选、证据和审计行；同一 workspace/run 的相同重试返回原候选，
// 不同输入失败关闭，替代关系与旧候选状态在同一事务中提交。
func (store *Store) Capture(ctx context.Context, candidate memory.Candidate) (memory.Candidate, error) {
	if err := candidate.Validate(); err != nil {
		return memory.Candidate{}, err
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Candidate{}, fmt.Errorf("start memory capture: %w", err)
	}
	defer transaction.Rollback()
	existing, found, err := lookupCandidate(ctx, transaction, candidate.ID)
	if err != nil {
		return memory.Candidate{}, err
	}
	if found {
		existing.Evidence, err = evidenceFor(ctx, transaction, existing.ID)
		if err != nil {
			return memory.Candidate{}, err
		}
		if captureEquivalent(existing, candidate) {
			if err := transaction.Commit(); err != nil {
				return memory.Candidate{}, fmt.Errorf("commit idempotent memory capture: %w", err)
			}
			return existing, nil
		}
		return memory.Candidate{}, fmt.Errorf("%w: run %s already captured with different facts", memory.ErrConflict, candidate.RunID)
	}
	if candidate.SupersedesID != "" {
		superseded, found, err := lookupCandidate(ctx, transaction, candidate.SupersedesID)
		if err != nil {
			return memory.Candidate{}, err
		}
		if !found || superseded.WorkspaceHash != candidate.WorkspaceHash || superseded.Status != memory.StatusActive || !superseded.ExpiresAt.After(store.currentUTC()) {
			return memory.Candidate{}, errors.New("memory supersede target must be a current candidate in the same workspace")
		}
		supersededAt := store.currentUTC()
		if supersededAt.Before(superseded.CreatedAt) {
			supersededAt = superseded.CreatedAt
		}
		if err := rejectDependentDerived(ctx, transaction, superseded.ID, supersededAt); err != nil {
			return memory.Candidate{}, err
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE candidates SET status = ? WHERE id = ?", memory.StatusSuperseded, superseded.ID); err != nil {
			return memory.Candidate{}, fmt.Errorf("supersede memory candidate: %w", err)
		}
		if err := insertAudit(ctx, transaction, superseded.ID, memory.AuditSuperseded, "", supersededAt); err != nil {
			return memory.Candidate{}, err
		}
	}
	if _, err := transaction.ExecContext(ctx, `INSERT INTO candidates(
		id, schema_version, kind, workspace_hash, task_prompt_digest, run_id, outcome,
		failure_class, trusted_verification, created_at, expires_at, status, supersedes_id
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		candidate.ID, candidate.SchemaVersion, candidate.Kind, candidate.WorkspaceHash, candidate.TaskPromptDigest,
		candidate.RunID, candidate.Outcome, nullableString(candidate.FailureClass), candidate.TrustedVerification,
		candidate.CreatedAt.UTC().Format(time.RFC3339Nano), candidate.ExpiresAt.UTC().Format(time.RFC3339Nano),
		candidate.Status, nullableString(candidate.SupersedesID)); err != nil {
		return memory.Candidate{}, fmt.Errorf("insert memory candidate: %w", err)
	}
	for ordinal, reference := range candidate.Evidence {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO evidence_refs(
			candidate_id, ordinal, run_id, event_seq, event_hash, event_type, artifact_sha256
		) VALUES (?, ?, ?, ?, ?, ?, ?)`, candidate.ID, ordinal, reference.RunID, reference.EventSeq,
			reference.EventHash, reference.EventType, nullableString(reference.ArtifactSHA256)); err != nil {
			return memory.Candidate{}, fmt.Errorf("insert memory evidence: %w", err)
		}
	}
	if err := insertAudit(ctx, transaction, candidate.ID, memory.AuditCaptured, "", candidate.CreatedAt); err != nil {
		return memory.Candidate{}, err
	}
	if err := transaction.Commit(); err != nil {
		return memory.Candidate{}, fmt.Errorf("commit memory capture: %w", err)
	}
	return candidate, nil
}

// Recall 返回当前 workspace 中 active、未过期且固定 run_outcome kind 的候选；
// 排序固定为 created_at 倒序、ID 正序，不会执行语义检索或写入状态。
func (store *Store) Recall(ctx context.Context, options memory.RecallOptions) ([]memory.Candidate, error) {
	precision := options.Precision
	if precision == "" {
		precision = memory.RecallPrecisionTrace
	}
	normalized, err := (memory.RecallQuery{
		WorkspaceHash:       options.WorkspaceHash,
		TaskPromptDigest:    options.TaskPromptDigest,
		Outcomes:            options.Outcomes,
		FailureClasses:      options.FailureClasses,
		TrustedVerification: options.TrustedVerification,
		CreatedFrom:         options.CreatedFrom,
		CreatedUntil:        options.CreatedUntil,
		Precision:           precision,
		Limit:               options.Limit,
	}).Normalize()
	if err != nil {
		return nil, err
	}
	now := options.Now.UTC()
	if now.IsZero() {
		now = store.currentUTC()
	}
	query := candidateSelect + ` WHERE workspace_hash = ? AND kind = ? AND status = ? AND expires_at > ?`
	arguments := []any{normalized.WorkspaceHash, memory.KindRunOutcome, memory.StatusActive, now.Format(time.RFC3339Nano)}
	if normalized.TaskPromptDigest != "" {
		query += " AND task_prompt_digest = ?"
		arguments = append(arguments, normalized.TaskPromptDigest)
	}
	if len(normalized.Outcomes) != 0 {
		query += " AND outcome IN (" + placeholders(len(normalized.Outcomes)) + ")"
		arguments = append(arguments, stringsToAny(normalized.Outcomes)...)
	}
	if len(normalized.FailureClasses) != 0 {
		query += " AND failure_class IN (" + placeholders(len(normalized.FailureClasses)) + ")"
		arguments = append(arguments, stringsToAny(normalized.FailureClasses)...)
	}
	switch normalized.TrustedVerification {
	case memory.TrustedVerificationRequired:
		query += " AND trusted_verification = 1"
	case memory.TrustedVerificationNotRequired:
		query += " AND trusted_verification = 0"
	}
	if normalized.CreatedFrom != nil {
		query += " AND created_at >= ? AND created_at < ?"
		arguments = append(arguments, normalized.CreatedFrom.Format(time.RFC3339Nano), normalized.CreatedUntil.Format(time.RFC3339Nano))
	}
	query += " ORDER BY created_at DESC, id ASC LIMIT ?"
	arguments = append(arguments, normalized.Limit)
	rows, err := store.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, fmt.Errorf("query memory recall: %w", err)
	}
	var candidates []memory.Candidate
	for rows.Next() {
		candidate, err := scanCandidate(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate memory recall: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close memory recall rows: %w", err)
	}
	for index := range candidates {
		candidate := &candidates[index]
		candidate.Evidence, err = evidenceFor(ctx, store.db, candidate.ID)
		if err != nil {
			return nil, err
		}
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("validate recalled memory candidate: %w", err)
		}
	}
	return candidates, nil
}

// placeholders 返回固定数量的 SQLite 参数占位符；调用方只对已规范化的封闭字段使用它。
func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

// stringsToAny 将已验证的受限 token 转为 database/sql 参数，避免拼接业务值到 SQL。
func stringsToAny(values []string) []any {
	arguments := make([]any, 0, len(values))
	for _, value := range values {
		arguments = append(arguments, value)
	}
	return arguments
}

// Inspect 返回候选及其审计历史；它不按状态过滤，便于解释撤销与替代原因。
func (store *Store) Inspect(ctx context.Context, candidateID string) (memory.InspectResult, error) {
	candidate, found, err := lookupCandidate(ctx, store.db, candidateID)
	if err != nil {
		return memory.InspectResult{}, err
	}
	if !found {
		return memory.InspectResult{}, memory.ErrNotFound
	}
	candidate.Evidence, err = evidenceFor(ctx, store.db, candidateID)
	if err != nil {
		return memory.InspectResult{}, err
	}
	if err := candidate.Validate(); err != nil {
		return memory.InspectResult{}, fmt.Errorf("validate memory candidate: %w", err)
	}
	audit, err := auditFor(ctx, store.db, candidateID)
	if err != nil {
		return memory.InspectResult{}, err
	}
	return memory.InspectResult{Candidate: candidate, Audit: audit}, nil
}

// Revoke 原子更新 active 候选为 revoked，并以封闭理由写入独立审计行；重复撤销
// 返回现有 revoked 候选而不制造额外审计记录。
func (store *Store) Revoke(ctx context.Context, candidateID, reason string, occurredAt time.Time) (memory.Candidate, error) {
	if !memory.ValidRevocationReason(reason) {
		return memory.Candidate{}, errors.New("memory revoke reason is invalid")
	}
	if occurredAt.IsZero() {
		occurredAt = store.currentUTC()
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return memory.Candidate{}, fmt.Errorf("start memory revoke: %w", err)
	}
	defer transaction.Rollback()
	candidate, found, err := lookupCandidate(ctx, transaction, candidateID)
	if err != nil {
		return memory.Candidate{}, err
	}
	if !found {
		return memory.Candidate{}, memory.ErrNotFound
	}
	if candidate.Status == memory.StatusRevoked {
		candidate.Evidence, err = evidenceFor(ctx, transaction, candidate.ID)
		if err != nil {
			return memory.Candidate{}, err
		}
		if err := transaction.Commit(); err != nil {
			return memory.Candidate{}, fmt.Errorf("commit idempotent memory revoke: %w", err)
		}
		return candidate, nil
	}
	if candidate.Status != memory.StatusActive {
		return memory.Candidate{}, errors.New("only active memory candidates can be revoked")
	}
	if !candidate.ExpiresAt.After(store.currentUTC()) {
		return memory.Candidate{}, errors.New("only current memory candidates can be revoked")
	}
	if occurredAt.Before(candidate.CreatedAt) {
		occurredAt = candidate.CreatedAt
	}
	if err := rejectDependentDerived(ctx, transaction, candidateID, occurredAt); err != nil {
		return memory.Candidate{}, err
	}
	if _, err := transaction.ExecContext(ctx, "UPDATE candidates SET status = ? WHERE id = ?", memory.StatusRevoked, candidateID); err != nil {
		return memory.Candidate{}, fmt.Errorf("revoke memory candidate: %w", err)
	}
	if err := insertAudit(ctx, transaction, candidateID, memory.AuditRevoked, reason, occurredAt); err != nil {
		return memory.Candidate{}, err
	}
	candidate.Status = memory.StatusRevoked
	candidate.Evidence, err = evidenceFor(ctx, transaction, candidate.ID)
	if err != nil {
		return memory.Candidate{}, err
	}
	if err := transaction.Commit(); err != nil {
		return memory.Candidate{}, fmt.Errorf("commit memory revoke: %w", err)
	}
	return candidate, nil
}

// ListAll 返回当前数据库全部候选及其证据，供 app.Doctor 跨库验证；没有状态或
// 过期过滤，避免诊断遗漏已经撤销或过期但仍应可审计的事实。
func (store *Store) ListAll(ctx context.Context) ([]memory.Candidate, error) {
	rows, err := store.db.QueryContext(ctx, candidateSelect+" ORDER BY created_at ASC, id ASC")
	if err != nil {
		return nil, fmt.Errorf("list memory candidates: %w", err)
	}
	var candidates []memory.Candidate
	for rows.Next() {
		candidate, err := scanCandidate(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, candidate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, fmt.Errorf("iterate memory candidates: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close memory candidate rows: %w", err)
	}
	for index := range candidates {
		candidate := &candidates[index]
		candidate.Evidence, err = evidenceFor(ctx, store.db, candidate.ID)
		if err != nil {
			return nil, err
		}
		if err := candidate.Validate(); err != nil {
			return nil, fmt.Errorf("validate memory candidate: %w", err)
		}
	}
	return candidates, nil
}

// Check 执行 SQLite 完整性检查、迁移校验和与内部候选结构校验；跨 Journal 与
// Artifact 的外部证据验证由 app.Doctor 在来源 owner 边界完成。
func (store *Store) Check(ctx context.Context) error {
	var quickCheck string
	if err := store.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&quickCheck); err != nil {
		return fmt.Errorf("memory SQLite quick_check: %w", err)
	}
	if quickCheck != "ok" {
		return fmt.Errorf("memory SQLite quick_check: %s", quickCheck)
	}
	if err := checkMigration(ctx, store.db); err != nil {
		return err
	}
	candidates, err := store.ListAll(ctx)
	if err != nil {
		return err
	}
	if err := checkAuditInvariants(ctx, store.db, candidates); err != nil {
		return err
	}
	if err := checkDerivedInvariants(ctx, store.db); err != nil {
		return err
	}
	if err := checkEvolutionInvariants(ctx, store.db, store.currentUTC()); err != nil {
		return err
	}
	return checkPromotionInvariants(ctx, store.db)
}

// queryer 约束 DB 与事务共同需要的只读单行查询能力。
type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// queryerRows 约束 DB 与事务共同需要的多行查询能力。
type queryerRows interface {
	queryer
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

const candidateSelect = `SELECT id, schema_version, kind, workspace_hash, task_prompt_digest, run_id, outcome,
	failure_class, trusted_verification, created_at, expires_at, status, supersedes_id FROM candidates`

// lookupCandidate 查找一条候选；不存在不是错误，供 capture/revoke 明确决定语义。
func lookupCandidate(ctx context.Context, source queryer, candidateID string) (memory.Candidate, bool, error) {
	candidate, err := scanCandidate(source.QueryRowContext(ctx, candidateSelect+" WHERE id = ?", candidateID))
	if errors.Is(err, sql.ErrNoRows) {
		return memory.Candidate{}, false, nil
	}
	if err != nil {
		return memory.Candidate{}, false, err
	}
	return candidate, true, nil
}

// scanCandidate 从固定 SQL 行还原候选，并解析严格 UTC 时间戳。
func scanCandidate(scanner interface{ Scan(...any) error }) (memory.Candidate, error) {
	var candidate memory.Candidate
	var failureClass, supersedesID sql.NullString
	var createdAt, expiresAt string
	if err := scanner.Scan(&candidate.ID, &candidate.SchemaVersion, &candidate.Kind, &candidate.WorkspaceHash, &candidate.TaskPromptDigest, &candidate.RunID, &candidate.Outcome, &failureClass, &candidate.TrustedVerification, &createdAt, &expiresAt, &candidate.Status, &supersedesID); err != nil {
		return memory.Candidate{}, err
	}
	var err error
	candidate.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return memory.Candidate{}, fmt.Errorf("parse memory created_at: %w", err)
	}
	candidate.ExpiresAt, err = time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		return memory.Candidate{}, fmt.Errorf("parse memory expires_at: %w", err)
	}
	if failureClass.Valid {
		candidate.FailureClass = failureClass.String
	}
	if supersedesID.Valid {
		candidate.SupersedesID = supersedesID.String
	}
	return candidate, nil
}

// evidenceFor 返回一条候选的固定序证据引用。
func evidenceFor(ctx context.Context, source queryerRows, candidateID string) ([]memory.EventEvidenceRef, error) {
	rows, err := source.QueryContext(ctx, `SELECT run_id, event_seq, event_hash, event_type, artifact_sha256
		FROM evidence_refs WHERE candidate_id = ? ORDER BY ordinal ASC`, candidateID)
	if err != nil {
		return nil, fmt.Errorf("query memory evidence: %w", err)
	}
	defer rows.Close()
	var references []memory.EventEvidenceRef
	for rows.Next() {
		var reference memory.EventEvidenceRef
		var artifactSHA256 sql.NullString
		if err := rows.Scan(&reference.RunID, &reference.EventSeq, &reference.EventHash, &reference.EventType, &artifactSHA256); err != nil {
			return nil, fmt.Errorf("scan memory evidence: %w", err)
		}
		if artifactSHA256.Valid {
			reference.ArtifactSHA256 = artifactSHA256.String
		}
		references = append(references, reference)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory evidence: %w", err)
	}
	return references, nil
}

// auditFor 返回候选审计历史的稳定插入序；不携带任何自由文本。
func auditFor(ctx context.Context, source queryerRows, candidateID string) ([]memory.AuditEntry, error) {
	rows, err := source.QueryContext(ctx, "SELECT candidate_id, action, reason, occurred_at FROM audit_log WHERE candidate_id = ? ORDER BY id ASC", candidateID)
	if err != nil {
		return nil, fmt.Errorf("query memory audit: %w", err)
	}
	defer rows.Close()
	var entries []memory.AuditEntry
	for rows.Next() {
		var entry memory.AuditEntry
		var reason sql.NullString
		var occurredAt string
		if err := rows.Scan(&entry.CandidateID, &entry.Action, &reason, &occurredAt); err != nil {
			return nil, fmt.Errorf("scan memory audit: %w", err)
		}
		var err error
		entry.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse memory audit time: %w", err)
		}
		if reason.Valid {
			entry.Reason = reason.String
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate memory audit: %w", err)
	}
	return entries, nil
}

// insertAudit 写入没有自由文本的状态变更审计行。
func insertAudit(ctx context.Context, transaction *sql.Tx, candidateID, action, reason string, occurredAt time.Time) error {
	if _, err := transaction.ExecContext(ctx, "INSERT INTO audit_log(candidate_id, action, reason, occurred_at) VALUES (?, ?, ?, ?)", candidateID, action, nullableString(reason), occurredAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("insert memory audit: %w", err)
	}
	return nil
}

// checkAuditInvariants 验证候选状态、撤销/替代关系与审计时间线完全一致。SQLite
// quick_check 无法发现这类逻辑篡改，因此每个只读信任入口都必须调用本检查。
func checkAuditInvariants(ctx context.Context, source queryerRows, candidates []memory.Candidate) error {
	var orphanCount int
	if err := source.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log
		LEFT JOIN candidates ON candidates.id = audit_log.candidate_id
		WHERE candidates.id IS NULL`).Scan(&orphanCount); err != nil {
		return fmt.Errorf("check orphan memory audit: %w", err)
	}
	if orphanCount != 0 {
		return errors.New("memory audit has orphan entries")
	}
	byID := make(map[string]memory.Candidate, len(candidates))
	reverseSupersedes := make(map[string]int)
	for _, candidate := range candidates {
		byID[candidate.ID] = candidate
	}
	for _, candidate := range candidates {
		entries, err := auditFor(ctx, source, candidate.ID)
		if err != nil {
			return err
		}
		captured, revoked, superseded := 0, 0, 0
		for _, entry := range entries {
			if entry.CandidateID != candidate.ID || entry.OccurredAt.Before(candidate.CreatedAt) {
				return fmt.Errorf("memory candidate %s has invalid audit timeline", candidate.ID)
			}
			switch entry.Action {
			case memory.AuditCaptured:
				if entry.Reason != "" || !entry.OccurredAt.Equal(candidate.CreatedAt) {
					return fmt.Errorf("memory candidate %s has invalid capture audit", candidate.ID)
				}
				captured++
			case memory.AuditRevoked:
				if !memory.ValidRevocationReason(entry.Reason) {
					return fmt.Errorf("memory candidate %s has invalid revoke audit", candidate.ID)
				}
				revoked++
			case memory.AuditSuperseded:
				if entry.Reason != "" {
					return fmt.Errorf("memory candidate %s has invalid supersede audit", candidate.ID)
				}
				superseded++
			default:
				return fmt.Errorf("memory candidate %s has unknown audit action", candidate.ID)
			}
		}
		if captured != 1 {
			return fmt.Errorf("memory candidate %s must have exactly one capture audit", candidate.ID)
		}
		switch candidate.Status {
		case memory.StatusActive:
			if revoked != 0 || superseded != 0 {
				return fmt.Errorf("active memory candidate %s has terminal audit", candidate.ID)
			}
		case memory.StatusRevoked:
			if revoked != 1 || superseded != 0 {
				return fmt.Errorf("revoked memory candidate %s has invalid audit state", candidate.ID)
			}
		case memory.StatusSuperseded:
			if revoked != 0 || superseded != 1 {
				return fmt.Errorf("superseded memory candidate %s has invalid audit state", candidate.ID)
			}
		}
		if candidate.SupersedesID == "" {
			continue
		}
		target, exists := byID[candidate.SupersedesID]
		if !exists || target.ID == candidate.ID || target.WorkspaceHash != candidate.WorkspaceHash || target.Status != memory.StatusSuperseded {
			return fmt.Errorf("memory candidate %s has invalid supersedes relation", candidate.ID)
		}
		reverseSupersedes[target.ID]++
	}
	for _, candidate := range candidates {
		if candidate.Status == memory.StatusSuperseded && reverseSupersedes[candidate.ID] != 1 {
			return fmt.Errorf("superseded memory candidate %s must have exactly one successor", candidate.ID)
		}
		if candidate.Status != memory.StatusSuperseded && reverseSupersedes[candidate.ID] != 0 {
			return fmt.Errorf("non-superseded memory candidate %s has successor", candidate.ID)
		}
	}
	return nil
}

// captureEquivalent 比较 capture 的不可变事实；当前状态会在撤销/替代后变化，
// 因而不参与同一 Run 重试的幂等判定。
func captureEquivalent(existing, candidate memory.Candidate) bool {
	return existing.ID == candidate.ID && existing.SchemaVersion == candidate.SchemaVersion && existing.Kind == candidate.Kind && existing.WorkspaceHash == candidate.WorkspaceHash && existing.TaskPromptDigest == candidate.TaskPromptDigest && existing.RunID == candidate.RunID && existing.Outcome == candidate.Outcome && existing.FailureClass == candidate.FailureClass && existing.TrustedVerification == candidate.TrustedVerification && existing.CreatedAt.Equal(candidate.CreatedAt) && existing.ExpiresAt.Equal(candidate.ExpiresAt) && existing.SupersedesID == candidate.SupersedesID && reflect.DeepEqual(existing.Evidence, candidate.Evidence)
}

// nullableString 将空值转换为 SQL NULL，避免把可选字段伪装成语义字符串。
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}
