// [INPUT]: 依赖原schema声明与本包查询端口。
// [OUTPUT]: 提供耐久性配置、迁移及不可变校验和。
// [POS]: 保持原数据库版本链与拒绝边界。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package sqlite 提供能力内的SQLite持久化叶子。
// Author: fxbin
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"time"
)

const (
	migrationVersion = 9
	migrationName    = "001_trusted_memory_v1"
	migrationHash    = "cairn-schema-v1-trusted-memory"
)

// migrate 配置耐久性选项并以迁移校验和初始化唯一支持的 schema 版本。
func (store *Store) migrate(ctx context.Context) error {
	for _, statement := range []string{"PRAGMA journal_mode = WAL", "PRAGMA synchronous = FULL", "PRAGMA foreign_keys = ON", "PRAGMA busy_timeout = 5000"} {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("configure memory SQLite: %w", err)
		}
	}
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start memory migration: %w", err)
	}
	defer transaction.Rollback()
	if _, err := transaction.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			applied_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS candidates (
			id TEXT PRIMARY KEY,
			schema_version INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK(kind = 'run_outcome'),
			workspace_hash TEXT NOT NULL,
			task_prompt_digest TEXT NOT NULL,
			run_id TEXT NOT NULL,
			outcome TEXT NOT NULL CHECK(outcome IN ('completed', 'failed', 'cancelled')),
			failure_class TEXT,
			trusted_verification INTEGER NOT NULL CHECK(trusted_verification IN (0, 1)),
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('active', 'revoked', 'superseded')),
			supersedes_id TEXT REFERENCES candidates(id),
			UNIQUE(workspace_hash, run_id)
		);
		CREATE TABLE IF NOT EXISTS evidence_refs (
			candidate_id TEXT NOT NULL REFERENCES candidates(id) ON DELETE CASCADE,
			ordinal INTEGER NOT NULL,
			run_id TEXT NOT NULL,
			event_seq INTEGER NOT NULL,
			event_hash TEXT NOT NULL,
			event_type TEXT NOT NULL,
			artifact_sha256 TEXT,
			PRIMARY KEY(candidate_id, ordinal)
		);
		CREATE TABLE IF NOT EXISTS audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			candidate_id TEXT NOT NULL REFERENCES candidates(id),
			action TEXT NOT NULL CHECK(action IN ('captured', 'revoked', 'superseded')),
			reason TEXT CHECK(reason IS NULL OR reason IN ('evidence_invalid', 'human_request')),
			occurred_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS candidates_recall ON candidates(workspace_hash, kind, status, expires_at, created_at DESC, id ASC);
		CREATE INDEX IF NOT EXISTS evidence_refs_by_event ON evidence_refs(run_id, event_seq, event_hash);
		CREATE INDEX IF NOT EXISTS audit_log_by_candidate ON audit_log(candidate_id, id);
		CREATE TABLE IF NOT EXISTS consolidations (
			id TEXT PRIMARY KEY,
			schema_version INTEGER NOT NULL,
			strategy TEXT NOT NULL CHECK(strategy IN ('task_outcome_stability', 'failure_class_recurrence')),
			workspace_hash TEXT NOT NULL,
			inputs_digest TEXT NOT NULL,
			input_ids TEXT NOT NULL,
			input_count INTEGER NOT NULL,
			committed_at TEXT NOT NULL,
			derived_ttl_seconds INTEGER NOT NULL CHECK(derived_ttl_seconds > 0),
			status TEXT NOT NULL CHECK(status IN ('committed'))
		);
		CREATE TABLE IF NOT EXISTS derived_candidates (
			id TEXT NOT NULL,
			schema_version INTEGER NOT NULL,
			kind TEXT NOT NULL CHECK(kind IN ('pattern', 'hypothesis')),
			consolidation_id TEXT NOT NULL REFERENCES consolidations(id),
			workspace_hash TEXT NOT NULL,
			scope_digest TEXT NOT NULL,
			payload_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			expires_at TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('active', 'rejected', 'superseded')),
			supersedes_id TEXT,
			supersedes_consolidation_id TEXT,
			PRIMARY KEY(id, consolidation_id),
			FOREIGN KEY (supersedes_id, supersedes_consolidation_id) REFERENCES derived_candidates(id, consolidation_id)
		);
		CREATE TABLE IF NOT EXISTS derived_evidence (
			derived_id TEXT NOT NULL,
			consolidation_id TEXT NOT NULL,
			ordinal INTEGER NOT NULL,
			source_candidate_id TEXT NOT NULL REFERENCES candidates(id),
			PRIMARY KEY(derived_id, consolidation_id, ordinal),
			FOREIGN KEY (derived_id, consolidation_id) REFERENCES derived_candidates(id, consolidation_id) ON DELETE CASCADE
		);
		CREATE TABLE IF NOT EXISTS derived_audit_log (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			derived_id TEXT NOT NULL,
			consolidation_id TEXT NOT NULL,
			action TEXT NOT NULL CHECK(action IN ('consolidated', 'rejected', 'superseded')),
			reason TEXT CHECK(reason IS NULL OR reason IN ('insufficient_evidence', 'human_request')),
			occurred_at TEXT NOT NULL,
			FOREIGN KEY (derived_id, consolidation_id) REFERENCES derived_candidates(id, consolidation_id)
		);
		CREATE INDEX IF NOT EXISTS consolidations_by_workspace ON consolidations(workspace_hash, id);
		CREATE INDEX IF NOT EXISTS derived_candidates_by_scope ON derived_candidates(workspace_hash, kind, scope_digest, status);
		CREATE INDEX IF NOT EXISTS derived_evidence_by_source ON derived_evidence(source_candidate_id);`); err != nil {
		return fmt.Errorf("create memory schema: %w", err)
	}
	var version int
	if err := transaction.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read memory schema version for migration: %w", err)
	}
	if version > migrationVersion || version < 0 {
		return fmt.Errorf("memory schema version %d cannot be migrated by this Cairn binary", version)
	}
	if version == 0 {
		if err := recordMigration(ctx, transaction, 1, migrationName, migrationHash); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, 1); err != nil {
			return err
		}
		version = 1
	} else if err := verifyAppliedMigration(ctx, transaction, 1, migrationHash); err != nil {
		return err
	}
	if version < migrationVersionTwo {
		if err := recordMigration(ctx, transaction, migrationVersionTwo, migrationNameTwo, migrationHashTwo); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionTwo); err != nil {
			return err
		}
		version = migrationVersionTwo
	} else if err := verifyAppliedMigration(ctx, transaction, migrationVersionTwo, migrationHashTwo); err != nil {
		return err
	}
	if version < migrationVersionThree {
		if _, err := transaction.ExecContext(ctx, `ALTER TABLE consolidations ADD COLUMN min_pattern_support INTEGER NOT NULL DEFAULT 2 CHECK(min_pattern_support >= 2)`); err != nil {
			return fmt.Errorf("add frozen pattern support to memory consolidations: %w", err)
		}
		if _, err := transaction.ExecContext(ctx, `ALTER TABLE consolidations ADD COLUMN min_hypothesis_support INTEGER NOT NULL DEFAULT 2 CHECK(min_hypothesis_support >= 2)`); err != nil {
			return fmt.Errorf("add frozen hypothesis support to memory consolidations: %w", err)
		}
		if err := recordMigration(ctx, transaction, migrationVersionThree, migrationNameThree, migrationHashThree); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionThree); err != nil {
			return err
		}
		version = migrationVersionThree
	}
	if version < migrationVersionFour {
		if _, err := transaction.ExecContext(ctx, `CREATE UNIQUE INDEX derived_candidates_one_active
			ON derived_candidates(id) WHERE status = 'active'`); err != nil {
			return fmt.Errorf("enforce unique active derived candidate version: %w", err)
		}
		if err := recordMigration(ctx, transaction, migrationVersionFour, migrationNameFour, migrationHashFour); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionFour); err != nil {
			return err
		}
		version = migrationVersionFour
	}
	if version < migrationVersionFive {
		if _, err := transaction.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS evolution_candidates (
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
			CREATE TABLE IF NOT EXISTS evolution_audit_log (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				candidate_id TEXT NOT NULL REFERENCES evolution_candidates(id),
				action TEXT NOT NULL CHECK(action IN ('created', 'rejected', 'invalidated', 'expired')),
				reason TEXT CHECK(reason IS NULL OR reason IN ('insufficient_evidence', 'evaluation_failed', 'safety_violation', 'inconclusive_limit', 'human_request')),
				occurred_at TEXT NOT NULL
			);
			CREATE INDEX IF NOT EXISTS evolution_audit_by_candidate ON evolution_audit_log(candidate_id, id);
			CREATE TABLE IF NOT EXISTS evolution_cooldowns (
				source_derived_id TEXT NOT NULL,
				source_consolidation_id TEXT NOT NULL,
				target_kind TEXT NOT NULL,
				variant_fingerprint TEXT NOT NULL,
				cooldown_until TEXT NOT NULL,
				PRIMARY KEY(source_derived_id, source_consolidation_id, target_kind, variant_fingerprint)
			);`); err != nil {
			return fmt.Errorf("create evolution registry schema: %w", err)
		}
		if err := recordMigration(ctx, transaction, migrationVersionFive, migrationNameFive, migrationHashFive); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionFive); err != nil {
			return err
		}
	}
	if version < migrationVersionSix {
		// v6 评测注册迁移。SQLite 不能修改表内嵌 CHECK，审计词表的扩展
		// （新增评测类动作）与拒绝理由收口（按 ADR-0012 §5.5 移除
		// inconclusive_limit）必须整表重建：新建影子表 → 显式列复制 →
		// 换名启用。若旧库已存在 reason='inconclusive_limit' 的审计行，
		// 复制阶段会触发新 CHECK 使整个迁移失败关闭——审计行不可变，
		// 不允许静默改写历史理由；处理方式是由维护者人工仲裁该行后
		// 手工归档再重开。
		if _, err := transaction.ExecContext(ctx, `
			CREATE TABLE evolution_evaluations (
				evaluation_id TEXT PRIMARY KEY,
				schema_version INTEGER NOT NULL,
				candidate_id TEXT NOT NULL UNIQUE REFERENCES evolution_candidates(id),
				eval_set_fingerprint TEXT NOT NULL,
				baseline_config_fingerprint TEXT NOT NULL,
				variant_config_fingerprint TEXT NOT NULL,
				comparator_version TEXT NOT NULL,
				status TEXT NOT NULL CHECK(status IN ('running', 'passed', 'failed', 'inconclusive', 'invalid')),
				suite_result_digest TEXT,
				started_at TEXT NOT NULL,
				finished_at TEXT
			);
			CREATE INDEX IF NOT EXISTS evolution_evaluations_by_candidate ON evolution_evaluations(candidate_id, evaluation_id);
			DROP INDEX IF EXISTS evolution_audit_by_candidate;
			CREATE TABLE evolution_audit_log_v6_rebuild (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				candidate_id TEXT NOT NULL REFERENCES evolution_candidates(id),
				action TEXT NOT NULL CHECK(action IN ('created', 'rejected', 'invalidated', 'expired', 'evaluation_started', 'evaluation_passed', 'evaluation_failed', 'evaluation_inconclusive')),
				reason TEXT CHECK(reason IS NULL OR reason IN ('insufficient_evidence', 'evaluation_failed', 'safety_violation', 'human_request')),
				occurred_at TEXT NOT NULL
			);
			INSERT INTO evolution_audit_log_v6_rebuild (id, candidate_id, action, reason, occurred_at)
				SELECT id, candidate_id, action, reason, occurred_at FROM evolution_audit_log;
			DROP TABLE evolution_audit_log;
			ALTER TABLE evolution_audit_log_v6_rebuild RENAME TO evolution_audit_log;
			CREATE INDEX IF NOT EXISTS evolution_audit_by_candidate ON evolution_audit_log(candidate_id, id);`); err != nil {
			return fmt.Errorf("migrate memory schema to evaluation registry: %w", err)
		}
		if err := recordMigration(ctx, transaction, migrationVersionSix, migrationNameSix, migrationHashSix); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionSix); err != nil {
			return err
		}
	}
	if version < migrationVersionSeven {
		// v7 晋升版本账本迁移。审计词表新增晋升与回退两动作，须再次整表
		// 重建 evolution_audit_log（SQLite 不能修改表内嵌 CHECK）；本次为
		// 纯扩词表，存量行不触发新约束，复制阶段无失败关闭面。两账本表：
		// promotions 承载版本记录正文（同候选唯一、同目标类型版本唯一），
		// activations 承载每目标类型至多一行的激活指针（行缺=产品基线）。
		if _, err := transaction.ExecContext(ctx, `
			DROP INDEX IF EXISTS evolution_audit_by_candidate;
			CREATE TABLE evolution_audit_log_v7_rebuild (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				candidate_id TEXT NOT NULL REFERENCES evolution_candidates(id),
				action TEXT NOT NULL CHECK(action IN ('created', 'rejected', 'invalidated', 'expired', 'evaluation_started', 'evaluation_passed', 'evaluation_failed', 'evaluation_inconclusive', 'promoted', 'promotion_rolled_back')),
				reason TEXT CHECK(reason IS NULL OR reason IN ('insufficient_evidence', 'evaluation_failed', 'safety_violation', 'human_request')),
				occurred_at TEXT NOT NULL
			);
			INSERT INTO evolution_audit_log_v7_rebuild (id, candidate_id, action, reason, occurred_at)
				SELECT id, candidate_id, action, reason, occurred_at FROM evolution_audit_log;
			DROP TABLE evolution_audit_log;
			ALTER TABLE evolution_audit_log_v7_rebuild RENAME TO evolution_audit_log;
			CREATE INDEX IF NOT EXISTS evolution_audit_by_candidate ON evolution_audit_log(candidate_id, id);
			CREATE TABLE evolution_promotions (
				promotion_id TEXT PRIMARY KEY,
				schema_version INTEGER NOT NULL,
				candidate_id TEXT NOT NULL UNIQUE REFERENCES evolution_candidates(id),
				target_kind TEXT NOT NULL,
				version INTEGER NOT NULL,
				variant_json TEXT NOT NULL,
				variant_fingerprint TEXT NOT NULL,
				variant_spec_sha256 TEXT NOT NULL,
				baseline_fingerprint TEXT NOT NULL,
				eval_set_fingerprint TEXT NOT NULL,
				suite_result_digest TEXT NOT NULL,
				comparator_version TEXT NOT NULL,
				conclusion TEXT NOT NULL,
				conclusion_trigger TEXT NOT NULL,
				metrics_json TEXT NOT NULL,
				approver TEXT NOT NULL,
				rollback_target_promotion_id TEXT,
				status TEXT NOT NULL CHECK(status IN ('active', 'rolled_back')),
				created_at TEXT NOT NULL,
				rolled_back_at TEXT,
				UNIQUE(target_kind, version)
			);
			CREATE INDEX IF NOT EXISTS evolution_promotions_by_target ON evolution_promotions(target_kind, version);
			CREATE TABLE evolution_activations (
				target_kind TEXT PRIMARY KEY,
				promotion_id TEXT NOT NULL REFERENCES evolution_promotions(promotion_id),
				activated_at TEXT NOT NULL
			);`); err != nil {
			return fmt.Errorf("create promotion ledger schema: %w", err)
		}
		if err := recordMigration(ctx, transaction, migrationVersionSeven, migrationNameSeven, migrationHashSeven); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionSeven); err != nil {
			return err
		}
	}
	if version < migrationVersionEight {
		var promotionCount int
		if err := transaction.QueryRowContext(ctx, "SELECT COUNT(*) FROM evolution_promotions").Scan(&promotionCount); err != nil {
			return fmt.Errorf("count legacy promotion ledger: %w", err)
		}
		if promotionCount != 0 {
			return errors.New("v7 promotion ledger is incompatible with the staged activation contract; rebuild the development memory store")
		}
		if _, err := transaction.ExecContext(ctx, `
			DROP TABLE evolution_activations;
			DROP TABLE evolution_promotions;
			DROP INDEX IF EXISTS evolution_audit_by_candidate;
			CREATE TABLE evolution_audit_log_v8_rebuild (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				candidate_id TEXT NOT NULL REFERENCES evolution_candidates(id),
				action TEXT NOT NULL CHECK(action IN ('created', 'rejected', 'invalidated', 'expired', 'evaluation_started', 'evaluation_passed', 'evaluation_failed', 'evaluation_inconclusive', 'promoted', 'sandbox_activated', 'promotion_rolled_back')),
				reason TEXT CHECK((action IN ('rejected', 'promotion_rolled_back') AND reason IS NOT NULL AND reason IN ('insufficient_evidence', 'evaluation_failed', 'safety_violation', 'human_request')) OR (action NOT IN ('rejected', 'promotion_rolled_back') AND reason IS NULL)),
				occurred_at TEXT NOT NULL
			);
			INSERT INTO evolution_audit_log_v8_rebuild (id, candidate_id, action, reason, occurred_at)
				SELECT id, candidate_id, action, reason, occurred_at FROM evolution_audit_log;
			DROP TABLE evolution_audit_log;
			ALTER TABLE evolution_audit_log_v8_rebuild RENAME TO evolution_audit_log;
			CREATE INDEX evolution_audit_by_candidate ON evolution_audit_log(candidate_id, id);
			CREATE TABLE evolution_promotion_decisions (
				decision_id TEXT PRIMARY KEY,
				schema_version INTEGER NOT NULL,
				candidate_id TEXT NOT NULL UNIQUE REFERENCES evolution_candidates(id),
				evaluation_id TEXT NOT NULL UNIQUE REFERENCES evolution_evaluations(evaluation_id),
				target_kind TEXT NOT NULL CHECK(target_kind IN ('context_compaction_params')),
				variant_fingerprint TEXT NOT NULL,
				variant_spec_sha256 TEXT NOT NULL,
				baseline_fingerprint TEXT NOT NULL,
				eval_set_fingerprint TEXT NOT NULL,
				suite_result_digest TEXT NOT NULL,
				comparator_version TEXT NOT NULL,
				conclusion_trigger TEXT NOT NULL,
				metrics_json TEXT NOT NULL,
				rollback_target_promotion_id TEXT,
				approval_fingerprint TEXT NOT NULL,
				approval_nonce TEXT NOT NULL UNIQUE,
				approver TEXT NOT NULL,
				decided_at TEXT NOT NULL
			);
			CREATE TABLE evolution_promotions (
				promotion_id TEXT PRIMARY KEY,
				schema_version INTEGER NOT NULL,
				decision_id TEXT NOT NULL UNIQUE REFERENCES evolution_promotion_decisions(decision_id),
				target_kind TEXT NOT NULL CHECK(target_kind IN ('context_compaction_params')),
				version INTEGER NOT NULL,
				variant_json TEXT NOT NULL,
				status TEXT NOT NULL CHECK(status IN ('promoted_inactive', 'sandbox_activated', 'revoked')),
				created_at TEXT NOT NULL,
				revoked_at TEXT,
				UNIQUE(target_kind, version)
			);
			CREATE INDEX evolution_promotions_by_target ON evolution_promotions(target_kind, version);
			CREATE TABLE evolution_activations (
				environment TEXT NOT NULL CHECK(environment = 'sandbox'),
				target_kind TEXT NOT NULL CHECK(target_kind IN ('context_compaction_params')),
				promotion_id TEXT NOT NULL REFERENCES evolution_promotions(promotion_id),
				previous_promotion_id TEXT,
				activated_at TEXT NOT NULL,
				PRIMARY KEY(environment, target_kind)
			);`); err != nil {
			return fmt.Errorf("replace promotion ledger with staged activation schema: %w", err)
		}
		if err := recordMigration(ctx, transaction, migrationVersionEight, migrationNameEight, migrationHashEight); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionEight); err != nil {
			return err
		}
	}
	if version < migrationVersionNine {
		// v9 召回遥测采集表：每次 explain 召回落一行聚合事实。零正文——
		// 路线图可观测性合同（§8）禁止导出记忆正文、用户私有内容或凭据。
		if _, err := transaction.ExecContext(ctx, `
			CREATE TABLE recall_telemetry (
				id INTEGER PRIMARY KEY AUTOINCREMENT,
				occurred_at TEXT NOT NULL,
				candidates_returned INTEGER NOT NULL,
				conflicts INTEGER NOT NULL,
				evidence_insufficient INTEGER NOT NULL,
				plan_strategy TEXT NOT NULL
			);`); err != nil {
			return fmt.Errorf("create recall telemetry schema: %w", err)
		}
		if err := recordMigration(ctx, transaction, migrationVersionNine, migrationNameNine, migrationHashNine); err != nil {
			return err
		}
		if err := setMigrationVersion(ctx, transaction, migrationVersionNine); err != nil {
			return err
		}
	}
	if err := checkMigration(ctx, transaction); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit memory migration: %w", err)
	}
	return nil
}

// recordMigration 记录一条已经在当前事务内完成的单向 schema 迁移。
func recordMigration(ctx context.Context, transaction *sql.Tx, version int, name, hash string) error {
	if _, err := transaction.ExecContext(ctx, "INSERT INTO schema_migrations(version, name, sha256, applied_at) VALUES (?, ?, ?, ?)", version, name, hash, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return fmt.Errorf("record memory migration %d: %w", version, err)
	}
	return nil
}

// setMigrationVersion 将 SQLite user_version 前移到已经记录的 schema 版本。
func setMigrationVersion(ctx context.Context, transaction *sql.Tx, version int) error {
	if _, err := transaction.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("write memory schema version %d: %w", version, err)
	}
	return nil
}

// verifyAppliedMigration 校验已有迁移的不可变校验和，防止篡改库被继续升级。
func verifyAppliedMigration(ctx context.Context, source queryer, version int, expectedHash string) error {
	var recorded string
	if err := source.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version = ?", version).Scan(&recorded); err != nil {
		return fmt.Errorf("read memory migration %d: %w", version, err)
	}
	if recorded != expectedHash {
		return fmt.Errorf("memory migration %d checksum mismatch", version)
	}
	return nil
}

// verifyLegacyV2Migration 校验历史 v2 迁移记录的名称与摘要。它只供只读预检调用，
// 用于确保损坏记录先作为完整性错误失败，而不会被误报为产品不兼容。
func verifyLegacyV2Migration(ctx context.Context, source queryer, version int, expectedName, expectedHash string) error {
	var recordedName, recordedHash string
	if err := source.QueryRowContext(ctx, "SELECT name, sha256 FROM schema_migrations WHERE version = ?", version).Scan(&recordedName, &recordedHash); err != nil {
		return fmt.Errorf("read memory migration %d: %w", version, err)
	}
	if recordedName != expectedName {
		return fmt.Errorf("memory migration %d name mismatch", version)
	}
	if recordedHash != expectedHash {
		return fmt.Errorf("memory migration %d checksum mismatch", version)
	}
	return nil
}

// checkMigration 读取版本链与各版本校验和；未来版本、缺失记录与篡改均失败关闭。
// 版本链当前为 1（情节记忆）、2（巩固事务）、3（冻结支持度）、4（派生
// 生命周期）、5（演化候选登记）、6（评测注册与审计词表收口）、7（晋升
// 版本账本与审计词表扩展）、8（分阶段晋升激活）与 9（召回遥测采集），单向兼容：旧二进制读新库失败关闭。
func checkMigration(ctx context.Context, source queryer) error {
	var version int
	if err := source.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read memory schema version: %w", err)
	}
	if version > migrationVersion {
		return fmt.Errorf("memory schema version %d is newer than this Cairn binary", version)
	}
	if version != migrationVersion {
		return fmt.Errorf("memory schema version %d is unsupported", version)
	}
	expected := map[int]string{1: migrationHash, migrationVersionTwo: migrationHashTwo, migrationVersionThree: migrationHashThree, migrationVersionFour: migrationHashFour, migrationVersionFive: migrationHashFive, migrationVersionSix: migrationHashSix, migrationVersionSeven: migrationHashSeven, migrationVersionEight: migrationHashEight, migrationVersionNine: migrationHashNine}
	for migrationID, hash := range expected {
		var recorded string
		if err := source.QueryRowContext(ctx, "SELECT sha256 FROM schema_migrations WHERE version = ?", migrationID).Scan(&recorded); err != nil {
			return fmt.Errorf("read memory migration %d: %w", migrationID, err)
		}
		if recorded != hash {
			return fmt.Errorf("memory migration %d checksum mismatch", migrationID)
		}
	}
	return nil
}
