package ports

import (
	"context"

	"github.com/fxbin/cairn/internal/workflow"
)

// WorkflowStore 定义工作流事件链的追加、加载、枚举与完整性校验端口；
// 与 Journal 分离的执行链事实源（ADR-0021），不得反向决定运行终态或权限。
type WorkflowStore interface {
	Append(ctx context.Context, expectedSeq uint64, draft workflow.EventDraft) (workflow.Event, error)
	Load(ctx context.Context, id workflow.ID) ([]workflow.Event, error)
	WorkflowIDs(ctx context.Context, workspaceHash string) ([]workflow.ID, error)
	Check(ctx context.Context) error
}
