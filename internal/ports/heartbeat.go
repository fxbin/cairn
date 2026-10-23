// heartbeat.go 定义 Provider 心跳与可见重试状态端口：探测连通性并暴露
// 最近一次重试事实，供诊断与 TUI 投影；不隐式重放工具调用。
// Author: fxbin
package ports

import (
	"context"
	"time"
)

// HeartbeatProbe 是提供方连通性探测结果。
type HeartbeatProbe struct {
	// OK 表示最近一次探测是否成功。
	OK bool `json:"ok"`
	// Latency 是探测往返耗时；失败时为 0。
	Latency time.Duration `json:"latency"`
	// ErrorClass 是失败分类（network/rate_limited 等）；成功为空。
	ErrorClass string `json:"error_class,omitempty"`
	// CheckedAt 是探测时刻。
	CheckedAt time.Time `json:"checked_at"`
}

// RetryRecord 是一次可见的瞬态重试事实。
type RetryRecord struct {
	// Attempt 是第几次尝试（从 1 起）。
	Attempt int `json:"attempt"`
	// ErrorClass 是触发重试的失败分类。
	ErrorClass string `json:"error_class"`
	// At 是重试决定时刻。
	At time.Time `json:"at"`
}

// HeartbeatReporter 暴露最近心跳与重试状态；零值表示尚无观测。
type HeartbeatReporter interface {
	// Heartbeat 对提供方做一次有界探测。
	Heartbeat(ctx context.Context) HeartbeatProbe
	// RecentRetries 返回最近若干次可见重试记录（新到旧）。
	RecentRetries() []RetryRecord
}
