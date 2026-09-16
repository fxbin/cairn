package benchmark

import (
	"testing"
	"time"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// variantFixture 返回通过全部边界校验的压缩参数变体。
func variantFixture() memory.CompactionParamsVariant {
	return memory.CompactionParamsVariant{
		SchemaVersion:           memory.EvolutionSchemaVersion,
		MaxPromptTokens:         24_000,
		MaxSummaryBytes:         8_192,
		InventoryBudgetPercent:  25,
		ThresholdReservePercent: 20,
	}
}

// baseConfigFixture 返回一份合法基线配置：无 provider 能力声明的配置仍应通
// 过整体校验（能力卡由运行装配层把关）。
func baseConfigFixture() config.Config {
	base := config.Defaults()
	base.Provider.BaseURL = "https://provider.test/v1"
	base.Provider.Model = "test-model"
	base.Context.MaxPromptTokens = 48_000
	base.Context.MaxSummaryBytes = 16_384
	return base
}

// TestSandboxScheduleAlternatesArmsWithinAdjacentPairs 锁定成对交错调度：
// 任务升序、pair 相邻、奇数次尝试先基线后变体、偶数次尝试反向。
func TestSandboxScheduleAlternatesArmsWithinAdjacentPairs(t *testing.T) {
	steps, err := SandboxSchedule([]string{"TopicEye", "TokenTracker", "AgentScope"}, 3)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if len(steps) != 18 {
		t.Fatalf("step count = %d, want 18", len(steps))
	}
	if steps[0].TaskID != "AgentScope" || steps[0].Arm != sandboxeval.ArmBaseline || steps[1].Arm != sandboxeval.ArmVariant {
		t.Fatalf("first pair must be baseline then variant on the first task: %#v %#v", steps[0], steps[1])
	}
	// 不变量化校验：相同任务相邻两步构成一对；奇数次尝试先基线后变体，
	// 偶数次尝试反向。
	for i := 0; i < len(steps); i += 2 {
		first, second := steps[i], steps[i+1]
		if first.TaskID != second.TaskID || first.Try != second.Try {
			t.Fatalf("steps %d/%d are not a pair: %#v %#v", i, i+1, first, second)
		}
		wantFirst, wantSecond := sandboxeval.ArmBaseline, sandboxeval.ArmVariant
		if first.Try%2 == 0 {
			wantFirst, wantSecond = sandboxeval.ArmVariant, sandboxeval.ArmBaseline
		}
		if first.Arm != wantFirst || second.Arm != wantSecond {
			t.Fatalf("pair order wrong at try %d (%s): %#v %#v", first.Try, first.TaskID, first, second)
		}
	}
}

// TestExpandCompactionVariantAppliesAndFailsClosed 验证变体四参数全部覆盖真实
// 上下文配置、越界变体被拒且展开结果仍满足配置校验。
func TestExpandCompactionVariantAppliesAndFailsClosed(t *testing.T) {
	base := baseConfigFixture()
	expanded, err := ExpandCompactionVariant(base, variantFixture())
	if err != nil {
		t.Fatalf("expand: %v", err)
	}
	variant := variantFixture()
	if expanded.Context.MaxPromptTokens != 24_000 || expanded.Context.MaxSummaryBytes != 8_192 ||
		expanded.Context.InventoryBudgetPercent != variant.InventoryBudgetPercent ||
		expanded.Context.ThresholdReservePercent != variant.ThresholdReservePercent {
		t.Fatalf("expanded context mismatch: %#v", expanded.Context)
	}
	if expanded.Fingerprint() == base.Fingerprint() {
		t.Fatal("variant expansion must change the frozen config fingerprint")
	}
	outOfRange := variantFixture()
	outOfRange.MaxPromptTokens = 999_999_999
	if _, err := ExpandCompactionVariant(base, outOfRange); err == nil {
		t.Fatal("out-of-range variant must fail closed")
	}
}

// TestVariantExpandedFingerprintSensitivity 锁定展开身份视图：任一成分漂移
// （基线指纹、覆盖值、规格摘要）都改变指纹。
func TestVariantExpandedFingerprintSensitivity(t *testing.T) {
	variant := variantFixture()
	baselineFP := baseConfigFixture().Fingerprint()
	first, err := VariantExpandedFingerprint(baselineFP, variant)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}
	same, err := VariantExpandedFingerprint(baselineFP, variant)
	if err != nil || same != first {
		t.Fatalf("fingerprint must be deterministic: %q vs %q (%v)", first, same, err)
	}
	driftedSpec := variant
	driftedSpec.ThresholdReservePercent++
	changed, err := VariantExpandedFingerprint(baselineFP, driftedSpec)
	if err != nil {
		t.Fatalf("drifted fingerprint: %v", err)
	}
	if changed == first {
		t.Fatal("spec drift must change the expanded identity")
	}
}

// TestRunOfflinePrescreenCatchesFingerprintDrift 锁定预筛会执行内置确定性压缩
// 探针，并拒绝展开指纹与冻结事实的任何漂移。
func TestRunOfflinePrescreenCatchesFingerprintDrift(t *testing.T) {
	variant := variantFixture()
	base := baseConfigFixture()
	evalSet := sandboxeval.EvalSet{}
	baselineFP := base.Fingerprint()
	expandedFP, err := VariantExpandedFingerprint(baselineFP, variant)
	if err != nil {
		t.Fatal(err)
	}
	evalSet.BaselineConfigFingerprint = baselineFP
	evalSet.VariantConfigFingerprint = expandedFP

	report, err := RunOfflinePrescreen(evalSet, base, base, variant)
	if err != nil {
		t.Fatalf("prescreen with consistent fingerprints: %v", err)
	}
	if !report.Passed() {
		t.Fatalf("consistent set must pass, got issues %v", report.Issues)
	}

	evalSet.VariantConfigFingerprint = "sha256:" + string(make([]byte, 64))
	report, err = RunOfflinePrescreen(evalSet, base, base, variant)
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed() {
		t.Fatal("fingerprint divergence must be reported")
	}
	if _, err := RunOfflinePrescreen(evalSet, config.Defaults(), base, variant); err == nil {
		t.Fatal("invalid baseline config must fail before fingerprint derivation")
	}

}

func tooLong(size int) string {
	payload := make([]byte, size)
	for i := range payload {
		payload[i] = 'x'
	}
	return string(payload)
}

func contains(haystack, needle string) bool { return indexOf(haystack, needle) >= 0 }

// guard 引用 time 包避免测试文件未来扩展时遗漏导入带来的编译歧义。
var _ = time.Second
