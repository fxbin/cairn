// [INPUT]: 依赖持久任务应用门面、运行身份和 CLI 配置解析。
// [OUTPUT]: 暴露任务定义、进度、会话分配及生命周期命令。
// [POS]: CLI 仅提交显式修订和身份。状态与证据来自应用层。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// 本文件提供持久任务入口。
// Author: fxbin
package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// taskCommand 分派持久任务查询、分配和生命周期命令。
func (command Command) taskCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "task requires a subcommand: create, get, progress, run, pause, resume, cancel, complete")
		return ExitUsage
	}
	switch arguments[0] {
	case "create":
		return command.taskCreate(ctx, arguments[1:])
	case "get":
		return command.taskGet(ctx, arguments[1:])
	case "run":
		return command.taskRun(ctx, arguments[1:])
	case "progress", "pause", "resume", "cancel", "complete":
		return command.taskChange(ctx, arguments[0], arguments[1:])
	default:
		fmt.Fprintf(command.Stderr, "unknown task subcommand %q\n", arguments[0])
		return ExitUsage
	}
}

// taskScope 为持久任务复用统一工作区旗标。
func (command Command) taskScope(flags *appCommandFlags) (app.TaskScope, error) {
	configuration, err := command.loadAppConfiguration(flags)
	return app.TaskScope{Workspace: flags.workspace, StateDir: flags.stateDir, Config: configuration}, err
}

// taskCreate 保存用户提供的目标、验收条件和可选的结构化检查绑定。
func (command Command) taskCreate(ctx context.Context, arguments []string) int {
	flags := command.appFlags("task create", "<goal> --accept <condition> [--accept <condition> ...] [--check <ids> ... | --coding] [--criterion-id <id> ...]")
	var acceptance stringListFlag
	var checks stringListFlag
	var criterionIDs stringListFlag
	useCoding := flags.Bool("coding", false, "bind acceptance to standard Coding checks (build/test/lint/diff) by position; requires verification commands with matching id in config")
	flags.Var(&acceptance, "accept", "acceptance condition (repeatable)")
	flags.Var(&checks, "check", "comma-separated verification check ids bound to the acceptance at the same position")
	flags.Var(&criterionIDs, "criterion-id", "stable criterion id at the same position (auto-generated when absent)")
	if !flags.parse(arguments, 1) {
		return ExitUsage
	}
	if len(acceptance) == 0 {
		flags.Usage()
		return ExitUsage
	}
	selectedChecks := []string(checks)
	if *useCoding {
		if len(checks) != 0 {
			fmt.Fprintln(command.Stderr, "acceptance error: --coding cannot be combined with --check")
			return ExitUsage
		}
		standard := app.CodingCheckIDs()
		if len(acceptance) > len(standard) {
			fmt.Fprintln(command.Stderr, "acceptance error: more criteria than standard Coding checks")
			return ExitUsage
		}
		selectedChecks = standard[:len(acceptance)]
	}
	criteria, err := buildAcceptanceCriteria(acceptance, selectedChecks, criterionIDs)
	if err != nil {
		fmt.Fprintln(command.Stderr, "acceptance error:", err)
		return ExitUsage
	}
	scope, err := command.taskScope(flags)
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	definition := app.TaskDefinition{Goal: flags.Arg(0), Acceptance: acceptance}
	if len(criteria) != 0 {
		definition.Criteria = criteria
	}
	current, err := command.Service.CreateTask(ctx, scope, definition)
	return command.reportTask("task create", flags.jsonOutput, &current, nil, err)
}

// buildAcceptanceCriteria 把并列的 --accept/--check/--criterion-id 参数组装为
// 结构化合同。未提供任何 --check 时返回空切片，保持旧任务的自由文本兼容。
func buildAcceptanceCriteria(acceptance, checks, criterionIDs []string) ([]contract.AcceptanceCriterion, error) {
	if len(checks) == 0 {
		if len(criterionIDs) != 0 {
			return nil, errors.New("--criterion-id requires --check")
		}
		return nil, nil
	}
	if len(checks) != len(acceptance) {
		return nil, errors.New("--check count must match --accept count")
	}
	if len(criterionIDs) != 0 && len(criterionIDs) != len(acceptance) {
		return nil, errors.New("--criterion-id count must match --accept count")
	}
	criteria := make([]contract.AcceptanceCriterion, 0, len(acceptance))
	for index, statement := range acceptance {
		id := fmt.Sprintf("criterion-%d", index+1)
		if len(criterionIDs) != 0 {
			id = strings.TrimSpace(criterionIDs[index])
		}
		bound := strings.Split(checks[index], ",")
		for position, check := range bound {
			bound[position] = strings.TrimSpace(check)
		}
		criteria = append(criteria, contract.AcceptanceCriterion{ID: id, Statement: statement, Required: true, Checks: bound})
	}
	return criteria, nil
}

// taskGet 展示任务当前修订及持久分配的运行历史。
func (command Command) taskGet(ctx context.Context, arguments []string) int {
	flags := command.appFlags("task get", "<task-id>")
	if !flags.parse(arguments, 1) {
		return ExitUsage
	}
	scope, err := command.taskScope(flags)
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	view, err := command.Service.GetTask(ctx, scope, app.TaskID(flags.Arg(0)))
	return command.reportTask("task get", flags.jsonOutput, nil, &view, err)
}

// taskRun 使用显式分配标识及预期任务修订启动或查回同一请求。
func (command Command) taskRun(ctx context.Context, arguments []string) int {
	flags := command.appFlags("task run", "<task-id> --revision <n> --assignment <id> --session <id> [--previous-run <run-id>]")
	revision := flags.Uint64("revision", 0, "expected task revision")
	assignment := flags.String("assignment", "", "idempotent assignment identifier")
	session := flags.String("session", "", "session identifier")
	previous := flags.String("previous-run", "", "previous run within the session")
	if !flags.parse(arguments, 1) {
		return ExitUsage
	}
	if *revision == 0 || strings.TrimSpace(*assignment) == "" || strings.TrimSpace(*session) == "" {
		flags.Usage()
		return ExitUsage
	}
	scope, err := command.taskScope(flags)
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	view, err := command.Service.RunTask(ctx, scope, app.TaskID(flags.Arg(0)), app.TaskRunOptions{
		Revision: *revision, AssignmentID: *assignment, SessionID: *session, PreviousRunID: domain.RunID(*previous),
	})
	return command.reportTask("task run", flags.jsonOutput, nil, &view, err)
}

// taskChange 提交绑定所见修订的状态迁移或进度替换。
func (command Command) taskChange(ctx context.Context, action string, arguments []string) int {
	label := "task " + action
	signature := "<task-id> --revision <n>"
	switch action {
	case "progress":
		signature += " [--summary <text>] [--remaining <item> ...]"
	case "complete":
		signature += " --run <run-id>"
	}
	flags := command.appFlags(label, signature)
	revision := flags.Uint64("revision", 0, "expected task revision")
	var progress app.TaskProgress
	var remaining stringListFlag
	var run string
	if action == "progress" {
		flags.StringVar(&progress.Summary, "summary", "", "user-confirmed progress summary")
		flags.Var(&remaining, "remaining", "remaining item (repeatable, replaces the previous list)")
	}
	if action == "complete" {
		flags.StringVar(&run, "run", "", "latest run with trusted verification")
	}
	if !flags.parse(arguments, 1) {
		return ExitUsage
	}
	if *revision == 0 || (action == "complete" && strings.TrimSpace(run) == "") {
		flags.Usage()
		return ExitUsage
	}
	scope, err := command.taskScope(flags)
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	id := app.TaskID(flags.Arg(0))
	var current app.Task
	switch action {
	case "progress":
		progress.Remaining = remaining
		current, err = command.Service.UpdateTaskProgress(ctx, scope, id, *revision, progress)
	case "pause":
		current, err = command.Service.PauseTask(ctx, scope, id, *revision)
	case "resume":
		current, err = command.Service.ResumeTask(ctx, scope, id, *revision)
	case "cancel":
		current, err = command.Service.CancelTask(ctx, scope, id, *revision)
	case "complete":
		current, err = command.Service.CompleteTask(ctx, scope, id, *revision, domain.RunID(run))
	}
	return command.reportTask(label, flags.jsonOutput, &current, nil, err)
}

// reportTask 输出可用的持久记录，随后报告审批、对账或其他错误。
func (command Command) reportTask(label string, jsonOutput bool, current *app.Task, view *app.TaskView, err error) int {
	if view != nil {
		current = &view.Task
	}
	if current.ID != "" {
		if jsonOutput {
			var value any = current
			if view != nil {
				value = view
			}
			if code := command.writeJSONOutput(label, value); code != ExitOK {
				return code
			}
		} else {
			command.printTask(*current)
			if view != nil {
				for _, assignment := range view.Assignments {
					fmt.Fprintf(command.Stdout, "  assignment %s · run %s · session %s · %s\n",
						assignment.ID, assignment.RunID, assignment.SessionID, assignment.Phase)
				}
			}
		}
	}
	if err != nil {
		fmt.Fprintln(command.Stderr, label+" failed:", err)
		return exitFor(err)
	}
	return ExitOK
}

// printTask 展示持久任务的目标、进度与已记录验收依据。
func (command Command) printTask(current app.Task) {
	fmt.Fprintf(command.Stdout, "task: %s\nstate: %s\nrevision: %d\ngoal: %s\n", current.ID, current.State, current.Revision, current.Definition.Goal)
	for _, item := range current.Definition.Acceptance {
		fmt.Fprintf(command.Stdout, "  acceptance: %s\n", item)
	}
	if current.Definition.AcceptanceDigest != "" {
		fmt.Fprintf(command.Stdout, "acceptance_digest: %s\n", current.Definition.AcceptanceDigest)
		for _, criterion := range current.Definition.Criteria {
			fmt.Fprintf(command.Stdout, "  criterion: %s required=%t checks=%s\n", criterion.ID, criterion.Required, strings.Join(criterion.Checks, ","))
		}
	}
	if current.Progress.Summary != "" {
		fmt.Fprintf(command.Stdout, "progress: %s\n", current.Progress.Summary)
	}
	for _, item := range current.Progress.Remaining {
		fmt.Fprintf(command.Stdout, "  remaining: %s\n", item)
	}
	if current.Completion != nil {
		fmt.Fprintf(command.Stdout, "completion: run %s seq %d %s\n", current.Completion.RunID, current.Completion.Sequence, current.Completion.EventHash)
	}
}
