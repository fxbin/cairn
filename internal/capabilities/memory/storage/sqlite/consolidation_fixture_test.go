// [INPUT]: 依赖真实SQLite与父能力结构化合同。
// [OUTPUT]: 提供真实巩固与历史v2库夹具。
// [POS]: 能力存储回归，保留原全部安全与恢复断言。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

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

	"strconv"

	"testing"
	"time"

	"github.com/fxbin/cairn/internal/capabilities/memory"
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
