/*
 * [INPUT]: 依赖定义修订、制品锚点与 SQLite 单语句写入。
 * [OUTPUT]: 提供只追加修订索引、版本比较和旧库只读查询。
 * [POS]: 叶子只保存索引和摘要。应用层校验正文与归属。
 * [PROTOCOL]: Update this header on change, then check AGENTS.md
 */
// 本文件为工作流事件库增加独立的定义索引。
// Author: fxbin
package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/workflow"
)

const (
	definitionSchemaVersion = 2
	definitionMigrationName = "002_workflow_definitions"
	definitionSchemaSQL     = `
		CREATE TABLE workflow_definitions (
			workspace_hash TEXT NOT NULL,
			definition_id TEXT NOT NULL,
			revision INTEGER NOT NULL CHECK (revision > 0),
			artifact_sha256 TEXT NOT NULL,
			artifact_bytes INTEGER NOT NULL CHECK (artifact_bytes > 0),
			PRIMARY KEY (workspace_hash, definition_id, revision)
		);`
)

// migrateDefinitions 在旧事件表迁移后添加修订索引，并核对已登记的脚本摘要。
func migrateDefinitions(ctx context.Context, transaction *sql.Tx, databaseVersion int) error {
	expected := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(definitionSchemaSQL)))
	var recorded string
	err := transaction.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version = ?", definitionSchemaVersion).Scan(&recorded)
	switch {
	case err == nil:
		if recorded != expected || databaseVersion != definitionSchemaVersion {
			return fmt.Errorf("%w: workflow definition migration mismatch", ErrPersistedContent)
		}
		return nil
	case !errors.Is(err, sql.ErrNoRows):
		return err
	case databaseVersion == definitionSchemaVersion:
		return fmt.Errorf("%w: workflow definition migration is missing", ErrPersistedContent)
	}
	if _, err := transaction.ExecContext(ctx, definitionSchemaSQL); err != nil {
		return fmt.Errorf("create workflow definition index: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES(?, ?, ?, ?)",
		definitionSchemaVersion, definitionMigrationName, expected, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record workflow definition migration: %w", err)
	}
	if _, err := transaction.ExecContext(ctx, "PRAGMA user_version = 2"); err != nil {
		return fmt.Errorf("write workflow definition schema version: %w", err)
	}
	return nil
}

// SaveDefinition 仅在最新修订仍为预期值时追加。修订比较和写入在同一语句内完成。
func (store *Store) SaveDefinition(ctx context.Context, workspaceHash string, reference workflow.DefinitionRef, expectedRevision uint64) error {
	if err := reference.Validate(); err != nil {
		return err
	}
	if workspaceHash == "" || expectedRevision >= math.MaxInt64 || reference.Revision != expectedRevision+1 {
		return errors.New("invalid workflow definition append")
	}
	result, err := store.db.ExecContext(ctx, `
		INSERT INTO workflow_definitions(workspace_hash, definition_id, revision, artifact_sha256, artifact_bytes)
		SELECT ?, ?, ?, ?, ?
		WHERE COALESCE((SELECT MAX(revision) FROM workflow_definitions WHERE workspace_hash = ? AND definition_id = ?), 0) = ?`,
		workspaceHash, reference.ID, reference.Revision, reference.Artifact.SHA256, reference.Artifact.Bytes,
		workspaceHash, reference.ID, expectedRevision)
	if err != nil {
		return fmt.Errorf("append workflow definition revision: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows != 1 {
		return fmt.Errorf("%w: stale workflow definition revision", workflow.ErrConflict)
	}
	return nil
}

// ReadDefinition 读取指定修订锚点。修订为零时选择最新锚点。
func (store *Store) ReadDefinition(ctx context.Context, workspaceHash string, id workflow.DefinitionID, revision uint64) (workflow.DefinitionRef, error) {
	available, err := store.definitionsAvailable(ctx)
	if err != nil {
		return workflow.DefinitionRef{}, err
	}
	if !available {
		return workflow.DefinitionRef{}, sql.ErrNoRows
	}
	row := store.db.QueryRowContext(ctx, `
		SELECT definition_id, revision, artifact_sha256, artifact_bytes
		FROM workflow_definitions WHERE workspace_hash = ? AND definition_id = ? AND (? = 0 OR revision = ?)
		ORDER BY revision DESC LIMIT 1`, workspaceHash, id, revision, revision)
	return scanDefinitionRef(row)
}

// ListDefinitions 按标识返回当前工作区每个定义的最新修订锚点。
func (store *Store) ListDefinitions(ctx context.Context, workspaceHash string) ([]workflow.DefinitionRef, error) {
	available, err := store.definitionsAvailable(ctx)
	if err != nil || !available {
		return nil, err
	}
	rows, err := store.db.QueryContext(ctx, `
		SELECT d.definition_id, d.revision, d.artifact_sha256, d.artifact_bytes
		FROM workflow_definitions d
		WHERE d.workspace_hash = ? AND d.revision = (
			SELECT MAX(revision) FROM workflow_definitions
			WHERE workspace_hash = d.workspace_hash AND definition_id = d.definition_id
		)
		ORDER BY d.definition_id`, workspaceHash)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var references []workflow.DefinitionRef
	for rows.Next() {
		reference, err := scanDefinitionRef(rows)
		if err != nil {
			return nil, err
		}
		references = append(references, reference)
	}
	return references, rows.Err()
}

// definitionsAvailable 允许只读查询旧事件库，且拒绝无法识别的更新模式。
func (store *Store) definitionsAvailable(ctx context.Context) (bool, error) {
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return false, err
	}
	if version > definitionSchemaVersion {
		return false, fmt.Errorf("unsupported workflow store schema %d", version)
	}
	return version == definitionSchemaVersion, nil
}

// definitionRefRow 统一单行查询和列表行的扫描合同。
type definitionRefRow interface {
	Scan(dest ...any) error
}

// scanDefinitionRef 解码并校验定义的索引锚点。
func scanDefinitionRef(row definitionRefRow) (workflow.DefinitionRef, error) {
	var reference workflow.DefinitionRef
	err := row.Scan(&reference.ID, &reference.Revision, &reference.Artifact.SHA256, &reference.Artifact.Bytes)
	if err != nil {
		return workflow.DefinitionRef{}, err
	}
	reference.Artifact.MediaType = "application/json"
	if err := reference.Validate(); err != nil {
		return workflow.DefinitionRef{}, fmt.Errorf("%w: %v", ErrPersistedContent, err)
	}
	return reference, nil
}
