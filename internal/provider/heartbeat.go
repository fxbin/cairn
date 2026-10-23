// heartbeat.go 提供 Provider 心跳探测与可见重试状态的内存实现；
// 不持久化、不跨进程共享，仅供诊断投影与测试。
// Author: fxbin
package provider

import (
	"context"
	"sync"
	"time"

	"github.com/fxbin/cairn/internal/ports"
)

// maxRetryRecords 限制保留的重试记录条数。
const maxRetryRecords = 16

// HeartbeatTracker 记录心跳与重试；并发安全。
type HeartbeatTracker struct {
	mu      sync.RWMutex
	probe   ports.HeartbeatProbe
	retries []ports.RetryRecord
	ping    func(ctx context.Context) ports.HeartbeatProbe
}

// NewHeartbeatTracker 创建跟踪器；ping 为空时心跳恒为失败（not_configured）。
func NewHeartbeatTracker(ping func(ctx context.Context) ports.HeartbeatProbe) *HeartbeatTracker {
	return &HeartbeatTracker{ping: ping}
}

// Heartbeat 执行一次探测并记录结果。
func (tracker *HeartbeatTracker) Heartbeat(ctx context.Context) ports.HeartbeatProbe {
	start := time.Now()
	var probe ports.HeartbeatProbe
	if tracker.ping == nil {
		probe = ports.HeartbeatProbe{OK: false, ErrorClass: "not_configured", CheckedAt: start.UTC()}
	} else {
		probe = tracker.ping(ctx)
		probe.CheckedAt = start.UTC()
		if probe.OK && probe.Latency == 0 {
			probe.Latency = time.Since(start)
		}
	}
	tracker.mu.Lock()
	tracker.probe = probe
	tracker.mu.Unlock()
	return probe
}

// RecordRetry 追加一条可见重试记录（新到旧保留有限条数）。
func (tracker *HeartbeatTracker) RecordRetry(attempt int, errorClass string, at time.Time) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	tracker.retries = append([]ports.RetryRecord{{
		Attempt:    attempt,
		ErrorClass: errorClass,
		At:         at.UTC(),
	}}, tracker.retries...)
	if len(tracker.retries) > maxRetryRecords {
		tracker.retries = tracker.retries[:maxRetryRecords]
	}
}

// RecentRetries 返回最近重试记录（新到旧）。
func (tracker *HeartbeatTracker) RecentRetries() []ports.RetryRecord {
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	out := make([]ports.RetryRecord, len(tracker.retries))
	copy(out, tracker.retries)
	return out
}

// LastHeartbeat 返回最近一次探测结果；零值表示尚未探测。
func (tracker *HeartbeatTracker) LastHeartbeat() ports.HeartbeatProbe {
	tracker.mu.RLock()
	defer tracker.mu.RUnlock()
	return tracker.probe
}
