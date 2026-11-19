// [INPUT]: 依赖 App 工作流历史及显式执行入口。
// [OUTPUT]: 保留固定计划命令，并分派版本化工作流命令。
// [POS]: CLI 展示 App 投影。JSON 标准输出为 NDJSON。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件连接工作流命令分派、旧计划和实例结果。
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
)

// workflowCommand 分派固定计划与已声明定义的工作流命令。
func (command Command) workflowCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "workflow requires a subcommand: status, run, resume, accept, definitions, save-definition, start, history, pause, continue, cancel, approve, reject, retry, reconcile")
		return ExitUsage
	}
	switch arguments[0] {
	case "status":
		return command.workflowStatus(ctx, arguments[1:])
	case "run":
		return command.workflowRun(ctx, arguments[1:])
	case "resume":
		return command.workflowResume(ctx, arguments[1:])
	case "accept":
		return command.workflowAccept(ctx, arguments[1:])
	case "definitions":
		return command.workflowDefinitions(ctx, arguments[1:])
	case "save-definition":
		return command.workflowSaveDefinition(ctx, arguments[1:])
	case "start":
		return command.workflowStart(ctx, arguments[1:])
	case "history":
		return command.workflowHistory(ctx, arguments[1:])
	case "pause", "continue", "cancel", "retry":
		return command.workflowControl(ctx, arguments[0], arguments[1:])
	case "approve", "reject":
		return command.workflowDecide(ctx, arguments[0], arguments[1:])
	case "reconcile":
		return command.workflowReconcile(ctx, arguments[1:])
	default:
		fmt.Fprintf(command.Stderr, "unknown workflow subcommand %q\n", arguments[0])
		return ExitUsage
	}
}

// workflowStatus 输出指定工作流的重放状态投影；只读，不触碰执行路径。
func (command Command) workflowStatus(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("workflow status", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write the workflow status as NDJSON to stdout")
	normalized := normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {},
	})
	if err := flags.Parse(normalized); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "workflow status requires a workflow ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	view, err := command.Service.WorkflowStatus(ctx, *workspace, *stateDir, configuration, app.WorkflowID(flags.Arg(0)))
	if err != nil {
		fmt.Fprintln(command.Stderr, "workflow status failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("workflow status", view)
	}
	command.printWorkflowView(view)
	return ExitOK
}

// workflowRun 创建固定计划工作流并顺序推进；需要人工介入时按稳定退出码
// 退出并在错误信息中给出恢复指引。
func (command Command) workflowRun(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("workflow run", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write the workflow outcome as NDJSON to stdout")
	normalized := normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {},
	})
	if err := flags.Parse(normalized); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "workflow run requires a task text")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	outcome, err := command.Service.WorkflowRun(ctx, app.WorkflowRunOptions{
		Task: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration,
	})
	return command.reportWorkflowOutcome("workflow run", outcome, err, *jsonOutput)
}

// workflowResume 恢复既有工作流：回填中断步骤、续跑遗留 run 并顺序推进；
// 暂停的实例通过显式 continue 解除。
func (command Command) workflowResume(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("workflow resume", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	seq := flags.Uint64("seq", 0, "observed workflow sequence")
	jsonOutput := flags.Bool("json", false, "write the workflow outcome as NDJSON to stdout")
	normalized := normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {}, "--seq": {},
	})
	if err := flags.Parse(normalized); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "workflow resume requires a workflow ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	outcome, err := command.Service.WorkflowResume(ctx, app.WorkflowResumeOptions{
		WorkflowID: app.WorkflowID(flags.Arg(0)), Workspace: *workspace, StateDir: *stateDir, Config: configuration, Sequence: *seq,
	})
	return command.reportWorkflowOutcome("workflow resume", outcome, err, *jsonOutput)
}

// workflowAccept 显式通过人工审批门；仅当工作流停在 accept 步骤时生效。
func (command Command) workflowAccept(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("workflow accept", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write the workflow outcome as NDJSON to stdout")
	normalized := normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {},
	})
	if err := flags.Parse(normalized); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "workflow accept requires a workflow ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	outcome, err := command.Service.WorkflowAccept(ctx, app.WorkflowAcceptOptions{
		WorkflowID: app.WorkflowID(flags.Arg(0)), Workspace: *workspace, StateDir: *stateDir, Config: configuration,
	})
	return command.reportWorkflowOutcome("workflow accept", outcome, err, *jsonOutput)
}

// reportWorkflowOutcome 输出可继续的实例身份并保持稳定退出码。
func (command Command) reportWorkflowOutcome(label string, outcome app.WorkflowOutcome, err error, jsonOutput bool) int {
	if err == nil || (outcome.View.Definition != nil && outcome.View.WorkflowID != "") {
		if jsonOutput {
			if code := command.writeJSONOutput(label, outcome); code != ExitOK {
				return code
			}
		} else {
			command.printWorkflowView(outcome.View)
			if outcome.NextAction != "" {
				fmt.Fprintln(command.Stdout, "next action:", outcome.NextAction)
			}
		}
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, label+" failed:", err)
		return exitFor(err)
	}
	return ExitOK
}

// printWorkflowView 输出人类可读的工作流状态块；只读查询与执行类命令共用。
func (command Command) printWorkflowView(view app.WorkflowStatusView) {
	state := "running"
	if view.Phase != "" {
		state = view.Phase
	}
	if view.Finished {
		state = "finished: " + view.FinishState
	}
	fmt.Fprintf(command.Stdout, "workflow: %s\nstate: %s\n", view.WorkflowID, state)
	fmt.Fprintf(command.Stdout, "created: %s\n", view.CreatedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(command.Stdout, "workspace: %s\n", view.WorkspaceHash)
	fmt.Fprintf(command.Stdout, "config fingerprint: %s\n", view.ConfigFingerprint)
	fmt.Fprintf(command.Stdout, "plan digest: %s\n", view.PlanSHA256)
	fmt.Fprintf(command.Stdout, "authorization: %s\n", view.Authorization)
	if view.Definition != nil {
		fmt.Fprintf(command.Stdout, "definition: %s · revision %d · %s\n", view.Definition.ID, view.Definition.Revision, view.DefinitionName)
		fmt.Fprintf(command.Stdout, "sequence: %d\n", view.Sequence)
	}
	if approval := view.PendingApproval; approval != nil {
		fmt.Fprintf(command.Stdout, "pending approval: run %s · operation %s · fingerprint %s\n", approval.RunID, approval.OperationID, approval.Fingerprint)
		if approval.Summary != "" {
			fmt.Fprintln(command.Stdout, approval.Summary)
		}
	}
	if pending := view.PendingReconcile; pending != nil {
		fmt.Fprintf(command.Stdout, "pending reconciliation: run %s · operation %s · %s · %s\n",
			pending.RunID, pending.OperationID, pending.ToolName, pending.SideEffectClass)
	}
	if len(view.RetryTargets) != 0 {
		fmt.Fprintln(command.Stdout, "retry targets:", strings.Join(view.RetryTargets, ", "))
	}
	if view.Finished {
		fmt.Fprintln(command.Stdout, "next step: none")
	} else if len(view.RequiresReconcile) > 0 {
		fmt.Fprintf(command.Stdout, "next step: blocked, reconcile first: %s\n", strings.Join(view.RequiresReconcile, ","))
	} else if view.ResumeIndex >= 0 && view.ResumeIndex < len(view.Steps) {
		next := view.Steps[view.ResumeIndex]
		fmt.Fprintf(command.Stdout, "next step: %d. %s (%s)\n", next.Index+1, next.ID, next.Kind)
	} else {
		fmt.Fprintln(command.Stdout, "next step: none")
	}
	for _, step := range view.Steps {
		line := fmt.Sprintf("  %d. %s [%s] %s", step.Index+1, step.ID, step.Kind, step.State)
		if step.ErrorClass != "" {
			line += fmt.Sprintf(" (%s)", step.ErrorClass)
		}
		fmt.Fprintln(command.Stdout, line)
		for _, attempt := range step.Attempts {
			fmt.Fprintf(command.Stdout, "     attempt %d · %s", attempt.Number, attempt.State)
			if attempt.RunID != "" {
				fmt.Fprintf(command.Stdout, " · run %s", attempt.RunID)
			}
			if attempt.Decision != "" {
				fmt.Fprintf(command.Stdout, " · %s", attempt.Decision)
			}
			fmt.Fprintln(command.Stdout)
		}
	}
}
