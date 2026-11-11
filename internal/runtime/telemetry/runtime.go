// 本文件实现语义观测运行时：把已持久化事件投影为快照并交给导出器。
//
// Author: fxbin
package telemetry

import (
	"context"
	"sync"

	"github.com/fxbin/cairn/internal/domain"
)

// Runtime 是可选语义观测运行时。它只消费已经成功追加到事件日志的事件，
// 到达终态时导出一个语义快照；导出失败、队列拥塞或接收器不可用都只体现在
// 统计里，永远不改变运行终态、权限判定或验证可信等级。
type Runtime struct {
	aggregator *Aggregator
	exporter   Exporter
	mu         sync.Mutex
	exported   map[domain.RunID]bool
	recorder   func(ExportStats)
}

// NewRuntime 创建语义观测运行时；exporter 为 nil 时运行时只聚合不导出。
func NewRuntime(exporter Exporter, limits Limits, samplePercent int) *Runtime {
	return &Runtime{
		aggregator: NewAggregator(limits, samplePercent),
		exporter:   exporter,
		exported:   make(map[domain.RunID]bool),
	}
}

// Observe 消费一个已成功持久化的事件；运行到达终态时导出一次语义快照。
func (runtime *Runtime) Observe(event domain.Event) {
	if runtime == nil {
		return
	}
	runtime.aggregator.Observe(event)
	if !isTerminalEvent(event.Type) {
		return
	}
	snapshot, ok := runtime.aggregator.Snapshot(event.RunID)
	if !ok {
		return
	}
	runtime.mu.Lock()
	if runtime.exported[event.RunID] {
		runtime.mu.Unlock()
		return
	}
	runtime.exported[event.RunID] = true
	runtime.mu.Unlock()
	if runtime.exporter == nil {
		return
	}
	// 导出会话可能阻塞，交给后台队列；这里只投递，不等待接收器。
	_ = runtime.exporter.Export(context.Background(), snapshot)
}

// Snapshot 返回指定运行的语义快照，供诊断与离线对账使用。
func (runtime *Runtime) Snapshot(runID domain.RunID) (Snapshot, bool) {
	if runtime == nil {
		return Snapshot{}, false
	}
	return runtime.aggregator.Snapshot(runID)
}

// RunIDs 返回已观察到的运行标识，按字典序排列。
func (runtime *Runtime) RunIDs() []domain.RunID {
	if runtime == nil {
		return nil
	}
	return runtime.aggregator.RunIDs()
}

// SetStatsRecorder 装配导出统计的落盘回调。运行边界收束时用它记录成功、
// 失败与丢弃计数，使导出故障在诊断面可见；回调失败不影响运行结果。
func (runtime *Runtime) SetStatsRecorder(recorder func(ExportStats)) {
	if runtime == nil {
		return
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	runtime.recorder = recorder
}

// Stats 返回本次运行的导出统计；导出器不支持统计时返回零值。
func (runtime *Runtime) Stats() ExportStats {
	if runtime == nil || runtime.exporter == nil {
		return ExportStats{}
	}
	provider, supported := runtime.exporter.(interface{ Stats() ExportStats })
	if !supported {
		return ExportStats{}
	}
	return provider.Stats()
}

// Flush 收束已投递的导出；导出器不支持收束时立即返回。超时或失败只记录
// 事实，调用方不得据此改变运行结果。收束完成后把导出统计交给落盘回调，
// 使诊断面能如实看到导出失败与丢弃。
func (runtime *Runtime) Flush(ctx context.Context) error {
	if runtime == nil || runtime.exporter == nil {
		return nil
	}
	var flushErr error
	flusher, supportsFlush := runtime.exporter.(interface{ Flush(context.Context) error })
	if supportsFlush {
		flushErr = flusher.Flush(ctx)
	}
	runtime.mu.Lock()
	recorder := runtime.recorder
	runtime.mu.Unlock()
	if recorder != nil {
		recorder(runtime.Stats())
	}
	return flushErr
}

// Close 关闭运行时持有的导出器（如果有）。它只影响观测面，不改变运行终态；
// 重复调用安全。
func (runtime *Runtime) Close() error {
	if runtime == nil || runtime.exporter == nil {
		return nil
	}
	closer, supportsClose := runtime.exporter.(interface{ Close() error })
	if !supportsClose {
		return nil
	}
	return closer.Close()
}

// Aggregate 离线从完整事件序列重建语义快照。事件序列必须属于同一个运行。
// 它与实时逐条观察使用同一聚合器与同一抽样判定，因此相同事件序列总是得到
// 一致结果：实时采集丢失不影响从 Journal 重建的权威统计。
func Aggregate(events []domain.Event, limits Limits, samplePercent int) (Snapshot, bool) {
	aggregator := NewAggregator(limits, samplePercent)
	for _, event := range events {
		aggregator.Observe(event)
	}
	if len(events) == 0 {
		return Snapshot{}, false
	}
	return aggregator.Snapshot(events[0].RunID)
}

// isTerminalEvent 判断事件类型是否表示运行终态。
func isTerminalEvent(eventType domain.EventType) bool {
	switch eventType {
	case domain.EventRunCompleted, domain.EventRunFailed, domain.EventRunCancelled:
		return true
	default:
		return false
	}
}
