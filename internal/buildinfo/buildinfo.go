// Package buildinfo 承载编译期固化的构建形态事实：构建 flavor、源码状态与
// 可选观测工具链身份。默认值描述 plain 构建；otelc flavor 由构建脚本经
// -ldflags 在同一源码上注入。遥测端点与认证永远只从环境读取，本包不得承载
// 任何端点、密钥或路径值。
//
// Author: fxbin
package buildinfo

import (
	"os"
	"strings"
)

// Flavor 是当前二进制的构建形态：plain（默认、无观测探针）或 otelc（可选
// 编译期插桩产物）。由构建脚本注入，源码默认值为 plain。
var Flavor = "plain"

// ToolchainVersion 是 otelc 构建所用插桩工具链的固定版本；plain 构建恒为空。
var ToolchainVersion = ""

// InstrumentationDigest 是 otelc 构建实际编译进二进制的插桩集合摘要
// （sha256:…，来源为工具链生成的规则匹配清单）；plain 构建恒为空。
var InstrumentationDigest = ""

// Revision 是构建开始前捕获的源码修订；plain 构建默认留空并由 Go BuildInfo
// 补充，otelc 使用 -buildvcs=false 时由构建脚本显式注入。
var Revision = ""

// Dirty 是构建开始前捕获的工作树状态，取 unknown/true/false；plain 构建默认
// unknown 并由 Go BuildInfo 补充，otelc 由构建脚本显式注入。
var Dirty = "unknown"

// TelemetryMode 报告三个信号 exporter 的非敏感聚合状态。plain 构建恒为
// unavailable；全局禁用优先于 exporter 配置；otelc 下三个 exporter 全为 none
// 时 disabled，任一为 otlp 时 otlp，仅启用 metrics prometheus 时 enabled，配置
// 值与上游一样严格区分大小写且不裁剪，无法识别时返回 config_error。空值采用
// OTel 默认 exporter otlp。
func TelemetryMode() string {
	if Flavor != "otelc" {
		return "unavailable"
	}
	if strings.EqualFold(os.Getenv("OTEL_SDK_DISABLED"), "true") {
		return "disabled"
	}
	modes := []string{
		exporterMode("OTEL_TRACES_EXPORTER", false),
		exporterMode("OTEL_METRICS_EXPORTER", true),
		exporterMode("OTEL_LOGS_EXPORTER", false),
	}
	hasOTLP, hasEnabled := false, false
	for _, mode := range modes {
		switch mode {
		case "otlp":
			hasOTLP = true
		case "prometheus":
			hasEnabled = true
		case "none":
		default:
			return "config_error"
		}
	}
	if hasOTLP {
		return "otlp"
	}
	if hasEnabled {
		return "enabled"
	}
	return "disabled"
}

// exporterMode 按上游 autoexport 的精确语义解析单个信号 exporter；空值采用
// otlp，名称严格区分大小写且不裁剪，仅 metrics 信号额外支持 prometheus。
func exporterMode(name string, allowPrometheus bool) string {
	value := os.Getenv(name)
	if value == "" {
		return "otlp"
	}
	if value == "otlp" || value == "none" || allowPrometheus && value == "prometheus" {
		return value
	}
	return "invalid"
}
