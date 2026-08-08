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

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/app"
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
	case "help", "--help", "-h":
		command.usage()
		return ExitOK
	default:
		fmt.Fprintf(command.Stderr, "unknown command %q\n", args[0])
		command.usage()
		return ExitUsage
	}
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
		if result.Projection.LastAssistantMessage != "" {
			fmt.Fprintf(command.Stdout, "response: %s\n", result.Projection.LastAssistantMessage)
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
	if err := command.Service.Doctor(ctx, *workspace, *stateDir, configuration); err != nil {
		fmt.Fprintln(command.Stderr, "doctor failed:", err)
		return ExitCorrupt
	}
	fmt.Fprintln(command.Stdout, "journal: ok")
	return ExitOK
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

Exit status:
  0 completed   2 usage/config   3 policy denied   4 failed
  5 cancelled   6 approval required   7 reconciliation required   8 state corrupt
`)
}

// writeJSON 将值编码为一行 JSON；编码失败时不向 stdout 泄露部分数据。
func writeJSON(writer io.Writer, value any) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(writer, string(encoded))
}
