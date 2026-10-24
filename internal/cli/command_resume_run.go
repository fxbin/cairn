// resume / run 子命令与参数规范化。
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

func (command Command) resume(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("resume", flag.ContinueOnError)
	mode := flags.String("mode", "", "named mode from config modes map")
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
	trialActivation := flags.Bool("trial-activation", false, "resolve the sandbox-activated compaction variant before resuming")
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
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{Mode: *mode})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	configuration, err = command.resolveTrialActivation(ctx, *trialActivation, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "trial activation error:", err)
		return ExitFailed
	}
	var outputErr error
	onEvent := func(event domain.Event) {
		if *jsonOutput && outputErr == nil {
			outputErr = writeJSONChecked(command.Stdout, struct {
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
		if outputErr != nil {
			markOutputErrorReported(command.Stdout)
			fmt.Fprintln(command.Stderr, "reconcile output failed:", outputErr)
			return ExitFailed
		}
		if err != nil {
			fmt.Fprintln(command.Stderr, "reconcile failed:", err)
			return exitFor(err)
		}
		if *jsonOutput {
			return command.writeJSONOutput("reconcile", struct {
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
		if outputErr != nil {
			markOutputErrorReported(command.Stdout)
			fmt.Fprintln(command.Stderr, "abort output failed:", outputErr)
			return ExitFailed
		}
		if err != nil {
			fmt.Fprintln(command.Stderr, "abort failed:", err)
			return exitFor(err)
		}
		if *jsonOutput {
			if code := command.writeJSONOutput("abort", struct {
				Type     string                `json:"type"`
				RunID    domain.RunID          `json:"run_id"`
				Manifest *contract.RunManifest `json:"manifest,omitempty"`
			}{Type: "aborted", RunID: result.ID, Manifest: manifestOf(result)}); code != ExitOK {
				return code
			}
		}
		return ExitFailed
	}
	fingerprint := *approve
	approved := true
	if fingerprint == "" && *deny != "" {
		fingerprint = *deny
		approved = false
	}
	result, err := command.Service.Resume(ctx, app.ResumeOptions{RunID: domain.RunID(flags.Arg(0)), Workspace: *workspace, StateDir: *stateDir, Config: configuration, Approval: ports.Approval{Fingerprint: fingerprint, Approve: approved, Reason: *reason}, OnEvent: onEvent})
	if outputErr != nil {
		markOutputErrorReported(command.Stdout)
		fmt.Fprintln(command.Stderr, "resume output failed:", outputErr)
		return ExitFailed
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, "resume failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("resume", struct {
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
	mode := flags.String("mode", "", "named mode from config modes map (overrides active_mode)")
	trialActivation := flags.Bool("trial-activation", false, "resolve the sandbox-activated compaction variant before running (explicit trial opt-in)")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeRunArguments(arguments)); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" {
		fmt.Fprintln(command.Stderr, "run requires exactly one task argument")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{
		Mode:    *mode,
		BaseURL: *baseURL, Model: *model, APIKeyEnv: *apiKeyEnv, MaxTurns: *maxTurns,
		MaxToolCalls: *maxToolCalls, MaxInputTokens: *maxInputTokens,
		MaxOutputTokens: *maxOutputTokens, MaxWallTime: *maxWallTime, MaxCostUSD: *maxCostUSD,
	})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	configuration, err = command.resolveTrialActivation(ctx, *trialActivation, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "trial activation error:", err)
		return ExitFailed
	}
	var outputErr error
	onEvent := func(event domain.Event) {
		if *jsonOutput && outputErr == nil {
			outputErr = writeJSONChecked(command.Stdout, struct {
				Type  string       `json:"type"`
				Event domain.Event `json:"event"`
			}{Type: "event", Event: event})
		}
	}
	result, err := command.Service.Run(ctx, app.RunOptions{Task: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration, OnEvent: onEvent})
	if outputErr != nil {
		markOutputErrorReported(command.Stdout)
		fmt.Fprintln(command.Stderr, "run output failed:", outputErr)
		return ExitFailed
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, "run failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("run", struct {
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
