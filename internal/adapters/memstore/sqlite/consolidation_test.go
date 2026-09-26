package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

const (
	persistentV2FixtureName           = "memory-v2-lineage.db"
	persistentV2FixtureSHA256         = "1377a80a118cb563ebb4f212e8752956a30b6ce5149f51d323c105b872fec6c6"
	persistentV2SchemaVersion         = 2
	persistentV2ConsolidationCount    = 4
	persistentV2DerivedCandidateCount = 4
	persistentV2DerivedEvidenceCount  = 12
	persistentV2DerivedAuditCount     = 6
	persistentV2MigrationRecordCount  = 2
)

// persistentMigrationRecord 表示 fixture 或升级后库的一条已持久化迁移记录。
type persistentMigrationRecord struct {
	Version   int
	Name      string
	SHA256    string
	AppliedAt time.Time
}

// persistentColumnDefinition 表示 PRAGMA table_info 返回的非敏感列定义投影。
type persistentColumnDefinition struct {
	Type         string
	NotNull      bool
	DefaultValue sql.NullString
}

// consolidationStoreFixture 打开临时 Memory Store 并写入两条同事实输入候选。
func consolidationStoreFixture(t *testing.T) (*Store, memory.ConsolidationInputs, time.Time) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	workspaceHash := "sha256:" + consolidationHex(1)
	taskDigest := "sha256:" + consolidationHex(2)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	freezeStoreClock(t, store, now)
	inputs := memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []memory.Candidate{
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_a", now.Add(-2*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_b", now.Add(-1*time.Hour)),
	}}
	for _, candidate := range inputs.Candidates {
		if _, err := store.Capture(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
	}
	return store, inputs, now
}

// freezeStoreClock 为 Store 注入固定 UTC 时钟，保证到期和审计时间断言不依赖真实时间。
func freezeStoreClock(t *testing.T, store *Store, now time.Time) {
	t.Helper()
	store.now = func() time.Time {
		return now.UTC()
	}
}

// consolidationBatchFixture 打开含两个任务摘要、同一失败类别的输入集合，确保
// 一次确定性归纳同时产生 hypothesis 与 pattern 两个策略计划。
func consolidationBatchFixture(t *testing.T) (*Store, memory.ConsolidationInputs, time.Time) {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	workspaceHash := "sha256:" + consolidationHex(1)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	freezeStoreClock(t, store, now)
	inputs := memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []memory.Candidate{
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(2), "run_a", now.Add(-4*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(2), "run_b", now.Add(-3*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(4), "run_c", now.Add(-2*time.Hour)),
		consolidationInputCandidate(t, workspaceHash, "sha256:"+consolidationHex(4), "run_d", now.Add(-time.Hour)),
	}}
	for _, candidate := range inputs.Candidates {
		if _, err := store.Capture(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
	}
	return store, inputs, now
}

// storePath 返回测试 Store 当前主数据库的绝对路径，供关闭后重开耐久性断言使用。
func storePath(t *testing.T, store *Store) string {
	t.Helper()
	var sequence int
	var name, path string
	if err := store.db.QueryRowContext(context.Background(), "PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		t.Fatal(err)
	}
	return path
}

// consolidationInputCandidate 构造一条合法 run_outcome 输入候选。
func consolidationInputCandidate(t *testing.T, workspaceHash, taskDigest, runID string, createdAt time.Time) memory.Candidate {
	t.Helper()
	evidence := []memory.EventEvidenceRef{{RunID: runID, EventSeq: 3, EventHash: "sha256:" + consolidationHex(3), EventType: "run_failed"}}
	candidate, err := memory.NewRunOutcomeCandidate(workspaceHash, runID, taskDigest, "failed", "budget_turns", false, createdAt, memory.DefaultTTL, evidence, "")
	if err != nil {
		t.Fatalf("input candidate %s: %v", runID, err)
	}
	return candidate
}

// consolidatedDerivedFixture 写入一个确定性巩固计划，并返回其唯一派生候选供生命周期断言使用。
func consolidatedDerivedFixture(t *testing.T) (*Store, memory.ConsolidationInputs, time.Time, memory.PlannedConsolidation) {
	t.Helper()
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatal(err)
	}
	return store, inputs, now, plans[0]
}

// supersededDerivedFixture 生成同一派生标识的前后两个版本，供血缘完整性断言使用。
func supersededDerivedFixture(t *testing.T) (*Store, memory.DerivedCandidate, memory.DerivedCandidate) {
	t.Helper()
	store, inputs, now, first := consolidatedDerivedFixture(t)
	extra := consolidationInputCandidate(t, inputs.WorkspaceHash, inputs.Candidates[0].TaskPromptDigest, "run_c", now)
	if _, err := store.Capture(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	expanded := memory.ConsolidationInputs{
		WorkspaceHash: inputs.WorkspaceHash,
		Candidates:    append(append([]memory.Candidate(nil), inputs.Candidates...), extra),
	}
	plans, err := memory.DeriveConsolidations(expanded, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 1 {
		t.Fatalf("expanded plans = %#v err = %v", plans, err)
	}
	if plans[0].ConsolidationID == first.ConsolidationID {
		t.Fatal("expanded input set must produce a new consolidation")
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatal(err)
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, successor := range derived {
		if successor.SupersedesID == "" {
			continue
		}
		for _, predecessor := range derived {
			if predecessor.ID == successor.SupersedesID && predecessor.ConsolidationID == successor.SupersedesConsolidationID {
				return store, predecessor, successor
			}
		}
	}
	t.Fatal("missing complete derived predecessor/successor pair")
	return nil, memory.DerivedCandidate{}, memory.DerivedCandidate{}
}

// sameAnchorSuccessorFixture 生成同一 FactAnchor 的前后派生版本，并刻意选择字典序
// 更小的后继 consolidation ID，避免测试意外依赖哈希排序。
func sameAnchorSuccessorFixture(t *testing.T, commitOffset time.Duration) (*Store, memory.PlannedConsolidation, memory.PlannedConsolidation) {
	t.Helper()
	store, inputs, now, first := consolidatedDerivedFixture(t)
	const sameAnchorSearchLimit = 128
	var extra memory.Candidate
	var second memory.PlannedConsolidation
	for attempt := 0; attempt < sameAnchorSearchLimit; attempt++ {
		candidate := consolidationInputCandidate(t, inputs.WorkspaceHash, inputs.Candidates[0].TaskPromptDigest,
			"run_same_anchor_"+strconv.Itoa(attempt), now.Add(-time.Hour))
		expanded := memory.ConsolidationInputs{
			WorkspaceHash: inputs.WorkspaceHash,
			Candidates:    append(append([]memory.Candidate(nil), inputs.Candidates...), candidate),
		}
		plans, err := memory.DeriveConsolidations(expanded, memory.ConsolidationOptions{Now: now})
		if err != nil || len(plans) != 1 {
			t.Fatalf("same-anchor plans = %#v err = %v", plans, err)
		}
		if plans[0].ConsolidationID < first.ConsolidationID {
			extra = candidate
			second = plans[0]
			break
		}
	}
	if second.ConsolidationID == "" {
		t.Fatal("could not construct a successor that disproves consolidation ID ordering")
	}
	if !second.FactAnchor.Equal(first.FactAnchor) || second.Derived[0].ID != first.Derived[0].ID {
		t.Fatalf("same-anchor plans differ unexpectedly: first=%#v second=%#v", first, second)
	}
	if _, err := store.Capture(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	freezeStoreClock(t, store, now.Add(commitOffset))
	if _, err := store.Consolidate(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	return store, first, second
}

// consolidationHex 生成测试用 sha256 形式十六进制摘要。
func consolidationHex(seed int) string {
	const hexChars = "0123456789abcdef"
	result := make([]byte, 0, 64)
	for index := 0; index < 64; index++ {
		result = append(result, hexChars[(index+seed)%len(hexChars)])
	}
	return string(result)
}

// persistentV2FixturePath 返回冻结的历史 v2 fixture 的相对测试数据路径。
func persistentV2FixturePath() string {
	return filepath.Join("testdata", persistentV2FixtureName)
}

// copyPersistentV2Fixture 将冻结 fixture 复制到测试私有目录，并先校验其不可变摘要。
func copyPersistentV2Fixture(t *testing.T) string {
	t.Helper()
	contents, err := os.ReadFile(persistentV2FixturePath())
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	if actual := hex.EncodeToString(sum[:]); actual != persistentV2FixtureSHA256 {
		t.Fatalf("persistent v2 fixture sha256 = %s", actual)
	}
	path := filepath.Join(t.TempDir(), persistentV2FixtureName)
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// openPersistentFixtureDatabase 用原始 SQLite 连接读取或定向篡改测试私有 fixture。
func openPersistentFixtureDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return database
}

// persistentFixtureVersion 读取测试 fixture 当前记录的 SQLite schema 版本。
func persistentFixtureVersion(t *testing.T, database *sql.DB) int {
	t.Helper()
	var version int
	if err := database.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	return version
}

// persistentMigrationRecords 读取并解析 schema_migrations 的完整有序投影。
func persistentMigrationRecords(t *testing.T, database *sql.DB) []persistentMigrationRecord {
	t.Helper()
	rows, err := database.QueryContext(context.Background(), "SELECT version, name, sha256, applied_at FROM schema_migrations ORDER BY version ASC")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	records := make([]persistentMigrationRecord, 0)
	for rows.Next() {
		var record persistentMigrationRecord
		var appliedAt string
		if err := rows.Scan(&record.Version, &record.Name, &record.SHA256, &appliedAt); err != nil {
			t.Fatal(err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, appliedAt)
		if err != nil {
			t.Fatalf("parse migration %d applied_at: %v", record.Version, err)
		}
		record.AppliedAt = parsed
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return records
}

// persistentConsolidationColumns 读取 consolidations 的列约束投影，用于迁移断言。
func persistentConsolidationColumns(t *testing.T, database *sql.DB) map[string]persistentColumnDefinition {
	t.Helper()
	rows, err := database.QueryContext(context.Background(), "PRAGMA table_info(consolidations)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns := make(map[string]persistentColumnDefinition)
	for rows.Next() {
		var ordinal, primaryKey, notNull int
		var name string
		var definition persistentColumnDefinition
		if err := rows.Scan(&ordinal, &name, &definition.Type, &notNull, &definition.DefaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		definition.NotNull = notNull != 0
		columns[name] = definition
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return columns
}

// expectedPersistentV2Migrations 返回历史 fixture 应有的两条不可变迁移记录。
func expectedPersistentV2Migrations() []persistentMigrationRecord {
	return []persistentMigrationRecord{
		{Version: 1, Name: migrationName, SHA256: migrationHash},
		{Version: migrationVersionTwo, Name: migrationNameTwo, SHA256: migrationHashTwo},
	}
}

// assertPersistentV2Shape 验证 fixture 或失败回滚后的数据库仍是完整 v2，且没有
// v3 冻结支持列或 v4 partial index。checkHashes 为 false 时允许测试故意篡改 v2 摘要。
func assertPersistentV2Shape(t *testing.T, database *sql.DB, checkHashes bool) {
	t.Helper()
	if version := persistentFixtureVersion(t, database); version != persistentV2SchemaVersion {
		t.Fatalf("persistent v2 user_version = %d want %d", version, persistentV2SchemaVersion)
	}
	records := persistentMigrationRecords(t, database)
	if len(records) != persistentV2MigrationRecordCount {
		t.Fatalf("persistent v2 migration count = %d", len(records))
	}
	expected := expectedPersistentV2Migrations()
	for index := range expected {
		if records[index].Version != expected[index].Version || records[index].Name != expected[index].Name || records[index].AppliedAt.IsZero() {
			t.Fatalf("persistent v2 migration %d = %#v", index, records[index])
		}
		if checkHashes && records[index].SHA256 != expected[index].SHA256 {
			t.Fatalf("persistent v2 migration %d hash = %s", records[index].Version, records[index].SHA256)
		}
	}
	columns := persistentConsolidationColumns(t, database)
	if _, found := columns["min_pattern_support"]; found {
		t.Fatal("v2 fixture unexpectedly has min_pattern_support")
	}
	if _, found := columns["min_hypothesis_support"]; found {
		t.Fatal("v2 fixture unexpectedly has min_hypothesis_support")
	}
	var activeIndexCount, consolidationCount, derivedCount, evidenceCount, auditCount int
	if err := database.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?", "derived_candidates_one_active").Scan(&activeIndexCount); err != nil {
		t.Fatal(err)
	}
	if activeIndexCount != 0 {
		t.Fatal("v2 fixture unexpectedly has derived active unique index")
	}
	if err := database.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM consolidations").Scan(&consolidationCount); err != nil {
		t.Fatal(err)
	}
	if consolidationCount != persistentV2ConsolidationCount {
		t.Fatalf("persistent v2 consolidations = %d want %d", consolidationCount, persistentV2ConsolidationCount)
	}
	if err := database.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_candidates").Scan(&derivedCount); err != nil {
		t.Fatal(err)
	}
	if derivedCount != persistentV2DerivedCandidateCount {
		t.Fatalf("persistent v2 derived count = %d want %d", derivedCount, persistentV2DerivedCandidateCount)
	}
	if err := database.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_evidence").Scan(&evidenceCount); err != nil {
		t.Fatal(err)
	}
	if evidenceCount != persistentV2DerivedEvidenceCount {
		t.Fatalf("persistent v2 derived evidence = %d want %d", evidenceCount, persistentV2DerivedEvidenceCount)
	}
	if err := database.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_audit_log").Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if auditCount != persistentV2DerivedAuditCount {
		t.Fatalf("persistent v2 derived audit = %d want %d", auditCount, persistentV2DerivedAuditCount)
	}
}

// assertPersistentFixtureUnchanged 验证失败打开既不改写主数据库，也不遗留 SQLite
// journal、WAL 或 shared-memory 辅助文件，并保留完整的历史 v2 SQL 投影。
func assertPersistentFixtureUnchanged(t *testing.T, path string, before []byte, checkHashes bool) {
	t.Helper()
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, before) {
		t.Fatal("legacy v2 fixture bytes changed after rejected open")
	}
	for _, suffix := range []string{"-journal", "-shm", "-wal"} {
		if _, err := os.Stat(path + suffix); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("rejected legacy v2 open left SQLite sidecar %s: %v", suffix, err)
		}
	}
	database := openPersistentFixtureDatabase(t, path)
	defer database.Close()
	assertPersistentV2Shape(t, database, checkHashes)
}

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

// TestConsolidateCommitsPersistsAndRecomputes 锁定 M3 退出门的基线：事务提交、
// 重开库后审计耐久、Check 按输入重算派生事实一致。
func TestConsolidateCommitsPersistsAndRecomputes(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	committed := 0
	for _, plan := range plans {
		result, err := store.Consolidate(context.Background(), plan)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.SupersededVersion) != 0 {
			t.Fatalf("first consolidation must not supersede: %#v", result.SupersededVersion)
		}
		committed++
	}
	if committed == 0 {
		t.Fatal("expected at least one committed consolidation")
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatalf("post-consolidation check: %v", err)
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(derived) == 0 {
		t.Fatal("derived candidates missing after consolidation")
	}
	for _, candidate := range derived {
		if candidate.Status != memory.DerivedStatusActive {
			t.Fatalf("fresh derived candidate status = %s", candidate.Status)
		}
	}
}

// TestConsolidateIsIdempotentOnRetry 锁定幂等：同一计划重试返回等价结果且不
// 产生重复版本或重复审计。
func TestConsolidateIsIdempotentOnRetry(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
		again, err := store.Consolidate(context.Background(), plan)
		if err != nil {
			t.Fatalf("idempotent retry: %v", err)
		}
		if len(again.SupersededVersion) != 0 {
			t.Fatalf("retry must not supersede: %#v", again.SupersededVersion)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	perID := make(map[string]int)
	for _, candidate := range derived {
		perID[candidate.ID]++
	}
	for id, count := range perID {
		if count != 1 {
			t.Fatalf("derived candidate %s has %d versions after retry", id, count)
		}
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestConsolidateExistingPlanIsIdempotentAfterInputsExpire 锁定恢复边界：已完整
// 提交的同一计划即使其历史输入后来过期，仍按已存事实幂等返回，且不会新增
// consolidation、派生版本、间接证据或审计；从未提交的新计划仍由过期校验拒绝。
func TestConsolidateExistingPlanIsIdempotentAfterInputsExpire(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	plan := plans[0]
	if _, err := store.Consolidate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	var beforeConsolidations, beforeDerived, beforeEvidence, beforeAudit int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM consolidations").Scan(&beforeConsolidations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_candidates").Scan(&beforeDerived); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_evidence").Scan(&beforeEvidence); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_audit_log").Scan(&beforeAudit); err != nil {
		t.Fatal(err)
	}
	expiredAt := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET expires_at = ?", expiredAt); err != nil {
		t.Fatal(err)
	}
	again, err := store.Consolidate(context.Background(), plan)
	if err != nil {
		t.Fatalf("expired historical inputs must not break idempotent recovery: %v", err)
	}
	if again.Plan.ConsolidationID != plan.ConsolidationID || len(again.SupersededVersion) != 0 {
		t.Fatalf("idempotent recovery = %#v", again)
	}
	var afterConsolidations, afterDerived, afterEvidence, afterAudit int
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM consolidations").Scan(&afterConsolidations); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_candidates").Scan(&afterDerived); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_evidence").Scan(&afterEvidence); err != nil {
		t.Fatal(err)
	}
	if err := store.db.QueryRowContext(context.Background(), "SELECT COUNT(*) FROM derived_audit_log").Scan(&afterAudit); err != nil {
		t.Fatal(err)
	}
	if beforeConsolidations != afterConsolidations || beforeDerived != afterDerived || beforeEvidence != afterEvidence || beforeAudit != afterAudit {
		t.Fatalf("idempotent recovery changed rows: before=(%d,%d,%d,%d) after=(%d,%d,%d,%d)", beforeConsolidations, beforeDerived, beforeEvidence, beforeAudit, afterConsolidations, afterDerived, afterEvidence, afterAudit)
	}
}

// TestConsolidateSupersedesPreviousScopeVersion 锁定版本替代：输入集合扩大后
// 新一轮巩固把同 scope 旧版本置 superseded，替代链与审计完整。
func TestConsolidateSupersedesPreviousScopeVersion(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	first, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(first) == 0 {
		t.Fatalf("first plans = %#v err = %v", first, err)
	}
	for _, plan := range first {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	extra := consolidationInputCandidate(t, inputs.WorkspaceHash, inputs.Candidates[0].TaskPromptDigest, "run_c", now)
	if _, err := store.Capture(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	expanded := memory.ConsolidationInputs{WorkspaceHash: inputs.WorkspaceHash, Candidates: append(append([]memory.Candidate(nil), inputs.Candidates...), extra)}
	second, err := memory.DeriveConsolidations(expanded, memory.ConsolidationOptions{Now: now.Add(time.Hour)})
	if err != nil || len(second) == 0 {
		t.Fatalf("second plans = %#v err = %v", second, err)
	}
	for _, plan := range second {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active := make(map[string]int)
	superseded := make(map[string]int)
	for _, candidate := range derived {
		switch candidate.Status {
		case memory.DerivedStatusActive:
			active[candidate.ID]++
		case memory.DerivedStatusSuperseded:
			superseded[candidate.ID]++
		}
	}
	if len(active) == 0 {
		t.Fatal("no active derived version after superseding round")
	}
	for id := range active {
		if active[id] != 1 {
			t.Fatalf("derived candidate %s has %d active versions", id, active[id])
		}
	}
	if len(superseded) == 0 {
		t.Fatal("expected at least one superseded version after the second round")
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestInspectDerivedOrdersByCommittedVersion 锁定 InspectDerived 的版本选择：同一
// FactAnchor 的后继不能由 consolidation ID 哈希决定，而必须按真实提交顺序；
// 提交时间相同时以 consolidated 审计的写入顺序选择后继。
func TestInspectDerivedOrdersByCommittedVersion(t *testing.T) {
	t.Run("same second fractional successor", func(t *testing.T) {
		store, first, second := sameAnchorSuccessorFixture(t, 100*time.Millisecond)
		inspected, err := store.InspectDerived(context.Background(), first.Derived[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Candidate.ConsolidationID != second.ConsolidationID || inspected.Candidate.Status != memory.DerivedStatusActive || !inspected.Current || !inspected.Eligible {
			t.Fatalf("fractional-committed-at inspect = %#v", inspected)
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("later committed at", func(t *testing.T) {
		store, first, second := sameAnchorSuccessorFixture(t, time.Second)
		inspected, err := store.InspectDerived(context.Background(), first.Derived[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Candidate.ConsolidationID != second.ConsolidationID || inspected.Candidate.Status != memory.DerivedStatusActive || !inspected.Current || !inspected.Eligible {
			t.Fatalf("later-committed inspect = %#v", inspected)
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("same committed at", func(t *testing.T) {
		store, first, second := sameAnchorSuccessorFixture(t, 0)
		inspected, err := store.InspectDerived(context.Background(), first.Derived[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Candidate.ConsolidationID != second.ConsolidationID || inspected.Candidate.Status != memory.DerivedStatusActive || !inspected.Current || !inspected.Eligible {
			t.Fatalf("same-committed-at inspect = %#v", inspected)
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestExplicitSourceLifecycleClosesCurrentDerived 锁定来源撤销和替代的原子级联：
// 当前派生版本必须在同一事务内关闭为 rejected/insufficient_evidence，重复操作
// 不能追加审计，重开后 Check 仍可验证历史。
func TestExplicitSourceLifecycleClosesCurrentDerived(t *testing.T) {
	t.Run("revoke", func(t *testing.T) {
		store, inputs, now, plan := consolidatedDerivedFixture(t)
		if _, err := store.Revoke(context.Background(), inputs.Candidates[0].ID, memory.RevokeEvidenceInvalid, now); err != nil {
			t.Fatal(err)
		}
		inspected, err := store.InspectDerived(context.Background(), plan.Derived[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Candidate.Status != memory.DerivedStatusRejected || inspected.Current || inspected.Eligible {
			t.Fatalf("revoked-source derived projection = %#v", inspected)
		}
		if len(inspected.Audit) != 2 || inspected.Audit[1].Action != memory.DerivedAuditRejected || inspected.Audit[1].Reason != memory.RejectInsufficientEvidence {
			t.Fatalf("revoked-source derived audit = %#v", inspected.Audit)
		}
		if _, err := store.Revoke(context.Background(), inputs.Candidates[0].ID, memory.RevokeEvidenceInvalid, now.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		repeated, err := store.InspectDerived(context.Background(), plan.Derived[0].ID)
		if err != nil || len(repeated.Audit) != len(inspected.Audit) {
			t.Fatalf("idempotent source revoke audit = %#v err = %v", repeated.Audit, err)
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
		path := storePath(t, store)
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		reopened, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer reopened.Close()
		freezeStoreClock(t, reopened, now)
		if err := reopened.Check(context.Background()); err != nil {
			t.Fatalf("check after reopen = %v", err)
		}
	})

	t.Run("supersede", func(t *testing.T) {
		store, inputs, now, plan := consolidatedDerivedFixture(t)
		replacement := consolidationInputCandidate(t, inputs.WorkspaceHash, inputs.Candidates[0].TaskPromptDigest, "run_replacement", now.Add(time.Minute))
		replacement.SupersedesID = inputs.Candidates[0].ID
		if err := replacement.Validate(); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Capture(context.Background(), replacement); err != nil {
			t.Fatal(err)
		}
		inspected, err := store.InspectDerived(context.Background(), plan.Derived[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Candidate.Status != memory.DerivedStatusRejected || inspected.Current || inspected.Eligible {
			t.Fatalf("superseded-source derived projection = %#v", inspected)
		}
		if len(inspected.Audit) != 2 || inspected.Audit[1].Action != memory.DerivedAuditRejected || inspected.Audit[1].Reason != memory.RejectInsufficientEvidence {
			t.Fatalf("superseded-source derived audit = %#v", inspected.Audit)
		}
		if _, err := store.Capture(context.Background(), replacement); err != nil {
			t.Fatal(err)
		}
		repeated, err := store.InspectDerived(context.Background(), plan.Derived[0].ID)
		if err != nil || len(repeated.Audit) != len(inspected.Audit) {
			t.Fatalf("idempotent source supersede audit = %#v err = %v", repeated.Audit, err)
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestNaturalExpiryRemainsHistoricalButNotCurrent 锁定自然到期：候选行和审计
// 仍可供 Check 验证，但来源或派生到期后 Inspect 必须明确为非当前、非 eligible，
// 且拒绝写入不得把它们重新当作 active。
func TestNaturalExpiryRemainsHistoricalButNotCurrent(t *testing.T) {
	t.Run("source expiry", func(t *testing.T) {
		store, inputs, _, plan := consolidatedDerivedFixture(t)
		freezeStoreClock(t, store, inputs.Candidates[0].ExpiresAt.Add(time.Second))
		inspected, err := store.InspectDerived(context.Background(), plan.Derived[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Candidate.Status != memory.DerivedStatusActive || inspected.Current || inspected.Eligible {
			t.Fatalf("source-expired derived projection = %#v", inspected)
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatalf("source natural expiry must remain checkable: %v", err)
		}
	})

	t.Run("derived expiry", func(t *testing.T) {
		store, _, _, plan := consolidatedDerivedFixture(t)
		freezeStoreClock(t, store, plan.Derived[0].ExpiresAt.Add(time.Second))
		inspected, err := store.InspectDerived(context.Background(), plan.Derived[0].ID)
		if err != nil {
			t.Fatal(err)
		}
		if inspected.Candidate.Status != memory.DerivedStatusActive || inspected.Current || inspected.Eligible {
			t.Fatalf("derived-expired projection = %#v", inspected)
		}
		if _, err := store.RejectDerived(context.Background(), plan.Derived[0].ID, memory.RejectHumanRequest, time.Time{}); err == nil {
			t.Fatal("expired derived candidate must not be rejected as current")
		}
		if err := store.Check(context.Background()); err != nil {
			t.Fatalf("derived natural expiry must remain checkable: %v", err)
		}
	})
}

// TestCheckRejectsInvalidDerivedLineage 锁定完整复合 predecessor key、逆边和
// 单 active 版本的不变量；任何孤儿、自指向错误版本或重复 active 都必须失败关闭。
func TestCheckRejectsInvalidDerivedLineage(t *testing.T) {
	t.Run("orphan predecessor", func(t *testing.T) {
		store, _, successor := supersededDerivedFixture(t)
		if _, err := store.db.ExecContext(context.Background(), `UPDATE derived_candidates
			SET supersedes_id = NULL, supersedes_consolidation_id = NULL
			WHERE id = ? AND consolidation_id = ?`, successor.ID, successor.ConsolidationID); err != nil {
			t.Fatal(err)
		}
		if err := store.Check(context.Background()); err == nil {
			t.Fatal("orphan predecessor must fail Check")
		}
	})

	t.Run("wrong composite predecessor", func(t *testing.T) {
		store, _, successor := supersededDerivedFixture(t)
		if _, err := store.db.ExecContext(context.Background(), `UPDATE derived_candidates
			SET supersedes_consolidation_id = ? WHERE id = ? AND consolidation_id = ?`,
			successor.ConsolidationID, successor.ID, successor.ConsolidationID); err != nil {
			t.Fatal(err)
		}
		if err := store.Check(context.Background()); err == nil {
			t.Fatal("wrong composite predecessor must fail Check")
		}
	})

	t.Run("duplicate active", func(t *testing.T) {
		store, predecessor, _ := supersededDerivedFixture(t)
		if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusActive, predecessor.ID, predecessor.ConsolidationID); err == nil {
			t.Fatal("unique active version index must reject duplicate active versions")
		}
		if _, err := store.db.ExecContext(context.Background(), "DROP INDEX derived_candidates_one_active"); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET status = ? WHERE id = ? AND consolidation_id = ?", memory.DerivedStatusActive, predecessor.ID, predecessor.ConsolidationID); err != nil {
			t.Fatal(err)
		}
		if err := store.Check(context.Background()); err == nil {
			t.Fatal("duplicate active versions must fail Check when legacy index is absent")
		}
	})
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

// TestRejectDerivedIsIdempotentAndClosed 锁定显式拒绝：封闭理由、幂等重拒与
// 非法理由失败关闭。
func TestRejectDerivedIsIdempotentAndClosed(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil || len(derived) == 0 {
		t.Fatalf("derived = %#v err = %v", derived, err)
	}
	target := derived[0]
	if _, err := store.RejectDerived(context.Background(), target.ID, "because_i_say_so", now); err == nil {
		t.Fatal("invalid rejection reason must fail closed")
	}
	key, err := store.RejectDerived(context.Background(), target.ID, memory.RejectHumanRequest, now)
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != target.ID {
		t.Fatalf("rejected key = %#v", key)
	}
	repeat, err := store.RejectDerived(context.Background(), target.ID, memory.RejectHumanRequest, now.Add(time.Minute))
	if err != nil || repeat != key {
		t.Fatalf("idempotent rejection = %#v err = %v", repeat, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestConsolidateRollsBackFullyOnFailure 锁定两层失败关闭：计划自洽校验在事务
// 前拒绝损坏计划；临时触发器在事务中途注入失败后，巩固与派生零残留。
func TestConsolidateRollsBackFullyOnFailure(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	corrupted := plans[0]
	corrupted.Derived = append(corrupted.Derived, memory.DerivedCandidate{ID: "pat_foreignbody", SchemaVersion: memory.ConsolidationSchemaVersion, Kind: memory.KindPattern, ConsolidationID: "cons_other"})
	if _, err := store.Consolidate(context.Background(), corrupted); err == nil {
		t.Fatal("inconsistent plan must fail closed")
	}
	assertNoConsolidationResidue(t, store)
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER fail_derived_evidence BEFORE INSERT ON derived_evidence BEGIN SELECT RAISE(ABORT, 'injected failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err == nil {
		t.Fatal("mid-transaction failure must roll back the consolidation")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER fail_derived_evidence"); err != nil {
		t.Fatal(err)
	}
	assertNoConsolidationResidue(t, store)
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatalf("retry after rollback must succeed: %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestConsolidateRejectsInputsThatDriftBeforeWrite 锁定事务内输入复核：计划的
// 证据不得越出 InputIDs，候选在计划和写入之间若撤销或过期，也必须拒绝且
// 不产生 consolidation、derived、evidence 或 audit 残留。
func TestConsolidateRejectsInputsThatDriftBeforeWrite(t *testing.T) {
	t.Run("evidence outside inputs", func(t *testing.T) {
		store, inputs, now := consolidationStoreFixture(t)
		plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
		if err != nil || len(plans) == 0 {
			t.Fatalf("plans = %#v err = %v", plans, err)
		}
		plan := plans[0]
		outsideID := memory.CandidateID(inputs.WorkspaceHash, "run_outside")
		plan.Derived[0].Evidence[0].CandidateID = outsideID
		sort.Slice(plan.Derived[0].Evidence, func(left, right int) bool {
			return plan.Derived[0].Evidence[left].CandidateID < plan.Derived[0].Evidence[right].CandidateID
		})
		if _, err := store.Consolidate(context.Background(), plan); err == nil {
			t.Fatal("evidence outside input IDs must fail closed")
		}
		assertNoConsolidationResidue(t, store)
	})
	t.Run("revoked input", func(t *testing.T) {
		store, inputs, now := consolidationStoreFixture(t)
		plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
		if err != nil || len(plans) == 0 {
			t.Fatalf("plans = %#v err = %v", plans, err)
		}
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET status = ? WHERE id = ?", memory.StatusRevoked, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Consolidate(context.Background(), plans[0]); err == nil {
			t.Fatal("revoked input must fail closed")
		}
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET status = ? WHERE id = ?", memory.StatusActive, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		assertNoConsolidationResidue(t, store)
	})
	t.Run("expired input", func(t *testing.T) {
		store, inputs, now := consolidationStoreFixture(t)
		plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
		if err != nil || len(plans) == 0 {
			t.Fatalf("plans = %#v err = %v", plans, err)
		}
		expiredAt := now.Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET expires_at = ? WHERE id = ?", expiredAt, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Consolidate(context.Background(), plans[0]); err == nil {
			t.Fatal("expired input must fail closed")
		}
		restoredAt := inputs.Candidates[0].ExpiresAt.UTC().Format(time.RFC3339Nano)
		if _, err := store.db.ExecContext(context.Background(), "UPDATE candidates SET expires_at = ? WHERE id = ?", restoredAt, inputs.Candidates[0].ID); err != nil {
			t.Fatal(err)
		}
		assertNoConsolidationResidue(t, store)
	})
}

// TestConsolidateBatchRollsBackEarlierPlan 锁定同一应用调用的多策略原子性：
// 第二个策略触发器失败时，第一个策略写入的所有行也必须随事务整体回滚。
func TestConsolidateBatchRollsBackEarlierPlan(t *testing.T) {
	store, inputs, now := consolidationBatchFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) != 2 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	if plans[0].Strategy != memory.StrategyFailureClassRecurrence || plans[1].Strategy != memory.StrategyTaskOutcomeStability {
		t.Fatalf("unexpected strategy order: %#v", plans)
	}
	if _, err := store.db.ExecContext(context.Background(), `CREATE TEMP TRIGGER fail_second_consolidation
		BEFORE INSERT ON derived_candidates WHEN NEW.kind = 'pattern'
		BEGIN SELECT RAISE(ABORT, 'injected second-plan failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConsolidateBatch(context.Background(), plans); err == nil {
		t.Fatal("second plan failure must roll back the full batch")
	}
	if _, err := store.db.ExecContext(context.Background(), "DROP TRIGGER fail_second_consolidation"); err != nil {
		t.Fatal(err)
	}
	assertNoConsolidationResidue(t, store)
}

// TestConsolidateSeparatesFactAnchorFromCommitAuditTime 锁定来源事实时间与真实
// 写入时间的职责分离：输入历史时间只锚定派生候选，consolidations.committed_at
// 和 consolidated 审计必须等于当前事务时间且不能回填该锚点。
func TestConsolidateSeparatesFactAnchorFromCommitAuditTime(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatal(err)
	}
	consolidations, err := store.ListConsolidations(context.Background())
	if err != nil || len(consolidations) != 1 {
		t.Fatalf("consolidations = %#v err = %v", consolidations, err)
	}
	stored := consolidations[0]
	if !stored.CommittedAt.After(plans[0].FactAnchor) || stored.CommittedAt.Equal(plans[0].FactAnchor) {
		t.Fatalf("committed at = %s fact anchor = %s", stored.CommittedAt, plans[0].FactAnchor)
	}
	inspected, err := store.InspectDerived(context.Background(), plans[0].Derived[0].ID)
	if err != nil || len(inspected.Audit) != 1 {
		t.Fatalf("derived audit = %#v err = %v", inspected.Audit, err)
	}
	if !inspected.Audit[0].OccurredAt.Equal(stored.CommittedAt) || inspected.Audit[0].OccurredAt.Equal(plans[0].FactAnchor) {
		t.Fatalf("audit time = %s committed = %s anchor = %s", inspected.Audit[0].OccurredAt, stored.CommittedAt, plans[0].FactAnchor)
	}
}

// TestConsolidatePersistsFrozenSupportForRecompute 锁定非默认最小支持度随计划
// 入库；重开后 Check 必须使用冻结值重算，不能退回编译期默认阈值。
func TestConsolidatePersistsFrozenSupportForRecompute(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	extra := consolidationInputCandidate(t, inputs.WorkspaceHash, inputs.Candidates[0].TaskPromptDigest, "run_c", now)
	if _, err := store.Capture(context.Background(), extra); err != nil {
		t.Fatal(err)
	}
	inputs.Candidates = append(inputs.Candidates, extra)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now.Add(time.Hour), MinPatternSupport: 3, MinHypothesisSupport: 3})
	if err != nil || len(plans) != 1 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	if _, err := store.Consolidate(context.Background(), plans[0]); err != nil {
		t.Fatal(err)
	}
	consolidations, err := store.ListConsolidations(context.Background())
	if err != nil || len(consolidations) != 1 {
		t.Fatalf("consolidations = %#v err = %v", consolidations, err)
	}
	if consolidations[0].MinPatternSupport != 3 || consolidations[0].MinHypothesisSupport != 3 {
		t.Fatalf("frozen supports = %#v", consolidations[0])
	}
	path := storePath(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.Check(context.Background()); err != nil {
		t.Fatalf("recompute with frozen supports: %v", err)
	}
}

// TestCheckDetectsTTLTampering 锁定保留期篡改：直接改写派生候选过期时间后，
// 重算校验必须失败关闭而不是信任存储 TTL。
func TestCheckDetectsTTLTampering(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET expires_at = ? WHERE id = (SELECT id FROM derived_candidates LIMIT 1)",
		now.Add(memory.MaxTTL).UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("tampered retention window must fail the recompute check")
	}
}

// TestConsolidateRejectsEmptyDerivedPlan 锁定空派生计划的失败关闭：
// 不因访问首个派生候选而 panic，返回封闭错误且数据库零残留。
func TestConsolidateRejectsEmptyDerivedPlan(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	empty := plans[0]
	empty.Derived = nil
	if _, err := store.Consolidate(context.Background(), empty); err == nil {
		t.Fatal("empty derived plan must fail closed")
	}
	assertNoConsolidationResidue(t, store)
}

// assertNoConsolidationResidue 断言数据库没有任何巩固或派生残留。
func assertNoConsolidationResidue(t *testing.T, store *Store) {
	t.Helper()
	consolidations, err := store.ListConsolidations(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(consolidations) != 0 {
		t.Fatalf("rolled back consolidation rows = %d", len(consolidations))
	}
	derived, err := store.ListDerived(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(derived) != 0 {
		t.Fatalf("rolled back derived rows = %d", len(derived))
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestCheckDetectsPayloadTampering 锁定重算校验：直接篡改派生载荷后 Check
// 必须失败关闭，而不是信任存储文本。
func TestCheckDetectsPayloadTampering(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(context.Background(), "UPDATE derived_candidates SET payload_json = ? WHERE id = (SELECT id FROM derived_candidates LIMIT 1)",
		`{"task_prompt_digest":"sha256:`+consolidationHex(9)+`","outcome":"completed","support":2,"trusted_verification":true,"first_created_at":"2026-08-25T10:00:00Z","last_created_at":"2026-08-25T11:00:00Z"}`); err != nil {
		t.Fatal(err)
	}
	if err := store.Check(context.Background()); err == nil {
		t.Fatal("tampered derived payload must fail the recompute check")
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

// TestConsolidationDurableAcrossReopen 锁定审计耐久：关闭并重开数据库后，
// 巩固行、派生版本与审计不变量完整。
func TestConsolidationDurableAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	workspaceHash := "sha256:" + consolidationHex(1)
	taskDigest := "sha256:" + consolidationHex(2)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	freezeStoreClock(t, store, now)
	inputs := memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: []memory.Candidate{
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_a", now.Add(-time.Hour)),
		consolidationInputCandidate(t, workspaceHash, taskDigest, "run_b", now),
	}}
	for _, candidate := range inputs.Candidates {
		if _, err := store.Capture(context.Background(), candidate); err != nil {
			t.Fatal(err)
		}
	}
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	for _, plan := range plans {
		if _, err := store.Consolidate(context.Background(), plan); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	freezeStoreClock(t, reopened, now)
	if err := reopened.Check(context.Background()); err != nil {
		t.Fatalf("durable audit check: %v", err)
	}
	consolidations, err := reopened.ListConsolidations(context.Background())
	if err != nil || len(consolidations) != len(plans) {
		t.Fatalf("durable consolidations = %#v err = %v", consolidations, err)
	}
}

// TestConflictOnSameConsolidationDifferentFacts 锁定冲突失败关闭：同一巩固
// 标识携带不同派生事实时拒绝而不是覆盖。
func TestConflictOnSameConsolidationDifferentFacts(t *testing.T) {
	store, inputs, now := consolidationStoreFixture(t)
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{Now: now})
	if err != nil || len(plans) == 0 {
		t.Fatalf("plans = %#v err = %v", plans, err)
	}
	plan := plans[0]
	if _, err := store.Consolidate(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	variant := plan
	for index := range variant.Derived {
		variant.Derived[index].ExpiresAt = variant.Derived[index].ExpiresAt.Add(time.Hour)
	}
	if _, err := store.Consolidate(context.Background(), variant); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("conflicting retry must fail with ErrConflict, got %v", err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}
