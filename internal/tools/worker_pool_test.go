// worker_pool_test 锁定有界并行、顺序保真与取消传播。
//
// Author: fxbin
package tools

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// TestWorkerPoolRunsAllTasksInOrder 锁定结果顺序与输入一致。
func TestWorkerPoolRunsAllTasksInOrder(t *testing.T) {
	pool := NewWorkerPool(2)
	tasks := make([]func(context.Context) (any, error), 5)
	for i := range tasks {
		i := i
		tasks[i] = func(context.Context) (any, error) { return i * 10, nil }
	}
	results := pool.Run(context.Background(), tasks)
	for i, result := range results {
		if result.Err != nil || result.Value != i*10 {
			t.Fatalf("下标 %d 结果不符: %+v", i, result)
		}
	}
}

// TestWorkerPoolBoundsConcurrency 锁定并发不超过上限（排队而非失败）。
func TestWorkerPoolBoundsConcurrency(t *testing.T) {
	pool := NewWorkerPool(2)
	var current, peak int64
	tasks := make([]func(context.Context) (any, error), 8)
	for i := range tasks {
		tasks[i] = func(context.Context) (any, error) {
			now := atomic.AddInt64(&current, 1)
			for {
				old := atomic.LoadInt64(&peak)
				if now <= old || atomic.CompareAndSwapInt64(&peak, old, now) {
					break
				}
			}
			time.Sleep(5 * time.Millisecond)
			atomic.AddInt64(&current, -1)
			return nil, nil
		}
	}
	pool.Run(context.Background(), tasks)
	if peak > 2 {
		t.Fatalf("并发峰值 %d 超过上限 2", peak)
	}
}

// TestWorkerPoolCancelled 锁定取消传播到未启动任务。
func TestWorkerPoolCancelled(t *testing.T) {
	pool := NewWorkerPool(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	tasks := []func(context.Context) (any, error){
		func(ctx context.Context) (any, error) {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("should not run")
		},
	}
	results := pool.Run(ctx, tasks)
	if results[0].Err == nil {
		t.Fatal("取消后应返回错误")
	}
}

// TestNewWorkerPoolDefaults 锁定默认上限与 getter。
func TestNewWorkerPoolDefaults(t *testing.T) {
	if pool := NewWorkerPool(0); pool.MaxWorkers() != DefaultMaxWorkers {
		t.Fatalf("默认上限不符: %d", pool.MaxWorkers())
	}
	if pool := NewWorkerPool(8); pool.MaxWorkers() != 8 {
		t.Fatalf("显式上限不符: %d", pool.MaxWorkers())
	}
}
