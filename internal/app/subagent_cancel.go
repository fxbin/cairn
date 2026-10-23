// subagent_cancel.go 实现父运行取消向子运行的合作式传播：从父投影收集
// 未终态子身份，对每个子 run 追加 run_cancel_requested；不强杀、不重放工具。
// Author: fxbin
package app

import (
	"context"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// PendingChildRunIDs 返回父投影中尚未完成的子运行标识。
func PendingChildRunIDs(projection domain.RunProjection) []domain.RunID {
	ids := make([]domain.RunID, 0, len(projection.Children))
	for id, record := range projection.Children {
		if record.State == "delegated" {
			ids = append(ids, domain.RunID(id))
		}
	}
	return ids
}

// PropagateSubagentCancel 在父运行取消时向未终态子 run 追加取消请求事实。
// 已终态子 run 跳过；单个失败不阻断其余传播，错误聚合返回。
func PropagateSubagentCancel(ctx context.Context, journal ports.Journal, workspaceHash string, parentRunID domain.RunID, occurredAt time.Time) error {
	if journal == nil || parentRunID == "" {
		return nil
	}
	events, err := journal.Load(ctx, parentRunID)
	if err != nil {
		return fmt.Errorf("load parent for cancel: %w", err)
	}
	if len(events) == 0 {
		return nil
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return fmt.Errorf("replay parent for cancel: %w", err)
	}
	var joined error
	for _, childID := range PendingChildRunIDs(projection) {
		childEvents, err := journal.Load(ctx, childID)
		if err != nil {
			joined = fmt.Errorf("%w; load child %s: %v", ErrSubagentAppend, childID, err)
			continue
		}
		if len(childEvents) == 0 {
			continue
		}
		childProjection, err := domain.Replay(childEvents)
		if err != nil {
			joined = fmt.Errorf("%w; replay child %s: %v", ErrSubagentAppend, childID, err)
			continue
		}
		if childProjection.Terminal() || childProjection.Cancelling() {
			continue
		}
		draft := domain.EventDraft{
			RunID:         childID,
			WorkspaceHash: workspaceHash,
			OccurredAt:    occurredAt,
			Type:          domain.EventRunCancelRequested,
			Payload:       []byte(`{"payload_version":1,"cause":"parent_cancelled"}`),
		}
		if _, err := journal.Append(ctx, childProjection.LastSeq, draft); err != nil {
			joined = fmt.Errorf("%w; cancel child %s: %v", ErrSubagentAppend, childID, err)
		}
	}
	return joined
}
