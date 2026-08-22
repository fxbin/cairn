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
	"github.com/fxbin/cairn/internal/domain"
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
	case "doctor":
		return command.doctor(ctx, args[1:])
	case "resume":
		return command.resume(ctx, args[1:])
	case "benchmark":
		return command.benchmark(ctx, args[1:])
	case "help", "--help", "-h":
		command.usage()
		return ExitOK
	default:
		fmt.Fprintf(command.Stderr, "unknown command %q\n", args[0])
		command.usage()
		return ExitUsage
	}
}

// benchmark 分派由调用方显式指定的本地评测配置，执行准备或实际评测；它不接受任意 shell 命令。
func (command Command) benchmark(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 || arguments[0] != "run" {
		fmt.Fprintln(command.Stderr, "benchmark requires the run subcommand")
		return ExitUsage
	}
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
	if err := flags.Parse(arguments[1:]); err != nil {
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
				Type     string       `json:"type"`
				RunID    domain.RunID `json:"run_id"`
				Response string       `json:"response"`
			}{Type: "result", RunID: result.ID, Response: result.Response})
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
				Type  string       `json:"type"`
				RunID domain.RunID `json:"run_id"`
			}{Type: "aborted", RunID: result.ID})
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
			Type     string       `json:"type"`
			RunID    domain.RunID `json:"run_id"`
			Response string       `json:"response"`
		}{Type: "result", RunID: result.ID, Response: result.Response})
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
			Type     string       `json:"type"`
			RunID    domain.RunID `json:"run_id"`
			Response string       `json:"response"`
		}{Type: "result", RunID: result.ID, Response: result.Response})
	} else {
		fmt.Fprintln(command.Stdout, result.Response)
	}
	return ExitOK
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
	if report.Sandbox.Backend == "docker" {
		fmt.Fprintf(command.Stdout, "sandbox: docker image %s available=%s\n", report.Sandbox.DockerImage, report.Sandbox.ImageAvailable)
	} else {
		fmt.Fprintf(command.Stdout, "sandbox: %s\n", report.Sandbox.Backend)
	}
	fmt.Fprintf(command.Stdout, "tools: %s\n", strings.Join(report.Tools, ", "))
	fmt.Fprintf(command.Stdout, "budget: turns %d, tool calls %d, input %d, output %d, wall %s\n", report.Capabilities.Budget.MaxTurns, report.Capabilities.Budget.MaxToolCalls, report.Capabilities.Budget.MaxInputTokens, report.Capabilities.Budget.MaxOutputTokens, report.Capabilities.Budget.MaxWallTime)
	fmt.Fprintf(command.Stdout, "verification: %d command(s) %s\n", report.Capabilities.Verification.Commands, report.Capabilities.Verification.Fingerprint)
	fmt.Fprintf(command.Stdout, "verification commands: %d\n", report.VerificationCount)
	for _, entry := range report.Resolution {
		fmt.Fprintf(command.Stdout, "resolved %s = %s (%s)\n", entry.Field, entry.Value, entry.Source)
	}
	fmt.Fprintf(command.Stdout, "journal: %s ok=%t artifacts=%t\n", report.JournalState, report.JournalOK, report.ArtifactsOK)
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
  cairn inspect <run-id> [--workspace <path>] [--events] [--json]
  cairn doctor [--workspace <path>] [--state-dir <path>]
  cairn benchmark run --local-config <path> [--task <id>] [--attempts <n>] [--manifest <path>] [--config <path>] [--prepare-timeout <duration>] [--prepare-proxy <url>] [--results-dir <path>] [--workspaces-dir <path>] [--state-dir <path>] [--private-dir <path>]

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
