// [INPUT]: 依赖Memory评测合同、冻结资产与受控执行入口。
// [OUTPUT]: 提供恢复记录、套件结论重算与原子结果提交。
// [POS]: 带外评测工具链，不进入最小Agent内核。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package benchmark 提供隔离评测与冻结证据。
// Author: fxbin
package benchmark

import (
	"bytes"

	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"strings"
	"time"

	"github.com/fxbin/cairn/internal/contract"

	"github.com/fxbin/cairn/internal/capabilities/memory"
	"github.com/fxbin/cairn/internal/runtime/sandboxeval"
)

// sandboxRecordFileName 将调度步映射为评测目录内稳定且可读的记录文件名；
// 权威身份仍以文件内 run_id 字段为准。
func sandboxRecordFileName(step SandboxPairStep) string {
	return safePathComponent(step.TaskID) + fmt.Sprintf("-a%02d-%s.json", step.Try, sandboxArmTag(step.Arm))
}

// decodeStrictSandboxJSON 解码单一 JSON 文档并拒绝未知字段与尾随内容，恢复
// 路径不得接受比首次写入合同更宽松的输入。
func decodeStrictSandboxJSON(content []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("sandbox JSON contains trailing content")
		}
		return err
	}
	return nil
}

// validateSandboxRunRecord 核对恢复记录的 schema、确定性身份、时间与闭合
// 指标；任何错位都保留 running 状态并要求人工处理，绝不猜测归属。
func validateSandboxRunRecord(record SandboxRunRecord, evaluationID string, evalSet sandboxeval.EvalSet) error {
	if record.SchemaVersion != sandboxRecordSchemaVersion {
		return fmt.Errorf("sandbox run record schema %d cannot satisfy trusted success schema %d; use a fresh evaluation state root", record.SchemaVersion, sandboxRecordSchemaVersion)
	}
	if record.EvaluationID != evaluationID {
		return errors.New("sandbox run record evaluation identity mismatch")
	}
	arm := sandboxeval.SuiteArm(record.Arm)
	if arm != sandboxeval.ArmBaseline && arm != sandboxeval.ArmVariant {
		return fmt.Errorf("sandbox run record %q has an invalid arm", record.RunID)
	}
	if record.AttemptIndex < 1 || record.AttemptIndex > evalSet.AttemptsPerTask {
		return fmt.Errorf("sandbox run record %q has an invalid attempt index", record.RunID)
	}
	knownTask := false
	for _, taskID := range evalSet.TaskIDs {
		if taskID == record.TaskID {
			knownTask = true
			break
		}
	}
	if !knownTask {
		return fmt.Errorf("sandbox run record %q references an unfrozen task", record.RunID)
	}
	expectedRunID, err := sandboxeval.SandboxRunID(evaluationID, arm, record.TaskID, record.AttemptIndex)
	if err != nil || expectedRunID != record.RunID {
		return fmt.Errorf("sandbox run record identity mismatch")
	}
	if record.StartedAt.IsZero() || record.FinishedAt.IsZero() || record.FinishedAt.Before(record.StartedAt) {
		return fmt.Errorf("sandbox run record %q has invalid timestamps", record.RunID)
	}
	if record.LeakScan.Outcome != sandboxeval.LeakClean && record.LeakScan.Outcome != sandboxeval.LeakDetected {
		return fmt.Errorf("sandbox run record %q has an invalid leak scan outcome", record.RunID)
	}
	hasLeakHits := false
	for surface, count := range record.LeakScan.Hits {
		if strings.TrimSpace(surface) == "" || count <= 0 {
			return fmt.Errorf("sandbox run record %q has invalid leak scan hits", record.RunID)
		}
		hasLeakHits = true
	}
	if record.LeakScan.Outcome == sandboxeval.LeakClean && hasLeakHits {
		return fmt.Errorf("sandbox run record %q reports clean with leak hits", record.RunID)
	}
	if record.LeakScan.Outcome == sandboxeval.LeakClean && record.SecurityIncidents != 0 {
		return fmt.Errorf("sandbox run record %q reports security incidents without leak evidence", record.RunID)
	}
	if record.LeakScan.Outcome == sandboxeval.LeakDetected {
		if !hasLeakHits {
			return fmt.Errorf("sandbox run record %q reports a leak without hit evidence", record.RunID)
		}
		if record.SecurityIncidents != 1 || record.FailureClass != sandboxTriggerCredentialLeak {
			return fmt.Errorf("sandbox run record %q has inconsistent credential leak evidence", record.RunID)
		}
		if record.Counted {
			return fmt.Errorf("sandbox run record %q counts a credential leak as comparable evidence", record.RunID)
		}
	}
	if record.Success && (strings.TrimSpace(record.FailureClass) != "" || strings.TrimSpace(record.Error) != "" || record.SecurityIncidents > 0) {
		return fmt.Errorf("sandbox run record %q reports success with failure or security evidence", record.RunID)
	}
	if record.Success && !record.Counted {
		return fmt.Errorf("sandbox run record %q reports an uncounted success", record.RunID)
	}
	if record.Counted {
		if !record.ReplayConsistent || record.LeakScan.Outcome != sandboxeval.LeakClean {
			return fmt.Errorf("sandbox run record %q counts incomplete evidence", record.RunID)
		}
		if !record.Success && strings.TrimSpace(record.FailureClass) == "" {
			return fmt.Errorf("sandbox run record %q counts a failure without a failure class", record.RunID)
		}
		if !record.Success && record.FailureClass != verificationFailedClass && !contract.IsBudgetFailure(record.FailureClass) {
			return fmt.Errorf("sandbox run record %q counts an unsupported failure class", record.RunID)
		}
	} else if recoveredInvalidTrigger(record) == "" &&
		record.FailureClass != sandboxStopProviderFailure &&
		!contract.IsKnownTerminalFailureClass(record.FailureClass) {
		return fmt.Errorf("sandbox run record %q has an unsupported uncounted failure class", record.RunID)
	}
	if record.InputTokens < 0 || record.OutputTokens < 0 || record.WallTimeMS < 0 || record.ToolPairingBreaks < 0 || record.RepeatedExplorations < 0 || record.SecurityIncidents < 0 {
		return fmt.Errorf("sandbox run record %q has negative metrics", record.RunID)
	}
	return nil
}

// loadRecoveredSandboxRunRecords 只装载调度中存在的稳定文件名并逐条验证；额外
// 文件、目录、符号链接或重复身份都视为恢复表面损坏。
func loadRecoveredSandboxRunRecords(evaluationDir, evaluationID string, evalSet sandboxeval.EvalSet, steps []SandboxPairStep) (map[string]SandboxRunRecord, error) {
	runsDirectory := filepath.Join(evaluationDir, "runs")
	entries, err := os.ReadDir(runsDirectory)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]SandboxRunRecord{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read recovered runs directory: %w", err)
	}
	expected := make(map[string]SandboxPairStep, len(steps))
	for _, step := range steps {
		expected[sandboxRecordFileName(step)] = step
	}
	recovered := make(map[string]SandboxRunRecord, len(entries))
	for _, entry := range entries {
		step, allowed := expected[entry.Name()]
		if !allowed || entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("recovered runs directory contains an unexpected entry")
		}
		content, err := os.ReadFile(filepath.Join(runsDirectory, entry.Name()))
		if err != nil {
			return nil, fmt.Errorf("read recovered sandbox run record: %w", err)
		}
		var record SandboxRunRecord
		if err := decodeStrictSandboxJSON(content, &record); err != nil {
			return nil, fmt.Errorf("decode recovered sandbox run record: %w", err)
		}
		if err := validateSandboxRunRecord(record, evaluationID, evalSet); err != nil {
			return nil, err
		}
		if record.TaskID != step.TaskID || record.AttemptIndex != step.Try || record.Arm != string(step.Arm) {
			return nil, fmt.Errorf("sandbox run record does not match its stable filename")
		}
		recovered[entry.Name()] = record
	}
	return recovered, nil
}

// validatePersistedSuiteStopFields 将停止原因及明细限制为执行器真实可产生的封闭
// 组合；明细不能脱离原因单独出现，也不能携带任意错误文本。
func validatePersistedSuiteStopFields(reason, detail string) error {
	switch reason {
	case "":
		if detail != "" {
			return errors.New("persisted suite result has stop detail without a stop reason")
		}
	case sandboxStopLeaseExhausted, sandboxStopContextCancelled:
		if detail != "" {
			return fmt.Errorf("persisted suite result stop reason %q does not allow detail", reason)
		}
	case sandboxStopProviderFailure:
		if detail == sandboxStopProviderFailure {
			return nil
		}
		if detail == "" || !contract.IsKnownTerminalFailureClass(detail) {
			return errors.New("persisted suite result has an invalid provider stop detail")
		}
	case sandboxStopEnvironmentFailure:
		switch detail {
		case "preparation_prepare", "preparation_verifier", "preparation_bootstrap",
			"preparation_sandbox", "preparation_config", sandboxStopDetailTaskImageDigest:
			return nil
		default:
			return errors.New("persisted suite result has an invalid environment stop detail")
		}
	default:
		return fmt.Errorf("persisted suite result has an unknown stop reason %q", reason)
	}
	return nil
}

// judgePersistedSuite 复用生产判定并把比较器不可计算错误映射回执行器持久化
// 使用的稳定 invalid 触发名，避免恢复路径把合法留档误判为损坏。
func judgePersistedSuite(evalSet sandboxeval.EvalSet, verdicts []sandboxeval.SuiteVerdict) (sandboxeval.Conclusion, error) {
	conclusion, err := sandboxeval.JudgeSuite(evalSet, verdicts)
	if err != nil {
		return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: sandboxTriggerComparisonNotComputable}, nil
	}
	return conclusion, nil
}

// recomputePersistedSuiteConclusion 按冻结调度顺序重放记录与租约账面，确定套件
// 唯一可接受的结论。记录必须构成调度前缀；invalid 只能来自预筛、下一步启动前
// 的封闭触发、记录内证据、成本计算或比较器，不能由结果正文自行声明。
func recomputePersistedSuiteConclusion(options SandboxExecutorOptions, result SandboxSuiteResult, prescreenFailed bool) (sandboxeval.Conclusion, error) {
	steps, err := SandboxSchedule(options.EvalSet.TaskIDs, options.EvalSet.AttemptsPerTask)
	if err != nil {
		return sandboxeval.Conclusion{}, fmt.Errorf("rebuild sandbox schedule: %w", err)
	}
	if len(result.Records) > len(steps) {
		return sandboxeval.Conclusion{}, errors.New("persisted suite result contains more records than the frozen schedule")
	}
	if err := validatePersistedSuiteStopFields(result.StopReason, result.StopDetail); err != nil {
		return sandboxeval.Conclusion{}, err
	}
	if prescreenFailed {
		if len(result.Records) != 0 || result.StopReason != "" {
			return sandboxeval.Conclusion{}, errors.New("offline prescreen invalidation cannot contain run or stop evidence")
		}
		return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: sandboxTriggerOfflinePrescreenFailed}, nil
	}

	verdicts := make([]sandboxeval.SuiteVerdict, 0, len(result.Records))
	ledger := &leaseLedger{}
	for index, record := range result.Records {
		step := steps[index]
		if record.TaskID != step.TaskID || record.AttemptIndex != step.Try || record.Arm != string(step.Arm) {
			return sandboxeval.Conclusion{}, errors.New("persisted suite records do not form the frozen schedule prefix")
		}
		lastRecord := index == len(result.Records)-1
		if record.Counted {
			verdict := verdictFromSandboxRunRecord(record)
			verdicts = append(verdicts, verdict)
			units, costErr := sandboxeval.ComputeRunCostUnits(verdict.InputTokens, verdict.OutputTokens,
				int64(options.EvalSet.PricingSnapshot.InputPriceMicrousdPerMillionTokens),
				int64(options.EvalSet.PricingSnapshot.OutputPriceMicrousdPerMillionTokens))
			if costErr != nil {
				if !lastRecord || result.StopReason != "" {
					return sandboxeval.Conclusion{}, errors.New("persisted suite result contradicts its cost overflow boundary")
				}
				return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: sandboxTriggerCostUnitsOverflow}, nil
			}
			ledger.chargeUnits(units)
			ledger.chargeUsage(verdict.InputTokens, verdict.OutputTokens, 1)
			if ledger.exceedsLease(options.Lease) {
				if !lastRecord || result.StopReason != sandboxStopLeaseExhausted {
					return sandboxeval.Conclusion{}, errors.New("persisted suite result contradicts its lease exhaustion boundary")
				}
				return judgePersistedSuite(options.EvalSet, verdicts)
			}
			continue
		}

		if !lastRecord {
			return sandboxeval.Conclusion{}, errors.New("persisted suite result continues after an uncounted run")
		}
		if trigger := recoveredInvalidTrigger(record); trigger != "" {
			if result.StopReason != "" {
				return sandboxeval.Conclusion{}, errors.New("invalid run evidence cannot carry an independent stop reason")
			}
			return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: trigger}, nil
		}
		if result.StopReason != sandboxStopProviderFailure || result.StopDetail != record.FailureClass {
			return sandboxeval.Conclusion{}, errors.New("persisted provider stop does not match its terminal run record")
		}
		return judgePersistedSuite(options.EvalSet, verdicts)
	}
	if len(result.Records) == len(steps) {
		if result.StopReason != "" {
			return sandboxeval.Conclusion{}, errors.New("complete persisted suite result carries an unexplained stop reason")
		}
		return judgePersistedSuite(options.EvalSet, verdicts)
	}

	switch result.StopReason {
	case sandboxStopContextCancelled, sandboxStopEnvironmentFailure:
		return judgePersistedSuite(options.EvalSet, verdicts)
	case "":
		if result.Trigger != "" {
			return sandboxeval.Conclusion{}, errors.New("persisted invalid result lacks independently replayable evidence")
		}
		if len(result.Records) == 0 {
			return judgePersistedSuite(options.EvalSet, verdicts)
		}
		return sandboxeval.Conclusion{}, errors.New("incomplete persisted suite result lacks deterministic stop or invalid evidence")
	default:
		return sandboxeval.Conclusion{}, errors.New("persisted suite stop is inconsistent with its record prefix")
	}
}

// loadPersistedSuiteResult 在崩溃发生于“套件结果落盘、Memory 终态提交”之间
// 时恢复不可变结论；正文身份不符时失败关闭，绝不覆盖既有结果。
func loadPersistedSuiteResult(options SandboxExecutorOptions, evaluationID string, prescreenFailure ...bool) (SandboxSuiteResult, string, string, bool, error) {
	resultPath := filepath.Join(evaluationDirectory(options.StateRoot, evaluationID), "suite-result.json")
	content, err := os.ReadFile(resultPath)
	if errors.Is(err, fs.ErrNotExist) {
		return SandboxSuiteResult{}, "", "", false, nil
	}
	if err != nil {
		return SandboxSuiteResult{}, "", "", false, err
	}
	var result SandboxSuiteResult
	if err := decodeStrictSandboxJSON(content, &result); err != nil {
		return SandboxSuiteResult{}, "", "", false, err
	}
	if result.SchemaVersion != sandboxRecordSchemaVersion || result.EvaluationID != evaluationID || result.CandidateID != options.CandidateID {
		return SandboxSuiteResult{}, "", "", false, errors.New("persisted suite result identity mismatch")
	}
	if !memory.ValidEvaluationStatus(result.Status) || result.Status == memory.EvaluationStatusRunning {
		return SandboxSuiteResult{}, "", "", false, errors.New("persisted suite result has an invalid conclusion")
	}
	if result.StartedAt.IsZero() || result.FinishedAt.IsZero() || result.FinishedAt.Before(result.StartedAt) {
		return SandboxSuiteResult{}, "", "", false, errors.New("persisted suite result has invalid timestamps")
	}
	seen := make(map[string]struct{}, len(result.Records))
	for _, record := range result.Records {
		if err := validateSandboxRunRecord(record, evaluationID, options.EvalSet); err != nil {
			return SandboxSuiteResult{}, "", "", false, err
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", record.Arm, record.TaskID, record.AttemptIndex)
		if _, duplicated := seen[key]; duplicated {
			return SandboxSuiteResult{}, "", "", false, errors.New("persisted suite result contains duplicate run identities")
		}
		seen[key] = struct{}{}
	}
	prescreenFailed := len(prescreenFailure) > 0 && prescreenFailure[0]
	recomputed, err := recomputePersistedSuiteConclusion(options, result, prescreenFailed)
	if err != nil {
		return SandboxSuiteResult{}, "", "", false, fmt.Errorf("recompute persisted suite result: %w", err)
	}
	if recomputed.Status != result.Status || recomputed.Trigger != result.Trigger {
		return SandboxSuiteResult{}, "", "", false, errors.New("persisted suite result diverges from its run and stop evidence")
	}
	digest := sha256.Sum256(content)
	return result, resultPath, "sha256:" + hex.EncodeToString(digest[:]), true, nil
}

// persistSuiteResult 把套件结论原子落盘并读回计算字节摘要：Memory 侧只保存
// 该 sha256 引用，读回环节保证留存正文与其引用不可静默漂移。
func (executor SandboxExecutor) persistSuiteResult(options SandboxExecutorOptions, evaluationID string, startedAt time.Time, conclusion sandboxeval.Conclusion, stopReason, stopDetail string, records []SandboxRunRecord) (string, string, error) {
	result := SandboxSuiteResult{
		SchemaVersion: sandboxRecordSchemaVersion,
		EvaluationID:  evaluationID,
		CandidateID:   options.CandidateID,
		StartedAt:     startedAt,
		FinishedAt:    executor.now(),
		Status:        conclusion.Status,
		Trigger:       conclusion.Trigger,
		StopReason:    stopReason,
		StopDetail:    stopDetail,
		Records:       append([]SandboxRunRecord(nil), records...),
	}
	resultPath := filepath.Join(evaluationDirectory(options.StateRoot, evaluationID), "suite-result.json")
	if err := writeJSONExclusive(resultPath, filepath.Join(evaluationDirectory(options.StateRoot, evaluationID), ".tmp"), result); err != nil {
		return "", "", err
	}
	content, readErr := os.ReadFile(resultPath)
	if readErr != nil {
		return "", "", fmt.Errorf("read back suite result for digest: %w", readErr)
	}
	digest := sha256.Sum256(content)
	return resultPath, "sha256:" + hex.EncodeToString(digest[:]), nil
}
