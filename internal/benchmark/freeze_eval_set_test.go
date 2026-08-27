package benchmark

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// frozenFakeImageDigest 是镜像自省替身返回的自描述摘要，形态须与真实 docker
// .Id 输出一致。
const frozenFakeImageDigest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// TestFreezeEvaluationAssetsProducesSelfCheckedArtifacts 锁定冻结装配合同：
// 自洽输入经执行器启动前检查全套后落盘两份产物，回读保真且两个指纹稳定；
// 私有 verifier 缺失则整体失败且零落盘。
func TestFreezeEvaluationAssetsProducesSelfCheckedArtifacts(t *testing.T) {
	t.Setenv(sandboxeval.SandboxProviderKeyEnv, sandboxProviderKey)
	source, baseline := localGitRepository(t)
	pack := sandboxThreeTaskPack(testPack(source, baseline))
	output := t.TempDir()
	// 冻结口径要求真实文件装载：解析计划记录由装载过程生成，手工构造的默认
	// 配置没有该事实，不能充当冻结输入。
	agentConfigPath := filepath.Join(output, "agent-config.yaml")
	agentConfigYAML := "version: 1\nprovider:\n  base_url: https://provider.test/v4\n  model: test-model\n  api_key_env: " + sandboxeval.SandboxProviderKeyEnv + "\n  capability:\n    tool_calls: true\n    streaming_usage: true\n  generation:\n    max_output_tokens: 4096\n    tool_choice: auto\npricing:\n  input_token_usd: 0.00000014\n  output_token_usd: 0.00000028\n"
	if err := os.WriteFile(agentConfigPath, []byte(agentConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	baselineConfig, err := config.Load(agentConfigPath, config.Overrides{})
	if err != nil {
		t.Fatalf("load agent config fixture: %v", err)
	}
	privateRoot := filepath.Join(output, "private", "verifiers")
	if err := os.MkdirAll(privateRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, task := range pack.Tasks {
		path := filepath.Join(privateRoot, safePathComponent(task.ID)+".patch")
		if err := os.WriteFile(path, []byte("verifier patch bytes"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	originalProbe := dockerImageSelfDigestLookup
	dockerImageSelfDigestLookup = func(_ context.Context, _ string) (string, error) {
		return frozenFakeImageDigest, nil
	}
	defer func() { dockerImageSelfDigestLookup = originalProbe }()
	originalIdentity := frozenBuildIdentityLookup
	frozenBuildIdentityLookup = func() (sandboxeval.EvalBuildIdentity, error) {
		return sandboxeval.EvalBuildIdentity{Revision: "frozen-fixture", Dirty: "false", Flavor: "plain", Fingerprint: sandboxFixtureDigest}, nil
	}
	defer func() { frozenBuildIdentityLookup = originalIdentity }()

	options := FreezeEvalSetOptions{
		Pack: pack, TaskIDs: []string{pack.Tasks[0].ID, pack.Tasks[1].ID, pack.Tasks[2].ID}, AttemptsPerTask: sandboxeval.FirstEvalAttemptsPerTask,
		BaselineConfig:           baselineConfig,
		Variant:                  sandboxVariant(),
		ComparatorVersion:        "cairn-freeze-fixture-comparator",
		PolicyProfileID:          "sandbox-profile",
		SuiteMaxWallMilliseconds: (24 * time.Hour).Milliseconds(),
		MinRequests:              100, MinInputTokens: 1 << 20,
		MinOutputTokens: 1 << 20, MinCostMicrousd: 10_000_000,
		PrivateRoot: filepath.Join(output, "private"),
		OutDir:      filepath.Join(output, "frozen"),
	}
	report, err := FreezeEvaluationAssets(context.Background(), options)
	if err != nil {
		t.Fatalf("self-consistent freeze must pass: %v", err)
	}
	if !strings.HasPrefix(report.EvalSetFingerprint, "sha256:") || !strings.HasPrefix(report.CredentialPolicyFingerprint, "sha256:") {
		t.Fatalf("fingerprints must be digest references: %#v", report)
	}
	evalSet, err := sandboxeval.LoadEvalSet(report.EvalSetPath)
	if err != nil {
		t.Fatalf("round-trip the frozen eval set: %v", err)
	}
	rebuiltFingerprint, err := evalSet.Fingerprint()
	if err != nil || rebuiltFingerprint != report.EvalSetFingerprint {
		t.Fatalf("persisted eval set must reproduce its fingerprint: %q vs %q (%v)", rebuiltFingerprint, report.EvalSetFingerprint, err)
	}
	policy, err := sandboxeval.LoadCredentialPolicy(report.CredentialPolicyPath)
	if err != nil {
		t.Fatalf("round-trip the credential policy: %v", err)
	}
	policyFingerprint, err := policy.Fingerprint()
	if err != nil || policyFingerprint != report.CredentialPolicyFingerprint {
		t.Fatalf("persisted policy must reproduce its fingerprint: %q vs %q (%v)", policyFingerprint, report.CredentialPolicyFingerprint, err)
	}

	missingVerifierOptions := options
	missingVerifierOptions.PrivateRoot = filepath.Join(output, "absent-private")
	missingOutDir := filepath.Join(output, "frozen-missing-verifier")
	missingVerifierOptions.OutDir = missingOutDir
	failure, err := FreezeEvaluationAssets(context.Background(), missingVerifierOptions)
	if err == nil || failure.EvalSetPath != "" {
		t.Fatalf("a missing private verifier must fail the whole assembly: %#v err=%v", failure, err)
	}
	if _, statErr := os.Stat(missingOutDir); !os.IsNotExist(statErr) {
		t.Fatalf("failed assembly must not write anything: %v", statErr)
	}
}
