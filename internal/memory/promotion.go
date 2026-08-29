// 本文件承载演化候选晋升决策、版本与沙箱激活的闭合合同。决策、版本与激活
// 是三个不可混同的阶段：决策只批准冻结证据，版本先以 inactive 形式落账，
// 只有显式 sandbox 激活才会改变激活指针。
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

const (
	promotionSchemaV1         = 1
	promotionDecisionSchemaV1 = 1
	promotionIDPrefix         = "cairn-evolution-version-v1"
	promotionDecisionIDPrefix = "cairn-evolution-promotion-decision-v1"
	promotionApprovalPrefix   = "cairn-evolution-promotion-approval-v2"
	approverLabelMaxRunes     = 64
)

// PromotionSchemaVersionV1 是面向编排方的当前演化版本合同版本。
const PromotionSchemaVersionV1 = promotionSchemaV1

// SandboxEnvironment 是 M4 唯一允许的激活环境；生产环境没有激活 API。
const SandboxEnvironment = "sandbox"

// 晋升版本状态的闭合词表。promoted_inactive 仅表示人工决策已落账，绝不
// 表示运行时生效；sandbox_activated 只表示曾在 sandbox 指针中激活；revoked
// 表示已由显式回退关闭，不能再次激活。
const (
	PromotionStatusPromotedInactive = "promoted_inactive"
	PromotionStatusSandboxActivated = "sandbox_activated"
	PromotionStatusRevoked          = "revoked"
)

// PromotionMetricSummary 是从摘要已校验的套件结果提取的整数比较事实。
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

// PromotionDecision 是一次人工批准的不可变事实。它绑定候选、通过的评测、
// 基线、变体、预定回退目标与一次性 nonce；approval fingerprint 只对此完整
// 元组计算，不能复用到另一项决策。
type PromotionDecision struct {
	ID                  string                 `json:"decision_id"`
	SchemaVersion       int                    `json:"schema_version"`
	CandidateID         string                 `json:"candidate_id"`
	EvaluationID        string                 `json:"evaluation_id"`
	TargetKind          string                 `json:"target_kind"`
	VariantFingerprint  string                 `json:"variant_fingerprint"`
	VariantSpecSHA256   string                 `json:"variant_spec_sha256"`
	BaselineFingerprint string                 `json:"baseline_fingerprint"`
	EvalSetFingerprint  string                 `json:"eval_set_fingerprint"`
	SuiteResultDigest   string                 `json:"suite_result_digest"`
	ComparatorVersion   string                 `json:"comparator_version"`
	ConclusionTrigger   string                 `json:"conclusion_trigger"`
	Metrics             PromotionMetricSummary `json:"metrics"`
	RollbackTargetID    string                 `json:"rollback_target_promotion_id,omitempty"`
	ApprovalFingerprint string                 `json:"approval_fingerprint"`
	ApprovalNonce       string                 `json:"approval_nonce"`
	Approver            string                 `json:"approver"`
	DecidedAt           time.Time              `json:"decided_at"`
}

// Promotion 是由 PromotionDecision 创建的不可变 EvolutionVersion 投影。证据
// 字段保留在投影中供 CLI 只读展示，但 Store.Check 会反向与决策、候选和评测
// 行逐字段核对，不能把它们视为第二事实源。
type Promotion struct {
	ID                  string                  `json:"promotion_id"`
	SchemaVersion       int                     `json:"schema_version"`
	DecisionID          string                  `json:"decision_id"`
	CandidateID         string                  `json:"candidate_id"`
	EvaluationID        string                  `json:"evaluation_id"`
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
	RollbackTargetID    string                  `json:"rollback_target_promotion_id,omitempty"`
	ApprovalFingerprint string                  `json:"approval_fingerprint"`
	ApprovalNonce       string                  `json:"approval_nonce"`
	Approver            string                  `json:"approver"`
	Status              string                  `json:"status"`
	CreatedAt           time.Time               `json:"created_at"`
	RevokedAt           time.Time               `json:"revoked_at,omitempty"`
}

// Activation 是 sandbox 环境当前的版本指针，并保留本次激活固定的 previous
// version。不存在记录表示 sandbox 使用产品基线。
type Activation struct {
	Environment         string    `json:"environment"`
	TargetKind          string    `json:"target_kind"`
	PromotionID         string    `json:"promotion_id"`
	PreviousPromotionID string    `json:"previous_promotion_id,omitempty"`
	ActivatedAt         time.Time `json:"activated_at"`
}

// Validate 校验版本投影的结构合同；跨表事实一致性由 SQLite Store.Check 负责。
func (promotion Promotion) Validate() error {
	if promotion.SchemaVersion != promotionSchemaV1 {
		return fmt.Errorf("promotion schema_version %d is not supported", promotion.SchemaVersion)
	}
	if promotion.ID == "" || promotion.DecisionID == "" || promotion.CandidateID == "" || promotion.EvaluationID == "" || promotion.TargetKind == "" {
		return errors.New("promotion identity fields must not be empty")
	}
	if promotion.TargetKind != TargetKindContextCompactionParams || promotion.Version < 1 {
		return errors.New("promotion target kind or version is invalid")
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
		"approval_nonce":       promotion.ApprovalNonce,
	} {
		if !DigestHexShape(value) {
			return fmt.Errorf("promotion %s must be a sha256 digest", name)
		}
	}
	if !DigestHexShape(promotion.ApprovalFingerprint) || promotion.ComparatorVersion == "" || promotion.Conclusion != EvaluationStatusPassed || promotion.ConclusionTrigger == "" {
		return errors.New("promotion approval or conclusion fields are invalid")
	}
	if promotion.Metrics.BaselineSuccesses < 0 || promotion.Metrics.VariantSuccesses < 0 || promotion.Metrics.BaselineInputTokens < 0 || promotion.Metrics.VariantInputTokens < 0 || promotion.Metrics.BaselineOutputTokens < 0 || promotion.Metrics.VariantOutputTokens < 0 || promotion.Metrics.BaselineMedianWallMS < 0 || promotion.Metrics.VariantMedianWallMS < 0 {
		return errors.New("promotion metrics cannot be negative")
	}
	if strings.TrimSpace(promotion.Approver) == "" || len([]rune(promotion.Approver)) > approverLabelMaxRunes || promotion.CreatedAt.IsZero() {
		return errors.New("promotion approver or creation time is invalid")
	}
	if promotion.RollbackTargetID != "" && !DigestHexShape(promotion.RollbackTargetID) {
		return errors.New("promotion rollback target must be a sha256-shaped version id when present")
	}
	switch promotion.Status {
	case PromotionStatusPromotedInactive, PromotionStatusSandboxActivated, PromotionStatusRevoked:
	default:
		return fmt.Errorf("promotion status %q is not in the closed lexicon", promotion.Status)
	}
	if promotion.Status == PromotionStatusRevoked && promotion.RevokedAt.IsZero() {
		return errors.New("revoked promotion requires revoked_at")
	}
	return nil
}

// Decision 返回与版本投影一一对应的不可变人工决策。
func (promotion Promotion) Decision() PromotionDecision {
	return PromotionDecision{
		ID: promotion.DecisionID, SchemaVersion: promotionDecisionSchemaV1,
		CandidateID: promotion.CandidateID, EvaluationID: promotion.EvaluationID,
		TargetKind: promotion.TargetKind, VariantFingerprint: promotion.VariantFingerprint,
		VariantSpecSHA256: promotion.VariantSpecSHA256, BaselineFingerprint: promotion.BaselineFingerprint,
		EvalSetFingerprint: promotion.EvalSetFingerprint, SuiteResultDigest: promotion.SuiteResultDigest,
		ComparatorVersion: promotion.ComparatorVersion, ConclusionTrigger: promotion.ConclusionTrigger,
		Metrics: promotion.Metrics, RollbackTargetID: promotion.RollbackTargetID,
		ApprovalFingerprint: promotion.ApprovalFingerprint, ApprovalNonce: promotion.ApprovalNonce,
		Approver: promotion.Approver, DecidedAt: promotion.CreatedAt,
	}
}

// Validate 校验人工决策的字段形态与一次性审批绑定；存储层还会把它与候选、
// 评测及版本行交叉验证。
func (decision PromotionDecision) Validate() error {
	if decision.SchemaVersion != promotionDecisionSchemaV1 || decision.ID == "" || decision.CandidateID == "" || decision.EvaluationID == "" || decision.TargetKind != TargetKindContextCompactionParams {
		return errors.New("promotion decision identity is invalid")
	}
	for name, value := range map[string]string{"variant_fingerprint": decision.VariantFingerprint, "variant_spec_sha256": decision.VariantSpecSHA256, "baseline_fingerprint": decision.BaselineFingerprint, "eval_set_fingerprint": decision.EvalSetFingerprint, "suite_result_digest": decision.SuiteResultDigest, "approval_fingerprint": decision.ApprovalFingerprint, "approval_nonce": decision.ApprovalNonce} {
		if !DigestHexShape(value) {
			return fmt.Errorf("promotion decision %s must be a sha256 digest", name)
		}
	}
	if decision.ComparatorVersion == "" || decision.ConclusionTrigger == "" || strings.TrimSpace(decision.Approver) == "" || len([]rune(decision.Approver)) > approverLabelMaxRunes || decision.DecidedAt.IsZero() {
		return errors.New("promotion decision metadata is invalid")
	}
	if decision.RollbackTargetID != "" && !DigestHexShape(decision.RollbackTargetID) {
		return errors.New("promotion decision rollback target is invalid")
	}
	return nil
}

// PromotionID 按目标类型和单调版本号派生 EvolutionVersion 身份。
func PromotionID(targetKind string, version int) (string, error) {
	if targetKind != TargetKindContextCompactionParams || version < 1 {
		return "", errors.New("promotion identity requires a supported target kind and one-based version")
	}
	return canonicaljson.Fingerprint(promotionIDPrefix, struct {
		TargetKind string `json:"target_kind"`
		Version    int    `json:"version"`
	}{targetKind, version})
}

// PromotionApprovalFingerprint 对完整人工决策元组计算可人工复核的指纹。
func PromotionApprovalFingerprint(promotion Promotion) (string, error) {
	if promotion.CandidateID == "" || promotion.EvaluationID == "" || promotion.TargetKind != TargetKindContextCompactionParams || promotion.Version < 1 || promotion.ComparatorVersion == "" || promotion.ConclusionTrigger == "" {
		return "", errors.New("promotion approval tuple is incomplete")
	}
	for name, value := range map[string]string{"baseline_fingerprint": promotion.BaselineFingerprint, "variant_fingerprint": promotion.VariantFingerprint, "variant_spec_sha256": promotion.VariantSpecSHA256, "eval_set_fingerprint": promotion.EvalSetFingerprint, "suite_result_digest": promotion.SuiteResultDigest, "approval_nonce": promotion.ApprovalNonce} {
		if !DigestHexShape(value) {
			return "", fmt.Errorf("promotion approval %s must be a sha256 digest", name)
		}
	}
	return canonicaljson.Fingerprint(promotionApprovalPrefix, struct {
		CandidateID         string                 `json:"candidate_id"`
		EvaluationID        string                 `json:"evaluation_id"`
		BaselineFingerprint string                 `json:"baseline_fingerprint"`
		VariantFingerprint  string                 `json:"variant_fingerprint"`
		VariantSpecSHA256   string                 `json:"variant_spec_sha256"`
		EvalSetFingerprint  string                 `json:"eval_set_fingerprint"`
		SuiteResultDigest   string                 `json:"suite_result_digest"`
		ComparatorVersion   string                 `json:"comparator_version"`
		ConclusionTrigger   string                 `json:"conclusion_trigger"`
		Metrics             PromotionMetricSummary `json:"metrics"`
		RollbackTargetID    string                 `json:"rollback_target_promotion_id"`
		ApprovalNonce       string                 `json:"approval_nonce"`
		TargetKind          string                 `json:"target_kind"`
		Version             int                    `json:"version"`
	}{promotion.CandidateID, promotion.EvaluationID, promotion.BaselineFingerprint, promotion.VariantFingerprint, promotion.VariantSpecSHA256, promotion.EvalSetFingerprint, promotion.SuiteResultDigest, promotion.ComparatorVersion, promotion.ConclusionTrigger, promotion.Metrics, promotion.RollbackTargetID, promotion.ApprovalNonce, promotion.TargetKind, promotion.Version})
}

// PromotionDecisionID 从完整决策事实派生稳定身份，连同批准者和决定时刻一并
// 绑定，防止旁路篡改责任归属或审计时间。
func PromotionDecisionID(promotion Promotion) (string, error) {
	fingerprint, err := PromotionApprovalFingerprint(promotion)
	if err != nil {
		return "", err
	}
	return canonicaljson.Fingerprint(promotionDecisionIDPrefix, struct {
		ApprovalFingerprint string    `json:"approval_fingerprint"`
		Approver            string    `json:"approver"`
		DecidedAt           time.Time `json:"decided_at"`
	}{fingerprint, promotion.Approver, promotion.CreatedAt.UTC()})
}
