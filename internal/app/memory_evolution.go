// 本文件补充 M4 的演化候选用例：创建前执行与 doctor 同等的全库验证和
// 来源派生版本复核；变体参数由人工显式提供，绝不自动生成候选。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"time"

	memorysqlite "github.com/fxbin/cairn/internal/adapters/memory/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
)

// MemoryEvolutionCreateOptions 描述创建演化候选的输入边界：闭合变体参数、
// 精确来源版本复合键与基线指纹；不接受任务正文或自由文本。
type MemoryEvolutionCreateOptions struct {
	Workspace             string
	StateDir              string
	Config                config.Config
	SourceDerivedID       string
	SourceConsolidationID string
	BaselineFingerprint   string
	NoteDigest            string
	Variant               memory.CompactionParamsVariant
	TTL                   time.Duration
	Cooldown              time.Duration
}

// MemoryEvolutionActionOptions 描述拒绝/作废等迁移动作的封闭输入。
type MemoryEvolutionActionOptions struct {
	CandidateID string
	Reason      string
	Workspace   string
	StateDir    string
	Config      config.Config
	Cooldown    time.Duration
}

// MemoryEvolutionInspectOptions 描述演化候选的只读解释输入。
type MemoryEvolutionInspectOptions struct {
	CandidateID string
	Workspace   string
	StateDir    string
	Config      config.Config
}

// CreateEvolutionCandidate 在工作区锁内创建一条 draft 演化候选：先做全库
// 跨库验证，再复核来源派生版本存在、active、未过期且属于当前工作区，最后
// 由变体参数确定性派生指纹并写入登记事务。缺少记忆库或来源失效即失败关闭。
func (service Service) CreateEvolutionCandidate(ctx context.Context, options MemoryEvolutionCreateOptions) (memory.EvolutionCandidate, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	source := memory.EvolutionSourceRef{DerivedID: options.SourceDerivedID, ConsolidationID: options.SourceConsolidationID}
	derivedInspect, err := store.InspectDerived(ctx, source.DerivedID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if derivedInspect.Candidate.ConsolidationID != source.ConsolidationID || derivedInspect.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	if derivedInspect.Candidate.Status != memory.DerivedStatusActive {
		return memory.EvolutionCandidate{}, errors.New("evolution source derived version is not active")
	}
	now := time.Now().UTC()
	if !derivedInspect.Candidate.ExpiresAt.After(now) {
		return memory.EvolutionCandidate{}, errors.New("evolution source derived version is expired")
	}
	variantFingerprint, err := memory.VariantFingerprint(memory.TargetKindContextCompactionParams, options.Variant)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	variantDigest, err := memory.VariantSpecDigest(options.Variant)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	ttl, err := memory.NormalizeCandidateTTL(options.TTL)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	candidate := memory.EvolutionCandidate{
		ID:                  memory.EvolutionCandidateID(workspaceHash, source, memory.TargetKindContextCompactionParams, variantFingerprint),
		SchemaVersion:       memory.EvolutionSchemaVersion,
		WorkspaceHash:       workspaceHash,
		Source:              source,
		TargetKind:          memory.TargetKindContextCompactionParams,
		VariantSpecSHA256:   variantDigest,
		VariantFingerprint:  variantFingerprint,
		BaselineFingerprint: options.BaselineFingerprint,
		CreatedAt:           now,
		ExpiresAt:           now.Add(ttl),
		Status:              memory.CandidateStateDraft,
		NoteDigest:          options.NoteDigest,
	}
	return store.CreateEvolutionCandidate(ctx, candidate, now)
}

// RejectEvolutionCandidate 在工作区锁内以封闭理由拒绝候选并落入冷却；
// 全库验证先行，跨工作区标识失败关闭。
func (service Service) RejectEvolutionCandidate(ctx context.Context, options MemoryEvolutionActionOptions) (memory.EvolutionCandidate, error) {
	if !memory.ValidEvolutionRejectionReason(options.Reason) {
		return memory.EvolutionCandidate{}, errors.New("evolution rejection reason is invalid")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	return store.RejectEvolutionCandidate(ctx, options.CandidateID, options.Reason, time.Now().UTC(), options.Cooldown)
}

// InvalidateEvolutionCandidate 在工作区锁内把来源失效的候选显式作废。
func (service Service) InvalidateEvolutionCandidate(ctx context.Context, options MemoryEvolutionActionOptions) (memory.EvolutionCandidate, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	return store.InvalidateEvolutionCandidate(ctx, options.CandidateID, time.Now().UTC())
}

// ExpireEvolutionCandidate 在工作区锁内把保留期耗尽的候选显式置为 expired。
func (service Service) ExpireEvolutionCandidate(ctx context.Context, options MemoryEvolutionActionOptions) (memory.EvolutionCandidate, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memory.EvolutionCandidate{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memory.EvolutionCandidate{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memory.EvolutionCandidate{}, memory.ErrNotFound
	}
	return store.ExpireEvolutionCandidate(ctx, options.CandidateID, time.Now().UTC())
}

// InspectEvolutionCandidate 只读返回候选与审计历史；跨工作区失败关闭。
func (service Service) InspectEvolutionCandidate(ctx context.Context, options MemoryEvolutionInspectOptions) (memorysqlite.EvolutionInspectResult, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memorysqlite.EvolutionInspectResult{}, err
	}
	store, err := openReadOnlyMemory(options.StateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return memorysqlite.EvolutionInspectResult{}, memory.ErrNotFound
		}
		return memorysqlite.EvolutionInspectResult{}, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memorysqlite.EvolutionInspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectEvolutionCandidate(ctx, options.CandidateID)
	if err != nil {
		return memorysqlite.EvolutionInspectResult{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memorysqlite.EvolutionInspectResult{}, memory.ErrNotFound
	}
	return inspected, nil
}
