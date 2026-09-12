// Package buildinfo 承载编译期固化的构建形态事实：构建 flavor 与可选观测
// 工具链身份。默认值描述 plain 构建；otelc flavor 由构建脚本经 -ldflags 在
// 同一源码上注入三个变量，运行时不可变。遥测端点与认证永远只从环境读取，
// 本包不得承载任何端点、密钥或路径值。
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

// TelemetryMode 报告运行级 OTLP 遥测导出的真实状态，供诊断如实展示：
// unavailable 表示 plain 构建无观测探针；disabled 表示 otelc 构建且用户经
// OTEL_SDK_DISABLED=true 显式关闭；otlp 表示 otelc 构建且注入 SDK 按标准
// OTel 环境变量运行（端点等仅从环境读取，本状态不携带端点值）。
func TelemetryMode() string {
	if Flavor != "otelc" {
		return "unavailable"
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return "disabled"
	}
	return "otlp"
}
