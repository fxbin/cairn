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
	defer func() {
		buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = originalFlavor, originalVersion, originalDigest
	}()

	plain := buildIdentityFromSettings(nil)
	if plain.Flavor != "plain" || plain.OtelcVersion != "" || plain.InstrumentationDigest != "" {
		t.Fatalf("plain identity = %+v", plain)
	}
	if plain.Fingerprint != plain.ComputeFingerprint() {
		t.Fatalf("plain fingerprint not self-consistent: %s", plain.Fingerprint)
	}

	buildinfo.Flavor, buildinfo.ToolchainVersion, buildinfo.InstrumentationDigest = "otelc", "v1.1.0", "sha256:digest"
	otelc := buildIdentityFromSettings(nil)
	if otelc.Flavor != "otelc" || otelc.OtelcVersion != "v1.1.0" || otelc.InstrumentationDigest != "sha256:digest" {
		t.Fatalf("otelc identity = %+v", otelc)
	}
	if otelc.Fingerprint == plain.Fingerprint {
		t.Fatal("otelc flavor must change the build fingerprint")
	}
	if identity := otelc; identity.ComputeFingerprint() != identity.Fingerprint {
		t.Fatalf("otelc fingerprint not self-consistent: %s", identity.Fingerprint)
	}
}

// TestTelemetryModeProjection 锁定遥测模式投影合同：plain 构建恒为
// unavailable；otelc 构建按 OTEL_SDK_DISABLED 映射 disabled/otlp，端点值永远
// 不进入投影。
func TestTelemetryModeProjection(t *testing.T) {
	originalFlavor := buildinfo.Flavor
	defer func() { buildinfo.Flavor = originalFlavor }()
	buildinfo.Flavor = "plain"
	if got := buildinfo.TelemetryMode(); got != "unavailable" {
		t.Fatalf("plain telemetry mode = %q, want unavailable", got)
	}
	buildinfo.Flavor = "otelc"
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if got := buildinfo.TelemetryMode(); got != "disabled" {
		t.Fatalf("disabled telemetry mode = %q", got)
	}
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:4318")
	if got := buildinfo.TelemetryMode(); got != "otlp" {
		t.Fatalf("otlp telemetry mode = %q", got)
	}
}
