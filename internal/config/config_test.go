package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadAppliesFlagOverridesAfterConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nprovider:\n  base_url: http://config.test/v1\n  model: config-model\n  api_key_env: CONFIG_KEY\n  request_timeout: 3s\nworkspace:\n  max_read_bytes: 5\n  max_search_files: 6\n  max_search_bytes: 7\nbudget:\n  max_turns: 8\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, Overrides{Model: "flag-model", MaxTurns: 9})
	if err != nil {
		t.Fatal(err)
	}
	if config.Provider.Model != "flag-model" || config.Budget.MaxTurns != 9 || config.Provider.APIKeyEnv != "CONFIG_KEY" {
		t.Fatalf("config = %#v", config)
	}
}

func TestValidateRejectsSecretLikeEnvironmentAllowlistEntries(t *testing.T) {
	configuration := Defaults()
	configuration.Policy.EnvAllow = append(configuration.Policy.EnvAllow, "AGENT_API_KEY")
	if err := Validate(configuration); err == nil {
		t.Fatal("secret-like environment variable was accepted")
	}
}

func TestValidateRequiresVerificationEnvironmentToBeAllowlisted(t *testing.T) {
	configuration := Defaults()
	configuration.Verification.Commands = []VerificationCommand{{Argv: []string{"/usr/bin/true"}, Env: map[string]string{"UNAPPROVED": "1"}}}
	if err := Validate(configuration); err == nil {
		t.Fatal("verification environment bypassed policy allowlist")
	}
}

func TestLoadUsesDocumentedEnvironmentAllowlistKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\npolicy:\n  write: ask\n  exec: ask\n  protected_paths: []\n  env_allowlist: [PATH, LANG]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if len(configuration.Policy.EnvAllow) != 2 || configuration.Policy.EnvAllow[0] != "PATH" || configuration.Policy.EnvAllow[1] != "LANG" {
		t.Fatalf("allowlist = %#v", configuration.Policy.EnvAllow)
	}
}

func TestLoadAppliesExtendedBudgetOverrides(t *testing.T) {
	configuration, err := Load("", Overrides{MaxToolCalls: 7, MaxInputTokens: 11, MaxOutputTokens: 13, MaxWallTime: "45s"})
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Budget.MaxToolCalls != 7 || configuration.Budget.MaxInputTokens != 11 || configuration.Budget.MaxOutputTokens != 13 || configuration.Budget.MaxWallTime != "45s" {
		t.Fatalf("budget = %#v", configuration.Budget)
	}
}

func TestValidateRejectsInvalidWallBudget(t *testing.T) {
	configuration := Defaults()
	configuration.Budget.MaxWallTime = "not-a-duration"
	if err := Validate(configuration); err == nil {
		t.Fatal("invalid wall budget was accepted")
	}
}

func TestValidateRequiresExplicitPricingForCostBudget(t *testing.T) {
	configuration := Defaults()
	configuration.Budget.MaxCostUSD = 1
	if err := Validate(configuration); err == nil {
		t.Fatal("cost budget without pricing was accepted")
	}
	configuration.Pricing.InputTokenUSD = 0.001
	configuration.Pricing.OutputTokenUSD = 0.002
	if err := Validate(configuration); err != nil {
		t.Fatalf("explicit pricing rejected: %v", err)
	}
}

func TestValidateRejectsInvalidOutputAndCancellationLimits(t *testing.T) {
	configuration := Defaults()
	configuration.Output.MaxArtifactBytes = 0
	if err := Validate(configuration); err == nil {
		t.Fatal("zero artifact limit was accepted")
	}
	configuration = Defaults()
	configuration.Cancellation.TerminateGrace = "zero"
	if err := Validate(configuration); err == nil {
		t.Fatal("invalid termination grace was accepted")
	}
}

func TestDefaultsEnableBoundedContextCompaction(t *testing.T) {
	configuration := Defaults()
	if !configuration.Context.AutoCompact || configuration.Context.MaxPromptTokens < 1 || configuration.Context.MaxSummaryBytes < 1 {
		t.Fatalf("context defaults = %#v", configuration.Context)
	}
	configuration.Context.MaxSummaryBytes = 0
	if err := Validate(configuration); err == nil {
		t.Fatal("zero context summary limit was accepted")
	}
}

func TestDefaultsUseFortyTurnBudgetAndExposeComplexPreset(t *testing.T) {
	configuration := Defaults()
	if configuration.Budget.MaxTurns != DefaultMaxTurns || DefaultMaxTurns != 40 || ComplexMaxTurns != 80 {
		t.Fatalf("turn presets: default=%d complex=%d config=%d", DefaultMaxTurns, ComplexMaxTurns, configuration.Budget.MaxTurns)
	}
}

func TestDefaultsKeepAutomaticVerificationAfterPatchDisabled(t *testing.T) {
	configuration := Defaults()
	if configuration.Verification.AutoAfterPatch {
		t.Fatal("automatic verification after patch is enabled by default")
	}
}

func TestValidateSandboxRequiresAnExplicitValidDockerConfiguration(t *testing.T) {
	configuration := Defaults()
	if configuration.Sandbox.Backend != "host" || configuration.Sandbox.DockerImage != "" || configuration.Sandbox.MaxPIDs < 1 {
		t.Fatalf("sandbox defaults = %#v", configuration.Sandbox)
	}
	configuration.Sandbox.Backend = "docker"
	if err := Validate(configuration); err == nil {
		t.Fatal("docker backend without image was accepted")
	}
	configuration.Sandbox.DockerImage = "-unsafe-image"
	if err := Validate(configuration); err == nil {
		t.Fatal("option-like docker image was accepted")
	}
	configuration.Sandbox.DockerImage = "alpine:3.21"
	configuration.Sandbox.MaxPIDs = 0
	if err := Validate(configuration); err == nil {
		t.Fatal("unbounded docker pids were accepted")
	}
	configuration.Sandbox.MaxPIDs = 64
	if err := Validate(configuration); err != nil {
		t.Fatalf("valid docker sandbox rejected: %v", err)
	}
	configuration.Sandbox.Backend = "host"
	if err := Validate(configuration); err == nil {
		t.Fatal("host backend with dormant docker image was accepted")
	}
}

func TestFingerprintChangesWhenSandboxChanges(t *testing.T) {
	configuration := Defaults()
	before := configuration.Fingerprint()
	configuration.Sandbox.Backend = "docker"
	configuration.Sandbox.DockerImage = "alpine:3.21"
	if before == configuration.Fingerprint() {
		t.Fatal("sandbox configuration was omitted from fingerprint")
	}
}

func TestFingerprintChangesWhenAutomaticVerificationAfterPatchChanges(t *testing.T) {
	configuration := Defaults()
	before := configuration.Fingerprint()
	configuration.Verification.AutoAfterPatch = true
	if before == configuration.Fingerprint() {
		t.Fatal("automatic verification after patch was omitted from fingerprint")
	}
}

// TestFingerprintIgnoresOperationalFields 锁定运维/展示类字段不参与指纹：
// 调整它们不应令在途 run 的冻结配置漂移。
func TestFingerprintIgnoresOperationalFields(t *testing.T) {
	configuration := Defaults()
	before := configuration.Fingerprint()
	configuration.Provider.RequestTimeout = "5m"
	configuration.Output.MaxPreviewBytes = 999
	configuration.Output.MaxArtifactBytes = 999
	configuration.Cancellation.TerminateGrace = "9s"
	if before != configuration.Fingerprint() {
		t.Fatal("operational-only fields must not change the fingerprint")
	}
}

// TestFingerprintChangesWhenAuthorizationSemanticsChange 锁定授权与执行语义
// 字段仍参与指纹：模型、策略与工作区视图变化必须使旧审批绑定失效。
func TestFingerprintChangesWhenAuthorizationSemanticsChange(t *testing.T) {
	configuration := Defaults()
	before := configuration.Fingerprint()
	configuration.Provider.Model = "other-model"
	if before == configuration.Fingerprint() {
		t.Fatal("provider model must participate in the fingerprint")
	}
	configuration = Defaults()
	configuration.Policy.Exec = "allow"
	if before == configuration.Fingerprint() {
		t.Fatal("policy exec must participate in the fingerprint")
	}
	configuration = Defaults()
	configuration.Workspace.Ignore = append(configuration.Workspace.Ignore, "build")
	if before == configuration.Fingerprint() {
		t.Fatal("workspace ignore view must participate in the fingerprint")
	}
}

// TestValidateRejectsExpandedSecretLikeEnvironmentKeys 锁定密钥名启发式扩面：
// 常见云与令牌变量被拒绝，普通配置变量不受影响。
func TestValidateRejectsExpandedSecretLikeEnvironmentKeys(t *testing.T) {
	for _, key := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "GITHUB_PAT", "DEPLOY_PRIVATE_KEY", "OPENAI_APIKEY", "DB_PASSWD"} {
		configuration := Defaults()
		configuration.Policy.EnvAllow = append(configuration.Policy.EnvAllow, key)
		if err := Validate(configuration); err == nil || !strings.Contains(err.Error(), "secret-like") {
			t.Fatalf("env_allow %q err = %v", key, err)
		}
	}
	for _, key := range []string{"PATH", "LANG", "TERM", "TMPDIR", "GOFLAGS", "HOME"} {
		configuration := Defaults()
		configuration.Policy.EnvAllow = append(configuration.Policy.EnvAllow, key)
		if err := Validate(configuration); err != nil {
			t.Fatalf("env_allow %q err = %v", key, err)
		}
	}
}

// TestLoadRejectsInvalidAgentEnvironmentOverride 锁定 AGENT_* 数值型环境变量
// 的非法值被拒绝而不是静默忽略。
func TestLoadRejectsInvalidAgentEnvironmentOverride(t *testing.T) {
	t.Setenv("AGENT_MAX_TURNS", "not-a-number")
	if _, err := Load("", Overrides{}); err == nil || !strings.Contains(err.Error(), "AGENT_MAX_TURNS") {
		t.Fatalf("err = %v", err)
	}
}

// TestDefaultsRequireApprovalForWriteAndExec 锁定写入与命令执行默认都要求
// 审批的安全基线。
func TestDefaultsRequireApprovalForWriteAndExec(t *testing.T) {
	defaults := Defaults()
	if defaults.Policy.Write != "ask" || defaults.Policy.Exec != "ask" {
		t.Fatalf("defaults = write:%s exec:%s, want ask/ask", defaults.Policy.Write, defaults.Policy.Exec)
	}
}

// TestExecutionProfilePrecedenceTable 按固定优先级（默认 → Profile → YAML → env
// → CLI）表驱动验证 Profile 解析语义：default=40、complex=80，显式值覆盖预设。
func TestExecutionProfilePrecedenceTable(t *testing.T) {
	writeConfig := func(content string) string {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cases := []struct {
		name       string
		yaml       string
		env        map[string]string
		overrides  Overrides
		wantTurns  int
		wantTools  int
		wantInput  int
		wantSource string
	}{
		{"旧配置无 profile 保持默认", "version: 1\nprovider:\n  api_key_env: K\n", nil, Overrides{}, 40, 100, 500_000, "default"},
		{"complex 预设生效", "version: 1\nprofile: complex\nprovider:\n  api_key_env: K\n", nil, Overrides{}, 80, 160, 1_400_000, "profile:complex"},
		{"YAML 显式轮次覆盖预设", "version: 1\nprofile: complex\nprovider:\n  api_key_env: K\nbudget:\n  max_turns: 50\n", nil, Overrides{}, 50, 160, 1_400_000, "yaml"},
		{"更高值必须显式出现", "version: 1\nprofile: complex\nprovider:\n  api_key_env: K\nbudget:\n  max_turns: 100\n", nil, Overrides{}, 100, 160, 1_400_000, "yaml"},
		{"环境覆盖优先于 Profile", "version: 1\nprofile: complex\nprovider:\n  api_key_env: K\n", map[string]string{"AGENT_MAX_TURNS": "60"}, Overrides{}, 60, 160, 1_400_000, "env"},
		{"CLI 显式值最高优先", "version: 1\nprofile: complex\nprovider:\n  api_key_env: K\n", map[string]string{"AGENT_MAX_TURNS": "60"}, Overrides{MaxTurns: 70}, 70, 160, 1_400_000, "cli"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			for key, value := range testCase.env {
				t.Setenv(key, value)
			}
			config, err := Load(writeConfig(testCase.yaml), testCase.overrides)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if config.Budget.MaxTurns != testCase.wantTurns || config.Budget.MaxToolCalls != testCase.wantTools || config.Budget.MaxInputTokens != testCase.wantInput {
				t.Fatalf("budget = %+v", config.Budget)
			}
			if source := resolutionSource(config.Resolution, "budget.max_turns"); source != testCase.wantSource {
				t.Fatalf("max_turns source = %q, want %q (plan: %+v)", source, testCase.wantSource, config.Resolution)
			}
		})
	}
}

// TestUnknownExecutionProfileRejected 锁定未知 Profile 启动失败关闭。
func TestUnknownExecutionProfileRejected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nprofile: turbo\nprovider:\n  api_key_env: K\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, Overrides{}); err == nil || !strings.Contains(err.Error(), "unknown execution profile") {
		t.Fatalf("err = %v", err)
	}
}

// TestProfileChangesFingerprint 锁定 Profile 语义进入配置指纹：恢复时 Profile
// 漂移会被指纹校验拒绝。
func TestProfileChangesFingerprint(t *testing.T) {
	base := Defaults()
	if base.Profile != ProfileDefault {
		t.Fatalf("default profile = %q", base.Profile)
	}
	complexConfig := base
	complexConfig.Profile = ProfileComplex
	if base.Fingerprint() == complexConfig.Fingerprint() {
		t.Fatal("profile name alone must change the fingerprint")
	}
	emptyProfile := base
	emptyProfile.Profile = ""
	if emptyProfile.Fingerprint() != base.Fingerprint() {
		t.Fatal("empty profile must remain semantically equivalent to default")
	}
	explicit := complexConfig
	explicit.Budget.MaxTurns = 50
	if explicit.Fingerprint() == complexConfig.Fingerprint() {
		t.Fatal("explicit turn override must change the fingerprint")
	}
}

func TestProviderOverrideResolutionTracksEnvironmentAndCLI(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\nprovider:\n  base_url: http://yaml.test/v1\n  model: yaml-model\n  api_key_env: YAML_KEY\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_BASE_URL", "http://env.test/v1")
	t.Setenv("AGENT_MODEL", "env-model")
	t.Setenv("AGENT_API_KEY_ENV", "ENV_KEY")
	cases := []struct {
		name       string
		overrides  Overrides
		wantSource string
		wantURL    string
		wantModel  string
		wantKey    string
	}{
		{name: "environment", wantSource: "env", wantURL: "http://env.test/v1", wantModel: "env-model", wantKey: "ENV_KEY"},
		{name: "cli", overrides: Overrides{BaseURL: "http://cli.test/v1", Model: "cli-model", APIKeyEnv: "CLI_KEY"}, wantSource: "cli", wantURL: "http://cli.test/v1", wantModel: "cli-model", wantKey: "CLI_KEY"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			configuration, err := Load(path, testCase.overrides)
			if err != nil {
				t.Fatal(err)
			}
			for field, want := range map[string]string{
				"provider.base_url":    testCase.wantURL,
				"provider.model":       testCase.wantModel,
				"provider.api_key_env": testCase.wantKey,
			} {
				entry, found := resolutionEntry(configuration.Resolution, field)
				if !found || entry.Value != want || entry.Source != testCase.wantSource {
					t.Fatalf("%s = %+v, found=%t; want value %q source %q", field, entry, found, want, testCase.wantSource)
				}
			}
		})
	}
}

// resolutionSource 从 Resolved Execution Plan 中查询单个字段的来源。
func resolutionSource(entries []ResolutionEntry, field string) string {
	for _, entry := range entries {
		if entry.Field == field {
			return entry.Source
		}
	}
	return ""
}

// resolutionEntry 从 Resolved Execution Plan 中查询完整字段记录。
func resolutionEntry(entries []ResolutionEntry, field string) (ResolutionEntry, bool) {
	for _, entry := range entries {
		if entry.Field == field {
			return entry, true
		}
	}
	return ResolutionEntry{}, false
}

// TestResolutionMarshalsIntoFrozenPlan 锁定 Resolved Execution Plan 随冻结配置
// 制品持久化的契约：Run 冻结的是 json.Marshal(config)，Resolution 必须在场。
func TestResolutionMarshalsIntoFrozenPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\nprofile: complex\nprovider:\n  api_key_env: K\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(encoded, []byte(`"resolution"`)) || !bytes.Contains(encoded, []byte("profile:complex")) {
		t.Fatalf("frozen plan missing resolution: %s", encoded)
	}
	if bytes.Contains(encoded, []byte("AGENT_API_KEY_VALUE")) {
		t.Fatal("resolution must not carry secret values")
	}
}

// TestPresetReadOnlyForcesDenyAndFailsVerification 锁定 read-only Preset 的组合
// 语义：强制 deny/deny 并记录来源；携带验证命令时失败关闭。
func TestPresetReadOnlyForcesDenyAndFailsVerification(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\npreset: read-only\nprovider:\n  api_key_env: K\npolicy:\n  write: allow\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := Load(path, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	if config.Policy.Write != "deny" || config.Policy.Exec != "deny" {
		t.Fatalf("policy = %s/%s", config.Policy.Write, config.Policy.Exec)
	}
	if source := resolutionSource(config.Resolution, "policy.write"); source != "preset:read-only" {
		t.Fatalf("policy.write source = %q", source)
	}
	withVerify := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(withVerify, []byte("version: 1\npreset: read-only\nprovider:\n  api_key_env: K\nverification:\n  commands:\n    - argv: [\"/usr/bin/true\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(withVerify, Overrides{}); err == nil || !strings.Contains(err.Error(), "read-only preset") {
		t.Fatalf("err = %v", err)
	}
}

// TestUnknownPresetFailsClosedAndFingerprintCoversPreset 锁定未知 Preset 失败
// 关闭与 Preset 进入指纹（能力组合漂移在恢复时被拒绝）。
func TestUnknownPresetFailsClosedAndFingerprintCoversPreset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("version: 1\npreset: turbo\nprovider:\n  api_key_env: K\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, Overrides{}); err == nil || !strings.Contains(err.Error(), "unknown preset") {
		t.Fatalf("err = %v", err)
	}
	coding := Defaults()
	readOnly := Defaults()
	readOnly.Preset = PresetReadOnly
	readOnly.Policy.Write = "deny"
	readOnly.Policy.Exec = "deny"
	if coding.Fingerprint() == readOnly.Fingerprint() {
		t.Fatal("preset must participate in the fingerprint")
	}
	if coding.Preset != PresetCoding {
		t.Fatalf("default preset = %q", coding.Preset)
	}
}
