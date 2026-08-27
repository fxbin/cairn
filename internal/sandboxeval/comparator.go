package sandboxeval

import (
	"errors"
	"fmt"
	"sort"
)

// SuiteArm 是候选评测的执行臂：baseline 为对照配置，variant 为待评审配置。
type SuiteArm string

// 两臂闭合词表；RunVerdict 不得携带其他取值。
const (
	ArmBaseline SuiteArm = "baseline"
	ArmVariant  SuiteArm = "variant"
)

// 比较器结论的闭合词表，与 Memory 评测行状态的结论子集逐字一致。
const (
	ConclusionPassed       = "passed"
	ConclusionFailed       = "failed"
	ConclusionInconclusive = "inconclusive"
	ConclusionInvalid      = "invalid"
)

// SuiteVerdict 是单个 Run 的最小判定投影：由编排器从事件流与外部检查中提
// 取，比较器只读取本结构与冻结 EvalSet，绝不接触原始正文、路径或参考答案。
type SuiteVerdict struct {
	Arm                  SuiteArm
	TaskID               string
	Success              bool
	SecurityIncidents    int
	DoctorReplayBroken   bool // doctor 或确定性回放任一失败即为真
	ToolPairingBreaks    int
	RepeatedExplorations int
	InputTokens          int64
	OutputTokens         int64
	WallTimeMS           int64
}

// Conclusion 是比较器的闭合输出：结论态之外附触发门槛名称，供本地评测报
// 告归档与人工复核追溯。
type Conclusion struct {
	Status  string `json:"status"`
	Trigger string `json:"trigger,omitempty"`
}

// armAggregate 是单臂的聚合指标集合，跨任务汇总并保留按任务的细分面。
type armAggregate struct {
	successes          int
	successesByTask    map[string]int
	repeatedByTask     map[string]int
	securityIncidents  int
	doctorReplayBroken int
	toolPairingBreaks  int
	costUnits          int64
	wallTimeMS         []int64
}

// newArmAggregate 构造以任务清单预置键位的空聚合并返回两臂。
func newArms(taskIDs []string) (armAggregate, armAggregate) {
	baseline := armAggregate{successesByTask: map[string]int{}, repeatedByTask: map[string]int{}}
	variant := armAggregate{successesByTask: map[string]int{}, repeatedByTask: map[string]int{}}
	for _, taskID := range taskIDs {
		baseline.successesByTask[taskID] = 0
		baseline.repeatedByTask[taskID] = 0
		variant.successesByTask[taskID] = 0
		variant.repeatedByTask[taskID] = 0
	}
	return baseline, variant
}

// medianOdd 返回臂内壁钟值的中位数：判定样本恒为奇数（attempts × 任务数），
// 排序后取正中一位；偶数样本属于冻结合同破坏并失败关闭。
func medianOdd(values []int64) (int64, error) {
	if len(values)%2 == 0 {
		return 0, errors.New("median wall time requires an odd sample count")
	}
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(left, right int) bool { return sorted[left] < sorted[right] })
	return sorted[len(sorted)/2], nil
}

// scaleOverflowFree 以 checked 运算计算 value×scale；任何溢出都令成本与时
// 延比较不可计算，必须失败关闭而不是折回负数继续评审。
func scaleOverflowFree(value int64, scale int64) (int64, error) {
	if value == 0 {
		return 0, nil
	}
	product := value * scale
	if product/value != scale {
		return 0, fmt.Errorf("scaling %d by %d overflows the comparison range", value, scale)
	}
	return product, nil
}

// violatesBasisPointGate 判定是否越过万分比上限门：variant×10000 必须
// 不大于 baseline×gate；baseline 为零则无从对照，由调用方先行处理。
func violatesBasisPointGate(baselineValue, variantValue int64, gate int64) (bool, error) {
	scaledVariant, err := scaleOverflowFree(variantValue, basisPointsScale)
	if err != nil {
		return false, err
	}
	scaledGate, err := scaleOverflowFree(baselineValue, gate)
	if err != nil {
		return false, err
	}
	return scaledVariant > scaledGate, nil
}

// runCostUnits 按 ADR 公式累计单个 Run 的整数成本单位；tokens 与单价均来
// 自冻结快照，永不除一百万，也不做浮点估算。
func runCostUnits(verdict SuiteVerdict, pricing contractPricingView) (int64, error) {
	inputPart, err := scaleOverflowFree(verdict.InputTokens, pricing.inputPrice)
	if err != nil {
		return 0, err
	}
	outputPart, err := scaleOverflowFree(verdict.OutputTokens, pricing.outputPrice)
	if err != nil {
		return 0, err
	}
	total := inputPart + outputPart
	if total < inputPart || total < outputPart {
		return 0, errors.New("cost units addition overflows")
	}
	return total, nil
}

// contractPricingView 是 EvalSet 冻结定价的最小读取视图，仅供比较器内部使用。
type contractPricingView struct {
	inputPrice  int64
	outputPrice int64
}

// pricingView 从冻结 EvalSet 提取成本比较所需的整数千价视图。
func (evalSet EvalSet) pricingView() contractPricingView {
	return contractPricingView{
		inputPrice:  evalSet.PricingSnapshot.InputPriceMicrousdPerMillionTokens,
		outputPrice: evalSet.PricingSnapshot.OutputPriceMicrousdPerMillionTokens,
	}
}

// aggregateRuns 把投影按臂聚合；arm 取值未知、任务不在冻结清单等结构漂移
// 都令整套证据作废。
func aggregateRuns(evalSet EvalSet, runs []SuiteVerdict) (armAggregate, armAggregate, error) {
	baseline, variant := newArms(evalSet.TaskIDs)
	taskSet := map[string]bool{}
	for _, taskID := range evalSet.TaskIDs {
		taskSet[taskID] = true
	}
	pricing := evalSet.pricingView()
	for _, verdict := range runs {
		switch verdict.Arm {
		case ArmBaseline:
		case ArmVariant:
		default:
			return baseline, variant, fmt.Errorf("run for task %q carries an unknown arm", verdict.TaskID)
		}
		if !taskSet[verdict.TaskID] {
			return baseline, variant, fmt.Errorf("run task %q is not part of the frozen eval set", verdict.TaskID)
		}
		target := &baseline
		if verdict.Arm == ArmVariant {
			target = &variant
		}
		target.successesByTask[verdict.TaskID] += boolCount(verdict.Success)
		target.securityIncidents += verdict.SecurityIncidents
		if verdict.DoctorReplayBroken {
			target.doctorReplayBroken++
		}
		target.toolPairingBreaks += verdict.ToolPairingBreaks
		target.repeatedByTask[verdict.TaskID] += verdict.RepeatedExplorations
		cost, err := runCostUnits(verdict, pricing)
		if err != nil {
			return baseline, variant, err
		}
		target.costUnits += cost
		target.wallTimeMS = append(target.wallTimeMS, verdict.WallTimeMS)
	}
	var baselineSuccesses, variantSuccesses int
	for _, count := range baseline.successesByTask {
		baselineSuccesses += count
	}
	for _, count := range variant.successesByTask {
		variantSuccesses += count
	}
	baseline.successes, variant.successes = baselineSuccesses, variantSuccesses
	return baseline, variant, nil
}

// boolCount 把布尔事实折叠为计数。
func boolCount(flag bool) int {
	if flag {
		return 1
	}
	return 0
}

// JudgeSuite 以预注册门槛评审整套 Run。裁决次序与 ADR 主次关系严格一致：
// 套件不完备 → inconclusive；基线侧安全/证据完整性破坏或溢出 → invalid；
// 变体单侧违反安全、证据、配对、绝对门与非退化门 → failed；通过绝对门与
// 非退化门后须满足改善模式 A 或 B 方为 passed，其余一律 inconclusive。
func JudgeSuite(evalSet EvalSet, runs []SuiteVerdict) (Conclusion, error) {
	if err := evalSet.Validate(); err != nil {
		return Conclusion{}, fmt.Errorf("judge requires a valid eval set: %w", err)
	}
	expectedRuns := len(evalSet.TaskIDs) * evalSet.AttemptsPerTask * 2
	if len(runs) != expectedRuns {
		return Conclusion{Status: ConclusionInconclusive, Trigger: "incomplete_suite"}, nil
	}
	baseline, variant, err := aggregateRuns(evalSet, runs)
	if err != nil {
		return Conclusion{}, err
	}
	comparator := evalSet.ComparatorContract
	if baseline.securityIncidents > 0 {
		return Conclusion{Status: ConclusionInvalid, Trigger: "baseline_security"}, nil
	}
	if baseline.toolPairingBreaks > 0 {
		return Conclusion{Status: ConclusionInvalid, Trigger: "baseline_tool_pairing"}, nil
	}
	if baseline.doctorReplayBroken > 0 {
		return Conclusion{Status: ConclusionInvalid, Trigger: "baseline_evidence_integrity"}, nil
	}
	if variant.securityIncidents > 0 {
		return Conclusion{Status: ConclusionFailed, Trigger: "variant_security"}, nil
	}
	if variant.doctorReplayBroken > 0 {
		return Conclusion{Status: ConclusionFailed, Trigger: "variant_evidence_integrity"}, nil
	}
	if variant.toolPairingBreaks > 0 {
		return Conclusion{Status: ConclusionFailed, Trigger: "variant_tool_pairing"}, nil
	}
	if baseline.costUnits == 0 {
		// 基线成本为零使万分比门失去可对照量纲（免费对照不存在可比较的
		// 成本事实），按比较不可计算映射为 invalid，而不是崩溃或改判退化。
		return Conclusion{Status: ConclusionInvalid, Trigger: "baseline_zero_cost"}, nil
	}
	costViolated, err := violatesBasisPointGate(baseline.costUnits, variant.costUnits, int64(comparator.CostPassBasisPoints))
	if err != nil {
		return Conclusion{}, err
	}
	if costViolated {
		return Conclusion{Status: ConclusionFailed, Trigger: "absolute_cost_gate"}, nil
	}
	baselineWall, err := medianOdd(baseline.wallTimeMS)
	if err != nil {
		return Conclusion{}, err
	}
	if baselineWall == 0 {
		return Conclusion{Status: ConclusionInvalid, Trigger: "baseline_zero_wall"}, nil
	}
	variantWall, err := medianOdd(variant.wallTimeMS)
	if err != nil {
		return Conclusion{}, err
	}
	wallViolated, err := violatesBasisPointGate(baselineWall, variantWall, int64(comparator.WallPassBasisPoints))
	if err != nil {
		return Conclusion{}, err
	}
	if wallViolated {
		return Conclusion{Status: ConclusionFailed, Trigger: "absolute_wall_gate"}, nil
	}
	for _, taskID := range evalSet.TaskIDs {
		if variant.repeatedByTask[taskID] > baseline.repeatedByTask[taskID] {
			return Conclusion{Status: ConclusionFailed, Trigger: "non_degenerate_exploration"}, nil
		}
		if variant.successesByTask[taskID] < baseline.successesByTask[taskID] {
			return Conclusion{Status: ConclusionFailed, Trigger: "non_degenerate_success"}, nil
		}
	}
	modeTrigger := improvementMode(evalSet.TaskIDs, comparator, baseline, variant, variantWall, baselineWall)
	if modeTrigger == "" {
		return Conclusion{Status: ConclusionInconclusive, Trigger: "insufficient_improvement"}, nil
	}
	return Conclusion{Status: ConclusionPassed, Trigger: modeTrigger}, nil
}

// improvementMode 在非退化前提下评估两个预注册改善模式，命中即返回其标识，
// 全未命中返回空串。
func improvementMode(taskIDs []string, comparator ComparatorContract, baseline, variant armAggregate, variantWall, baselineWall int64) string {
	if variant.successes >= baseline.successes+comparator.SuccessImprovementDelta {
		return "mode_a_effectiveness"
	}
	for _, taskID := range taskIDs {
		if variant.successesByTask[taskID] != baseline.successesByTask[taskID] {
			return ""
		}
	}
	if withinBasisPointCap(baseline.costUnits, variant.costUnits, int64(comparator.CostEfficiencyBasisPoints)) &&
		withinBasisPointCap(baselineWall, variantWall, int64(comparator.WallEfficiencyBasisPoints)) {
		return "mode_b_efficiency"
	}
	return ""
}

// withinBasisPointCap 判定变体值是否落在基线值的倍率帽内。
func withinBasisPointCap(baselineValue, variantValue int64, cap int64) bool {
	violated, err := violatesBasisPointGate(baselineValue, variantValue, cap)
	if err != nil {
		return false
	}
	return !violated
}
