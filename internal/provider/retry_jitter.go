// retry_jitter.go 实现指数退避与抖动，避免重试风暴；对齐 opencode
// 「capped retries + jitter」语义。
// Author: fxbin
package provider

import (
	"math/rand"
	"time"
)

// RetryJitterConfig 是重试退避配置。
type RetryJitterConfig struct {
	// MaxAttempts 最大重试次数；<=0 使用默认 3。
	MaxAttempts int
	// BaseDelay 基础延迟。
	BaseDelay time.Duration
	// MaxDelay 最大延迟上限。
	MaxDelay time.Duration
	// JitterRatio 抖动比例（0-1），0 表示无抖动。
	JitterRatio float64
}

// DefaultRetryJitterConfig 返回保守默认配置。
func DefaultRetryJitterConfig() RetryJitterConfig {
	return RetryJitterConfig{
		MaxAttempts: 3,
		BaseDelay:   100 * time.Millisecond,
		MaxDelay:    5 * time.Second,
		JitterRatio: 0.3,
	}
}

// BackoffDelay 计算第 attempt 次重试的退避延迟（含抖动）。
func (config RetryJitterConfig) BackoffDelay(attempt int, rng *rand.Rand) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	maxAttempts := config.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	if attempt >= maxAttempts {
		return 0
	}
	base := config.BaseDelay
	if base <= 0 {
		base = 100 * time.Millisecond
	}
	maxDelay := config.MaxDelay
	if maxDelay <= 0 {
		maxDelay = 5 * time.Second
	}
	// 指数退避
	delay := base << uint(attempt)
	if delay > maxDelay {
		delay = maxDelay
	}
	if config.JitterRatio > 0 && rng != nil {
		jitter := time.Duration(float64(delay) * config.JitterRatio * (rng.Float64()*2 - 1))
		delay += jitter
		if delay < 0 {
			delay = 0
		}
		if delay > maxDelay {
			delay = maxDelay
		}
	}
	return delay
}

// ShouldRetry 判断是否还可重试。
func (config RetryJitterConfig) ShouldRetry(attempt int) bool {
	maxAttempts := config.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	return attempt < maxAttempts
}
