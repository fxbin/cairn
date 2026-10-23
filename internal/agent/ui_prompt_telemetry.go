// ui_prompt_telemetry.go 区分 Agent 活跃执行与等待用户输入的时间段；
// 供遥测/扩展统计使用，不影响授权与执行语义。
// Author: fxbin
package agent

import (
	"sync"
	"time"
)

// UIPhase 是 UI 交互阶段。
type UIPhase string

const (
	// UIPhaseActive 表示 Agent 正在执行工具/模型调用。
	UIPhaseActive UIPhase = "active"
	// UIPhasePrompt 表示等待用户输入（审批、问题、配置向导）。
	UIPhasePrompt UIPhase = "prompt"
)

// UIPhaseRecord 是一段阶段的时间跨度。
type UIPhaseRecord struct {
	Phase     UIPhase   `json:"phase"`
	StartedAt time.Time `json:"started_at"`
	EndedAt   time.Time `json:"ended_at,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// UIPhaseTracker 记录活跃/等待阶段时长。
type UIPhaseTracker struct {
	mu      sync.Mutex
	current *UIPhaseRecord
	records []UIPhaseRecord
	limit   int
}

// NewUIPhaseTracker 创建跟踪器；limit <=0 使用默认 64。
func NewUIPhaseTracker(limit int) *UIPhaseTracker {
	if limit <= 0 {
		limit = 64
	}
	return &UIPhaseTracker{limit: limit}
}

// Begin 开始一个阶段；结束上一阶段。
func (tracker *UIPhaseTracker) Begin(phase UIPhase, reason string, at time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if at.IsZero() {
		at = time.Now().UTC()
	}
	if tracker.current != nil {
		tracker.current.EndedAt = at
		tracker.records = append([]UIPhaseRecord{*tracker.current}, tracker.records...)
		if len(tracker.records) > tracker.limit {
			tracker.records = tracker.records[:tracker.limit]
		}
	}
	tracker.current = &UIPhaseRecord{Phase: phase, StartedAt: at, Reason: reason}
}

// End 结束当前阶段。
func (tracker *UIPhaseTracker) End(at time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	if tracker.current == nil {
		return
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}
	tracker.current.EndedAt = at
	tracker.records = append([]UIPhaseRecord{*tracker.current}, tracker.records...)
	if len(tracker.records) > tracker.limit {
		tracker.records = tracker.records[:tracker.limit]
	}
	tracker.current = nil
}

// Snapshot 返回阶段记录（新到旧）。
func (tracker *UIPhaseTracker) Snapshot() []UIPhaseRecord {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	out := make([]UIPhaseRecord, len(tracker.records))
	copy(out, tracker.records)
	if tracker.current != nil {
		out = append([]UIPhaseRecord{*tracker.current}, out...)
	}
	return out
}

// TotalByPhase 统计各阶段累计时长。
func (tracker *UIPhaseTracker) TotalByPhase() map[UIPhase]time.Duration {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	totals := make(map[UIPhase]time.Duration)
	for _, record := range tracker.records {
		if !record.EndedAt.IsZero() {
			totals[record.Phase] += record.EndedAt.Sub(record.StartedAt)
		}
	}
	return totals
}
