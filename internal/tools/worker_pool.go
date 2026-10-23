// worker_pool.go 实现有界并行 worker 池：只读工具组并行执行，超出上限排队
// 而非失败；与 PlanParallelGroups 配合使用。
// Author: fxbin
package tools

import (
	"context"
	"fmt"
	"sync"
)

// DefaultMaxWorkers 是并行 worker 默认上限。
const DefaultMaxWorkers = 4

// TaskResult 是单个任务的执行结果。
type TaskResult struct {
	Index int
	Value any
	Err   error
}

// WorkerPool 有界并行执行；超额任务排队等待，不失败。
type WorkerPool struct {
	maxWorkers int
}

// NewWorkerPool 创建池；maxWorkers <=0 时使用默认上限。
func NewWorkerPool(maxWorkers int) *WorkerPool {
	if maxWorkers <= 0 {
		maxWorkers = DefaultMaxWorkers
	}
	return &WorkerPool{maxWorkers: maxWorkers}
}

// Run 并行执行 tasks；返回结果顺序与输入一致。
// ctx 取消时立即返回已收集结果与上下文错误。
func (pool *WorkerPool) Run(ctx context.Context, tasks []func(context.Context) (any, error)) []TaskResult {
	results := make([]TaskResult, len(tasks))
	if len(tasks) == 0 {
		return results
	}
	sem := make(chan struct{}, pool.maxWorkers)
	var wg sync.WaitGroup
	for index, task := range tasks {
		wg.Add(1)
		go func(index int, task func(context.Context) (any, error)) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[index] = TaskResult{Index: index, Err: ctx.Err()}
				return
			}
			value, err := task(ctx)
			results[index] = TaskResult{Index: index, Value: value, Err: err}
		}(index, task)
	}
	wg.Wait()
	return results
}

// MaxWorkers 返回池上限。
func (pool *WorkerPool) MaxWorkers() int { return pool.maxWorkers }

// ErrPoolClosed 表示池已关闭。
var ErrPoolClosed = fmt.Errorf("worker pool closed")
