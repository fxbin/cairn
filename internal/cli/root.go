// Package cli 提供 Cairn 的窄命令行边界与稳定退出码。
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/benchmark"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/memory"
	"github.com/fxbin/cairn/internal/sandboxeval"
)

const (
	ExitOK        = 0
	ExitUsage     = 2
	ExitDenied    = 3
	ExitFailed    = 4
	ExitCancelled = 5
	ExitApproval  = 6
	ExitReconcile = 7
	ExitCorrupt   = 8
)

// Command 保存 CLI 依赖及标准输出、错误输出目标。
type Command struct {
	Service app.Service
	Stdout  io.Writer
	Stderr  io.Writer
}

// Run 使用默认 Command 执行一组 CLI 参数并返回进程退出码。
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return Command{Stdout: stdout, Stderr: stderr}.Run(ctx, args)
}

// Run 分派顶级命令，并为缺失输出目标填充标准流。
func (command Command) Run(ctx context.Context, args []string) int {
	if command.Stdout == nil {
		command.Stdout = os.Stdout
	}
	if command.Stderr == nil {
		command.Stderr = os.Stderr
	}
	if len(args) == 0 {
		command.usage()
		return ExitUsage
	}
	switch args[0] {
	case "run":
		return command.run(ctx, args[1:])
	case "inspect":
		return command.inspect(ctx, args[1:])
	case "evidence":
		return command.evidence(ctx, args[1:])
	case "doctor":
		return command.doctor(ctx, args[1:])
	case "memory":
		return command.memory(ctx, args[1:])
	case "resume":
		return command.resume(ctx, args[1:])
	case "benchmark":
		return command.benchmark(ctx, args[1:])
	case "check-provider":
		return command.checkProvider(ctx, args[1:])
	case "help", "--help", "-h":
		command.usage()
		return ExitOK
	default:
		fmt.Fprintf(command.Stderr, "unknown command %q\n", args[0])
		command.usage()
		return ExitUsage
	}
}

// benchmark 分派本地评测域的固定子命令：run 执行基准套件；freeze-eval-set
// 把本地冻结资产装配为受控评测集与凭据策略文件。它不接受任意 shell 命令。
func (command Command) benchmark(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "benchmark requires the run or freeze-eval-set subcommand")
		return ExitUsage
	}
	switch arguments[0] {
	case "run":
		return command.benchmarkRun(ctx, arguments[1:])
	case "freeze-eval-set":
		return command.benchmarkFreezeEvalSet(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "benchmark requires the run or freeze-eval-set subcommand")
		return ExitUsage
	}
}

// benchmarkRun 执行本地基准套件的准备或实际运行；目录 override 语义与冻结资
// 产边界检查沿用既有合同。
func (command Command) benchmarkRun(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("benchmark run", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	localConfigPath := flags.String("local-config", "", "Git-ignored local benchmark configuration (required)")
	manifestPath := flags.String("manifest", "", "override the local task manifest path")
	configPath := flags.String("config", "", "override the frozen non-secret Agent configuration path")
	resultsRoot := flags.String("results-dir", "", "override the ignored benchmark results directory")
	workspacesRoot := flags.String("workspaces-dir", "", "override the ignored disposable clone directory")
	stateRoot := flags.String("state-dir", "", "override the ignored Cairn state directory")
	privateRoot := flags.String("private-dir", "", "override the ignored private verifier directory")
	cairnRoot := flags.String("cairn-root", "", "Cairn repository used to record the source revision")
	attempts := flags.Int("attempts", 0, "attempts per selected task; defaults to the task contract")
	prepareTimeout := flags.String("prepare-timeout", "", "override maximum clone, verifier, and bootstrap time per attempt")
	prepareProxy := flags.String("prepare-proxy", "", "override credential-free loopback HTTP proxy for clone only")
	prepareOnly := flags.Bool("prepare-only", false, "clone, checkout, and bootstrap without calling a model")
	var taskIDs stringList
	flags.Var(&taskIDs, "task", "local task ID to run; repeatable, defaults to all tasks")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "benchmark run does not accept positional arguments")
		return ExitUsage
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(command.Stderr, "resolve current directory:", err)
		return ExitFailed
	}
	if *cairnRoot == "" {
		*cairnRoot = root
	}
	if *localConfigPath == "" {
		fmt.Fprintln(command.Stderr, "benchmark run requires --local-config for Git-ignored local evaluation assets")
		return ExitUsage
	}
	localConfiguration, err := benchmark.LoadLocalConfig(*localConfigPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "local benchmark configuration error:", err)
		return ExitUsage
	}
	if *manifestPath == "" {
		*manifestPath = localConfiguration.Paths.Manifest
	}
	if *configPath == "" {
		*configPath = localConfiguration.Paths.AgentConfig
	}
	if *resultsRoot == "" {
		*resultsRoot = localConfiguration.Paths.ResultsDir
	}
	if *workspacesRoot == "" {
		*workspacesRoot = localConfiguration.Paths.WorkspacesDir
	}
	if *stateRoot == "" {
		*stateRoot = localConfiguration.Paths.StateDir
	}
	if *privateRoot == "" {
		*privateRoot = localConfiguration.Paths.PrivateDir
	}
	if *prepareTimeout == "" {
		*prepareTimeout = localConfiguration.Execution.PrepareTimeout
	}
	if *prepareProxy == "" {
		*prepareProxy = localConfiguration.Execution.PrepareProxy
	}
	prepareDuration, err := time.ParseDuration(*prepareTimeout)
	if err != nil || prepareDuration <= 0 {
		fmt.Fprintln(command.Stderr, "--prepare-timeout must be a positive duration")
		return ExitUsage
	}
	localAssets := append([]string{*localConfigPath}, localConfiguration.AssetPaths()...)
	localAssets = append(localAssets, *manifestPath, *configPath, *resultsRoot, *workspacesRoot, *stateRoot, *privateRoot)
	if err := benchmark.EnsureLocalAssetsIgnored(*cairnRoot, localAssets...); err != nil {
		fmt.Fprintln(command.Stderr, "local benchmark asset boundary error:", err)
		return ExitUsage
	}
	configuration, err := config.Load(*configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	pack, err := benchmark.LoadTaskPack(*manifestPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "benchmark manifest error:", err)
		return ExitUsage
	}
	report, err := (benchmark.Runner{Service: command.Service}).Run(ctx, benchmark.RunOptions{Pack: pack, TaskIDs: taskIDs, Attempts: *attempts, Configuration: configuration, ResultsRoot: *resultsRoot, WorkspacesRoot: *workspacesRoot, StateRoot: *stateRoot, PrivateRoot: *privateRoot, CairnRoot: *cairnRoot, PrepareTimeout: prepareDuration, PrepareProxy: *prepareProxy, PrepareOnly: *prepareOnly})
	if err != nil {
		fmt.Fprintln(command.Stderr, "benchmark failed:", err)
		return ExitFailed
	}
	if err := json.NewEncoder(command.Stdout).Encode(report); err != nil {
		fmt.Fprintln(command.Stderr, "write benchmark report:", err)
		return ExitFailed
	}
	if *prepareOnly || report.Passed() {
		return ExitOK
	}
	return ExitFailed
}

// benchmarkFreezeEvalSet 把本地忽略资产装配为受控评测集与凭据策略文件：产
// 物经执行器启动前检查全套自检后才落盘，回显两个冻结指纹供候选登记绑定。
// 宿主环境需注入占位的沙箱 Key 变量以满足自检存在性检查。
func (command Command) benchmarkFreezeEvalSet(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("benchmark freeze-eval-set", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	localConfigPath := flags.String("local-config", "", "Git-ignored local benchmark configuration (required)")
	attemptsPerTask := flags.Int("attempts-per-task", 3, "attempts per task frozen into the eval set")
	profileID := flags.String("policy-profile-id", "", "sandbox credential profile id allowed by policy (required)")
	suiteMaxWall := flags.String("suite-max-wall", "", "policy cap on total suite wall time (required)")
	comparatorVersion := flags.String("comparator-version", "", "preregistered comparator version label (required)")
	outDir := flags.String("out-dir", "", "ignored output directory for the frozen files (required)")
	cairnRoot := flags.String("cairn-root", "", "Cairn repository used for asset boundary checks")
	maxPromptTokens := flags.Int("max-prompt-tokens", 0, "variant max prompt tokens (required)")
	maxSummaryBytes := flags.Int("max-summary-bytes", 0, "variant max summary bytes (required)")
	inventoryPercent := flags.Int("inventory-percent", 0, "variant inventory budget percent (required)")
	reservePercent := flags.Int("reserve-percent", 0, "variant threshold reserve percent (required)")
	minRequests := flags.Int64("min-requests", 0, "policy minimum request quota")
	minInputTokens := flags.Int64("min-input-tokens", 0, "policy minimum input token quota")
	minOutputTokens := flags.Int64("min-output-tokens", 0, "policy minimum output token quota")
	minCostMicrousd := flags.Int64("min-cost-microusd", 0, "policy minimum cost quota in micro-USD")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	var taskIDs stringList
	flags.Var(&taskIDs, "task", "local task ID to freeze; repeatable, defaults to all tasks")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if *localConfigPath == "" || *profileID == "" || *suiteMaxWall == "" || *comparatorVersion == "" || *outDir == "" ||
		flags.NArg() != 0 || *maxPromptTokens == 0 || *maxSummaryBytes == 0 || *inventoryPercent == 0 || *reservePercent == 0 {
		fmt.Fprintln(command.Stderr, "benchmark freeze-eval-set requires --local-config, --policy-profile-id, --suite-max-wall, --comparator-version, --out-dir and all variant parameters")
		return ExitUsage
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(command.Stderr, "resolve current directory:", err)
		return ExitFailed
	}
	if *cairnRoot == "" {
		*cairnRoot = root
	}
	localConfiguration, err := benchmark.LoadLocalConfig(*localConfigPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "local benchmark configuration error:", err)
		return ExitUsage
	}
	freezeAssets := append([]string{*localConfigPath}, localConfiguration.AssetPaths()...)
	freezeAssets = append(freezeAssets, *outDir)
	if err := benchmark.EnsureLocalAssetsIgnored(*cairnRoot, freezeAssets...); err != nil {
		fmt.Fprintln(command.Stderr, "freeze asset boundary error:", err)
		return ExitUsage
	}
	configuration, err := config.Load(localConfiguration.Paths.AgentConfig, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "frozen agent configuration error:", err)
		return ExitUsage
	}
	pack, err := benchmark.LoadTaskPack(localConfiguration.Paths.Manifest)
	if err != nil {
		fmt.Fprintln(command.Stderr, "benchmark manifest error:", err)
		return ExitUsage
	}
	wallDuration, err := time.ParseDuration(*suiteMaxWall)
	if err != nil || wallDuration <= 0 {
		fmt.Fprintln(command.Stderr, "--suite-max-wall must be a positive duration")
		return ExitUsage
	}
	report, err := benchmark.FreezeEvaluationAssets(ctx, benchmark.FreezeEvalSetOptions{
		Pack: pack, TaskIDs: taskIDs, AttemptsPerTask: *attemptsPerTask,
		BaselineConfig: configuration,
		Variant: memory.CompactionParamsVariant{
			SchemaVersion:           memory.EvolutionSchemaVersion,
			MaxPromptTokens:         *maxPromptTokens,
			MaxSummaryBytes:         *maxSummaryBytes,
			InventoryBudgetPercent:  *inventoryPercent,
			ThresholdReservePercent: *reservePercent,
		},
		ComparatorVersion:        *comparatorVersion,
		PolicyProfileID:          *profileID,
		SuiteMaxWallMilliseconds: wallDuration.Milliseconds(),
		MinRequests:              *minRequests, MinInputTokens: *minInputTokens,
		MinOutputTokens: *minOutputTokens, MinCostMicrousd: *minCostMicrousd,
		PrivateRoot: localConfiguration.Paths.PrivateDir,
		OutDir:      *outDir,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "benchmark freeze-eval-set failed:", err)
		return ExitFailed
	}
	if *jsonOutput {
		writeJSON(command.Stdout, report)
		return ExitOK
	}
	fmt.Fprintf(command.Stdout, "frozen: eval_set=%s fingerprint=%s\ncredential policy=%s fingerprint=%s\n", report.EvalSetPath, report.EvalSetFingerprint, report.CredentialPolicyPath, report.CredentialPolicyFingerprint)
	return ExitOK
}

// memory 分派受限可信记忆控制面命令。它只暴露 capture、recall、explain、inspect、revoke、
// consolidate、reject、candidate 八个固定动作，绝不接收或输出任务正文、模型文本、文件内容或自由文本摘要。
func (command Command) memory(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "memory requires capture, recall, explain, inspect, revoke, consolidate, reject, or candidate")
		return ExitUsage
	}
	switch arguments[0] {
	case "capture":
		return command.memoryCapture(ctx, arguments[1:])
	case "recall":
		return command.memoryRecall(ctx, arguments[1:])
	case "explain":
		return command.memoryExplain(ctx, arguments[1:])
	case "inspect":
		return command.memoryInspectDispatch(ctx, arguments[1:])
	case "revoke":
		return command.memoryRevoke(ctx, arguments[1:])
	case "consolidate":
		return command.memoryConsolidate(ctx, arguments[1:])
	case "reject":
		return command.memoryRejectDerived(ctx, arguments[1:])
	case "candidate":
		return command.memoryCandidate(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "memory requires capture, recall, explain, inspect, revoke, consolidate, reject, or candidate")
		return ExitUsage
	}
}

// memoryCandidate 分派演化候选的固定动作：创建必须由人工显式提供闭合变体
// 参数；评测驱动沙箱执行器并要求候选已就绪；拒绝使用封闭理由；作废与过期
// 为显式终态迁移；解释只读。不存在任何自动生成候选或晋升入口。
func (command Command) memoryCandidate(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "memory candidate requires create, evaluate, reject, invalidate, expire, or inspect")
		return ExitUsage
	}
	switch arguments[0] {
	case "create":
		return command.memoryCandidateCreate(ctx, arguments[1:])
	case "evaluate":
		return command.memoryCandidateEvaluate(ctx, arguments[1:])
	case "reject":
		return command.memoryCandidateReject(ctx, arguments[1:])
	case "invalidate":
		return command.memoryCandidateInvalidate(ctx, arguments[1:])
	case "expire":
		return command.memoryCandidateExpire(ctx, arguments[1:])
	case "inspect":
		return command.memoryCandidateInspect(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "memory candidate requires create, evaluate, reject, invalidate, expire, or inspect")
		return ExitUsage
	}
}

// memoryCandidateCreate 从人工提供的闭合变体参数创建 draft 演化候选；
// 来源派生版本复合键与基线指纹为必填，不接受任务正文。
func (command Command) memoryCandidateCreate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate create", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	sourceDerived := flags.String("source-derived", "", "source derived candidate ID (required)")
	sourceConsolidation := flags.String("source-consolidation", "", "source consolidation ID (required)")
	baselineFingerprint := flags.String("baseline-fingerprint", "", "baseline configuration fingerprint sha256 (required)")
	noteDigest := flags.String("note-digest", "", "optional sha256 digest of an operator note artifact")
	maxPromptTokens := flags.Int("max-prompt-tokens", 0, "variant max prompt tokens (required)")
	maxSummaryBytes := flags.Int("max-summary-bytes", 0, "variant max summary bytes (required)")
	inventoryPercent := flags.Int("inventory-percent", 0, "variant inventory budget percent (required)")
	reservePercent := flags.Int("reserve-percent", 0, "variant threshold reserve percent (required)")
	ttlText := flags.String("ttl", "", "candidate retention duration (default: 30d, max: 90d)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 || *sourceDerived == "" || *sourceConsolidation == "" || *baselineFingerprint == "" || *maxPromptTokens == 0 || *maxSummaryBytes == 0 || *inventoryPercent == 0 || *reservePercent == 0 {
		fmt.Fprintln(command.Stderr, "memory candidate create requires source, baseline fingerprint and all variant parameters")
		return ExitUsage
	}
	var ttl time.Duration
	var err error
	if *ttlText != "" {
		ttl, err = time.ParseDuration(*ttlText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory candidate create --ttl must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.CreateEvolutionCandidate(ctx, app.MemoryEvolutionCreateOptions{
		Workspace:             *workspace,
		StateDir:              *stateDir,
		Config:                configuration,
		SourceDerivedID:       *sourceDerived,
		SourceConsolidationID: *sourceConsolidation,
		BaselineFingerprint:   *baselineFingerprint,
		NoteDigest:            *noteDigest,
		Variant: memory.CompactionParamsVariant{
			SchemaVersion:           memory.EvolutionSchemaVersion,
			MaxPromptTokens:         *maxPromptTokens,
			MaxSummaryBytes:         *maxSummaryBytes,
			InventoryBudgetPercent:  *inventoryPercent,
			ThresholdReservePercent: *reservePercent,
		},
		TTL: ttl,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate create failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, candidate)
		return ExitOK
	}
	fmt.Fprintf(command.Stdout, "candidate: %s status=%s target=%s variant=%s source=%s/%s\n", candidate.ID, candidate.Status, candidate.TargetKind, candidate.VariantFingerprint, candidate.Source.DerivedID, candidate.Source.ConsolidationID)
	return ExitOK
}

// memoryCandidateEvaluate 驱动沙箱执行器对已就绪候选发起受控评测：评测集、
// 租约与凭据策略都来自 Git 忽略的本地冻结文件；变体参数必须人工重述并经候
// 选登记的规格摘要复核。--dry-run 只跑离线预筛，不开事务也不调用模型。
func (command Command) memoryCandidateEvaluate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate evaluate", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	candidateID := flags.String("candidate", "", "evolution candidate ID (required)")
	localConfigPath := flags.String("local-config", "", "Git-ignored local evaluation configuration (required)")
	evalSetPath := flags.String("eval-set", "", "frozen eval set JSON file (required)")
	leasePath := flags.String("lease", "", "credential lease description JSON file (required)")
	policyPath := flags.String("policy", "", "frozen credential policy JSON file (required)")
	maxPromptTokens := flags.Int("max-prompt-tokens", 0, "variant max prompt tokens, must match the registration (required)")
	maxSummaryBytes := flags.Int("max-summary-bytes", 0, "variant max summary bytes (required)")
	inventoryPercent := flags.Int("inventory-percent", 0, "variant inventory budget percent (required)")
	reservePercent := flags.Int("reserve-percent", 0, "variant threshold reserve percent (required)")
	workspace := flags.String("workspace", "", "workspace the candidate was created against (default: current directory)")
	stateDir := flags.String("state-dir", "", "candidate state directory")
	configPath := flags.String("config", "", "configuration path used when the candidate was created")
	cairnRoot := flags.String("cairn-root", "", "Cairn repository used for asset boundary checks")
	dryRun := flags.Bool("dry-run", false, "run offline prescreen only: no transaction, no model calls")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if *candidateID == "" || *localConfigPath == "" || *evalSetPath == "" || *leasePath == "" || *policyPath == "" ||
		flags.NArg() != 0 || *maxPromptTokens == 0 || *maxSummaryBytes == 0 || *inventoryPercent == 0 || *reservePercent == 0 {
		fmt.Fprintln(command.Stderr, "memory candidate evaluate requires --candidate, --local-config, --eval-set, --lease, --policy and all variant parameters; create the candidate first if it does not exist")
		return ExitUsage
	}
	root, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(command.Stderr, "resolve current directory:", err)
		return ExitFailed
	}
	if *cairnRoot == "" {
		*cairnRoot = root
	}
	localConfiguration, err := benchmark.LoadLocalConfig(*localConfigPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "local benchmark configuration error:", err)
		return ExitUsage
	}
	evalAssets := append([]string{*localConfigPath}, localConfiguration.AssetPaths()...)
	evalAssets = append(evalAssets, *evalSetPath, *leasePath, *policyPath)
	if err := benchmark.EnsureLocalAssetsIgnored(*cairnRoot, evalAssets...); err != nil {
		fmt.Fprintln(command.Stderr, "evaluation asset boundary error:", err)
		return ExitUsage
	}
	memoryConfiguration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	baselineConfiguration, err := config.Load(localConfiguration.Paths.AgentConfig, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "frozen agent configuration error:", err)
		return ExitUsage
	}
	variant := memory.CompactionParamsVariant{
		SchemaVersion:           memory.EvolutionSchemaVersion,
		MaxPromptTokens:         *maxPromptTokens,
		MaxSummaryBytes:         *maxSummaryBytes,
		InventoryBudgetPercent:  *inventoryPercent,
		ThresholdReservePercent: *reservePercent,
	}
	specDigest, err := memory.VariantSpecDigest(variant)
	if err != nil {
		fmt.Fprintln(command.Stderr, "variant parameters rejected:", err)
		return ExitUsage
	}
	inspected, err := command.Service.InspectEvolutionCandidate(ctx, app.MemoryEvolutionInspectOptions{CandidateID: *candidateID, Workspace: *workspace, StateDir: *stateDir, Config: memoryConfiguration})
	if err != nil {
		fmt.Fprintf(command.Stderr, "memory candidate evaluate failed: %v; create the candidate first with memory candidate create\n", err)
		return exitFor(err)
	}
	if inspected.Candidate.Status != memory.CandidateStateReady {
		fmt.Fprintf(command.Stderr, "memory candidate evaluate requires a ready candidate, got %s; create it first or resolve its current state\n", inspected.Candidate.Status)
		return ExitFailed
	}
	if inspected.Candidate.VariantSpecSHA256 != specDigest {
		fmt.Fprintln(command.Stderr, "restated variant parameters diverge from the registered candidate specification")
		return ExitFailed
	}
	evalSet, err := sandboxeval.LoadEvalSet(*evalSetPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "frozen eval set error:", err)
		return ExitUsage
	}
	lease, err := sandboxeval.LoadCredentialLease(*leasePath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "credential lease error:", err)
		return ExitUsage
	}
	policy, err := sandboxeval.LoadCredentialPolicy(*policyPath)
	if err != nil {
		fmt.Fprintln(command.Stderr, "credential policy error:", err)
		return ExitUsage
	}
	pack, err := benchmark.LoadTaskPack(localConfiguration.Paths.Manifest)
	if err != nil {
		fmt.Fprintln(command.Stderr, "benchmark manifest error:", err)
		return ExitUsage
	}
	prepareDuration, err := time.ParseDuration(localConfiguration.Execution.PrepareTimeout)
	if err != nil || prepareDuration <= 0 {
		fmt.Fprintln(command.Stderr, "local benchmark prepare_timeout must be a positive duration")
		return ExitUsage
	}
	executor := benchmark.SandboxExecutor{Runner: benchmark.Runner{Service: command.Service}}
	outcome, err := executor.Run(ctx, benchmark.SandboxExecutorOptions{
		Pack:            pack,
		EvalSet:         evalSet,
		Lease:           lease,
		Policy:          policy,
		CandidateID:     *candidateID,
		MemoryWorkspace: *workspace,
		MemoryStateDir:  *stateDir,
		MemoryConfig:    memoryConfiguration,
		BaselineConfig:  baselineConfiguration,
		Variant:         variant,
		StateRoot:       localConfiguration.Paths.StateDir,
		WorkspacesRoot:  localConfiguration.Paths.WorkspacesDir,
		PrivateRoot:     localConfiguration.Paths.PrivateDir,
		CairnRoot:       *cairnRoot,
		PrepareTimeout:  prepareDuration,
		PrepareProxy:    localConfiguration.Execution.PrepareProxy,
		DryRun:          *dryRun,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate evaluate failed:", err)
		return ExitFailed
	}
	if *jsonOutput {
		writeJSON(command.Stdout, outcome)
		return ExitOK
	}
	if outcome.DryRun {
		fmt.Fprintln(command.Stdout, "evaluate dry-run: prescreen passed; no transaction opened, no model called")
		return ExitOK
	}
	conclusion := outcome.Conclusion
	if outcome.StopDetail != "" {
		fmt.Fprintf(command.Stdout, "evaluate: %s status=%s trigger=%s stop_detail=%s runs=%d result=%s\n", outcome.Evaluation.ID, conclusion.Status, conclusion.Trigger, outcome.StopDetail, outcome.RunsCompleted, outcome.ResultDigest)
	} else {
		fmt.Fprintf(command.Stdout, "evaluate: %s status=%s trigger=%s runs=%d result=%s\n", outcome.Evaluation.ID, conclusion.Status, conclusion.Trigger, outcome.RunsCompleted, outcome.ResultDigest)
	}
	if conclusion.Status == sandboxeval.ConclusionPassed {
		return ExitOK
	}
	return ExitFailed
}

// memoryCandidateReject 以封闭理由拒绝演化候选并落入冷却。
func (command Command) memoryCandidateReject(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate reject", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	reason := flags.String("reason", "", "rejection reason from the closed lexicon (required)")
	cooldownText := flags.String("cooldown", "", "cooldown duration (default: 7d, max: 90d)")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || *reason == "" {
		fmt.Fprintln(command.Stderr, "memory candidate reject requires a candidate ID and --reason")
		return ExitUsage
	}
	var cooldown time.Duration
	var err error
	if *cooldownText != "" {
		cooldown, err = time.ParseDuration(*cooldownText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory candidate reject --cooldown must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.RejectEvolutionCandidate(ctx, app.MemoryEvolutionActionOptions{CandidateID: flags.Arg(0), Reason: *reason, Workspace: *workspace, StateDir: *stateDir, Config: configuration, Cooldown: cooldown})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate reject failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "rejected: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// memoryCandidateInvalidate 显式作废来源失效的演化候选。
func (command Command) memoryCandidateInvalidate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate invalidate", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory candidate invalidate requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.InvalidateEvolutionCandidate(ctx, app.MemoryEvolutionActionOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate invalidate failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "invalidated: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// memoryCandidateExpire 显式把保留期耗尽的演化候选置为 expired 终态。
func (command Command) memoryCandidateExpire(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate expire", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory candidate expire requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.ExpireEvolutionCandidate(ctx, app.MemoryEvolutionActionOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate expire failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "expired: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// memoryCandidateInspect 输出演化候选与审计历史的只读解释。
func (command Command) memoryCandidateInspect(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory candidate inspect", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory candidate inspect requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	inspected, err := command.Service.InspectEvolutionCandidate(ctx, app.MemoryEvolutionInspectOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory candidate inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, inspected)
		return ExitOK
	}
	candidate := inspected.Candidate
	fmt.Fprintf(command.Stdout, "candidate: %s status=%s target=%s\n", candidate.ID, candidate.Status, candidate.TargetKind)
	fmt.Fprintf(command.Stdout, "source: %s/%s\n", candidate.Source.DerivedID, candidate.Source.ConsolidationID)
	fmt.Fprintf(command.Stdout, "variant: %s spec=%s\n", candidate.VariantFingerprint, candidate.VariantSpecSHA256)
	fmt.Fprintf(command.Stdout, "baseline: %s\n", candidate.BaselineFingerprint)
	fmt.Fprintf(command.Stdout, "retention: %s -> %s\n", candidate.CreatedAt.Format(time.RFC3339), candidate.ExpiresAt.Format(time.RFC3339))
	for _, entry := range inspected.Audit {
		reason := ""
		if entry.Reason != "" {
			reason = " reason=" + entry.Reason
		}
		fmt.Fprintf(command.Stdout, "audit: %s%s at %s\n", entry.Action, reason, entry.OccurredAt.Format(time.RFC3339))
	}
	if evaluation := inspected.Evaluation; evaluation != nil {
		finished := "-"
		if !evaluation.FinishedAt.IsZero() {
			finished = evaluation.FinishedAt.Format(time.RFC3339)
		}
		fmt.Fprintf(command.Stdout, "evaluation: %s status=%s evidence=%s started=%s finished=%s\n",
			evaluation.ID, evaluation.Status, evidenceRef(evaluation.SuiteResultDigest), evaluation.StartedAt.Format(time.RFC3339), finished)
	}
	return ExitOK
}

// evidenceRef 把证据摘要引用收敛为展示形态：空值显示 pending。
func evidenceRef(digest string) string {
	if digest == "" {
		return "pending"
	}
	return digest
}

// memoryConsolidate 在工作区锁内执行一次事务化巩固，并把提交的计划、派生候选与
// 被替代版本以人类或 NDJSON 视图输出；不接入运行时召回。
func (command Command) memoryConsolidate(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory consolidate", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	ttlText := flags.String("ttl", "", "derived candidate retention duration (default: 30d, max: 90d)")
	minSupport := flags.Int("min-support", 0, "minimum evidence support for pattern and hypothesis (default: 2, cannot be lowered)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory consolidate does not accept positional arguments")
		return ExitUsage
	}
	var ttl time.Duration
	var err error
	if *ttlText != "" {
		ttl, err = time.ParseDuration(*ttlText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory consolidate --ttl must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	results, err := command.Service.ConsolidateMemory(ctx, app.MemoryConsolidateOptions{Workspace: *workspace, StateDir: *stateDir, Config: configuration, TTL: ttl, MinPatternSupport: *minSupport, MinHypothesisSupport: *minSupport})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory consolidate failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, results)
		return ExitOK
	}
	if len(results) == 0 {
		fmt.Fprintln(command.Stdout, "consolidation: no derivable candidates from current inputs")
		return ExitOK
	}
	for _, result := range results {
		fmt.Fprintf(command.Stdout, "consolidation: %s strategy=%s inputs=%d\n", result.Plan.ConsolidationID, result.Plan.Strategy, len(result.Plan.InputIDs))
		for _, derived := range result.Plan.Derived {
			fmt.Fprintf(command.Stdout, "derived: %s kind=%s support=%d status=%s\n", derived.ID, derived.Kind, len(derived.Evidence), derived.Status)
		}
		for _, superseded := range result.SupersededVersion {
			fmt.Fprintf(command.Stdout, "superseded: %s consolidation=%s\n", superseded.ID, superseded.ConsolidationID)
		}
	}
	return ExitOK
}

// memoryRejectDerived 显式拒绝当前 active 的派生候选版本；理由只能是封闭词表。
func (command Command) memoryRejectDerived(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory reject", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	reason := flags.String("reason", "", "rejection reason: insufficient_evidence or human_request (required)")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || *reason == "" {
		fmt.Fprintln(command.Stderr, "memory reject requires a derived candidate ID and --reason")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	key, err := command.Service.RejectDerivedMemory(ctx, app.MemoryRejectDerivedOptions{DerivedID: flags.Arg(0), Reason: *reason, Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory reject failed:", err)
		return exitFor(err)
	}
	fmt.Fprintf(command.Stdout, "rejected: %s consolidation=%s\n", key.ID, key.ConsolidationID)
	return ExitOK
}

// memoryExplain 显式执行 M2-A 的 sqlite_exact 计划和一次性证据账本投影。它仅接收
// 摘要、枚举和 UTC 时间范围，绝不接收任务正文或把结果注入 Agent Loop。
func (command Command) memoryExplain(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory explain", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	taskDigest := flags.String("task-digest", "", "exact sha256 task digest")
	trustedVerification := flags.String("trusted-verification", "any", "any, required, or not_required")
	createdFromText := flags.String("created-from", "", "UTC RFC3339 start (inclusive)")
	createdUntilText := flags.String("created-until", "", "UTC RFC3339 end (exclusive)")
	limit := flags.Int("limit", 0, "candidates: compact default/max 10; trace default 20, max 100")
	precision := flags.String("precision", "compact", "compact or trace")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	var outcomes, failureClasses stringList
	flags.Var(&outcomes, "outcome", "completed, failed, or cancelled; repeatable")
	flags.Var(&failureClasses, "failure-class", "controlled failure class; repeatable")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory explain does not accept positional arguments")
		return ExitUsage
	}
	createdFrom, parseErr := parseUTCTimestamp(*createdFromText)
	if parseErr != nil {
		fmt.Fprintln(command.Stderr, "memory explain --created-from must be UTC RFC3339")
		return ExitUsage
	}
	createdUntil, parseErr := parseUTCTimestamp(*createdUntilText)
	if parseErr != nil {
		fmt.Fprintln(command.Stderr, "memory explain --created-until must be UTC RFC3339")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	ledger, err := command.Service.ExplainMemoryRecall(ctx, app.MemoryExplainOptions{
		Query: memory.RecallQuery{
			TaskPromptDigest:    *taskDigest,
			Outcomes:            outcomes,
			FailureClasses:      failureClasses,
			TrustedVerification: *trustedVerification,
			CreatedFrom:         createdFrom,
			CreatedUntil:        createdUntil,
			Limit:               *limit,
			Precision:           *precision,
		},
		Workspace: *workspace,
		StateDir:  *stateDir,
		Config:    configuration,
	})
	if err != nil && ledger.SchemaVersion == 0 {
		fmt.Fprintln(command.Stderr, "memory explain query is invalid")
		return ExitUsage
	}
	if *jsonOutput {
		writeJSON(command.Stdout, struct {
			Type   string                `json:"type"`
			Ledger memory.EvidenceLedger `json:"ledger"`
		}{Type: "memory_explain", Ledger: ledger})
	} else {
		fmt.Fprintf(command.Stdout, "memory explain: strategy=%s status=%s candidates=%d conflicts=%d\n", ledger.Plan.Strategy, ledger.Status, len(ledger.Entries), len(ledger.Conflicts))
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory explain source invalid")
		return exitFor(err)
	}
	return ExitOK
}

// parseUTCTimestamp 解析 CLI 时间边界；空值表示未提供，非 UTC 表示拒绝而非隐式换算。
func parseUTCTimestamp(value string) (*time.Time, error) {
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Location() != time.UTC {
		return nil, errors.New("timestamp is not UTC RFC3339")
	}
	return &parsed, nil
}

// memoryCapture 解析固定 Run 身份与 TTL，从已校验终态 Journal 写入结构化候选。
func (command Command) memoryCapture(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory capture", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	runID := flags.String("run", "", "terminal run ID (required)")
	ttlText := flags.String("ttl", "", "memory retention duration (default: 30d, max: 90d)")
	supersedesID := flags.String("supersedes", "", "active memory candidate ID to atomically supersede")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 || *runID == "" {
		fmt.Fprintln(command.Stderr, "memory capture requires --run and no positional arguments")
		return ExitUsage
	}
	var ttl time.Duration
	var err error
	if *ttlText != "" {
		ttl, err = time.ParseDuration(*ttlText)
		if err != nil {
			fmt.Fprintln(command.Stderr, "memory capture --ttl must be a duration")
			return ExitUsage
		}
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.CaptureMemory(ctx, app.MemoryCaptureOptions{RunID: domain.RunID(*runID), Workspace: *workspace, StateDir: *stateDir, Config: configuration, TTL: ttl, SupersedesID: *supersedesID})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory capture failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, struct {
			Type      string `json:"type"`
			Candidate any    `json:"candidate"`
		}{Type: "memory_capture", Candidate: candidate})
		return ExitOK
	}
	fmt.Fprintf(command.Stdout, "memory candidate: %s kind=%s outcome=%s status=%s expires=%s\n", candidate.ID, candidate.Kind, candidate.Outcome, candidate.Status, candidate.ExpiresAt.UTC().Format(time.RFC3339Nano))
	return ExitOK
}

// memoryRecall 只读检索当前工作区的 active、未过期 run_outcome 候选。
func (command Command) memoryRecall(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory recall", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	limit := flags.Int("limit", 0, "maximum recalled candidates (default: 20, max: 100)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "memory recall does not accept positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidates, err := command.Service.RecallMemory(ctx, app.MemoryRecallOptions{Workspace: *workspace, StateDir: *stateDir, Config: configuration, Limit: *limit})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory recall failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, struct {
			Type       string `json:"type"`
			Candidates any    `json:"candidates"`
		}{Type: "memory_recall", Candidates: candidates})
		return ExitOK
	}
	for _, candidate := range candidates {
		fmt.Fprintf(command.Stdout, "memory candidate: %s outcome=%s status=%s created=%s expires=%s\n", candidate.ID, candidate.Outcome, candidate.Status, candidate.CreatedAt.UTC().Format(time.RFC3339Nano), candidate.ExpiresAt.UTC().Format(time.RFC3339Nano))
	}
	return ExitOK
}

// memoryInspect 只读显示当前工作区一个候选及其无自由文本的审计历史。
func (command Command) memoryInspectDispatch(ctx context.Context, arguments []string) int {
	for _, argument := range normalizeMemoryPositionals(arguments) {
		if memory.IsDerivedID(argument) {
			return command.memoryInspectDerived(ctx, arguments)
		}
	}
	return command.memoryInspect(ctx, arguments)
}

// memoryInspectDerived 输出派生候选最新版本的候选事实与审计历史；人类与
// NDJSON 视图共享同一只读投影。
func (command Command) memoryInspectDerived(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory inspect derived", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory inspect requires a derived candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	inspected, err := command.Service.InspectDerivedMemory(ctx, app.MemoryInspectDerivedOptions{DerivedID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, inspected)
		return ExitOK
	}
	candidate := inspected.Candidate
	fmt.Fprintf(command.Stdout, "derived: %s kind=%s status=%s current=%t eligible=%t\n", candidate.ID, candidate.Kind, candidate.Status, inspected.Current, inspected.Eligible)
	fmt.Fprintf(command.Stdout, "consolidation: %s\n", candidate.ConsolidationID)
	fmt.Fprintf(command.Stdout, "scope: %s\n", candidate.ScopeDigest)
	fmt.Fprintf(command.Stdout, "payload: %s\n", string(candidate.Payload))
	fmt.Fprintf(command.Stdout, "retention: %s -> %s\n", candidate.CreatedAt.Format(time.RFC3339), candidate.ExpiresAt.Format(time.RFC3339))
	for _, evidence := range candidate.Evidence {
		fmt.Fprintf(command.Stdout, "evidence: %s\n", evidence.CandidateID)
	}
	for _, entry := range inspected.Audit {
		reason := ""
		if entry.Reason != "" {
			reason = " reason=" + entry.Reason
		}
		fmt.Fprintf(command.Stdout, "audit: %s%s at %s\n", entry.Action, reason, entry.OccurredAt.Format(time.RFC3339))
	}
	return ExitOK
}

func (command Command) memoryInspect(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory inspect", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "memory inspect requires a candidate ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	result, err := command.Service.InspectMemory(ctx, app.MemoryInspectOptions{CandidateID: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, struct {
			Type   string `json:"type"`
			Memory any    `json:"memory"`
		}{Type: "memory_inspect", Memory: result})
		return ExitOK
	}
	candidate := result.Candidate
	fmt.Fprintf(command.Stdout, "memory candidate: %s kind=%s outcome=%s failure=%s trusted_verification=%t status=%s\n", candidate.ID, candidate.Kind, candidate.Outcome, candidate.FailureClass, candidate.TrustedVerification, candidate.Status)
	for _, entry := range result.Audit {
		fmt.Fprintf(command.Stdout, "audit: %s reason=%s at=%s\n", entry.Action, entry.Reason, entry.OccurredAt.UTC().Format(time.RFC3339Nano))
	}
	return ExitOK
}

// memoryRevoke 原子撤销当前工作区的 active 候选，并要求封闭的、无自由文本理由。
func (command Command) memoryRevoke(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("memory revoke", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	reason := flags.String("reason", "", "evidence_invalid or human_request (required)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeMemoryPositionals(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || (*reason != "evidence_invalid" && *reason != "human_request") {
		fmt.Fprintln(command.Stderr, "memory revoke requires a candidate ID and --reason evidence_invalid|human_request")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	candidate, err := command.Service.RevokeMemory(ctx, app.MemoryRevokeOptions{CandidateID: flags.Arg(0), Reason: *reason, Workspace: *workspace, StateDir: *stateDir, Config: configuration})
	if err != nil {
		fmt.Fprintln(command.Stderr, "memory revoke failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, struct {
			Type      string `json:"type"`
			Candidate any    `json:"candidate"`
		}{Type: "memory_revoke", Candidate: candidate})
		return ExitOK
	}
	fmt.Fprintf(command.Stdout, "memory candidate: %s status=%s\n", candidate.ID, candidate.Status)
	return ExitOK
}

// resume 解析恢复、审批、对账或中止参数，并调用相应应用用例。
func (command Command) resume(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	approve := flags.String("approve", "", "approve the exact pending action fingerprint")
	deny := flags.String("deny", "", "deny the exact pending action fingerprint")
	resolve := flags.String("resolve", "", "reconcile <operation-id>=<verdict>")
	abortRun := flags.Bool("abort-run", false, "abort a running run with no dangling operation")
	evidence := flags.String("evidence", "", "evidence reference for reconciliation")
	note := flags.String("note", "", "non-empty reconciliation note")
	reason := flags.String("reason", "", "reason for a denial")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeResumeArguments(arguments)); err != nil {
		return ExitUsage
	}
	approvalCount := 0
	if *approve != "" {
		approvalCount++
	}
	if *deny != "" {
		approvalCount++
	}
	if *resolve != "" {
		approvalCount++
	}
	if *abortRun {
		approvalCount++
	}
	if flags.NArg() != 1 || approvalCount > 1 {
		fmt.Fprintln(command.Stderr, "resume requires a run ID and at most one of --approve, --deny, --resolve, or --abort-run")
		return ExitUsage
	}
	if *abortRun && strings.TrimSpace(*reason) == "" {
		fmt.Fprintln(command.Stderr, "--abort-run requires --reason")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	onEvent := func(event domain.Event) {
		if *jsonOutput {
			writeJSON(command.Stdout, struct {
				Type  string       `json:"type"`
				Event domain.Event `json:"event"`
			}{Type: "event", Event: event})
		}
	}
	if *resolve != "" {
		parts := strings.SplitN(*resolve, "=", 2)
		if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
			fmt.Fprintln(command.Stderr, "--resolve must be <operation-id>=<verdict>")
			return ExitUsage
		}
		if strings.TrimSpace(*note) == "" || strings.TrimSpace(*evidence) == "" {
			fmt.Fprintln(command.Stderr, "--resolve requires both --note and --evidence")
			return ExitUsage
		}
		result, err := command.Service.Reconcile(ctx, app.ReconcileOptions{RunID: domain.RunID(flags.Arg(0)), OperationID: domain.OperationID(parts[0]), Verdict: parts[1], Note: *note, Evidence: *evidence, Workspace: *workspace, StateDir: *stateDir, Config: configuration, OnEvent: onEvent})
		if err != nil {
			fmt.Fprintln(command.Stderr, "reconcile failed:", err)
			return exitFor(err)
		}
		if *jsonOutput {
			writeJSON(command.Stdout, struct {
				Type     string                `json:"type"`
				RunID    domain.RunID          `json:"run_id"`
				Response string                `json:"response"`
				Manifest *contract.RunManifest `json:"manifest,omitempty"`
			}{Type: "result", RunID: result.ID, Response: result.Response, Manifest: manifestOf(result)})
		} else {
			fmt.Fprintln(command.Stdout, result.Response)
		}
		return ExitOK
	}
	if *abortRun {
		result, err := command.Service.Abort(ctx, app.AbortOptions{RunID: domain.RunID(flags.Arg(0)), Reason: *reason, Workspace: *workspace, StateDir: *stateDir, Config: configuration, OnEvent: onEvent})
		if err != nil {
			fmt.Fprintln(command.Stderr, "abort failed:", err)
			return exitFor(err)
		}
		if *jsonOutput {
			writeJSON(command.Stdout, struct {
				Type     string                `json:"type"`
				RunID    domain.RunID          `json:"run_id"`
				Manifest *contract.RunManifest `json:"manifest,omitempty"`
			}{Type: "aborted", RunID: result.ID, Manifest: manifestOf(result)})
		}
		return ExitFailed
	}
	fingerprint := *approve
	approved := true
	if fingerprint == "" && *deny != "" {
		fingerprint = *deny
		approved = false
	}
	result, err := command.Service.Resume(ctx, app.ResumeOptions{RunID: domain.RunID(flags.Arg(0)), Workspace: *workspace, StateDir: *stateDir, Config: configuration, Approval: agent.Approval{Fingerprint: fingerprint, Approve: approved, Reason: *reason}, OnEvent: onEvent})
	if err != nil {
		fmt.Fprintln(command.Stderr, "resume failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, struct {
			Type     string                `json:"type"`
			RunID    domain.RunID          `json:"run_id"`
			Response string                `json:"response"`
			Manifest *contract.RunManifest `json:"manifest,omitempty"`
		}{Type: "result", RunID: result.ID, Response: result.Response, Manifest: manifestOf(result)})
	} else {
		fmt.Fprintln(command.Stdout, result.Response)
	}
	return ExitOK
}

// normalizeResumeArguments 保持运行 ID 为位置参数，同时允许按公开 CLI 契约将
// 它放在 flags 之前；标准 flag 包会在第一个位置参数处停止解析。
func normalizeResumeArguments(arguments []string) []string {
	return normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {}, "--approve": {},
		"--deny": {}, "--resolve": {}, "--evidence": {}, "--note": {}, "--reason": {},
	})
}

// normalizeRunArguments 允许 run 的唯一任务位置参数出现在 flags 之前。
func normalizeRunArguments(arguments []string) []string {
	return normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {}, "--base-url": {}, "--model": {},
		"--api-key-env": {}, "--max-turns": {}, "--max-tool-calls": {}, "--max-input-tokens": {},
		"--max-output-tokens": {}, "--max-wall-time": {}, "--max-cost-usd": {},
	})
}

// normalizeMemoryPositionals 允许 inspect/revoke 的候选标识放在 flags 之前，
// 同时保持标准 flag 包对公共 workspace/state/config 选项的解析语义。
func normalizeMemoryPositionals(arguments []string) []string {
	return normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {}, "--reason": {},
	})
}

// normalizeFlagsThenPositionals 将 flags 与其值移动到位置参数之前以兼容 flag 包。
func normalizeFlagsThenPositionals(arguments []string, valueFlags map[string]struct{}) []string {
	flags := make([]string, 0, len(arguments))
	positionals := make([]string, 0, 1)
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument == "--" {
			positionals = append(positionals, arguments[index+1:]...)
			break
		}
		if !strings.HasPrefix(argument, "-") {
			positionals = append(positionals, argument)
			continue
		}
		flags = append(flags, argument)
		if _, takesValue := valueFlags[argument]; takesValue && index+1 < len(arguments) {
			index++
			flags = append(flags, arguments[index])
		}
	}
	return append(flags, positionals...)
}

// run 解析一次新 Agent 运行的参数，转交应用层并输出最终结果。
func (command Command) run(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	baseURL := flags.String("base-url", "", "OpenAI-compatible provider base URL")
	model := flags.String("model", "", "provider model")
	apiKeyEnv := flags.String("api-key-env", "", "environment variable containing the API key")
	maxTurns := flags.Int("max-turns", 0, "maximum model turns")
	maxToolCalls := flags.Int("max-tool-calls", 0, "maximum tool operations")
	maxInputTokens := flags.Int("max-input-tokens", 0, "maximum reported input tokens")
	maxOutputTokens := flags.Int("max-output-tokens", 0, "maximum reported output tokens")
	maxWallTime := flags.String("max-wall-time", "", "maximum wall-clock duration")
	maxCostUSD := flags.Float64("max-cost-usd", 0, "maximum reported-token cost in USD (requires configured pricing)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeRunArguments(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" {
		fmt.Fprintln(command.Stderr, "run requires exactly one task argument")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{
		BaseURL: *baseURL, Model: *model, APIKeyEnv: *apiKeyEnv, MaxTurns: *maxTurns,
		MaxToolCalls: *maxToolCalls, MaxInputTokens: *maxInputTokens,
		MaxOutputTokens: *maxOutputTokens, MaxWallTime: *maxWallTime, MaxCostUSD: *maxCostUSD,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	onEvent := func(event domain.Event) {
		if *jsonOutput {
			writeJSON(command.Stdout, struct {
				Type  string       `json:"type"`
				Event domain.Event `json:"event"`
			}{Type: "event", Event: event})
		}
	}
	result, err := command.Service.Run(ctx, app.RunOptions{Task: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration, OnEvent: onEvent})
	if err != nil {
		fmt.Fprintln(command.Stderr, "run failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, struct {
			Type     string                `json:"type"`
			RunID    domain.RunID          `json:"run_id"`
			Response string                `json:"response"`
			Manifest *contract.RunManifest `json:"manifest,omitempty"`
		}{Type: "result", RunID: result.ID, Response: result.Response, Manifest: manifestOf(result)})
	} else {
		fmt.Fprintln(command.Stdout, result.Response)
	}
	return ExitOK
}

// manifestOf 从运行结果中提取产品合同 manifest；投影失败或旧路径无合同时
// 返回 nil，--json 输出省略该字段而不输出空对象。
func manifestOf(result app.RunResult) *contract.RunManifest {
	if result.Result == nil {
		return nil
	}
	return &result.Result.Manifest
}

// exitFor 将领域和应用错误映射为公开稳定的 CLI 退出码。
func exitFor(err error) int {
	var waiting *agent.WaitingApprovalError
	if errors.As(err, &waiting) {
		return ExitApproval
	}
	var denied *agent.PolicyDeniedError
	if errors.As(err, &denied) {
		return ExitDenied
	}
	if errors.Is(err, app.ErrWaitingReconcile) {
		return ExitReconcile
	}
	if errors.Is(err, app.ErrConfigDrift) {
		return ExitUsage
	}
	if errors.Is(err, app.ErrStateCorrupt) {
		return ExitCorrupt
	}
	if errors.Is(err, context.Canceled) {
		return ExitCancelled
	}
	return ExitFailed
}

// inspect 查询指定运行的投影，并可选择输出其完整事件历史。
func (command Command) inspect(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	includeEvents := flags.Bool("events", false, "include full event history")
	jsonOutput := flags.Bool("json", false, "write JSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "inspect requires a run ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	result, err := command.Service.Inspect(ctx, *workspace, *stateDir, configuration, domain.RunID(flags.Arg(0)), *includeEvents)
	if err != nil {
		fmt.Fprintln(command.Stderr, "inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, result)
	} else {
		fmt.Fprintf(command.Stdout, "run: %s\nstate: %s\nlast sequence: %d\n", result.Projection.ID, result.Projection.Phase(), result.Projection.LastSeq)
		summary := result.Summary
		if summary.ConfigFingerprint != "" {
			fmt.Fprintf(command.Stdout, "config fingerprint: %s\n", summary.ConfigFingerprint)
		} else {
			fmt.Fprintln(command.Stdout, "config fingerprint: unavailable (legacy run)")
		}
		if summary.Build != nil {
			writeBuildIdentity(command.Stdout, "build", *summary.Build)
		} else {
			fmt.Fprintln(command.Stdout, "build: unavailable (legacy run)")
		}
		if result.Manifest != nil {
			writeRunManifest(command.Stdout, *result.Manifest)
		}
		fmt.Fprintf(command.Stdout, "workspace: %s\n", summary.WorkspaceHash)
		if summary.WorkspaceObservation != "" {
			fmt.Fprintf(command.Stdout, "workspace observation: %s\n", summary.WorkspaceObservation)
		}
		if result.Projection.LastAssistantMessage != "" {
			fmt.Fprintf(command.Stdout, "response: %s\n", result.Projection.LastAssistantMessage)
		}
		if summary.StopReason != "" {
			fmt.Fprintf(command.Stdout, "stopped: %s\n", summary.StopReason)
		}
		fmt.Fprintf(command.Stdout, "budget: turns %d, tool calls %d, tokens %d in / %d out\n", summary.TurnsUsed, summary.ToolCallsStarted, summary.InputTokens, summary.OutputTokens)
		fmt.Fprintf(command.Stdout, "changes: last mutation seq %d\n", summary.LastMutationSeq)
		if summary.LastVerification != nil {
			fmt.Fprintf(command.Stdout, "last verification: seq %d ok=%t (last mutation seq %d)\n", summary.LastVerification.Seq, summary.LastVerification.OK, summary.LastMutationSeq)
		}
		if summary.CompletionTrusted {
			fmt.Fprintln(command.Stdout, "completion: trusted (verification binds all mutations)")
		}
		if summary.NextAction != "" {
			fmt.Fprintf(command.Stdout, "next: %s\n", summary.NextAction)
		}
	}
	return ExitOK
}

// evidence 输出一次运行的统一证据视图：任务/运行/Provider 卡身份、终局、
// 失败分类、完成证据与资源指标，全部来自事件与冻结配置制品的回放投影。
func (command Command) evidence(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("evidence", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write the evidence result as NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "evidence requires a run ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	result, err := command.Service.Evidence(ctx, *workspace, *stateDir, configuration, domain.RunID(flags.Arg(0)))
	if err != nil {
		fmt.Fprintln(command.Stderr, "evidence failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		writeJSON(command.Stdout, result)
		return ExitOK
	}
	fmt.Fprintf(command.Stdout, "run: %s\n", result.Manifest.RunID)
	if result.Manifest.Provider.Card != nil {
		card := result.Manifest.Provider.Card
		fmt.Fprintf(command.Stdout, "provider card: %s protocol=%s tool_calls=%t streaming_usage=%t context=%d output=%d stops=%s source=%s\n", card.Model, card.Protocol, card.ToolCalls, card.StreamingUsage, card.ContextWindowTokens, card.MaxOutputTokens, strings.Join(card.StopReasons, "/"), card.Source)
	}
	writeRunManifest(command.Stdout, result.Manifest)
	fmt.Fprintf(command.Stdout, "outcome: %s (state %s)\n", result.Outcome, result.State)
	if result.FailureClass != "" {
		fmt.Fprintf(command.Stdout, "failure class: %s\n", result.FailureClass)
	}
	if result.StopReason != "" {
		fmt.Fprintf(command.Stdout, "stop reason: %s\n", result.StopReason)
	}
	fmt.Fprintf(command.Stdout, "metrics: turns %d, tool calls %d, tokens %d in / %d out (estimated=%t), compactions %d\n", result.Metrics.Turns, result.Metrics.ToolCalls, result.Metrics.InputTokens, result.Metrics.OutputTokens, result.Metrics.UsageEstimated, result.Metrics.ContextCompactions)
	fmt.Fprintf(command.Stdout, "completion evidence: verification seq %d ok=%t, last mutation seq %d, trusted=%t\n", result.Evidence.VerificationSeq, result.Evidence.VerificationOK, result.Evidence.LastMutationSeq, result.Evidence.Trusted)
	return ExitOK
}

// doctor 执行不改变状态的 Journal 与制品库完整性诊断。
func (command Command) doctor(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("doctor", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write the doctor report as NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "doctor does not accept positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	report, err := command.Service.Doctor(ctx, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "doctor failed:", err)
		return ExitCorrupt
	}
	if *jsonOutput {
		writeJSON(command.Stdout, report)
		return ExitOK
	}
	fmt.Fprintf(command.Stdout, "ready: %t\n", report.Ready)
	fmt.Fprintf(command.Stdout, "config: %t (preset %s profile %s)\n", report.ConfigValid, report.Preset, report.Profile)
	fmt.Fprintf(command.Stdout, "config fingerprint: %s\n", report.ConfigFingerprint)
	writeBuildIdentity(command.Stdout, "build", report.Build)
	fmt.Fprintf(command.Stdout, "workspace: %s\n", report.WorkspaceHash)
	fmt.Fprintf(command.Stdout, "policy: write=%s exec=%s\n", report.Policy.Write, report.Policy.Exec)
	fmt.Fprintf(command.Stdout, "provider: %s model %s key env %s present=%t\n", report.Provider.BaseURL, report.Provider.Model, report.Provider.APIKeyEnv, report.Provider.APIKeyPresent)
	if report.ProviderCard != nil {
		card := report.ProviderCard
		fmt.Fprintf(command.Stdout, "provider card: %s protocol=%s tool_calls=%t streaming_usage=%t context=%d output=%d stops=%s source=%s\n", card.Model, card.Protocol, card.ToolCalls, card.StreamingUsage, card.ContextWindowTokens, card.MaxOutputTokens, strings.Join(card.StopReasons, "/"), card.Source)
	} else {
		fmt.Fprintf(command.Stdout, "provider card: unresolved (%s)\n", report.ProviderCardError)
	}
	if report.Sandbox.Backend == "docker" {
		fmt.Fprintf(command.Stdout, "sandbox: docker image %s available=%s\n", report.Sandbox.DockerImage, report.Sandbox.ImageAvailable)
	} else {
		fmt.Fprintf(command.Stdout, "sandbox: %s\n", report.Sandbox.Backend)
	}
	fmt.Fprintf(command.Stdout, "tools: %s\n", strings.Join(report.Tools, ", "))
	fmt.Fprintf(command.Stdout, "budget: turns %d, tool calls %d, input %d, output %d, wall %s\n", report.Capabilities.Budget.MaxTurns, report.Capabilities.Budget.MaxToolCalls, report.Capabilities.Budget.MaxInputTokens, report.Capabilities.Budget.MaxOutputTokens, report.Capabilities.Budget.MaxWallTime)
	fmt.Fprintf(command.Stdout, "verification: %d command(s) %s\n", report.Capabilities.Verification.Commands, report.Capabilities.Verification.Fingerprint)
	fmt.Fprintf(command.Stdout, "telemetry: %s\n", report.Telemetry.Status)
	fmt.Fprintf(command.Stdout, "verification commands: %d\n", report.VerificationCount)
	for _, entry := range report.Resolution {
		fmt.Fprintf(command.Stdout, "resolved %s = %s (%s)\n", entry.Field, entry.Value, entry.Source)
	}
	fmt.Fprintf(command.Stdout, "journal: %s ok=%t artifacts=%t\n", report.JournalState, report.JournalOK, report.ArtifactsOK)
	fmt.Fprintf(command.Stdout, "memory: %s ok=%t\n", report.MemoryState, report.MemoryOK)
	return ExitOK
}

// writeBuildIdentity 输出实际二进制的非敏感身份；空 revision 代表构建信息未提供，
// 不把它误报成某个可验证的提交。
func writeBuildIdentity(writer io.Writer, label string, identity domain.BuildIdentity) {
	revision := identity.Revision
	if revision == "" {
		revision = "unknown"
	}
	fmt.Fprintf(writer, "%s: revision=%s dirty=%s flavor=%s fingerprint=%s\n", label, revision, identity.Dirty, identity.Flavor, identity.Fingerprint)
}

// writeRunManifest 输出运行产品合同 manifest 的非敏感身份块；缺失字段直接
// 省略，不伪造默认值，任务正文与本地路径绝不出现。
func writeRunManifest(writer io.Writer, manifest contract.RunManifest) {
	fmt.Fprintf(writer, "task: %s", manifest.Task.Kind)
	if manifest.Task.Source != "" {
		fmt.Fprintf(writer, " (%s)", manifest.Task.Source)
	}
	if manifest.Task.Benchmark != nil {
		fmt.Fprintf(writer, " id=%s", manifest.Task.Benchmark.TaskID)
	}
	fmt.Fprintf(writer, " prompt=%s\n", manifest.Task.PromptDigest)
	if provider := manifest.Provider; provider.Kind != "" || provider.BaseURL != "" || provider.Model != "" {
		parts := make([]string, 0, 3)
		for _, value := range []string{provider.Kind, provider.BaseURL, provider.Model} {
			if value != "" {
				parts = append(parts, value)
			}
		}
		fmt.Fprintf(writer, "provider: %s\n", strings.Join(parts, " "))
	}
	if manifest.Preset != "" || manifest.Profile != "" {
		fmt.Fprintf(writer, "preset: %s profile %s\n", manifest.Preset, manifest.Profile)
	}
	if manifest.Policy.Write != "" || manifest.Policy.Exec != "" {
		fmt.Fprintf(writer, "policy: write=%s exec=%s\n", manifest.Policy.Write, manifest.Policy.Exec)
	}
	if manifest.Verification.Fingerprint != "" {
		fmt.Fprintf(writer, "verification plan: %d command(s) %s\n", manifest.Verification.Commands, manifest.Verification.Fingerprint)
	}
}

// loadConfig 解析默认工作区与配置路径，再加载合并后的运行配置。
func (command Command) loadConfig(workspace, configuredPath string, overrides config.Overrides) (config.Config, error) {
	if workspace == "" {
		current, err := os.Getwd()
		if err != nil {
			return config.Config{}, err
		}
		workspace = current
	}
	if configuredPath == "" {
		configuredPath = config.DefaultPath(workspace)
	}
	return config.Load(configuredPath, overrides)
}

// usage 向错误输出写入 CLI 使用说明与退出码语义。
func (command Command) usage() {
	fmt.Fprint(command.Stderr, `Cairn — an event-sourced coding agent

Usage:
  cairn run <task> [--workspace <path>] [--state-dir <path>] [--config <path>] [--base-url <url>] [--model <name>] [--api-key-env <name>] [--max-turns <n>] [--max-tool-calls <n>] [--max-input-tokens <n>] [--max-output-tokens <n>] [--max-wall-time <duration>] [--max-cost-usd <n>] [--json]
  cairn resume <run-id> [--approve <fingerprint> | --deny <fingerprint> | --resolve <op-id>=<verdict> --note <text> --evidence <ref> | --abort-run --reason <text>] [--json]
  cairn inspect [--workspace <path>] [--state-dir <path>] [--config <path>] [--events] [--json] <run-id>
  cairn evidence [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <run-id>
  cairn doctor [--workspace <path>] [--state-dir <path>]
  cairn memory capture --run <run-id> [--ttl <duration>] [--supersedes <candidate-id>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory recall [--limit <n>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory explain [--task-digest <sha256>] [--outcome <value>] [--failure-class <value>] [--trusted-verification any|required|not_required] [--created-from <UTC-RFC3339>] [--created-until <UTC-RFC3339>] [--limit <n>] [--precision compact|trace] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory inspect [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <candidate-id>
  cairn memory revoke --reason evidence_invalid|human_request [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <candidate-id>
  cairn memory consolidate [--min-support <n>] [--ttl <duration>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory reject --reason insufficient_evidence|human_request [--workspace <path>] [--state-dir <path>] [--config <path>] <derived-id>
  cairn memory candidate create --source-derived <id> --source-consolidation <id> --baseline-fingerprint <sha256> --max-prompt-tokens <n> --max-summary-bytes <n> --inventory-percent <n> --reserve-percent <n> [--ttl <duration>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory candidate reject --reason insufficient_evidence|evaluation_failed|safety_violation|human_request [--cooldown <duration>] [--workspace <path>] [--state-dir <path>] [--config <path>] <candidate-id>
  cairn memory candidate invalidate [--workspace <path>] [--state-dir <path>] [--config <path>] <candidate-id>
  cairn memory candidate expire [--workspace <path>] [--state-dir <path>] [--config <path>] <candidate-id>
  cairn memory candidate inspect [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <candidate-id>
  cairn benchmark run --local-config <path> [--task <id>] [--attempts <n>] [--manifest <path>] [--config <path>] [--prepare-timeout <duration>] [--prepare-proxy <url>] [--results-dir <path>] [--workspaces-dir <path>] [--state-dir <path>] [--private-dir <path>]
  cairn check-provider [--workspace <path>] [--config <path>] [--probe-tools] [--json]

Exit status:
  0 completed   2 usage/config   3 policy denied   4 failed
  5 cancelled   6 approval required   7 reconciliation required   8 state corrupt
`)
}

// stringList 支持 repeatable --task 参数，同时保留传入任务 ID 的顺序。
type stringList []string

// String 返回 flag 包显示的任务 ID 列表。
func (values *stringList) String() string { return strings.Join(*values, ",") }

// Set 追加一个非空任务 ID；空值会在参数解析阶段失败。
func (values *stringList) Set(value string) error {
	if strings.TrimSpace(value) == "" {
		return errors.New("task ID must not be empty")
	}
	*values = append(*values, value)
	return nil
}

// writeJSON 将值编码为一行 JSON；编码失败时不向 stdout 泄露部分数据。
func writeJSON(writer io.Writer, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(writer, string(encoded))
}
