// 本文件承载演化候选晋升的版本账本合同：版本记录、激活指针、确定性身份
// 与审批指纹。M4 范围内只记录决策与版本事实，不提供任何运行时激活通道；
// 生产激活属于后续独立决策。批准者字段是操作者算名（≤64 字符的非敏感标
// 签），不是身份证明。
//
// Author: fxbin
package memory

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

// promotionSchemaV1 是晋升版本记录的当前合同版本。
const promotionSchemaV1 = 1

// promotionIDPrefix 是晋升身份的版本化域前缀。
const promotionIDPrefix = "cairn-evolution-promotion-v1"

// promotionApprovalPrefix 是晋升审批指纹的版本化域前缀。
const promotionApprovalPrefix = "cairn-evolution-promotion-approval-v1"

// approverLabelMaxRunes 限制批准者算名的长度上限，防止把自由文本借道算名
// 字段写入版本记录。
const approverLabelMaxRunes = 64

// 晋升版本状态的闭合词表：active 表示当前激活版本集；rolled_back 表示已
// 被显式回退、仅作为历史版本留存，不可再次激活。
const (
	PromotionStatusActive     = "active"
	PromotionStatusRolledBack = "rolled_back"
)

// PromotionMetricSummary 是从闭合评测结果提取的指标比较摘要：只承载可由
// suite-result 复核的整数事实，不包含成本单价换算或任何文本。
type PromotionMetricSummary struct {
	BaselineSuccesses    int   `json:"baseline_successes"`
	VariantSuccesses     int   `json:"variant_successes"`
	BaselineInputTokens  int64 `json:"baseline_input_tokens"`
	VariantInputTokens   int64 `json:"variant_input_tokens"`
	BaselineOutputTokens int64 `json:"baseline_output_tokens"`
	VariantOutputTokens  int64 `json:"variant_output_tokens"`
	BaselineMedianWallMS int64 `json:"baseline_median_wall_ms"`
	VariantMedianWallMS  int64 `json:"variant_median_wall_ms"`
}

// Promotion 是一次人工晋升的版本记录：候选、变体正文与指纹、冻结输入引用、
// 评测结论与指标比较、批准者与回退目标全部落账。同一候选至多一条、同一
// 目标类型的版本号单调递增；回退只移动激活指针与状态，版本记录正文不可
// 改写。
type Promotion struct {
	ID                  string                  `json:"promotion_id"`
	SchemaVersion       int                     `json:"schema_version"`
	CandidateID         string                  `json:"candidate_id"`
	TargetKind          string                  `json:"target_kind"`
	Version             int                     `json:"version"`
	Variant             CompactionParamsVariant `json:"variant"`
	VariantFingerprint  string                  `json:"variant_fingerprint"`
	VariantSpecSHA256   string                  `json:"variant_spec_sha256"`
	BaselineFingerprint string                  `json:"baseline_fingerprint"`
	EvalSetFingerprint  string                  `json:"eval_set_fingerprint"`
	SuiteResultDigest   string                  `json:"suite_result_digest"`
	ComparatorVersion   string                  `json:"comparator_version"`
	Conclusion          string                  `json:"conclusion"`
	ConclusionTrigger   string                  `json:"conclusion_trigger"`
	Metrics             PromotionMetricSummary  `json:"metrics"`
	Approver            string                  `json:"approver"`
	RollbackTargetID    string                  `json:"rollback_target_promotion_id,omitempty"`
	Status              string                  `json:"status"`
	CreatedAt           time.Time               `json:"created_at"`
	RolledBackAt        time.Time               `json:"rolled_back_at,omitempty"`
}

// Activation 是当前激活版本集的指针：每个目标类型至多一行，行不存在表示
// 激活产品基线。M4 内没有任何运行时消费者，指针只服务账本查询与回退。
type Activation struct {
	TargetKind  string    `json:"target_kind"`
	PromotionID string    `json:"promotion_id"`
	ActivatedAt time.Time `json:"activated_at"`
}

// Validate 校验版本记录的结构合同：身份与版本完整、闭合词表、指标非负、
// 算名上限与回退目标的版本约束（首版回退产品基线不带目标，其余版本必须
// 携带可追溯的回退目标身份）。
func (promotion Promotion) Validate() error {
	if promotion.SchemaVersion != promotionSchemaV1 {
		return fmt.Errorf("promotion schema_version %d is not supported", promotion.SchemaVersion)
	}
	if promotion.ID == "" || promotion.CandidateID == "" || promotion.TargetKind == "" {
		return errors.New("promotion identity fields must not be empty")
	}
	if promotion.TargetKind != TargetKindContextCompactionParams {
		return fmt.Errorf("unsupported promotion target kind %q", promotion.TargetKind)
	}
	if promotion.Version < 1 {
		return errors.New("promotion version must be one-based")
	}
	if err := promotion.Variant.Validate(); err != nil {
		return fmt.Errorf("promotion variant: %w", err)
	}
	for name, value := range map[string]string{
		"variant_fingerprint":  promotion.VariantFingerprint,
		"variant_spec_sha256":  promotion.VariantSpecSHA256,
		"baseline_fingerprint": promotion.BaselineFingerprint,
		"eval_set_fingerprint": promotion.EvalSetFingerprint,
		"suite_result_digest":  promotion.SuiteResultDigest,
	} {
		if !DigestHexShape(value) {
			return fmt.Errorf("promotion %s must be a sha256 digest", name)
		}
	}
	if promotion.ComparatorVersion == "" || promotion.Conclusion == "" || promotion.ConclusionTrigger == "" {
		return errors.New("promotion requires comparator version and a concluded trigger")
	}
	if promotion.Conclusion != EvaluationStatusPassed {
		return fmt.Errorf("only passed evaluations may be promoted, got %q", promotion.Conclusion)
	}
	if promotion.Metrics.BaselineSuccesses < 0 || promotion.Metrics.VariantSuccesses < 0 ||
		promotion.Metrics.BaselineInputTokens < 0 || promotion.Metrics.VariantInputTokens < 0 ||
		promotion.Metrics.BaselineOutputTokens < 0 || promotion.Metrics.VariantOutputTokens < 0 ||
		promotion.Metrics.BaselineMedianWallMS < 0 || promotion.Metrics.VariantMedianWallMS < 0 {
		return errors.New("promotion metrics cannot be negative")
	}
	if strings.TrimSpace(promotion.Approver) == "" || len([]rune(promotion.Approver)) > approverLabelMaxRunes {
		return fmt.Errorf("promotion approver label must be 1..%d runes", approverLabelMaxRunes)
	}
	switch promotion.Status {
	case PromotionStatusActive, PromotionStatusRolledBack:
	default:
		return fmt.Errorf("promotion status %q is not in the closed lexicon", promotion.Status)
	}
	if promotion.CreatedAt.IsZero() {
		return errors.New("promotion created_at is required")
	}
	if promotion.Status == PromotionStatusRolledBack && promotion.RolledBackAt.IsZero() {
		return errors.New("rolled-back promotion requires rolled_back_at")
	}
	if promotion.Version == 1 && promotion.RollbackTargetID != "" {
		return errors.New("the first promotion rolls back to the product baseline and carries no target id")
	}
	if promotion.Version > 1 && !DigestHexShape(promotion.RollbackTargetID) {
		return errors.New("non-first promotion requires a sha256-shaped rollback target id")
	}
	return nil
}

// PromotionID 由候选、变体指纹与版本号派生确定性晋升身份：同输入恒同 ID，
// 事务内重派生比对防身份漂移。版本号从一开始计数。
func PromotionID(candidateID, variantFingerprint string, version int) (string, error) {
	if candidateID == "" || variantFingerprint == "" || version < 1 {
		return "", errors.New("promotion identity requires candidate, variant fingerprint and a one-based version")
	}
	view := struct {
		CandidateID        string `json:"candidate_id"`
		VariantFingerprint string `json:"variant_fingerprint"`
		Version            int    `json:"version"`
	}{candidateID, variantFingerprint, version}
	return canonicaljson.Fingerprint(promotionIDPrefix, view)
}

// PromotionApprovalFingerprint 计算晋升审批指纹：操作者必须显式携带与本次
// 晋升全部冻结输入一致的摘要才能触发事务，防止误晋升或内容漂移下的盲批。
// 算名不参与指纹——审批绑定的是被批对象而非批准人。
func PromotionApprovalFingerprint(candidateID, variantFingerprint, evalSetFingerprint, suiteResultDigest, targetKind string, version int) (string, error) {
	if candidateID == "" || variantFingerprint == "" || evalSetFingerprint == "" || suiteResultDigest == "" || targetKind == "" || version < 1 {
		return "", errors.New("promotion approval fingerprint requires the complete frozen tuple")
	}
	view := struct {
		CandidateID        string `json:"candidate_id"`
		VariantFingerprint string `json:"variant_fingerprint"`
		EvalSetFingerprint string `json:"eval_set_fingerprint"`
		SuiteResultDigest  string `json:"suite_result_digest"`
		TargetKind         string `json:"target_kind"`
		Version            int    `json:"version"`
	}{candidateID, variantFingerprint, evalSetFingerprint, suiteResultDigest, targetKind, version}
	return canonicaljson.Fingerprint(promotionApprovalPrefix, view)
}
