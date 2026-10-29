// CLI 入口、分发与核心子命令。
// Author: fxbin
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tui"
	"github.com/mattn/go-isatty"
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

// outputTracker 保存标准流的首个输出错误及其诊断状态，供顶级 Run 统一
// 映射为失败退出码，并避免重复报告已由子命令处理的错误。
type outputTracker struct {
	writer   io.Writer
	err      error
	reported bool
}

// Write 将内容写入底层标准流，并保存首次写入失败。
func (tracker *outputTracker) Write(content []byte) (int, error) {
	written, err := tracker.writer.Write(content)
	if err != nil {
		tracker.record(err)
	}
	return written, err
}

// record 保存首个输出错误，避免后续失败覆盖根因。
func (tracker *outputTracker) record(err error) {
	if tracker.err == nil {
		tracker.err = err
	}
}

// markOutputErrorReported 标记调用方已经针对当前标准输出首错写出诊断。
func markOutputErrorReported(writer io.Writer) {
	if tracker, ok := writer.(*outputTracker); ok && tracker.err != nil {
		tracker.reported = true
	}
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
	stderr := command.Stderr
	stdoutTracker := &outputTracker{writer: command.Stdout}
	stderrTracker := &outputTracker{writer: command.Stderr}
	command.Stdout = stdoutTracker
	command.Stderr = stderrTracker
	exitCode := command.dispatch(ctx, args)
	if stdoutTracker.err != nil {
		if !stdoutTracker.reported {
			fmt.Fprintln(stderr, "stdout output failed:", stdoutTracker.err)
		}
		return ExitFailed
	}
	if stderrTracker.err != nil {
		fmt.Fprintln(stderr, "stderr output failed:", stderrTracker.err)
		return ExitFailed
	}
	return exitCode
}

// dispatch 分派顶级命令；输出错误由 Run 在统一出口处理。
func (command Command) dispatch(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return command.bareCommand(ctx)
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
	case "provider":
		return command.providerCommand(ctx, args[1:])
	case "mcp":
		return command.mcpCommand(ctx, args[1:])
	case "skills", "skill":
		return command.skillsCommand(ctx, args[1:])
	case "template", "templates":
		return command.templateCommand(ctx, args[1:])
	case "package", "packages":
		return command.packageCommand(ctx, args[1:])
	case "session":
		return command.sessionCommand(ctx, args[1:])
	case "workflow":
		return command.workflowCommand(ctx, args[1:])
	case "init":
		return command.initCommand(ctx, args[1:])
	case "tui":
		return command.tuiCommand(ctx, args[1:])
	case "help", "--help", "-h":
		command.usage()
		return ExitOK
	default:
		fmt.Fprintf(command.Stderr, "unknown command %q\n", args[0])
		command.usage()
		return ExitUsage
	}
}

// initCommand 解析首跑向导的旗标并执行：三要素可全部以旗标提供（脚本
// 化），缺省项进入交互问答。
func (command Command) initCommand(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("init", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	baseURL := flags.String("base-url", "", "provider base URL")
	model := flags.String("model", "", "model name")
	apiKeyEnv := flags.String("api-key-env", "", "environment variable name holding the API key")
	catalogURL := flags.String("catalog-url", "", "model metadata catalog URL override")
	catalogCache := flags.String("catalog-cache", "", "model metadata cache file path override")
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "init does not accept positional arguments")
		return ExitUsage
	}
	if *workspace != "" {
		if err := os.Chdir(*workspace); err != nil {
			fmt.Fprintln(command.Stderr, "workspace error:", err)
			return ExitFailed
		}
	}
	outcome, err := command.runInit(ctx, initFlags{BaseURL: *baseURL, Model: *model, APIKeyEnv: *apiKeyEnv, CatalogURL: *catalogURL, CatalogCache: *catalogCache})
	if err != nil {
		fmt.Fprintln(command.Stderr, "init failed:", err)
		return ExitFailed
	}
	fmt.Fprintf(command.Stdout, "\n配置已生成: %s\n", outcome.ConfigPath)
	for _, note := range outcome.ProbeNotes {
		fmt.Fprintf(command.Stdout, "  %s\n", note)
	}
	fmt.Fprintf(command.Stdout, "\n下一步:\n  cairn run \"你的任务\"\n  密钥两种方式任选: export %s=<密钥>，或重跑 cairn tui 在向导内配置\n", outcome.APIKeyEnv)
	return ExitOK
}

// benchmark 分派本地评测域的固定子命令：run 执行基准套件；freeze-eval-set
// 把本地冻结资产装配为受控评测集与凭据策略文件。它不接受任意 shell 命令。
func (command Command) inspect(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	trialActivation := flags.Bool("trial-activation", false, "resolve the sandbox-activated compaction variant before inspection")
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
	configuration, err = command.resolveTrialActivation(ctx, *trialActivation, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "trial activation error:", err)
		return ExitFailed
	}
	result, err := command.Service.Inspect(ctx, *workspace, *stateDir, configuration, domain.RunID(flags.Arg(0)), *includeEvents)
	if err != nil {
		fmt.Fprintln(command.Stderr, "inspect failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("inspect", result)
	} else {
		fmt.Fprintf(command.Stdout, "run: %s\nstate: %s\nlast sequence: %d\n", result.Projection.ID, result.Projection.Phase(), result.Projection.LastSeq)
		summary := result.Summary
		if summary.ConfigFingerprint != "" {
			fmt.Fprintf(command.Stdout, "config fingerprint: %s\n", summary.ConfigFingerprint)
		} else {
			fmt.Fprintln(command.Stdout, "config fingerprint: unavailable")
		}
		if summary.Build != nil {
			writeBuildIdentity(command.Stdout, "build", *summary.Build)
		} else {
			fmt.Fprintln(command.Stdout, "build: unavailable")
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
		return command.writeJSONOutput("evidence", result)
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
	trialActivation := flags.Bool("trial-activation", false, "resolve the sandbox-activated compaction variant before diagnostics")
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
	configuration, err = command.resolveTrialActivation(ctx, *trialActivation, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "trial activation error:", err)
		return ExitFailed
	}
	report, err := command.Service.Doctor(ctx, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "doctor failed:", err)
		return ExitCorrupt
	}
	if *jsonOutput {
		return command.writeJSONOutput("doctor", report)
	}
	fmt.Fprintf(command.Stdout, "ready: %t\n", report.Ready)
	fmt.Fprintf(command.Stdout, "config: %t (preset %s profile %s)\n", report.ConfigValid, report.Preset, report.Profile)
	fmt.Fprintf(command.Stdout, "config fingerprint: %s\n", report.ConfigFingerprint)
	writeBuildIdentity(command.Stdout, "build", report.Build)
	fmt.Fprintf(command.Stdout, "build valid: %t", report.BuildValid)
	if report.BuildError != "" {
		fmt.Fprintf(command.Stdout, " (%s)", report.BuildError)
	}
	fmt.Fprintln(command.Stdout)
	fmt.Fprintf(command.Stdout, "workspace: %s\n", report.WorkspaceHash)
	fmt.Fprintf(command.Stdout, "policy: write=%s exec=%s\n", report.Policy.Write, report.Policy.Exec)
	fmt.Fprintf(command.Stdout, "provider: %s model %s key env %s present=%t\n", report.Provider.BaseURL, report.Provider.Model, report.Provider.APIKeyEnv, report.Provider.APIKeyPresent)
	if report.ProviderCard != nil {
		card := report.ProviderCard
		fmt.Fprintf(command.Stdout, "provider card: %s protocol=%s tool_calls=%t streaming_usage=%t context=%d output=%d stops=%s source=%s\n", card.Model, card.Protocol, card.ToolCalls, card.StreamingUsage, card.ContextWindowTokens, card.MaxOutputTokens, strings.Join(card.StopReasons, "/"), card.Source)
	} else {
		fmt.Fprintf(command.Stdout, "provider card: unresolved (%s)\n", report.ProviderCardError)
	}
	fmt.Fprintf(command.Stdout, "admission: mode=%s admitted=%t", report.Admission.Mode, report.Admission.Admitted)
	if report.Admission.Error != "" {
		fmt.Fprintf(command.Stdout, " (%s)", report.Admission.Error)
	}
	fmt.Fprintln(command.Stdout)
	if report.Sandbox.Backend == "docker" {
		fmt.Fprintf(command.Stdout, "sandbox: docker image %s available=%s\n", report.Sandbox.DockerImage, report.Sandbox.ImageAvailable)
	} else {
		fmt.Fprintf(command.Stdout, "sandbox: %s\n", report.Sandbox.Backend)
	}
	fmt.Fprintf(command.Stdout, "tools: %s\n", strings.Join(report.Tools, ", "))
	fmt.Fprintf(command.Stdout, "budget: turns %d, tool calls %d, input %d, output %d, wall %s\n", report.Capabilities.Budget.MaxTurns, report.Capabilities.Budget.MaxToolCalls, report.Capabilities.Budget.MaxInputTokens, report.Capabilities.Budget.MaxOutputTokens, report.Capabilities.Budget.MaxWallTime)
	fmt.Fprintf(command.Stdout, "verification: %d command(s) %s\n", report.Capabilities.Verification.Commands, report.Capabilities.Verification.Fingerprint)
	// 三个观测面分面呈现：记忆侧只读聚合、语义面（Journal 事件投影）与
	// SDK 面（otelc 构建形态）互不替代，读者不会把其中一面当成采集成功。
	fmt.Fprintf(command.Stdout, "telemetry memory: %s\n", report.Telemetry.Status)
	fmt.Fprintf(command.Stdout, "telemetry semantic: %s", report.Telemetry.Semantic)
	if report.Telemetry.SemanticSignals != "" {
		fmt.Fprintf(command.Stdout, " signals=%s", report.Telemetry.SemanticSignals)
	}
	if report.Telemetry.SemanticReason != "" {
		fmt.Fprintf(command.Stdout, " reason=%s", report.Telemetry.SemanticReason)
	}
	fmt.Fprintln(command.Stdout)
	fmt.Fprintf(command.Stdout, "telemetry otlp: %s\n", report.Telemetry.OTLP)
	if exports := report.Telemetry.SemanticExports; exports != nil {
		fmt.Fprintf(command.Stdout, "telemetry exports: exported=%d failed=%d dropped=%d at=%s\n", exports.Exported, exports.Failed, exports.Dropped, exports.UpdatedAt.UTC().Format(time.RFC3339))
	}
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
func (command Command) resolveTrialActivation(ctx context.Context, requested bool, workspace, stateDir string, configuration config.Config) (config.Config, error) {
	if !requested {
		return configuration, nil
	}
	resolved, activated, err := command.Service.ResolveTrialActivation(ctx, workspace, stateDir, configuration)
	if err != nil {
		return config.Config{}, err
	}
	if !activated {
		fmt.Fprintln(command.Stderr, "trial activation: no sandbox promotion is active; refusing to silently run the baseline")
		return config.Config{}, errors.New("no active sandbox promotion to resolve")
	}
	return resolved, nil
}

// loadConfig 解析默认工作区与配置路径（缺省时按工作区→用户级回退，ADR-
// 0016），再加载合并后的运行配置。
func (command Command) loadConfig(workspace, configuredPath string, overrides config.Overrides) (config.Config, error) {
	if workspace == "" {
		current, err := os.Getwd()
		if err != nil {
			return config.Config{}, err
		}
		workspace = current
	}
	if configuredPath == "" {
		configuredPath = config.ResolveDefaultPath(workspace)
	}
	return config.Load(configuredPath, overrides)
}

// bareCommand 处理裸命令：交互终端下未配置（工作区与用户级皆缺，ADR-0016
// 回退链两处无配置）直接进入配置向导，已配置则装载配置并进入单任务运行
// 主界面——对齐同类工具"裸命令进入交互界面"的默认。非交互终端输出使用
// 说明，脚本永不因裸命令挂起；配置装载失败如实报错并指引 doctor，
// help/-h 永远纯文本。
func (command Command) bareCommand(ctx context.Context) int {
	if !isatty.IsTerminal(os.Stdin.Fd()) || !isatty.IsTerminal(os.Stdout.Fd()) {
		command.usage()
		return ExitUsage
	}
	workspace, err := os.Getwd()
	if err != nil {
		command.usage()
		return ExitUsage
	}
	if _, statErr := os.Stat(config.ResolveDefaultPath(workspace)); statErr != nil {
		return command.tuiCommand(ctx, []string{})
	}
	configuration, err := command.loadConfig(workspace, "", config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "配置装载失败（可运行 cairn doctor 诊断）:", err)
		return ExitUsage
	}
	consoleInput := tui.RunConsoleInput{Workspace: workspace, StateDir: app.DefaultStateDir(), Config: configuration, Service: command.Service}
	consoleInput.ReloadConfig = func() (config.Config, error) {
		return command.loadConfig(workspace, "", config.Overrides{})
	}
	if err := tui.RunConsoleLoop(ctx, consoleInput); err != nil {
		fmt.Fprintln(command.Stderr, "console failed:", err)
		return ExitFailed
	}
	return ExitOK
}

// usage 向错误输出写入 CLI 使用说明与退出码语义。
func (command Command) usage() {
	fmt.Fprint(command.Stderr, `Cairn — an event-sourced coding agent

快速开始:
  cairn              交互界面（未配置时进入配置向导；已配置时进入单任务运行）
  cairn run <task>   非交互运行一个任务

AGENTS.md 规约:
  新运行自动读取 Cairn 用户配置目录及工作区内可见的 AGENTS.md；项目规则优先于用户全局规则。
  子目录规则仅适用于所在目录及后代。
  文件修改从下一次新运行生效；恢复运行沿用开始时的规约。

Usage:
  cairn run <task> [--workspace <path>] [--state-dir <path>] [--config <path>] [--base-url <url>] [--model <name>] [--api-key-env <name>] [--max-turns <n>] [--max-tool-calls <n>] [--max-input-tokens <n>] [--max-output-tokens <n>] [--max-wall-time <duration>] [--max-cost-usd <n>] [--trial-activation] [--json]
  cairn resume <run-id> [--approve <fingerprint> | --deny <fingerprint> | --resolve <op-id>=<verdict> --note <text> --evidence <ref> | --abort-run --reason <text>] [--trial-activation] [--json]
  cairn inspect [--workspace <path>] [--state-dir <path>] [--config <path>] [--trial-activation] [--events] [--json] <run-id>
  cairn evidence [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <run-id>
  cairn doctor [--workspace <path>] [--state-dir <path>] [--trial-activation]
  cairn memory capture --run <run-id> [--ttl <duration>] [--supersedes <candidate-id>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory recall [--limit <n>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory explain [--task-digest <sha256>] [--outcome <value>] [--failure-class <value>] [--trusted-verification any|required|not_required] [--created-from <UTC-RFC3339>] [--created-until <UTC-RFC3339>] [--limit <n>] [--precision compact|trace] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory inspect [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <candidate-id>
  cairn memory revoke --reason evidence_invalid|human_request [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <candidate-id>
  cairn memory consolidate [--min-support <n>] [--ttl <duration>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory reject --reason insufficient_evidence|human_request [--workspace <path>] [--state-dir <path>] [--config <path>] <derived-id>
  cairn memory candidate create --source-derived <id> --source-consolidation <id> --baseline-fingerprint <sha256> --max-prompt-tokens <n> --max-summary-bytes <n> --inventory-percent <n> --reserve-percent <n> [--eval-set-fingerprint <sha256>] [--ttl <duration>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn benchmark run --local-config <yaml> [--task <id> ...] [directory overrides] [--prepare-only] [--json]
  cairn benchmark freeze-eval-set --local-config <yaml> --policy-profile-id <id> --suite-max-wall <duration> --comparator-version <label> --out-dir <dir> --max-prompt-tokens <n> --max-summary-bytes <n> --inventory-percent <n> --reserve-percent <n> [--task <id> ...] [--attempts-per-task <n>] [--cairn-root <path>] [--json]
  cairn memory candidate reject --reason insufficient_evidence|evaluation_failed|safety_violation|human_request [--cooldown <duration>] [--workspace <path>] [--state-dir <path>] [--config <path>] <candidate-id>
  cairn memory candidate invalidate [--workspace <path>] [--state-dir <path>] [--config <path>] <candidate-id>
  cairn memory candidate expire [--workspace <path>] [--state-dir <path>] [--config <path>] <candidate-id>
  cairn memory candidate inspect [--workspace <path>] [--state-dir <path>] [--config <path>] [--json] <candidate-id>
  cairn memory candidate evaluate --candidate <id> --local-config <yaml> --eval-set <json> --lease <json> --policy <json> --max-prompt-tokens <n> --max-summary-bytes <n> --inventory-percent <n> --reserve-percent <n> [--dry-run] [--cairn-root <path>] [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory candidate evidence --candidate <id> --eval-set <json> --suite-state-dir <dir> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory candidate promote --candidate <id> --approver <label> --approval-nonce <sha256> --approval-fingerprint <sha256> --suite-state-dir <dir> --max-prompt-tokens <n> --max-summary-bytes <n> --inventory-percent <n> --reserve-percent <n> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory promotion activate --version <id> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory promotion rollback --reason insufficient_evidence|evaluation_failed|safety_violation|human_request [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory promotion list [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn memory telemetry [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn benchmark run --local-config <path> [--task <id>] [--attempts <n>] [--manifest <path>] [--config <path>] [--prepare-timeout <duration>] [--prepare-proxy <url>] [--results-dir <path>] [--workspaces-dir <path>] [--state-dir <path>] [--private-dir <path>]
  cairn check-provider [--workspace <path>] [--config <path>] [--state-dir <path>] [--probe-tools] [--json]
  cairn provider list [--json]
  cairn provider presets [--workspace <path>] [--config <path>] [--json]
  cairn provider presets save <name> --base-url <url> --model <name> --api-key-env <name> [--admission optional|required] [--overwrite] [--workspace <path>] [--json]
  cairn provider presets remove <name> [--workspace <path>] [--config <path>] [--json]
  cairn provider presets show <name> [--workspace <path>] [--config <path>] [--json]
  cairn provider use <name> [--workspace <path>] [--config <path>] [--json]
  cairn mcp list [--workspace <path>] [--config <path>] [--json]
  cairn mcp login <server> [--workspace <path>] [--config <path>] [--timeout <duration>] [--json]
  cairn mcp logout <server> [--workspace <path>] [--config <path>] [--json]
  cairn skills list [--workspace <path>] [--shared] [--json]
  cairn skills show <name> [--workspace <path>] [--shared] [--json]
  cairn skills install <local-directory> [--workspace <path>] [--scope workspace|user] [--json]
  cairn skills remove|enable|disable <name> [--workspace <path>] [--scope workspace|user] [--json]
  cairn template list [--root <path>] [--json]
  cairn template show <name> [--root <path>] [--json]
  cairn template render <name> [--root <path>] [--param name=value]...
  cairn package inspect|install <dir> [--root <path>] [--json]
  cairn package list [--root <path>] [--json]
  cairn package enable|disable <name> [--root <path>]
  cairn session list|tree [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn session show <run-id> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn session fork --from <run-id> <task> [run flags] [--json]
  cairn session continue [--from <run-id>] <task> [run flags] [--json]
  cairn workflow status <workflow-id> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn workflow run <task> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn workflow resume <workflow-id> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn workflow accept <workflow-id> [--workspace <path>] [--state-dir <path>] [--config <path>] [--json]
  cairn provider models [--query <name>] [--provider <id>] [--tool-call] [--reasoning] [--refresh] [--catalog-url <url>] [--catalog-cache <path>] [--limit <n>] [--json]
  cairn provider probe [--workspace <path>] [--config <path>] [--state-dir <path>] [--probe-tools] [--json]
  cairn provider configure --base-url <url> --model <name> --api-key-env <name> [--admission optional|required] [--save-preset <name>] [--overwrite-preset] [--user] [--overwrite] [--catalog-url <url>] [--catalog-cache <path>] [--workspace <path>] [--json]
  cairn provider admit --evidence <sha256> --decision admitted|rejected --reason <text> [--supersedes <sha256>] [--workspace <path>] [--config <path>] [--state-dir <path>]
  cairn provider status [--workspace <path>] [--config <path>] [--state-dir <path>] [--json]
  cairn init [--base-url <url>] [--model <name>] [--api-key-env <name>] [--catalog-url <url>] [--workspace <path>]
  cairn tui [--workspace <path>] [--local] [--admission optional|required] [--catalog-url <url>] [--catalog-cache <path>]

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

// writeJSONChecked 将值完整编码为一行 JSON，并把编码或写入失败返回调用方。
