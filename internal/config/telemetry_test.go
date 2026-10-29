package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTelemetryDefaultsAreDisabled 锁定默认关闭合同：默认配置不开启语义观测，
// 抽样比例取默认值。
func TestTelemetryDefaultsAreDisabled(t *testing.T) {
	defaults := Defaults()
	if defaults.Telemetry.Enabled {
		t.Fatal("semantic telemetry must be disabled by default")
	}
	if defaults.Telemetry.SamplePercent != DefaultTelemetrySamplePercent {
		t.Fatalf("sample percent = %d", defaults.Telemetry.SamplePercent)
	}
	if defaults.Telemetry.EffectiveSamplePercent() != DefaultTelemetrySamplePercent {
		t.Fatalf("effective sample percent = %d", defaults.Telemetry.EffectiveSamplePercent())
	}
}

// TestTelemetrySettingsDoNotChangeFingerprint 锁定运维字段边界：语义观测是
// 可选旁路，开启或调整抽样比例都不改变冻结配置指纹，既有运行不被作废。
func TestTelemetrySettingsDoNotChangeFingerprint(t *testing.T) {
	configuration := validDefaults()
	before := configuration.Fingerprint()
	configuration.Telemetry.Enabled = true
	configuration.Telemetry.SamplePercent = 25
	if before != configuration.Fingerprint() {
		t.Fatal("telemetry settings must not change the configuration fingerprint")
	}
}

// TestTelemetrySamplePercentValidation 锁定严格校验：抽样比例越界失败关闭，
// 未声明的 0 按默认值处理。
func TestTelemetrySamplePercentValidation(t *testing.T) {
	configuration := validDefaults()
	configuration.Telemetry.SamplePercent = 0
	if err := Validate(configuration); err != nil {
		t.Fatalf("unset sample percent must be accepted: %v", err)
	}
	configuration.Telemetry.SamplePercent = 101
	if err := Validate(configuration); err == nil || !strings.Contains(err.Error(), "telemetry.sample_percent") {
		t.Fatalf("out of range sample percent must fail closed: %v", err)
	}
	configuration.Telemetry.SamplePercent = -1
	if err := Validate(configuration); err == nil {
		t.Fatal("negative sample percent must fail closed")
	}
	configuration.Telemetry.SamplePercent = 1
	if err := Validate(configuration); err != nil {
		t.Fatalf("minimum sample percent must be accepted: %v", err)
	}
}

// TestLoadReadsTelemetryBlock 锁定配置装载：telemetry 块可显式开启并声明抽样
// 比例，未声明的比例按默认值归一。
func TestLoadReadsTelemetryBlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 2\nprovider:\n" + validProviderYAML + "telemetry:\n  enabled: true\n  sample_percent: 25\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if !configuration.Telemetry.Enabled || configuration.Telemetry.SamplePercent != 25 {
		t.Fatalf("telemetry = %+v", configuration.Telemetry)
	}
	if configuration.Telemetry.EffectiveSamplePercent() != 25 {
		t.Fatalf("effective sample percent = %d", configuration.Telemetry.EffectiveSamplePercent())
	}
}

// TestLoadKeepsTelemetryDisabledWithoutDeclaration 锁定默认关闭：没有声明
// telemetry 块时装配面保持关闭。
func TestLoadKeepsTelemetryDisabledWithoutDeclaration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 2\nprovider:\n" + validProviderYAML
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Telemetry.Enabled {
		t.Fatal("telemetry must stay disabled when it is not declared")
	}
	if configuration.Telemetry.SamplePercent != DefaultTelemetrySamplePercent {
		t.Fatalf("sample percent = %d", configuration.Telemetry.SamplePercent)
	}
}

// TestLoadRejectsUnknownTelemetryField 锁定严格解码：telemetry 块里的未知字段
// 失败关闭，不被静默忽略。
func TestLoadRejectsUnknownTelemetryField(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 2\nprovider:\n" + validProviderYAML + "telemetry:\n  enabled: true\n  endpoint: http://127.0.0.1:4318\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, Overrides{}); err == nil {
		t.Fatal("unknown telemetry field must fail closed")
	}
}
