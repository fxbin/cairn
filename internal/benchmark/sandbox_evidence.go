// Package benchmark 提供本地评测任务编排与可验证结果投影。
//
// Author: fxbin
package benchmark

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

const sandboxEvidenceSchemaVersion = 1

// ErrSandboxEvidenceCorrupt 表示持久化的评测身份、摘要或运行证据无法通过重算校验。
var ErrSandboxEvidenceCorrupt = errors.New("sandbox benefit evidence is corrupt")

// corruptSandboxEvidence 保留具体失败原因，同时提供稳定的损坏分类供 CLI 映射。
func corruptSandboxEvidence(format string, arguments ...any) error {
	return fmt.Errorf("%w: %s", ErrSandboxEvidenceCorrupt, fmt.Sprintf(format, arguments...))
}

// SandboxArmEvidence 是单个评测臂的可复算整数收益事实，不包含任务正文、路径或模型输出。
type SandboxArmEvidence struct {
	Runs                 int   `json:"runs"`
	Successes            int   `json:"successes"`
	InputTokens          int64 `json:"input_tokens"`
	OutputTokens         int64 `json:"output_tokens"`
	CostUnits            int64 `json:"cost_units"`
	MedianWallMS         int64 `json:"median_wall_ms"`
	RepeatedExplorations int   `json:"repeated_explorations"`
	SecurityIncidents    int   `json:"security_incidents"`
	DoctorReplayBroken   int   `json:"doctor_replay_broken"`
	ToolPairingBreaks    int   `json:"tool_pairing_breaks"`
}

// SandboxBenefitEvidence 是经摘要、身份、完整性和比较器重算验证后的收益证据投影。
type SandboxBenefitEvidence struct {
	SchemaVersion             int                `json:"schema_version"`
	Verified                  bool               `json:"verified"`
	EvaluationID              string             `json:"evaluation_id"`
	CandidateID               string             `json:"candidate_id"`
	EvalSetFingerprint        string             `json:"eval_set_fingerprint"`
	BaselineConfigFingerprint string             `json:"baseline_config_fingerprint"`
	VariantConfigFingerprint  string             `json:"variant_config_fingerprint"`
	ComparatorVersion         string             `json:"comparator_version"`
	SuiteResultDigest         string             `json:"suite_result_digest"`
	ExpectedRuns              int                `json:"expected_runs"`
	CountedRuns               int                `json:"counted_runs"`
	Status                    string             `json:"status"`
	Trigger                   string             `json:"trigger,omitempty"`
	Baseline                  SandboxArmEvidence `json:"baseline"`
	Variant                   SandboxArmEvidence `json:"variant"`
}

// VerifySandboxBenefitEvidence 严格读取套件结果，绑定 Memory 评测行与冻结评测集，
// 重算比较器结论和两臂收益指标；任何摘要、身份或完整性漂移都失败关闭。
func VerifySandboxBenefitEvidence(stateRoot string, evaluation memory.Evaluation, evalSet sandboxeval.EvalSet) (SandboxBenefitEvidence, error) {
	if err := evaluation.Validate(); err != nil {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("invalid evaluation binding: %v", err)
	}
	if evaluation.Status == memory.EvaluationStatusRunning {
		return SandboxBenefitEvidence{}, errors.New("evaluation has not concluded")
	}
	expectedEvaluationID, err := memory.EvaluationID(evaluation.CandidateID, evaluation.EvalSetFingerprint, evaluation.BaselineConfigFingerprint, evaluation.VariantConfigFingerprint, evaluation.ComparatorVersion)
	if err != nil {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("recompute evaluation identity: %v", err)
	}
	if evaluation.ID != expectedEvaluationID {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("evaluation identity mismatch")
	}
	evalSetFingerprint, err := evalSet.Fingerprint()
	if err != nil {
		return SandboxBenefitEvidence{}, err
	}
	if evalSetFingerprint != evaluation.EvalSetFingerprint ||
		evalSet.ComparatorContract.ComparatorVersion != evaluation.ComparatorVersion ||
		evalSet.BaselineConfigFingerprint != evaluation.BaselineConfigFingerprint ||
		evalSet.VariantConfigFingerprint != evaluation.VariantConfigFingerprint {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("frozen eval set diverges from the evaluation binding")
	}
	resultPath := filepath.Join(evaluationDirectory(stateRoot, evaluation.ID), "suite-result.json")
	content, err := os.ReadFile(resultPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return SandboxBenefitEvidence{}, corruptSandboxEvidence("referenced suite result is missing")
		}
		return SandboxBenefitEvidence{}, fmt.Errorf("read suite result: %w", err)
	}
	digestBytes := sha256.Sum256(content)
	digest := "sha256:" + hex.EncodeToString(digestBytes[:])
	if digest != evaluation.SuiteResultDigest {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("suite result bytes diverge from the stored digest")
	}
	var result SandboxSuiteResult
	if err := decodeStrictSandboxJSON(content, &result); err != nil {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("decode suite result: %v", err)
	}
	if result.SchemaVersion != sandboxRecordSchemaVersion || result.EvaluationID != evaluation.ID || result.CandidateID != evaluation.CandidateID {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("suite result identity mismatch")
	}
	if result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("suite result has invalid timestamps")
	}
	seen := make(map[string]struct{}, len(result.Records))
	verdicts := make([]sandboxeval.SuiteVerdict, 0, len(result.Records))
	baseline := sandboxArmAccumulator{}
	variant := sandboxArmAccumulator{}
	for _, record := range result.Records {
		if err := validateSandboxRunRecord(record, evaluation.ID, evalSet); err != nil {
			return SandboxBenefitEvidence{}, corruptSandboxEvidence("invalid suite run record: %v", err)
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", record.Arm, record.TaskID, record.AttemptIndex)
		if _, duplicated := seen[key]; duplicated {
			return SandboxBenefitEvidence{}, corruptSandboxEvidence("suite result contains duplicate run identities")
		}
		seen[key] = struct{}{}
		if !record.Counted {
			continue
		}
		verdict := verdictFromSandboxRunRecord(record)
		verdicts = append(verdicts, verdict)
		target := &baseline
		if verdict.Arm == sandboxeval.ArmVariant {
			target = &variant
		}
		if err := target.add(verdict, evalSet); err != nil {
			return SandboxBenefitEvidence{}, corruptSandboxEvidence("aggregate suite run metrics: %v", err)
		}
	}
	if result.Status == sandboxeval.ConclusionInvalid && result.Trigger == "" {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("invalid suite result requires a trigger")
	}
	if evalSet.AttemptsPerTask > math.MaxInt/2 || len(evalSet.TaskIDs) > math.MaxInt/(evalSet.AttemptsPerTask*2) {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("expected run count overflows")
	}
	expectedRuns := len(evalSet.TaskIDs) * evalSet.AttemptsPerTask * 2
	conclusion, err := sandboxeval.JudgeSuite(evalSet, verdicts)
	if err != nil {
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("recompute suite result: %v", err)
	}
	if conclusion.Status != result.Status || conclusion.Trigger != result.Trigger || result.Status != evaluation.Status {
		if result.Status == sandboxeval.ConclusionInvalid && len(verdicts) != expectedRuns && result.Status == evaluation.Status {
			return SandboxBenefitEvidence{}, errors.New("invalid evaluation does not provide a complete comparable suite")
		}
		return SandboxBenefitEvidence{}, corruptSandboxEvidence("suite result diverges from its run evidence or evaluation conclusion")
	}
	if len(verdicts) != expectedRuns {
		return SandboxBenefitEvidence{}, errors.New("suite result does not contain a complete counted comparison")
	}
	return SandboxBenefitEvidence{
		SchemaVersion:             sandboxEvidenceSchemaVersion,
		Verified:                  true,
		EvaluationID:              evaluation.ID,
		CandidateID:               evaluation.CandidateID,
		EvalSetFingerprint:        evaluation.EvalSetFingerprint,
		BaselineConfigFingerprint: evaluation.BaselineConfigFingerprint,
		VariantConfigFingerprint:  evaluation.VariantConfigFingerprint,
		ComparatorVersion:         evaluation.ComparatorVersion,
		SuiteResultDigest:         digest,
		ExpectedRuns:              expectedRuns,
		CountedRuns:               len(verdicts),
		Status:                    conclusion.Status,
		Trigger:                   conclusion.Trigger,
		Baseline:                  baseline.evidence(),
		Variant:                   variant.evidence(),
	}, nil
}

// sandboxArmAccumulator 在验真期间累计单臂整数指标，不作为独立事实持久化。
type sandboxArmAccumulator struct {
	runs               int
	successes          int
	repeated           int
	securityIncidents  int
	doctorReplayBroken int
	toolPairingBreaks  int
	inputTokens        int64
	outputTokens       int64
	costUnits          int64
	wallTimes          []int64
}

// add 将一个已校验、已计数的 Run 事实并入单臂聚合，任何整数溢出都失败关闭。
func (accumulator *sandboxArmAccumulator) add(verdict sandboxeval.SuiteVerdict, evalSet sandboxeval.EvalSet) error {
	cost, err := sandboxeval.ComputeRunCostUnits(verdict.InputTokens, verdict.OutputTokens, evalSet.PricingSnapshot.InputPriceMicrousdPerMillionTokens, evalSet.PricingSnapshot.OutputPriceMicrousdPerMillionTokens)
	if err != nil {
		return err
	}
	if accumulator.inputTokens > math.MaxInt64-verdict.InputTokens ||
		accumulator.outputTokens > math.MaxInt64-verdict.OutputTokens ||
		accumulator.costUnits > math.MaxInt64-cost ||
		accumulator.repeated > math.MaxInt-verdict.RepeatedExplorations ||
		accumulator.securityIncidents > math.MaxInt-verdict.SecurityIncidents ||
		accumulator.toolPairingBreaks > math.MaxInt-verdict.ToolPairingBreaks {
		return errors.New("sandbox evidence metric aggregation overflows")
	}
	accumulator.runs++
	if verdict.Success {
		accumulator.successes++
	}
	accumulator.inputTokens += verdict.InputTokens
	accumulator.outputTokens += verdict.OutputTokens
	accumulator.costUnits += cost
	accumulator.repeated += verdict.RepeatedExplorations
	accumulator.securityIncidents += verdict.SecurityIncidents
	if verdict.DoctorReplayBroken {
		accumulator.doctorReplayBroken++
	}
	accumulator.toolPairingBreaks += verdict.ToolPairingBreaks
	accumulator.wallTimes = append(accumulator.wallTimes, verdict.WallTimeMS)
	return nil
}

// evidence 生成单臂只读投影；墙钟样本按比较器合同取奇数样本的中位数。
func (accumulator sandboxArmAccumulator) evidence() SandboxArmEvidence {
	walls := append([]int64(nil), accumulator.wallTimes...)
	sort.Slice(walls, func(left, right int) bool { return walls[left] < walls[right] })
	median := int64(0)
	if len(walls) > 0 {
		median = walls[len(walls)/2]
	}
	return SandboxArmEvidence{
		Runs:                 accumulator.runs,
		Successes:            accumulator.successes,
		InputTokens:          accumulator.inputTokens,
		OutputTokens:         accumulator.outputTokens,
		CostUnits:            accumulator.costUnits,
		MedianWallMS:         median,
		RepeatedExplorations: accumulator.repeated,
		SecurityIncidents:    accumulator.securityIncidents,
		DoctorReplayBroken:   accumulator.doctorReplayBroken,
		ToolPairingBreaks:    accumulator.toolPairingBreaks,
	}
}
