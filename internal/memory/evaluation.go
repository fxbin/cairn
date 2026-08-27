// Package memory 的 Evaluation 部分定义候选评测的身份与状态合同：同一
// 候选只绑定一个 EvalSet 并只允许一个 Evaluation 身份；评测一旦进入终态
// 不得重跑或改写。身份由版本化域前缀与规范化五字段派生，确定性即幂等。
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

// evaluationSchemaV1 是 Evaluation 行的当前合同版本。
const evaluationSchemaV1 = 1

// EvaluationSchemaVersionV1 是面向编排方的当前评测行 schema 版本导出常量；
// 构造新评测行必须经此表达版本事实，不得散落字面量。
const EvaluationSchemaVersionV1 = evaluationSchemaV1

// evaluationIDPrefix 是 Evaluation 标识的版本化域前缀。
const evaluationIDPrefix = "cairn-evaluation-v1"

// sourceInvalidationDigestPrefix 是运行中评测因来源失效被受控闭合时的证据
// 摘要域；摘要只绑定既有候选与评测身份，不携带自由文本或本机路径。
const sourceInvalidationDigestPrefix = "cairn-evaluation-source-invalidation-v1"

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

// Evaluation 是一次候选评测的持久化事实：五元组身份冻结输入，状态只允许
// 从 running 单向进入一个结论态。SuiteResultDigest 通常引用本地套件结果，
// 来源失效时则记录确定性失效证据摘要；FinishedAt 零值表示评测尚未闭合。
type Evaluation struct {
	ID                        string    `json:"evaluation_id"`
	SchemaVersion             int       `json:"schema_version"`
	CandidateID               string    `json:"candidate_id"`
	EvalSetFingerprint        string    `json:"eval_set_fingerprint"`
	BaselineConfigFingerprint string    `json:"baseline_config_fingerprint"`
	VariantConfigFingerprint  string    `json:"variant_config_fingerprint"`
	ComparatorVersion         string    `json:"comparator_version"`
	Status                    string    `json:"status"`
	SuiteResultDigest         string    `json:"suite_result_digest,omitempty"`
	StartedAt                 time.Time `json:"started_at"`
	FinishedAt                time.Time `json:"finished_at,omitempty"`
}

// EvaluationBinding 是启动事务必须与候选不可变事实核对的三元组；它不另行
// 持久化，避免产生第二事实源，只作为同一事务内的比较输入。
type EvaluationBinding struct {
	BaselineFingerprint         string
	VariantSpecSHA256           string
	CandidateVariantFingerprint string
}

// Validate 校验事务绑定三元组均为闭合的 sha256 摘要。
func (binding EvaluationBinding) Validate() error {
	if !DigestHexShape(binding.BaselineFingerprint) || !DigestHexShape(binding.VariantSpecSHA256) || !DigestHexShape(binding.CandidateVariantFingerprint) {
		return errors.New("evaluation candidate binding requires three sha256 digests")
	}
	return nil
}

// Validate 校验评测行的结构合同：schema 版本、身份词表、状态合法性与
// running/结论两态的完整性约束（running 不得携带结论产物；结论态必须齐全）。
func (evaluation Evaluation) Validate() error {
	if evaluation.SchemaVersion != evaluationSchemaV1 {
		return fmt.Errorf("evaluation schema_version %d is not supported", evaluation.SchemaVersion)
	}
	if evaluation.ID == "" || evaluation.CandidateID == "" {
		return errors.New("evaluation identity fields must not be empty")
	}
	for name, value := range map[string]string{
		"eval_set_fingerprint":        evaluation.EvalSetFingerprint,
		"baseline_config_fingerprint": evaluation.BaselineConfigFingerprint,
		"variant_config_fingerprint":  evaluation.VariantConfigFingerprint,
	} {
		if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
			return fmt.Errorf("evaluation %s must be a sha256 digest", name)
		}
	}
	if evaluation.ComparatorVersion == "" {
		return errors.New("evaluation comparator version must not be empty")
	}
	if !ValidEvaluationStatus(evaluation.Status) {
		return fmt.Errorf("evaluation status %q is not in the closed lexicon", evaluation.Status)
	}
	if evaluation.StartedAt.IsZero() {
		return errors.New("evaluation started_at is required")
	}
	if evaluation.Status == EvaluationStatusRunning {
		if evaluation.SuiteResultDigest != "" || !evaluation.FinishedAt.IsZero() {
			return errors.New("running evaluation cannot carry conclusion artifacts")
		}
		return nil
	}
	if !DigestHexShape(evaluation.SuiteResultDigest) {
		return errors.New("concluded evaluation requires a sha256 suite result digest")
	}
	if evaluation.FinishedAt.IsZero() || evaluation.FinishedAt.Before(evaluation.StartedAt) {
		return errors.New("concluded evaluation requires a finished_at no earlier than started_at")
	}
	return nil
}

// DigestHexShape 报告字符串是否为形态完整的 sha256 摘要引用；存储层入口
// 前置校验与行级合同校验共用同一形态结论。
func DigestHexShape(value string) bool {
	return strings.HasPrefix(value, "sha256:") && len(value) == len("sha256:")+64
}

// CandidateStatusForEvaluation 返回评测状态对应的候选终态：三个业务结论各
// 有专属候选态，invalid 映射为候选 invalidated（作废即安全事件终态），
// running 对应 evaluating。
func CandidateStatusForEvaluation(status string) (string, error) {
	switch status {
	case EvaluationStatusRunning:
		return CandidateStateEvaluating, nil
	case EvaluationStatusPassed:
		return CandidateStateEvaluatedPassed, nil
	case EvaluationStatusFailed:
		return CandidateStateEvaluatedFailed, nil
	case EvaluationStatusInconclusive:
		return CandidateStateInconclusive, nil
	case EvaluationStatusInvalid:
		return CandidateStateInvalidated, nil
	default:
		return "", fmt.Errorf("unknown evaluation status %q", status)
	}
}

// ConclusionAuditActionForEvaluation 返回评测结论对应的候选审计动作；
// invalid 复用作废动作，与既有作废词表保持一致。
func ConclusionAuditActionForEvaluation(status string) (string, error) {
	switch status {
	case EvaluationStatusPassed:
		return EvolutionAuditEvaluatedPassed, nil
	case EvaluationStatusFailed:
		return EvolutionAuditEvaluatedFailed, nil
	case EvaluationStatusInconclusive:
		return EvolutionAuditEvaluationInconclusive, nil
	case EvaluationStatusInvalid:
		return EvolutionAuditInvalidated, nil
	default:
		return "", fmt.Errorf("evaluation status %q has no conclusion audit action", status)
	}
}

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

// SourceInvalidationDigest 为来源失效导致的运行中评测闭合生成确定性证据摘要；
// 它可由数据库既有身份重算，供恢复路径与全库检查共同核验。
func SourceInvalidationDigest(candidateID, evaluationID string) (string, error) {
	if candidateID == "" || evaluationID == "" {
		return "", errors.New("source invalidation digest requires candidate and evaluation identities")
	}
	view := struct {
		CandidateID  string `json:"candidate_id"`
		EvaluationID string `json:"evaluation_id"`
	}{CandidateID: candidateID, EvaluationID: evaluationID}
	return canonicaljson.Fingerprint(sourceInvalidationDigestPrefix, view)
}
