// retry_jitter_test 锁定指数退避、上限与抖动范围。
//
// Author: fxbin
package provider

import (
	"math/rand"
	"testing"
	"time"
)

// TestBackoffDelayExponential 锁定指数增长与上限。
func TestBackoffDelayExponential(t *testing.T) {
	config := RetryJitterConfig{MaxAttempts: 5, BaseDelay: 100 * time.Millisecond, MaxDelay: 1 * time.Second, JitterRatio: 0}
	rng := rand.New(rand.NewSource(1))
	if got := config.BackoffDelay(0, rng); got != 100*time.Millisecond {
		t.Fatalf("首次应 100ms，得到 %v", got)
	}
	if got := config.BackoffDelay(1, rng); got != 200*time.Millisecond {
		t.Fatalf("第二次应 200ms，得到 %v", got)
	}
	if got := config.BackoffDelay(4, rng); got != 1*time.Second {
		t.Fatalf("应受 MaxDelay 上限，得到 %v", got)
	}
}

// TestShouldRetryCapsAttempts 锁定重试上限。
func TestShouldRetryCapsAttempts(t *testing.T) {
	config := RetryJitterConfig{MaxAttempts: 3}
	if !config.ShouldRetry(0) || !config.ShouldRetry(2) {
		t.Fatal("前 3 次应可重试")
	}
	if config.ShouldRetry(3) {
		t.Fatal("超过上限不应重试")
	}
}

// TestJitterStaysWithinRange 锁定抖动在 ±JitterRatio 内。
func TestJitterStaysWithinRange(t *testing.T) {
	config := RetryJitterConfig{MaxAttempts: 3, BaseDelay: 100 * time.Millisecond, MaxDelay: 10 * time.Second, JitterRatio: 0.3}
	rng := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		delay := config.BackoffDelay(1, rng)
		if delay < 140*time.Millisecond || delay > 260*time.Millisecond {
			t.Fatalf("抖动超出范围: %v", delay)
		}
	}
}
