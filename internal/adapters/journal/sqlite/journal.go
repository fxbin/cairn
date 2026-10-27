// Package sqlite persists Cairn's append-only event journal in SQLite.
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

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// ErrConflict 表示追加时观察到的日志头序号与调用方预期不一致；与 ports
// 共享哨兵，供运行中 Loop 做确定性的重载重试判定。
var ErrConflict = ports.ErrJournalConflict

// ErrPersistedContent 表示 SQLite 行可读取，但其中的持久化事件字段无法解码。
var ErrPersistedContent = errors.New("invalid persisted journal content")

// Journal 将追加式领域事件持久化在单连接 SQLite 数据库中。
type Journal struct {
	db *sql.DB
}

// Open 创建或打开可写日志，并在首次打开时执行受校验的 schema 迁移。
func Open(path string) (*Journal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("journal path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create journal directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite journal: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	journal := &Journal{db: db}
	if err := journal.migrate(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return journal, nil
}

// OpenReadOnly 打开既有日志但不创建目录、不迁移也不修复 SQLite 状态；
// 兼容既有调用方，打开阶段使用后台上下文。
func OpenReadOnly(path string) (*Journal, error) {
	return OpenReadOnlyContext(context.Background(), path)
}

// OpenReadOnlyContext 在调用方上下文约束下打开既有日志，不创建目录、不迁移
// 也不修复 SQLite 状态，保证只读查询可在连接探测阶段响应取消。
func OpenReadOnlyContext(ctx context.Context, path string) (*Journal, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("journal path is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat read-only SQLite journal: %w", err)
	}
	if info.IsDir() {
		return nil, errors.New("read-only SQLite journal path is a directory")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only SQLite journal: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if err := pingReadOnlyContext(ctx, db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping read-only SQLite journal: %w", err)
	}
	return &Journal{db: db}, nil
}

// 只读连接只等待短暂的 SQLite 启动锁；不改变 SQLite 的全局忙等待，
// 每次连接失败之间均观察调用方取消，超过等待窗口仍报告连接失败。
const (
	readOnlyOpenWait          = time.Second
	readOnlyOpenRetryInterval = 10 * time.Millisecond
)

// pingReadOnlyContext 只重试连接阶段的 BUSY/LOCKED，绝不重试写入或吞掉损坏。
func pingReadOnlyContext(ctx context.Context, db *sql.DB) error {
	waitContext, cancel := context.WithTimeout(ctx, readOnlyOpenWait)
	defer cancel()
	for {
		err := db.PingContext(waitContext)
		if err == nil {
			return nil
		}
		var sqliteError *modernsqlite.Error
		if !errors.As(err, &sqliteError) || (sqliteError.Code()&0xff != sqlite3.SQLITE_BUSY && sqliteError.Code()&0xff != sqlite3.SQLITE_LOCKED) {
			return err
		}
		timer := time.NewTimer(readOnlyOpenRetryInterval)
		select {
		case <-waitContext.Done():
			timer.Stop()
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		case <-timer.C:
		}
	}
}

// Close 关闭 SQLite 连接并释放相关资源。
func (journal *Journal) Close() error { return journal.db.Close() }

// IsStateCorruptError 判断错误是否表明日志内容或 schema 已不可读取；SQLite
// 权限、资源耗尽、锁竞争和其他环境 I/O 错误不归入状态损坏。
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

// migrate 配置 SQLite 的耐久性选项并以校验和保护初始 schema 迁移。
func (journal *Journal) migrate(ctx context.Context) error {
	statements := []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = FULL",
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
	}
	for _, statement := range statements {
		if _, err := journal.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure SQLite journal: %w", err)
		}
	}
	transaction, err := journal.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start journal migration: %w", err)
	}
	defer transaction.Rollback()
	_, err = transaction.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			applied_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS events (
			run_id TEXT NOT NULL,
			workspace_hash TEXT NOT NULL,
			seq INTEGER NOT NULL,
			event_id TEXT NOT NULL UNIQUE,
			schema_version INTEGER NOT NULL,
			occurred_at TEXT NOT NULL,
			event_type TEXT NOT NULL,
			turn_id TEXT,
			operation_id TEXT,
			attempt_id TEXT,
			payload_json BLOB NOT NULL,
			prev_hash TEXT NOT NULL,
			event_hash TEXT NOT NULL,
			PRIMARY KEY (run_id, seq)
		);
		CREATE INDEX IF NOT EXISTS events_by_operation ON events(run_id, operation_id, seq) WHERE operation_id IS NOT NULL;
		CREATE INDEX IF NOT EXISTS events_by_workspace_run ON events(workspace_hash, run_id, seq);`)
	if err != nil {
		return fmt.Errorf("create journal schema: %w", err)
	}
	const migrationName = "001_event_journal_v1"
	const migrationHash = "cairn-schema-v1-event-journal"
	var currentVersion int
	if err := transaction.QueryRowContext(ctx, "PRAGMA user_version").Scan(&currentVersion); err != nil {
		return fmt.Errorf("read SQLite user_version: %w", err)
	}
	if currentVersion > 1 {
		return fmt.Errorf("journal schema version %d is newer than this Cairn binary", currentVersion)
	}
	var recordedHash string
	err = transaction.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version = 1").Scan(&recordedHash)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := transaction.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES(1, ?, ?, ?)", migrationName, migrationHash, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record journal migration: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, "PRAGMA user_version = 1"); err != nil {
			return fmt.Errorf("write SQLite user_version: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("read journal migration: %w", err)
	} else if recordedHash != migrationHash {
		return errors.New("journal migration checksum mismatch")
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit journal migration: %w", err)
	}
	return nil
}

// Append 在单个事务中校验日志头并追加带哈希的领域事件。
func (journal *Journal) Append(ctx context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error) {
	transaction, err := journal.db.BeginTx(ctx, nil)
	if err != nil {
		return domain.Event{}, fmt.Errorf("start journal append: %w", err)
	}
	defer transaction.Rollback()
	var sequence uint64
	previousHash := domain.GenesisHash
	var latestSeq int64
	var latestHash string
	err = transaction.QueryRowContext(ctx, "SELECT seq, event_hash FROM events WHERE run_id = ? ORDER BY seq DESC LIMIT 1", draft.RunID).Scan(&latestSeq, &latestHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return domain.Event{}, fmt.Errorf("read journal head: %w", err)
	default:
		sequence = uint64(latestSeq)
		previousHash = latestHash
	}
	if sequence != expectedSeq {
		return domain.Event{}, fmt.Errorf("%w: run %s expected %d, current %d", ErrConflict, draft.RunID, expectedSeq, sequence)
	}
	event, err := domain.NewEvent(draft, sequence+1, previousHash)
	if err != nil {
		return domain.Event{}, err
	}
	_, err = transaction.ExecContext(ctx, `INSERT INTO events(
		run_id, workspace_hash, seq, event_id, schema_version, occurred_at, event_type,
		turn_id, operation_id, attempt_id, payload_json, prev_hash, event_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		event.RunID, event.WorkspaceHash, event.Seq, event.EventID, event.SchemaVersion,
		event.OccurredAt.UTC().Format(time.RFC3339Nano), event.Type,
		nullableID(event.TurnID), nullableID(event.OperationID), nullableID(event.AttemptID),
		[]byte(event.Payload), event.PrevHash, event.EventHash)
	if err != nil {
		return domain.Event{}, fmt.Errorf("insert journal event: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return domain.Event{}, fmt.Errorf("commit journal event: %w", err)
	}
	return event, nil
}

// Load 按序号升序读取指定运行的全部持久化事件。
func (journal *Journal) Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error) {
	rows, err := journal.db.QueryContext(ctx, `SELECT schema_version, event_id, run_id, workspace_hash, seq, occurred_at, event_type,
		turn_id, operation_id, attempt_id, payload_json, prev_hash, event_hash
		FROM events WHERE run_id = ? ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("load journal events: %w", err)
	}
	defer rows.Close()
	var events []domain.Event
	for rows.Next() {
		var event domain.Event
		var occurredAt string
		var turnID, operationID, attemptID sql.NullString
		var payload []byte
		if err := rows.Scan(&event.SchemaVersion, &event.EventID, &event.RunID, &event.WorkspaceHash, &event.Seq, &occurredAt, &event.Type, &turnID, &operationID, &attemptID, &payload, &event.PrevHash, &event.EventHash); err != nil {
			return nil, fmt.Errorf("%w: scan journal event: %v", ErrPersistedContent, err)
		}
		var err error
		event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("%w: parse journal event time: %v", ErrPersistedContent, err)
		}
		event.Payload = append(event.Payload[:0], payload...)
		event.TurnID = optionalTurnID(turnID)
		event.OperationID = optionalOperationID(operationID)
		event.AttemptID = optionalAttemptID(attemptID)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate journal events: %w", err)
	}
	return events, nil
}

// NonTerminalRuns 重放工作区对应的运行，并筛出可恢复的非终态投影。
func (journal *Journal) NonTerminalRuns(ctx context.Context, workspaceHash string) ([]domain.RunProjection, error) {
	rows, err := journal.db.QueryContext(ctx, "SELECT run_id, MIN(seq) AS first_seq FROM events WHERE workspace_hash = ? GROUP BY run_id ORDER BY first_seq", workspaceHash)
	if err != nil {
		return nil, fmt.Errorf("list workspace runs: %w", err)
	}
	var runIDs []domain.RunID
	for rows.Next() {
		var runID domain.RunID
		var firstSeq uint64
		if err := rows.Scan(&runID, &firstSeq); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan workspace run: %w", err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var results []domain.RunProjection
	for _, runID := range runIDs {
		if runID == domain.QueueRunID {
			continue
		}
		events, err := journal.Load(ctx, runID)
		if err != nil {
			return nil, err
		}
		projection, err := domain.Replay(events)
		if err != nil {
			return nil, fmt.Errorf("replay run %s: %w", runID, err)
		}
		if !projection.Terminal() {
			results = append(results, projection)
		}
	}
	return results, nil
}

// Check 同时执行 SQLite 快速完整性检查和所有运行的完整事件回放校验。
func (journal *Journal) Check(ctx context.Context) error {
	var result string
	if err := journal.db.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&result); err != nil {
		return fmt.Errorf("SQLite quick_check: %w", err)
	}
	if result != "ok" {
		return fmt.Errorf("SQLite quick_check: %s", result)
	}
	rows, err := journal.db.QueryContext(ctx, "SELECT DISTINCT run_id FROM events")
	if err != nil {
		return fmt.Errorf("list journal runs: %w", err)
	}
	var runIDs []domain.RunID
	for rows.Next() {
		var runID domain.RunID
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return err
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, runID := range runIDs {
		events, err := journal.Load(ctx, runID)
		if err != nil {
			return err
		}
		if runID == domain.QueueRunID {
			if _, err := domain.ProjectQueue(events); err != nil {
				return fmt.Errorf("check queue: %w", err)
			}
			continue
		}
		if _, err := domain.Replay(events); err != nil {
			return fmt.Errorf("check run %s: %w", runID, err)
		}
	}
	return nil
}

// RunIDs 返回日志内运行标识符的有序集合，供应用层跨事件 payload 与制品库进行只读完整性检查。
func (journal *Journal) RunIDs(ctx context.Context) ([]domain.RunID, error) {
	rows, err := journal.db.QueryContext(ctx, "SELECT DISTINCT run_id FROM events ORDER BY run_id")
	if err != nil {
		return nil, fmt.Errorf("list journal runs: %w", err)
	}
	defer rows.Close()
	var runIDs []domain.RunID
	for rows.Next() {
		var runID domain.RunID
		if err := rows.Scan(&runID); err != nil {
			return nil, fmt.Errorf("scan journal run: %w", err)
		}
		runIDs = append(runIDs, runID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate journal runs: %w", err)
	}
	return runIDs, nil
}

// nullableID 将可选领域标识转换为 SQLite 可接受的 NULL 或字符串。
func nullableID[T ~string](value *T) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

// optionalTurnID 将 SQLite 可空字符串还原为可选轮次标识。
func optionalTurnID(value sql.NullString) *domain.TurnID {
	if !value.Valid {
		return nil
	}
	id := domain.TurnID(value.String)
	return &id
}

// optionalOperationID 将 SQLite 可空字符串还原为可选操作标识。
func optionalOperationID(value sql.NullString) *domain.OperationID {
	if !value.Valid {
		return nil
	}
	id := domain.OperationID(value.String)
	return &id
}

// optionalAttemptID 将 SQLite 可空字符串还原为可选尝试标识。
func optionalAttemptID(value sql.NullString) *domain.AttemptID {
	if !value.Valid {
		return nil
	}
	id := domain.AttemptID(value.String)
	return &id
}
