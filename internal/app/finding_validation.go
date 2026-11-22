// [INPUT]: 依赖 Verifier 产出的待核验 Finding 与证据合同。
// [OUTPUT]: 经程序核验与保守规则推进的 Finding，确认的阻塞项驱动返工。
// [POS]: 复核依据可追溯；验证失败/超时不得解释为没有问题或静默删除。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件实现结构化 Finding 的程序核验与状态推进。
// Author: fxbin
package app

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/fxbin/cairn/internal/contract"
)

// EvidenceVerifier 由确定性核验器提供。返回 nil 表示引用包含支持待核验
// 发现的证据。制品库可读只证明引用完整，不能单独充当核验器。
type EvidenceVerifier func(ref contract.Ref) error

// FindingValidation 是一次复核决定。原始发现与证据始终保留，没有被删除。
type FindingValidation struct {
	Finding   contract.Finding
	Validator string
	Rationale string
}

// ValidateFinding 对一条 Proposed Finding 做程序核验并推进状态。
// 规则保守：
//   - 结构非法 → Inconclusive（不可当作没问题）；
//   - 证据引用必须在真实制品库通过摘要校验；格式合法但制品不存在不计入；
//   - 任何严重度的发现都需要有效证据，定位只用于寻找证据；
//   - 确认复核只引用已核验的证据。
func ValidateFinding(finding contract.Finding, validator string, verifyEvidence EvidenceVerifier) (FindingValidation, error) {
	if finding.Status != contract.FindingProposed {
		return FindingValidation{}, errors.New("only proposed findings can be validated")
	}
	if strings.TrimSpace(validator) == "" {
		return FindingValidation{}, errors.New("finding validation requires a validator identity")
	}
	// 结构非法时保留原始发现，记为 Inconclusive，而不是丢弃。
	if err := finding.Validate(); err != nil {
		finding.Status = contract.FindingInconclusive
		finding.Reviews = append(finding.Reviews, contract.FindingReview{
			ReviewerID: validator, Outcome: contract.FindingInconclusive,
			Rationale: "finding failed structural validation: " + err.Error(),
		})
		return FindingValidation{Finding: finding, Validator: validator, Rationale: "structural validation failed"}, nil
	}
	// 证据必须在真实制品库校验通过；模型凭空编造的摘要不计为有效证据。
	verified := verifiedFindingEvidence(finding.EvidenceRefs, verifyEvidence)
	if len(verified) == 0 {
		finding.Status = contract.FindingInconclusive
		finding.Reviews = append(finding.Reviews, contract.FindingReview{
			ReviewerID: validator, Outcome: contract.FindingInconclusive,
			Rationale: "insufficient verifiable evidence to confirm or reject",
		})
		return FindingValidation{Finding: finding, Validator: validator, Rationale: "insufficient evidence"}, nil
	}
	finding.Status = contract.FindingConfirmed
	finding.Reviews = append(finding.Reviews, contract.FindingReview{
		ReviewerID: validator, Outcome: contract.FindingConfirmed,
		Rationale:    fmt.Sprintf("confirmed with %d verified evidence references", len(verified)),
		EvidenceRefs: verified,
	})
	return FindingValidation{Finding: finding, Validator: validator, Rationale: "confirmed with verified evidence"}, nil
}

// verifiedFindingEvidence 返回通过校验的不同证据。原始引用仍保留在发现中。
func verifiedFindingEvidence(refs []contract.Ref, verify EvidenceVerifier) []contract.Ref {
	if verify == nil {
		return nil
	}
	var verified []contract.Ref
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if !seen[ref.SHA256] && verify(ref) == nil {
			seen[ref.SHA256] = true
			ref.TruncatedPreview = ""
			verified = append(verified, ref)
		}
	}
	return verified
}

// RejectFinding 记录一条有依据的拒绝复核。高风险发现必须提供理由或证据，
// 否则失败关闭，防止静默删除。原始发现与所有复核记录保留。
func RejectFinding(finding contract.Finding, validator, rationale string, evidence []contract.Ref) (FindingValidation, error) {
	if finding.Status != contract.FindingProposed {
		return FindingValidation{}, errors.New("only proposed findings can be rejected")
	}
	highRisk := finding.Severity == contract.SeverityHigh || finding.Severity == contract.SeverityCritical
	if highRisk && strings.TrimSpace(rationale) == "" && len(evidence) == 0 {
		return FindingValidation{}, errors.New("rejecting a high-severity finding requires rationale or evidence")
	}
	if strings.TrimSpace(rationale) == "" && len(evidence) == 0 {
		return FindingValidation{}, errors.New("rejection requires rationale or evidence")
	}
	finding.Status = contract.FindingRejected
	finding.Reviews = append(finding.Reviews, contract.FindingReview{
		ReviewerID: validator, Outcome: contract.FindingRejected,
		Rationale: rationale, EvidenceRefs: append([]contract.Ref(nil), evidence...),
	})
	if err := finding.Validate(); err != nil {
		return FindingValidation{}, fmt.Errorf("rejected finding is invalid: %w", err)
	}
	return FindingValidation{Finding: finding, Validator: validator, Rationale: rationale}, nil
}

// DeduplicateAndValidate 对一组 Proposed Finding 去重（同 ID 合并证据），逐条
// 核验并返回稳定的确认/未决/拒绝清单。重复项的证据引用并入保留项，不静默丢失。
func DeduplicateAndValidate(findings []contract.Finding, validator string, verifyEvidence EvidenceVerifier) (confirmed, inconclusive, rejected []contract.Finding, err error) {
	merged := make(map[string]*contract.Finding, len(findings))
	order := make([]string, 0, len(findings))
	for _, finding := range findings {
		if err := finding.Validate(); err != nil {
			return nil, nil, nil, fmt.Errorf("finding %q: %w", finding.ID, err)
		}
		// 核验前统一为 Proposed，清除不可信的自报状态与复核。
		finding.Status = contract.FindingProposed
		finding.Reviews = nil
		current, found := merged[finding.ID]
		if !found {
			cloned := finding
			merged[finding.ID] = &cloned
			order = append(order, finding.ID)
			continue
		}
		current.EvidenceRefs = append(current.EvidenceRefs, finding.EvidenceRefs...)
	}
	sort.Strings(order)
	for _, id := range order {
		validation, validateErr := ValidateFinding(*merged[id], validator, verifyEvidence)
		if validateErr != nil {
			return nil, nil, nil, validateErr
		}
		switch validation.Finding.Status {
		case contract.FindingConfirmed:
			confirmed = append(confirmed, validation.Finding)
		case contract.FindingInconclusive:
			inconclusive = append(inconclusive, validation.Finding)
		case contract.FindingRejected:
			rejected = append(rejected, validation.Finding)
		}
	}
	sort.Slice(confirmed, func(i, j int) bool { return confirmed[i].ID < confirmed[j].ID })
	sort.Slice(inconclusive, func(i, j int) bool { return inconclusive[i].ID < inconclusive[j].ID })
	sort.Slice(rejected, func(i, j int) bool { return rejected[i].ID < rejected[j].ID })
	return confirmed, inconclusive, rejected, nil
}

// BlockingFindingIDs 返回确认成立且阻塞的发现身份，按字典序稳定排列。
func BlockingFindingIDs(findings []contract.Finding) []string {
	blocking := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.Status == contract.FindingConfirmed && finding.IsBlocking() {
			blocking = append(blocking, finding.ID)
		}
	}
	sort.Strings(blocking)
	return blocking
}
