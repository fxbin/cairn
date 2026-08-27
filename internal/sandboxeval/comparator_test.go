package sandboxeval

import (
	"strings"
	"testing"
)

// suiteFixture 构造单任务三次尝试的 EvalSet 与两臂全绿投影，供各门用例在其
// 基础上做单点变异。
func suiteFixture(t *testing.T) (EvalSet, []SuiteVerdict) {
	t.Helper()
	evalSet := validEvalSet(t)
	runCount := len(evalSet.TaskIDs) * evalSet.AttemptsPerTask * 2
	runs := make([]SuiteVerdict, 0, runCount)
	for _, taskID := range evalSet.TaskIDs {
		for attempt := 0; attempt < evalSet.AttemptsPerTask; attempt++ {
			baselineSuccess := attempt != 0 // 基线每任务首试失败，与变体形成有效性差距
			runs = append(runs,
				SuiteVerdict{Arm: ArmBaseline, TaskID: taskID, Success: baselineSuccess, RepeatedExplorations: 2, InputTokens: 1000, OutputTokens: 500, WallTimeMS: 1000},
				SuiteVerdict{Arm: ArmVariant, TaskID: taskID, Success: true, RepeatedExplorations: 1, InputTokens: 800, OutputTokens: 400, WallTimeMS: 950},
			)
		}
	}
	return evalSet, runs
}

// TestJudgeSuitePassesOnModeAAndModeBFixtures 锁定两个改善模式的正路径：
// 模式 A 以额外成功越过门槛；模式 B 在质量全等时以成本效率达标。
func TestJudgeSuitePassesOnModeAAndModeB(t *testing.T) {
	evalSet, runs := suiteFixture(t)
	conclusion, err := JudgeSuite(evalSet, runs)
	if err != nil {
		t.Fatalf("judge mode A fixture: %v", err)
	}
	if conclusion.Status != ConclusionPassed || conclusion.Trigger != "mode_a_effectiveness" {
		t.Fatalf("expected mode A pass, got %#v", conclusion)
	}

	// 模式 B：抹平成功差并以显著的成本节省越过效率帽（时延同值不构成退化）。
	modeB := make([]SuiteVerdict, 0, len(runs))
	for index := range runs {
		copied := runs[index]
		copied.Success = true
		copied.InputTokens = 1500
		copied.OutputTokens = 400
		copied.WallTimeMS = 950
		if copied.Arm == ArmBaseline {
			copied.InputTokens = 2200
			copied.OutputTokens = 500
			copied.WallTimeMS = 950
		}
		modeB = append(modeB, copied)
	}
	conclusion, err = JudgeSuite(evalSet, modeB)
	if err != nil {
		t.Fatalf("judge mode B fixture: %v", err)
	}
	if conclusion.Status != ConclusionPassed || conclusion.Trigger != "mode_b_efficiency" {
		t.Fatalf("expected mode B pass, got %#v", conclusion)
	}
}

// TestJudgeSuitePriorityOrdering 表驱动锁定主次关系：套件不完备→inconclusive；
// 基线侧破坏→invalid；变体单侧违反安全/证据/配对/绝对门/非退化门→failed。
func TestJudgeSuitePriorityOrdering(t *testing.T) {
	cases := []struct {
		name           string
		mutate         func([]SuiteVerdict) []SuiteVerdict
		expectedStatus string
		expectedHint   string
	}{
		{
			name:           "incomplete suite",
			mutate:         func(runs []SuiteVerdict) []SuiteVerdict { return runs[:len(runs)-1] },
			expectedStatus: ConclusionInconclusive,
			expectedHint:   "incomplete_suite",
		},
		{
			name: "baseline security",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				for index := range runs {
					if runs[index].Arm == ArmBaseline {
						runs[index].SecurityIncidents++
						break
					}
				}
				return runs
			},
			expectedStatus: ConclusionInvalid,
			expectedHint:   "baseline_security",
		},
		{
			name: "baseline evidence integrity",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				for index := range runs {
					if runs[index].Arm == ArmBaseline {
						runs[index].DoctorReplayBroken = true
						break
					}
				}
				return runs
			},
			expectedStatus: ConclusionInvalid,
			expectedHint:   "baseline_evidence_integrity",
		},
		{
			name: "baseline tool pairing",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				for index := range runs {
					if runs[index].Arm == ArmBaseline {
						runs[index].ToolPairingBreaks++
						break
					}
				}
				return runs
			},
			expectedStatus: ConclusionInvalid,
			expectedHint:   "baseline_tool_pairing",
		},
		{
			name: "variant security fails instead of invalidating",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				for index := range runs {
					if runs[index].Arm == ArmVariant {
						runs[index].SecurityIncidents++
						break
					}
				}
				return runs
			},
			expectedStatus: ConclusionFailed,
			expectedHint:   "variant_security",
		},
		{
			name: "absolute cost gate violation",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				for index := range runs {
					if runs[index].Arm == ArmVariant {
						runs[index].InputTokens *= 10 // 整臂成本翻越百分之三十上限门
					}
				}
				return runs
			},
			expectedStatus: ConclusionFailed,
			expectedHint:   "absolute_cost_gate",
		},
		{
			name: "non degenerate exploration regression",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				for index := range runs {
					if runs[index].Arm == ArmVariant {
						runs[index].RepeatedExplorations = 5
						break
					}
				}
				return runs
			},
			expectedStatus: ConclusionFailed,
			expectedHint:   "non_degenerate_exploration",
		},
		{
			name: "success regression on any task fails outright",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				regressedTask := ""
				for index := range runs {
					if runs[index].Arm == ArmVariant && regressedTask == "" {
						regressedTask = runs[index].TaskID
					}
					if runs[index].Arm == ArmVariant && runs[index].TaskID == regressedTask {
						runs[index].Success = false // 整任务臂失败即低于基线，非退化门立即否决
					}
				}
				return runs
			},
			expectedStatus: ConclusionFailed,
			expectedHint:   "non_degenerate_success",
		},
		{
			name: "quality neutral without efficiency is inconclusive",
			mutate: func(runs []SuiteVerdict) []SuiteVerdict {
				for index := range runs {
					// 两臂成功率、成本与时延完全一致：无有效性改善也无效率节省，
					// 改善模式全未命中，按合同只能产出 inconclusive。
					runs[index].Success = true
					runs[index].InputTokens = 800
					runs[index].OutputTokens = 400
					runs[index].WallTimeMS = 950
				}
				return runs
			},
			expectedStatus: ConclusionInconclusive,
			expectedHint:   "",
		},
	}
	for _, item := range cases {
		evalSet, runs := suiteFixture(t)
		conclusion, err := JudgeSuite(evalSet, item.mutate(runs))
		if err != nil {
			t.Fatalf("%s judge error: %v", item.name, err)
		}
		if conclusion.Status != item.expectedStatus {
			t.Fatalf("%s status = %q, want %q (trigger %q)", item.name, conclusion.Status, item.expectedStatus, conclusion.Trigger)
		}
		if item.expectedHint != "" && !strings.Contains(conclusion.Trigger, item.expectedHint) {
			t.Fatalf("%s trigger = %q, want it to reference %q", item.name, conclusion.Trigger, item.expectedHint)
		}
	}
}

// TestCostUnitsIntegerFormulaWithoutDivision 锁定整数成本公式：tokens×千价
// 直接相乘不得先除一百万；单价为零的免费模型恒为零成本。
func TestCostUnitsIntegerFormulaWithoutDivision(t *testing.T) {
	pricing := contractPricingView{inputPrice: 140000, outputPrice: 280000}
	total, err := runCostUnits(SuiteVerdict{InputTokens: 3, OutputTokens: 2}, pricing)
	if err != nil {
		t.Fatalf("cost units: %v", err)
	}
	if total != 3*140000+2*280000 {
		t.Fatalf("cost units mismatch: %d", total)
	}
	freePricing := contractPricingView{}
	total, err = runCostUnits(SuiteVerdict{InputTokens: 999999, OutputTokens: 888888}, freePricing)
	if err != nil || total != 0 {
		t.Fatalf("zero pricing must yield zero cost units: %d %v", total, err)
	}
}

// TestMedianOddRejectsEvenSamples 验证中位数的奇数样本合同与取值位置。
func TestMedianOddRejectsEvenSamples(t *testing.T) {
	if _, err := medianOdd([]int64{1, 2}); err == nil {
		t.Fatal("even sample count must fail closed")
	}
	median, err := medianOdd([]int64{30, 10, 20})
	if err != nil || median != 20 {
		t.Fatalf("median = %d err = %v", median, err)
	}
}
