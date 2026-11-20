// finding.go 定义领域无关的结构化 Finding：验证器提出的待核验发现、复核记录
// 与状态机。Finding 不是已确认缺陷；通用合同不强制代码路径，定位属领域扩展。
//
// Author: fxbin
package contract

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// FindingVersion 是 Finding 的当前格式版本。
	FindingVersion = 1
	// MaxFindingDescriptionBytes 限制单条发现正文体积。
	MaxFindingDescriptionBytes = 4 << 10
)

// FindingStatus 是 Finding 的核验状态。
type FindingStatus string

const (
	// FindingProposed 表示验证器提出、尚未复核的发现。
	FindingProposed FindingStatus = "proposed"
	// FindingConfirmed 表示经复核确认成立。
	FindingConfirmed FindingStatus = "confirmed"
	// FindingRejected 表示经复核证明不成立，原始发现仍须保留。
	FindingRejected FindingStatus = "rejected"
	// FindingInconclusive 表示证据不足、无法定论；不得当作没有问题。
	FindingInconclusive FindingStatus = "inconclusive"
	// FindingResolved 表示已确认并已由后续返工修复。
	FindingResolved FindingStatus = "resolved"
)

// FindingSeverity 是闭合的严重度词表。
type FindingSeverity string

const (
	// SeverityInfo 是提示级。
	SeverityInfo FindingSeverity = "info"
	// SeverityLow 是低严重度。
	SeverityLow FindingSeverity = "low"
	// SeverityMedium 是中严重度。
	SeverityMedium FindingSeverity = "medium"
	// SeverityHigh 是高严重度，复核删除须有依据。
	SeverityHigh FindingSeverity = "high"
	// SeverityCritical 是致命严重度，复核删除须有证据。
	SeverityCritical FindingSeverity = "critical"
)

// FindingReview 是一次复核记录。原始发现、结论与依据必须同时保留，验证失败、
// 超时或额度耗尽只能记为 Inconclusive，不能被解释为没有问题。
type FindingReview struct {
	ReviewerID   string        `json:"reviewer_id"`
	Outcome      FindingStatus `json:"outcome"`
	Rationale    string        `json:"rationale,omitempty"`
	EvidenceRefs []Ref         `json:"evidence_refs,omitempty"`
}

// Finding 是一条结构化发现。TargetID/CriterionID/Category 均为不透明标识；
// Location 是可选的不透明定位字段，通用层不解释其含义。
type Finding struct {
	Version             int             `json:"version"`
	ID                  string          `json:"id"`
	TargetID            string          `json:"target_id"`
	CriterionID         string          `json:"criterion_id,omitempty"`
	Category            string          `json:"category"`
	Severity            FindingSeverity `json:"severity"`
	Description         string          `json:"description"`
	EvidenceRefs        []Ref           `json:"evidence_refs,omitempty"`
	Location            string          `json:"location,omitempty"`
	Reproduction        string          `json:"reproduction,omitempty"`
	SuggestedResolution string          `json:"suggested_resolution,omitempty"`
	Status              FindingStatus   `json:"status"`
	Reviews             []FindingReview `json:"reviews,omitempty"`
}

// Validate 校验 Finding 结构、状态与复核记录。高风险发现被拒绝必须带明确依据
// 或证据，防止仅凭另一轮模型判断被静默删除。
func (finding Finding) Validate() error {
	if finding.Version != FindingVersion {
		return fmt.Errorf("finding version %d is unsupported", finding.Version)
	}
	if !validFindingToken(finding.ID) || !validFindingToken(finding.TargetID) || !validFindingToken(finding.Category) {
		return errors.New("finding has an invalid identity, target or category")
	}
	if finding.CriterionID != "" && !ValidAcceptanceIdentifier(finding.CriterionID) {
		return errors.New("finding has an invalid criterion id")
	}
	switch finding.Severity {
	case SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
	default:
		return fmt.Errorf("finding severity %q is unsupported", finding.Severity)
	}
	if !validFindingText(finding.Description) {
		return errors.New("finding has an invalid description")
	}
	if !validFindingOptionalText(finding.Location) || !validFindingOptionalText(finding.Reproduction) ||
		!validFindingOptionalText(finding.SuggestedResolution) {
		return errors.New("finding has invalid optional text")
	}
	for _, reference := range finding.EvidenceRefs {
		if !validContentDigest(reference.SHA256) {
			return errors.New("finding has an invalid evidence reference")
		}
	}
	switch finding.Status {
	case FindingProposed, FindingConfirmed, FindingRejected, FindingInconclusive, FindingResolved:
	default:
		return fmt.Errorf("finding status %q is unsupported", finding.Status)
	}
	if finding.Status != FindingProposed && len(finding.Reviews) == 0 {
		return errors.New("non-proposed finding requires at least one review")
	}
	lastOutcome := FindingStatus("")
	for _, review := range finding.Reviews {
		if strings.TrimSpace(review.ReviewerID) == "" {
			return errors.New("finding review has no reviewer identity")
		}
		switch review.Outcome {
		case FindingConfirmed, FindingRejected, FindingInconclusive:
		default:
			return fmt.Errorf("finding review outcome %q is unsupported", review.Outcome)
		}
		if !validFindingOptionalText(review.Rationale) {
			return errors.New("finding review has invalid rationale")
		}
		for _, reference := range review.EvidenceRefs {
			if !validContentDigest(reference.SHA256) {
				return errors.New("finding review has an invalid evidence reference")
			}
		}
		lastOutcome = review.Outcome
	}
	// 最终状态必须与最后一次复核结论一致，防止用不一致的 review 组合把
	// 高风险发现伪装成非拒绝状态后静默删除。
	switch finding.Status {
	case FindingConfirmed:
		if lastOutcome != FindingConfirmed {
			return errors.New("confirmed finding's last review is not confirmed")
		}
	case FindingRejected:
		if lastOutcome != FindingRejected {
			return errors.New("rejected finding's last review is not rejected")
		}
		if finding.Severity == SeverityHigh || finding.Severity == SeverityCritical {
			justified := false
			for _, review := range finding.Reviews {
				if review.Outcome == FindingRejected && (strings.TrimSpace(review.Rationale) != "" || len(review.EvidenceRefs) != 0) {
					justified = true
				}
			}
			if !justified {
				return errors.New("rejecting a high-severity finding requires a justified rejection review")
			}
		}
	case FindingInconclusive:
		if lastOutcome != FindingInconclusive {
			return errors.New("inconclusive finding's last review is not inconclusive")
		}
	case FindingResolved:
		// 已修复需以确认复核收尾，并绑定再验证证据；否则不得解除阻塞。
		if lastOutcome != FindingConfirmed || len(finding.EvidenceRefs) == 0 {
			return errors.New("resolved finding requires a final confirmed review and re-verification evidence")
		}
	}
	return nil
}

// IsBlocking 判断 Finding 是否构成阻塞项：只有复核确认且中严重度以上才阻塞。
// Resolved（已绑定再验证证据）不再阻塞；Proposed/Inconclusive 不阻塞，但也不
// 等于没有问题。
func (finding Finding) IsBlocking() bool {
	if err := finding.Validate(); err != nil {
		return true
	}
	if finding.Status != FindingConfirmed {
		return false
	}
	return finding.Severity == SeverityMedium || finding.Severity == SeverityHigh || finding.Severity == SeverityCritical
}

// ConfirmedFindingIDs 返回仍确认成立（未修复）的发现身份，按字典序稳定排列。
func ConfirmedFindingIDs(findings []Finding) []string {
	ids := make([]string, 0, len(findings))
	for _, finding := range findings {
		if finding.Status == FindingConfirmed {
			ids = append(ids, finding.ID)
		}
	}
	sort.Strings(ids)
	return ids
}

// validFindingToken 校验不透明身份词元。
func validFindingToken(value string) bool {
	if value == "" || len(value) > 128 || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) || character == '_' || character == '-' || character == '.' {
			continue
		}
		return false
	}
	return true
}

func validFindingText(value string) bool {
	if value == "" || len(value) > MaxFindingDescriptionBytes || !utf8.ValidString(value) {
		return false
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	return string(Redact([]byte(value))) == value
}

func validFindingOptionalText(value string) bool {
	if value == "" {
		return true
	}
	return validFindingText(value)
}
