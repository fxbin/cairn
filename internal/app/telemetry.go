// 本文件装配可选的语义观测导出面：配置门、事件接线、导出统计与运行边界收束。
//
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/telemetry"
)

// 语义观测导出面的状态取值直接来自 telemetry 包的接线判定；doctor 只报告
// 这些状态，绝不据此宣称已经成功采集到观测数据。
const (
	// TelemetrySemanticDisabled 表示语义观测导出未被显式启用（默认状态）。
	TelemetrySemanticDisabled = telemetry.ExportStatusDisabled
	// TelemetrySemanticSDKDisabled 表示被 OTel 全局关闭开关禁用。
	TelemetrySemanticSDKDisabled = telemetry.ExportStatusSDKDisabled
	// TelemetrySemanticConfigError 表示显式启用但连接事实不受支持。
	TelemetrySemanticConfigError = telemetry.ExportStatusConfigError
	// TelemetrySemanticNoSignal 表示接线就绪但两个信号都被 exporter 选择关闭。
	TelemetrySemanticNoSignal = telemetry.ExportStatusNoSignal
	// TelemetrySemanticInstrumented 表示导出接线就绪且至少一个信号会被发送；
	// 它只说明观测面就绪，不代表任何一次导出已经成功。
	TelemetrySemanticInstrumented = telemetry.ExportStatusInstrumented
)

// telemetryFlushTimeout 是运行边界处收束待导出快照的上限。超时只说明观测
// 数据没有全部送达，绝不改变运行结果或终态。
const telemetryFlushTimeout = 2 * time.Second

// telemetryStatsFileName 是工作区级语义导出统计的文件名；它与运行日志、
// 制品库分开，只承载计数。
const telemetryStatsFileName = "telemetry.json"

// newTelemetryRuntime 装配可选语义观测运行时，并给出同一次接线判定。
//
// 判定只有一处：telemetry.ResolvePlan。未启用、被 OTEL_SDK_DISABLED 全局
// 关闭、连接事实不合法或两个信号都被关闭时都不装配任何导出器：运行照常
// 执行且不导出，原因在 doctor 的 semantic 状态与原因字段可见。
func newTelemetryRuntime(configuration config.Config) (*telemetry.Runtime, telemetry.Plan, error) {
	plan := telemetry.ResolvePlan(configuration.Telemetry.Enabled)
	if plan.Status != telemetry.ExportStatusInstrumented {
		return nil, plan, nil
	}
	exporter, err := telemetry.NewOTLPExporter(plan)
	if err != nil {
		// 判定与端点校验同源，这里失败说明判定与实现不一致：放弃导出而不是
		// 发出不受支持的请求。
		return nil, plan, err
	}
	async := telemetry.NewAsyncExporter(exporter, telemetry.DefaultExportQueueSize)
	return telemetry.NewRuntime(async, telemetryLimits(configuration), configuration.Telemetry.EffectiveSamplePercent()), plan, nil
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

// attachTelemetryStatsRecorder 让运行边界收束时把导出统计写入工作区状态
// 目录，使导出失败与丢弃在诊断面可见。
func attachTelemetryStatsRecorder(runtime *telemetry.Runtime, stateDir, workspaceHash string) {
	if runtime == nil {
		return
	}
	runtime.SetStatsRecorder(func(stats telemetry.ExportStats) {
		_ = persistTelemetryStats(stateDir, workspaceHash, stats)
	})
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

// telemetrySemanticPlan 给出语义观测导出面的接线判定。它与运行路径共用
// telemetry.ResolvePlan，因此 doctor 报告的状态与运行时是否导出永远一致。
func telemetrySemanticPlan(configuration config.Config) telemetry.Plan {
	return telemetry.ResolvePlan(configuration.Telemetry.Enabled)
}

// TelemetryExportRecord 是语义导出统计的落盘与投影形态：只有计数与时间戳，
// 没有任务内容、路径取值或凭据。
type TelemetryExportRecord struct {
	Exported  int       `json:"exported"`
	Failed    int       `json:"failed"`
	Dropped   int       `json:"dropped"`
	UpdatedAt time.Time `json:"updated_at"`
}

// telemetryStatsPath 返回工作区级语义导出统计的落盘路径。
func telemetryStatsPath(stateDir, workspaceHash string) string {
	if stateDir == "" {
		stateDir = DefaultStateDir()
	}
	return filepath.Join(stateDir, "workspaces", workspaceHash, telemetryStatsFileName)
}

// persistTelemetryStats 原子写入本次运行的导出统计。写入失败只说明统计
// 缺失，绝不改变运行结果。
func persistTelemetryStats(stateDir, workspaceHash string, stats telemetry.ExportStats) error {
	if workspaceHash == "" {
		return nil
	}
	path := telemetryStatsPath(stateDir, workspaceHash)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	record := TelemetryExportRecord{Exported: stats.Exported, Failed: stats.Failed, Dropped: stats.Dropped, UpdatedAt: time.Now().UTC()}
	encoded, err := json.Marshal(record)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".telemetry-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.Write(encoded); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}

// readTelemetryStats 读取工作区级语义导出统计。文件缺失或损坏时返回 nil，
// 不影响诊断的其他结论；doctor 只读，不创建任何状态。
func readTelemetryStats(stateDir, workspaceHash string) *TelemetryExportRecord {
	if workspaceHash == "" {
		return nil
	}
	content, err := os.ReadFile(telemetryStatsPath(stateDir, workspaceHash))
	if err != nil {
		return nil
	}
	var record TelemetryExportRecord
	if json.Unmarshal(content, &record) != nil {
		return nil
	}
	return &record
}

// telemetrySemanticSignals 把接线判定给出的信号列表拼成稳定的展示串。
func telemetrySemanticSignals(plan telemetry.Plan) string {
	return strings.Join(plan.Signals, ",")
}
