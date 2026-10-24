// 查询用例：Inspect / ListRuns / Evidence。
// Author: fxbin
package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/fxbin/cairn/internal/adapters/journal/sqlite"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

func (service Service) Inspect(ctx context.Context, workspacePath, stateDir string, config config.Config, runID domain.RunID, includeEvents bool) (InspectResult, error) {
	_, hash, err := workspaceFor(workspacePath, config)
	if err != nil {
		return InspectResult{}, err
	}
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		return InspectResult{}, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return InspectResult{}, err
	}
	if len(events) == 0 {
		return InspectResult{}, fmt.Errorf("run %s not found", runID)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkArtifactReferences(ctx, journal, stateDir, hash); err != nil {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	contractResult, contractErr := runResultFromEvents(events, projection, stateDir, hash)
	if contractErr != nil {
		return InspectResult{}, fmt.Errorf("%w: %v", ErrStateCorrupt, contractErr)
	}
	result := InspectResult{Projection: projection, Summary: SummarizeRun(events, projection, contractResult.Evidence.Trusted), Manifest: &contractResult.Manifest}
	if includeEvents {
		result.Events = events
	}
	return result, nil
}

// ListRuns 只读列出当前工作区的历史运行；每条记录均经过完整事件回放，按
// 创建时间降序、同时间 RunID 升序返回。首次使用尚无日志时返回空列表。
func (service Service) ListRuns(ctx context.Context, workspacePath, stateDir string, configuration config.Config) ([]RunHistoryItem, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_, workspaceHash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return nil, err
	}
	journal, err := openReadOnlyJournalContext(ctx, stateDir, workspaceHash)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []RunHistoryItem{}, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sqlite.IsStateCorruptError(err) {
			return nil, fmt.Errorf("%w: open run journal: %w", ErrStateCorrupt, err)
		}
		return nil, err
	}
	defer journal.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	items, err := listRunHistory(ctx, journal, workspaceHash)
	if err != nil {
		return nil, err
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].CreatedAt.Equal(items[right].CreatedAt) {
			return items[left].ID < items[right].ID
		}
		return items[left].CreatedAt.After(items[right].CreatedAt)
	})
	return items, nil
}

// listRunHistory 从已打开的日志完整回放当前工作区运行；调用方决定排序与
// 失败分类与完成证据。只读，不依赖当前配置漂移与否。
func (service Service) Evidence(ctx context.Context, workspacePath, stateDir string, configuration config.Config, runID domain.RunID) (contract.Result, error) {
	_, hash, err := workspaceFor(workspacePath, configuration)
	if err != nil {
		return contract.Result{}, err
	}
	journal, err := openReadOnlyJournal(stateDir, hash)
	if err != nil {
		return contract.Result{}, err
	}
	defer journal.Close()
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return contract.Result{}, err
	}
	if len(events) == 0 {
		return contract.Result{}, fmt.Errorf("run %s not found", runID)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return contract.Result{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	if err := checkRunArtifactReferences(events, stateDir, hash); err != nil {
		return contract.Result{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	result, err := runResultFromEvents(events, projection, stateDir, hash)
	if err != nil {
		return contract.Result{}, fmt.Errorf("%w: %v", ErrStateCorrupt, err)
	}
	return result, nil
}

// Resume 在校验冻结配置、日志和制品后恢复等待审批或普通运行；返回前关闭
