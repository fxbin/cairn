// Package memory 定义可信记忆控制面的受限值合同；本文件补充 M4 的演化候选
// 合同：闭合首发 TargetKind、变体参数、状态机、冷却键与来源资格复核。
// 候选绝不自动生成；M3 派生候选只能作为来源证据被人工显式引用。
//
// Author: fxbin
package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

const (
	// EvolutionSchemaVersion 是演化候选、变体与冷却合同的当前版本。
	EvolutionSchemaVersion = 1
	// TargetKindContextCompactionParams 是唯一批准的首发变体目标类型。
	TargetKindContextCompactionParams = "context_compaction_params"
	// VariantPercentBase 是比例字段的百分比基数，比例一律以整数百分比表达。
	VariantPercentBase = 100
	// VariantMinPromptTokens 限制变体提示上限的最小值，防止零值绕过压缩。
	VariantMinPromptTokens = 1024
	// VariantMaxPromptTokens 限制变体提示上限不超过既有产品档的最大值。
	VariantMaxPromptTokens = 1 << 20
	// VariantMinSummaryBytes 限制变体摘要下限，保证检查点可承载清单与事实。
	VariantMinSummaryBytes = 512
	// VariantMaxSummaryBytes 限制变体摘要上限与既有产品字段同阶。
	VariantMaxSummaryBytes = 1 << 20
	// VariantMinBudgetPercent 限制清单/事实预算比例的最小百分比。
	VariantMinBudgetPercent = 10
	// VariantMaxBudgetPercent 限制清单/事实预算比例的最大百分比。
	VariantMaxBudgetPercent = 90
	// VariantMinReservePercent 限制触发阈值余量的最小百分比。
	VariantMinReservePercent = 5
	// VariantMaxReservePercent 限制触发阈值余量的最大百分比。
	VariantMaxReservePercent = 50
	// CandidateStateDraft 表示候选已创建但尚未绑定评测集。
	CandidateStateDraft = "draft"
	// CandidateStateReady 表示候选已绑定冻结评测集并等待评测。
	CandidateStateReady = "ready"
	// CandidateStateEvaluating 表示候选正在冻结评测集中评测。
	CandidateStateEvaluating = "evaluating"
	// CandidateStateEvaluatedPassed 表示评测达到预注册通过门。
	CandidateStateEvaluatedPassed = "evaluated_passed"
	// CandidateStateEvaluatedFailed 表示评测未达到预注册通过门。
	CandidateStateEvaluatedFailed = "evaluated_failed"
	// CandidateStateInconclusive 表示评测中断且永不具备晋升资格。
	CandidateStateInconclusive = "inconclusive"
	// CandidateStateInvalidated 表示来源失效导致的终态作废。
	CandidateStateInvalidated = "invalidated"
	// CandidateStatePromoted 表示候选经 HITL 晋升（M4-3 才可达）。
	CandidateStatePromoted = "promoted"
	// CandidateStateRejected 表示候选被显式拒绝并进入冷却。
	CandidateStateRejected = "rejected"
	// CandidateStateExpired 表示候选保留期耗尽。
	CandidateStateExpired = "expired"
	// EvolutionAuditCreated 记录候选创建审计动作。
	EvolutionAuditCreated = "created"
	// EvolutionAuditRejected 记录候选拒绝审计动作。
	EvolutionAuditRejected = "rejected"
	// EvolutionAuditInvalidated 记录来源失效作废审计动作。
	EvolutionAuditInvalidated = "invalidated"
	// EvolutionAuditExpired 记录保留期耗尽审计动作。
	EvolutionAuditExpired = "expired"
	// EvolutionAuditPromoted 记录人工晋升审计动作（版本账本落账）。
	EvolutionAuditPromoted = "promoted"
	// EvolutionAuditSandboxActivated 记录已批准版本被显式激活到 sandbox。
	EvolutionAuditSandboxActivated = "sandbox_activated"
	// EvolutionAuditPromotionRolledBack 记录激活版本回退审计动作。
	EvolutionAuditPromotionRolledBack = "promotion_rolled_back"
	// RejectReasonInsufficientEvidence 表示来源证据不再充分。
	RejectReasonInsufficientEvidence = "insufficient_evidence"
	// RejectReasonEvaluationFailed 保留给 M4-2 评测未达门槛的拒绝。
	RejectReasonEvaluationFailed = "evaluation_failed"
	// RejectReasonSafetyViolation 表示隔离或安全检查失败。
	RejectReasonSafetyViolation = "safety_violation"
	// RejectReasonHumanRequest 表示维护者明确请求拒绝。
	RejectReasonHumanRequest = "human_request"
	// DefaultCandidateTTL 是候选未显式指定保留期时的默认值。
	DefaultCandidateTTL = 30 * 24 * time.Hour
	// DefaultCooldown 是拒绝后的默认冷却期。
	DefaultCooldown = 7 * 24 * time.Hour
	// MaxCooldown 限制单次冷却的最长时长。
	MaxCooldown = 90 * 24 * time.Hour
	// candidateIDPrefix 是演化候选标识的固定前缀。
	candidateIDPrefix = "evo_"
)

// CompactionParamsVariant 是首发 TargetKind 的闭合变体结构：全部为带界的
// 整数参数，比例以百分比表达，不含任何自由文本或路径。
type CompactionParamsVariant struct {
	SchemaVersion           int `json:"schema_version"`
	MaxPromptTokens         int `json:"max_prompt_tokens"`
	MaxSummaryBytes         int `json:"max_summary_bytes"`
	InventoryBudgetPercent  int `json:"inventory_budget_percent"`
	ThresholdReservePercent int `json:"threshold_reserve_percent"`
}

// EvolutionSourceRef 精确指向一条 M3 派生候选版本（复合键），仅作证据引用。
type EvolutionSourceRef struct {
	DerivedID       string `json:"derived_id"`
	ConsolidationID string `json:"consolidation_id"`
}

// EvolutionCandidate 是一条不可变的演化候选：创建后除状态列外不得改写。
// 全部字段为闭合值、不可逆摘要或稳定标识符。
type EvolutionCandidate struct {
	ID                  string             `json:"id"`
	SchemaVersion       int                `json:"schema_version"`
	WorkspaceHash       string             `json:"workspace_hash"`
	Source              EvolutionSourceRef `json:"source"`
	TargetKind          string             `json:"target_kind"`
	VariantSpecSHA256   string             `json:"variant_spec_sha256"`
	VariantFingerprint  string             `json:"variant_fingerprint"`
	BaselineFingerprint string             `json:"baseline_fingerprint"`
	EvalSetFingerprint  string             `json:"eval_set_fingerprint,omitempty"`
	CreatedAt           time.Time          `json:"created_at"`
	ExpiresAt           time.Time          `json:"expires_at"`
	Status              string             `json:"status"`
	NoteDigest          string             `json:"note_digest,omitempty"`
}

// EvolutionAuditEntry 是演化候选状态迁移的脱敏审计行；Reason 仅枚举。
type EvolutionAuditEntry struct {
	CandidateID string    `json:"candidate_id"`
	Action      string    `json:"action"`
	Reason      string    `json:"reason,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
}

// CooldownKey 唯一标识一条冷却：同来源版本、同目标类型、同变体指纹。
type CooldownKey struct {
	Source             EvolutionSourceRef
	TargetKind         string
	VariantFingerprint string
}

// CooldownRecord 是一条冷却记录及其截止时间。
type CooldownRecord struct {
	Key           CooldownKey
	CooldownUntil time.Time
}

// VariantFingerprint 由 TargetKind 与规范化变体结构派生；同参数恒同指纹。
func VariantFingerprint(targetKind string, variant CompactionParamsVariant) (string, error) {
	if targetKind != TargetKindContextCompactionParams {
		return "", fmt.Errorf("unsupported evolution target kind %q", targetKind)
	}
	if err := variant.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(variant)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte("cairn-evolution-variant-v1\x00" + targetKind + "\x00" + string(encoded)))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// VariantSpecDigest 返回变体结构的独立内容摘要，供制品引用交叉核对。
func VariantSpecDigest(variant CompactionParamsVariant) (string, error) {
	if err := variant.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(variant)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// EvolutionCandidateID 由工作区、来源复合键、目标类型与变体指纹派生；
// 同一组合的重试恒命中同一候选，天然幂等。
func EvolutionCandidateID(workspaceHash string, source EvolutionSourceRef, targetKind, variantFingerprint string) string {
	sum := sha256.Sum256([]byte("cairn-evolution-candidate-v1\x00" + workspaceHash + "\x00" + source.DerivedID + "\x00" + source.ConsolidationID + "\x00" + targetKind + "\x00" + variantFingerprint))
	return candidateIDPrefix + hex.EncodeToString(sum[:])
}

// NormalizeCandidateTTL 规范候选保留期：零值取默认，越界失败关闭。
func NormalizeCandidateTTL(ttl time.Duration) (time.Duration, error) {
	if ttl == 0 {
		return DefaultCandidateTTL, nil
	}
	if ttl < 0 || ttl > MaxTTL {
		return 0, fmt.Errorf("evolution candidate TTL must be within 1ns and %s", MaxTTL)
	}
	return ttl, nil
}

// NormalizeCooldown 规范冷却期：零值取默认，越界失败关闭。
func NormalizeCooldown(cooldown time.Duration) (time.Duration, error) {
	if cooldown == 0 {
		return DefaultCooldown, nil
	}
	if cooldown < 0 || cooldown > MaxCooldown {
		return 0, fmt.Errorf("evolution cooldown must be within 1ns and %s", MaxCooldown)
	}
	return cooldown, nil
}

// ValidEvolutionRejectionReason 判断拒绝理由是否属于封闭词表。
func ValidEvolutionRejectionReason(reason string) bool {
	switch reason {
	case RejectReasonInsufficientEvidence, RejectReasonEvaluationFailed, RejectReasonSafetyViolation, RejectReasonHumanRequest:
		return true
	default:
		return false
	}
}

// ValidCandidateTransition 判断状态迁移是否合法。开放面覆盖创建后的三个
// 手工终态入口、评测生命周期（ready→evaluating 启动、evaluating→三个结
// 论态）与人工晋升入口（evaluated_passed→promoted，由版本账本事务承载）。
func ValidCandidateTransition(from, to string) error {
	switch {
	case from == CandidateStateDraft && to == CandidateStateRejected:
		return nil
	case from == CandidateStateDraft && to == CandidateStateInvalidated:
		return nil
	case from == CandidateStateDraft && to == CandidateStateExpired:
		return nil
	case from == CandidateStateReady && to == CandidateStateRejected:
		return nil
	case from == CandidateStateReady && to == CandidateStateInvalidated:
		return nil
	case from == CandidateStateReady && to == CandidateStateExpired:
		return nil
	case from == CandidateStateReady && to == CandidateStateEvaluating:
		return nil
	case from == CandidateStateEvaluating && to == CandidateStateInvalidated:
		return nil
	case from == CandidateStateEvaluating && to == CandidateStateEvaluatedPassed:
		return nil
	case from == CandidateStateEvaluating && to == CandidateStateEvaluatedFailed:
		return nil
	case from == CandidateStateEvaluating && to == CandidateStateInconclusive:
		return nil
	// M4-3 人工晋升入口：唯一从评测通过态出发的迁移，由版本账本事务
	// 携带读回校验写入。
	case from == CandidateStateEvaluatedPassed && to == CandidateStatePromoted:
		return nil
	default:
		return fmt.Errorf("evolution candidate transition %s -> %s is not permitted in this slice", from, to)
	}
}

// IsTerminalCandidateState 判断候选是否处于不可再迁移的生命周期终态。评测
// 通过只是结论固定且仍可进入一次人工晋升，不属于生命周期终态。
func IsTerminalCandidateState(state string) bool {
	switch state {
	case CandidateStateRejected, CandidateStateInvalidated, CandidateStateExpired, CandidateStatePromoted,
		CandidateStateEvaluatedFailed, CandidateStateInconclusive:
		return true
	default:
		return false
	}
}

// Validate 校验变体参数全部落在闭合边界内。
func (variant CompactionParamsVariant) Validate() error {
	if variant.SchemaVersion != EvolutionSchemaVersion {
		return fmt.Errorf("unsupported variant schema version %d", variant.SchemaVersion)
	}
	if variant.MaxPromptTokens < VariantMinPromptTokens || variant.MaxPromptTokens > VariantMaxPromptTokens {
		return fmt.Errorf("variant max_prompt_tokens must be within %d and %d", VariantMinPromptTokens, VariantMaxPromptTokens)
	}
	if variant.MaxSummaryBytes < VariantMinSummaryBytes || variant.MaxSummaryBytes > VariantMaxSummaryBytes {
		return fmt.Errorf("variant max_summary_bytes must be within %d and %d", VariantMinSummaryBytes, VariantMaxSummaryBytes)
	}
	if variant.InventoryBudgetPercent < VariantMinBudgetPercent || variant.InventoryBudgetPercent > VariantMaxBudgetPercent {
		return fmt.Errorf("variant inventory_budget_percent must be within %d and %d", VariantMinBudgetPercent, VariantMaxBudgetPercent)
	}
	if variant.ThresholdReservePercent < VariantMinReservePercent || variant.ThresholdReservePercent > VariantMaxReservePercent {
		return fmt.Errorf("variant threshold_reserve_percent must be within %d and %d", VariantMinReservePercent, VariantMaxReservePercent)
	}
	return nil
}

// Validate 校验演化候选的完整闭合合同：标识一致性、来源复合键、指纹格式、
// 保留期与状态词表。
func (candidate EvolutionCandidate) Validate() error {
	if candidate.SchemaVersion != EvolutionSchemaVersion {
		return fmt.Errorf("unsupported evolution candidate schema version %d", candidate.SchemaVersion)
	}
	if candidate.TargetKind != TargetKindContextCompactionParams {
		return fmt.Errorf("unsupported evolution target kind %q", candidate.TargetKind)
	}
	if !validDigest(candidate.WorkspaceHash) || !validDigest(candidate.BaselineFingerprint) {
		return errors.New("evolution candidate digest fields are invalid")
	}
	if candidate.EvalSetFingerprint != "" && !validDigest(candidate.EvalSetFingerprint) {
		return errors.New("evolution candidate eval set fingerprint is invalid")
	}
	if !validDigest(candidate.VariantSpecSHA256) || !validDigest(candidate.VariantFingerprint) {
		return errors.New("evolution candidate variant fingerprints are invalid")
	}
	if candidate.NoteDigest != "" && !validDigest(candidate.NoteDigest) {
		return errors.New("evolution candidate note digest is invalid")
	}
	if !validIdentifier(candidate.ID) || !validIdentifier(candidate.Source.DerivedID) || !validIdentifier(candidate.Source.ConsolidationID) {
		return errors.New("evolution candidate identifier fields are invalid")
	}
	if candidate.ID != EvolutionCandidateID(candidate.WorkspaceHash, candidate.Source, candidate.TargetKind, candidate.VariantFingerprint) {
		return errors.New("evolution candidate ID does not match workspace, source, target and variant")
	}
	if candidate.CreatedAt.IsZero() || candidate.ExpiresAt.IsZero() || !candidate.ExpiresAt.After(candidate.CreatedAt) || candidate.ExpiresAt.Sub(candidate.CreatedAt) > MaxTTL {
		return errors.New("evolution candidate retention period is invalid")
	}
	switch candidate.Status {
	case CandidateStateDraft, CandidateStateReady, CandidateStateEvaluating,
		CandidateStateEvaluatedPassed, CandidateStateEvaluatedFailed,
		CandidateStateInconclusive, CandidateStateInvalidated,
		CandidateStatePromoted, CandidateStateRejected, CandidateStateExpired:
	default:
		return fmt.Errorf("evolution candidate status %q is invalid", candidate.Status)
	}
	if candidate.Status == CandidateStateReady && candidate.EvalSetFingerprint == "" {
		return errors.New("ready evolution candidate requires a frozen eval set fingerprint")
	}
	return nil
}

// Validate 校验冷却键的闭合字段。
func (key CooldownKey) Validate() error {
	if !validIdentifier(key.Source.DerivedID) || !validIdentifier(key.Source.ConsolidationID) {
		return errors.New("cooldown source version key is invalid")
	}
	if key.TargetKind != TargetKindContextCompactionParams {
		return fmt.Errorf("unsupported cooldown target kind %q", key.TargetKind)
	}
	if !validDigest(key.VariantFingerprint) {
		return errors.New("cooldown variant fingerprint is invalid")
	}
	return nil
}
