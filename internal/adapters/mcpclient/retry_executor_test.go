// retry_executor_test 锁定幂等工具重试与非幂等失败关闭。
//
// Author: fxbin
package mcpclient

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/provider"
)

// TestRetryExecutorRetriesIdempotent 锁定幂等工具失败后重试。
func TestRetryExecutorRetriesIdempotent(t *testing.T) {
	executor := &RetryExecutor{
		Policy: provider.RetryJitterConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond, JitterRatio: 0},
		Retry:  NewRetryPolicy(),
		Rand:   rand.New(rand.NewSource(1)),
	}
	calls := 0
	err := executor.Do(context.Background(), "tool_search", func(context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil || calls != 3 {
		t.Fatalf("应重试至成功: err=%v calls=%d", err, calls)
	}
}

// TestRetryExecutorNonIdempotentNoRetry 锁定非幂等工具不重试。
func TestRetryExecutorNonIdempotentNoRetry(t *testing.T) {
	executor := NewRetryExecutor()
	executor.Policy = provider.RetryJitterConfig{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: 10 * time.Millisecond}
	calls := 0
	err := executor.Do(context.Background(), "web_fetch", func(context.Context) error {
		calls++
		return errors.New("fail")
	})
	if err == nil || calls != 1 {
		t.Fatalf("非幂等不应重试: err=%v calls=%d", err, calls)
	}
}
