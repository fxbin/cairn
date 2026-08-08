package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/fxbin/cairn/internal/domain"
)

// Journal 是用于测试和进程内运行的并发安全内存事件日志。
type Journal struct {
	mu     sync.Mutex
	events map[domain.RunID][]domain.Event
}

// New 创建空的内存事件日志。
func New() *Journal { return &Journal{events: make(map[domain.RunID][]domain.Event)} }

// Append 在预期序号匹配时追加事件，避免并发写入覆盖既有日志。
func (journal *Journal) Append(ctx context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return domain.Event{}, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	events := journal.events[draft.RunID]
	if uint64(len(events)) != expectedSeq {
		return domain.Event{}, fmt.Errorf("journal conflict for %s: got expected seq %d, current %d", draft.RunID, expectedSeq, len(events))
	}
	previousHash := domain.GenesisHash
	if len(events) > 0 {
		previousHash = events[len(events)-1].EventHash
	}
	event, err := domain.NewEvent(draft, expectedSeq+1, previousHash)
	if err != nil {
		return domain.Event{}, err
	}
	journal.events[draft.RunID] = append(events, event)
	return event, nil
}

// Load 返回指定运行的事件副本，调用者不能修改日志内部切片。
func (journal *Journal) Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	events := journal.events[runID]
	return append([]domain.Event(nil), events...), nil
}

// NonTerminalRuns 重放同一工作区中尚未终态的全部运行。
func (journal *Journal) NonTerminalRuns(ctx context.Context, workspaceHash string) ([]domain.RunProjection, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	var runs []domain.RunProjection
	for _, events := range journal.events {
		projection, err := domain.Replay(events)
		if err != nil {
			return nil, err
		}
		if projection.WorkspaceHash == workspaceHash && !projection.Terminal() {
			runs = append(runs, projection)
		}
	}
	return runs, nil
}

// Check 验证内存日志中每条运行的事件哈希链。
func (journal *Journal) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	journal.mu.Lock()
	defer journal.mu.Unlock()
	for runID, events := range journal.events {
		if err := domain.VerifyChain(events); err != nil {
			return fmt.Errorf("check run %s: %w", runID, err)
		}
	}
	return nil
}
