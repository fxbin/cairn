// safety_event.go 记录工具被策略/审批/沙箱阻止或要求确认的可观测安全事件；
// 只做事实投影，不改变授权决策。
// Author: fxbin
package policy

import (
	"sync"
	"time"
)

// SafetyAction 是安全事件的动作类型。
type SafetyAction string

const (
	// SafetyBlocked 表示被拒绝执行。
	SafetyBlocked SafetyAction = "blocked"
	// SafetyPrompted 表示需要人工审批。
	SafetyPrompted SafetyAction = "prompted"
	// SafetyHooked 表示被 hook 拦截。
	SafetyHooked SafetyAction = "hooked"
	// SafetySandboxed 表示被沙箱拦截。
	SafetySandboxed SafetyAction = "sandboxed"
)

// SafetyEvent 是一条可观测安全事实。
type SafetyEvent struct {
	ToolName   string       `json:"tool_name"`
	Action     SafetyAction `json:"action"`
	RuleID     string       `json:"rule_id,omitempty"`
	Summary    string       `json:"summary,omitempty"`
	OccurredAt time.Time    `json:"occurred_at"`
}

// SafetyLog 保留最近安全事件；并发安全，仅供诊断投影。
type SafetyLog struct {
	mu     sync.RWMutex
	events []SafetyEvent
	limit  int
}

// NewSafetyLog 创建有界日志；limit <=0 使用默认 64。
func NewSafetyLog(limit int) *SafetyLog {
	if limit <= 0 {
		limit = 64
	}
	return &SafetyLog{limit: limit}
}

// Record 追加事件（新到旧）。
func (log *SafetyLog) Record(event SafetyEvent) {
	if event.OccurredAt.IsZero() {
		event.OccurredAt = time.Now().UTC()
	}
	log.mu.Lock()
	defer log.mu.Unlock()
	log.events = append([]SafetyEvent{event}, log.events...)
	if len(log.events) > log.limit {
		log.events = log.events[:log.limit]
	}
}

// Snapshot 返回事件副本。
func (log *SafetyLog) Snapshot() []SafetyEvent {
	log.mu.RLock()
	defer log.mu.RUnlock()
	out := make([]SafetyEvent, len(log.events))
	copy(out, log.events)
	return out
}

// CountByAction 统计各动作数量。
func (log *SafetyLog) CountByAction() map[SafetyAction]int {
	log.mu.RLock()
	defer log.mu.RUnlock()
	counts := make(map[SafetyAction]int)
	for _, event := range log.events {
		counts[event.Action]++
	}
	return counts
}
