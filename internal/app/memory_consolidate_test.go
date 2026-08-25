package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
	_ "modernc.org/sqlite"

	"github.com/fxbin/cairn/internal/ports"
)

// consolidateMemoryFixture 在同一工作区跑两次同任务运行并捕获两条候选，
// 为巩固提供满足 pattern 下限的输入。
func consolidateMemoryFixture(t *testing.T) (Service, string, string, config.Config) {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	for index := 0; index < 2; index++ {
		run, err := service.Run(context.Background(), RunOptions{Task: "consolidation-fixture-task", Workspace: workspace, StateDir: stateDir, Config: configuration})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	return service, workspace, stateDir, configuration
}

// TestConsolidateMemoryProducesPatternAndStaysOutOfRecall 锁定 M3 应用闭环：
// 巩固从已验证候选派生 pattern，重试幂等，派生候选不进入运行时召回，
// doctor 跨库验证保持通过。
func TestConsolidateMemoryProducesPatternAndStaysOutOfRecall(t *testing.T) {
	service, workspace, stateDir, configuration := consolidateMemoryFixture(t)
	results, err := service.ConsolidateMemory(context.Background(), MemoryConsolidateOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Plan.Strategy != memory.StrategyTaskOutcomeStability || len(results[0].Plan.Derived) != 1 {
		t.Fatalf("results = %#v", results)
	}
	pattern := results[0].Plan.Derived[0]
	if pattern.Kind != memory.KindPattern || len(pattern.Evidence) < memory.PatternMinSupport {
		t.Fatalf("pattern = %#v", pattern)
	}
	retry, err := service.ConsolidateMemory(context.Background(), MemoryConsolidateOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: time.Hour})
	if err != nil || len(retry) != 1 || len(retry[0].SupersededVersion) != 0 {
		t.Fatalf("idempotent retry = %#v err = %v", retry, err)
	}
	recalled, err := service.RecallMemory(context.Background(), MemoryRecallOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range recalled {
		if candidate.Kind != memory.KindRunOutcome {
			t.Fatalf("derived candidate leaked into recall: %#v", candidate)
		}
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); err != nil {
		t.Fatalf("doctor after consolidation: %v", err)
	}
	inspected, err := service.InspectDerivedMemory(context.Background(), MemoryInspectDerivedOptions{DerivedID: pattern.ID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || inspected.Candidate.ID != pattern.ID || len(inspected.Audit) != 1 || inspected.Audit[0].Action != memory.DerivedAuditConsolidated {
		t.Fatalf("derived inspect = %#v err = %v", inspected, err)
	}
}

// TestConsolidateMemoryRejectsAndRederivesOnNewInput 锁定拒绝与新输入再巩固：
// 显式拒绝封闭理由幂等；新候选加入后同范围产生新版本并替代旧版本。
func TestConsolidateMemoryRejectsAndRederivesOnNewInput(t *testing.T) {
	service, workspace, stateDir, configuration := consolidateMemoryFixture(t)
	results, err := service.ConsolidateMemory(context.Background(), MemoryConsolidateOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: time.Hour})
	if err != nil || len(results) != 1 || len(results[0].Plan.Derived) != 1 {
		t.Fatalf("results = %#v err = %v", results, err)
	}
	pattern := results[0].Plan.Derived[0]
	key, err := service.RejectDerivedMemory(context.Background(), MemoryRejectDerivedOptions{DerivedID: pattern.ID, Reason: memory.RejectHumanRequest, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || key.ID != pattern.ID {
		t.Fatalf("reject = %#v err = %v", key, err)
	}
	repeat, err := service.RejectDerivedMemory(context.Background(), MemoryRejectDerivedOptions{DerivedID: pattern.ID, Reason: memory.RejectInsufficientEvidence, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || repeat != key {
		t.Fatalf("idempotent reject = %#v err = %v", repeat, err)
	}
	if _, err := service.RejectDerivedMemory(context.Background(), MemoryRejectDerivedOptions{DerivedID: pattern.ID, Reason: "wrong", Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil {
		t.Fatal("invalid rejection reason must fail closed")
	}
	run, err := service.Run(context.Background(), RunOptions{Task: "consolidation-fixture-task", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	second, err := service.ConsolidateMemory(context.Background(), MemoryConsolidateOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: time.Hour})
	if err != nil || len(second) != 1 {
		t.Fatalf("second consolidation = %#v err = %v", second, err)
	}
	for _, superseded := range second[0].SupersededVersion {
		if superseded.ID == pattern.ID {
			t.Fatalf("rejected version must not be superseded again: %#v", second[0].SupersededVersion)
		}
	}
	inspected, err := service.InspectDerivedMemory(context.Background(), MemoryInspectDerivedOptions{DerivedID: pattern.ID, Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err != nil || inspected.Candidate.Status != memory.DerivedStatusActive || len(inspected.Audit) != 1 {
		t.Fatalf("rederived latest = %#v err = %v", inspected, err)
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); err != nil {
		t.Fatalf("doctor after supersede: %v", err)
	}
}

// TestConsolidateMemoryFailsClosedWithoutStoreOrCorruption 锁定失败关闭：
// 缺少 Memory Store 的巩固返回未找到；候选源损坏时巩固拒绝执行。
func TestConsolidateMemoryFailsClosedWithoutStoreOrCorruption(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	if _, err := service.ConsolidateMemory(context.Background(), MemoryConsolidateOptions{Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing store err = %v", err)
	}
	service, populatedWorkspace, populatedStateDir, populatedConfig := consolidateMemoryFixture(t)
	_, hash, err := workspaceFor(populatedWorkspace, populatedConfig)
	if err != nil {
		t.Fatal(err)
	}
	memoryDatabase, err := sql.Open("sqlite", memoryDBPath(populatedStateDir, hash))
	if err != nil {
		t.Fatal(err)
	}
	var capturedRunID string
	if err := memoryDatabase.QueryRow("SELECT run_id FROM candidates LIMIT 1").Scan(&capturedRunID); err != nil {
		memoryDatabase.Close()
		t.Fatal(err)
	}
	if err := memoryDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	journalDatabase, err := sql.Open("sqlite", filepath.Join(populatedStateDir, "workspaces", hash, "runs.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := journalDatabase.Exec("DELETE FROM events WHERE run_id = ?", capturedRunID); err != nil {
		journalDatabase.Close()
		t.Fatal(err)
	}
	if err := journalDatabase.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ConsolidateMemory(context.Background(), MemoryConsolidateOptions{Workspace: populatedWorkspace, StateDir: populatedStateDir, Config: populatedConfig, TTL: time.Hour}); err == nil {
		t.Fatal("corrupted memory source must fail consolidation")
	}
}
