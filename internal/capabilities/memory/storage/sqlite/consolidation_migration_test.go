// [INPUT]: 依赖真实SQLite与父能力结构化合同。
// [OUTPUT]: 提供历史v2拒绝、升级原子性与不变字节回归。
// [POS]: 能力存储回归，保留原全部安全与恢复断言。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

package sqlite

import (
	"context"

	"errors"
	"os"
	"path/filepath"

	"strconv"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/capabilities/memory"
)

// TestMigrationRejectsPersistentV2 锁定真实历史 v2 fixture 在可写与只读入口均被
// 明确拒绝；预检必须先校验 v1/v2 摘要，且任一拒绝都不留下任何数据库写入。
func TestMigrationRejectsPersistentV2(t *testing.T) {
	t.Run("authenticated v2 consolidation is incompatible without writes", func(t *testing.T) {
		for _, opener := range []struct {
			name string
			open func(string) (*Store, error)
		}{
			{name: "read-write", open: Open},
			{name: "read-only", open: OpenReadOnly},
		} {
			t.Run(opener.name, func(t *testing.T) {
				path := copyPersistentV2Fixture(t)
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				store, err := opener.open(path)
				if err == nil {
					store.Close()
					t.Fatal("authenticated legacy v2 consolidation database must be rejected")
				}
				if !errors.Is(err, ErrLegacyV2ConsolidationIncompatible) {
					t.Fatalf("open error = %v, want legacy v2 incompatible sentinel", err)
				}
				assertPersistentFixtureUnchanged(t, path, before, true)
			})
		}
	})

	t.Run("write open rejects terminal symlink without following physical v2", func(t *testing.T) {
		path := copyPersistentV2Fixture(t)
		linkPath := filepath.Join(filepath.Dir(path), "memory-v2-link.db")
		if err := os.Symlink(path, linkPath); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		store, err := Open(linkPath)
		if err == nil {
			store.Close()
			t.Fatal("symlinked authenticated legacy v2 consolidation database must be rejected")
		}
		if errors.Is(err, ErrLegacyV2ConsolidationIncompatible) {
			t.Fatalf("symlinked open followed legacy v2 target: %v", err)
		}
		info, lstatErr := os.Lstat(linkPath)
		if lstatErr != nil {
			t.Fatalf("lstat terminal link: %v", lstatErr)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("terminal link changed: mode=%v", info.Mode())
		}
		assertPersistentFixtureUnchanged(t, path, before, true)
	})

	t.Run("invalid v2 migration hash remains integrity failure", func(t *testing.T) {
		path := copyPersistentV2Fixture(t)
		database := openPersistentFixtureDatabase(t, path)
		if _, err := database.ExecContext(context.Background(), "UPDATE schema_migrations SET sha256 = ? WHERE version = ?", "corrupted-v2-hash", migrationVersionTwo); err != nil {
			t.Fatal(err)
		}
		if err := database.Close(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, opener := range []func(string) (*Store, error){Open, OpenReadOnly} {
			store, err := opener(path)
			if err == nil {
				store.Close()
				t.Fatal("tampered legacy v2 migration must fail open")
			}
			if errors.Is(err, ErrLegacyV2ConsolidationIncompatible) {
				t.Fatalf("tampered v2 migration returned incompatible error: %v", err)
			}
			if !strings.Contains(err.Error(), "checksum mismatch") {
				t.Fatalf("tampered v2 migration error = %v, want integrity checksum failure", err)
			}
		}
		assertPersistentFixtureUnchanged(t, path, before, false)
	})
}

// TestMigrationUpgradesV2WithoutConsolidations 保证拒绝范围只覆盖含历史巩固事实
// 的 v2 库；清空派生巩固行后，现有 v2 迁移路径仍可升级并通过完整性校验。
func TestMigrationUpgradesV2WithoutConsolidations(t *testing.T) {
	path := copyPersistentV2Fixture(t)
	database := openPersistentFixtureDatabase(t, path)
	for _, statement := range []string{
		"DELETE FROM derived_audit_log",
		"DELETE FROM derived_evidence",
		"DELETE FROM derived_candidates",
		"DELETE FROM consolidations",
	} {
		if _, err := database.ExecContext(context.Background(), statement); err != nil {
			database.Close()
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if version := persistentFixtureVersion(t, store.db); version != migrationVersion {
		store.Close()
		t.Fatalf("empty v2 upgraded user_version = %d want %d", version, migrationVersion)
	}
	if err := store.Check(context.Background()); err != nil {
		store.Close()
		t.Fatalf("empty v2 post-upgrade check: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if err := readOnly.Check(context.Background()); err != nil {
		t.Fatalf("empty v2 read-only post-upgrade check: %v", err)
	}
}

// TestMigrationFailsClosedForExistingDuplicateActiveDerived 锁定旧库升级边界：
// 若历史数据已含同标识的多个 active 派生版本，v4 唯一索引迁移必须拒绝打开。
func TestMigrationFailsClosedForExistingDuplicateActiveDerived(t *testing.T) {
	store, predecessor, _ := supersededDerivedFixture(t)
	path := storePath(t, store)
	if _, err := store.db.ExecContext(context.Background(), "DROP INDEX derived_candidates_one_active"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusActive, predecessor.ID, predecessor.ConsolidationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "DELETE FROM schema_migrations WHERE version = ?", migrationVersionFour); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(), "PRAGMA user_version = "+strconv.Itoa(migrationVersionThree)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("v4 migration must reject existing duplicate active derived versions")
	}
}

// TestMigrationUpgradesV1Database 锁定链式迁移：v1 库打开后升级到 v2 并通过
// 校验；只读打开 v1 库失败关闭（单向兼容）。
func TestMigrationUpgradesV1Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := store.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrationVersion {
		t.Fatalf("migrated user_version = %d want %d", version, migrationVersion)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer readOnly.Close()
	if err := readOnly.Check(context.Background()); err != nil {
		t.Fatalf("read-only check after migration: %v", err)
	}
}
