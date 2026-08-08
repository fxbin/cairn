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

	_ "modernc.org/sqlite"

	"github.com/fxbin/cairn/internal/domain"
)

var ErrConflict = errors.New("journal conflict")

type Journal struct {
	db *sql.DB
}

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

// OpenReadOnly opens an existing journal without creating its parent
// directory, running migrations, or allowing SQLite to repair state. It is
// used by doctor so a diagnostic command cannot turn a missing or corrupt
// state directory into a new source of truth.
func OpenReadOnly(path string) (*Journal, error) {
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
	if err := db.PingContext(context.Background()); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping read-only SQLite journal: %w", err)
	}
	return &Journal{db: db}, nil
}

func (journal *Journal) Close() error { return journal.db.Close() }

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
			return nil, fmt.Errorf("scan journal event: %w", err)
		}
		var err error
		event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt)
		if err != nil {
			return nil, fmt.Errorf("parse journal event time: %w", err)
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

func (journal *Journal) NonTerminalRuns(ctx context.Context, workspaceHash string) ([]domain.RunProjection, error) {
	rows, err := journal.db.QueryContext(ctx, "SELECT DISTINCT run_id FROM events WHERE workspace_hash = ?", workspaceHash)
	if err != nil {
		return nil, fmt.Errorf("list workspace runs: %w", err)
	}
	var runIDs []domain.RunID
	for rows.Next() {
		var runID domain.RunID
		if err := rows.Scan(&runID); err != nil {
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
		if err := domain.VerifyChain(events); err != nil {
			return fmt.Errorf("check run %s: %w", runID, err)
		}
	}
	return nil
}

// RunIDs returns the immutable set of runs stored by this journal. The
// application uses it for read-only integrity checks that span event payload
// references and the adjacent artifact store.
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

func nullableID[T ~string](value *T) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func optionalTurnID(value sql.NullString) *domain.TurnID {
	if !value.Valid {
		return nil
	}
	id := domain.TurnID(value.String)
	return &id
}

func optionalOperationID(value sql.NullString) *domain.OperationID {
	if !value.Valid {
		return nil
	}
	id := domain.OperationID(value.String)
	return &id
}

func optionalAttemptID(value sql.NullString) *domain.AttemptID {
	if !value.Valid {
		return nil
	}
	id := domain.AttemptID(value.String)
	return &id
}
