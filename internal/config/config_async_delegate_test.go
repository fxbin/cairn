// config_async_delegate_test.go 锁定异步委派配置门禁：默认关闭、指纹进入、
// read-only Preset 拒绝与 YAML 解析。
// Author: fxbin
package config

import (
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"
)

// TestDelegateAsyncDefaultsOffAndEntersFingerprint 锁定默认关闭与指纹语义：
// 显式 false 与缺省指纹逐位一致；开启改变指纹（影响执行语义必须进指纹）。
func TestDelegateAsyncDefaultsOffAndEntersFingerprint(t *testing.T) {
	base := withValidTestProvider(Defaults())
	base.Tools.DelegateAsync = false
	if withValidTestProvider(Defaults()).Fingerprint() != base.Fingerprint() {
		t.Fatal("explicit delegate_async=false must keep the default fingerprint")
	}
	enabled := base
	enabled.Tools.DelegateAsync = true
	if enabled.Fingerprint() == base.Fingerprint() {
		t.Fatal("enabling delegate_async must change the frozen config fingerprint")
	}
	if toolsFingerprint(base.Tools) != nil {
		t.Fatal("all-off tools view must stay out of the fingerprint")
	}
	view := toolsFingerprint(enabled.Tools)
	if view == nil || !view.DelegateAsync {
		t.Fatalf("enabled tools fingerprint view = %+v", view)
	}
}

// TestDelegateAsyncRejectedOnReadOnlyPreset 锁定 read-only Preset 上的异步委派
// 开关在配置层失败关闭。
func TestDelegateAsyncRejectedOnReadOnlyPreset(t *testing.T) {
	configuration := withValidTestProvider(Defaults())
	configuration.Preset = PresetReadOnly
	configuration.Policy.Write = "deny"
	configuration.Policy.Exec = "deny"
	if err := Validate(configuration); err != nil {
		t.Fatalf("read-only baseline must stay valid: %v", err)
	}
	configuration.Tools.DelegateAsync = true
	err := Validate(configuration)
	if err == nil || !strings.Contains(err.Error(), "delegate_async") {
		t.Fatalf("read-only preset must reject delegate_async: %v", err)
	}
}

// TestDelegateAsyncParsesFromYAML 锁定 delegate_async 的 YAML 键解析。
func TestDelegateAsyncParsesFromYAML(t *testing.T) {
	var decoded struct {
		Tools struct {
			DelegateAsync bool `yaml:"delegate_async"`
		} `yaml:"tools"`
	}
	if err := yaml.Unmarshal([]byte("tools:\n  delegate_async: true\n"), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.Tools.DelegateAsync {
		t.Fatal("delegate_async must parse from yaml")
	}
}

// withValidTestProvider 补齐最小合法 Provider 连接事实，供校验类测试复用。
func withValidTestProvider(configuration Config) Config {
	configuration.Version = CurrentVersion
	configuration.Provider.BaseURL = "https://provider.test/v1"
	configuration.Provider.Model = "test-model"
	configuration.Provider.APIKeyEnv = "CAIRN_TEST_KEY"
	configuration.Provider.Admission = ProviderAdmissionOptional
	configuration.Provider.RequestTimeout = "30s"
	return configuration
}
