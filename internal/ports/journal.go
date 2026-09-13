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

// RunLister 定义跨运行的只读标识枚举能力；结果按 RunID 升序排列并由调用方独占。
type RunLister interface {
	RunIDs(ctx context.Context) ([]domain.RunID, error)
}
