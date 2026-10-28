// Package sqlite 持久化 Cairn 工作流执行链的追加式事件存储。
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	modernsqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/fxbin/cairn/internal/workflow"
)

// ErrPersistedContent 表示行可读取，但持久化的事件字段无法解码。
var ErrPersistedContent = errors.New("invalid persisted workflow content")

// Store 将工作流事件持久化在单连接 SQLite 数据库中；每条工作流一条
// 独立哈希链，序号从 1 连续递增，追加采用链头序号比较实现乐观并发。
type Store struct {
	db *sql.DB
}

// Open 创建或打开可写工作流存储，并在首次打开时执行受校验的迁移。
func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("workflow store path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create workflow store directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite workflow store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db}
	if err := store.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// OpenReadOnly 打开既有工作流存储但不创建目录、不迁移也不修复 SQLite 状态。
func OpenReadOnly(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("workflow store path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat read-only SQLite workflow store: %w", err)
	}
	if info.IsDir() {
		return nil, errors.New("read-only SQLite workflow store path is a directory")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only SQLite workflow store: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping read-only SQLite workflow store: %w", err)
	}
	return &Store{db: db}, nil
}

// Close 关闭 SQLite 连接并释放相关资源。
func (store *Store) Close() error { return store.db.Close() }

// migrate 配置 SQLite 耐久性选项并以校验和保护初始 schema 迁移。
func (store *Store) migrate(ctx context.Context) error {
	statements := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA busy_timeout = 5000",
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite workflow store: %w", err)
		}
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start workflow store migration: %w", err)
	}
	defer transaction.Rollback()
	_, err = transaction.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			applied_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS workflow_events (
			workflow_id TEXT NOT NULL,
			workspace_hash TEXT NOT NULL,
			seq INTEGER NOT NULL,
			event_id TEXT NOT NULL UNIQUE,
			schema_version INTEGER NOT NULL,
			occurred_at TEXT NOT NULL,
			event_type TEXT NOT NULL,
			payload_json BLOB NOT NULL,
			prev_hash TEXT NOT NULL,
			event_hash TEXT NOT NULL,
			PRIMARY KEY (workflow_id, seq)
		);
		CREATE INDEX IF NOT EXISTS workflow_events_by_workspace ON workflow_events(workspace_hash, workflow_id, seq);`)
	if err != nil {
		return fmt.Errorf("create workflow store schema: %w", err)
	}
	const migrationName = "001_workflow_events_v1"
	const migrationHash = "cairn-schema-v1-workflow-events"
	var currentVersion int
	if err := transaction.QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentVersion); err != nil {
		return fmt.Errorf("read SQLite user_version: %w", err)
	}
	if currentVersion > 1 {
		return fmt.Errorf("workflow store schema version %d is newer than this Cairn binary", currentVersion)
	}
	var recordedHash string
	err = transaction.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version = 1").Scan(&recordedHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := transaction.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES(1, ?, ?, ?)", migrationName, migrationHash, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record workflow store migration: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("write SQLite user_version: %w", err)
		}
	case err != nil:
		return fmt.Errorf("read workflow store migration: %w", err)
	case recordedHash != migrationHash:
		return errors.New("workflow store migration checksum mismatch")
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit workflow store migration: %w", err)
	}
	return nil
}

// Append 在单个事务中校验链头并追加带哈希的工作流事件。
func (store *Store) Append(ctx context.Context, expectedSeq uint64, draft workflow.EventDraft) (workflow.Event, error) {
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return workflow.Event{}, fmt.Errorf("start workflow store append: %w", err)
	}
	defer transaction.Rollback()
	var sequence uint64
	previousHash := workflow.GenesisHash
	var latestSeq int64
	var latestHash string
	err = transaction.QueryRowContext(ctx, "SELECT seq, event_hash FROM workflow_events WHERE workflow_id = ? ORDER BY seq DESC LIMIT 1", draft.WorkflowID).Scan(&latestSeq, &latestHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return workflow.Event{}, fmt.Errorf("read workflow store head: %w", err)
	default:
		sequence = uint64(latestSeq)
		previousHash = latestHash
	}
	if sequence != expectedSeq {
		return workflow.Event{}, fmt.Errorf("%w: workflow %s expected %d, current %d", workflow.ErrConflict, draft.WorkflowID, expectedSeq, sequence)
	}
	event, err := workflow.NewEvent(draft, sequence+1, previousHash)
	if err != nil {
		return workflow.Event{}, err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO workflow_events(
		workflow_id, workspace_hash, seq, event_id, schema_version, occurred_at, event_type,
		payload_json, prev_hash, event_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.WorkflowID, event.WorkspaceHash, event.Seq, event.EventID, event.SchemaVersion,
		event.OccurredAt.UTC().Format(time.RFC3339Nano), event.Type,
		[]byte(event.Payload), event.PrevHash, event.EventHash)
	if err != nil {
		return workflow.Event{}, fmt.Errorf("insert workflow event: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return workflow.Event{}, fmt.Errorf("commit workflow event: %w", err)
	}
	return event, nil
}

// Load 按序号升序读取指定工作流的全部持久化事件。
func (store *Store) Load(ctx context.Context, id workflow.ID) ([]workflow.Event, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT schema_version, event_id, workflow_id, workspace_hash, seq, occurred_at, event_type,
		payload_json, prev_hash, event_hash
		FROM workflow_events WHERE workflow_id = ? ORDER BY seq ASC`, id)
	if err != nil {
		return nil, fmt.Errorf("load workflow events: %w", err)
	}
	defer rows.Close()
	var events []workflow.Event
	for rows.Next() {
		var event workflow.Event
		var occurredAt string
		var payload []byte
		if err := rows.Scan(&event.SchemaVersion, &event.EventID, &event.WorkflowID, &event.WorkspaceHash, &event.Seq, &occurredAt, &event.Type, &payload, &event.PrevHash, &event.EventHash); err != nil {
			return nil, fmt.Errorf("%w: scan workflow event: %v", ErrPersistedContent, err)
		}
		event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("%w: parse workflow event time: %v", ErrPersistedContent, err)
		}
		event.Payload = append(event.Payload[:0], payload...)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow events: %w", err)
	}
	return events, nil
}

// WorkflowIDs 返回工作区内全部工作流标识，按创建顺序（首事件序号）排列。
func (store *Store) WorkflowIDs(ctx context.Context, workspaceHash string) ([]workflow.ID, error) {
	rows, err := store.db.QueryContext(ctx, `SELECT workflow_id, MIN(seq) AS first_seq FROM workflow_events WHERE workspace_hash = ? GROUP BY workflow_id ORDER BY first_seq`, workspaceHash)
	if err != nil {
		return nil, fmt.Errorf("list workspace workflows: %w", err)
	}
	defer rows.Close()
	var ids []workflow.ID
	for rows.Next() {
		var id workflow.ID
		var firstSeq uint64
		if err := rows.Scan(&id, &firstSeq); err != nil {
			return nil, fmt.Errorf("scan workspace workflow: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workspace workflows: %w", err)
	}
	return ids, nil
}

// Check 执行 SQLite 快速完整性检查并校验全部工作流事件链。
func (store *Store) Check(ctx context.Context) error {
	var result string
	if err := store.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("SQLite quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check: %s", result)
	}
	rows, err := store.db.QueryContext(ctx, "SELECT DISTINCT workflow_id FROM workflow_events")
	if err != nil {
		return fmt.Errorf("list workflow store chains: %w", err)
	}
	var ids []workflow.ID
	for rows.Next() {
		var id workflow.ID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		events, err := store.Load(ctx, id)
		if err != nil {
			return err
		}
		if err := workflow.VerifyChain(events); err != nil {
			return fmt.Errorf("check workflow %s: %w", id, err)
		}
	}
	return nil
}

// IsStateCorruptError 判断错误是否表明工作流存储内容或 schema 已不可读取；
// 权限、资源与锁竞争等环境错误不归入状态损坏。
func IsStateCorruptError(err error) bool {
	if errors.Is(err, ErrPersistedContent) {
		return true
	}
	var sqliteErr *modernsqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	switch sqliteErr.Code() & 0xff {
	case sqlite3.SQLITE_ERROR, sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_SCHEMA, sqlite3.SQLITE_NOTADB:
		return true
	default:
		return false
	}
}
