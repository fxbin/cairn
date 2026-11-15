// [INPUT]: 依赖Memory评测合同、冻结资产与受控执行入口。
// [OUTPUT]: 提供交错调度、单次运行、证据提取与租约计账。
// [POS]: 带外评测工具链，不进入最小Agent内核。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package benchmark 提供隔离评测与冻结证据。
// Author: fxbin
package benchmark

import (
	"context"

	"errors"
	"fmt"

	"io/fs"
	"os"
	"path/filepath"

	"strings"

	"github.com/fxbin/cairn/internal/app"

	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"

	"github.com/fxbin/cairn/internal/runtime/sandboxeval"
)

// scheduleAbort 承载必须以 invalid 收敛的即时停止信号：触发名进入套件结果，
// 底层错误只回传编排调用方，不落盘。
type scheduleAbort struct {
	trigger string
	err     error
}

// Error 返回触发名与底层原因的组合描述。
func (abort *scheduleAbort) Error() string {
	if abort.err == nil {
		return "sandbox suite aborted: " + abort.trigger
	}
	return "sandbox suite aborted at " + abort.trigger + ": " + abort.err.Error()
}

// Unwrap 暴露底层错误供 errors.Is/As 判别。
func (abort *scheduleAbort) Unwrap() error { return abort.err }

// runSchedule 按成对交错顺序驱动全部计划 Run：每步即时准备即时执行，正常
// 样本累计 verdict 与 record。停止信号只中断调度并把剩余样本留空——套件交
// 由比较器的完备性检查自然收敛为 inconclusive；遇到立即短路信号则把结论固
// 定为 invalid。
func (executor SandboxExecutor) runSchedule(ctx context.Context, options SandboxExecutorOptions, precheck precheckResult) (sandboxeval.Conclusion, string, string, []SandboxRunRecord, []sandboxeval.SuiteVerdict, error) {
	conclusion := sandboxeval.Conclusion{}
	stopReason, stopDetail := "", ""
	records := make([]SandboxRunRecord, 0)
	verdicts := make([]sandboxeval.SuiteVerdict, 0)
	steps, err := SandboxSchedule(options.EvalSet.TaskIDs, options.EvalSet.AttemptsPerTask)
	if err != nil {
		return conclusion, stopReason, stopDetail, records, verdicts, err
	}
	tasks, err := options.Pack.SelectedTasks(options.EvalSet.TaskIDs)
	if err != nil {
		return conclusion, stopReason, stopDetail, records, verdicts, err
	}
	taskByID := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		taskByID[task.ID] = task
	}
	evaluationDir := evaluationDirectory(options.StateRoot, precheck.evaluationID)
	if err := os.MkdirAll(filepath.Join(evaluationDir, "runs"), 0o700); err != nil {
		return conclusion, stopReason, stopDetail, records, verdicts, fmt.Errorf("create runs directory: %w", err)
	}
	recovered, err := loadRecoveredSandboxRunRecords(evaluationDir, precheck.evaluationID, options.EvalSet, steps)
	if err != nil {
		return conclusion, stopReason, stopDetail, records, verdicts, err
	}
	ledger := &leaseLedger{}
	stop := &scheduleStop{}
	for _, step := range steps {
		if ctx.Err() != nil {
			stopReason = sandboxStopContextCancelled
			break
		}
		if recoveredRecord, found := recovered[sandboxRecordFileName(step)]; found {
			records = append(records, recoveredRecord)
			if recoveredRecord.Counted {
				verdict := verdictFromSandboxRunRecord(recoveredRecord)
				verdicts = append(verdicts, verdict)
				units, costErr := sandboxeval.ComputeRunCostUnits(verdict.InputTokens, verdict.OutputTokens,
					int64(options.EvalSet.PricingSnapshot.InputPriceMicrousdPerMillionTokens),
					int64(options.EvalSet.PricingSnapshot.OutputPriceMicrousdPerMillionTokens))
				if costErr != nil {
					return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: sandboxTriggerCostUnitsOverflow}, stopReason, stopDetail, records, verdicts, costErr
				}
				ledger.chargeUnits(units)
				ledger.chargeUsage(verdict.InputTokens, verdict.OutputTokens, 1)
				if ledger.exceedsLease(options.Lease) {
					stopReason = sandboxStopLeaseExhausted
					break
				}
				continue
			}
			if trigger := recoveredInvalidTrigger(recoveredRecord); trigger != "" {
				return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: trigger}, stopReason, stopDetail, records, verdicts, nil
			}
			stopReason = sandboxStopProviderFailure
			stopDetail = recoveredRecord.FailureClass
			break
		}
		record, verdict, stepAbort := executor.executeScheduledStep(ctx, options, precheck, taskByID[step.TaskID], step, ledger, evaluationDir, stop)
		if record != nil {
			if writeErr := writeJSONExclusive(filepath.Join(evaluationDir, "runs", sandboxRecordFileName(step)), filepath.Join(evaluationDir, ".tmp"), record); writeErr != nil {
				return conclusion, stopReason, stopDetail, records, verdicts, fmt.Errorf("persist sandbox run record: %w", writeErr)
			}
			records = append(records, *record)
		}
		if verdict != nil {
			verdicts = append(verdicts, *verdict)
		}
		if stepAbort != nil {
			return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: stepAbort.trigger}, "", "", records, verdicts, nil
		}
		if stop.reason != "" {
			stopReason, stopDetail = stop.reason, stop.detail
			break
		}
		if record != nil && ledger.exceedsLease(options.Lease) {
			stopReason = sandboxStopLeaseExhausted
			break
		}
	}
	conclusion, err = sandboxeval.JudgeSuite(options.EvalSet, verdicts)
	if err != nil {
		// 受检运算溢出、证据臂归属漂移等属于可计算性合同破坏：按无效闭合而
		// 不是遗留 running，让套件终态始终合法可读；底层诊断随错误回传调用方。
		return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: sandboxTriggerComparisonNotComputable}, stopReason, stopDetail, records, verdicts, fmt.Errorf("judge suite: %w", err)
	}
	return conclusion, stopReason, stopDetail, records, verdicts, nil
}

// executeScheduledStep 准备并执行单个调度步：record 始终如实留档，verdict
// 只在产生可计入评分的有效终态样本时非空；stepAbort 表示隔离或证据合同破坏
// 必须 invalid 闭合。环境准备失败与 Provider 无终态不产 abort，经停止信号出
// 口中断调度并收敛为不确定；明细只携带步骤级分类词。租约账面由循环层在每步
// 后统一判定，超出的瞬时风险由租约合同强制的服务端配额兜底。
func (executor SandboxExecutor) executeScheduledStep(ctx context.Context, options SandboxExecutorOptions, precheck precheckResult, task Task, step SandboxPairStep, ledger *leaseLedger, evaluationDir string, stop *scheduleStop) (*SandboxRunRecord, *sandboxeval.SuiteVerdict, *scheduleAbort) {
	armConfig := precheck.baselineConfig
	if step.Arm == sandboxeval.ArmVariant {
		armConfig = precheck.variantConfig
	}
	runID, err := sandboxeval.SandboxRunID(precheck.evaluationID, step.Arm, step.TaskID, step.Try)
	if err != nil {
		return nil, nil, &scheduleAbort{trigger: sandboxTriggerRunIdentityDerivation, err: err}
	}
	runOptions := RunOptions{
		Pack:           options.Pack,
		Attempts:       1,
		Configuration:  armConfig,
		WorkspacesRoot: options.WorkspacesRoot,
		StateRoot:      options.StateRoot,
		PrivateRoot:    options.PrivateRoot,
		CairnRoot:      options.CairnRoot,
		PrepareTimeout: options.PrepareTimeout,
		PrepareProxy:   options.PrepareProxy,
	}
	report := SuiteResult{SuiteRunID: suiteSegmentForArm(precheck.evaluationID, step.Arm), TaskPackFingerprint: options.Pack.Fingerprint()}
	prepared, preparation := executor.prepareAgentAttempt(ctx, runOptions, report, task, step.Try)
	if preparation.Outcome != "prepared" {
		stop.reason = sandboxStopEnvironmentFailure
		stop.detail = preparation.FailureClass
		return nil, nil, nil
	}
	frozenContract, found := sandboxTaskContract(options.EvalSet, task.ID)
	if !found {
		return nil, nil, &scheduleAbort{trigger: sandboxTriggerFrozenAssetDivergence, err: fmt.Errorf("frozen contract for task %q vanished", task.ID)}
	}
	if err := verifySandboxFrozenAssets(ctx, preparation, frozenContract); err != nil {
		var divergence *frozenAssetDivergenceError
		if errors.As(err, &divergence) {
			return nil, nil, &scheduleAbort{trigger: sandboxTriggerFrozenAssetDivergence, err: err}
		}
		stop.reason = sandboxStopEnvironmentFailure
		stop.detail = sandboxStopDetailTaskImageDigest
		return nil, nil, nil
	}
	// 运行必须使用任务特化配置（任务 verifier 命令与任务镜像沙箱），原始臂配置只用于指纹视图。
	record, verdict, stepAbort := executor.runSandboxAttempt(ctx, prepared, runID, precheck.evaluationID, step, evaluationDir, stop)
	if verdict == nil {
		return record, verdict, stepAbort
	}
	units, costErr := sandboxeval.ComputeRunCostUnits(verdict.InputTokens, verdict.OutputTokens,
		int64(options.EvalSet.PricingSnapshot.InputPriceMicrousdPerMillionTokens),
		int64(options.EvalSet.PricingSnapshot.OutputPriceMicrousdPerMillionTokens))
	if costErr != nil {
		return record, nil, &scheduleAbort{trigger: sandboxTriggerCostUnitsOverflow, err: costErr}
	}
	ledger.chargeUnits(units)
	ledger.chargeUsage(verdict.InputTokens, verdict.OutputTokens, 1)
	return record, verdict, stepAbort
}

// frozenAssetDivergenceError 把"摘要已取得但与冻结值不符"从环境执行失败中区
// 分出来：前者是冻结合同破坏必须以无效闭合，后者只是基础设施故障。
type frozenAssetDivergenceError struct {
	detail string
}

// Error 返回漂移面描述。
func (err *frozenAssetDivergenceError) Error() string { return err.detail }

// sandboxTaskContract 返回指定任务的冻结执行合同。
func sandboxTaskContract(evalSet sandboxeval.EvalSet, taskID string) (sandboxeval.TaskContract, bool) {
	for _, frozen := range evalSet.TaskContracts {
		if frozen.TaskID == taskID {
			return frozen, true
		}
	}
	return sandboxeval.TaskContract{}, false
}

// verifySandboxFrozenAssets 复核只能在真实工作区上取得的冻结资产摘要：私有
// verifier 摘要在准备阶段已随工作区注入产生，任务镜像摘要经本地镜像自省取
// 得。摘要取得即返回 divergence 错误（无效轨）；取不到摘要属环境故障（停止
// 轨），两者绝不混同。
func verifySandboxFrozenAssets(ctx context.Context, preparation PreparationResult, frozen sandboxeval.TaskContract) error {
	if preparation.VerifierFingerprint != frozen.VerifierDigest {
		return &frozenAssetDivergenceError{detail: "applied verifier digest diverges from the frozen task contract"}
	}
	digest, err := dockerImageSelfDigest(ctx, preparation.DockerImage)
	if err != nil {
		return fmt.Errorf("inspect task docker image digest: %w", err)
	}
	if digest != frozen.DockerImageDigest {
		return &frozenAssetDivergenceError{detail: "task docker image self digest diverges from the frozen task contract"}
	}
	return nil
}

// dockerImageSelfDigestLookup 是本地镜像自省的受控进程边界；生产实现经无
// shell 的 docker argv 读取镜像配置摘要，测试以替身替换后必须还原。
var dockerImageSelfDigestLookup = func(ctx context.Context, image string) (string, error) {
	output, err := runProcessOutput(ctx, "", []string{"docker", "image", "inspect", "--format", "{{.Id}}", image})
	if err != nil {
		return "", fmt.Errorf("inspect task docker image digest: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// dockerImageSelfDigest 读取本地镜像的自描述配置摘要；与存在性检查不同，摘
// 要参与冻结身份比对，任何偏差都在启动 Provider 前暴露。
func dockerImageSelfDigest(ctx context.Context, image string) (string, error) {
	return dockerImageSelfDigestLookup(ctx, image)
}

// runSandboxAttempt 对已就绪的工作区发起一次 Agent 运行并完成全链证据提取：
// Inspect、哈希链与二次确定性回放比对、合同投影、工具配对与探索计数、diff
// 归档与泄漏扫描。任何终局出口（含 Provider 无终态与证据合同破坏）都先补齐
// 当步泄漏扫描再封口——未扫描的表面不得冒充干净证据。错误文本只保留分类词，
// 原文一律不落盘。
func (executor SandboxExecutor) runSandboxAttempt(ctx context.Context, prepared preparedAttempt, runID, evaluationID string, step SandboxPairStep, evaluationDir string, stop *scheduleStop) (*SandboxRunRecord, *sandboxeval.SuiteVerdict, *scheduleAbort) {
	record := &SandboxRunRecord{
		SchemaVersion: sandboxRecordSchemaVersion,
		RunID:         runID,
		EvaluationID:  evaluationID,
		Arm:           string(step.Arm),
		TaskID:        step.TaskID,
		AttemptIndex:  step.Try,
		StartedAt:     executor.now(),
		LeakScan:      sandboxeval.LeakScanReport{Outcome: sandboxeval.LeakClean},
	}
	diffDirectory := filepath.Join(evaluationDir, "diffs", sandboxArmTag(step.Arm))
	if err := os.MkdirAll(diffDirectory, 0o700); err != nil {
		// 此时尚无任何运行发生，工作区与状态目录都是全新空面，clean 初值与
		// 事实一致；不满足"未扫描不得称干净"的例外必须在注释中显式成立。
		record.FailureClass = sandboxTriggerLeakScanFailed
		record.FinishedAt = executor.now()
		return record, nil, &scheduleAbort{trigger: sandboxTriggerLeakScanFailed, err: fmt.Errorf("create diff archive directory: %w", err)}
	}
	// finalizeSurfaces 是所有出口共用的封口段：归档工作区 diff、枚举全部持久
	// 化表面并用内存中的原始 Key 扫描，扫描结论写回 record 后才允许返回；自
	// 身失败也必须补齐结束时间戳，任何终局 record 都不得停留在零值时刻。
	finalizeSurfaces := func() *scheduleAbort {
		record.FinishedAt = executor.now()
		captured := executor.captureDiff(ctx, prepared.Result.Workspace, diffDirectory, AttemptResult{TaskID: step.TaskID, Attempt: step.Try})
		scanSurfaces, surfaceErr := fileSurfacePathsUnder(prepared.Result.Workspace, prepared.Result.StateDir)
		if surfaceErr != nil {
			return &scheduleAbort{trigger: sandboxTriggerLeakScanFailed, err: surfaceErr}
		}
		if captured.DiffPath != "" {
			scanSurfaces = append(scanSurfaces, filepath.Join(diffDirectory, captured.DiffPath))
		}
		report, scanErr := sandboxeval.ScanPersistenceSurfaces(scanSurfaces, []byte(os.Getenv(sandboxeval.SandboxProviderKeyEnv)))
		if scanErr != nil {
			return &scheduleAbort{trigger: sandboxTriggerLeakScanFailed, err: scanErr}
		}
		record.LeakScan = report
		if report.Outcome == sandboxeval.LeakDetected {
			record.SecurityIncidents = 1
			return &scheduleAbort{trigger: sandboxTriggerCredentialLeak, err: errors.New("persistence surfaces carried the sandbox provider key")}
		}
		return nil
	}
	abortInvalid := func(trigger string, cause error) (*SandboxRunRecord, *sandboxeval.SuiteVerdict, *scheduleAbort) {
		record.FailureClass = trigger
		if scanErr := finalizeSurfaces(); scanErr != nil && trigger != sandboxTriggerLeakScanFailed {
			record.FailureClass = scanErr.trigger
			return record, nil, scanErr
		}
		record.FinishedAt = executor.now()
		return record, nil, &scheduleAbort{trigger: trigger, err: cause}
	}
	failWithoutVerdict := func(class string) (*SandboxRunRecord, *sandboxeval.SuiteVerdict, *scheduleAbort) {
		record.FailureClass = class
		if scanErr := finalizeSurfaces(); scanErr != nil {
			record.FailureClass = scanErr.trigger
			return record, nil, scanErr
		}
		record.FinishedAt = executor.now()
		return record, nil, nil
	}
	result, err := executor.Service.Run(ctx, app.RunOptions{
		Task:                    prepared.Task.AgentPrompt(),
		Workspace:               prepared.Result.Workspace,
		StateDir:                prepared.Result.StateDir,
		Config:                  prepared.Configuration,
		DisableUserInstructions: true,
	})
	record.WallTimeMS = executor.now().Sub(record.StartedAt).Milliseconds()
	// 错误分轨：Agent 自身预算终止（轮次/工具/token/墙钟/成本）携带完整事
	// 件证据与用量，属评测合同的有效失败样本——落入证据提取路径计入成功
	// 率与成本，套件继续；提供方类失效或缺失运行身份才是计划缺口，调度以
	// 停止原因中断并收敛为不确定。
	budgetClass := ""
	if err != nil {
		class := contract.ClassifyError(err)
		if result.ID == "" || !contract.IsBudgetFailure(class) {
			stop.reason = sandboxStopProviderFailure
			stop.detail = class
			return failWithoutVerdict(class)
		}
		budgetClass = class
	}
	if result.ID == "" {
		// 无错误的空运行身份属装配异常，与提供方失效同一停止轨。
		stop.reason = sandboxStopProviderFailure
		stop.detail = sandboxStopProviderFailure
		return failWithoutVerdict(sandboxStopProviderFailure)
	}
	inspected, inspectErr := executor.Service.Inspect(context.Background(), prepared.Result.Workspace, prepared.Result.StateDir, prepared.Configuration, result.ID, true)
	if inspectErr != nil {
		return abortInvalid(sandboxTriggerEvidenceUnverifiable, inspectErr)
	}
	replayConsistent, replayErr := replayConsistentWithProjection(inspected.Projection, inspected.Events)
	if replayErr != nil {
		return abortInvalid(sandboxTriggerEvidenceIntegrityBroken, replayErr)
	}
	if !replayConsistent {
		return abortInvalid(sandboxTriggerReplayDivergence, errors.New("deterministic replay diverges from the persisted projection"))
	}
	record.ReplayConsistent = true
	runResult, projectionErr := executor.Service.Evidence(
		context.Background(),
		prepared.Result.Workspace,
		prepared.Result.StateDir,
		prepared.Configuration,
		result.ID,
	)
	if projectionErr != nil {
		return abortInvalid(sandboxTriggerContractProjectionFailed, projectionErr)
	}
	runResult.Manifest.Task = prepared.Result.Task
	completedTrusted := runResult.Evidence.Trusted
	pairingBreaks, pairingErr := domain.ToolCallPairingBreaks(inspected.Events)
	if pairingErr != nil {
		return abortInvalid(sandboxTriggerToolPairingExtraction, pairingErr)
	}
	evidence, evidenceErr := domain.CollectRunEvidenceMetrics(inspected.Events)
	if evidenceErr != nil {
		return abortInvalid(sandboxTriggerExplorationSignatureMissing, evidenceErr)
	}
	doctorOK := true
	if _, doctorErr := executor.Service.Doctor(context.Background(), prepared.Result.Workspace, prepared.Result.StateDir, prepared.Configuration); doctorErr != nil {
		doctorOK = false
	}
	record.InputTokens = runResult.Metrics.InputTokens
	record.OutputTokens = runResult.Metrics.OutputTokens
	record.ToolPairingBreaks = pairingBreaks
	record.RepeatedExplorations = evidence.RepeatedExplorations
	record.DoctorOK = doctorOK
	if scanErr := finalizeSurfaces(); scanErr != nil {
		record.FailureClass = scanErr.trigger
		return record, nil, scanErr
	}
	record.Success = completedTrusted
	if !completedTrusted && record.FailureClass == "" {
		// 预算终止的失败样本保留其精确预算分类，供逐类归因。
		if budgetClass != "" {
			record.FailureClass = budgetClass
		} else {
			record.FailureClass = verificationFailedClass
		}
	}
	record.Counted = true
	record.FinishedAt = executor.now()
	return record, &sandboxeval.SuiteVerdict{
		RunID:                record.RunID,
		Arm:                  step.Arm,
		TaskID:               step.TaskID,
		AttemptIndex:         step.Try,
		Success:              completedTrusted,
		SecurityIncidents:    record.SecurityIncidents,
		DoctorReplayBroken:   !doctorOK || !replayConsistent,
		ToolPairingBreaks:    pairingBreaks,
		RepeatedExplorations: evidence.RepeatedExplorations,
		InputTokens:          record.InputTokens,
		OutputTokens:         record.OutputTokens,
		WallTimeMS:           record.WallTimeMS,
	}, nil
}

// verdictFromSandboxRunRecord 把已校验的闭合记录恢复为比较器最小投影；恢复
// 过程只消费冻结字段，不重新执行 Agent 或 verifier。
func verdictFromSandboxRunRecord(record SandboxRunRecord) sandboxeval.SuiteVerdict {
	securityIncidents := record.SecurityIncidents
	if record.LeakScan.Outcome == sandboxeval.LeakDetected && securityIncidents == 0 {
		securityIncidents = 1
	}
	return sandboxeval.SuiteVerdict{
		RunID: record.RunID, Arm: sandboxeval.SuiteArm(record.Arm), TaskID: record.TaskID,
		AttemptIndex: record.AttemptIndex, Success: record.Success,
		SecurityIncidents:  securityIncidents,
		DoctorReplayBroken: !record.DoctorOK || !record.ReplayConsistent,
		ToolPairingBreaks:  record.ToolPairingBreaks, RepeatedExplorations: record.RepeatedExplorations,
		InputTokens: record.InputTokens, OutputTokens: record.OutputTokens, WallTimeMS: record.WallTimeMS,
	}
}

// recoveredInvalidTrigger 返回未计分记录中可重放的安全/证据破坏触发名。
func recoveredInvalidTrigger(record SandboxRunRecord) string {
	if record.FailureClass == sandboxTriggerCredentialLeak &&
		record.SecurityIncidents == 1 &&
		record.LeakScan.Outcome == sandboxeval.LeakDetected {
		return sandboxTriggerCredentialLeak
	}
	switch record.FailureClass {
	case sandboxTriggerEvidenceUnverifiable,
		sandboxTriggerEvidenceIntegrityBroken, sandboxTriggerReplayDivergence,
		sandboxTriggerContractProjectionFailed, sandboxTriggerToolPairingExtraction,
		sandboxTriggerExplorationSignatureMissing, sandboxTriggerLeakScanFailed:
		return record.FailureClass
	default:
		return ""
	}
}

// replayConsistentWithProjection 对持久化投影做二次确定性核验：事件流必须先
// 通过哈希链完整性校验，再由回放状态机独立重建终态，其状态与验证结论两个关
// 键字段与存储投影逐一相符才算一致。校验或回放失败返回错误，不一致仅返回
// false，调用方分别落不同触发名。
func replayConsistentWithProjection(projection domain.RunProjection, events []domain.Event) (bool, error) {
	if err := domain.VerifyChain(events); err != nil {
		return false, fmt.Errorf("hash chain verification: %w", err)
	}
	replayed, err := domain.Replay(events)
	if err != nil {
		return false, fmt.Errorf("deterministic replay: %w", err)
	}
	return replayed.State == projection.State && replayed.VerificationSucceeded == projection.VerificationSucceeded, nil
}

// leaseLedger 是整套评测的租约账面：请求、输入、输出直接累加，成本以纯整
// 数进位法折算微美元——原始 CostUnits 的商立即入账、余数滚存待后续进位，
// 全程无浮点。
type leaseLedger struct {
	requests        int64
	inputTokens     int64
	outputTokens    int64
	carriedMicroUSD int64
	remainderUnits  int64
}

// chargeUnits 把一次 Run 的原始成本单位入账并完成余数进位。
func (ledger *leaseLedger) chargeUnits(units int64) {
	ledger.carriedMicroUSD += units / sandboxeval.CostUnitsPerMicroUSD
	ledger.remainderUnits += units % sandboxeval.CostUnitsPerMicroUSD
	if ledger.remainderUnits >= sandboxeval.CostUnitsPerMicroUSD {
		ledger.carriedMicroUSD += ledger.remainderUnits / sandboxeval.CostUnitsPerMicroUSD
		ledger.remainderUnits %= sandboxeval.CostUnitsPerMicroUSD
	}
}

// chargeUsage 累计一次 Run 的 token 用量与请求计数。
func (ledger *leaseLedger) chargeUsage(inputTokens, outputTokens, requests int64) {
	ledger.inputTokens += inputTokens
	ledger.outputTokens += outputTokens
	ledger.requests += requests
}

// exceedsLease 判定账面是否触及租约任一上限：达到即视为耗尽并停止发起新的
// Run。判定在 Run 完成记账之后进行，最后一步的瞬时超额风险由租约合同强制
// 的服务端配额兜底。
func (ledger *leaseLedger) exceedsLease(lease sandboxeval.CredentialLease) bool {
	return ledger.requests >= lease.MaxRequests ||
		ledger.inputTokens >= lease.MaxInputTokens ||
		ledger.outputTokens >= lease.MaxOutputTokens ||
		ledger.carriedMicroUSD >= lease.MaxCostMicrousd
}

// fileSurfacePathsUnder 递归枚举泄漏扫描表面下的全部普通文件路径：目录跳过，
// 符号链接一律失败关闭，防止扫描跟随链接读取工作区外宿主内容。
func fileSurfacePathsUnder(roots ...string) ([]string, error) {
	surfaces := make([]string, 0)
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("leak scan surface contains a symbolic link")
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("leak scan surface contains a non-regular file")
			}
			surfaces = append(surfaces, path)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("enumerate leak scan surfaces under %s: %w", filepath.Base(root), err)
		}
	}
	return surfaces, nil
}
