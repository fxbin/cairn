// 本文件补充 M3 的巩固、派生拒绝与派生解释用例：巩固先做跨库验证再进入
// Memory Store 单写入者事务；派生候选永不进入运行时召回。
//
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	memstoresqlite "github.com/fxbin/cairn/internal/adapters/memstore/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
)

// MemoryConsolidateOptions 描述一次事务化巩固的输入边界：工作区、保留期、
// 支持度下限与冻结时钟；不接受任务正文或任何自由文本。
type MemoryConsolidateOptions struct {
	Workspace            string
	StateDir             string
	Config               config.Config
	TTL                  time.Duration
	MinPatternSupport    int
	MinHypothesisSupport int
	Now                  time.Time
}

// MemoryRejectDerivedOptions 描述显式拒绝派生候选的封闭输入。
type MemoryRejectDerivedOptions struct {
	DerivedID string
	Reason    string
	Workspace string
	StateDir  string
	Config    config.Config
}

// MemoryInspectDerivedOptions 描述派生候选的只读解释输入。
type MemoryInspectDerivedOptions struct {
	DerivedID string
	Workspace string
	StateDir  string
	Config    config.Config
}

// ConsolidateMemory 在既有工作区锁内执行一次事务化巩固：先做与 doctor 同等
// 严格的跨库验证，再从全部 active、未过期的 run_outcome 候选确定性归纳，最后
// 将全部计划作为一个批次在 Memory Store 单写入者事务中提交。缺少 Memory Store 时失败关闭，
// 绝不为巩固创建新状态；派生候选永不进入运行时召回。
func (service Service) ConsolidateMemory(ctx context.Context, options MemoryConsolidateOptions) ([]memstoresqlite.ConsolidationResult, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return nil, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return nil, err
	}
	defer workspaceLock.Release()
	path := memoryDBPath(options.StateDir, workspaceHash)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, memory.ErrNotFound
		}
		return nil, err
	}
	store, err := memstoresqlite.Open(path)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	now := options.Now.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	all, err := store.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	inputs := memory.ConsolidationInputs{WorkspaceHash: workspaceHash, Candidates: make([]memory.Candidate, 0, len(all))}
	for _, candidate := range all {
		if candidate.Status == memory.StatusActive && candidate.ExpiresAt.After(now) {
			inputs.Candidates = append(inputs.Candidates, candidate)
		}
	}
	if len(inputs.Candidates) == 0 {
		return []memstoresqlite.ConsolidationResult{}, nil
	}
	plans, err := memory.DeriveConsolidations(inputs, memory.ConsolidationOptions{
		Now:                  now,
		TTL:                  options.TTL,
		MinPatternSupport:    options.MinPatternSupport,
		MinHypothesisSupport: options.MinHypothesisSupport,
	})
	if err != nil {
		return nil, err
	}
	results, err := store.ConsolidateBatch(ctx, plans)
	if err != nil {
		return nil, err
	}
	return results, nil
}

// RejectDerivedMemory 在工作区锁内显式拒绝当前 active 的派生候选版本；
// 拒绝理由只能是封闭词表，重复拒绝幂等，不产生任何自动晋升。
func (service Service) RejectDerivedMemory(ctx context.Context, options MemoryRejectDerivedOptions) (memstoresqlite.DerivedVersionKey, error) {
	if !memory.ValidRejectionReason(options.Reason) {
		return memstoresqlite.DerivedVersionKey{}, errors.New("derived rejection reason must be insufficient_evidence or human_request")
	}
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memstoresqlite.DerivedVersionKey{}, err
	}
	workspaceLock, err := acquireWorkspaceLock(options.StateDir, workspaceHash)
	if err != nil {
		return memstoresqlite.DerivedVersionKey{}, err
	}
	defer workspaceLock.Release()
	store, err := openWritableExistingMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memstoresqlite.DerivedVersionKey{}, err
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memstoresqlite.DerivedVersionKey{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectDerived(ctx, options.DerivedID)
	if err != nil {
		return memstoresqlite.DerivedVersionKey{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memstoresqlite.DerivedVersionKey{}, memory.ErrNotFound
	}
	return store.RejectDerived(ctx, options.DerivedID, options.Reason, time.Now().UTC())
}

// InspectDerivedMemory 只读返回派生候选最新版本与审计历史；跨工作区标识
// 即使猜中也失败关闭。
func (service Service) InspectDerivedMemory(ctx context.Context, options MemoryInspectDerivedOptions) (memstoresqlite.DerivedInspectResult, error) {
	_, workspaceHash, err := workspaceFor(options.Workspace, options.Config)
	if err != nil {
		return memstoresqlite.DerivedInspectResult{}, err
	}
	store, err := openReadOnlyMemory(options.StateDir, workspaceHash)
	if err != nil {
		return memstoresqlite.DerivedInspectResult{}, fmt.Errorf("%w: open memory store: %v", ErrStateCorrupt, err)
	}
	defer store.Close()
	if err := verifyReadOnlyMemoryStore(ctx, store, options.StateDir, workspaceHash); err != nil {
		return memstoresqlite.DerivedInspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	inspected, err := store.InspectDerived(ctx, options.DerivedID)
	if err != nil {
		return memstoresqlite.DerivedInspectResult{}, err
	}
	if inspected.Candidate.WorkspaceHash != workspaceHash {
		return memstoresqlite.DerivedInspectResult{}, memory.ErrNotFound
	}
	return inspected, nil
}

// openWritableExistingMemory 打开既有的可写 Memory Store；缺失路径按各用例
// 的封闭语义返回 ErrNotFound，而不是为写入用例创建新数据库。
func openWritableExistingMemory(stateDir, workspaceHash string) (*memstoresqlite.Store, error) {
	path := memoryDBPath(stateDir, workspaceHash)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, memory.ErrNotFound
		}
		return nil, err
	}
	return memstoresqlite.Open(path)
}
