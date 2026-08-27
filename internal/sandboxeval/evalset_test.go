package sandboxeval

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/contract"
)

// validProfileDigest 与 companionDigest 是测试共用的形态合法摘要。
const (
	validProfileDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	companionDigest    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	thirdDigest        = "sha256:3333333333333333333333333333333333333333333333333333333333333333"
)

// validEvalSet 返回通过全部结构校验的 EvalSet fixture。
func validEvalSet(t *testing.T) EvalSet {
	t.Helper()
	profile, err := contract.ResolveProviderRequestProfile(contract.ProviderKindOpenAICompatible, contract.ProviderRequestDeclaration{
		BaseURL: "https://provider.test/v4", Model: "glm-5.2",
		Capability: &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true},
	})
	if err != nil {
		t.Fatalf("provider request profile fixture: %v", err)
	}
	return EvalSet{
		SchemaVersion:       evalSetSchemaV1,
		TaskPackFingerprint: validProfileDigest,
		TaskIDs:             []string{"TokenTracker", "TopicEye"},
		TaskContracts: []TaskContract{
			{TaskID: "TokenTracker", BaselineCommit: "abc12345", VerifierDigest: thirdDigest, DockerImageDigest: companionDigest, BootstrapContractDigest: validProfileDigest},
			{TaskID: "TopicEye", BaselineCommit: "2dfeb260", VerifierDigest: thirdDigest, DockerImageDigest: companionDigest, BootstrapContractDigest: validProfileDigest},
		},
		AttemptsPerTask:             3,
		ExecutionSchedule:           ExecutionSchedule{ScheduleVersion: "first-generation", Concurrency: 1, PairOrder: PairOrderAlternating},
		BaselineConfigFingerprint:   validProfileDigest,
		VariantConfigFingerprint:    companionDigest,
		ResolvedExecutionPlanDigest: thirdDigest,
		BuildIdentity:               EvalBuildIdentity{Revision: "5e9007e", Dirty: buildIdentityCleanMarker, Flavor: "release", Fingerprint: thirdDigest},
		ToolRegistryFingerprint:     companionDigest,
		CredentialPolicyFingerprint: validProfileDigest,
		ProviderRequestProfile:      profile,
		PricingSnapshot:             mustPricingSnapshot(t),
		ComparatorContract:          ComparatorContract{SchemaVersion: comparatorContractSchemaV1, ComparatorVersion: "cairn-comparator-v1-absolute-gates"},
	}
}

// TestEvalSetValidationAcceptsOnlyCompleteContracts 锁定 EvalSet 的失败关闭
// 校验面：结构性缺失、乱序任务清单、并发偏离与脏构建身份一律拒绝。
func TestEvalSetValidationRejectsIncompleteContracts(t *testing.T) {
	mutations := map[string]func(*EvalSet){
		"bad schema": func(e *EvalSet) { e.SchemaVersion = 99 },
		"unordered task ids": func(e *EvalSet) {
			e.TaskIDs = []string{"TokenTracker", "TopicEye"}
			e.TaskContracts = []TaskContract{e.TaskContracts[1], e.TaskContracts[0]}
		},
		"duplicated task id":      func(e *EvalSet) { e.TaskIDs = []string{"TopicEye", "TopicEye"} },
		"contracts length drift":  func(e *EvalSet) { e.TaskContracts = e.TaskContracts[:1] },
		"contract id mismatch":    func(e *EvalSet) { e.TaskContracts[0].TaskID = "OtherTask" },
		"missing baseline commit": func(e *EvalSet) { e.TaskContracts[0].BaselineCommit = "" },
		"digest-shaped task ref":  func(e *EvalSet) { e.TaskContracts[0].DockerImageDigest = "image:latest-tag-only" },
		"zero attempts":           func(e *EvalSet) { e.AttemptsPerTask = 0 },
		"concurrency above one":   func(e *EvalSet) { e.ExecutionSchedule.Concurrency = 4 },
		"unknown pair order":      func(e *EvalSet) { e.ExecutionSchedule.PairOrder = "baseline-first-all" },
		"dirty build":             func(e *EvalSet) { e.BuildIdentity.Dirty = "true" },
		"missing comparator ver":  func(e *EvalSet) { e.ComparatorContract.ComparatorVersion = "" },
		"negative price":          func(e *EvalSet) { e.PricingSnapshot.InputPriceMicrousdPerMillionTokens = -1 },
	}
	for name, mutate := range mutations {
		evalSet := validEvalSet(t)
		mutate(&evalSet)
		if err := evalSet.Validate(); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
	if err := validEvalSet(t).Validate(); err != nil {
		t.Fatalf("valid eval set rejected: %v", err)
	}
}

// TestEvalSetFingerprintDeterminismAndSensitivity 验证指纹确定性：相同冻结
// 内容恒同指纹，任一字段漂移都改变指纹且不可对非法结构产出。
func TestEvalSetFingerprintDeterminismAndSensitivity(t *testing.T) {
	evalSet := validEvalSet(t)
	first, err := evalSet.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	second, err := evalSet.Fingerprint()
	if err != nil {
		t.Fatalf("fingerprint again: %v", err)
	}
	if first != second || !strings.HasPrefix(first, "sha256:") {
		t.Fatalf("fingerprint must be deterministic sha256 digests: %q vs %q", first, second)
	}
	drifted := evalSet
	drifted.AttemptsPerTask++
	changed, err := drifted.Fingerprint()
	if err != nil {
		t.Fatalf("drifted fingerprint: %v", err)
	}
	if changed == first {
		t.Fatal("attempts_per_task drift must change the fingerprint")
	}
	broken := evalSet
	broken.BuildIdentity.Dirty = "true"
	if _, err := broken.Fingerprint(); err == nil {
		t.Fatal("invalid structures must not yield a fingerprint")
	}
}

// TestComparatorConstantsMatchAdrThresholds 把比较器万分比阈值钉死在测试里，
// 防止在评测后偷改判定门槛（ADR §5.3/§5.4 数值）。
func TestComparatorConstantsMatchAdrThresholds(t *testing.T) {
	if comparatorCostPassBasisPoints != 13000 || comparatorWallPassBasisPoints != 15000 {
		t.Fatal("pass gates must stay at 13000/15000 basis points")
	}
	if comparatorCostEfficiencyBasisPoints != 8000 || comparatorWallEfficiencyBasisPoints != 11000 {
		t.Fatal("mode B efficiency caps must stay at 8000/11000 basis points")
	}
	if comparatorSuccessImprovementDelta != 2 || basisPointsScale != 10000 {
		t.Fatal("success delta and scale must match the preregistered contract")
	}
}

// mustPricingSnapshot 由真实 USD 单价构造通过校验的定价快照 fixture。
func mustPricingSnapshot(t *testing.T) contract.PricingSnapshot {
	t.Helper()
	snapshot, err := contract.NewPricingSnapshot(0.00000014, 0.00000028)
	if err != nil {
		t.Fatalf("pricing snapshot fixture: %v", err)
	}
	return snapshot
}
