// queue.go 定义 workspace 级持久后续队列的独立投影：三态
// pending → linked{target_run_id} | cancelled{reason}。队列事实源仍是
// Event Journal 的 queue 命名空间事件；本文件只做只读投影，不写日志。
//
// Author: fxbin
package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

// QueueRunID 是队列命名空间的 RunID 哨兵；不对应任何真实 run。
const QueueRunID RunID = "queue"

// QueueItemState 是队列项三态。
type QueueItemState string

const (
	QueueItemPending   QueueItemState = "pending"
	QueueItemLinked    QueueItemState = "linked"
	QueueItemCancelled QueueItemState = "cancelled"
)

// QueueItem 是队列项的只读投影。
type QueueItem struct {
	InputID     string          `json:"input_id"`
	State       QueueItemState  `json:"state"`
	ContentRef  json.RawMessage `json:"content_ref,omitempty"`
	TargetRunID RunID           `json:"target_run_id,omitempty"`
	Reason      string          `json:"reason,omitempty"`
	SubmittedAt time.Time       `json:"submitted_at,omitempty"`
	ResolvedAt  time.Time       `json:"resolved_at,omitempty"`
}

// QueueProjection 是队列命名空间事件的确定性投影。
type QueueProjection struct {
	Items map[string]*QueueItem
}

// ProjectQueue 从 queue 命名空间事件重建队列投影；顺序敏感，后事件覆盖先事件。
// 先完整校验事件链（序号/哈希/schema）再投影，与 run 链同级完整性保护。
func ProjectQueue(events []Event) (QueueProjection, error) {
	if len(events) > 0 {
		if err := VerifyChain(events); err != nil {
			return QueueProjection{}, fmt.Errorf("queue event chain: %w", err)
		}
	}
	projection := QueueProjection{Items: make(map[string]*QueueItem)}
	for _, event := range events {
		if event.RunID != QueueRunID {
			return QueueProjection{}, fmt.Errorf("queue projection received non-queue event from run %s", event.RunID)
		}
		if event.WorkspaceHash == "" {
			return QueueProjection{}, fmt.Errorf("queue event missing workspace hash")
		}
		switch event.Type {
		case EventQueueInputSubmitted:
			var payload struct {
				InputID    string          `json:"input_id"`
				ContentRef json.RawMessage `json:"content_ref"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return QueueProjection{}, fmt.Errorf("decode queue_input_submitted: %w", err)
			}
			if payload.InputID == "" {
				return QueueProjection{}, fmt.Errorf("queue_input_submitted missing input_id")
			}
			if existing, ok := projection.Items[payload.InputID]; ok && existing.State != QueueItemPending {
				// 已终结的项不允许被重复提交复活。
				return QueueProjection{}, fmt.Errorf("queue input %s already %s", payload.InputID, existing.State)
			}
			projection.Items[payload.InputID] = &QueueItem{
				InputID: payload.InputID, State: QueueItemPending,
				ContentRef: payload.ContentRef, SubmittedAt: event.OccurredAt,
			}
		case EventQueueInputConsumed:
			var payload struct {
				InputID     string `json:"input_id"`
				Outcome     string `json:"outcome"`
				TargetRunID string `json:"target_run_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return QueueProjection{}, fmt.Errorf("decode queue_input_consumed: %w", err)
			}
			item, exists := projection.Items[payload.InputID]
			if !exists {
				return QueueProjection{}, fmt.Errorf("queue input %s consumed before submit", payload.InputID)
			}
			if item.State != QueueItemPending {
				return QueueProjection{}, fmt.Errorf("queue input %s consumed from state %s", payload.InputID, item.State)
			}
			item.State = QueueItemLinked
			item.TargetRunID = RunID(payload.TargetRunID)
			item.ResolvedAt = event.OccurredAt
		case EventQueueInputCancelled:
			var payload struct {
				InputID string `json:"input_id"`
				Reason  string `json:"reason"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return QueueProjection{}, fmt.Errorf("decode queue_input_cancelled: %w", err)
			}
			item, exists := projection.Items[payload.InputID]
			if !exists {
				return QueueProjection{}, fmt.Errorf("queue input %s cancelled before submit", payload.InputID)
			}
			if item.State != QueueItemPending {
				return QueueProjection{}, fmt.Errorf("queue input %s cancelled from state %s", payload.InputID, item.State)
			}
			item.State = QueueItemCancelled
			item.Reason = payload.Reason
			item.ResolvedAt = event.OccurredAt
		default:
			return QueueProjection{}, fmt.Errorf("unexpected event type %q in queue namespace", event.Type)
		}
	}
	return projection, nil
}

// Pending 返回待消费输入 ID（按提交时间稳定排序由调用方决定）。
func (projection QueueProjection) Pending() []*QueueItem {
	pending := make([]*QueueItem, 0)
	for _, item := range projection.Items {
		if item.State == QueueItemPending {
			pending = append(pending, item)
		}
	}
	return pending
}
