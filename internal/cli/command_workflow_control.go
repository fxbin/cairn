// [INPUT]: 依赖工作流控制门面和客户端已显示的链头。
// [OUTPUT]: 暴露暂停、继续、取消、决策、返工和对账子命令。
// [POS]: CLI 绑定显式的步骤、序号与审批身份。应用层核验并写入。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件适配工作流的显式控制决定。
// Author: fxbin
package cli

import (
	"context"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
)

// workflowControl 提交实例级控制，或提交绑定失败步骤的重试决定。
func (command Command) workflowControl(ctx context.Context, action string, arguments []string) int {
	label := "workflow " + action
	signature := "<workflow-id> --seq <n> --reason <text>"
	count := 1
	if action == "retry" {
		count = 2
		signature = "<workflow-id> <step-id> --seq <n> --reason <text>"
	}
	flags := command.appFlags(label, signature)
	seq := flags.Uint64("seq", 0, "observed workflow sequence")
	reason := flags.String("reason", "", "reason for the explicit decision")
	if !flags.parse(arguments, count) {
		return ExitUsage
	}
	if *seq == 0 || strings.TrimSpace(*reason) == "" {
		flags.Usage()
		return ExitUsage
	}
	scope, err := command.workflowScope(flags)
	if err != nil {
		return ExitUsage
	}
	options := app.WorkflowControlOptions{
		Scope: scope, WorkflowID: app.WorkflowID(flags.Arg(0)), Sequence: *seq, Reason: *reason,
	}
	var outcome app.WorkflowOutcome
	switch action {
	case "pause":
		outcome, err = command.Service.PauseWorkflow(ctx, options)
	case "continue":
		outcome, err = command.Service.ResumeWorkflow(ctx, options)
	case "cancel":
		outcome, err = command.Service.CancelWorkflow(ctx, options)
	case "retry":
		outcome, err = command.Service.RetryStep(ctx, app.WorkflowRetryOptions{WorkflowControlOptions: options, StepID: flags.Arg(1)})
	}
	return command.reportWorkflowOutcome(label, outcome, err, flags.jsonOutput)
}

// workflowDecide 将人工门决定或具体运行工具审批提交到应用层。
func (command Command) workflowDecide(ctx context.Context, action string, arguments []string) int {
	label := "workflow " + action
	flags := command.appFlags(label, "<workflow-id> <step-id> --seq <n> --reason <text> [--run <id> --operation <id> --fingerprint <digest>]")
	seq := flags.Uint64("seq", 0, "observed workflow sequence")
	reason := flags.String("reason", "", "reason for the explicit decision")
	run := flags.String("run", "", "run shown in the pending approval")
	operation := flags.String("operation", "", "operation shown in the pending approval")
	fingerprint := flags.String("fingerprint", "", "fingerprint shown in the pending approval")
	if !flags.parse(arguments, 2) {
		return ExitUsage
	}
	decisionFields := 0
	for _, field := range []string{*run, *operation, *fingerprint} {
		if strings.TrimSpace(field) != "" {
			decisionFields++
		}
	}
	if *seq == 0 || strings.TrimSpace(*reason) == "" || (decisionFields != 0 && decisionFields != 3) {
		flags.Usage()
		return ExitUsage
	}
	scope, err := command.workflowScope(flags)
	if err != nil {
		return ExitUsage
	}
	options := app.WorkflowDecisionOptions{
		Scope: scope, WorkflowID: app.WorkflowID(flags.Arg(0)), StepID: flags.Arg(1), Sequence: *seq, Reason: *reason,
		RunID: domain.RunID(*run), OperationID: domain.OperationID(*operation), Fingerprint: *fingerprint,
	}
	var outcome app.WorkflowOutcome
	if action == "approve" {
		outcome, err = command.Service.ApproveStep(ctx, options)
	} else {
		outcome, err = command.Service.RejectStep(ctx, options)
	}
	return command.reportWorkflowOutcome(label, outcome, err, flags.jsonOutput)
}

// workflowReconcile 提交具名步骤的外部观察，不把未知副作用解释成重试许可。
func (command Command) workflowReconcile(ctx context.Context, arguments []string) int {
	flags := command.appFlags("workflow reconcile", "<workflow-id> <step-id> --seq <n> --reason <text> --run <id> --operation <id> --verdict <value> [--evidence <ref>]")
	seq := flags.Uint64("seq", 0, "observed workflow sequence")
	reason := flags.String("reason", "", "reason for the reconciliation")
	run := flags.String("run", "", "run shown in pending reconciliation")
	operation := flags.String("operation", "", "operation that requires reconciliation")
	verdict := flags.String("verdict", "", "reconciliation verdict")
	evidence := flags.String("evidence", "", "reference to external evidence")
	if !flags.parse(arguments, 2) {
		return ExitUsage
	}
	if *seq == 0 || strings.TrimSpace(*reason) == "" || strings.TrimSpace(*run) == "" || strings.TrimSpace(*operation) == "" || strings.TrimSpace(*verdict) == "" {
		flags.Usage()
		return ExitUsage
	}
	scope, err := command.workflowScope(flags)
	if err != nil {
		return ExitUsage
	}
	outcome, err := command.Service.ReconcileWorkflowStep(ctx, app.WorkflowReconcileOptions{
		WorkflowControlOptions: app.WorkflowControlOptions{
			Scope: scope, WorkflowID: app.WorkflowID(flags.Arg(0)), Sequence: *seq, Reason: *reason,
		},
		StepID: flags.Arg(1), RunID: domain.RunID(*run), OperationID: domain.OperationID(*operation), Verdict: *verdict, Evidence: *evidence,
	})
	return command.reportWorkflowOutcome("workflow reconcile", outcome, err, flags.jsonOutput)
}
