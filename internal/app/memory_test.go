package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/lock"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/ports"
)

// TestMemoryCaptureRecallRevokeAndDoctor 锁定从终态 Journal 派生的 M0/M1
// 垂直切片：Memory 只存结构化结果和摘要，recall/revoke 不影响 Agent 运行事实。
func TestMemoryCaptureRecallRevokeAndDoctor(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	run, err := service.Run(context.Background(), RunOptions{Task: "memory-secret-task-sentinel", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	captured, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: time.Hour})
	if err != nil || captured.Kind != memory.KindRunOutcome || captured.Status != memory.StatusActive || len(captured.Evidence) == 0 {
		t.Fatalf("capture=%#v err=%v", captured, err)
	}
	for _, evidence := range captured.Evidence {
		if strings.Contains(evidence.ArtifactSHA256, "truncated_preview") {
			t.Fatalf("memory evidence retained artifact preview: %#v", evidence)
		}
	}
	if _, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: time.Hour}); err != nil {
		t.Fatalf("idempotent capture failed: %v", err)
	}
	recalled, err := service.RecallMemory(context.Background(), MemoryRecallOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Limit: 10})
	if err != nil || len(recalled) != 1 || recalled[0].ID != captured.ID {
		t.Fatalf("recall=%#v err=%v", recalled, err)
	}
	inspected, err := service.InspectMemory(context.Background(), MemoryInspectOptions{CandidateID: captured.ID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || len(inspected.Audit) != 1 || inspected.Audit[0].Action != memory.AuditCaptured {
		t.Fatalf("inspect=%#v err=%v", inspected, err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	memoryBytes, err := os.ReadFile(memoryDBPath(stateDir, hash))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(memoryBytes), "memory-secret-task-sentinel") {
		t.Fatal("memory database retained task text")
	}
	report, err := service.Doctor(context.Background(), workspace, stateDir, configuration)
	if err != nil || report.MemoryState != "ok" || !report.MemoryOK {
		t.Fatalf("doctor=%#v err=%v", report, err)
	}
	revoked, err := service.RevokeMemory(context.Background(), MemoryRevokeOptions{CandidateID: captured.ID, Reason: memory.RevokeHumanRequest, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || revoked.Status != memory.StatusRevoked {
		t.Fatalf("revoke=%#v err=%v", revoked, err)
	}
	recalled, err = service.RecallMemory(context.Background(), MemoryRecallOptions{Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || len(recalled) != 0 {
		t.Fatalf("recall after revoke=%#v err=%v", recalled, err)
	}
}

// TestMemoryCaptureRequiresTerminalRunAndWorkspaceLock 锁定 capture 不会绕过
// 既有工作区锁，也不会接受非终态或跨工作区的 Journal 事实。
func TestMemoryCaptureRequiresTerminalRunAndWorkspaceLock(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	workspaceLock, err := acquireWorkspaceLock(stateDir, hash)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{}
	_, err = service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: "run_missing", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if !errors.Is(err, lock.ErrHeld) {
		workspaceLock.Release()
		t.Fatalf("capture error=%v, want workspace lock contention", err)
	}
	if err := workspaceLock.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: "run_missing", Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil {
		t.Fatal("capture accepted missing/non-terminal run")
	}
}

// TestDoctorRejectsTamperedMemoryEventEvidence 锁定 doctor 的跨库失败关闭：即使
// Memory SQLite 内部结构仍合法，seq/hash 与 Journal 不匹配也不得报告 ok。
func TestDoctorRejectsTamperedMemoryEventEvidence(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	run, err := service.Run(context.Background(), RunOptions{Task: "doctor memory evidence", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", memoryDBPath(stateDir, hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE evidence_refs SET event_hash = ? WHERE candidate_id = ? AND ordinal = 0", "sha256:0000000000000000000000000000000000000000000000000000000000000000", candidate.ID); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("doctor error=%v, want state corrupt", err)
	}
}

// TestRecallAndInspectFailClosedOnCorruptMemorySources 锁定 recall/inspect 与
// doctor 共享外部证据边界：Event、Artifact 或源 Journal 一旦失真，不能返回候选。
func TestRecallAndInspectFailClosedOnCorruptMemorySources(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, stateDir, workspaceHash string, candidate memory.Candidate)
	}{
		{
			name: "event_hash",
			mutate: func(t *testing.T, stateDir, workspaceHash string, candidate memory.Candidate) {
				t.Helper()
				database, err := sql.Open("sqlite", filepath.Join(stateDir, "workspaces", workspaceHash, "runs.db"))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := database.Exec("UPDATE events SET event_hash = ? WHERE run_id = ? AND seq = 1", "0000000000000000000000000000000000000000000000000000000000000000", candidate.RunID); err != nil {
					database.Close()
					t.Fatal(err)
				}
				if err := database.Close(); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "artifact_removed",
			mutate: func(t *testing.T, stateDir, workspaceHash string, candidate memory.Candidate) {
				t.Helper()
				artifactSHA := ""
				for _, evidence := range candidate.Evidence {
					if evidence.ArtifactSHA256 != "" {
						artifactSHA = evidence.ArtifactSHA256
						break
					}
				}
				if artifactSHA == "" {
					t.Fatal("captured memory has no artifact evidence")
				}
				digest := strings.TrimPrefix(artifactSHA, "sha256:")
				if err := os.Remove(filepath.Join(stateDir, "workspaces", workspaceHash, "artifacts", "sha256", digest[:2], digest)); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "source_journal_missing",
			mutate: func(t *testing.T, stateDir, workspaceHash string, _ memory.Candidate) {
				t.Helper()
				if err := os.Remove(filepath.Join(stateDir, "workspaces", workspaceHash, "runs.db")); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			workspace, stateDir := t.TempDir(), t.TempDir()
			configuration := withTestProvider(config.Defaults())
			service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
			run, err := service.Run(context.Background(), RunOptions{Task: "recall source integrity", Workspace: workspace, StateDir: stateDir, Config: configuration})
			if err != nil {
				t.Fatal(err)
			}
			candidate, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration})
			if err != nil {
				t.Fatal(err)
			}
			_, hash, err := workspaceFor(workspace, configuration)
			if err != nil {
				t.Fatal(err)
			}
			memoryPath := memoryDBPath(stateDir, hash)
			before, err := os.ReadFile(memoryPath)
			if err != nil {
				t.Fatal(err)
			}
			testCase.mutate(t, stateDir, hash, candidate)
			beforeReadOnly, err := os.ReadFile(memoryPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.RecallMemory(context.Background(), MemoryRecallOptions{Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("recall error=%v, want state corrupt", err)
			}
			if _, err := service.InspectMemory(context.Background(), MemoryInspectOptions{CandidateID: candidate.ID, Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("inspect error=%v, want state corrupt", err)
			}
			if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("doctor error=%v, want state corrupt", err)
			}
			after, err := os.ReadFile(memoryPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(beforeReadOnly, after) || !bytes.Equal(before, after) {
				t.Fatal("read-only integrity checks changed memory database")
			}
		})
	}
}

// TestRecallAndDoctorRejectUnsafeMemoryPermissions 锁定 memory.db 与父目录的
// 0600/0700 契约在 recall、inspect 和 doctor 中都失败关闭且绝不自动 chmod 修复。
func TestRecallAndDoctorRejectUnsafeMemoryPermissions(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	run, err := service.Run(context.Background(), RunOptions{Task: "memory permissions", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	memoryPath := memoryDBPath(stateDir, hash)
	for _, testCase := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{name: "database_0644", path: memoryPath, mode: 0o644},
		{name: "directory_0755", path: filepath.Dir(memoryPath), mode: 0o755},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if err := os.Chmod(testCase.path, testCase.mode); err != nil {
				t.Fatal(err)
			}
			restoreMode := os.FileMode(0o700)
			if testCase.path == memoryPath {
				restoreMode = 0o600
			}
			defer os.Chmod(testCase.path, restoreMode)
			if _, err := service.RecallMemory(context.Background(), MemoryRecallOptions{Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("recall error=%v, want state corrupt", err)
			}
			if _, err := service.InspectMemory(context.Background(), MemoryInspectOptions{CandidateID: candidate.ID, Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("inspect error=%v, want state corrupt", err)
			}
			if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("doctor error=%v, want state corrupt", err)
			}
			info, err := os.Lstat(testCase.path)
			if err != nil || info.Mode().Perm() != testCase.mode {
				t.Fatalf("read-only path repaired permissions: mode=%v err=%v", info.Mode().Perm(), err)
			}
		})
	}
}

// TestRecallAndDoctorRejectTamperedMemoryAudit 锁定 audit_log 的逻辑篡改由
// Store.Check 驱动 recall 与 doctor 一并失败关闭，而不是只依赖 SQLite quick_check。
func TestRecallAndDoctorRejectTamperedMemoryAudit(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	run, err := service.Run(context.Background(), RunOptions{Task: "memory audit integrity", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	_, hash, err := workspaceFor(workspace, configuration)
	if err != nil {
		t.Fatal(err)
	}
	database, err := sql.Open("sqlite", memoryDBPath(stateDir, hash))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("DELETE FROM audit_log WHERE candidate_id = ? AND action = ?", candidate.ID, memory.AuditCaptured); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.RecallMemory(context.Background(), MemoryRecallOptions{Workspace: workspace, StateDir: stateDir, Config: configuration}); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("recall error=%v, want state corrupt", err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("doctor error=%v, want state corrupt", err)
	}
}
