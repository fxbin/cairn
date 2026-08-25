// Package sqlite 将受限的可信记忆控制面持久化为独立 SQLite 数据库；它不读取
// Event Journal 内容，也不承担运行事实源职责。
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fxbin/cairn/internal/memory"
)

const (
	migrationVersion = 2
	migrationName    = "001_trusted_memory_v1"
	migrationHash    = "cairn-schema-v1-trusted-memory"
)

// Store 管理一个工作区专属、单连接的 Memory SQLite 数据库。
type Store struct {
	db *sql.DB
}

// Open 创建或打开可写 Memory Store，并执行受校验迁移与严格 SQLite 耐久性配置。
func Open(path string) (*Store, error) {
	path, err := absolutePath(path)
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return nil, fmt.Errorf("create memory directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return nil, fmt.Errorf("secure memory directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open memory SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("secure memory database: %w", err)
	}
	return store, nil
}

// OpenReadOnly 打开既有且权限严格的 Memory Store，绝不创建、迁移、修复、chmod
// 或修改数据库。它拒绝符号链接、特殊文件、非 0600 数据库和非 0700 父目录。
func OpenReadOnly(path string) (*Store, error) {
	path, err := absolutePath(path)
	if err != nil {
		return nil, err
	}
	directory := filepath.Dir(path)
	directoryInfo, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("lstat read-only memory directory: %w", err)
	}
	if directoryInfo.Mode()&os.ModeSymlink != 0 || !directoryInfo.IsDir() || directoryInfo.Mode().Perm() != 0o700 {
		return nil, errors.New("read-only memory SQLite directory must be a non-symlink 0700 directory")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("lstat read-only memory SQLite: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, errors.New("read-only memory SQLite must be a non-symlink regular 0600 file")
	}
	// immutable=1 使 SQLite 不尝试创建 -wal/-shm 辅助文件；只读 recall/doctor
	// 接受的是一个关闭后可校验的 Memory Store，若文件处于未检查点写入状态应由
	// 后续完整性校验失败关闭，而不是在诊断路径写入辅助状态。
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only memory SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping read-only memory SQLite: %w", err)
	}
	if _, err := db.ExecContext(context.Background(), "PRAGMA query_only = ON"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("configure read-only memory SQLite: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭 Memory SQLite 连接并释放文件描述符。
func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

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
		if !found || superseded.WorkspaceHash != candidate.WorkspaceHash || superseded.Status != memory.StatusActive {
			return memory.Candidate{}, errors.New("memory supersede target must be an active candidate in the same workspace")
		}
		if _, err := transaction.ExecContext(ctx, "UPDATE candidates SET status = ? WHERE id = ?", memory.StatusSuperseded, superseded.ID); err != nil {
			return memory.Candidate{}, fmt.Errorf("supersede memory candidate: %w", err)
		}
		supersededAt := time.Now().UTC()
		if supersededAt.Before(superseded.CreatedAt) {
			supersededAt = superseded.CreatedAt
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
		now = time.Now().UTC()
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
		occurredAt = time.Now().UTC()
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
	if occurredAt.Before(candidate.CreatedAt) {
		occurredAt = candidate.CreatedAt
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
	return checkDerivedInvariants(ctx, store.db)
}

// migrate 配置耐久性选项并以迁移校验和初始化唯一支持的 schema 版本。
func (store *Store) migrate(ctx context.Context) error {
	for _, statement := range []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL", "PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000"} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure memory SQLite: %w", err)
		}
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start memory migration: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			applied_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS candidates (
			id TEXT PRIMARY KEY,
			schema_version INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK(kind = 'run_outcome'),
			workspace_hash TEXT NOT NULL,
			task_prompt_digest TEXT NOT NULL,
			run_id TEXT NOT NULL,
			outcome TEXT NOT NULL CHECK(outcome IN ('completed', 'failed', 'cancelled')),
			failure_class TEXT,
			trusted_verification INTEGER NOT NULL CHECK(trusted_verification IN (0, 1)),
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('active', 'revoked', 'superseded')),
			supersedes_id TEXT REFERENCES candidates(id),
			UNIQUE(workspace_hash, run_id)
		);
		CREATE TABLE IF NOT EXISTS evidence_refs (
			candidate_id TEXT NOT NULL REFERENCES candidates(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			run_id TEXT NOT NULL,
			event_seq INTEGER NOT NULL,
			event_hash TEXT NOT NULL,
			event_type TEXT NOT NULL,
			artifact_sha256 TEXT,
			PRIMARY KEY(candidate_id, ordinal)
		);
		CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			candidate_id TEXT NOT NULL REFERENCES candidates(id),
			action TEXT NOT NULL CHECK(action IN ('captured', 'revoked', 'superseded')),
			reason TEXT CHECK(reason IS NULL OR reason IN ('evidence_invalid', 'human_request')),
			occurred_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS candidates_recall ON candidates(workspace_hash, kind, status, expires_at, created_at DESC, id ASC);
		CREATE INDEX IF NOT EXISTS evidence_refs_by_event ON evidence_refs(run_id, event_seq, event_hash);
		CREATE INDEX IF NOT EXISTS audit_log_by_candidate ON audit_log(candidate_id, id);
		CREATE TABLE IF NOT EXISTS consolidations (
			id TEXT PRIMARY KEY,
			schema_version INTEGER NOT NULL,
			strategy TEXT NOT NULL CHECK(strategy IN ('task_outcome_stability', 'failure_class_recurrence')),
			workspace_hash TEXT NOT NULL,
			inputs_digest TEXT NOT NULL,
			input_ids TEXT NOT NULL,
			input_count INTEGER NOT NULL,
			committed_at TEXT NOT NULL,
			derived_ttl_seconds INTEGER NOT NULL CHECK(derived_ttl_seconds > 0),
			status TEXT NOT NULL CHECK(status IN ('committed'))
		);
		CREATE TABLE IF NOT EXISTS derived_candidates (
			id TEXT NOT NULL,
			schema_version INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK(kind IN ('pattern', 'hypothesis')),
			consolidation_id TEXT NOT NULL REFERENCES consolidations(id),
			workspace_hash TEXT NOT NULL,
			scope_digest TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('active', 'rejected', 'superseded')),
			supersedes_id TEXT,
			supersedes_consolidation_id TEXT,
			PRIMARY KEY(id, consolidation_id),
			FOREIGN KEY (supersedes_id, supersedes_consolidation_id) REFERENCES derived_candidates(id, consolidation_id)
		);
		CREATE TABLE IF NOT EXISTS derived_evidence (
			derived_id TEXT NOT NULL,
			consolidation_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			source_candidate_id TEXT NOT NULL REFERENCES candidates(id),
			PRIMARY KEY(derived_id, consolidation_id, ordinal),
			FOREIGN KEY (derived_id, consolidation_id) REFERENCES derived_candidates(id, consolidation_id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS derived_audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			derived_id TEXT NOT NULL,
			consolidation_id TEXT NOT NULL,
			action TEXT NOT NULL CHECK(action IN ('consolidated', 'rejected', 'superseded')),
			reason TEXT CHECK(reason IS NULL OR reason IN ('insufficient_evidence', 'human_request')),
			occurred_at TEXT NOT NULL,
			FOREIGN KEY (derived_id, consolidation_id) REFERENCES derived_candidates(id, consolidation_id)
		);
		CREATE INDEX IF NOT EXISTS consolidations_by_workspace ON consolidations(workspace_hash, id);
		CREATE INDEX IF NOT EXISTS derived_candidates_by_scope ON derived_candidates(workspace_hash, kind, scope_digest, status);
		CREATE INDEX IF NOT EXISTS derived_evidence_by_source ON derived_evidence(source_candidate_id);`); err != nil {
		return fmt.Errorf("create memory schema: %w", err)
	}
	if err := checkMigration(ctx, transaction); err != nil {
		var version int
		if queryErr := transaction.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); queryErr != nil || (version != 0 && version != 1) {
			return err
		}
		if version == 1 {
			var recorded string
			if verifyErr := transaction.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version = 1").Scan(&recorded); verifyErr != nil || recorded != migrationHash {
				return errors.New("memory v1 migration checksum mismatch before upgrade")
			}
		}
		if version == 0 {
			if _, insertErr := transaction.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)", 1, migrationName, migrationHash, time.Now().UTC().Format(time.RFC3339Nano)); insertErr != nil {
				return fmt.Errorf("record memory migration: %w", insertErr)
			}
			if _, setErr := transaction.ExecContext(ctx, "PRAGMA user_version = 1"); setErr != nil {
				return fmt.Errorf("write memory schema version: %w", setErr)
			}
		}
		if _, insertErr := transaction.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)", migrationVersionTwo, migrationNameTwo, migrationHashTwo, time.Now().UTC().Format(time.RFC3339Nano)); insertErr != nil {
			return fmt.Errorf("record memory consolidation migration: %w", insertErr)
		}
		if _, setErr := transaction.ExecContext(ctx, "PRAGMA user_version = 2"); setErr != nil {
			return fmt.Errorf("write memory consolidation schema version: %w", setErr)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit memory migration: %w", err)
	}
	return nil
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

// checkMigration 读取版本链与各版本校验和；未来版本、缺失记录与篡改均失败关闭。
// 版本链当前为 1（情节记忆）与 2（巩固事务），单向兼容：旧二进制读新库失败关闭。
func checkMigration(ctx context.Context, source queryer) error {
	var version int
	if err := source.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read memory schema version: %w", err)
	}
	if version > migrationVersion {
		return fmt.Errorf("memory schema version %d is newer than this Cairn binary", version)
	}
	if version != migrationVersion {
		return fmt.Errorf("memory schema version %d is unsupported", version)
	}
	expected := map[int]string{1: migrationHash, migrationVersionTwo: migrationHashTwo}
	for migrationID, hash := range expected {
		var recorded string
		if err := source.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version = ?", migrationID).Scan(&recorded); err != nil {
			return fmt.Errorf("read memory migration %d: %w", migrationID, err)
		}
		if recorded != hash {
			return fmt.Errorf("memory migration %d checksum mismatch", migrationID)
		}
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

// absolutePath 规范化数据库路径，避免相对 SQLite URI 被解析成 authority。
func absolutePath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("memory SQLite path is required")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve memory SQLite path: %w", err)
	}
	return abs, nil
}

// nullableString 将空值转换为 SQL NULL，避免把可选字段伪装成语义字符串。
func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// validWorkspaceDigest 复用 memory 的严格摘要格式，而不向适配器暴露内部校验函数。
func validWorkspaceDigest(value string) bool {
	probe, err := memory.NewRunOutcomeCandidate(value, "run_probe", "sha256:0000000000000000000000000000000000000000000000000000000000000000", "failed", "agent", false, time.Unix(0, 0), time.Second, []memory.EventEvidenceRef{{RunID: "run_probe", EventSeq: 1, EventHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000", EventType: "run_failed"}}, "")
	return err == nil && probe.WorkspaceHash == value
}
