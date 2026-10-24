package benchmark

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
)

// Runner 在独立 clone 中依次运行本地任务，并把每次尝试写入忽略的结果目录。
type Runner struct {
	Service              app.Service
	Now                  func() time.Time
	CloneAtBaseline      func(context.Context, Task, string, string) error
	ApplyPrivateVerifier func(context.Context, string, string, string) (string, error)
	RunBootstrap         func(context.Context, string, string, []Command) error
	DockerImageAvailable func(context.Context, string) error
}

// RunOptions 描述一次基准套件执行的本地任务、冻结基础配置和本地输出路径。
type RunOptions struct {
	Pack           TaskPack
	TaskIDs        []string
	Attempts       int
	Configuration  config.Config
	ResultsRoot    string
	WorkspacesRoot string
	StateRoot      string
	PrivateRoot    string
	CairnRoot      string
	PrepareTimeout time.Duration
	PrepareProxy   string
	PrepareOnly    bool
}

// SuiteResult 汇总一轮基准套件的环境、合同指纹与所有独立尝试。
type SuiteResult struct {
	SchemaVersion       int                   `json:"schema_version"`
	SuiteRunID          string                `json:"suite_run_id"`
	StartedAt           time.Time             `json:"started_at"`
	FinishedAt          time.Time             `json:"finished_at"`
	Outcome             string                `json:"outcome,omitempty"`
	EnvironmentValid    bool                  `json:"environment_valid"`
	TaskPackFingerprint string                `json:"task_pack_fingerprint"`
	CairnRevision       string                `json:"cairn_revision"`
	Qualifications      []QualificationResult `json:"qualifications,omitempty"`
	Preparations        []PreparationResult   `json:"preparations,omitempty"`
	Results             []AttemptResult       `json:"results"`
}

// QualificationResult 保存一次正式计分前独立环境资格检查的非敏感结果；它
// 不包含 Agent 运行、状态目录、工作区路径或模型调用，绝不能被计入 Agent 通过率。
type QualificationResult struct {
	SchemaVersion       int       `json:"schema_version"`
	TaskID              string    `json:"task_id"`
	Attempt             int       `json:"attempt"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	Outcome             string    `json:"outcome"`
	FailureClass        string    `json:"failure_class,omitempty"`
	Error               string    `json:"error,omitempty"`
	Workspace           string    `json:"-"`
	SourceRevision      string    `json:"source_revision,omitempty"`
	VerifierFingerprint string    `json:"verifier_fingerprint,omitempty"`
	DockerImage         string    `json:"docker_image,omitempty"`
	BootstrapDurationMS int64     `json:"bootstrap_duration_ms"`
}

// PreparationResult 保存正式 Agent 工作区启动前的独立准备记录；它与
// QualificationResult 的 probe clone 分离，且失败不会伪装为 Agent 尝试。路径
// 仅供进程内编排，绝不写入结果 JSON。
type PreparationResult struct {
	SchemaVersion       int       `json:"schema_version"`
	TaskID              string    `json:"task_id"`
	Attempt             int       `json:"attempt"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	Outcome             string    `json:"outcome"`
	FailureClass        string    `json:"failure_class,omitempty"`
	Error               string    `json:"error,omitempty"`
	Workspace           string    `json:"-"`
	StateDir            string    `json:"-"`
	SourceRevision      string    `json:"source_revision,omitempty"`
	VerifierFingerprint string    `json:"verifier_fingerprint,omitempty"`
	DockerImage         string    `json:"docker_image,omitempty"`
	BootstrapDurationMS int64     `json:"bootstrap_duration_ms"`
}

// preparedAttempt 只在正式工作区已完成全部环境准备后存在，携带启动 Agent
// 所需的固定配置与非敏感准备事实，避免后续重新 clone 或重新 bootstrap。
type preparedAttempt struct {
	Task          Task
	Attempt       int
	Result        AttemptResult
	Configuration config.Config
}

// AttemptResult 保存一次 clone、bootstrap、Agent 运行、验证和诊断的非敏感事实。
// SchemaVersion 2 起运行期身份与结果统一由 contract.TaskSpec 与 contract.Result
// 携带（与普通 Run 共用同一产品合同投影），本结构只保留基准特有的事实。
type AttemptResult struct {
	SchemaVersion       int               `json:"schema_version"`
	TaskID              string            `json:"task_id"`
	Attempt             int               `json:"attempt"`
	StartedAt           time.Time         `json:"started_at"`
	FinishedAt          time.Time         `json:"finished_at"`
	Outcome             string            `json:"outcome"`
	FailureClass        string            `json:"failure_class,omitempty"`
	Error               string            `json:"error,omitempty"`
	Workspace           string            `json:"-"`
	StateDir            string            `json:"-"`
	Task                contract.TaskSpec `json:"task_spec"`
	Run                 *contract.Result  `json:"run,omitempty"`
	DoctorOK            bool              `json:"doctor_ok"`
	Observations        AttemptMetrics    `json:"observations"`
	BootstrapDurationMS int64             `json:"bootstrap_duration_ms"`
	AgentDurationMS     int64             `json:"agent_duration_ms"`
	DiffPath            string            `json:"diff_path,omitempty"`
	DiffSHA256          string            `json:"diff_sha256,omitempty"`
}

// AttemptMetrics 只保存基准特有、不属于核心运行合同的观测：补丁提议/成功
// 轮次、首次补丁前探索、提出后无成功补丁、首次验证所在轮次、重复探索、
// 零 patch 耗尽轮次预算，以及按冻结单价由 contract.Result 的 token 事实
// 换算的成本。
//
// 语义边界（勿混用）：
//   - FirstPatchTurn 是 apply_patch 的首次工具提议轮次，不是成功写入；
//   - FirstSuccessfulPatchTurn 是首次 operation_succeeded 且 tool=apply_patch；
//   - PrePatchExplorationTurns 是首次 apply_patch 提议之前结束的轮数；若全程
//     未提出补丁，则等于本次观测到的全部轮数（刻画「只探索不动手」）；
//   - MaxNoSuccessfulPatchStreak 仅在首次补丁提议之后累计，表示连续没有
//     成功写入 apply_patch 的最大轮数（仅观测，不作为熔断判据）。
//
// 轮次与 token 总量以 Run.Metrics 为准，避免两处统计互相漂移。
type AttemptMetrics struct {
	FirstPatchTurn                int     `json:"first_patch_turn,omitempty"`
	FirstSuccessfulPatchTurn      int     `json:"first_successful_patch_turn,omitempty"`
	PrePatchExplorationTurns      int     `json:"pre_patch_exploration_turns"`
	MaxNoSuccessfulPatchStreak    int     `json:"max_no_successful_patch_streak"`
	FirstVerificationTurn         int     `json:"first_verification_turn,omitempty"`
	RepeatedReadSearchCalls       int     `json:"repeated_read_search_calls"`
	ZeroPatchTurnsBudgetExhausted bool    `json:"zero_patch_turns_budget_exhausted"`
	CostUSD                       float64 `json:"cost_usd"`
}

// Run 执行所选任务的每次独立尝试；单次失败会写入结果并继续，只有合同或本地环境错误才返回 error。
func (runner Runner) Run(ctx context.Context, options RunOptions) (SuiteResult, error) {
	if err := options.Pack.Validate(); err != nil {
		return SuiteResult{}, err
	}
	if options.Attempts < 1 {
		options.Attempts = options.Pack.Execution.AttemptsPerTask
	}
	if options.PrepareTimeout <= 0 {
		options.PrepareTimeout = 10 * time.Minute
	}
	if options.Attempts < 1 || options.ResultsRoot == "" || options.WorkspacesRoot == "" || options.StateRoot == "" || options.PrivateRoot == "" {
		return SuiteResult{}, errors.New("attempts and results, workspaces, state, and private roots are required")
	}
	if err := validatePrepareProxy(options.PrepareProxy); err != nil {
		return SuiteResult{}, err
	}
	if err := config.Validate(options.Configuration); err != nil {
		return SuiteResult{}, fmt.Errorf("base benchmark config: %w", err)
	}
	tasks, err := options.Pack.SelectedTasks(options.TaskIDs)
	if err != nil {
		return SuiteResult{}, err
	}
	now := runner.now()
	suiteID := "benchmark-" + now.UTC().Format("20060102T150405.000000000Z")
	resultDirectory := filepath.Join(options.ResultsRoot, suiteID)
	if err := os.MkdirAll(resultDirectory, 0o700); err != nil {
		return SuiteResult{}, fmt.Errorf("create result directory: %w", err)
	}
	revision, err := cairnRevision(options.CairnRoot)
	if err != nil {
		return SuiteResult{}, err
	}
	report := SuiteResult{SchemaVersion: 2, SuiteRunID: suiteID, StartedAt: now, TaskPackFingerprint: options.Pack.Fingerprint(), CairnRevision: revision, Results: make([]AttemptResult, 0)}
	qualificationsValid := true
	for _, task := range tasks {
		for attempt := 1; attempt <= options.Attempts; attempt++ {
			qualification := runner.qualifyAttempt(ctx, options, report, task, attempt)
			report.Qualifications = append(report.Qualifications, qualification)
			if qualification.Outcome != "qualified" {
				qualificationsValid = false
			}
			if err := writeJSON(filepath.Join(resultDirectory, qualificationFileName(task.ID, attempt)), qualification); err != nil {
				return SuiteResult{}, err
			}
		}
	}
	if !qualificationsValid {
		report.Outcome = "environment_invalid"
		report.FinishedAt = runner.now()
		if err := writeJSON(filepath.Join(resultDirectory, "suite-result.json"), report); err != nil {
			return SuiteResult{}, err
		}
		return report, nil
	}
	report.EnvironmentValid = true
	if options.PrepareOnly {
		report.Outcome = "prepared"
		report.FinishedAt = runner.now()
		if err := writeJSON(filepath.Join(resultDirectory, "suite-result.json"), report); err != nil {
			return SuiteResult{}, err
		}
		return report, nil
	}
	preparedAttempts := make([]preparedAttempt, 0, len(tasks)*options.Attempts)
	preparationsValid := true
	for _, task := range tasks {
		for attempt := 1; attempt <= options.Attempts; attempt++ {
			prepared, preparation := runner.prepareAgentAttempt(ctx, options, report, task, attempt)
			report.Preparations = append(report.Preparations, preparation)
			if preparation.Outcome != "prepared" {
				preparationsValid = false
				continue
			}
			preparedAttempts = append(preparedAttempts, prepared)
		}
	}
	if !preparationsValid {
		report.Outcome = "environment_invalid"
		report.EnvironmentValid = false
		report.Results = make([]AttemptResult, 0)
		report.FinishedAt = runner.now()
		if err := writeJSON(filepath.Join(resultDirectory, "suite-result.json"), report); err != nil {
			return SuiteResult{}, err
		}
		return report, nil
	}
	if options.Configuration.Provider.BaseURL == "" || options.Configuration.Provider.Model == "" {
		return SuiteResult{}, errors.New("benchmark run requires provider base_url and model")
	}
	if os.Getenv(options.Configuration.Provider.APIKeyEnv) == "" {
		return SuiteResult{}, fmt.Errorf("benchmark run requires provider API key environment variable %q", options.Configuration.Provider.APIKeyEnv)
	}
	for _, prepared := range preparedAttempts {
		result := runner.runPreparedAttempt(ctx, prepared, resultDirectory)
		report.Results = append(report.Results, result)
		if err := writeJSON(filepath.Join(resultDirectory, resultFileName(prepared.Task.ID, prepared.Attempt)), result); err != nil {
			return SuiteResult{}, err
		}
	}
	report.FinishedAt = runner.now()
	report.Outcome = "completed"
	if err := writeJSON(filepath.Join(resultDirectory, "suite-result.json"), report); err != nil {
		return SuiteResult{}, err
	}
	return report, nil
}

// prepareAgentAttempt 创建一个仅供正式 Agent 使用的全新工作区，并在 Provider
// 构造前完成 clone、基线、verifier、bootstrap 与镜像检查；任一失败由套件按
// environment_invalid 处理，绝不写入 AttemptResult。
func (runner Runner) prepareAgentAttempt(ctx context.Context, options RunOptions, report SuiteResult, task Task, attempt int) (preparedAttempt, PreparationResult) {
	started := runner.now()
	workspace := filepath.Join(options.WorkspacesRoot, report.SuiteRunID, safePathComponent(task.ID), fmt.Sprintf("attempt-%02d", attempt))
	stateDirectory := filepath.Join(options.StateRoot, report.SuiteRunID, safePathComponent(task.ID), fmt.Sprintf("attempt-%02d", attempt))
	preparation := PreparationResult{SchemaVersion: 1, TaskID: task.ID, Attempt: attempt, StartedAt: started, Outcome: "failed", Workspace: workspace, StateDir: stateDirectory, DockerImage: task.Runtime.DockerImage}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_prepare", err)
	}
	if err := os.MkdirAll(stateDirectory, 0o700); err != nil {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_prepare", err)
	}
	prepareContext, cancelPrepare := context.WithTimeout(ctx, options.PrepareTimeout)
	defer cancelPrepare()
	if err := runner.cloneAtBaseline(prepareContext, task, workspace, options.PrepareProxy, options.WorkspacesRoot); err != nil {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_prepare", err)
	}
	revision, err := gitRevision(prepareContext, workspace)
	if err != nil {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_prepare", err)
	}
	if revision != task.Repository.BaselineCommit {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_prepare", errors.New("baseline revision mismatch"))
	}
	preparation.SourceRevision = revision
	verifierFingerprint, err := runner.applyPrivateVerifier(prepareContext, workspace, options.PrivateRoot, task.ID)
	if err != nil {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_verifier", err)
	}
	preparation.VerifierFingerprint = verifierFingerprint
	bootstrapStarted := runner.now()
	if err := runner.runBootstrap(prepareContext, workspace, task.Runtime.DockerImage, task.Verification.Bootstrap); err != nil {
		preparation.BootstrapDurationMS = runner.now().Sub(bootstrapStarted).Milliseconds()
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_bootstrap", err)
	}
	preparation.BootstrapDurationMS = runner.now().Sub(bootstrapStarted).Milliseconds()
	if err := runner.dockerImageAvailable(prepareContext, task.Runtime.DockerImage); err != nil {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_sandbox", err)
	}
	configuration, err := configurationForTask(options.Configuration, task)
	if err != nil {
		return preparedAttempt{}, preparation.finish(runner.now()).fail("preparation_config", err)
	}
	preparation.Outcome = "prepared"
	preparation = preparation.finish(runner.now())
	// 任务身份在准备阶段即冻结：TaskPack、套件、基线与 verifier 摘要组成
	// contract.TaskSpec 的 benchmark 投影，prompt 摘要与正式 Agent 提示同源。
	taskSpec := contract.BenchmarkTaskSpec(task.AgentPrompt(), contract.BenchmarkTaskRef{
		TaskID:              task.ID,
		SuiteID:             report.SuiteRunID,
		TaskPackFingerprint: report.TaskPackFingerprint,
		RepositoryName:      task.Repository.Name,
		RepositoryURL:       task.Repository.URL,
		BaselineCommit:      task.Repository.BaselineCommit,
		SourceRevision:      revision,
		VerifierFingerprint: verifierFingerprint,
		DockerImage:         task.Runtime.DockerImage,
	})
	attemptResult := AttemptResult{SchemaVersion: 2, TaskID: task.ID, Attempt: attempt, StartedAt: started, Outcome: "failed", Workspace: workspace, StateDir: stateDirectory, Task: taskSpec, BootstrapDurationMS: preparation.BootstrapDurationMS}
	return preparedAttempt{Task: task, Attempt: attempt, Result: attemptResult, Configuration: configuration}, preparation
}

// Passed 报告所有实际 Agent 尝试是否都通过固定验证和 journal 诊断。
func (result SuiteResult) Passed() bool {
	return result.Outcome != "environment_invalid" && len(result.Results) != 0 && allAttemptsPassed(result.Results)
}

// qualifyAttempt 在正式 Agent 尝试前创建独立 clone，验证固定 baseline、私有
// verifier、bootstrap 和任务镜像；失败只会使整套评测环境无效，不启动模型。
func (runner Runner) qualifyAttempt(ctx context.Context, options RunOptions, report SuiteResult, task Task, attempt int) QualificationResult {
	started := runner.now()
	workspace := filepath.Join(options.WorkspacesRoot, report.SuiteRunID, "qualification", safePathComponent(task.ID), fmt.Sprintf("attempt-%02d", attempt))
	result := QualificationResult{SchemaVersion: 1, TaskID: task.ID, Attempt: attempt, StartedAt: started, Outcome: "failed", Workspace: workspace, DockerImage: task.Runtime.DockerImage}
	if err := os.MkdirAll(filepath.Dir(workspace), 0o700); err != nil {
		return result.finish(runner.now()).fail("qualification_prepare", err)
	}
	prepareContext, cancelPrepare := context.WithTimeout(ctx, options.PrepareTimeout)
	defer cancelPrepare()
	if err := runner.cloneAtBaseline(prepareContext, task, workspace, options.PrepareProxy, options.WorkspacesRoot); err != nil {
		return result.finish(runner.now()).fail("qualification_prepare", err)
	}
	revision, err := gitRevision(prepareContext, workspace)
	if err != nil {
		return result.finish(runner.now()).fail("qualification_prepare", err)
	}
	if revision != task.Repository.BaselineCommit {
		return result.finish(runner.now()).fail("qualification_prepare", fmt.Errorf("checked out revision %s does not match baseline %s", revision, task.Repository.BaselineCommit))
	}
	result.SourceRevision = revision
	verifierFingerprint, err := runner.applyPrivateVerifier(prepareContext, workspace, options.PrivateRoot, task.ID)
	if err != nil {
		return result.finish(runner.now()).fail("qualification_verifier", err)
	}
	result.VerifierFingerprint = verifierFingerprint
	bootstrapStarted := runner.now()
	if err := runner.runBootstrap(prepareContext, workspace, task.Runtime.DockerImage, task.Verification.Bootstrap); err != nil {
		result.BootstrapDurationMS = runner.now().Sub(bootstrapStarted).Milliseconds()
		return result.finish(runner.now()).fail("qualification_bootstrap", err)
	}
	result.BootstrapDurationMS = runner.now().Sub(bootstrapStarted).Milliseconds()
	if err := runner.dockerImageAvailable(prepareContext, task.Runtime.DockerImage); err != nil {
		return result.finish(runner.now()).fail("qualification_sandbox", err)
	}
	result.Outcome = "qualified"
	return result.finish(runner.now())
}

// runPreparedAttempt 只在 prepareAgentAttempt 已完整成功后调用 Provider；它
// 不再执行 clone、verifier、bootstrap 或镜像检查，从而使这些环境失败无法进入
// Agent 计分结果。运行期身份与结果统一经 Service.Evidence 从已落盘的
// config_ref 投影，与普通 Run 共用同一事实源。
func (runner Runner) runPreparedAttempt(ctx context.Context, prepared preparedAttempt, resultDirectory string) AttemptResult {
	result := prepared.Result
	agentStarted := runner.now()
	run, runErr := runner.Service.Run(ctx, app.RunOptions{Task: prepared.Task.AgentPrompt(), Workspace: result.Workspace, StateDir: result.StateDir, Config: prepared.Configuration, DisableUserInstructions: true})
	result.AgentDurationMS = runner.now().Sub(agentStarted).Milliseconds()
	completedTrusted := false
	if run.ID != "" {
		inspected, inspectErr := runner.Service.Inspect(context.Background(), result.Workspace, result.StateDir, prepared.Configuration, run.ID, true)
		if inspectErr == nil {
			if runResult, contractErr := runner.Service.Evidence(context.Background(), result.Workspace, result.StateDir, prepared.Configuration, run.ID); contractErr == nil {
				completedTrusted = runResult.Evidence.Trusted
				runResult.Manifest.Task = prepared.Result.Task
				result.Run = &runResult
				result.Observations.CostUSD = float64(runResult.Metrics.InputTokens)*prepared.Configuration.Pricing.InputTokenUSD + float64(runResult.Metrics.OutputTokens)*prepared.Configuration.Pricing.OutputTokenUSD
			}
			result.Observations = mergeObservations(result.Observations, observationsFromEvents(inspected.Events))
		}
		if _, doctorErr := runner.Service.Doctor(context.Background(), result.Workspace, result.StateDir, prepared.Configuration); doctorErr == nil {
			result.DoctorOK = true
		} else if runErr == nil {
			runErr = doctorErr
		}
	}
	result = result.finish(runner.now())
	if runErr != nil {
		return runner.captureDiff(ctx, result.Workspace, resultDirectory, result.fail(contract.ClassifyError(runErr), runErr))
	}
	if result.Run == nil || !completedTrusted || !result.DoctorOK {
		return runner.captureDiff(ctx, result.Workspace, resultDirectory, result.fail(contract.FailureVerification, errors.New("run did not complete with trusted verification and healthy state")))
	}
	result.Outcome = "passed"
	return runner.captureDiff(ctx, result.Workspace, resultDirectory, result)
}

// mergeObservations 把事件推导的基准观测合并进已含成本的结果；成本来自
// contract.Result 的 token 事实，事件侧只补轮次类观测，互不覆盖来源。
func mergeObservations(target AttemptMetrics, derived AttemptMetrics) AttemptMetrics {
	target.FirstPatchTurn = derived.FirstPatchTurn
	target.FirstSuccessfulPatchTurn = derived.FirstSuccessfulPatchTurn
	target.PrePatchExplorationTurns = derived.PrePatchExplorationTurns
	target.MaxNoSuccessfulPatchStreak = derived.MaxNoSuccessfulPatchStreak
	target.FirstVerificationTurn = derived.FirstVerificationTurn
	target.RepeatedReadSearchCalls = derived.RepeatedReadSearchCalls
	target.ZeroPatchTurnsBudgetExhausted = derived.ZeroPatchTurnsBudgetExhausted
	return target
}

// applyPrivateVerifier 将 evaluator-only patch 应用于 disposable clone；补丁内容
// 始终留在忽略目录，结果只保存其摘要，且缺失补丁会阻止正式基准运行。
