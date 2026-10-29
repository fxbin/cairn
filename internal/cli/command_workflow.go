// 工作流域子命令：status 只读重放；run/resume/accept 是显式执行、恢复与人
// 工门入口（ADR-0021 片 2）。所有命令不接受任意脚本，--json 仅输出 NDJSON。
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/workflow"
)

// workflowCommand 分派工作流域的固定子命令，不接受任意脚本。
func (command Command) workflowCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "workflow requires a subcommand: status, run, resume, accept")
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
	view, err := command.Service.WorkflowStatus(ctx, *workspace, *stateDir, configuration, workflow.ID(flags.Arg(0)))
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
// 审批与对账一律先在 run 级 resume 入口解决。
func (command Command) workflowResume(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("workflow resume", flag.ContinueOnError)
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
		fmt.Fprintln(command.Stderr, "workflow resume requires a workflow ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	outcome, err := command.Service.WorkflowResume(ctx, app.WorkflowResumeOptions{
		WorkflowID: workflow.ID(flags.Arg(0)), Workspace: *workspace, StateDir: *stateDir, Config: configuration,
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
		WorkflowID: workflow.ID(flags.Arg(0)), Workspace: *workspace, StateDir: *stateDir, Config: configuration,
	})
	return command.reportWorkflowOutcome("workflow accept", outcome, err, *jsonOutput)
}

// reportWorkflowOutcome 统一渲染执行类工作流命令：错误映射稳定退出码，成
// 功输出状态视图与下一步动作；--json 仅输出单行 NDJSON。
func (command Command) reportWorkflowOutcome(label string, outcome app.WorkflowOutcome, err error, jsonOutput bool) int {
	if err != nil {
		fmt.Fprintln(command.Stderr, label+" failed:", err)
		return exitFor(err)
	}
	if jsonOutput {
		return command.writeJSONOutput(label, outcome)
	}
	command.printWorkflowView(outcome.View)
	if outcome.NextAction != "" {
		fmt.Fprintln(command.Stdout, "next action:", outcome.NextAction)
	}
	return ExitOK
}

// printWorkflowView 输出人类可读的工作流状态块；只读查询与执行类命令共用。
func (command Command) printWorkflowView(view app.WorkflowStatusView) {
	state := "running"
	if view.Finished {
		state = "finished: " + view.FinishState
	}
	fmt.Fprintf(command.Stdout, "workflow: %s\nstate: %s\n", view.WorkflowID, state)
	fmt.Fprintf(command.Stdout, "created: %s\n", view.CreatedAt.Format("2006-01-02 15:04:05 MST"))
	fmt.Fprintf(command.Stdout, "workspace: %s\n", view.WorkspaceHash)
	fmt.Fprintf(command.Stdout, "config fingerprint: %s\n", view.ConfigFingerprint)
	fmt.Fprintf(command.Stdout, "plan digest: %s\n", view.PlanSHA256)
	fmt.Fprintf(command.Stdout, "authorization: %s\n", view.Authorization)
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
	}
}
