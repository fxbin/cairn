// retry_state.go 记录 Provider 调用的可见重试状态，供诊断与界面展示；
// 不发起网络探测，不改变重试策略。
// Author: fxbin
package provider

import (
	"sync"
	"time"
)

// RetryPhase 描述当前重试阶段。
type RetryPhase string

const (
	// RetryPhaseIdle 表示无进行中的重试。
	RetryPhaseIdle RetryPhase = "idle"
	// RetryPhaseRetrying 表示失败后正在等待再次调用。
	RetryPhaseRetrying RetryPhase = "retrying"
	// RetryPhaseGaveUp 表示已达重试上限并放弃。
	RetryPhaseGaveUp RetryPhase = "gave_up"
)

// RetryState 是单次调用的可见重试快照。
type RetryState struct {
	Phase      RetryPhase `json:"phase"`
	Attempts   int        `json:"attempts"`
	LastError  string     `json:"last_error,omitempty"`
	NextAt     time.Time  `json:"next_at,omitempty"`
	LastUpdate time.Time  `json:"last_update"`
}

// RetryTracker 汇总各 operation 的重试状态；并发安全，只读展示用。
type RetryTracker struct {
	mu     sync.RWMutex
	states map[string]RetryState
}

// NewRetryTracker 创建空跟踪器。
func NewRetryTracker() *RetryTracker {
	return &RetryTracker{states: make(map[string]RetryState)}
}

// BeginRetrying 记录一次失败后进入重试。
func (tracker *RetryTracker) BeginRetrying(operationID, lastError string, attempts int, nextAt time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.states[operationID] = RetryState{
		Phase:      RetryPhaseRetrying,
		Attempts:   attempts,
		LastError:  lastError,
		NextAt:     nextAt,
		LastUpdate: time.Now().UTC(),
	}
}

// MarkGaveUp 记录达到上限并放弃。
func (tracker *RetryTracker) MarkGaveUp(operationID, lastError string, attempts int) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.states[operationID] = RetryState{
		Phase:      RetryPhaseGaveUp,
		Attempts:   attempts,
		LastError:  lastError,
		LastUpdate: time.Now().UTC(),
	}
}

// MarkIdle 清除重试状态（成功或无需重试）。
func (tracker *RetryTracker) MarkIdle(operationID string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	delete(tracker.states, operationID)
}

// Snapshot 返回当前全部重试状态副本。
func (tracker *RetryTracker) Snapshot() map[string]RetryState {
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	out := make(map[string]RetryState, len(tracker.states))
	for id, state := range tracker.states {
		out[id] = state
	}
	return out
}

// ActiveCount 返回处于 retrying 的数量，供诊断水位展示。
func (tracker *RetryTracker) ActiveCount() int {
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	count := 0
	for _, state := range tracker.states {
		if state.Phase == RetryPhaseRetrying {
			count++
		}
	}
	return count
}
