package config

import (
	"os"
	"path/filepath"
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
