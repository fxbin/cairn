// [INPUT]: 依赖Memory评测合同、冻结资产与受控执行入口。
// [OUTPUT]: 提供受控评测入口、唯一评测事务与闭合输出。
// [POS]: 带外评测工具链，不进入最小Agent内核。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package benchmark 提供隔离评测与冻结证据。
// Author: fxbin
package benchmark

import (
	"context"

	"errors"
	"fmt"

	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"

	"github.com/fxbin/cairn/internal/capabilities/memory"
	"github.com/fxbin/cairn/internal/runtime/lock"
	"github.com/fxbin/cairn/internal/runtime/sandboxeval"
)

// 沙箱评测执行器的闭合状态词：停止原因表示未完成计划但合同未被破坏的情形，
// 一律收敛为不确定；触发名表示立即以无效闭合的合同破坏，两者都进入本地评测
// 结果供人工复核追溯。
const (
	// sandboxStopLeaseExhausted 表示租约任一配额维度耗尽，按计划缺口进入
	// inconclusive。
	sandboxStopLeaseExhausted = "lease_exhausted"
	// sandboxStopContextCancelled 表示调用方取消或超时，按计划缺口进入
	// inconclusive。
	sandboxStopContextCancelled = "context_cancelled"
	// sandboxStopProviderFailure 表示 Provider 调用未能产生可校验终态，计入
	// 计划 pair 缺失而非有效失败样本。
	sandboxStopProviderFailure = "provider_failure"
	// sandboxStopEnvironmentFailure 表示 clone/bootstrap/镜像检查等环境准备
	// 无法把工作区带入合同状态：停止发起新 Run 而不是烧毁候选的唯一评测机会，
	// 摘要确实漂移才走无效轨。
	sandboxStopEnvironmentFailure = "environment_failure"
)

// 立即无效闭合的触发名词表；与套件结果一起留档，任何取值都必须可追溯到具
// 体的合同破坏面。
const (
	// sandboxTriggerCredentialLeak 是泄漏扫描命中的触发名：隔离破坏安全事件，
	// 无论发生于哪一臂都立即以 invalid 闭合。
	sandboxTriggerCredentialLeak = "credential_leak_detected"
	// sandboxTriggerOfflinePrescreenFailed 表示离线预筛在启动事务后发现冻结
	// 输入破坏（ADR §3.3）：评测以 invalid 闭合且绝不调用模型。
	sandboxTriggerOfflinePrescreenFailed = "offline_prescreen_failed"
	// sandboxTriggerFrozenAssetDivergence 表示运行期取得的镜像或 verifier 摘
	// 要与冻结合同不符。
	sandboxTriggerFrozenAssetDivergence = "frozen_asset_divergence"
	// sandboxTriggerCostUnitsOverflow 表示成本单位计算受检溢出。
	sandboxTriggerCostUnitsOverflow = "cost_units_overflow"
	// sandboxTriggerEvidenceUnverifiable 表示 Inspect 无法返回可核证的事件流。
	sandboxTriggerEvidenceUnverifiable = "evidence_unverifiable"
	// sandboxTriggerEvidenceIntegrityBroken 表示哈希链校验或确定性回放本身失败。
	sandboxTriggerEvidenceIntegrityBroken = "evidence_integrity_broken"
	// sandboxTriggerReplayDivergence 表示回放重建的终态与持久化投影不一致。
	sandboxTriggerReplayDivergence = "replay_divergence"
	// sandboxTriggerContractProjectionFailed 表示统一产品合同投影失败关闭。
	sandboxTriggerContractProjectionFailed = "contract_projection_failed"
	// sandboxTriggerToolPairingExtraction 表示配对提取器遇到结构漂移。
	sandboxTriggerToolPairingExtraction = "tool_pairing_extraction"
	// sandboxTriggerExplorationSignatureMissing 表示 continue 关轮缺失探索签名。
	sandboxTriggerExplorationSignatureMissing = "exploration_signature_missing"
	// sandboxTriggerLeakScanFailed 表示扫描表面无法读取：无法证明干净即不作
	// 干净声明。
	sandboxTriggerLeakScanFailed = "leak_scan_failed"
	// sandboxTriggerComparisonNotComputable 表示比较器因可计算性合同破坏拒绝
	// 产出结论。
	sandboxTriggerComparisonNotComputable = "comparison_not_computable"
	// sandboxTriggerRunIdentityDerivation 表示运行身份派生输入残缺。
	sandboxTriggerRunIdentityDerivation = "run_identity_derivation"
)

// verificationFailedClass 是有效失败样本（有终态但验证未通过）的稳定类别词。
const verificationFailedClass = "verification_failed"

// 停止明细的封闭取值：它们只描述"停止发生在哪一步"，绝不携带命令输出、
// 路径或镜像名等本机细节。
const (
	// sandboxStopDetailTaskImageDigest 表示任务镜像自省摘要失败。
	sandboxStopDetailTaskImageDigest = "task_docker_image_self_digest"
)

// scheduleStop 是调度停止信号：原因词进入套件结果的 StopReason，明细进入
// StopDetail；二者都是封闭分类词，供人工定位中断环节而不泄露本机事实。
type scheduleStop struct {
	reason string
	detail string
}

// sandboxRecordSchemaVersion 是支持断点恢复计账的本地记录与套件结果 schema 版本。
// 第三版把 Success 绑定完整的 Evidence.Trusted；旧版成功记录不得进入新判门。
const sandboxRecordSchemaVersion = sandboxeval.TrustedResultSchemaVersion

// RequireTrustedSandboxSuiteVersion 拒绝旧成功语义的套件和 Run 记录；用于
// 无法从晋升 CLI 取得冻结评测集时，至少禁止 v2 Success 穿透新可信判门。
func RequireTrustedSandboxSuiteVersion(result SandboxSuiteResult) error {
	if result.SchemaVersion != sandboxRecordSchemaVersion {
		return fmt.Errorf("sandbox suite schema %d cannot satisfy trusted success schema %d", result.SchemaVersion, sandboxRecordSchemaVersion)
	}
	for _, record := range result.Records {
		if record.SchemaVersion != sandboxRecordSchemaVersion {
			return fmt.Errorf("sandbox run schema %d cannot satisfy trusted success schema %d", record.SchemaVersion, sandboxRecordSchemaVersion)
		}
	}
	return nil
}

// sandboxArmTag 返回臂的稳定短标识，用于按臂分离的目录段、记录与 diff 文件
// 命名；基线与变体永不共享可写归档路径。
func sandboxArmTag(arm sandboxeval.SuiteArm) string {
	if arm == sandboxeval.ArmVariant {
		return "va"
	}
	return "bl"
}

// SandboxExecutor 复用基准 Runner 的准备链与测试替身字段编排一次受控演化候
// 选评测：启动事务、成对交错 Run 循环、租约记账、泄漏扫描与比较器收尾都由
// 它驱动。
type SandboxExecutor struct {
	Runner
	// AuditEnvironment 仅供测试注入已成型环境快照；生产默认读取当前子进程环境。
	AuditEnvironment func() []string
}

// SandboxExecutorOptions 描述一次候选评测的全部输入合同：Memory 三元组显式
// 锁定候选登记工作区哈希对应的存储位置；两臂配置从冻结基线与登记变体展开；
// Key 不经本结构传递，只在执行进程内经环境变量链路消费。
type SandboxExecutorOptions struct {
	Pack            TaskPack
	EvalSet         sandboxeval.EvalSet
	Lease           sandboxeval.CredentialLease
	Policy          sandboxeval.CredentialPolicy
	CandidateID     string
	MemoryWorkspace string
	MemoryStateDir  string
	MemoryConfig    config.Config
	BaselineConfig  config.Config
	Variant         memory.CompactionParamsVariant
	StateRoot       string
	WorkspacesRoot  string
	PrivateRoot     string
	CairnRoot       string
	PrepareTimeout  time.Duration
	PrepareProxy    string
	DryRun          bool
}

// SandboxRunRecord 是单个 Run 的非敏感留档事实：身份四元组、终态证据计数、
// 回放一致性与当步泄漏扫描结论。任务正文、模型输出正文、路径细节与密钥绝
// 不进入本结构。
type SandboxRunRecord struct {
	SchemaVersion        int                        `json:"schema_version"`
	RunID                string                     `json:"run_id"`
	EvaluationID         string                     `json:"evaluation_id"`
	Arm                  string                     `json:"arm"`
	TaskID               string                     `json:"task_id"`
	AttemptIndex         int                        `json:"attempt_index"`
	StartedAt            time.Time                  `json:"started_at"`
	FinishedAt           time.Time                  `json:"finished_at"`
	Counted              bool                       `json:"counted"`
	Success              bool                       `json:"success"`
	FailureClass         string                     `json:"failure_class,omitempty"`
	Error                string                     `json:"error,omitempty"`
	InputTokens          int64                      `json:"input_tokens"`
	OutputTokens         int64                      `json:"output_tokens"`
	WallTimeMS           int64                      `json:"wall_time_ms"`
	ToolPairingBreaks    int                        `json:"tool_pairing_breaks"`
	RepeatedExplorations int                        `json:"repeated_explorations"`
	ReplayConsistent     bool                       `json:"replay_consistent"`
	DoctorOK             bool                       `json:"doctor_ok"`
	SecurityIncidents    int                        `json:"security_incidents"`
	LeakScan             sandboxeval.LeakScanReport `json:"leak_scan"`
}

// SandboxSuiteResult 是整个评测的持久化套件结论：判定闭包、停止原因与全部
// Run 记录。Memory 只保存本文件字节串的 sha256 引用，正文留在忽略目录。
type SandboxSuiteResult struct {
	SchemaVersion int                `json:"schema_version"`
	EvaluationID  string             `json:"evaluation_id"`
	CandidateID   string             `json:"candidate_id"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    time.Time          `json:"finished_at"`
	Status        string             `json:"status"`
	Trigger       string             `json:"trigger,omitempty"`
	StopReason    string             `json:"stop_reason,omitempty"`
	StopDetail    string             `json:"stop_detail,omitempty"`
	Records       []SandboxRunRecord `json:"records"`
}

// SandboxExecutionOutcome 是执行器返回给调用方的闭环报告：dry-run 只携带预
// 筛结论；正式运行携带闭合后的评测行、判定触发名与套件结果摘要引用。
type SandboxExecutionOutcome struct {
	DryRun        bool
	Evaluation    memory.Evaluation
	Conclusion    sandboxeval.Conclusion
	ResultPath    string
	ResultDigest  string
	RunsCompleted int
	StopReason    string
	StopDetail    string
}

// Run 编排一次完整的候选评测：前置检查 →（dry-run 止步）→ 启动事务 → 成对
// 交错 Run 循环 → 判定收尾。返回值与落盘产物不含任务正文与密钥；无法合法
// 闭合时如实报错保留 running 状态，不做启发式改写。
func (executor SandboxExecutor) Run(ctx context.Context, options SandboxExecutorOptions) (outcome SandboxExecutionOutcome, runErr error) {
	precheck, prescreenIssues, err := executor.runPrecheck(options)
	if err != nil {
		return outcome, err
	}
	evaluationDir := evaluationDirectory(options.StateRoot, precheck.evaluationID)
	if options.DryRun {
		outcome.DryRun = true
		if len(prescreenIssues) > 0 {
			return outcome, &prescreenFailure{issues: prescreenIssues}
		}
		return outcome, nil
	}
	evaluationLock, err := lock.Acquire(evaluationLockPath(options.MemoryStateDir, precheck.evaluationID))
	if err != nil {
		if errors.Is(err, lock.ErrHeld) {
			return outcome, fmt.Errorf("evaluation %s is already active in another process: %w", precheck.evaluationID, err)
		}
		return outcome, fmt.Errorf("acquire evaluation lock: %w", err)
	}
	defer func() {
		if releaseErr := evaluationLock.Release(); releaseErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("release evaluation lock: %w", releaseErr))
		}
	}()
	inspected, err := executor.Service.InspectEvolutionCandidate(ctx, app.MemoryEvolutionInspectOptions{
		CandidateID: options.CandidateID, Workspace: options.MemoryWorkspace,
		StateDir: options.MemoryStateDir, Config: options.MemoryConfig,
	})
	if err != nil {
		return outcome, fmt.Errorf("inspect evaluation candidate: %w", err)
	}
	resuming := inspected.Candidate.Status == memory.CandidateStateEvaluating
	if inspected.Candidate.Status != memory.CandidateStateReady && !resuming {
		return outcome, fmt.Errorf("candidate %s cannot start or resume evaluation from state %s", options.CandidateID, inspected.Candidate.Status)
	}
	if err := prepareEvaluationDirectory(evaluationDir, resuming); err != nil {
		return outcome, err
	}
	created, err := executor.startEvaluationTransaction(ctx, options, precheck)
	if err != nil {
		return outcome, fmt.Errorf("start evaluation transaction: %w", err)
	}
	outcome.Evaluation = created
	if resuming {
		persisted, resultPath, resultDigest, found, loadErr := loadPersistedSuiteResult(options, precheck.evaluationID, len(prescreenIssues) > 0)
		if loadErr != nil {
			return outcome, fmt.Errorf("load persisted suite result: %w", loadErr)
		}
		if found {
			if !persisted.StartedAt.Equal(created.StartedAt) {
				return outcome, errors.New("persisted suite result start time diverges from the running evaluation")
			}
			concluded, finishErr := executor.finishEvaluationTransaction(ctx, options, precheck.evaluationID, persisted.Status, resultDigest)
			if finishErr != nil {
				return outcome, fmt.Errorf("finish recovered evaluation transaction: %w", finishErr)
			}
			outcome.Evaluation = concluded
			outcome.Conclusion = sandboxeval.Conclusion{Status: persisted.Status, Trigger: persisted.Trigger}
			outcome.ResultPath = resultPath
			outcome.ResultDigest = resultDigest
			outcome.RunsCompleted = len(persisted.Records)
			outcome.StopReason = persisted.StopReason
			outcome.StopDetail = persisted.StopDetail
			return outcome, nil
		}
	}
	// 幂等重入意味着评测时钟由首次提交锚定：恢复路径用更早的起始时刻复核租
	// 约到期窗口，绝不以新时钟放松约束。
	if created.StartedAt.Before(executor.now()) {
		if err := sandboxeval.AuthorizeLease(options.Lease, options.Policy, created.StartedAt); err != nil {
			return outcome, fmt.Errorf("credential lease no longer covers the resumed evaluation window: %w", err)
		}
	}
	conclusion := sandboxeval.Conclusion{}
	stopReason, stopDetail := "", ""
	var records []SandboxRunRecord
	var scheduleErr error
	if len(prescreenIssues) > 0 {
		// 预筛失败属冻结合同破坏：开评测行后立即以无效闭合且绝不调用模型，
		// 保证"评测结果为 invalid"存在可审计的一次评测事实。
		conclusion = sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: sandboxTriggerOfflinePrescreenFailed}
	} else {
		conclusion, stopReason, stopDetail, records, _, scheduleErr = executor.runSchedule(ctx, options, precheck)
	}
	if conclusion.Status == "" {
		// 结论为空意味着执行链在合法判定前遭遇 I/O 故障：不落盘、不闭合、
		// 保留 running 状态由人工对账，绝不伪造结论。
		return outcome, fmt.Errorf("suite execution failed before a legal conclusion: %w", scheduleErr)
	}
	resultPath, resultDigest, persistErr := executor.persistSuiteResult(options, precheck.evaluationID, outcome.Evaluation.StartedAt, conclusion, stopReason, stopDetail, records)
	if persistErr != nil {
		return outcome, fmt.Errorf("persist suite result: %w", persistErr)
	}
	outcome.ResultPath = resultPath
	outcome.ResultDigest = resultDigest
	outcome.Conclusion = conclusion
	outcome.StopReason = stopReason
	outcome.StopDetail = stopDetail
	outcome.RunsCompleted = len(records)
	concluded, err := executor.finishEvaluationTransaction(ctx, options, precheck.evaluationID, conclusion.Status, resultDigest)
	if err != nil {
		return outcome, fmt.Errorf("finish evaluation transaction: %w", err)
	}
	outcome.Evaluation = concluded
	if scheduleErr != nil {
		return outcome, fmt.Errorf("suite reached a legal %s conclusion with suppressed diagnostics: %w", conclusion.Status, scheduleErr)
	}
	return outcome, nil
}

// startEvaluationTransaction 在工作区锁内创建 running 评测行并迁移候选；同
// ID 重入按存储层幂等语义返回既有 running 行。
func (executor SandboxExecutor) startEvaluationTransaction(ctx context.Context, options SandboxExecutorOptions, precheck precheckResult) (memory.Evaluation, error) {
	return executor.Service.StartEvolutionEvaluation(ctx, app.MemoryEvaluationStartOptions{
		Evaluation: memory.Evaluation{
			SchemaVersion:             memory.EvaluationSchemaVersion,
			ID:                        precheck.evaluationID,
			CandidateID:               options.CandidateID,
			EvalSetFingerprint:        precheck.evalSetFingerprint,
			BaselineConfigFingerprint: precheck.baselineFingerprint,
			VariantConfigFingerprint:  precheck.variantFingerprint,
			ComparatorVersion:         options.EvalSet.ComparatorContract.ComparatorVersion,
			Status:                    memory.EvaluationStatusRunning,
		},
		Binding: memory.EvaluationBinding{
			BaselineFingerprint:         precheck.baselineFingerprint,
			VariantSpecSHA256:           precheck.variantSpecDigest,
			CandidateVariantFingerprint: precheck.candidateVariantFingerprint,
		},
		Workspace: options.MemoryWorkspace,
		StateDir:  options.MemoryStateDir,
		Config:    options.MemoryConfig,
	})
}

// finishEvaluationTransaction 在工作区锁内闭合评测：结论四态映射到候选终态
// 并写入套件结果摘要引用，四件套在同一事务提交。
func (executor SandboxExecutor) finishEvaluationTransaction(ctx context.Context, options SandboxExecutorOptions, evaluationID, status, resultDigest string) (memory.Evaluation, error) {
	return executor.Service.FinishEvolutionEvaluation(ctx, evaluationID, options.MemoryWorkspace, options.MemoryStateDir, options.MemoryConfig, status, resultDigest)
}
