package benchmark

import (
	"fmt"
	"sort"
	"time"

	"github.com/fxbin/cairn/internal/canonicaljson"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

// variantConfigPrefix 是"展开后变体配置指纹"的版本化域前缀：视图由基线配置
// 指纹、应用后的上下文两值与完整四参规格摘要共同构成，保证任何参数漂移都
// 会改变冻结身份。
const variantConfigPrefix = "cairn-sandbox-variant-config-v1"

// ExpandCompactionVariant 把压缩参数变体的可生效部分应用到基线配置副本上，
// 返回可直接用于执行的变体臂配置。当前只有 max_prompt_tokens 与
// max_summary_bytes 存在运行时消费路径；其余两个百分比参数只随规格摘要进
// 入身份，接线属独立评审切片。
func ExpandCompactionVariant(base config.Config, variant memory.CompactionParamsVariant) (config.Config, error) {
	if err := variant.Validate(); err != nil {
		return config.Config{}, fmt.Errorf("expand compaction variant: %w", err)
	}
	expanded := base
	expanded.Context.MaxPromptTokens = variant.MaxPromptTokens
	expanded.Context.MaxSummaryBytes = variant.MaxSummaryBytes
	if err := config.Validate(expanded); err != nil {
		return config.Config{}, fmt.Errorf("expanded variant config invalid: %w", err)
	}
	return expanded, nil
}

// VariantExpandedFingerprint 以闭合视图计算展开后变体配置的身份：只取基线
// 配置指纹、生效覆盖值与规格全文摘要三要素，避免整份配置重复进入比较器视
// 野造成无关漂移。
func VariantExpandedFingerprint(baselineConfigFingerprint string, variant memory.CompactionParamsVariant) (string, error) {
	specDigest, err := memory.VariantSpecDigest(variant)
	if err != nil {
		return "", err
	}
	view := struct {
		BaselineConfigFingerprint string                         `json:"baseline_config_fingerprint"`
		AppliedContext            map[string]int                 `json:"applied_context"`
		VariantSpec               memory.CompactionParamsVariant `json:"variant_spec"`
		SpecDigest                string                         `json:"spec_digest"`
	}{
		BaselineConfigFingerprint: baselineConfigFingerprint,
		AppliedContext: map[string]int{
			"max_prompt_tokens": variant.MaxPromptTokens,
			"max_summary_bytes": variant.MaxSummaryBytes,
		},
		VariantSpec: variant,
		SpecDigest:  specDigest,
	}
	return canonicaljson.Fingerprint(variantConfigPrefix, view)
}

// offlinePrescreenReport 是离线预筛的闭合结论载体：逐项列出命中的失败面，
// 通过项不产生条目。
type offlinePrescreenReport struct {
	Issues []string
}

// Passed 报告预筛是否存在任何失败面。
func (report offlinePrescreenReport) Passed() bool { return len(report.Issues) == 0 }

// RunOfflinePrescreen 执行在线评测前的离线预筛：核对展开后指纹与冻结一致、
// 两臂配置仍满足全部合同校验，并对可选提供的压缩产物样本断言尺寸上限与清
// 单锚标记保留。样本缺失不算失败——首轮可能尚无历史压缩事件。
func RunOfflinePrescreen(evalSet sandboxeval.EvalSet, baselineConfig, variantConfig config.Config, variant memory.CompactionParamsVariant, compactedSummaries [][]byte) (offlinePrescreenReport, error) {
	report := offlinePrescreenReport{}
	addIssue := func(format string, args ...any) {
		report.Issues = append(report.Issues, fmt.Sprintf(format, args...))
	}
	baselineFingerprint := baselineConfig.Fingerprint()
	variantConfigFingerprint, err := VariantExpandedFingerprint(baselineFingerprint, variant)
	if err != nil {
		return report, err
	}
	if variantConfigFingerprint != evalSet.VariantConfigFingerprint {
		addIssue("expanded variant fingerprint %q diverges from frozen %q", variantConfigFingerprint, evalSet.VariantConfigFingerprint)
	}
	if baselineFingerprint != evalSet.BaselineConfigFingerprint {
		addIssue("baseline config fingerprint %q diverges from frozen %q", baselineFingerprint, evalSet.BaselineConfigFingerprint)
	}
	for index, summary := range compactedSummaries {
		if len(summary) > variant.MaxSummaryBytes {
			addIssue("compacted summary #%d exceeds the variant budget %d", index, variant.MaxSummaryBytes)
		}
		if !stringsContains(string(summary), explorationInventoryAnchor) {
			addIssue("compacted summary #%d lost the exploration inventory anchor", index)
		}
	}
	return report, nil
}

// stringsContains 是纯函数域内的有限子串判断，避免为单一用途引入标准库之外的依赖分叉。
func stringsContains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

// indexOf 返回子串位置或 -1。
func indexOf(haystack, needle string) int {
	for start := 0; start+len(needle) <= len(haystack); start++ {
		if haystack[start:start+len(needle)] == needle {
			return start
		}
	}
	return -1
}

// explorationInventoryAnchor 是压缩摘要必须保留的探索清单锚标记；它由 Agent
// 侧压缩器写入，丢失即视为清单证据被破坏。
// explorationInventoryAnchor 是压缩摘要必须保留的探索清单锚片段；它与 Agent
// 侧检查点前缀的清单字句保持同源，丢失即视为清单证据被破坏。
const explorationInventoryAnchor = "已检查对象"

// SandboxScheduleVersion 是成对交错调度的算法世代标识：EvalSet 冻结与执行器
// 双侧引用同一常量，调度语义漂移必然改变冻结身份。
const SandboxScheduleVersion = "pair-alternating-v1"

// SandboxPairStep 是成对交错调度中的单步：任务升序排列，pair 相邻，奇数次
// 尝试先跑基线再跑变体，偶数次尝试反向。
type SandboxPairStep struct {
	TaskID string
	Arm    sandboxeval.SuiteArm
	Try    int
}

// SandboxSchedule 以排序后的任务清单生成全套执行步。
func SandboxSchedule(taskIDs []string, attemptsPerTask int) ([]SandboxPairStep, error) {
	sortedIDs := append([]string(nil), taskIDs...)
	sort.Strings(sortedIDs)
	steps := make([]SandboxPairStep, 0, len(sortedIDs)*attemptsPerTask*2)
	for _, taskID := range sortedIDs {
		for attempt := 1; attempt <= attemptsPerTask; attempt++ {
			first, second := sandboxeval.ArmBaseline, sandboxeval.ArmVariant
			if attempt%2 == 0 {
				first, second = sandboxeval.ArmVariant, sandboxeval.ArmBaseline
			}
			steps = append(steps, SandboxPairStep{TaskID: taskID, Arm: first, Try: attempt}, SandboxPairStep{TaskID: taskID, Arm: second, Try: attempt})
		}
	}
	if len(steps) != len(sortedIDs)*attemptsPerTask*2 {
		return nil, errorsNewScheduleMismatch()
	}
	return steps, nil
}

// errorsNewScheduleMismatch 返回调度规模对不上任务数的关闭错误。
func errorsNewScheduleMismatch() error {
	return fmt.Errorf("sandbox schedule size does not cover the frozen task set")
}

// sandboxDeadline 计算套件最坏截止时刻。
func sandboxDeadline(start time.Time, wallMilliseconds int64) time.Time {
	return start.Add(time.Duration(wallMilliseconds) * time.Millisecond)
}
