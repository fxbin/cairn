// 工作流域子命令：片 1 仅提供只读 status；执行、恢复与审批入口随片 2 接入。
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/workflow"
)

// workflowCommand 分派工作流域的固定子命令，不接受任意脚本。
func (command Command) workflowCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "workflow requires a subcommand: status")
		return ExitUsage
	}
	switch arguments[0] {
	case "status":
		return command.workflowStatus(ctx, arguments[1:])
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
	return ExitOK
}
