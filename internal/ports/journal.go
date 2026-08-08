package ports

import (
	"context"

	"github.com/fxbin/cairn/internal/domain"
)

// Journal 定义事件的追加、加载、恢复检索与完整性校验端口。
type Journal interface {
	Append(ctx context.Context, expectedSeq uint64, draft domain.EventDraft) (domain.Event, error)
	Load(ctx context.Context, runID domain.RunID) ([]domain.Event, error)
	NonTerminalRuns(ctx context.Context, workspaceHash string) ([]domain.RunProjection, error)
	Check(ctx context.Context) error
}
