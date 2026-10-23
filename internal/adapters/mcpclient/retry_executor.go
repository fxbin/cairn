// retry_executor.go 把 RetryJitter 接进 MCP 工具重试路径：仅幂等工具
// 自动重试，指数退避 + 抖动，上限次数内成功即返回。
// Author: fxbin
package mcpclient

import (
	"context"
	"math/rand"
	"time"

	"github.com/fxbin/cairn/internal/provider"
)

// RetryExecutor 按策略执行带退避的重试。
type RetryExecutor struct {
	Policy provider.RetryJitterConfig
	Retry  *RetryPolicy
	Rand   *rand.Rand
}

// NewRetryExecutor 创建执行器；缺省使用保守策略。
func NewRetryExecutor() *RetryExecutor {
	return &RetryExecutor{
		Policy: provider.DefaultRetryJitterConfig(),
		Retry:  NewRetryPolicy(),
		Rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// Do 执行带重试的函数；仅幂等工具重试。
func (executor *RetryExecutor) Do(ctx context.Context, toolName string, fn func(context.Context) error) error {
	if executor == nil || executor.Retry == nil {
		return fn(ctx)
	}
	if !executor.Retry.AllowRetry(toolName) {
		return fn(ctx)
	}
	attempt := 0
	for {
		err := fn(ctx)
		if err == nil {
			return nil
		}
		if !executor.Policy.ShouldRetry(attempt) {
			return err
		}
		delay := executor.Policy.BackoffDelay(attempt, executor.Rand)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		attempt++
	}
}
