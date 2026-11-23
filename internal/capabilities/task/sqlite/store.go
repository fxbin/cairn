// [INPUT]: 依赖 Task 合同与 SQLite 驱动。
// [OUTPUT]: 提供只读快照、版本更新与原子的分配写入。
// [POS]: 存储任务自身事实与跨库锚点。Run 事实由应用层读取。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 持久化工作区任务和分配。
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	_ "modernc.org/sqlite"

	"github.com/fxbin/cairn/internal/capabilities/task"
)

const schemaVersion = 1

// Store 拥有任务数据库连接。
type Store struct {
	db *sql.DB
}

// Open 使用固定工作区路径打开任务库。只读调用不创建文件或迁移。
func Open(ctx context.Context, path string, readOnly bool) (*Store, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	mode := "ro"
	if !readOnly {
		mode = "rw"
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		switch {
		case err == nil:
			if err := file.Close(); err != nil {
				return nil, err
			}
		case !errors.Is(err, os.ErrExist):
			return nil, err
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || (runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("task database requires a private regular file")
	}
	query := url.Values{"mode": {mode}, "_pragma": {"busy_timeout(5000)", "foreign_keys(ON)"}}
	dsn := sqliteDSN(path, query.Encode())
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db}
	if err := store.initialize(ctx, readOnly); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close 释放任务数据库连接。
func (store *Store) Close() error { return store.db.Close() }

// initialize 只在空白库的首次写入时建立 schema。未知版本拒绝读写。
func (store *Store) initialize(ctx context.Context, readOnly bool) error {
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version == schemaVersion {
		return nil
	}
	if readOnly || version != 0 {
		return fmt.Errorf("unsupported task database schema: %d", version)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE tasks (
			id TEXT PRIMARY KEY,
			revision INTEGER NOT NULL CHECK (revision > 0),
			body BLOB NOT NULL
		);
		CREATE TABLE assignments (
			task_id TEXT NOT NULL REFERENCES tasks(id),
			id TEXT NOT NULL,
			run_id TEXT NOT NULL UNIQUE,
			task_revision INTEGER NOT NULL CHECK (task_revision > 0),
			body BLOB NOT NULL,
			PRIMARY KEY (task_id, id),
			UNIQUE (task_id, task_revision)
		);
		PRAGMA user_version = 1;
	`); err != nil {
		return err
	}
	return tx.Commit()
}

// Create 插入初始任务版本，禁止覆盖已有任务。
func (store *Store) Create(ctx context.Context, current task.Task) error {
	body, err := json.Marshal(current)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(ctx, "INSERT INTO tasks (id, revision, body) VALUES (?, ?, ?)", current.ID, current.Revision, body)
	return err
}

// Read 在同一个只读事务中取任务和按版本排序的不可变分配。
func (store *Store) Read(ctx context.Context, id task.ID) (task.Task, []task.Assignment, error) {
	var current task.Task
	tx, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return current, nil, err
	}
	defer tx.Rollback()
	var body []byte
	var revision uint64
	if err := tx.QueryRowContext(ctx, "SELECT revision, body FROM tasks WHERE id = ?", id).Scan(&revision, &body); err != nil {
		return current, nil, err
	}
	if err := json.Unmarshal(body, &current); err != nil {
		return current, nil, err
	}
	if current.ID != id || current.Revision != revision || current.SchemaVersion != task.SchemaVersion {
		return task.Task{}, nil, errors.New("invalid task identity or revision in storage")
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, task_revision, run_id, body FROM assignments WHERE task_id = ? ORDER BY task_revision", id)
	if err != nil {
		return current, nil, err
	}
	defer rows.Close()
	var assignments []task.Assignment
	for rows.Next() {
		var (
			assignment task.Assignment
			id, runID  string
			revision   uint64
		)
		if err := rows.Scan(&id, &revision, &runID, &body); err != nil {
			return current, nil, err
		}
		if err := json.Unmarshal(body, &assignment); err != nil {
			return current, nil, err
		}
		if assignment.ID != id || string(assignment.RunID) != runID || assignment.Revision != revision || revision >= current.Revision {
			return current, nil, errors.New("invalid task assignment identity or revision in storage")
		}
		assignments = append(assignments, assignment)
	}
	return current, assignments, rows.Err()
}

// Commit 按旧版本条件推进任务。分配与版本在单个事务内提交。
func (store *Store) Commit(ctx context.Context, current task.Task, assignment *task.Assignment) error {
	previous := current.Revision
	current.Revision++
	body, err := json.Marshal(current)
	if err != nil {
		return err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, "UPDATE tasks SET revision = ?, body = ? WHERE id = ? AND revision = ?", current.Revision, body, current.ID, previous)
	if err != nil {
		return err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if updated != 1 {
		return task.ErrConflict
	}
	if assignment != nil {
		body, err := json.Marshal(assignment)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO assignments (task_id, id, run_id, task_revision, body) VALUES (?, ?, ?, ?, ?)",
			current.ID, assignment.ID, assignment.RunID, previous, body); err != nil {
			return err
		}
	}
	return tx.Commit()
}
