// 本文件定义可选语义观测导出的配置面。
//
// Author: fxbin
package config

import "fmt"

// DefaultTelemetrySamplePercent 是语义观测导出的默认抽样比例：导出全部运行。
const DefaultTelemetrySamplePercent = 100

// TelemetryConfig 控制可选的语义观测导出。默认关闭：关闭时运行路径、网络
// 行为与既有实现完全一致，既不会建立连接也不会产生任何网络流量。
//
// 该块是运维字段，不进入配置指纹：它只决定观测数据是否外发，不改变执行
// 语义、权限边界、预算判定或验证可信等级。导出端点与认证只从标准 OTel
// 环境变量读取，配置面不携带端点与凭据。
type TelemetryConfig struct {
	// Enabled 显式开启语义观测导出。
	Enabled bool `yaml:"enabled"`
	// SamplePercent 是按运行标识稳定抽样的导出比例（1-100）。抽样判定由
	// 运行标识派生，实时采集与离线重建使用同一判定，不会产生分歧。
	SamplePercent int `yaml:"sample_percent"`
}

// EffectiveSamplePercent 返回生效的抽样比例；未声明（0）按默认值处理。
func (telemetry TelemetryConfig) EffectiveSamplePercent() int {
	if telemetry.SamplePercent <= 0 {
		return DefaultTelemetrySamplePercent
	}
	return telemetry.SamplePercent
}

// validateTelemetry 校验语义观测配置：抽样比例必须落在 1-100，未声明的 0
// 视为使用默认值。非法取值失败关闭，不静默改写为默认。
func validateTelemetry(telemetry TelemetryConfig) error {
	if telemetry.SamplePercent < 0 || telemetry.SamplePercent > 100 {
		return fmt.Errorf("telemetry.sample_percent must be within 1 and 100")
	}
	return nil
}
