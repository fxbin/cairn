package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/fxbin/cairn/internal/memory"
)

// v5LegacyFixtureDDL 是最小化还原的 v5 形态 Memory 库结构：只包含与本迁移
// 交互的演化三表与版本元数据，其余情节记忆/巩固表由 migrate 的幂等基线段
// 自动补建。审计 reason 词表仍含 inconclusive_limit（v5 原文）。
const v5LegacyFixtureDDL = `
CREATE TABLE schema_migrations (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    sha256 TEXT NOT NULL,
    applied_at TEXT NOT NULL
);
CREATE TABLE evolution_candidates (
    id TEXT PRIMARY KEY,
    schema_version INTEGER NOT NULL,
    workspace_hash TEXT NOT NULL,
    source_derived_id TEXT NOT NULL,
    source_consolidation_id TEXT NOT NULL,
    target_kind TEXT NOT NULL CHECK(target_kind IN ('context_compaction_params')),
    variant_spec_sha256 TEXT NOT NULL,
    variant_fingerprint TEXT NOT NULL,
    baseline_fingerprint TEXT NOT NULL,
    eval_set_fingerprint TEXT,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    status TEXT NOT NULL CHECK(status IN ('draft', 'ready', 'evaluating', 'evaluated_passed', 'evaluated_failed', 'inconclusive', 'invalidated', 'promoted', 'rejected', 'expired')),
    note_digest TEXT,
    UNIQUE(source_derived_id, source_consolidation_id, target_kind, variant_fingerprint)
);
CREATE TABLE evolution_audit_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    candidate_id TEXT NOT NULL REFERENCES evolution_candidates(id),
    action TEXT NOT NULL CHECK(action IN ('created', 'rejected', 'invalidated', 'expired')),
    reason TEXT CHECK(reason IS NULL OR reason IN ('insufficient_evidence', 'evaluation_failed', 'safety_violation', 'inconclusive_limit', 'human_request')),
    occurred_at TEXT NOT NULL
);
CREATE TABLE evolution_cooldowns (
    source_derived_id TEXT NOT NULL,
    source_consolidation_id TEXT NOT NULL,
    target_kind TEXT NOT NULL,
    variant_fingerprint TEXT NOT NULL,
    cooldown_until TEXT NOT NULL,
    PRIMARY KEY(source_derived_id, source_consolidation_id, target_kind, variant_fingerprint)
);
`

// writeV5LegacyFixture 落一个 user_version=5 的历史库：五条迁移记录使用真实
// 哨兵哈希，演化三表为 v5 形态，并携带一条 draft 候选、一条 created 审计与
// 一条被 ADR-0012 收口的 inconclusive_limit 拒绝审计。
func writeV5LegacyFixture(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(v5LegacyFixtureDDL); err != nil {
		t.Fatal(err)
	}
	for version, hash := range map[int]string{
		1: migrationHash, migrationVersionTwo: migrationHashTwo, migrationVersionThree: migrationHashThree,
		migrationVersionFour: migrationHashFour, migrationVersionFive: migrationHashFive,
	} {
		if _, err := db.Exec("INSERT INTO schema_migrations (version, name, sha256, applied_at) VALUES (?, ?, ?, ?)", version, fmt.Sprintf("fixture-%d", version), hash, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			t.Fatal(err)
		}
	}
	candidateTime := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := db.Exec(`INSERT INTO evolution_candidates (id, schema_version, workspace_hash, source_derived_id, source_consolidation_id, target_kind, variant_spec_sha256, variant_fingerprint, baseline_fingerprint, eval_set_fingerprint, created_at, expires_at, status, note_digest) VALUES ('evo_fixture', ?, 'wh', 'd1', 'c1', 'context_compaction_params', ?, ?, ?, NULL, ?, ?, 'draft', NULL)`,
		memory.EvolutionSchemaVersion, "sha256:"+hexOf(1), "sha256:"+hexOf(2), "sha256:"+hexOf(3), candidateTime, candidateTime); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO evolution_audit_log (candidate_id, action, reason, occurred_at) VALUES ('evo_fixture', 'created', NULL, ?)", candidateTime); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO evolution_audit_log (candidate_id, action, reason, occurred_at) VALUES ('evo_fixture', 'rejected', 'inconclusive_limit', ?)", candidateTime); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", migrationVersionFive)); err != nil {
		t.Fatal(err)
	}
}

// hexOf 返回重复 n 字节的伪摘要，仅为满足 v5 列形态，不参与密码学语义。
func hexOf(n int) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 64)
	for i := range out {
		out[i] = digits[n%16]
	}
	return string(out)
}

// TestMigrationSixFailsClosedOnSupersededRejectionReason 验证 v6 迁移对存量
// 违禁拒绝理由的失败关闭：Open 必须失败、文件保持 v5 状态且两行审计原样
// 保留；人工仲裁删除该行后同一文件可完成升级。
func TestMigrationSixFailsClosedOnSupersededRejectionReason(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy-v5.db")
	writeV5LegacyFixture(t, path)

	if _, err := Open(path); err == nil {
		t.Fatal("opening a v5 store carrying an inconclusive_limit audit row must fail closed")
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	var version int
	if err := raw.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != migrationVersionFive {
		t.Fatalf("failed migration must leave user_version at %d, got %d", migrationVersionFive, version)
	}
	var retained int
	if err := raw.QueryRow("SELECT COUNT(*) FROM evolution_audit_log").Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained != 2 {
		t.Fatalf("both original audit rows must survive the rolled-back migration, got %d", retained)
	}
	raw.Close()

	raw, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec("DELETE FROM evolution_audit_log WHERE reason = 'inconclusive_limit'"); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	store, err := Open(path)
	if err != nil {
		t.Fatalf("arbitrated legacy store must complete migration: %v", err)
	}
	defer store.Close()
	var migrated int
	if err := store.db.QueryRowContext(context.Background(), "PRAGMA user_version").Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if migrated != migrationVersion {
		t.Fatalf("user_version after arbitration = %d, want %d", migrated, migrationVersion)
	}
}

// TestEvaluationRegistrySchemaAcceptsOnlyPreregisteredForms 锁定 v6 新表与
// 重建后的审计词表：评测类动作可写入，被收口的理由与新审计词写入一律被拒。
func TestEvaluationRegistrySchemaAcceptsOnlyPreregisteredForms(t *testing.T) {
	store, candidate, now := evolutionFixture(t)
	ctx := context.Background()
	if _, err := store.CreateEvolutionCandidate(ctx, candidate, now); err != nil {
		t.Fatalf("seed candidate: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO evolution_evaluations (evaluation_id, schema_version, candidate_id, eval_set_fingerprint, baseline_config_fingerprint, variant_config_fingerprint, comparator_version, status, suite_result_digest, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'running', NULL, ?, NULL)`,
		"sha256:"+hexOf(7), memory.EvolutionSchemaVersion, candidate.ID, validEvalDigest(), "sha256:"+hexOf(8), "sha256:"+hexOf(9), "cairn-comparator-v1", now.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("preregistered evaluation row must be accepted: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO evolution_evaluations (evaluation_id, schema_version, candidate_id, eval_set_fingerprint, baseline_config_fingerprint, variant_config_fingerprint, comparator_version, status, suite_result_digest, started_at, finished_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'planned', NULL, ?, NULL)`,
		"sha256:"+hexOf(10), memory.EvolutionSchemaVersion, candidate.ID, validEvalDigest(), "sha256:"+hexOf(11), "sha256:"+hexOf(12), "cairn-comparator-v1", now.UTC().Format(time.RFC3339Nano)); err == nil {
		t.Fatal("undeclared evaluation status must be rejected by the schema")
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO evolution_audit_log (candidate_id, action, reason, occurred_at) VALUES (?, 'evaluation_started', NULL, ?)`, candidate.ID, now.UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatalf("evaluation_started action must be accepted by the rebuilt lexicon: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `INSERT INTO evolution_audit_log (candidate_id, action, reason, occurred_at) VALUES (?, 'rejected', 'inconclusive_limit', ?)`, candidate.ID, now.UTC().Format(time.RFC3339Nano)); err == nil {
		t.Fatal("superseded rejection reason must stay rejected at the schema level")
	}
}

// validEvalDigest 返回测试用的形态合法摘要。
func validEvalDigest() string { return "sha256:" + hexOf(13) }
