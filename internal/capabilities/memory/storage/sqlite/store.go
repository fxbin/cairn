// [INPUT]: 依赖父能力合同与SQLite驱动。
// [OUTPUT]: 提供私有连接、只读打开与历史预检。
// [POS]: 连接由应用持有；存储不读取运行Journal。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 提供能力内的SQLite持久化叶子。
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

	_ "modernc.org/sqlite"
)

// ErrLegacyV2ConsolidationIncompatible 标识含巩固事实的历史 v2 Memory SQLite
// 数据库无法以当前唯一派生语义打开。调用方可用 errors.Is 提示开发期重建，且该
// 拒绝路径在任何迁移或持久化写入前完成。
var ErrLegacyV2ConsolidationIncompatible = errors.New("legacy v2 memory consolidation database is incompatible")

// Store 管理一个工作区专属、单连接的 Memory SQLite 数据库。
type Store struct {
	db  *sql.DB
	now func() time.Time
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
	if err := preflightWritablePath(path); err != nil {
		return nil, err
	}
	if err := preflightLegacyV2(context.Background(), path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open memory SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, now: time.Now}
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
	if err := preflightLegacyV2(context.Background(), path); err != nil {
		return nil, err
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
	return &Store{db: db, now: time.Now}, nil
}

// preflightWritablePath 在父目录已私有化后拒绝末端链接和非常规文件，确保后续的
// legacy 预检与 SQLite 打开不会跟随它们。不存在的末端路径保留给 SQLite 创建。
func preflightWritablePath(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("lstat writable memory SQLite: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("writable memory SQLite must be a non-symlink regular file")
	}
	return nil
}

// preflightLegacyV2 在只读不可变连接中检查既有数据库的物理 schema 版本。它用
// Stat 读取调用方已验证为普通文件的物理目标；仅当版本为 v2 时，先验证 v1/v2
// 迁移摘要，随后拒绝含巩固事实的历史库。该函数不会开启事务、配置 journal_mode、
// 执行 DDL 或修改数据库文件。
func preflightLegacyV2(ctx context.Context, path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat legacy memory SQLite preflight: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return fmt.Errorf("open legacy memory SQLite preflight: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read legacy memory schema version: %w", err)
	}
	if version != migrationVersionTwo {
		return nil
	}
	if err := verifyLegacyV2Migration(ctx, db, 1, migrationName, migrationHash); err != nil {
		return fmt.Errorf("verify legacy memory migration: %w", err)
	}
	if err := verifyLegacyV2Migration(ctx, db, migrationVersionTwo, migrationNameTwo, migrationHashTwo); err != nil {
		return fmt.Errorf("verify legacy memory migration: %w", err)
	}
	var consolidationCount int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM consolidations").Scan(&consolidationCount); err != nil {
		return fmt.Errorf("read legacy memory consolidations: %w", err)
	}
	if consolidationCount == 0 {
		return nil
	}
	return fmt.Errorf("%w; rebuild the development memory database", ErrLegacyV2ConsolidationIncompatible)
}

// currentUTC 返回 Store 当前的 UTC 时钟。生产入口使用系统时钟，测试可注入
// 固定时钟以稳定验证到期与当前资格边界。
func (store *Store) currentUTC() time.Time {
	if store.now == nil {
		return time.Now().UTC()
	}
	return store.now().UTC()
}

// Close 关闭 Memory SQLite 连接并释放文件描述符。
func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
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
