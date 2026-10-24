// benchmark 与评测集冻结子命令。
// Author: fxbin
package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/fxbin/cairn/internal/benchmark"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/memory"
)

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
		markOutputErrorReported(command.Stdout)
		fmt.Fprintln(command.Stderr, "write benchmark report:", err)
		return ExitFailed
	}
	if *prepareOnly {
		if report.Outcome == "prepared" {
			return ExitOK
		}
		return ExitFailed
	}
	if report.Passed() {
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
	// 首轮冻结合同要求显式三任务清单：缺省展开为清单全部任务。
	freezeTaskIDs := taskIDs
	if len(freezeTaskIDs) == 0 {
		for _, task := range pack.Tasks {
			freezeTaskIDs = append(freezeTaskIDs, task.ID)
		}
	}
	report, err := benchmark.FreezeEvaluationAssets(ctx, benchmark.FreezeEvalSetOptions{
		Pack: pack, TaskIDs: freezeTaskIDs, AttemptsPerTask: *attemptsPerTask,
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
		return command.writeJSONOutput("benchmark freeze-eval-set", report)
	}
	fmt.Fprintf(command.Stdout, "frozen: eval_set=%s fingerprint=%s\ncredential policy=%s fingerprint=%s\n", report.EvalSetPath, report.EvalSetFingerprint, report.CredentialPolicyPath, report.CredentialPolicyFingerprint)
	return ExitOK
}

// memory 分派受限可信记忆控制面命令。它只暴露 capture、recall、explain、inspect、revoke、
// consolidate、reject、candidate 八个固定动作，绝不接收或输出任务正文、模型文本、文件内容或自由文本摘要。
