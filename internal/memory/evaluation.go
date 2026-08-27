// Package memory 的 Evaluation 部分定义候选评测的身份与状态合同：同一
// 候选只绑定一个 EvalSet 并只允许一个 Evaluation 身份；评测一旦进入终态
// 不得重跑或改写。身份由版本化域前缀与规范化五字段派生，确定性即幂等。
//
// Author: fxbin
package memory

import (
	"errors"

	"github.com/fxbin/cairn/internal/canonicaljson"
)

// evaluationIDPrefix 是 Evaluation 标识的版本化域前缀。
const evaluationIDPrefix = "cairn-evaluation-v1"

// Evaluation 生命周期的闭合状态词表：running 表示启动事务已提交、评测尚未
// 达成结论；四个结论态一经写入不可改写，invalid 对应隔离或证据合同的破坏。
const (
	EvaluationStatusRunning      = "running"
	EvaluationStatusPassed       = "passed"
	EvaluationStatusFailed       = "failed"
	EvaluationStatusInconclusive = "inconclusive"
	EvaluationStatusInvalid      = "invalid"
)

// ValidEvaluationStatus 判断字符串是否属于评测状态的闭合词表。
func ValidEvaluationStatus(status string) bool {
	switch status {
	case EvaluationStatusRunning, EvaluationStatusPassed, EvaluationStatusFailed, EvaluationStatusInconclusive, EvaluationStatusInvalid:
		return true
	default:
		return false
	}
}

// 评测相关的候选审计动作扩展词：started 记录 ready→evaluating 的启动迁移，
// 三个 evaluated 动作分别记录对应结论态的闭合；作废终态复用既有 invalidated。
const (
	EvolutionAuditEvaluationStarted      = "evaluation_started"
	EvolutionAuditEvaluatedPassed        = "evaluation_passed"
	EvolutionAuditEvaluatedFailed        = "evaluation_failed"
	EvolutionAuditEvaluationInconclusive = "evaluation_inconclusive"
)

// EvaluationID 由候选标识、冻结指纹组与比较器版本派生确定性标识：
// sha256("cairn-evaluation-v1\x00" || canonical_json(五字段))。同一输入恒得
// 同一 ID，崩溃恢复与重复启动都以此为幂等锚点。任一输入为空即拒绝派生，
// 不给残缺事实生成可用身份。
func EvaluationID(candidateID, evalSetFingerprint, baselineConfigFingerprint, variantConfigFingerprint, comparatorVersion string) (string, error) {
	if candidateID == "" || evalSetFingerprint == "" || baselineConfigFingerprint == "" || variantConfigFingerprint == "" || comparatorVersion == "" {
		return "", errors.New("evaluation identity requires candidate, eval set, both config fingerprints and a comparator version")
	}
	view := struct {
		CandidateID               string `json:"candidate_id"`
		EvalSetFingerprint        string `json:"eval_set_fingerprint"`
		BaselineConfigFingerprint string `json:"baseline_config_fingerprint"`
		VariantConfigFingerprint  string `json:"variant_config_fingerprint"`
		ComparatorVersion         string `json:"comparator_version"`
	}{candidateID, evalSetFingerprint, baselineConfigFingerprint, variantConfigFingerprint, comparatorVersion}
	return canonicaljson.Fingerprint(evaluationIDPrefix, view)
}
