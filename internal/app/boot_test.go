package app

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// TestAssembleBootReleasesLockOnContention 锁定生命周期顺序：第二个 Boot 在锁
// 竞争处失败且不泄漏资源；第一个 Boot Close 后同一工作区可再次装配。
func TestAssembleBootReleasesLockOnContention(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &finalProvider{}, nil
	}}
	first, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.assembleBoot(workspace, stateDir, configuration, nil); err == nil {
		t.Fatal("second boot unexpectedly acquired the same workspace lock")
	}
	first.Close()
	retry, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatalf("lock was not released by Close: %v", err)
	}
	if err := retry.Close(); err != nil {
		t.Fatalf("healthy boot close must not fail: %v", err)
	}
	// Close 幂等：重复调用不得 panic、不得重复释放，且返回零错误。
	if err := first.Close(); err != nil {
		t.Fatalf("idempotent close must return nil, got %v", err)
	}
}

// TestAssembleBootCleansUpWhenLateStageFails 锁定装配中段失败的逆序清理：Provider
// 构造失败时，锁与 Journal 必须已被释放，工作区可立即重试装配。
func TestAssembleBootCleansUpWhenLateStageFails(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	workspace, stateDir := t.TempDir(), t.TempDir()
	failing := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return nil, errors.New("provider unavailable")
	}}
	if _, err := failing.assembleBoot(workspace, stateDir, configuration, nil); err == nil || !strings.Contains(err.Error(), "provider unavailable") {
		t.Fatalf("err = %v", err)
	}
	healthy := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &finalProvider{}, nil
	}}
	boot, err := healthy.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatalf("failed assembly leaked the workspace lock: %v", err)
	}
	boot.Close()
}

// TestAssembleBootFailurePreservesCleanupErrors 锁定装配失败的错误语义：主失败
// 与所有已取得资源的关闭失败必须同时可由 errors.Is 识别，且两个关闭动作都执行。
func TestAssembleBootFailurePreservesCleanupErrors(t *testing.T) {
	providerErr := errors.New("provider unavailable")
	journalErr := errors.New("journal close failed")
	lockErr := errors.New("lock release failed")
	journalClosed, lockReleased := false, false
	boot := &Boot{
		closeJournal: func() error {
			journalClosed = true
			return journalErr
		},
		releaseLock: func() error {
			lockReleased = true
			return lockErr
		},
	}
	err := bootAssemblyFailure(boot, providerErr)
	for _, expected := range []error{providerErr, journalErr, lockErr} {
		if !errors.Is(err, expected) {
			t.Fatalf("assembly error %v does not preserve %v", err, expected)
		}
	}
	if !journalClosed || !lockReleased {
		t.Fatalf("cleanup calls journal=%t lock=%t, want both true", journalClosed, lockReleased)
	}
	if !boot.closed {
		t.Fatal("failed assembly must mark its temporary boot closed")
	}
}

// TestBootManifestDescribesCapabilities 锁定 Capability Manifest 的非敏感能力
// 事实：固定五工具、Profile、沙箱后端与验证命令数。
func TestBootManifestDescribesCapabilities(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Sandbox.Backend = "docker"
	configuration.Sandbox.DockerImage = "alpine:3.21"
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/true"}}}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &finalProvider{}, nil
	}}
	boot, err := service.assembleBoot(t.TempDir(), t.TempDir(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	manifest := boot.Manifest()
	if manifest.Profile != config.ProfileDefault || manifest.Sandbox.Backend != "docker" || manifest.Sandbox.DockerImage != "alpine:3.21" || manifest.Verification.Commands != 1 {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Verification.Fingerprint == "" || !strings.HasPrefix(manifest.Verification.Fingerprint, "sha256:") {
		t.Fatalf("manifest verification fingerprint = %q", manifest.Verification.Fingerprint)
	}
	toolNames := make([]string, 0, len(manifest.Tools))
	for _, tool := range manifest.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	if strings.Join(toolNames, ",") != "list_files,read_file,search_text,todo,git_status,git_diff,git_log,git_commit,apply_patch,exec_command,send_message" {
		t.Fatalf("manifest tools = %v", toolNames)
	}
	sideEffects := map[string]string{}
	for _, tool := range manifest.Tools {
		sideEffects[tool.Name] = tool.SideEffect
	}
	if sideEffects["read_file"] != "none" || sideEffects["apply_patch"] != "workspace" || sideEffects["exec_command"] != "process" || sideEffects["send_message"] != "none" || sideEffects["git_status"] != "none" {
		t.Fatalf("manifest side effects = %v", sideEffects)
	}
	if manifest.Budget.MaxTurns != configuration.Budget.MaxTurns || manifest.Budget.MaxToolCalls != configuration.Budget.MaxToolCalls || manifest.Budget.MaxInputTokens != configuration.Budget.MaxInputTokens || manifest.Budget.MaxWallTime != configuration.Budget.MaxWallTime {
		t.Fatalf("manifest budget = %+v", manifest.Budget)
	}
	if manifest.ConfigFingerprint != configuration.Fingerprint() {
		t.Fatalf("manifest config fingerprint = %q, want %q", manifest.ConfigFingerprint, configuration.Fingerprint())
	}
	allTools := strings.Join(toolNames, " ") + manifest.Provider + manifest.Model + manifest.Policy.Write + manifest.Policy.Exec
	if strings.Contains(strings.ToLower(allTools), "key") {
		t.Fatalf("manifest must not carry credential material: %+v", manifest)
	}
}

// TestCapabilityManifestIsSingleProjection 锁定 AB-3 验收合同：Boot.Manifest 与
// doctor 的 capability_manifest 是同一投影（不矛盾），预算展开与 Resolved
// Execution Plan 逐字段对账，验证命令摘要非空且随命令集变化。
func TestCapabilityManifestIsSingleProjection(t *testing.T) {
	workspace, stateDir := t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(workspace, ".agent"), 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(workspace, ".agent", "config.yaml")
	yaml := "version: 2\nprovider:\n  id: custom\n  admission: optional\n  base_url: https://provider.test/v1\n  model: m1\n  api_key_env: CAIRN_AB3_KEY\n  capability:\n    tool_calls: true\n    streaming_usage: true\n  request_timeout: 30s\nsandbox:\n  backend: docker\n  docker_image: alpine:3.21\nverification:\n  commands:\n    - argv: [\"/usr/bin/true\"]\n"
	if err := os.WriteFile(configPath, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := config.Load(configPath, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	boot, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	fromBoot := boot.Manifest()
	doctorReport, err := service.Doctor(context.Background(), workspace, stateDir, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fromBoot, doctorReport.Capabilities) {
		t.Fatalf("boot manifest and doctor projection diverge:\nboot   = %+v\ndoctor = %+v", fromBoot, doctorReport.Capabilities)
	}
	resolved := map[string]string{}
	for _, entry := range configuration.Resolution {
		resolved[entry.Field] = entry.Value
	}
	budgetPairs := []struct {
		field string
		value string
	}{
		{"budget.max_turns", strconv.Itoa(fromBoot.Budget.MaxTurns)},
		{"budget.max_tool_calls", strconv.Itoa(fromBoot.Budget.MaxToolCalls)},
		{"budget.max_input_tokens", strconv.Itoa(fromBoot.Budget.MaxInputTokens)},
		{"budget.max_output_tokens", strconv.Itoa(fromBoot.Budget.MaxOutputTokens)},
		{"budget.max_wall_time", fromBoot.Budget.MaxWallTime},
		{"budget.max_cost_usd", strconv.FormatFloat(fromBoot.Budget.MaxCostUSD, 'f', -1, 64)},
	}
	for _, pair := range budgetPairs {
		if resolved[pair.field] != pair.value {
			t.Fatalf("manifest budget field %s = %q, resolution = %q", pair.field, pair.value, resolved[pair.field])
		}
	}
	changed := withTestProvider(config.Defaults())
	changed.Verification.Commands = []config.VerificationCommand{{Argv: []string{"/usr/bin/false"}}}
	changedCommands, err := verificationCommands(changed.Verification)
	if err != nil {
		t.Fatal(err)
	}
	changedCard := mustTestCard(t, changed)
	changedManifest := buildCapabilityManifest(changed, nil, changedCommands, &changedCard)
	if changedManifest.Verification.Fingerprint == fromBoot.Verification.Fingerprint {
		t.Fatalf("verification fingerprint must change with command set: %q", fromBoot.Verification.Fingerprint)
	}
}

// TestCompositionRootDependencyDirection 锁定依赖方向：Domain 不依赖任何内部
// 基础设施包；app 作为组合根只允许引用已列入白名单的适配器，
// 且不得反向依赖 CLI。
func TestCompositionRootDependencyDirection(t *testing.T) {
	collectImports := func(packageDir string) []string {
		entries, err := os.ReadDir(packageDir)
		if err != nil {
			t.Fatal(err)
		}
		var imports []string
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, filepath.Join(packageDir, entry.Name()), nil, parser.ImportsOnly)
			if err != nil {
				t.Fatalf("parse %s: %v", entry.Name(), err)
			}
			for _, imported := range file.Imports {
				imports = append(imports, strings.Trim(imported.Path.Value, "\""))
			}
		}
		return imports
	}
	for _, imported := range collectImports("../domain") {
		if strings.HasPrefix(imported, "github.com/fxbin/cairn/internal/") {
			t.Fatalf("domain must not depend on internal packages, found %q", imported)
		}
	}
	allowedAdapters := map[string]bool{
		"github.com/fxbin/cairn/internal/adapters/mcpclient":                  true,
		"github.com/fxbin/cairn/internal/adapters/journal/sqlite":             true,
		"github.com/fxbin/cairn/internal/adapters/memstore/sqlite":            true,
		"github.com/fxbin/cairn/internal/adapters/provider/openaicompat":      true,
		"github.com/fxbin/cairn/internal/adapters/provider/anthropicmessages": true,
		"github.com/fxbin/cairn/internal/adapters/provider/openairesponses":   true,
		"github.com/fxbin/cairn/internal/adapters/provider/geminigen":         true,
	}
	for _, imported := range collectImports(".") {
		if strings.HasPrefix(imported, "github.com/fxbin/cairn/internal/adapters/") && !allowedAdapters[imported] {
			t.Fatalf("composition root references non-allowlisted adapter %q", imported)
		}
		if strings.HasPrefix(imported, "github.com/fxbin/cairn/internal/cli") {
			t.Fatalf("app must not depend on the CLI package, found %q", imported)
		}
	}
}

// TestReadOnlyPresetDropsSideEffectTools 锁定 read-only Preset 的工具子集与
// Manifest 展开：注册表只有三件只读工具，策略与预算进入能力清单。
func TestReadOnlyPresetDropsSideEffectTools(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Preset = config.PresetReadOnly
	configuration.Policy.Write = "deny"
	configuration.Policy.Exec = "deny"
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) {
		return &finalProvider{}, nil
	}}
	boot, err := service.assembleBoot(t.TempDir(), t.TempDir(), configuration, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer boot.Close()
	manifest := boot.Manifest()
	toolNames := make([]string, 0, len(manifest.Tools))
	for _, tool := range manifest.Tools {
		toolNames = append(toolNames, tool.Name)
	}
	if strings.Join(toolNames, ",") != "list_files,read_file,search_text,todo,git_status,git_diff,git_log" {
		t.Fatalf("read-only tools = %v", toolNames)
	}
	for _, tool := range manifest.Tools {
		if tool.SideEffect != "none" {
			t.Fatalf("read-only tool %q must be side-effect none, got %q", tool.Name, tool.SideEffect)
		}
	}
	if manifest.Preset != config.PresetReadOnly || manifest.Policy.Write != "deny" || manifest.Policy.Exec != "deny" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.Budget.MaxTurns != withTestProvider(config.Defaults()).Budget.MaxTurns {
		t.Fatalf("manifest budget turns = %d", manifest.Budget.MaxTurns)
	}
	if manifest.Verification.Commands != 0 {
		t.Fatalf("read-only manifest verification commands = %d", manifest.Verification.Commands)
	}
}

// TestBootFailsClosedOnUnknownProviderCapability 锁定失败关闭：模型既无
// 显式声明也不在内建目录时，装配在任何资源创建前失败，错误给出可行动指引。
func TestBootFailsClosedOnUnknownProviderCapability(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Provider.Model = "mystery-model"
	configuration.Provider.Capability = nil
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	workspace, stateDir := t.TempDir(), t.TempDir()
	_, err := service.assembleBoot(workspace, stateDir, configuration, nil)
	if err == nil || !strings.Contains(err.Error(), "declare provider.capability") {
		t.Fatalf("unknown capability must fail closed: %v", err)
	}
	if _, err := service.Run(context.Background(), RunOptions{Task: "x", Workspace: workspace, StateDir: stateDir, Config: configuration}); err == nil || !strings.Contains(err.Error(), "provider capability unknown") {
		t.Fatalf("run must fail closed for unknown capability: %v", err)
	}
	// 能力声明超窗时同样拒绝启动：预算不得越过已声明上限。
	overWindow := withTestProvider(config.Defaults())
	overWindow.Provider.Capability = &config.ProviderCapabilityConfig{ToolCalls: true, StreamingUsage: true, ContextWindowTokens: 100}
	if _, err := service.assembleBoot(t.TempDir(), t.TempDir(), overWindow, nil); err == nil || !strings.Contains(err.Error(), "context window") {
		t.Fatalf("over-window budget must fail closed: %v", err)
	}
}

// TestDoctorReportsProviderCapabilityCard 锁定 doctor 对能力卡的只读投影：
// 可解析时报告卡与来源，未知模型记录可行动错误并压低 ready。
func TestDoctorReportsProviderCapabilityCard(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	service := Service{NewProvider: func(config.Config) (ports.Provider, error) { return &finalProvider{}, nil }}
	report, err := service.Doctor(context.Background(), t.TempDir(), t.TempDir(), configuration)
	if err != nil {
		t.Fatal(err)
	}
	if report.ProviderCard == nil || report.ProviderCardError != "" || report.ProviderCard.Source != contract.ProviderCardSourceConfig || !report.ProviderCard.ToolCalls {
		t.Fatalf("doctor card = %#v error=%q", report.ProviderCard, report.ProviderCardError)
	}
	if report.Capabilities.ProviderCard == nil || !reflect.DeepEqual(*report.Capabilities.ProviderCard, *report.ProviderCard) {
		t.Fatalf("manifest card drifted from doctor card: %#v", report.Capabilities.ProviderCard)
	}
	unknown := configuration
	unknown.Provider.Model = "mystery-model"
	unknown.Provider.Capability = nil
	unknownReport, err := service.Doctor(context.Background(), t.TempDir(), t.TempDir(), unknown)
	if err != nil {
		t.Fatal(err)
	}
	if unknownReport.ProviderCard != nil || unknownReport.ProviderCardError == "" || unknownReport.Ready {
		t.Fatalf("unknown capability doctor = %#v", unknownReport.ProviderCard)
	}
}

// TestAssembleRecoveryBootRejectsLegacyConfigBeforeCreatingResources 锁定恢复
// 控制面同样只接受 v2，且拒绝发生在 Journal、锁或制品目录创建之前。
func TestAssembleRecoveryBootRejectsLegacyConfigBeforeCreatingResources(t *testing.T) {
	configuration := withTestProvider(config.Defaults())
	configuration.Version = 1
	workspace, stateDir := t.TempDir(), t.TempDir()
	if _, err := assembleRecoveryBoot(workspace, stateDir, configuration); err == nil || !strings.Contains(err.Error(), "unsupported config version 1") {
		t.Fatalf("legacy recovery config error = %v", err)
	}
	assertStateDirEmpty(t, stateDir)
}
