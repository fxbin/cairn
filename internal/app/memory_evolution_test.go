package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/ports"
)

// evolutionAppFixture 在同一工作区完成两次同任务运行、捕获并巩固，
// 返回服务、工作区、状态目录、配置与派生 pattern 的版本键。
func evolutionAppFixture(t *testing.T) (Service, string, string, config.Config, memory.EvolutionSourceRef) {
	t.Helper()
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	for index := 0; index < 2; index++ {
		run, err := service.Run(context.Background(), RunOptions{Task: "evolution-fixture-task", Workspace: workspace, StateDir: stateDir, Config: configuration})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.CaptureMemory(context.Background(), MemoryCaptureOptions{RunID: run.ID, Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	results, err := service.ConsolidateMemory(context.Background(), MemoryConsolidateOptions{Workspace: workspace, StateDir: stateDir, Config: configuration, TTL: 12 * time.Hour})
	if err != nil || len(results) == 0 {
		t.Fatalf("consolidation = %#v err = %v", results, err)
	}
	pattern := results[0].Plan.Derived[0]
	return service, workspace, stateDir, configuration, memory.EvolutionSourceRef{DerivedID: pattern.ID, ConsolidationID: pattern.ConsolidationID}
}

// evolutionVariantFixture 返回一条合法的闭合变体参数。
func evolutionVariantFixture() memory.CompactionParamsVariant {
	return memory.CompactionParamsVariant{
		SchemaVersion:           memory.EvolutionSchemaVersion,
		MaxPromptTokens:         50_000,
		MaxSummaryBytes:         16_384,
		InventoryBudgetPercent:  25,
		ThresholdReservePercent: 20,
	}
}

// TestEvolutionCandidateLifecycleClosedAtAppLevel 锁定应用层闭环：创建复核
// 来源、幂等重建、封闭理由拒绝、冷却阻断重建与只读解释。
func TestEvolutionCandidateLifecycleClosedAtAppLevel(t *testing.T) {
	service, workspace, stateDir, configuration, source := evolutionAppFixture(t)
	baseline := "sha256:" + strings.Repeat("ab", 32)
	created, err := service.CreateEvolutionCandidate(context.Background(), MemoryEvolutionCreateOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		SourceDerivedID: source.DerivedID, SourceConsolidationID: source.ConsolidationID,
		BaselineFingerprint: baseline, Variant: evolutionVariantFixture(),
	})
	if err != nil || created.Status != memory.CandidateStateDraft {
		t.Fatalf("create = %#v err = %v", created, err)
	}
	retry, err := service.CreateEvolutionCandidate(context.Background(), MemoryEvolutionCreateOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		SourceDerivedID: source.DerivedID, SourceConsolidationID: source.ConsolidationID,
		BaselineFingerprint: baseline, Variant: evolutionVariantFixture(),
	})
	if err != nil || retry.ID != created.ID {
		t.Fatalf("idempotent create = %#v err = %v", retry, err)
	}
	invalidVariant := evolutionVariantFixture()
	invalidVariant.MaxPromptTokens = memory.VariantMinPromptTokens - 1
	if _, err := service.CreateEvolutionCandidate(context.Background(), MemoryEvolutionCreateOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		SourceDerivedID: source.DerivedID, SourceConsolidationID: source.ConsolidationID,
		BaselineFingerprint: baseline, Variant: invalidVariant,
	}); err == nil {
		t.Fatal("out-of-bound variant must fail closed at the app boundary")
	}
	if _, err := service.CreateEvolutionCandidate(context.Background(), MemoryEvolutionCreateOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		SourceDerivedID: "pat_missing", SourceConsolidationID: source.ConsolidationID,
		BaselineFingerprint: baseline, Variant: evolutionVariantFixture(),
	}); err == nil {
		t.Fatal("missing source must fail closed")
	}
	rejected, err := service.RejectEvolutionCandidate(context.Background(), MemoryEvolutionActionOptions{
		CandidateID: created.ID, Reason: memory.RejectReasonHumanRequest,
		Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil || rejected.Status != memory.CandidateStateRejected {
		t.Fatalf("reject = %#v err = %v", rejected, err)
	}
	if _, err := service.CreateEvolutionCandidate(context.Background(), MemoryEvolutionCreateOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		SourceDerivedID: source.DerivedID, SourceConsolidationID: source.ConsolidationID,
		BaselineFingerprint: baseline, Variant: evolutionVariantFixture(),
	}); err == nil {
		t.Fatal("cooldown must block recreation after rejection")
	}
	if _, err := service.RejectEvolutionCandidate(context.Background(), MemoryEvolutionActionOptions{
		CandidateID: created.ID, Reason: "not_a_reason",
		Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); err == nil {
		t.Fatal("free-text rejection reason must fail closed")
	}
	inspected, err := service.InspectEvolutionCandidate(context.Background(), MemoryEvolutionInspectOptions{
		CandidateID: created.ID, Workspace: workspace, StateDir: stateDir, Config: configuration,
	})
	if err != nil || len(inspected.Audit) != 2 {
		t.Fatalf("inspect = %#v err = %v", inspected, err)
	}
	if _, err := service.ExpireEvolutionCandidate(context.Background(), MemoryEvolutionActionOptions{
		CandidateID: created.ID, Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); err == nil {
		t.Fatal("rejected candidate must not transition into expired")
	}
	if _, err := service.InvalidateEvolutionCandidate(context.Background(), MemoryEvolutionActionOptions{
		CandidateID: created.ID, Workspace: workspace, StateDir: stateDir, Config: configuration,
	}); err == nil {
		t.Fatal("rejected candidate must not transition into invalidated")
	}
	if _, err := service.Doctor(context.Background(), workspace, stateDir, configuration); err != nil {
		t.Fatalf("doctor after evolution lifecycle: %v", err)
	}
}

// TestEvolutionCandidateFailsClosedWithoutStore 锁定缺少记忆库时创建失败关闭
// 且不创建新状态。
func TestEvolutionCandidateFailsClosedWithoutStore(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, err := service.CreateEvolutionCandidate(context.Background(), MemoryEvolutionCreateOptions{
		Workspace: workspace, StateDir: stateDir, Config: configuration,
		SourceDerivedID: "pat_none", SourceConsolidationID: "cons_none",
		BaselineFingerprint: "sha256:" + strings.Repeat("cd", 32), Variant: evolutionVariantFixture(),
	})
	if err == nil || !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("missing memory store err = %v", err)
	}
}
