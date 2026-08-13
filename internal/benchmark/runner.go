package benchmark

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
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
// 不包含 Agent 运行、状态目录或模型调用，绝不能被计入 Agent 通过率。
type QualificationResult struct {
	SchemaVersion       int       `json:"schema_version"`
	TaskID              string    `json:"task_id"`
	Attempt             int       `json:"attempt"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	Outcome             string    `json:"outcome"`
	FailureClass        string    `json:"failure_class,omitempty"`
	Error               string    `json:"error,omitempty"`
	Workspace           string    `json:"workspace"`
	SourceRevision      string    `json:"source_revision,omitempty"`
	VerifierFingerprint string    `json:"verifier_fingerprint,omitempty"`
	DockerImage         string    `json:"docker_image,omitempty"`
	BootstrapDurationMS int64     `json:"bootstrap_duration_ms"`
}

// PreparationResult 保存正式 Agent 工作区启动前的独立准备记录；它与
// QualificationResult 的 probe clone 分离，且失败不会伪装为 Agent 尝试。
type PreparationResult struct {
	SchemaVersion       int       `json:"schema_version"`
	TaskID              string    `json:"task_id"`
	Attempt             int       `json:"attempt"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	Outcome             string    `json:"outcome"`
	FailureClass        string    `json:"failure_class,omitempty"`
	Error               string    `json:"error,omitempty"`
	Workspace           string    `json:"workspace"`
	StateDir            string    `json:"state_dir"`
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
type AttemptResult struct {
	SchemaVersion         int          `json:"schema_version"`
	TaskID                string       `json:"task_id"`
	Attempt               int          `json:"attempt"`
	StartedAt             time.Time    `json:"started_at"`
	FinishedAt            time.Time    `json:"finished_at"`
	Outcome               string       `json:"outcome"`
	FailureClass          string       `json:"failure_class,omitempty"`
	Error                 string       `json:"error,omitempty"`
	Workspace             string       `json:"workspace"`
	StateDir              string       `json:"state_dir"`
	SourceRevision        string       `json:"source_revision,omitempty"`
	TaskPackFingerprint   string       `json:"task_pack_fingerprint"`
	VerifierFingerprint   string       `json:"verifier_fingerprint,omitempty"`
	ConfigFingerprint     string       `json:"config_fingerprint,omitempty"`
	ProviderBaseURL       string       `json:"provider_base_url,omitempty"`
	Model                 string       `json:"model,omitempty"`
	DockerImage           string       `json:"docker_image,omitempty"`
	BootstrapDurationMS   int64        `json:"bootstrap_duration_ms"`
	AgentDurationMS       int64        `json:"agent_duration_ms"`
	RunID                 domain.RunID `json:"run_id,omitempty"`
	RunState              string       `json:"run_state,omitempty"`
	VerificationSucceeded bool         `json:"verification_succeeded"`
	DoctorOK              bool         `json:"doctor_ok"`
	Metrics               RunMetrics   `json:"metrics"`
	DiffPath              string       `json:"diff_path,omitempty"`
	DiffSHA256            string       `json:"diff_sha256,omitempty"`
}

// RunMetrics 从持久化事件汇总轮次、工具调用、token 与按冻结单价计算的成本。
type RunMetrics struct {
	Turns        int     `json:"turns"`
	ToolCalls    int     `json:"tool_calls"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
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
	if err := runner.cloneAtBaseline(prepareContext, task, workspace, options.PrepareProxy); err != nil {
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
	attemptResult := AttemptResult{SchemaVersion: 1, TaskID: task.ID, Attempt: attempt, StartedAt: started, Outcome: "failed", Workspace: workspace, StateDir: stateDirectory, SourceRevision: revision, TaskPackFingerprint: report.TaskPackFingerprint, VerifierFingerprint: verifierFingerprint, ConfigFingerprint: configuration.Fingerprint(), ProviderBaseURL: configuration.Provider.BaseURL, Model: configuration.Provider.Model, DockerImage: configuration.Sandbox.DockerImage, BootstrapDurationMS: preparation.BootstrapDurationMS}
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
	if err := runner.cloneAtBaseline(prepareContext, task, workspace, options.PrepareProxy); err != nil {
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
// Agent 计分结果。
func (runner Runner) runPreparedAttempt(ctx context.Context, prepared preparedAttempt, resultDirectory string) AttemptResult {
	result := prepared.Result
	agentStarted := runner.now()
	run, runErr := runner.Service.Run(ctx, app.RunOptions{Task: prepared.Task.AgentPrompt(), Workspace: result.Workspace, StateDir: result.StateDir, Config: prepared.Configuration})
	result.AgentDurationMS = runner.now().Sub(agentStarted).Milliseconds()
	result.RunID = run.ID
	if run.ID != "" {
		inspected, inspectErr := runner.Service.Inspect(context.Background(), result.Workspace, result.StateDir, prepared.Configuration, run.ID, true)
		if inspectErr == nil {
			result.RunState = inspected.Projection.Phase()
			result.VerificationSucceeded = inspected.Projection.VerificationSucceeded
			result.Metrics = metricsFromEvents(inspected.Events, prepared.Configuration)
		}
		if doctorErr := runner.Service.Doctor(context.Background(), result.Workspace, result.StateDir, prepared.Configuration); doctorErr == nil {
			result.DoctorOK = true
		} else if runErr == nil {
			runErr = doctorErr
		}
	}
	result = result.finish(runner.now())
	if runErr != nil {
		return runner.captureDiff(ctx, result.Workspace, resultDirectory, result.fail(classifyFailure(runErr), runErr))
	}
	if result.RunState != string(domain.RunCompleted) || !result.VerificationSucceeded || !result.DoctorOK {
		return runner.captureDiff(ctx, result.Workspace, resultDirectory, result.fail("verification", errors.New("run did not complete with verified and healthy state")))
	}
	result.Outcome = "passed"
	return runner.captureDiff(ctx, result.Workspace, resultDirectory, result)
}

// applyPrivateVerifier 将 evaluator-only patch 应用于 disposable clone；补丁内容
// 始终留在忽略目录，结果只保存其摘要，且缺失补丁会阻止正式基准运行。
func applyPrivateVerifier(ctx context.Context, workspace, privateRoot, taskID string) (string, error) {
	root, err := filepath.Abs(privateRoot)
	if err != nil {
		return "", err
	}
	patchPath := filepath.Join(root, "verifiers", safePathComponent(taskID)+".patch")
	contents, err := os.ReadFile(patchPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("private verifier patch is required at %s", patchPath)
		}
		return "", fmt.Errorf("read private verifier patch: %w", err)
	}
	if len(contents) == 0 {
		return "", errors.New("private verifier patch is empty")
	}
	if err := runProcess(ctx, workspace, []string{"git", "apply", "--check", patchPath}); err != nil {
		return "", fmt.Errorf("private verifier check: %w", err)
	}
	if err := runProcess(ctx, workspace, []string{"git", "apply", patchPath}); err != nil {
		return "", fmt.Errorf("apply private verifier: %w", err)
	}
	digest := sha256.Sum256(contents)
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

// configurationForTask 冻结任务专属的 Docker 镜像和固定验证命令，拒绝 host 后端与审批暂停。
func configurationForTask(base config.Config, task Task) (config.Config, error) {
	configuration := base
	configuration.Sandbox.Backend = "docker"
	configuration.Sandbox.DockerImage = task.Runtime.DockerImage
	verificationEnvironment := make(map[string]string, len(task.Verification.Command.Env))
	for key, value := range task.Verification.Command.Env {
		verificationEnvironment[key] = value
		configuration.Policy.EnvAllow = append(configuration.Policy.EnvAllow, key)
	}
	configuration.Verification.VolatilePaths = append([]string(nil), task.Verification.VolatilePaths...)
	configuration.Verification.Commands = []config.VerificationCommand{{Argv: append([]string(nil), task.Verification.Command.Argv...), CWD: task.Verification.Command.CWD, Timeout: "10m", MaxOutputBytes: 1024 * 1024, Env: verificationEnvironment}}
	if configuration.Policy.Write != "allow" || configuration.Policy.Exec != "allow" {
		return config.Config{}, errors.New("benchmark config requires policy.write=allow and policy.exec=allow for unattended audited runs")
	}
	if err := config.Validate(configuration); err != nil {
		return config.Config{}, err
	}
	return configuration, nil
}

// cloneAtBaseline 调用可替换的资格/正式 clone 实现；测试替身只能替换该
// 受控网络边界，生产默认仍使用固定 argv 的 cloneAtBaseline 函数。
func (runner Runner) cloneAtBaseline(ctx context.Context, task Task, workspace, prepareProxy string) error {
	if runner.CloneAtBaseline != nil {
		return runner.CloneAtBaseline(ctx, task, workspace, prepareProxy)
	}
	return cloneAtBaseline(ctx, task, workspace, prepareProxy)
}

// applyPrivateVerifier 调用可替换的 evaluator-only verifier 注入，实现
// qualification 与正式尝试共享同一摘要契约而不共享工作区。
func (runner Runner) applyPrivateVerifier(ctx context.Context, workspace, privateRoot, taskID string) (string, error) {
	if runner.ApplyPrivateVerifier != nil {
		return runner.ApplyPrivateVerifier(ctx, workspace, privateRoot, taskID)
	}
	return applyPrivateVerifier(ctx, workspace, privateRoot, taskID)
}

// runBootstrap 调用可替换的无网络 Docker 预备步骤，保留正式执行时的
// fail-closed 默认实现，并让单元测试无需真实 Docker。
func (runner Runner) runBootstrap(ctx context.Context, workspace, image string, commands []Command) error {
	if runner.RunBootstrap != nil {
		return runner.RunBootstrap(ctx, workspace, image, commands)
	}
	return runBootstrap(ctx, workspace, image, commands)
}

// dockerImageAvailable 调用可替换的本地镜像检查，生产路径绝不隐式拉取镜像。
func (runner Runner) dockerImageAvailable(ctx context.Context, image string) error {
	if runner.DockerImageAvailable != nil {
		return runner.DockerImageAvailable(ctx, image)
	}
	return dockerImageAvailable(ctx, image)
}

// cloneAtBaseline 通过 argv 固定从合同 URL 创建新的 clone 并检出不可变基线提交。
func cloneAtBaseline(ctx context.Context, task Task, workspace, prepareProxy string) error {
	if err := runProcessWithProxy(ctx, "", []string{"git", "-c", "http.version=HTTP/1.1", "clone", "--filter=blob:none", "--no-checkout", task.Repository.URL, workspace}, prepareProxy); err != nil {
		return fmt.Errorf("clone %s: %w", task.Repository.Name, err)
	}
	if err := runProcessWithProxy(ctx, workspace, []string{"git", "-c", "http.version=HTTP/1.1", "checkout", "--detach", task.Repository.BaselineCommit}, prepareProxy); err != nil {
		return fmt.Errorf("checkout baseline %s: %w", task.Repository.BaselineCommit, err)
	}
	return nil
}

// runBootstrap 在计费窗口之外按合同 argv 在任务专属 Docker 镜像中准备依赖；它
// 不经过 Agent 工具或模型上下文，也不把代理、模型密钥或宿主机 HOME 传给目标仓库代码。
func runBootstrap(ctx context.Context, workspace, image string, commands []Command) error {
	for index, command := range commands {
		arguments, err := bootstrapDockerArguments(workspace, image, command)
		if err != nil {
			return err
		}
		if err := runProcess(ctx, "", arguments); err != nil {
			return fmt.Errorf("bootstrap command %d: %w", index, err)
		}
	}
	return nil
}

// bootstrapDockerArguments 将合同的预备命令收敛为无网络、只读根文件系统的 Docker
// argv；工作区是唯一可写 bind mount，供任务镜像已经具备的本地编译缓存按需复用。
func bootstrapDockerArguments(workspace, image string, command Command) ([]string, error) {
	if _, err := commandDirectory(workspace, command.CWD); err != nil {
		return nil, err
	}
	if strings.Contains(workspace, ",") {
		return nil, errors.New("benchmark docker workspace path cannot contain a comma")
	}
	if image == "" || image != strings.TrimSpace(image) || strings.HasPrefix(image, "-") || strings.ContainsAny(image, " \t\r\n\x00") {
		return nil, errors.New("benchmark bootstrap requires a valid task docker image")
	}
	containerCWD := "/workspace"
	if command.CWD != "." {
		containerCWD = path.Join(containerCWD, filepath.ToSlash(command.CWD))
	}
	arguments := []string{
		"docker", "run", "--rm",
		"--pull", "never",
		"--network", "none",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", "64",
		"--mount", "type=bind,src=" + workspace + ",dst=/workspace",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=64m",
		"--workdir", containerCWD,
		"--env", "HOME=/tmp",
		"--env", "TMPDIR=/tmp",
		image,
	}
	return append(arguments, command.Argv...), nil
}

// dockerImageAvailable 确保 Agent 运行前镜像已经在本地，runner 不会隐式拉取镜像。
func dockerImageAvailable(ctx context.Context, image string) error {
	if err := runProcess(ctx, "", []string{"docker", "image", "inspect", image}); err != nil {
		return fmt.Errorf("required docker image %q is unavailable locally: %w", image, err)
	}
	return nil
}

// captureDiff 将工作区最终 diff 写到本地结果目录，并把稳定摘要放入结果 schema。
func (runner Runner) captureDiff(ctx context.Context, workspace, resultDirectory string, result AttemptResult) AttemptResult {
	diff, err := runProcessOutput(ctx, workspace, []string{"git", "diff", "--binary"})
	if err != nil {
		return result
	}
	path := filepath.Join(resultDirectory, resultFileName(result.TaskID, result.Attempt)+".diff")
	if err := os.WriteFile(path, diff, 0o600); err != nil {
		return result
	}
	digest := sha256.Sum256(diff)
	result.DiffPath = filepath.Base(path)
	result.DiffSHA256 = "sha256:" + hex.EncodeToString(digest[:])
	return result
}

// fail 在不丢失已收集证据的前提下标记可汇总的失败类别。
func (result AttemptResult) fail(class string, err error) AttemptResult {
	result.Outcome = "failed"
	result.FailureClass = class
	if err != nil {
		result.Error = err.Error()
	}
	return result
}

// finish 固定尝试结束时间，避免结果写入时间混入 Agent 墙钟预算。
func (result AttemptResult) finish(finished time.Time) AttemptResult {
	result.FinishedAt = finished
	return result
}

// fail 标记资格检查的稳定失败类别；结果中只保留有限摘要，绝不持久化 clone、
// verifier 或 Docker 的底层诊断。
func (result QualificationResult) fail(class string, err error) QualificationResult {
	result.Outcome = "failed"
	result.FailureClass = class
	result.Error = preparationErrorSummary(class, err)
	return result
}

// finish 固定资格检查结束时间，使其与 Agent 墙钟和模型预算完全分离。
func (result QualificationResult) finish(finished time.Time) QualificationResult {
	result.FinishedAt = finished
	return result
}

// fail 标记正式 Agent workspace 的准备失败；它采用与 qualification 相同的
// 脱敏错误摘要，以便 suite 环境无效报告不泄露私有 verifier 或命令输出。
func (result PreparationResult) fail(class string, err error) PreparationResult {
	result.Outcome = "failed"
	result.FailureClass = class
	result.Error = preparationErrorSummary(class, err)
	return result
}

// finish 固定正式准备结束时间，使环境用时不混入 Agent 墙钟预算。
func (result PreparationResult) finish(finished time.Time) PreparationResult {
	result.FinishedAt = finished
	return result
}

// preparationErrorSummary 将底层错误收敛为固定非敏感分类；调用方仍可从
// FailureClass 判断失败阶段，但不会把 URL、路径、命令输出或凭据写入结果。
func preparationErrorSummary(class string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "preparation timed out"
	}
	switch class {
	case "qualification_prepare", "preparation_prepare":
		return "source preparation failed"
	case "qualification_verifier", "preparation_verifier":
		return "private verifier preparation failed"
	case "qualification_bootstrap", "preparation_bootstrap":
		return "bootstrap preparation failed"
	case "qualification_sandbox", "preparation_sandbox":
		return "task sandbox preparation failed"
	case "preparation_config":
		return "task configuration preparation failed"
	default:
		return "environment preparation failed"
	}
}

// now 返回可测试的 UTC 时钟；零值 Runner 使用系统时间。
func (runner Runner) now() time.Time {
	if runner.Now != nil {
		return runner.Now().UTC()
	}
	return time.Now().UTC()
}

// cairnRevision 获取当前 Cairn 代码版本，缺失 Git 元数据时拒绝产生不可追溯结果。
func cairnRevision(root string) (string, error) {
	if root == "" {
		current, err := os.Getwd()
		if err != nil {
			return "", err
		}
		root = current
	}
	output, err := runProcessOutput(context.Background(), root, []string{"git", "rev-parse", "HEAD"})
	if err != nil {
		return "", fmt.Errorf("resolve Cairn revision: %w", err)
	}
	return strings.TrimSpace(string(output)), nil
}

// gitRevision 返回 disposable clone 当前检出的完整提交，用于验证它确为所声明的基线。
func gitRevision(ctx context.Context, workspace string) (string, error) {
	output, err := runProcessOutput(ctx, workspace, []string{"git", "rev-parse", "HEAD"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(output)), nil
}

// commandDirectory 将合同 cwd 解析为 clone 内路径，并再次检查目录存在性。
func commandDirectory(workspace, relative string) (string, error) {
	if err := validateRelativePath(relative); err != nil {
		return "", err
	}
	directory := filepath.Join(workspace, relative)
	info, err := os.Stat(directory)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("command cwd %q is not a workspace directory", relative)
	}
	return directory, nil
}

// runProcess 执行受 runner 控制的 argv，并在失败时返回截断的非敏感 stderr 摘要。
func runProcess(ctx context.Context, directory string, argv []string) error {
	_, err := runProcessOutput(ctx, directory, argv)
	return err
}

// runProcessWithProxy 只向用户显式指定的无凭据 loopback proxy 传递预备网络流量。
func runProcessWithProxy(ctx context.Context, directory string, argv []string, prepareProxy string) error {
	_, err := runProcessOutputWithProxy(ctx, directory, argv, prepareProxy)
	return err
}

// runProcessOutput 执行不经过 shell 的 argv，并限制诊断输出以保护结果文件体积。
func runProcessOutput(ctx context.Context, directory string, argv []string) ([]byte, error) {
	return runProcessOutputWithProxy(ctx, directory, argv, "")
}

// runProcessOutputWithProxy 在受限环境中执行预备 argv，并按需传递已校验的本地代理地址。
func runProcessOutputWithProxy(ctx context.Context, directory string, argv []string, prepareProxy string) ([]byte, error) {
	if len(argv) == 0 || argv[0] == "" {
		return nil, errors.New("command argv is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command := exec.Command(argv[0], argv[1:]...)
	command.Dir = directory
	command.Env = restrictedProcessEnvironment(prepareProxy)
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var outputBuffer bytes.Buffer
	command.Stdout = &outputBuffer
	command.Stderr = &outputBuffer
	if err := command.Start(); err != nil {
		return nil, err
	}
	finished := make(chan error, 1)
	go func() { finished <- command.Wait() }()
	var err error
	select {
	case err = <-finished:
	case <-ctx.Done():
		_ = syscall.Kill(-command.Process.Pid, syscall.SIGTERM)
		grace := time.NewTimer(2 * time.Second)
		select {
		case <-finished:
			if !grace.Stop() {
				select {
				case <-grace.C:
				default:
				}
			}
		case <-grace.C:
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
			<-finished
		}
		return truncateProcessOutput(outputBuffer.Bytes()), ctx.Err()
	}
	output := truncateProcessOutput(outputBuffer.Bytes())
	if err != nil {
		return output, fmt.Errorf("%s: %w: %s", strings.Join(argv, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
}

// truncateProcessOutput 为失败摘要和私有结果制品保留有限、可诊断的命令输出。
func truncateProcessOutput(output []byte) []byte {
	if len(output) > 64*1024 {
		output = append(append([]byte(nil), output[:64*1024]...), []byte("\n[output truncated]\n")...)
	}
	return output
}

// restrictedProcessEnvironment 为 clone 和受控 Docker CLI 只提供运行工具所需的基础
// 变量，不把模型 API key、代理凭据或用户 HOME 中的私有状态交给目标仓库代码。
func restrictedProcessEnvironment(prepareProxy string) []string {
	environment := []string{"HOME=" + os.TempDir(), "TMPDIR=" + os.TempDir(), "LANG=C", "GIT_TERMINAL_PROMPT=0"}
	if path := os.Getenv("PATH"); path != "" {
		environment = append(environment, "PATH="+path)
	}
	if prepareProxy != "" {
		environment = append(environment, "HTTP_PROXY="+prepareProxy, "HTTPS_PROXY="+prepareProxy, "http_proxy="+prepareProxy, "https_proxy="+prepareProxy)
	}
	return environment
}

// validatePrepareProxy 只允许显式、无凭据的回环 HTTP 代理，防止将公司代理或
// 用户环境中的认证信息交给目标仓库的 clone 进程。
func validatePrepareProxy(value string) error {
	if value == "" {
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("prepare proxy must be a credential-free http loopback URL")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "127.0.0.1" && host != "localhost" && host != "::1" {
		return errors.New("prepare proxy host must be loopback")
	}
	if parsed.Port() == "" {
		return errors.New("prepare proxy port is required")
	}
	return nil
}

// metricsFromEvents 汇总持久化事实，避免从模型文本或 CLI 输出猜测资源消耗。
func metricsFromEvents(events []domain.Event, configuration config.Config) RunMetrics {
	var metrics RunMetrics
	for _, event := range events {
		switch event.Type {
		case domain.EventTurnOpened:
			metrics.Turns++
		case domain.EventToolCallProposed:
			metrics.ToolCalls++
		case domain.EventModelResponseFinished:
			var payload struct {
				Usage *struct {
					PromptTokens     int `json:"prompt_tokens"`
					CompletionTokens int `json:"completion_tokens"`
				} `json:"usage"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Usage != nil {
				metrics.InputTokens += payload.Usage.PromptTokens
				metrics.OutputTokens += payload.Usage.CompletionTokens
			}
		}
	}
	metrics.CostUSD = float64(metrics.InputTokens)*configuration.Pricing.InputTokenUSD + float64(metrics.OutputTokens)*configuration.Pricing.OutputTokenUSD
	return metrics
}

// classifyFailure 将可恢复暂停、策略、预算与执行异常归入可用于基准汇总的稳定类别。
func classifyFailure(err error) string {
	var waiting *agent.WaitingApprovalError
	var denied *agent.PolicyDeniedError
	var exhausted *agent.BudgetExceededError
	switch {
	case errors.As(err, &waiting):
		return "approval"
	case errors.As(err, &denied):
		return "policy"
	case errors.As(err, &exhausted):
		return "budget_" + exhausted.Dimension
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	default:
		return "agent"
	}
}

// writeJSON 原子写入结果 JSON，避免被中断的评测留下半个可读取文件。
func writeJSON(path string, value any) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(encoded, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

// resultFileName 将本地任务 ID 映射为不会逃出结果目录的稳定文件名。
func resultFileName(taskID string, attempt int) string {
	return safePathComponent(taskID) + fmt.Sprintf("-attempt-%02d.json", attempt)
}

// qualificationFileName 为环境资格记录使用与正式尝试不冲突的稳定文件名。
func qualificationFileName(taskID string, attempt int) string {
	return safePathComponent(taskID) + fmt.Sprintf("-qualification-%02d.json", attempt)
}

// safePathComponent 将任务 ID 规整为本地目录名，保留字母、数字、点、下划线和连字符。
func safePathComponent(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '-' || character == '_' || character == '.' {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "task"
	}
	return builder.String()
}

// allAttemptsPassed 判断套件是否只包含成功的实际 Agent 尝试。
func allAttemptsPassed(results []AttemptResult) bool {
	for _, result := range results {
		if result.Outcome != "passed" {
			return false
		}
	}
	return true
}

// SortResults 按任务 ID 与尝试序号稳定排序，供报告读取方独立复现展示顺序。
func SortResults(results []AttemptResult) {
	sort.Slice(results, func(left, right int) bool {
		if results[left].TaskID == results[right].TaskID {
			return results[left].Attempt < results[right].Attempt
		}
		return results[left].TaskID < results[right].TaskID
	})
}
