package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/modelcatalog"
)

// stubCatalogLoader 返回固定目录或错误的替身。
func stubCatalogLoader(catalog *modelcatalog.Catalog, err error) func(modelcatalog.Options) (*modelcatalog.Catalog, error) {
	return func(modelcatalog.Options) (*modelcatalog.Catalog, error) { return catalog, err }
}

// stubProber 返回固定观测或错误的替身。
func stubProber(observations *ProbeObservations, err error) func(context.Context, string, string, string) (*ProbeObservations, error) {
	return func(context.Context, string, string, string) (*ProbeObservations, error) { return observations, err }
}

// setupFixture 构造离线向导服务与基础输入；目录与探测行为由用例覆写。
func setupFixture(t *testing.T, catalog *modelcatalog.Catalog, catalogErr error, observations *ProbeObservations, probeErr error) (*SetupService, SetupInput) {
	t.Helper()
	service := &SetupService{
		CatalogLoader:  stubCatalogLoader(catalog, catalogErr),
		EndpointProber: stubProber(observations, probeErr),
	}
	return service, SetupInput{BaseURL: "https://api.example.com/v1", Model: "setup-model", APIKeyEnv: "SETUP_TEST_KEY", Workspace: t.TempDir()}
}

// TestSetupRunPersistsValidatedConfig 锁定向导主合同：目录命中预填能力与
// 定价、密钥缺席跳过探测、生成的配置 0600 且严格装载自证、目录事实进入
// 注记；既有配置不被覆盖。
func TestSetupRunPersistsValidatedConfig(t *testing.T) {
	catalog := &modelcatalog.Catalog{Entries: []modelcatalog.ModelEntry{{
		Name: "setup-model", ToolCall: true, ContextTokens: 128000, OutputTokens: 8192,
		InputUSDPerMillion: 1, OutputUSDPerMillion: 2,
	}}}
	service, input := setupFixture(t, catalog, nil, nil, nil)
	t.Setenv("SETUP_TEST_KEY", "")
	outcome, err := service.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.CatalogMatched || outcome.CatalogName != "setup-model" {
		t.Fatalf("catalog outcome: %+v", outcome)
	}
	if outcome.ProbeRan {
		t.Fatal("probe must be skipped when the key is unset")
	}
	info, err := os.Stat(outcome.ConfigPath)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("config must be 0600: %v %v", info, err)
	}
	if filepath.Dir(outcome.ConfigPath) != input.Workspace+"/.agent" {
		t.Fatalf("config path = %q", outcome.ConfigPath)
	}
	loaded, err := config.Load(outcome.ConfigPath, config.Overrides{})
	if err != nil {
		t.Fatalf("generated config must load: %v", err)
	}
	if loaded.Provider.Capability == nil || loaded.Provider.Capability.ContextWindowTokens != 128000 {
		t.Fatalf("capability prefill missing: %+v", loaded.Provider.Capability)
	}
	if loaded.Pricing.InputTokenUSD == 0 || loaded.Pricing.OutputTokenUSD == 0 {
		t.Fatalf("pricing prefill missing: %+v", loaded.Pricing)
	}
	_, err = service.Run(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "不覆盖") {
		t.Fatalf("rerun must refuse to overwrite: %v", err)
	}
}

// TestSetupRunDegradesWhenCatalogUnavailable 锁定目录降级：装载失败时使用
// 安全默认能力声明并在注记中如实说明，向导仍完成。
func TestSetupRunDegradesWhenCatalogUnavailable(t *testing.T) {
	service, input := setupFixture(t, nil, errors.New("offline"), nil, nil)
	outcome, err := service.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.CatalogMatched {
		t.Fatal("catalog must not be reported as matched")
	}
	if len(outcome.Notes) == 0 || !strings.Contains(strings.Join(outcome.Notes, "\n"), "目录不可用") {
		t.Fatalf("degradation must be noted: %v", outcome.Notes)
	}
}

// TestSetupRunFailsClosedOnMissingHardCapabilities 锁定硬性校验：目录给出
// 否定的工具调用能力时向导终止且不留任何文件。
func TestSetupRunFailsClosedOnMissingHardCapabilities(t *testing.T) {
	catalog := &modelcatalog.Catalog{Entries: []modelcatalog.ModelEntry{{Name: "setup-model", ToolCall: false}}}
	service, input := setupFixture(t, catalog, nil, nil, nil)
	_, err := service.Run(context.Background(), input)
	if err == nil || !strings.Contains(err.Error(), "硬性要求") {
		t.Fatalf("hard capability gate must fail closed: %v", err)
	}
	if entries, readErr := os.ReadDir(filepath.Join(input.Workspace, ".agent")); readErr == nil && len(entries) != 0 {
		t.Fatalf("failed wizard must not leave files: %v", entries)
	}
}

// TestSetupRunMergesObservedStopReason 锁定探测合并：实测观测到协议标准集
// 外的停止原因时并入声明；探测失败只降级为注记，不阻断落盘。
func TestSetupRunMergesObservedStopReason(t *testing.T) {
	catalog := &modelcatalog.Catalog{Entries: []modelcatalog.ModelEntry{{Name: "setup-model", ToolCall: true}}}
	service, input := setupFixture(t, catalog, nil, &ProbeObservations{UsageStreamed: true, StopReason: "eos"}, nil)
	t.Setenv("SETUP_TEST_KEY", "present")
	outcome, err := service.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.ProbeRan {
		t.Fatal("probe must run when the key is set")
	}
	loaded, err := config.Load(outcome.ConfigPath, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, reason := range loaded.Provider.Capability.StopReasons {
		if reason == "eos" {
			found = true
		}
	}
	if !found {
		t.Fatalf("observed stop reason must be merged: %v", loaded.Provider.Capability.StopReasons)
	}
}

// TestSetupRunRequiresAllInputs 锁定输入校验：三要素缺一即失败。
func TestSetupRunRequiresAllInputs(t *testing.T) {
	service, _ := setupFixture(t, nil, nil, nil, nil)
	for _, input := range []SetupInput{
		{Model: "m", APIKeyEnv: "K"},
		{BaseURL: "https://api.example.com", APIKeyEnv: "K"},
		{BaseURL: "https://api.example.com", Model: "m"},
	} {
		if _, err := service.Run(context.Background(), input); err == nil {
			t.Fatalf("input %+v must be rejected", input)
		}
	}
}
