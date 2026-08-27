// 本文件承载候选评测的沙箱执行器：启动前的失败关闭检查、单事务评测启动、
// 成对交错 Run 循环、租约记账、持久化表面泄漏扫描、判定收尾与套件结果留档。
// 隔离破坏与证据合同破坏立即以无效闭合；环境类故障按停止原因收敛为不确定，
// 不烧毁候选的唯一评测机会。
//
// Author: fxbin
package benchmark

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
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

// sandboxRecordSchemaV1 是沙箱执行器本地记录与套件结果的当前 schema 版本。
const sandboxRecordSchemaV1 = 1

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

// precheck 结构化承载启动前检查的派生产物：展开后两臂配置、变体展开指纹与
// 确定性评测身份。任何一项失败都保持候选 ready 且零持久化。
type precheckResult struct {
	baselineConfig      config.Config
	variantConfig       config.Config
	variantFingerprint  string
	evaluationID        string
	evalSetFingerprint  string
	baselineFingerprint string
}

// prescreenFailure 把离线预筛失败从普通错误中区分出来：它代表冻结合同破坏，
// 走 invalid 结论轨而不是保持 ready 的拒绝轨。
type prescreenFailure struct {
	issues []string
}

// Error 汇总预筛失败面；条目本身已是封闭的比对结论文本。
func (failure *prescreenFailure) Error() string {
	return "offline prescreen failed: " + strings.Join(failure.issues, "; ")
}

// evaluationDirectory 返回评测状态目录：所有沙箱评测本地产物都以它为唯一归
// 档根，Memory 侧只保留其内套件结果的字节摘要引用。
func evaluationDirectory(stateRoot, evaluationID string) string {
	return filepath.Join(stateRoot, "evaluations", safePathComponent(evaluationID))
}

// suiteSegmentForArm 生成按臂分离的套件段标识：Workspace、状态目录与任务身
// 份投影都以它区分基线臂与变体臂，两臂永不共享可写边界。
func suiteSegmentForArm(evaluationID string, arm sandboxeval.SuiteArm) string {
	return "sandboxeval-" + sandboxArmTag(arm) + "-" + safePathComponent(evaluationID)
}

// BootstrapContractDigest 以规范化 JSON 计算 bootstrap 合同摘要：命令序列经
// 排序键稳定编码后取 sha256。冻结评测集制作者与本执行器共用同一实现，双侧
// 摘要不一致即视为冻结合同破坏。
func BootstrapContractDigest(commands []Command) (string, error) {
	view := struct {
		Commands []Command `json:"commands"`
	}{Commands: append([]Command(nil), commands...)}
	encoded, err := json.Marshal(view)
	if err != nil {
		return "", fmt.Errorf("encode bootstrap contract: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// runPrecheck 执行启动事务前的全量失败关闭检查。拒绝轨错误（结构与身份残缺、
// 租约不足等）以 error 返回，候选保持 ready 零持久化；离线预筛的冻结指纹破
// 坏不属拒绝轨，以第二返回值携带失败面——按 ADR 合同它将在开评测行后立即以
// invalid 闭合，绝不调用模型。dry-run 与正式运行共用同一检查面，保证两个入
// 口等价。
func (executor SandboxExecutor) runPrecheck(options SandboxExecutorOptions) (precheckResult, []string, error) {
	precheck := precheckResult{}
	if options.CandidateID == "" || options.StateRoot == "" || options.WorkspacesRoot == "" || options.PrivateRoot == "" {
		return precheck, nil, errors.New("sandbox evaluation requires candidate id and state, workspaces, private roots")
	}
	if err := validatePrepareProxy(options.PrepareProxy); err != nil {
		return precheck, nil, fmt.Errorf("prepare proxy: %w", err)
	}
	if err := options.Pack.Validate(); err != nil {
		return precheck, nil, fmt.Errorf("task pack invalid: %w", err)
	}
	if packFingerprint := options.Pack.Fingerprint(); packFingerprint != options.EvalSet.TaskPackFingerprint {
		return precheck, nil, fmt.Errorf("task pack fingerprint %q diverges from frozen %q", packFingerprint, options.EvalSet.TaskPackFingerprint)
	}
	if _, err := options.Pack.SelectedTasks(options.EvalSet.TaskIDs); err != nil {
		return precheck, nil, fmt.Errorf("frozen task selection: %w", err)
	}
	if err := validateSandboxTaskStructures(options); err != nil {
		return precheck, nil, err
	}
	if strings.TrimSpace(options.BaselineConfig.Provider.APIKeyEnv) != sandboxeval.SandboxProviderKeyEnv {
		return precheck, nil, fmt.Errorf("frozen config must name %s as its provider api_key_env", sandboxeval.SandboxProviderKeyEnv)
	}
	if strings.TrimSpace(os.Getenv(sandboxeval.SandboxProviderKeyEnv)) == "" {
		return precheck, nil, fmt.Errorf("host environment must inject %s for this evaluation", sandboxeval.SandboxProviderKeyEnv)
	}
	if err := config.Validate(options.BaselineConfig); err != nil {
		return precheck, nil, fmt.Errorf("baseline config invalid: %w", err)
	}
	variantConfig, err := ExpandCompactionVariant(options.BaselineConfig, options.Variant)
	if err != nil {
		return precheck, nil, err
	}
	if err := sandboxeval.TaskEnvironmentViolation(sandboxEnvironmentNames(options.BaselineConfig)); err != nil {
		return precheck, nil, err
	}
	if err := sandboxeval.TaskEnvironmentViolation(sandboxEnvironmentNames(variantConfig)); err != nil {
		return precheck, nil, err
	}
	if err := options.EvalSet.Validate(); err != nil {
		return precheck, nil, fmt.Errorf("frozen eval set rejected: %w", err)
	}
	if err := options.Policy.Validate(); err != nil {
		return precheck, nil, fmt.Errorf("credential policy rejected: %w", err)
	}
	policyFingerprint, err := options.Policy.Fingerprint()
	if err != nil {
		return precheck, nil, err
	}
	if policyFingerprint != options.EvalSet.CredentialPolicyFingerprint {
		return precheck, nil, fmt.Errorf("credential policy fingerprint %q diverges from frozen %q", policyFingerprint, options.EvalSet.CredentialPolicyFingerprint)
	}
	suiteStart := executor.now()
	if err := sandboxeval.AuthorizeLease(options.Lease, options.Policy, suiteStart); err != nil {
		return precheck, nil, fmt.Errorf("credential lease authorization failed before any persistence: %w", err)
	}
	evalSetFingerprint, err := options.EvalSet.Fingerprint()
	if err != nil {
		return precheck, nil, err
	}
	baselineFingerprint := options.BaselineConfig.Fingerprint()
	variantFingerprint, err := VariantExpandedFingerprint(baselineFingerprint, options.Variant)
	if err != nil {
		return precheck, nil, err
	}
	evaluationID, err := memory.EvaluationID(options.CandidateID, evalSetFingerprint,
		baselineFingerprint, variantFingerprint, options.EvalSet.ComparatorContract.ComparatorVersion)
	if err != nil {
		return precheck, nil, err
	}
	precheck = precheckResult{
		baselineConfig: options.BaselineConfig, variantConfig: variantConfig,
		variantFingerprint: variantFingerprint, evaluationID: evaluationID,
		evalSetFingerprint: evalSetFingerprint, baselineFingerprint: baselineFingerprint,
	}
	report, err := RunOfflinePrescreen(options.EvalSet, options.BaselineConfig, variantConfig, options.Variant, nil)
	if err != nil {
		return precheck, nil, err
	}
	if !report.Passed() {
		return precheck, report.Issues, nil
	}
	return precheck, nil, nil
}

// validateSandboxTaskStructures 对照冻结任务合同做纯内存的结构性核验：每个
// 冻结任务的基线提交与 bootstrap 合同摘要必须和本地任务合同逐字一致。镜像
// 与 verifier 摘要依赖工作区与环境，只能在运行期逐步复核并归入 invalid 结
// 论轨。
func validateSandboxTaskStructures(options SandboxExecutorOptions) error {
	tasks, err := options.Pack.SelectedTasks(options.EvalSet.TaskIDs)
	if err != nil {
		return fmt.Errorf("frozen task selection: %w", err)
	}
	selected := make(map[string]Task, len(tasks))
	for _, task := range tasks {
		selected[task.ID] = task
		for _, command := range task.Verification.Bootstrap {
			if len(command.Argv) == 0 {
				return fmt.Errorf("task %q carries an empty bootstrap command", task.ID)
			}
		}
	}
	for _, frozen := range options.EvalSet.TaskContracts {
		task, found := selected[frozen.TaskID]
		if !found {
			return fmt.Errorf("frozen task %q is absent from the local task pack", frozen.TaskID)
		}
		if task.Repository.BaselineCommit != frozen.BaselineCommit {
			return fmt.Errorf("frozen baseline commit for task %q diverges from the local task contract", frozen.TaskID)
		}
		digest, err := BootstrapContractDigest(task.Verification.Bootstrap)
		if err != nil {
			return err
		}
		if digest != frozen.BootstrapContractDigest {
			return fmt.Errorf("frozen bootstrap contract digest for task %q diverges from the local task contract", frozen.TaskID)
		}
	}
	return nil
}

// sandboxEnvironmentNames 收集冻结配置中全部会注入任务子进程的环境变量名单：
// 验证命令显式 env 与策略允许名单都属任务可见面，沙箱 Key 名混入任何一处都
// 是隔离破坏。
func sandboxEnvironmentNames(configuration config.Config) []string {
	names := make([]string, 0, len(configuration.Policy.EnvAllow))
	names = append(names, configuration.Policy.EnvAllow...)
	for _, command := range configuration.Verification.Commands {
		for key := range command.Env {
			names = append(names, key)
		}
	}
	return names
}

// ensureEmptyEvaluationDirectory 创建评测状态目录并拒绝任何既有残件：部分完
// 成的评测无法合规续算（已闭合 Run 不得重跑），检测到残留即失败关闭，把显
// 式恢复留给独立的人工决策切片。
func ensureEmptyEvaluationDirectory(directory string) error {
	entries, readErr := os.ReadDir(directory)
	if readErr == nil && len(entries) > 0 {
		return fmt.Errorf("evaluation directory %q already holds artifacts; interrupted evaluations must be resolved explicitly instead of rerun", filepath.Base(directory))
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create evaluation directory: %w", err)
	}
	return nil
}

// Run 编排一次完整的候选评测：前置检查 →（dry-run 止步）→ 启动事务 → 成对
// 交错 Run 循环 → 判定收尾。返回值与落盘产物不含任务正文与密钥；无法合法
// 闭合时如实报错保留 running 状态，不做启发式改写。
func (executor SandboxExecutor) Run(ctx context.Context, options SandboxExecutorOptions) (SandboxExecutionOutcome, error) {
	outcome := SandboxExecutionOutcome{}
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
	if err := ensureEmptyEvaluationDirectory(evaluationDir); err != nil {
		return outcome, err
	}
	created, err := executor.startEvaluationTransaction(ctx, options, precheck)
	if err != nil {
		return outcome, fmt.Errorf("start evaluation transaction: %w", err)
	}
	outcome.Evaluation = created
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
			SchemaVersion:             memory.EvaluationSchemaVersionV1,
			ID:                        precheck.evaluationID,
			CandidateID:               options.CandidateID,
			EvalSetFingerprint:        precheck.evalSetFingerprint,
			BaselineConfigFingerprint: precheck.baselineFingerprint,
			VariantConfigFingerprint:  precheck.variantFingerprint,
			ComparatorVersion:         options.EvalSet.ComparatorContract.ComparatorVersion,
			Status:                    memory.EvaluationStatusRunning,
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
	ledger := &leaseLedger{}
	stop := &scheduleStop{}
	for _, step := range steps {
		if ctx.Err() != nil {
			stopReason = sandboxStopContextCancelled
			break
		}
		record, verdict, stepAbort := executor.executeScheduledStep(ctx, options, precheck, taskByID[step.TaskID], step, ledger, evaluationDir, stop)
		if record != nil {
			if writeErr := writeJSON(filepath.Join(evaluationDir, "runs", sandboxRecordFileName(step)), record); writeErr != nil {
				return conclusion, stopReason, stopDetail, records, verdicts, fmt.Errorf("persist sandbox run record: %w", writeErr)
			}
			records = append(records, *record)
		}
		if verdict != nil {
			verdicts = append(verdicts, *verdict)
		}
		if stepAbort != nil {
			stopReason, stopDetail = stop.reason, stop.detail
			return sandboxeval.Conclusion{Status: sandboxeval.ConclusionInvalid, Trigger: stepAbort.trigger}, stopReason, stopDetail, records, verdicts, nil
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
	record, verdict, stepAbort := executor.runSandboxAttempt(ctx, armConfig, prepared, runID, precheck.evaluationID, step, evaluationDir, stop)
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
func (executor SandboxExecutor) runSandboxAttempt(ctx context.Context, armConfig config.Config, prepared preparedAttempt, runID, evaluationID string, step SandboxPairStep, evaluationDir string, stop *scheduleStop) (*SandboxRunRecord, *sandboxeval.SuiteVerdict, *scheduleAbort) {
	record := &SandboxRunRecord{
		SchemaVersion: sandboxRecordSchemaV1,
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
		Task:      prepared.Task.AgentPrompt(),
		Workspace: prepared.Result.Workspace,
		StateDir:  prepared.Result.StateDir,
		Config:    armConfig,
	})
	record.WallTimeMS = executor.now().Sub(record.StartedAt).Milliseconds()
	if err != nil || result.ID == "" {
		// 无终态的 Provider 失效或装配异常：当步证据照常封口留档，调度以停
		// 止原因中断，套件按计划缺口收敛为不确定。
		class := sandboxStopProviderFailure
		if err != nil {
			class = contract.ClassifyError(err)
		}
		stop.reason = sandboxStopProviderFailure
		stop.detail = class
		return failWithoutVerdict(class)
	}
	inspected, inspectErr := executor.Service.Inspect(context.Background(), prepared.Result.Workspace, prepared.Result.StateDir, armConfig, result.ID, true)
	if inspectErr != nil {
		return abortInvalid(sandboxTriggerEvidenceUnverifiable, inspectErr)
	}
	completedVerified := inspected.Projection.State == domain.RunCompleted && inspected.Projection.VerificationSucceeded
	replayConsistent, replayErr := replayConsistentWithProjection(inspected.Projection, inspected.Events)
	if replayErr != nil {
		return abortInvalid(sandboxTriggerEvidenceIntegrityBroken, replayErr)
	}
	if !replayConsistent {
		return abortInvalid(sandboxTriggerReplayDivergence, errors.New("deterministic replay diverges from the persisted projection"))
	}
	record.ReplayConsistent = true
	frozen, marshalErr := json.Marshal(armConfig)
	if marshalErr != nil {
		return abortInvalid(sandboxTriggerContractProjectionFailed, marshalErr)
	}
	runResult, projectionErr := contract.ResultFromRun(inspected.Events, inspected.Projection, prepared.Result.Task, artifact.RedactJSON(frozen))
	if projectionErr != nil {
		return abortInvalid(sandboxTriggerContractProjectionFailed, projectionErr)
	}
	pairingBreaks, pairingErr := domain.ToolCallPairingBreaks(inspected.Events)
	if pairingErr != nil {
		return abortInvalid(sandboxTriggerToolPairingExtraction, pairingErr)
	}
	evidence, evidenceErr := domain.CollectRunEvidenceMetrics(inspected.Events)
	if evidenceErr != nil {
		return abortInvalid(sandboxTriggerExplorationSignatureMissing, evidenceErr)
	}
	doctorOK := true
	if _, doctorErr := executor.Service.Doctor(context.Background(), prepared.Result.Workspace, prepared.Result.StateDir, armConfig); doctorErr != nil {
		doctorOK = false
	}
	record.InputTokens = runResult.Metrics.InputTokens
	record.OutputTokens = runResult.Metrics.OutputTokens
	record.ToolPairingBreaks = pairingBreaks
	record.RepeatedExplorations = evidence.RepeatedExplorations
	record.DoctorOK = doctorOK
	if scanErr := finalizeSurfaces(); scanErr != nil {
		return record, nil, scanErr
	}
	record.Success = completedVerified
	if !completedVerified && record.FailureClass == "" {
		record.FailureClass = verificationFailedClass
	}
	record.FinishedAt = executor.now()
	return record, &sandboxeval.SuiteVerdict{
		Arm:                  step.Arm,
		TaskID:               step.TaskID,
		Success:              completedVerified,
		SecurityIncidents:    record.SecurityIncidents,
		DoctorReplayBroken:   !doctorOK || !replayConsistent,
		ToolPairingBreaks:    pairingBreaks,
		RepeatedExplorations: evidence.RepeatedExplorations,
		InputTokens:          record.InputTokens,
		OutputTokens:         record.OutputTokens,
		WallTimeMS:           record.WallTimeMS,
	}, nil
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

// fileSurfacePathsUnder 递归枚举泄漏扫描表面下的全部文件路径：目录跳过，
// 符号链接保留（打开即目标内容），枚举错误整体冒泡使扫描失败关闭。
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
			surfaces = append(surfaces, path)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("enumerate leak scan surfaces under %s: %w", filepath.Base(root), err)
		}
	}
	return surfaces, nil
}

// sandboxRecordFileName 将调度步映射为评测目录内稳定且可读的记录文件名；
// 权威身份仍以文件内 run_id 字段为准。
func sandboxRecordFileName(step SandboxPairStep) string {
	return safePathComponent(step.TaskID) + fmt.Sprintf("-a%02d-%s.json", step.Try, sandboxArmTag(step.Arm))
}

// persistSuiteResult 把套件结论原子落盘并读回计算字节摘要：Memory 侧只保存
// 该 sha256 引用，读回环节保证留存正文与其引用不可静默漂移。
func (executor SandboxExecutor) persistSuiteResult(options SandboxExecutorOptions, evaluationID string, startedAt time.Time, conclusion sandboxeval.Conclusion, stopReason, stopDetail string, records []SandboxRunRecord) (string, string, error) {
	result := SandboxSuiteResult{
		SchemaVersion: sandboxRecordSchemaV1,
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
	if err := writeJSON(resultPath, result); err != nil {
		return "", "", err
	}
	content, readErr := os.ReadFile(resultPath)
	if readErr != nil {
		return "", "", fmt.Errorf("read back suite result for digest: %w", readErr)
	}
	digest := sha256.Sum256(content)
	return resultPath, "sha256:" + hex.EncodeToString(digest[:]), nil
}
