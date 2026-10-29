// 本文件装配可选的语义观测导出面：配置门、事件接线与运行边界收束。
//
// Author: fxbin
package app

import (
	"context"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/telemetry"
)

// 语义观测导出面的接线状态取值。doctor 只报告接线事实，绝不据此宣称已经
// 成功采集到观测数据。
const (
	// TelemetrySemanticDisabled 表示语义观测导出未启用（默认状态）。
	TelemetrySemanticDisabled = "disabled"
	// TelemetrySemanticInstrumented 表示导出已接线且连接事实合法；它只说明
	// 观测面就绪，不代表任何一次导出已经成功。
	TelemetrySemanticInstrumented = "instrumented"
	// TelemetrySemanticConfigError 表示显式启用但连接事实不受支持，导出
	// 失败关闭，运行本身不受影响。
	TelemetrySemanticConfigError = "config_error"
)

// telemetryFlushTimeout 是运行边界处收束待导出快照的上限。超时只说明观测
// 数据没有全部送达，绝不改变运行结果或终态。
const telemetryFlushTimeout = 2 * time.Second

// newTelemetryRuntime 装配可选语义观测运行时。未启用时返回 nil，运行路径与
// 网络行为与既有实现完全一致；连接事实非法时返回错误并且不装配任何导出器，
// 观测面失败不阻塞运行。
func newTelemetryRuntime(configuration config.Config) (*telemetry.Runtime, error) {
	if !configuration.Telemetry.Enabled {
		return nil, nil
	}
	exportConfig, err := telemetry.OTLPConfigFromEnv()
	if err != nil {
		return nil, err
	}
	exporter, err := telemetry.NewOTLPExporter(exportConfig)
	if err != nil {
		return nil, err
	}
	async := telemetry.NewAsyncExporter(exporter, telemetry.DefaultExportQueueSize)
	return telemetry.NewRuntime(async, telemetryLimits(configuration), configuration.Telemetry.EffectiveSamplePercent()), nil
}

// telemetryLimits 把冻结配置的预算上限投影为观测面的消耗上限。
func telemetryLimits(configuration config.Config) telemetry.Limits {
	limits := telemetry.Limits{
		MaxTurns:        configuration.Budget.MaxTurns,
		MaxToolCalls:    configuration.Budget.MaxToolCalls,
		MaxInputTokens:  configuration.Budget.MaxInputTokens,
		MaxOutputTokens: configuration.Budget.MaxOutputTokens,
	}
	if wallTime, err := configuration.WallTime(); err == nil {
		limits.MaxWallTimeMS = wallTime.Milliseconds()
	}
	return limits
}

// composeTelemetrySink 把已成功持久化的事件同时投递给调用方 sink 与语义
// 观测运行时。运行时缺席时原样返回调用方 sink，行为与既有实现完全一致。
func composeTelemetrySink(sink EventSink, runtime *telemetry.Runtime) EventSink {
	if runtime == nil {
		return sink
	}
	return func(event domain.Event) {
		if sink != nil {
			sink(event)
		}
		runtime.Observe(event)
	}
}

// flushTelemetry 在运行边界处收束待导出的快照。超时或导出失败只说明观测
// 数据缺失，调用方不得据此改变运行结果。
func flushTelemetry(runtime *telemetry.Runtime) {
	if runtime == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), telemetryFlushTimeout)
	defer cancel()
	_ = runtime.Flush(ctx)
}

// closeTelemetry 关闭观测面的后台导出队列；它只影响观测面，不改变运行终态。
func closeTelemetry(runtime *telemetry.Runtime) {
	if runtime == nil {
		return
	}
	_ = runtime.Close()
}

// telemetrySemanticStatus 给出语义观测导出面的接线状态：未启用为 disabled，
// 启用且连接事实合法为 instrumented（不代表已成功采集），连接事实非法为
// config_error。
func telemetrySemanticStatus(configuration config.Config) string {
	if !configuration.Telemetry.Enabled {
		return TelemetrySemanticDisabled
	}
	if _, err := telemetry.OTLPConfigFromEnv(); err != nil {
		return TelemetrySemanticConfigError
	}
	return TelemetrySemanticInstrumented
}
