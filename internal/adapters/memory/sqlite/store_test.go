package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/memory"
)

// TestStoreCapturesRecallsSupersedesAndRevokes 锁定 M0/M1 的单事务状态机：
// 同一 Run 幂等、替代保留历史、撤销有固定理由且 recall 只返回 active 未过期项。
func TestStoreCapturesRecallsSupersedesAndRevokes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	first := testCandidate(t, "run_first", now, memory.DefaultTTL, "")
	captured, err := store.Capture(context.Background(), first)
	if err != nil || captured.ID != first.ID || captured.Status != memory.StatusActive {
		t.Fatalf("capture=%#v err=%v", captured, err)
	}
	idempotent, err := store.Capture(context.Background(), first)
	if err != nil || idempotent.ID != first.ID {
		t.Fatalf("idempotent capture=%#v err=%v", idempotent, err)
	}
	recalled, err := store.Recall(context.Background(), memory.RecallOptions{WorkspaceHash: first.WorkspaceHash, Now: now})
	if err != nil || len(recalled) != 1 || recalled[0].ID != first.ID {
		t.Fatalf("recall=%#v err=%v", recalled, err)
	}
	second := testCandidate(t, "run_second", now.Add(time.Second), memory.DefaultTTL, first.ID)
	if _, err := store.Capture(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	firstInspect, err := store.Inspect(context.Background(), first.ID)
	if err != nil || firstInspect.Candidate.Status != memory.StatusSuperseded || len(firstInspect.Audit) != 2 || firstInspect.Audit[1].Action != memory.AuditSuperseded {
		t.Fatalf("first inspect=%#v err=%v", firstInspect, err)
	}
	recalled, err = store.Recall(context.Background(), memory.RecallOptions{WorkspaceHash: first.WorkspaceHash, Now: now.Add(2 * time.Second)})
	if err != nil || len(recalled) != 1 || recalled[0].ID != second.ID {
		t.Fatalf("recall after supersede=%#v err=%v", recalled, err)
	}
	revoked, err := store.Revoke(context.Background(), second.ID, memory.RevokeEvidenceInvalid, now.Add(3*time.Second))
	if err != nil || revoked.Status != memory.StatusRevoked {
		t.Fatalf("revoke=%#v err=%v", revoked, err)
	}
	recalled, err = store.Recall(context.Background(), memory.RecallOptions{WorkspaceHash: first.WorkspaceHash, Now: now.Add(4 * time.Second)})
	if err != nil || len(recalled) != 0 {
		t.Fatalf("recall after revoke=%#v err=%v", recalled, err)
	}
	secondInspect, err := store.Inspect(context.Background(), second.ID)
	if err != nil || len(secondInspect.Audit) != 2 || secondInspect.Audit[1].Action != memory.AuditRevoked || secondInspect.Audit[1].Reason != memory.RevokeEvidenceInvalid {
		t.Fatalf("second inspect=%#v err=%v", secondInspect, err)
	}
	if err := store.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// TestStoreRejectsExpiredRecallAndConflictingIdempotency 锁定 TTL 边界与同一
// Run 事实冲突的失败关闭语义。
func TestStoreRejectsExpiredRecallAndConflictingIdempotency(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	expired := testCandidate(t, "run_expired", now.Add(-time.Hour), time.Minute, "")
	if _, err := store.Capture(context.Background(), expired); err != nil {
		t.Fatal(err)
	}
	recalled, err := store.Recall(context.Background(), memory.RecallOptions{WorkspaceHash: expired.WorkspaceHash, Now: now})
	if err != nil || len(recalled) != 0 {
		t.Fatalf("expired recall=%#v err=%v", recalled, err)
	}
	conflicting := expired
	conflicting.Outcome = "cancelled"
	if _, err := store.Capture(context.Background(), conflicting); !errors.Is(err, memory.ErrConflict) {
		t.Fatalf("conflicting capture error=%v", err)
	}
	if _, err := memory.NormalizeTTL(memory.MaxTTL + time.Nanosecond); err == nil {
		t.Fatal("TTL beyond the maximum unexpectedly accepted")
	}
	if _, err := memory.NormalizeRecallLimit(memory.MaxRecallLimit + 1); err == nil {
		t.Fatal("recall limit beyond the maximum unexpectedly accepted")
	}
}

// TestStoreRecallAppliesStructuredFiltersWithoutWriting 锁定 M2-A 的 SQLite 精确
// 过滤与只读边界：受限条件只缩小结果集，查询前后数据库文件保持不变。
func TestStoreRecallAppliesStructuredFiltersWithoutWriting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	failed := testCandidate(t, "run_exact_failed", now, memory.DefaultTTL, "")
	completed := testCandidate(t, "run_exact_completed", now.Add(time.Second), memory.DefaultTTL, "")
	completed.Outcome = "completed"
	completed.FailureClass = ""
	completed.TrustedVerification = true
	if err := completed.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Capture(context.Background(), failed); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Capture(context.Background(), completed); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store, err = OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	from := now.Add(-time.Second)
	until := now.Add(2 * time.Second)
	recalled, err := store.Recall(context.Background(), memory.RecallOptions{
		WorkspaceHash:       failed.WorkspaceHash,
		Outcomes:            []string{"completed"},
		TrustedVerification: memory.TrustedVerificationRequired,
		CreatedFrom:         &from,
		CreatedUntil:        &until,
		Precision:           memory.RecallPrecisionTrace,
		Limit:               memory.DefaultTraceRecallLimit,
		Now:                 now,
	})
	if err != nil || len(recalled) != 1 || recalled[0].ID != completed.ID {
		t.Fatalf("recalled=%#v err=%v", recalled, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("read-only exact filter changed memory database")
	}
}

// TestStoreMigrationPermissionsAndReadOnlyMode 锁定独立 SQLite 的文件权限、
// 迁移 checksum/future-version 失败关闭，以及只读句柄不能修改候选。
func TestStoreMigrationPermissionsAndReadOnlyMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	candidate := testCandidate(t, "run_permissions", time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC), memory.DefaultTTL, "")
	if _, err := store.Capture(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Stat(path)
	if err != nil || file.Mode().Perm() != 0o600 {
		t.Fatalf("memory db permissions=%v err=%v", file.Mode().Perm(), err)
	}
	directory, err := os.Stat(filepath.Dir(path))
	if err != nil || directory.Mode().Perm() != 0o700 {
		t.Fatalf("memory dir permissions=%v err=%v", directory.Mode().Perm(), err)
	}
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := readOnly.Revoke(context.Background(), candidate.ID, memory.RevokeHumanRequest, time.Now()); err == nil {
		t.Fatal("read-only memory store unexpectedly revoked a candidate")
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE schema_migrations SET sha256 = 'tampered' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("checksum mismatch unexpectedly opened")
	}
	futurePath := filepath.Join(t.TempDir(), "future-memory.db")
	future, err := Open(futurePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := future.Close(); err != nil {
		t.Fatal(err)
	}
	futureDatabase, err := sql.Open("sqlite", futurePath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := futureDatabase.Exec("PRAGMA user_version = " + strconv.Itoa(migrationVersion+1)); err != nil {
		futureDatabase.Close()
		t.Fatal(err)
	}
	if err := futureDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(futurePath); err == nil {
		t.Fatal("future schema version unexpectedly opened")
	}
}

// TestOpenReadOnlyRejectsUnsafePathsWithoutRepair 锁定只读入口会拒绝权限放宽、
// 链接和非常规文件，并且不会通过 chmod、迁移或 WAL 创建来"修复"它们。
func TestOpenReadOnlyRejectsUnsafePathsWithoutRepair(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "memory.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(path)
	before := directorySnapshot(t, directory)
	readOnly, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := readOnly.Close(); err != nil {
		t.Fatal(err)
	}
	if after := directorySnapshot(t, directory); !reflect.DeepEqual(before, after) {
		t.Fatalf("read-only open changed state: before=%v after=%v", before, after)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("0644 memory database unexpectedly opened read-only")
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("read-only path repaired database permissions: mode=%v err=%v", info.Mode().Perm(), err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(path); err == nil {
		t.Fatal("0755 memory directory unexpectedly opened read-only")
	}
	info, err = os.Lstat(directory)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("read-only path repaired directory permissions: mode=%v err=%v", info.Mode().Perm(), err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(directory, "memory-link.db")
	if err := os.Symlink(path, linkPath); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenReadOnly(linkPath); err == nil {
		t.Fatal("symlinked memory database unexpectedly opened read-only")
	}
}

// TestStoreCheckRejectsAuditStatusAndSupersedeTampering 锁定 Store.Check 不能把
// SQLite 的物理完整性误当成审计逻辑完整性；三类直接篡改都必须失败关闭。
func TestStoreCheckRejectsAuditStatusAndSupersedeTampering(t *testing.T) {
	mutations := []struct {
		name  string
		apply func(*sql.DB, memory.Candidate, memory.Candidate) error
	}{
		{
			name: "capture_audit_deleted",
			apply: func(database *sql.DB, first, _ memory.Candidate) error {
				_, err := database.Exec("DELETE FROM audit_log WHERE candidate_id = ? AND action = ?", first.ID, memory.AuditCaptured)
				return err
			},
		},
		{
			name: "superseded_status_reactivated",
			apply: func(database *sql.DB, first, _ memory.Candidate) error {
				_, err := database.Exec("UPDATE candidates SET status = ? WHERE id = ?", memory.StatusActive, first.ID)
				return err
			},
		},
		{
			name: "successor_relation_removed",
			apply: func(database *sql.DB, _, second memory.Candidate) error {
				_, err := database.Exec("UPDATE candidates SET supersedes_id = NULL WHERE id = ?", second.ID)
				return err
			},
		},
	}
	for _, mutation := range mutations {
		t.Run(mutation.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "memory.db")
			store, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Add(-time.Minute)
			first := testCandidate(t, "run_first", now, memory.DefaultTTL, "")
			second := testCandidate(t, "run_second", now.Add(time.Second), memory.DefaultTTL, first.ID)
			if _, err := store.Capture(context.Background(), first); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Capture(context.Background(), second); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			database, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if err := mutation.apply(database, first, second); err != nil {
				database.Close()
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			if err := store.Check(context.Background()); err == nil {
				t.Fatal("tampered memory audit state unexpectedly passed Check")
			}
		})
	}
}

// testCandidate 构造不含来源正文的最小有效 run_outcome 候选。
func testCandidate(t *testing.T, runID string, createdAt time.Time, ttl time.Duration, supersedesID string) memory.Candidate {
	t.Helper()
	workspaceHash := "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	candidate, err := memory.NewRunOutcomeCandidate(workspaceHash, runID, "sha256:2222222222222222222222222222222222222222222222222222222222222222", "failed", "agent", false, createdAt, ttl, []memory.EventEvidenceRef{{RunID: runID, EventSeq: 3, EventHash: "sha256:3333333333333333333333333333333333333333333333333333333333333333", EventType: "run_failed"}}, supersedesID)
	if err != nil {
		t.Fatal(err)
	}
	return candidate
}

// directorySnapshot 返回目录中名称、权限和大小的只读前后对比摘要。
func directorySnapshot(t *testing.T, directory string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := make(map[string]string, len(entries))
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		snapshot[entry.Name()] = info.Mode().String() + ":" + strconv.FormatInt(info.Size(), 10)
	}
	return snapshot
}
