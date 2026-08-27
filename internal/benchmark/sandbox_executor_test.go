package benchmark

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// sandboxFixtureDigest 是测试中形态合法的占位 sha256 摘要。
const sandboxFixtureDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// sandboxProviderKey 是测试注入宿主环境的沙箱 Key 占位值：执行器只在内存中
// 消费它，绝不落盘。
const sandboxProviderKey = "sandbox-test-key-value"

// sandboxVariant 返回变体参数域内的合法压缩参数 fixture。
func sandboxVariant() memory.CompactionParamsVariant {
	return memory.CompactionParamsVariant{
		SchemaVersion:           memory.EvolutionSchemaVersion,
		MaxPromptTokens:         4096,
		MaxSummaryBytes:         8192,
		InventoryBudgetPercent:  40,
		ThresholdReservePercent: 25,
	}
}

// sandboxPolicy 返回形态合法且自洽的凭据策略 fixture。
func sandboxPolicy() sandboxeval.CredentialPolicy {
	return sandboxeval.CredentialPolicy{
		SchemaVersion:               1,
		EnvName:                     sandboxeval.SandboxProviderKeyEnv,
		AllowedCredentialProfileIDs: []string{"sandbox-profile"},
		ProviderIdentityFingerprint: sandboxFixtureDigest,
		SuiteMaxWallMilliseconds:    24 * time.Hour.Milliseconds(),
	}
}

// sandboxSetup 聚合一次沙箱评测输入的全部自洽 fixture。
type sandboxSetup struct {
	source         string
	baseline       string
	pack           TaskPack
	evalSet        sandboxeval.EvalSet
	variant        memory.CompactionParamsVariant
	policy         sandboxeval.CredentialPolicy
	baselineConfig config.Config
}

// buildSandboxSetup 组装任务包、基线配置、变体与冻结评测集，并保证指纹、任
// 务合同摘要与冻结身份全部互洽；mutate 允许单个用例在自洽完成后定点制造漂移。
func buildSandboxSetup(t *testing.T, mutate func(*sandboxeval.EvalSet)) sandboxSetup {
	t.Helper()
	t.Setenv(sandboxeval.SandboxProviderKeyEnv, sandboxProviderKey)
	source, baseline := localGitRepository(t)
	pack := testPack(source, baseline)
	baselineConfig := config.Defaults()
	baselineConfig.Provider.APIKeyEnv = sandboxeval.SandboxProviderKeyEnv
	variant := sandboxVariant()
	policy := sandboxPolicy()
	tasks, err := pack.SelectedTasks([]string{pack.Tasks[0].ID})
	if err != nil {
		t.Fatalf("select task fixture: %v", err)
	}
	bootstrapDigest, err := BootstrapContractDigest(tasks[0].Verification.Bootstrap)
	if err != nil {
		t.Fatalf("bootstrap digest fixture: %v", err)
	}
	profile, err := contract.ResolveProviderRequestProfile(contract.ProviderKindOpenAICompatible, contract.ProviderRequestDeclaration{
		BaseURL: "https://provider.test/v4", Model: "test-model",
		Capability: &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true},
	})
	if err != nil {
		t.Fatalf("provider profile fixture: %v", err)
	}
	pricing, err := contract.NewPricingSnapshot(0.00000014, 0.00000028)
	if err != nil {
		t.Fatalf("pricing snapshot fixture: %v", err)
	}
	baselineFingerprint := baselineConfig.Fingerprint()
	variantFingerprint, err := VariantExpandedFingerprint(baselineFingerprint, variant)
	if err != nil {
		t.Fatalf("variant fingerprint fixture: %v", err)
	}
	policyFingerprint, err := policy.Fingerprint()
	if err != nil {
		t.Fatalf("policy fingerprint fixture: %v", err)
	}
	taskIDs := []string{tasks[0].ID}
	evalSet := sandboxeval.EvalSet{
		SchemaVersion:       1,
		TaskPackFingerprint: pack.Fingerprint(),
		TaskIDs:             taskIDs,
		TaskContracts: []sandboxeval.TaskContract{{
			TaskID:                  tasks[0].ID,
			BaselineCommit:          baseline,
			VerifierDigest:          sandboxFixtureDigest,
			DockerImageDigest:       sandboxFixtureDigest,
			BootstrapContractDigest: bootstrapDigest,
		}},
		AttemptsPerTask:             1,
		ExecutionSchedule:           sandboxeval.ExecutionSchedule{ScheduleVersion: "fixture", Concurrency: 1, PairOrder: sandboxeval.PairOrderAlternating},
		BaselineConfigFingerprint:   baselineFingerprint,
		VariantConfigFingerprint:    variantFingerprint,
		ResolvedExecutionPlanDigest: sandboxFixtureDigest,
		BuildIdentity:               sandboxeval.EvalBuildIdentity{Revision: "fixture", Dirty: "false", Flavor: "release", Fingerprint: sandboxFixtureDigest},
		ToolRegistryFingerprint:     sandboxFixtureDigest,
		ProviderRequestProfile:      profile,
		PricingSnapshot:             pricing,
		CredentialPolicyFingerprint: policyFingerprint,
		ComparatorContract:          sandboxeval.PreregisteredComparatorContract("cairn-fixture-comparator"),
	}
	if mutate != nil {
		mutate(&evalSet)
	}
	return sandboxSetup{source: source, baseline: baseline, pack: pack, evalSet: evalSet, variant: variant, policy: policy, baselineConfig: baselineConfig}
}

// executorOptions 把 fixture 装配成指向临时目录的完整执行输入。
func (setup sandboxSetup) executorOptions(t *testing.T, candidateID string, dryRun bool) SandboxExecutorOptions {
	t.Helper()
	output := t.TempDir()
	lease, err := sandboxLeaseFor(setup.policy, time.Now().UTC())
	if err != nil {
		t.Fatalf("lease fixture: %v", err)
	}
	return SandboxExecutorOptions{
		Pack:           setup.pack,
		EvalSet:        setup.evalSet,
		Lease:          lease,
		Policy:         setup.policy,
		CandidateID:    candidateID,
		MemoryStateDir: filepath.Join(output, "memory-state"),
		BaselineConfig: setup.baselineConfig,
		Variant:        setup.variant,
		StateRoot:      filepath.Join(output, "state"),
		WorkspacesRoot: filepath.Join(output, "workspaces"),
		PrivateRoot:    filepath.Join(output, "private"),
		CairnRoot:      repositoryRoot(t),
		PrepareTimeout: time.Minute,
		DryRun:         dryRun,
	}
}

// TestSandboxPrecheckRejectsStructuralDivergence 锁定启动前的结构性失败关闭：
// 任务包指纹、冻结基线提交与 bootstrap 合同摘要任一漂移都必须在产生任何持久
// 化之前被拒绝，评测目录不创建、候选不被触动。
func TestSandboxPrecheckRejectsStructuralDivergence(t *testing.T) {
	mutations := map[string]func(*sandboxeval.EvalSet){
		"pack fingerprint divergence": func(e *sandboxeval.EvalSet) { e.TaskPackFingerprint = sandboxFixtureDigest },
		"baseline commit divergence":  func(e *sandboxeval.EvalSet) { e.TaskContracts[0].BaselineCommit = strings.Repeat("0", 40) },
		"bootstrap digest divergence": func(e *sandboxeval.EvalSet) { e.TaskContracts[0].BootstrapContractDigest = sandboxFixtureDigest },
	}
	for name, mutate := range mutations {
		setup := buildSandboxSetup(t, mutate)
		options := setup.executorOptions(t, "candidate-fixture", true)
		outcome, err := (SandboxExecutor{}).Run(context.Background(), options)
		if err == nil || outcome.DryRun {
			t.Fatalf("%s must fail closed before any persistence: outcome=%#v err=%v", name, outcome, err)
		}
		if _, statErr := os.Stat(filepath.Join(options.StateRoot, "evaluations")); !os.IsNotExist(statErr) {
			t.Fatalf("%s must not create the evaluations tree before checks pass: %v", name, statErr)
		}
	}
}

// TestEnsureEmptyEvaluationDirectoryRejectsResidue 锁定重入残件守卫：空目录可
// 创建（幂等重入合法），任何既有产物都会阻止新一轮写入，防止已闭合 Run 被
// 静默覆盖。
func TestEnsureEmptyEvaluationDirectoryRejectsResidue(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "evaluations", "abc")
	if err := ensureEmptyEvaluationDirectory(directory); err != nil {
		t.Fatalf("fresh directory must be accepted: %v", err)
	}
	stalePath := filepath.Join(directory, "stale-run.json")
	if err := os.WriteFile(stalePath, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write residue fixture: %v", err)
	}
	if err := ensureEmptyEvaluationDirectory(directory); err == nil {
		t.Fatal("residue in the evaluation directory must fail closed")
	}
}

// TestReplayConsistencyDetectsProjectionTampering 锁定二次确定性比对的两条失
// 败面：事件流载荷违约必须以错误暴露，静默改写投影终态必须被判不一致。
func TestReplayConsistentWithProjectionRejectsTampering(t *testing.T) {
	if _, err := replayConsistentWithProjection(domain.RunProjection{}, []domain.Event{{Seq: 1, Type: domain.EventTurnClosed, Payload: []byte(`{"outcome":"weird"}`)}}); err == nil {
		t.Fatal("malformed event stream must surface an error")
	}
	consistent, err := replayConsistentWithProjection(domain.RunProjection{State: domain.RunCompleted}, nil)
	if err != nil {
		t.Fatalf("empty stream must replay cleanly: %v", err)
	}
	if consistent {
		t.Fatal("a completed projection cannot be consistent with an empty event stream")
	}
}

// TestLeaseLedgerCarriesRemainderAndExhaustsDimensions 锁定租约记账的纯整数
// 进位法与逐维耗尽判定：余数跨步滚存进位不得丢失，任一维度触及上限即视为
// 耗尽。
func TestLeaseLedgerCarriesRemainderAndExhaustsDimensions(t *testing.T) {
	ledger := &leaseLedger{}
	ledger.chargeUnits(sandboxeval.CostUnitsPerMicroUSD - 1)
	if ledger.carriedMicroUSD != 0 || ledger.remainderUnits != sandboxeval.CostUnitsPerMicroUSD-1 {
		t.Fatalf("first charge must stay in remainder: %+v", ledger)
	}
	ledger.chargeUnits(2)
	if ledger.carriedMicroUSD != 1 || ledger.remainderUnits != 1 {
		t.Fatalf("remainder must carry into micro-USD: %+v", ledger)
	}
	exhausting := sandboxeval.CredentialLease{MaxRequests: 5, MaxInputTokens: 1000, MaxOutputTokens: 2000, MaxCostMicrousd: 1}
	if !ledger.exceedsLease(exhausting) {
		t.Fatal("reached cost ceiling must count as exhaustion")
	}
	tolerant := sandboxeval.CredentialLease{MaxRequests: 5, MaxInputTokens: 1000, MaxOutputTokens: 2000, MaxCostMicrousd: 2}
	if ledger.exceedsLease(tolerant) {
		t.Fatal("unreached dimensions must not count as exhaustion")
	}
	byDimension := map[string]struct {
		lease    sandboxeval.CredentialLease
		saturate func(*leaseLedger)
	}{
		"requests":      {lease: exhausting, saturate: func(l *leaseLedger) { l.requests = 5 }},
		"input tokens":  {lease: exhausting, saturate: func(l *leaseLedger) { l.inputTokens = 1000 }},
		"output tokens": {lease: exhausting, saturate: func(l *leaseLedger) { l.outputTokens = 2000 }},
	}
	for name, scenario := range byDimension {
		fresh := &leaseLedger{}
		scenario.saturate(fresh)
		if !fresh.exceedsLease(scenario.lease) {
			t.Fatalf("%s saturation must count as exhaustion", name)
		}
	}
	if (&leaseLedger{}).exceedsLease(exhausting) {
		t.Fatal("an empty ledger must not count as exhausted")
	}
	usageLedger := &leaseLedger{}
	usageLedger.chargeUsage(900, 1500, 4)
	if usageLedger.inputTokens != 900 || usageLedger.outputTokens != 1500 || usageLedger.requests != 4 {
		t.Fatalf("usage accumulation drifted: %+v", usageLedger)
	}
}

// TestSandboxExecutorDryRunRunsPrescreenOnly 验证 dry-run 入口的完整行为：
// 自洽输入走完全部启动前检查后以 DryRun 收束且零持久化（评测目录树不出现）；
// 冻结指纹漂移则以预筛失败类型暴露，供调用方判定 invalid 结论轨。
func TestSandboxExecutorDryRunRunsPrescreenOnly(t *testing.T) {
	setup := buildSandboxSetup(t, nil)
	options := setup.executorOptions(t, "candidate-dry-fixture", true)
	outcome, err := (SandboxExecutor{}).Run(context.Background(), options)
	if err != nil || !outcome.DryRun {
		t.Fatalf("self-consistent dry run must pass: outcome=%#v err=%v", outcome, err)
	}
	if _, statErr := os.Stat(filepath.Join(options.StateRoot, "evaluations")); !os.IsNotExist(statErr) {
		t.Fatalf("dry run must not create the evaluations tree: %v", statErr)
	}
	drifted := buildSandboxSetup(t, func(e *sandboxeval.EvalSet) { e.BaselineConfigFingerprint = sandboxFixtureDigest })
	driftedOptions := drifted.executorOptions(t, "candidate-dry-fixture", true)
	_, driftErr := (SandboxExecutor{}).Run(context.Background(), driftedOptions)
	failure := &prescreenFailure{}
	if !errors.As(driftErr, &failure) || len(failure.issues) == 0 || !strings.Contains(failure.Error(), "baseline config fingerprint") {
		t.Fatalf("frozen fingerprint drift must surface as prescreen failure: %v", driftErr)
	}
}

// sandboxLeaseFor 由策略最小需求构造出覆盖其上的合法租约 fixture；到期时刻
// 取套件最坏截止之后的一天。
func sandboxLeaseFor(policy sandboxeval.CredentialPolicy, now time.Time) (sandboxeval.CredentialLease, error) {
	expiry := now.Add(time.Duration(policy.SuiteMaxWallMilliseconds)*time.Millisecond + 24*time.Hour)
	lease := sandboxeval.CredentialLease{
		SchemaVersion:               1,
		LeaseID:                     "fixture-lease",
		CredentialProfileID:         "sandbox-profile",
		ProviderIdentityFingerprint: policy.ProviderIdentityFingerprint,
		EnvName:                     policy.EnvName,
		IssuedAt:                    now.Add(-time.Hour),
		ExpiresAt:                   expiry,
		MaxRequests:                 100,
		MaxInputTokens:              1 << 30,
		MaxOutputTokens:             1 << 30,
		MaxCostMicrousd:             10_000_000,
		ServerSideQuotaEnabled:      true,
		Revocable:                   true,
	}
	if err := sandboxeval.AuthorizeLease(lease, policy, now); err != nil {
		return sandboxeval.CredentialLease{}, err
	}
	return lease, nil
}
