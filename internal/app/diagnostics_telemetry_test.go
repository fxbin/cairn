package app

import (
	"testing"

	"github.com/fxbin/cairn/internal/buildinfo"
)

// TestBuildIdentityReflectsBuildInfo 锁定构建身份与编译期事实的接线：flavor、
// 工具链版本与插桩摘要来自 internal/buildinfo 的注入值；plain 二进制如实回退
// 为空工具链事实；身份指纹随 flavor 维度变化且派生值自洽（可进事件链回放）。
func TestBuildIdentityReflectsBuildInfo(t *testing.T) {
	originalFlavor, originalVersion, originalDigest := buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest
	originalRevision, originalDirty := buildinfo.Revision, buildinfo.Dirty
	defer func() {
		buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = originalFlavor, originalVersion, originalDigest
		buildinfo.Revision, buildinfo.Dirty = originalRevision, originalDirty
	}()

	plain := buildIdentityFromSettings(nil)
	if plain.Flavor != "plain" || plain.OtelcVersion != "" || plain.InstrumentationDigest != "" {
		t.Fatalf("plain identity = %+v", plain)
	}
	if plain.Fingerprint != plain.ComputeFingerprint() {
		t.Fatalf("plain fingerprint not self-consistent: %s", plain.Fingerprint)
	}

	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = "otelc", "v1.1.0", digest
	buildinfo.Revision, buildinfo.Dirty = "revision-from-script", "true"
	otelc := buildIdentityFromSettings(nil)
	if otelc.Flavor != "otelc" || otelc.OtelcVersion != "v1.1.0" || otelc.InstrumentationDigest != digest || otelc.Revision != "revision-from-script" || otelc.Dirty != "true" {
		t.Fatalf("otelc identity = %+v", otelc)
	}
	if otelc.Fingerprint == plain.Fingerprint {
		t.Fatal("otelc flavor must change the build fingerprint")
	}
	if identity := otelc; identity.ComputeFingerprint() != identity.Fingerprint {
		t.Fatalf("otelc fingerprint not self-consistent: %s", identity.Fingerprint)
	}
}

// TestBuildIdentityPreservesInvalidInjectedFacts 锁定非法 ldflags 原样进入校验，
// 不得通过清空 otelc 字段静默伪装为合法 plain 构建。
func TestBuildIdentityPreservesInvalidInjectedFacts(t *testing.T) {
	originalFlavor, originalVersion, originalDigest := buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest
	originalRevision, originalDirty := buildinfo.Revision, buildinfo.Dirty
	defer func() {
		buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = originalFlavor, originalVersion, originalDigest
		buildinfo.Revision, buildinfo.Dirty = originalRevision, originalDirty
	}()
	buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = "plain", "unexpected", "sha256:unexpected"
	identity := buildIdentityFromSettings(nil)
	if identity.OtelcVersion != "unexpected" || identity.InstrumentationDigest != "sha256:unexpected" {
		t.Fatalf("invalid injected facts were cleaned: %+v", identity)
	}
	if err := identity.Validate(); err == nil {
		t.Fatal("invalid injected build facts must fail validation")
	}
}

// TestTelemetryModeProjection 锁定三个信号 exporter 的聚合合同及全局禁用优先级。
func TestTelemetryModeProjection(t *testing.T) {
	originalFlavor := buildinfo.Flavor
	defer func() { buildinfo.Flavor = originalFlavor }()
	buildinfo.Flavor = "plain"
	if got := buildinfo.TelemetryMode(); got != "unavailable" {
		t.Fatalf("plain telemetry mode = %q, want unavailable", got)
	}
	buildinfo.Flavor = "otelc"
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_METRICS_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	if got := buildinfo.TelemetryMode(); got != "disabled" {
		t.Fatalf("all-none telemetry mode = %q", got)
	}
	t.Setenv("OTEL_METRICS_EXPORTER", "prometheus")
	if got := buildinfo.TelemetryMode(); got != "enabled" {
		t.Fatalf("prometheus telemetry mode = %q", got)
	}
	t.Setenv("OTEL_LOGS_EXPORTER", "otlp")
	if got := buildinfo.TelemetryMode(); got != "otlp" {
		t.Fatalf("mixed OTLP telemetry mode = %q", got)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "OTLP")
	if got := buildinfo.TelemetryMode(); got != "config_error" {
		t.Fatalf("uppercase telemetry mode = %q", got)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", " otlp ")
	if got := buildinfo.TelemetryMode(); got != "config_error" {
		t.Fatalf("space-padded telemetry mode = %q", got)
	}
	t.Setenv("OTEL_TRACES_EXPORTER", "none")
	t.Setenv("OTEL_LOGS_EXPORTER", "prometheus")
	if got := buildinfo.TelemetryMode(); got != "config_error" {
		t.Fatalf("unsupported logs prometheus mode = %q", got)
	}
	t.Setenv("OTEL_LOGS_EXPORTER", "none")
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if got := buildinfo.TelemetryMode(); got != "disabled" {
		t.Fatalf("global disabled telemetry mode = %q", got)
	}
}
