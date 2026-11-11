// studio_memory.go 为 CLI 提供记忆合同类型与辅助判定的 Application 门面；
// Studio 层不得直接依赖 internal/memory。
// Author: fxbin
package app

import "github.com/fxbin/cairn/internal/memory"

// 记忆合同类型别名：CLI 仅作展示与状态判定，不持有存储实现。
type (
	// MemoryPromotion 是演化晋升记录。
	MemoryPromotion = memory.Promotion
	// MemoryActivation 是当前激活版本。
	MemoryActivation = memory.Activation
	// MemoryEvaluation 是一次受控评测。
	MemoryEvaluation = memory.Evaluation
	// MemoryEvidenceLedger 是召回解释账本。
	MemoryEvidenceLedger = memory.EvidenceLedger
	// MemoryPromotionMetricSummary 是晋升指标摘要。
	MemoryPromotionMetricSummary = memory.PromotionMetricSummary
	// MemoryCompactionParamsVariant 是压缩参数闭合变体。
	MemoryCompactionParamsVariant = memory.CompactionParamsVariant
	// MemoryRecallQuery 是召回查询。
	MemoryRecallQuery = memory.RecallQuery
)

// 记忆合同常量别名：状态、schema 版本与目标种类。
const (
	MemoryCandidateStateReady               = memory.CandidateStateReady
	MemoryCandidateStateEvaluating          = memory.CandidateStateEvaluating
	MemoryCandidateStateEvaluatedPassed     = memory.CandidateStateEvaluatedPassed
	MemoryEvaluationStatusPassed            = memory.EvaluationStatusPassed
	MemoryPromotionStatusPromotedInactive   = memory.PromotionStatusPromotedInactive
	MemoryEvolutionSchemaVersion            = memory.EvolutionSchemaVersion
	MemoryPromotionSchemaVersion            = memory.PromotionSchemaVersion
	MemoryTargetKindContextCompactionParams = memory.TargetKindContextCompactionParams
)

// MemoryErrNotFound 是记忆条目缺失的哨兵错误。
var MemoryErrNotFound = memory.ErrNotFound

// MemoryIsDerivedID 判断晋升标识是否派生自指标摘要。
func MemoryIsDerivedID(id string) bool { return memory.IsDerivedID(id) }

// MemoryPromotionID 派生晋升标识。
func MemoryPromotionID(targetKind string, version int) (string, error) {
	return memory.PromotionID(targetKind, version)
}

// MemoryVariantSpecDigest 计算压缩参数变体的规格摘要。
func MemoryVariantSpecDigest(variant memory.CompactionParamsVariant) (string, error) {
	return memory.VariantSpecDigest(variant)
}
