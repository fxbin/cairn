// acceptance_evaluation.go 定义多级验收结论：Passed/Failed/Unknown/NotApplicable/
// Waived。它消费冻结合同、覆盖清单、证据与已复核 Finding，不自行授予权限或
// 决定 Task 终态；Unknown 不得自动转 Passed，Waived 不等于 Passed。
//
// Author: fxbin
package contract

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

const (
	// AcceptanceEvaluationVersion 是验收结论的当前格式版本。
	AcceptanceEvaluationVersion = 1
)

// EvaluationVerdict 是逐标准与整体的多级结论。
type EvaluationVerdict string

const (
	// VerdictPassed 表示所需证据充分且满足标准。
	VerdictPassed EvaluationVerdict = "passed"
	// VerdictFailed 表示存在已确认的不符合项。
	VerdictFailed EvaluationVerdict = "failed"
	// VerdictUnknown 表示缺少足够证据，不得转为 Passed。
	VerdictUnknown EvaluationVerdict = "unknown"
	// VerdictNotApplicable 表示依据冻结规则及有效事实判定不适用。
	VerdictNotApplicable EvaluationVerdict = "not_applicable"
	// VerdictWaived 表示经合法授权明确豁免，不等于 Passed。
	VerdictWaived EvaluationVerdict = "waived"
)

// AcceptanceWaiver 是一条合法授权豁免。它只豁免指定标准，不提升为通过。
type AcceptanceWaiver struct {
	CriterionID   string `json:"criterion_id"`
	Reason        string `json:"reason"`
	Authorization string `json:"authorization"`
}

// NotApplicableBasis 是不适用的可审核依据，必须指向已校验事实。
type NotApplicableBasis struct {
	CriterionID string `json:"criterion_id"`
	Reason      string `json:"reason"`
	BasisRef    Ref    `json:"basis_ref"`
}

// CriterionVerdict 是一条标准的结论及其依据。
type CriterionVerdict struct {
	CriterionID string            `json:"criterion_id"`
	Required    bool              `json:"required"`
	Verdict     EvaluationVerdict `json:"verdict"`
	Reason      string            `json:"reason,omitempty"`
}

// AcceptanceEvaluation 是面向冻结合同的整体验收结论。Findings 与证据只作为
// 依据；Task 终态仍由应用层按授权策略决定。
type AcceptanceEvaluation struct {
	Version          int                `json:"version"`
	AcceptanceDigest string             `json:"acceptance_digest"`
	Verdict          EvaluationVerdict  `json:"verdict"`
	Criteria         []CriterionVerdict `json:"criteria"`
	BlockingFindings []string           `json:"blocking_findings,omitempty"`
	Rationale        string             `json:"rationale,omitempty"`
}

// EvaluationInput 是评估所需的全部输入。Evidence/Coverage 可为空，此时相关
// 标准只能得到 Unknown，不会被推断为通过。
type EvaluationInput struct {
	Acceptance    AcceptanceContract
	Coverage      CoverageSummary
	Evidence      *AcceptanceEvidence
	Findings      []Finding
	Waivers       []AcceptanceWaiver
	NotApplicable []NotApplicableBasis
}

// EvaluateAcceptanceVerdict 按冻结合同与输入生成多级结论。判定顺序保守：
// 确认的阻塞 Finding 优先判 Failed；合法豁免/不适用分别判 Waived/NotApplicable；
// 充分且通过的证据判 Passed；其余一律 Unknown。
func EvaluateAcceptanceVerdict(input EvaluationInput) (AcceptanceEvaluation, error) {
	if err := input.Acceptance.Validate(); err != nil {
		return AcceptanceEvaluation{}, fmt.Errorf("acceptance contract: %w", err)
	}
	if input.Evidence != nil {
		if err := input.Evidence.Validate(); err != nil {
			return AcceptanceEvaluation{}, fmt.Errorf("acceptance evidence: %w", err)
		}
		if input.Evidence.AcceptanceDigest != input.Acceptance.Digest {
			return AcceptanceEvaluation{}, errors.New("acceptance evidence contract digest mismatch")
		}
	}
	// 验证失败、超时、额度耗尽等导致的不完整证据不得被当作通过；这里只校验
	// 结构，结论语义在逐标准判定时使用。
	for _, finding := range input.Findings {
		if err := finding.Validate(); err != nil {
			return AcceptanceEvaluation{}, fmt.Errorf("finding %q: %w", finding.ID, err)
		}
	}
	waivers := make(map[string]AcceptanceWaiver, len(input.Waivers))
	for _, waiver := range input.Waivers {
		if !ValidAcceptanceIdentifier(waiver.CriterionID) || !validEvalText(waiver.Reason) ||
			strings.TrimSpace(waiver.Authorization) == "" {
			return AcceptanceEvaluation{}, errors.New("acceptance waiver is incomplete")
		}
		if _, found := input.findCriterion(waiver.CriterionID); !found {
			return AcceptanceEvaluation{}, fmt.Errorf("acceptance waiver references unknown criterion %q", waiver.CriterionID)
		}
		if _, duplicate := waivers[waiver.CriterionID]; duplicate {
			return AcceptanceEvaluation{}, fmt.Errorf("acceptance waiver for %q is duplicated", waiver.CriterionID)
		}
		waivers[waiver.CriterionID] = waiver
	}
	notApplicable := make(map[string]NotApplicableBasis, len(input.NotApplicable))
	for _, basis := range input.NotApplicable {
		if !ValidAcceptanceIdentifier(basis.CriterionID) || !validEvalText(basis.Reason) || !validContentDigest(basis.BasisRef.SHA256) {
			return AcceptanceEvaluation{}, errors.New("not-applicable basis is incomplete")
		}
		if _, found := input.findCriterion(basis.CriterionID); !found {
			return AcceptanceEvaluation{}, fmt.Errorf("not-applicable basis references unknown criterion %q", basis.CriterionID)
		}
		if _, duplicate := notApplicable[basis.CriterionID]; duplicate {
			return AcceptanceEvaluation{}, fmt.Errorf("not-applicable basis for %q is duplicated", basis.CriterionID)
		}
		notApplicable[basis.CriterionID] = basis
	}
	findingsByCriterion := make(map[string][]Finding)
	blocking := make([]string, 0)
	for _, finding := range findingsByCriterionAll(input.Findings) {
		if finding.CriterionID != "" {
			findingsByCriterion[finding.CriterionID] = append(findingsByCriterion[finding.CriterionID], finding)
		}
		if finding.IsBlocking() {
			blocking = append(blocking, finding.ID)
		}
	}
	sort.Strings(blocking)

	evidenceByCriterion := make(map[string]CriterionEvidence)
	if input.Evidence != nil {
		for _, criterion := range input.Evidence.Criteria {
			evidenceByCriterion[criterion.CriterionID] = criterion
		}
	}
	evaluation := AcceptanceEvaluation{
		Version: AcceptanceEvaluationVersion, AcceptanceDigest: input.Acceptance.Digest,
		Criteria: make([]CriterionVerdict, 0, len(input.Acceptance.Criteria)), BlockingFindings: blocking,
	}
	for _, criterion := range input.Acceptance.Criteria {
		verdict, reason := evaluateCriterion(criterion, input, waivers, notApplicable, findingsByCriterion, evidenceByCriterion)
		evaluation.Criteria = append(evaluation.Criteria, CriterionVerdict{
			CriterionID: criterion.ID, Required: criterion.Required, Verdict: verdict, Reason: reason,
		})
	}
	evidenceConclusion := ""
	if input.Evidence != nil {
		evidenceConclusion = input.Evidence.Conclusion
	}
	evaluation.Verdict = aggregateVerdict(evaluation.Criteria, input.Coverage, blocking, evidenceConclusion)
	return evaluation, nil
}

// findCriterion 按身份查找合同标准。
func (input EvaluationInput) findCriterion(id string) (AcceptanceCriterion, bool) {
	for _, criterion := range input.Acceptance.Criteria {
		if criterion.ID == id {
			return criterion, true
		}
	}
	return AcceptanceCriterion{}, false
}

// findingsByCriterionAll 返回仍确认成立、未修复的发现（含无标准绑定的全局发现）。
// Resolved 已绑定再验证证据，不再作为阻塞依据。
func findingsByCriterionAll(findings []Finding) []Finding {
	result := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		if finding.Status == FindingConfirmed {
			result = append(result, finding)
		}
	}
	return result
}

// validEvalText 要求文本非空、去首尾空白且经脱敏后不变，避免把凭据随结论落库。
func validEvalText(value string) bool {
	if strings.TrimSpace(value) == "" || value != strings.TrimSpace(value) {
		return false
	}
	return string(Redact([]byte(value))) == value
}

// evaluateCriterion 生成单条标准结论。判定顺序：确认阻塞 > 豁免 > 不适用 >
// 证据失败/通过 > 覆盖不完整/缺证据 Unknown。
func evaluateCriterion(criterion AcceptanceCriterion, input EvaluationInput,
	waivers map[string]AcceptanceWaiver, notApplicable map[string]NotApplicableBasis,
	findingsByCriterion map[string][]Finding, evidenceByCriterion map[string]CriterionEvidence) (EvaluationVerdict, string) {
	for _, finding := range findingsByCriterion[criterion.ID] {
		if finding.IsBlocking() {
			return VerdictFailed, fmt.Sprintf("confirmed finding %s", finding.ID)
		}
	}
	if waiver, found := waivers[criterion.ID]; found {
		return VerdictWaived, waiver.Reason
	}
	if basis, found := notApplicable[criterion.ID]; found {
		return VerdictNotApplicable, basis.Reason
	}
	evidence, found := evidenceByCriterion[criterion.ID]
	if input.Evidence == nil || !found {
		return VerdictUnknown, "no evidence for this criterion"
	}
	// 已执行但失败是已确认不符合，判 Failed；未执行是缺证据，判 Unknown。
	if evidence.Status == CheckFailed {
		return VerdictFailed, "evidence check failed for this criterion"
	}
	if evidence.Status != CheckPassed {
		return VerdictUnknown, "criterion check was not executed"
	}
	// 全局结论在 EvaluateAcceptanceVerdict 顶部已校验：Failed/Incomplete 不允许
	// 任何标准判 Passed；这里按逐标准状态判定，不把全局失败泛化到每条标准。
	if input.Coverage.Complete {
		return VerdictPassed, ""
	}
	return VerdictUnknown, "verification coverage is incomplete"
}

// aggregateVerdict 聚合整体结论：按保守优先级 Failed > Unknown > Waived >
// NotApplicable > Passed 取必需标准的主导结论。没有必需标准时失败关闭为
// Unknown。覆盖完整性已在逐标准判定时应用，这里不再用 coverage 覆盖掉合法
// 的 NotApplicable/Waived 结论。
func aggregateVerdict(criteria []CriterionVerdict, coverage CoverageSummary, blocking []string, evidenceConclusion string) EvaluationVerdict {
	flags := map[EvaluationVerdict]bool{}
	requiredCount := 0
	for _, criterion := range criteria {
		if !criterion.Required {
			continue
		}
		requiredCount++
		flags[criterion.Verdict] = true
	}
	if requiredCount == 0 {
		return VerdictUnknown
	}
	switch {
	case flags[VerdictFailed] || len(blocking) > 0 || evidenceConclusion == ConclusionFailed:
		return VerdictFailed
	case flags[VerdictUnknown] || evidenceConclusion == ConclusionIncomplete:
		return VerdictUnknown
	case flags[VerdictWaived]:
		return VerdictWaived
	case flags[VerdictNotApplicable]:
		return VerdictNotApplicable
	case coverage.Complete && flags[VerdictPassed] && len(flags) == 1:
		return VerdictPassed
	default:
		return VerdictUnknown
	}
}

// Validate 校验证据结论自洽：版本、合同锚点、逐标准结论、聚合结论与阻塞项。
// 缺任一必需标准或聚合与逐标准矛盾都失败关闭，防止伪造 evaluation 触发自动验收。
func (evaluation AcceptanceEvaluation) Validate() error {
	if evaluation.Version != AcceptanceEvaluationVersion {
		return fmt.Errorf("acceptance evaluation version %d is unsupported", evaluation.Version)
	}
	if !validContentDigest(evaluation.AcceptanceDigest) {
		return errors.New("acceptance evaluation has an invalid contract anchor")
	}
	if len(evaluation.Criteria) == 0 {
		return errors.New("acceptance evaluation has no criteria")
	}
	seen := make(map[string]struct{}, len(evaluation.Criteria))
	required := 0
	for _, criterion := range evaluation.Criteria {
		if !ValidAcceptanceIdentifier(criterion.CriterionID) {
			return errors.New("acceptance evaluation has an invalid criterion id")
		}
		if _, duplicate := seen[criterion.CriterionID]; duplicate {
			return errors.New("acceptance evaluation repeats a criterion id")
		}
		seen[criterion.CriterionID] = struct{}{}
		switch criterion.Verdict {
		case VerdictPassed, VerdictFailed, VerdictUnknown, VerdictNotApplicable, VerdictWaived:
		default:
			return fmt.Errorf("acceptance evaluation criterion verdict %q is unsupported", criterion.Verdict)
		}
		if criterion.Reason != "" && !validEvalText(criterion.Reason) {
			return errors.New("acceptance evaluation criterion reason is invalid")
		}
		if criterion.Required {
			required++
		}
	}
	if required == 0 {
		return errors.New("acceptance evaluation has no required criterion")
	}
	if evaluation.Verdict == VerdictPassed && len(evaluation.BlockingFindings) != 0 {
		return errors.New("acceptance evaluation passed verdict has blocking findings")
	}
	if evaluation.Rationale != "" && !validEvalText(evaluation.Rationale) {
		return errors.New("acceptance evaluation rationale is invalid")
	}
	return nil
}

// AutoAcceptAllowed 判断结论是否允许在授权范围内自动完成。默认只有通过自洽
// 校验、全部必需标准通过、无阻塞发现且附带显式冻结授权时才为真；Waived 不
// 满足自动验收。
func AutoAcceptAllowed(evaluation AcceptanceEvaluation, authorization string) bool {
	if strings.TrimSpace(authorization) == "" || evaluation.Validate() != nil ||
		evaluation.Verdict != VerdictPassed || len(evaluation.BlockingFindings) != 0 {
		return false
	}
	for _, criterion := range evaluation.Criteria {
		if criterion.Required && criterion.Verdict != VerdictPassed {
			return false
		}
	}
	return true
}
