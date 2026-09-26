package cli

import (
	"errors"
	"fmt"
	"sort"

	"github.com/fxbin/cairn/internal/benchmark"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// extractSuiteMetricSummary 从字节摘要校验过的套件结果正文提取闭合指标比
// 较摘要：两臂成功数、总输入输出 token 与中位墙钟（每臂取排序中位）。
func extractSuiteMetricSummary(records []benchmark.SandboxRunRecord, attemptsPerTask int) (memory.PromotionMetricSummary, error) {
	if len(records) == 0 {
		return memory.PromotionMetricSummary{}, errors.New("suite result carries no run records")
	}
	baselineWalls, variantWalls := make([]int64, 0, attemptsPerTask), make([]int64, 0, attemptsPerTask)
	summary := memory.PromotionMetricSummary{}
	for _, record := range records {
		if record.WallTimeMS < 0 || record.InputTokens < 0 || record.OutputTokens < 0 {
			return memory.PromotionMetricSummary{}, errors.New("run record carries negative metrics")
		}
		if record.Arm == string(sandboxeval.ArmBaseline) {
			summary.BaselineSuccesses += boolToInt(record.Success)
			summary.BaselineInputTokens += record.InputTokens
			summary.BaselineOutputTokens += record.OutputTokens
			baselineWalls = append(baselineWalls, record.WallTimeMS)
		} else if record.Arm == string(sandboxeval.ArmVariant) {
			summary.VariantSuccesses += boolToInt(record.Success)
			summary.VariantInputTokens += record.InputTokens
			summary.VariantOutputTokens += record.OutputTokens
			variantWalls = append(variantWalls, record.WallTimeMS)
		} else {
			return memory.PromotionMetricSummary{}, fmt.Errorf("run record carries unknown arm %q", record.Arm)
		}
	}
	if len(baselineWalls) != attemptsPerTask*len(uniqueTaskIDs(records)) || len(variantWalls) != attemptsPerTask*len(uniqueTaskIDs(records)) {
		return memory.PromotionMetricSummary{}, errors.New("suite result does not cover both arms completely")
	}
	summary.BaselineMedianWallMS = medianInt64(baselineWalls)
	summary.VariantMedianWallMS = medianInt64(variantWalls)
	return summary, nil
}

// uniqueTaskIDs 返回记录中出现的去重任务清单。
func uniqueTaskIDs(records []benchmark.SandboxRunRecord) []string {
	seen := map[string]struct{}{}
	ids := make([]string, 0)
	for _, record := range records {
		if _, ok := seen[record.TaskID]; !ok {
			seen[record.TaskID] = struct{}{}
			ids = append(ids, record.TaskID)
		}
	}
	return ids
}

// medianInt64 返回排序中位；奇偶样本统一取下中位，账本只做留档不做判定。
func medianInt64(values []int64) int64 {
	sorted := append([]int64(nil), values...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if len(sorted) == 0 {
		return 0
	}
	return sorted[(len(sorted)-1)/2]
}

// boolToInt 把布尔折叠为整数计数。
func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
