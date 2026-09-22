// Package app 的墙钟收尾回归测试锁定四类边界：成功终态回调跨过截止点、
// 失败终态后预算到期、真正运行中预算耗尽、单请求超时但运行预算未耗尽。
//
// Author: fxbin
package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// classifiedTestError 是实现 ports.ClassifiedError 的测试错误，可选地携带
// 底层原因，用于复现 transport 可重试分类与 DeadlineExceeded 错误链并存
// 的真实形态。
type classifiedTestError struct {
	kind      string
	retryable bool
	cause     error
}

func (err *classifiedTestError) Error() string {
	if err.cause != nil {
		return "provider " + err.kind + ": " + err.cause.Error()
	}
	return "provider " + err.kind
}

func (err *classifiedTestError) Unwrap() error { return err.cause }

func (err *classifiedTestError) ErrorClass() string { return err.kind }

func (err *classifiedTestError) IsRetryable() bool { return err.retryable }

// delayingEventProvider 在首个模型请求返回最终文本，并允许在指定事件回调
// 中跨过墙钟截止点，复现"终态已持久化但返回调度晚于预算到期"的时序。
type delayingEventProvider struct {
	eventType domain.EventType
	delay     time.Duration
	seen      bool
}

func (provider *delayingEventProvider) Stream(_ context.Context, _ ports.ModelRequest, emit func(ports.ModelEvent) error) error {
	if err := emit(ports.ModelEvent{Kind: ports.TextDelta, Text: "done"}); err != nil {
		return err
	}
	return emitTestCompletion(emit)
}

// delayedOnEvent 返回在目标事件首次出现时阻塞超过墙钟预算的事件回调。
func (provider *delayingEventProvider) delayedOnEvent(event domain.Event) {
	if provider.seen || event.Type != provider.eventType {
		return
	}
	provider.seen = true
	time.Sleep(provider.delay)
}

// failingStreamProvider 让模型流以不可重试分类失败，驱动 run_failed 终态。
type failingStreamProvider struct {
	eventType domain.EventType
	delay     time.Duration
	seen      bool
}

func (provider *failingStreamProvider) Stream(_ context.Context, _ ports.ModelRequest, _ func(ports.ModelEvent) error) error {
	return &classifiedTestError{kind: "invalid_response", retryable: false}
}

func (provider *failingStreamProvider) delayingOnEvent(event domain.Event) {
	if provider.seen || event.Type != provider.eventType {
		return
	}
	provider.seen = true
	time.Sleep(provider.delay)
}

// hangingProvider 模拟模型请求挂起至墙钟预算耗尽：返回 transport 可重试分类
// 并携带真实预算上下文的 DeadlineExceeded，与适配器包装形态一致。
type hangingProvider struct {
	release chan struct{}
}

func (provider *hangingProvider) Stream(ctx context.Context, _ ports.ModelRequest, _ func(ports.ModelEvent) error) error {
	select {
	case <-provider.release:
		return nil
	case <-ctx.Done():
		return &classifiedTestError{kind: "transport", retryable: true, cause: ctx.Err()}
	}
}

// timeoutLikeProvider 立即返回错误链含 DeadlineExceeded 的 transport 可重试
// 失败，复现单请求超时：错误链满足 errors.Is(DeadlineExceeded) 而运行预算
// 上下文仍然存活。
type timeoutLikeProvider struct{}

func (provider *timeoutLikeProvider) Stream(_ context.Context, _ ports.ModelRequest, _ func(ports.ModelEvent) error) error {
	return &classifiedTestError{kind: "transport", retryable: true, cause: context.DeadlineExceeded}
}

// wallBudgetConfig 返回带指定墙钟预算的 v2 测试配置。
func wallBudgetConfig(wall string) config.Config {
	configuration := withTestProvider(config.Defaults())
	configuration.Budget.MaxWallTime = wall
	return configuration
}

// inspectState 只读投影指定运行的当前状态。
func inspectState(t *testing.T, service Service, workspace, stateDir string, configuration config.Config, runID domain.RunID) domain.RunState {
	t.Helper()
	inspected, err := service.Inspect(context.Background(), workspace, stateDir, configuration, runID, false)
	if err != nil {
		t.Fatal(err)
	}
	return inspected.Projection.State
}

// TestRunCompletedSurvivesCallbackCrossingWallDeadline 锁定：run_completed 已
// 持久化后，即使事件回调跨过墙钟截止点，调用方仍拿到原始成功响应，不会被
// 再次按超时收尾覆盖。
func TestRunCompletedSurvivesCallbackCrossingWallDeadline(t *testing.T) {
	provider := &delayingEventProvider{eventType: domain.EventRunCompleted, delay: 400 * time.Millisecond}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	result, err := service.Run(context.Background(), RunOptions{
		Task: "answer", Workspace: workspace, StateDir: stateDir,
		Config: wallBudgetConfig("150ms"), OnEvent: provider.delayedOnEvent,
	})
	if err != nil {
		t.Fatalf("completed run must not be re-finalized: %v", err)
	}
	if result.Response != "done" {
		t.Fatalf("original response was overwritten: %#v", result)
	}
	if state := inspectState(t, service, workspace, stateDir, wallBudgetConfig("150ms"), result.ID); state != domain.RunCompleted {
		t.Fatalf("state = %s, want completed", state)
	}
}

// TestRunFailedSurvivesWallDeadlineAfterFailure 锁定：run_failed 已持久化后
// 预算才到期，调用方收到原始失败分类，而不是预算耗尽或非法状态迁移错误。
func TestRunFailedSurvivesWallDeadlineAfterFailure(t *testing.T) {
	provider := &failingStreamProvider{eventType: domain.EventRunFailed, delay: 400 * time.Millisecond}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := wallBudgetConfig("150ms")
	result, err := service.Run(context.Background(), RunOptions{
		Task: "answer", Workspace: workspace, StateDir: stateDir,
		Config: configuration, OnEvent: provider.delayingOnEvent,
	})
	if err == nil || !strings.Contains(err.Error(), "invalid_response") {
		t.Fatalf("original failure classification must be preserved: %v", err)
	}
	if strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("failed run must not be re-finalized as budget exhaustion: %v", err)
	}
	if state := inspectState(t, service, workspace, stateDir, configuration, result.ID); state != domain.RunFailed {
		t.Fatalf("state = %s, want failed", state)
	}
}

// TestRunMidFlightWallExhaustionStillFinalizesBudget 锁定：预算真正在运行中
// 耗尽时仍按 wall_time 预算失败收尾，终态保护不吞掉真实的预算耗尽路径。
func TestRunMidFlightWallExhaustionStillFinalizesBudget(t *testing.T) {
	provider := &hangingProvider{release: make(chan struct{})}
	defer close(provider.release)
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := wallBudgetConfig("150ms")
	result, err := service.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err == nil || !strings.Contains(err.Error(), "budget exhausted: wall_time") {
		t.Fatalf("mid-flight wall exhaustion must surface budget error: %v", err)
	}
	if state := inspectState(t, service, workspace, stateDir, configuration, result.ID); state != domain.RunFailed {
		t.Fatalf("state = %s, want failed", state)
	}
}

// TestRunRequestTimeoutWithoutBudgetExhaustionKeepsRunOpen 锁定：单请求超时
// 的错误链即使满足 errors.Is(DeadlineLine)，只要运行预算未耗尽就不得按
// wall_time 收尾，运行保持非终态等待恢复。
func TestRunRequestTimeoutWithoutBudgetExhaustionKeepsRunOpen(t *testing.T) {
	provider := &timeoutLikeProvider{}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return provider, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	configuration := wallBudgetConfig("30s")
	result, err := service.Run(context.Background(), RunOptions{Task: "answer", Workspace: workspace, StateDir: stateDir, Config: configuration})
	if err == nil || !strings.Contains(err.Error(), "model request") {
		t.Fatalf("request timeout must surface as model request error: %v", err)
	}
	if strings.Contains(err.Error(), "budget exhausted") {
		t.Fatalf("alive budget must not be reported as exhausted: %v", err)
	}
	if state := inspectState(t, service, workspace, stateDir, configuration, result.ID); state == domain.RunFailed || state == domain.RunCompleted || state == domain.RunCancelled {
		t.Fatalf("run was finalized as %s, want non-terminal", state)
	}
}
