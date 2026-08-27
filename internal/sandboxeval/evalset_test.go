package sandboxeval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
	maximumOutputTokens := 4096
	profile, err := contract.ResolveProviderRequestProfile(contract.ProviderKindOpenAICompatible, contract.ProviderRequestDeclaration{
		BaseURL: "https://provider.test/v4", Model: "glm-5.2",
		Capability: &contract.ProviderCapabilityDeclaration{ToolCalls: true, StreamingUsage: true},
		Generation: &contract.ProviderGenerationDeclaration{MaxOutputTokens: maximumOutputTokens, ToolChoice: contract.GenerationToolChoiceAuto},
	})
	if err != nil {
		t.Fatalf("provider request profile fixture: %v", err)
	}
	return EvalSet{
		SchemaVersion:       evalSetSchemaV1,
		TaskPackFingerprint: validProfileDigest,
		TaskIDs:             []string{"AgentScope", "TokenTracker", "TopicEye"},
		TaskContracts: []TaskContract{
			{TaskID: "AgentScope", BaselineCommit: "def67890", VerifierDigest: thirdDigest, DockerImageDigest: companionDigest, BootstrapContractDigest: validProfileDigest},
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
		ComparatorContract:          contractPreregisteredComparator(),
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
		"wrong attempts":          func(e *EvalSet) { e.AttemptsPerTask = 2 },
		"wrong task count": func(e *EvalSet) {
			e.TaskIDs = e.TaskIDs[:2]
			e.TaskContracts = e.TaskContracts[:2]
		},
		"concurrency above one":  func(e *EvalSet) { e.ExecutionSchedule.Concurrency = 4 },
		"unknown pair order":     func(e *EvalSet) { e.ExecutionSchedule.PairOrder = "baseline-first-all" },
		"dirty build":            func(e *EvalSet) { e.BuildIdentity.Dirty = "true" },
		"missing comparator ver": func(e *EvalSet) { e.ComparatorContract.ComparatorVersion = "" },
		"negative price":         func(e *EvalSet) { e.PricingSnapshot.InputPriceMicrousdPerMillionTokens = -1 },
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
	if _, err := drifted.Fingerprint(); err == nil {
		t.Fatal("attempts_per_task drift must invalidate the fixed first-generation scope")
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

// contractPreregisteredComparator 返回经唯一入口构造的注册比较器合同。
func contractPreregisteredComparator() ComparatorContract {
	return PreregisteredComparatorContract("cairn-comparator-v1-absolute-gates")
}

// TestComparatorContractRejectsThresholdDeviation 验证阈值偏离（哪怕一个基
// 点）都会使合同非法并被指纹前置校验拦截，令"评测后调阈值"不可达。
func TestComparatorContractRejectsThresholdDeviation(t *testing.T) {
	comparator := contractPreregisteredComparator()
	if err := comparator.Validate(); err != nil {
		t.Fatalf("registered comparator rejected: %v", err)
	}
	drifted := comparator
	drifted.CostPassBasisPoints--
	if err := drifted.Validate(); err == nil || !strings.Contains(err.Error(), "preregistered") {
		t.Fatalf("threshold drift must fail closed against registration, got %v", err)
	}
}

// TestLoadEvalSetStrictDecoding 锁定冻结评测集的加载面：合法往返必须保真，
// 未知字段、尾随文档、损坏 JSON 与结构违约一律失败关闭。
func TestLoadEvalSetStrictDecoding(t *testing.T) {
	evalSet := validEvalSet(t)
	content, err := json.Marshal(evalSet)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	path := filepath.Join(t.TempDir(), "eval-set.json")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	loaded, err := LoadEvalSet(path)
	if err != nil {
		t.Fatalf("load valid eval set: %v", err)
	}
	if !reflect.DeepEqual(loaded, evalSet) {
		t.Fatal("loaded eval set must round-trip the frozen input byte-faithfully")
	}
	negative := map[string][]byte{
		"unknown field":    append(append([]byte{}, content[:len(content)-1]...), []byte(`,"unexpected_field":1}`)...),
		"trailing element": append(append([]byte{}, content...), []byte(` {"schema_version":9}`)...),
		"broken json":      []byte(`{"schema_version":`),
		"empty file":       {},
	}
	for name, payload := range negative {
		brokenPath := filepath.Join(t.TempDir(), "broken-eval-set.json")
		if err := os.WriteFile(brokenPath, payload, 0o600); err != nil {
			t.Fatalf("write %s fixture: %v", name, err)
		}
		if _, err := LoadEvalSet(brokenPath); err == nil {
			t.Fatalf("%s must fail closed", name)
		}
	}
	structurallyBroken := evalSet
	structurallyBroken.AttemptsPerTask = 0
	payload, err := json.Marshal(structurallyBroken)
	if err != nil {
		t.Fatalf("marshal broken structure: %v", err)
	}
	brokenPath := filepath.Join(t.TempDir(), "invalid-contract.json")
	if err := os.WriteFile(brokenPath, payload, 0o600); err != nil {
		t.Fatalf("write invalid contract fixture: %v", err)
	}
	if _, err := LoadEvalSet(brokenPath); err == nil {
		t.Fatal("structurally invalid eval set must fail closed")
	}
}
