// acceptance_evidence.go 定义通用验收证据：主体、生产者、来源与逐标准结论。
// 证据只保存状态与身份，不携带命令输出、凭据或路径；它引用 Journal 事件，
// 不替代 Journal 成为运行事实源。
//
// Author: fxbin
package contract

import (
	"errors"
	"fmt"
)

const (
	// AcceptanceEvidenceVersion 是验收证据的当前格式版本。
	AcceptanceEvidenceVersion = 1
	// CheckPassed 表示检查已执行且成功。
	CheckPassed = "passed"
	// CheckFailed 表示检查已执行但失败。
	CheckFailed = "failed"
	// CheckNotExecuted 表示检查未纳入本次验证执行。
	CheckNotExecuted = "not_executed"
	// ConclusionPassed 表示必需标准全部满足且覆盖完整。
	ConclusionPassed = "passed"
	// ConclusionFailed 表示至少一条必需标准失败。
	ConclusionFailed = "failed"
	// ConclusionIncomplete 表示覆盖不完整，没有足够证据支持通过。
	ConclusionIncomplete = "incomplete"
	// EvidenceKindWorkspace 是工作区主体的标识类型。
	EvidenceKindWorkspace = "workspace"
	// EvidenceProducerEngine 是验证引擎生产者的标识类型。
	EvidenceProducerEngine = "verification_engine"
	// EvidenceProducerID 是 Cairn 验证引擎的稳定身份。
	EvidenceProducerID = "cairn"
)

// EvidenceSubject 绑定证据校验的工作区及其版本。ID 是工作区哈希，Version
// 是验证开始时的不透明工作区观测摘要；两者变化都会使证据失效。
type EvidenceSubject struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"`
	Version string `json:"version"`
}

// EvidenceProducer 标识证据的生产者。人工证据在后续版本使用不同 Kind，
// 当前版本只接受验证引擎。
type EvidenceProducer struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// EvidenceSource 把证据绑定回 Journal 中一次具体的验证及其冻结计划。
type EvidenceSource struct {
	VerificationID      string `json:"verification_id"`
	PlanRef             *Ref   `json:"plan_ref,omitempty"`
	CommandsFingerprint string `json:"commands_fingerprint"`
}

// CheckEvidence 是单条检查的终态。
type CheckEvidence struct {
	CheckID string `json:"check_id"`
	Status  string `json:"status"`
}

// CriterionEvidence 是一条验收标准及其检查的终态投影。
type CriterionEvidence struct {
	CriterionID string          `json:"criterion_id"`
	Required    bool            `json:"required"`
	Status      string          `json:"status"`
	Checks      []CheckEvidence `json:"checks"`
}

// AcceptanceCoverage 把覆盖度与结论分离：覆盖不完整时 Complete 为假，
// 即便已执行的检查全部通过也不得宣布 Passed。
type AcceptanceCoverage struct {
	Required int  `json:"required"`
	Covered  int  `json:"covered"`
	Passed   int  `json:"passed"`
	Complete bool `json:"complete"`
}

// AcceptanceEvidence 是一次验证针对某份冻结验收合同生成的可持久证据。
type AcceptanceEvidence struct {
	Version          int                 `json:"version"`
	RunID            string              `json:"run_id"`
	AcceptanceDigest string              `json:"acceptance_digest"`
	Subject          EvidenceSubject     `json:"subject"`
	Producer         EvidenceProducer    `json:"producer"`
	Source           EvidenceSource      `json:"source"`
	Criteria         []CriterionEvidence `json:"criteria"`
	Coverage         AcceptanceCoverage  `json:"coverage"`
	Conclusion       string              `json:"conclusion"`
}

// AcceptanceCheckResult 描述一条检查在本次验证中的执行结果。Core 把冻结命令
// 的成功终态映射为该结构；合同层不关心命令如何执行。
type AcceptanceCheckResult struct {
	CheckID  string
	Executed bool
	Passed   bool
}

// EvaluateAcceptance 按合同与检查结果生成逐标准证据、覆盖度和结论。同一个
// 检查可服务多条标准；未执行的检查一律记为 not_executed，不按通过处理。
func EvaluateAcceptance(acceptance AcceptanceContract, results []AcceptanceCheckResult) ([]CriterionEvidence, AcceptanceCoverage, string) {
	byCheck := make(map[string]AcceptanceCheckResult, len(results))
	for _, result := range results {
		byCheck[result.CheckID] = result
	}
	criteria := make([]CriterionEvidence, 0, len(acceptance.Criteria))
	coverage := AcceptanceCoverage{}
	for _, criterion := range acceptance.Criteria {
		entry := CriterionEvidence{CriterionID: criterion.ID, Required: criterion.Required, Checks: make([]CheckEvidence, 0, len(criterion.Checks))}
		failed, missing := false, false
		for _, checkID := range criterion.Checks {
			result, found := byCheck[checkID]
			status := CheckNotExecuted
			switch {
			case !found || !result.Executed:
				missing = true
			case result.Passed:
				status = CheckPassed
			default:
				status = CheckFailed
				failed = true
			}
			entry.Checks = append(entry.Checks, CheckEvidence{CheckID: checkID, Status: status})
		}
		switch {
		case failed:
			entry.Status = CheckFailed
		case missing:
			entry.Status = CheckNotExecuted
		default:
			entry.Status = CheckPassed
		}
		if criterion.Required {
			coverage.Required++
			if entry.Status != CheckNotExecuted {
				coverage.Covered++
			}
			if entry.Status == CheckPassed {
				coverage.Passed++
			}
		}
		criteria = append(criteria, entry)
	}
	coverage.Complete = coverage.Required > 0 && coverage.Covered == coverage.Required
	conclusion := ConclusionIncomplete
	switch {
	case anyRequiredFailed(criteria):
		conclusion = ConclusionFailed
	case coverage.Complete && coverage.Passed == coverage.Required:
		conclusion = ConclusionPassed
	}
	return criteria, coverage, conclusion
}

// anyRequiredFailed 判断是否存在失败的必需标准。
func anyRequiredFailed(criteria []CriterionEvidence) bool {
	for _, criterion := range criteria {
		if criterion.Required && criterion.Status == CheckFailed {
			return true
		}
	}
	return false
}

// Validate 校验证据自身的版本、身份、计数与结论自洽，不依赖外部合同。
func (evidence AcceptanceEvidence) Validate() error {
	if evidence.Version != AcceptanceEvidenceVersion {
		return fmt.Errorf("acceptance evidence version %d is unsupported", evidence.Version)
	}
	if evidence.RunID == "" || evidence.AcceptanceDigest == "" {
		return errors.New("acceptance evidence has no run or contract anchor")
	}
	if evidence.Subject.Kind != EvidenceKindWorkspace || evidence.Subject.ID == "" || evidence.Subject.Version == "" {
		return errors.New("acceptance evidence subject is incomplete")
	}
	if evidence.Producer.Kind != EvidenceProducerEngine || evidence.Producer.ID != EvidenceProducerID {
		return errors.New("acceptance evidence producer is unsupported")
	}
	if evidence.Source.VerificationID == "" || evidence.Source.CommandsFingerprint == "" {
		return errors.New("acceptance evidence source is incomplete")
	}
	seen := make(map[string]struct{}, len(evidence.Criteria))
	required, covered, passed := 0, 0, 0
	for _, criterion := range evidence.Criteria {
		if !ValidAcceptanceIdentifier(criterion.CriterionID) {
			return errors.New("acceptance evidence has an invalid criterion id")
		}
		if _, duplicate := seen[criterion.CriterionID]; duplicate {
			return errors.New("acceptance evidence repeats a criterion id")
		}
		seen[criterion.CriterionID] = struct{}{}
		switch criterion.Status {
		case CheckPassed, CheckFailed, CheckNotExecuted:
		default:
			return errors.New("acceptance evidence has an invalid criterion status")
		}
		checks := make(map[string]struct{}, len(criterion.Checks))
		for _, check := range criterion.Checks {
			if !ValidAcceptanceIdentifier(check.CheckID) {
				return errors.New("acceptance evidence has an invalid check id")
			}
			if _, duplicate := checks[check.CheckID]; duplicate {
				return errors.New("acceptance evidence repeats a check id")
			}
			checks[check.CheckID] = struct{}{}
			switch check.Status {
			case CheckPassed, CheckFailed, CheckNotExecuted:
			default:
				return errors.New("acceptance evidence has an invalid check status")
			}
		}
		if criterion.Required {
			required++
			if criterion.Status != CheckNotExecuted {
				covered++
			}
			if criterion.Status == CheckPassed {
				passed++
			}
		}
	}
	if evidence.Coverage.Required != required || evidence.Coverage.Covered != covered || evidence.Coverage.Passed != passed {
		return errors.New("acceptance evidence coverage does not match its criteria")
	}
	complete := required > 0 && covered == required
	if evidence.Coverage.Complete != complete {
		return errors.New("acceptance evidence coverage flag is inconsistent")
	}
	conclusion := ConclusionIncomplete
	switch {
	case anyRequiredFailed(evidence.Criteria):
		conclusion = ConclusionFailed
	case complete && passed == required:
		conclusion = ConclusionPassed
	}
	if evidence.Conclusion != conclusion {
		return errors.New("acceptance evidence conclusion is inconsistent")
	}
	return nil
}

// Satisfies 判断证据是否是对给定合同与工作区主体的充分验收依据。摘要、标准
// 结构、覆盖度、结论和主体版本任一不符都失败关闭。
func (evidence AcceptanceEvidence) Satisfies(acceptance AcceptanceContract, workspaceHash, workspaceVersion string) error {
	if err := acceptance.Validate(); err != nil {
		return err
	}
	if err := evidence.Validate(); err != nil {
		return err
	}
	if evidence.AcceptanceDigest != acceptance.Digest {
		return errors.New("acceptance evidence contract digest mismatch")
	}
	if evidence.Subject.ID != workspaceHash || evidence.Subject.Version != workspaceVersion {
		return errors.New("acceptance evidence subject version is stale")
	}
	if len(evidence.Criteria) != len(acceptance.Criteria) {
		return errors.New("acceptance evidence criteria do not match the contract")
	}
	expected := make(map[string]AcceptanceCriterion, len(acceptance.Criteria))
	for _, criterion := range acceptance.Criteria {
		expected[criterion.ID] = criterion
	}
	for _, entry := range evidence.Criteria {
		criterion, found := expected[entry.CriterionID]
		if !found || criterion.Required != entry.Required || len(criterion.Checks) != len(entry.Checks) {
			return errors.New("acceptance evidence criterion does not match the contract")
		}
		wanted := make(map[string]struct{}, len(criterion.Checks))
		for _, check := range criterion.Checks {
			wanted[check] = struct{}{}
		}
		for _, check := range entry.Checks {
			if _, found := wanted[check.CheckID]; !found {
				return errors.New("acceptance evidence check does not belong to its criterion")
			}
			delete(wanted, check.CheckID)
		}
		if len(wanted) != 0 {
			return errors.New("acceptance evidence check set is incomplete")
		}
	}
	if !evidence.Coverage.Complete || evidence.Conclusion != ConclusionPassed {
		return fmt.Errorf("acceptance evidence does not satisfy the contract: %s", evidence.Conclusion)
	}
	return nil
}
