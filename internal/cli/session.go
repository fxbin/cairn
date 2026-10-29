// session.go 提供会话树与分支的窄命令行入口：list/show/tree 为只读投影，
// fork 以首次新任务创建跨会话分支，continue 从明确选定的唯一链尾续链。
// 选择与预览不留下无任务的幽灵运行；JSON 模式只向 stdout 输出 NDJSON。
//
// Author: fxbin
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

// sessionCommand 分派会话域的固定子命令；不接受任意 shell 命令。
func (command Command) sessionCommand(ctx context.Context, arguments []string) int {
	if len(arguments) == 0 {
		fmt.Fprintln(command.Stderr, "session requires list, show, tree, fork or continue")
		return ExitUsage
	}
	switch arguments[0] {
	case "list":
		return command.sessionList(ctx, arguments[1:])
	case "show":
		return command.sessionShow(ctx, arguments[1:])
	case "tree":
		return command.sessionTree(ctx, arguments[1:])
	case "fork":
		return command.sessionFork(ctx, arguments[1:])
	case "continue":
		return command.sessionContinue(ctx, arguments[1:])
	default:
		fmt.Fprintln(command.Stderr, "session requires list, show, tree, fork or continue")
		return ExitUsage
	}
}

// sessionList 只读列出当前工作区历史运行及其会话关系。
func (command Command) sessionList(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("session list", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "session list does not accept positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	items, err := command.Service.ListRuns(ctx, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "session list failed:", err)
		return exitFor(err)
	}
	for _, item := range items {
		if *jsonOutput {
			if writeJSONChecked(command.Stdout, item) != nil {
				return ExitFailed
			}
			continue
		}
		session := item.SessionID
		if session == "" {
			session = "未分组"
		}
		relation := ""
		if item.PrevRunID != "" {
			relation = " prev=" + string(item.PrevRunID)
		}
		if item.ForkSource != nil {
			relation = " fork_from=" + string(item.ForkSource.RunID)
		}
		fmt.Fprintf(command.Stdout, "%s · %s · 会话 %s · %s · %s%s\n", item.ID, item.Phase, session, item.State, firstLineClip(item.Task, 48), relation)
	}
	return ExitOK
}

// sessionShow 只读展示单个运行及其会话关系。
func (command Command) sessionShow(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("session show", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write JSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(command.Stderr, "session show requires a run ID")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	runID := domain.RunID(flags.Arg(0))
	tree, err := command.Service.ListSessionTree(ctx, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "session show failed:", err)
		return exitFor(err)
	}
	node, exists := tree.Nodes[runID]
	if !exists {
		fmt.Fprintf(command.Stderr, "run %s not found\n", runID)
		return ExitFailed
	}
	if *jsonOutput {
		return command.writeJSONOutput("session show", node)
	}
	session := node.SessionID
	if session == "" {
		session = "未分组"
	}
	fmt.Fprintf(command.Stdout, "run: %s\nsession: %s\nstate: %s (%s)\ntask: %s\n", node.ID, session, contract.RunPhaseLabel(node.Phase), node.Phase, node.Task)
	if node.PrevRunID != "" {
		fmt.Fprintf(command.Stdout, "prev: %s\n", node.PrevRunID)
	}
	if node.ForkSource != nil {
		fmt.Fprintf(command.Stdout, "fork from: %s (boundary %d %s)\n", node.ForkSource.RunID, node.ForkSource.LastSeq, node.ForkSource.LastEventHash)
	}
	if len(node.Children) > 0 {
		ids := make([]string, 0, len(node.Children))
		for _, child := range node.Children {
			ids = append(ids, string(child))
		}
		fmt.Fprintf(command.Stdout, "children: %s\n", strings.Join(ids, ", "))
	}
	fmt.Fprintf(command.Stdout, "created: %s\nupdated: %s\n", node.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), node.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z"))
	return ExitOK
}

// sessionTree 只读展示当前工作区会话树；不启动模型、外部 MCP 或写工作区。
func (command Command) sessionTree(ctx context.Context, arguments []string) int {
	flags := flag.NewFlagSet("session tree", flag.ContinueOnError)
	flags.SetOutput(command.Stderr)
	workspace := flags.String("workspace", "", "workspace path (default: current directory)")
	stateDir := flags.String("state-dir", "", "state directory")
	configPath := flags.String("config", "", "configuration path")
	jsonOutput := flags.Bool("json", false, "write JSON to stdout")
	if err := flags.Parse(arguments); err != nil {
		return ExitUsage
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(command.Stderr, "session tree does not accept positional arguments")
		return ExitUsage
	}
	configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
	if err != nil {
		fmt.Fprintln(command.Stderr, "configuration error:", err)
		return ExitUsage
	}
	tree, err := command.Service.ListSessionTree(ctx, *workspace, *stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "session tree failed:", err)
		return exitFor(err)
	}
	if *jsonOutput {
		return command.writeJSONOutput("session tree", tree)
	}
	if len(tree.Roots) == 0 {
		fmt.Fprintln(command.Stdout, "暂无会话")
		return ExitOK
	}
	for _, root := range tree.Roots {
		writeSessionTreeNode(command.Stdout, tree, root, "", map[domain.RunID]bool{})
	}
	return ExitOK
}

// writeSessionTreeNode 递归打印会话树节点；visited 防止损坏图导致无限递归。
func writeSessionTreeNode(out io.Writer, tree app.SessionTree, id domain.RunID, prefix string, visited map[domain.RunID]bool) {
	if visited[id] {
		fmt.Fprintf(out, "%s%s …（环）\n", prefix, id)
		return
	}
	visited[id] = true
	node := tree.Nodes[id]
	marker := ""
	if node.ForkSource != nil {
		marker = fmt.Sprintf(" [fork from %s]", node.ForkSource.RunID)
	}
	fmt.Fprintf(out, "%s%s · %s · %s%s\n", prefix, node.ID, node.Phase, firstLineClip(node.Task, 40), marker)
	for _, child := range node.Children {
		writeSessionTreeNode(out, tree, child, prefix+"  ", visited)
	}
}

// sessionFork 从历史终态 run 创建新会话分支；--from 必填，任务为位置参数。
// 首次新任务才创建分支，预览用 session tree/show，不留下幽灵运行。
func (command Command) sessionFork(ctx context.Context, arguments []string) int {
	return command.sessionRun(ctx, "session fork", arguments, true)
}

// sessionContinue 从明确选定的唯一链尾续链；无 --from 且无任务时列出可续链尾。
func (command Command) sessionContinue(ctx context.Context, arguments []string) int {
	return command.sessionRun(ctx, "session continue", arguments, false)
}

// sessionRun 执行 fork/continue 共用的参数装配与运行入口。
func (command Command) sessionRun(ctx context.Context, label string, arguments []string, fork bool) int {
	flags := flag.NewFlagSet(label, flag.ContinueOnError)
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
	fromRun := flags.String("from", "", "source run ID")
	jsonOutput := flags.Bool("json", false, "write NDJSON to stdout")
	if err := flags.Parse(normalizeFlagsThenPositionals(arguments, map[string]struct{}{
		"--workspace": {}, "--state-dir": {}, "--config": {}, "--base-url": {}, "--model": {},
		"--api-key-env": {}, "--max-turns": {}, "--max-tool-calls": {}, "--max-input-tokens": {},
		"--max-output-tokens": {}, "--max-wall-time": {}, "--max-cost-usd": {}, "--from": {},
	})); err != nil {
		return ExitUsage
	}
	// 参数形状先于配置解析：usage 错误不应依赖本机配置是否就绪。
	if flags.NArg() == 0 {
		if !fork && *fromRun == "" {
			configuration, err := command.loadConfig(*workspace, *configPath, config.Overrides{})
			if err != nil {
				fmt.Fprintln(command.Stderr, "configuration error:", err)
				return ExitUsage
			}
			return command.sessionListContinuable(ctx, *workspace, *stateDir, configuration, *jsonOutput)
		}
		fmt.Fprintf(command.Stderr, "%s requires a task argument\n", label)
		return ExitUsage
	}
	if flags.NArg() != 1 || strings.TrimSpace(flags.Arg(0)) == "" {
		fmt.Fprintf(command.Stderr, "%s requires exactly one task argument\n", label)
		return ExitUsage
	}
	if *fromRun == "" {
		fmt.Fprintf(command.Stderr, "%s requires --from <run-id>\n", label)
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
	options := app.RunOptions{
		Task: flags.Arg(0), Workspace: *workspace, StateDir: *stateDir, Config: configuration,
	}
	if fork {
		anchor, resolveErr := command.Service.ResolveSessionFork(ctx, *workspace, *stateDir, configuration, domain.RunID(*fromRun))
		if resolveErr != nil {
			fmt.Fprintln(command.Stderr, "session fork failed:", resolveErr)
			return exitFor(resolveErr)
		}
		options.ForkSource = &anchor.Source
	} else {
		options.PrevRunID = domain.RunID(*fromRun)
		options.RequireSessionTail = true
	}
	var outputErr error
	options.OnEvent = func(event domain.Event) {
		if *jsonOutput && outputErr == nil {
			outputErr = writeJSONChecked(command.Stdout, struct {
				Type  string       `json:"type"`
				Event domain.Event `json:"event"`
			}{Type: "event", Event: event})
		}
	}
	result, runErr := command.Service.Run(ctx, options)
	if outputErr != nil {
		markOutputErrorReported(command.Stdout)
		fmt.Fprintln(command.Stderr, label+" output failed:", outputErr)
		return ExitFailed
	}
	if runErr != nil {
		fmt.Fprintln(command.Stderr, label+" failed:", runErr)
		return exitFor(runErr)
	}
	if *jsonOutput {
		return command.writeJSONOutput(label, struct {
			Type     string                `json:"type"`
			RunID    domain.RunID          `json:"run_id"`
			Response string                `json:"response"`
			Manifest *contract.RunManifest `json:"manifest,omitempty"`
		}{Type: "result", RunID: result.ID, Response: result.Response, Manifest: sessionManifestOf(result)})
	}
	fmt.Fprintln(command.Stdout, result.Response)
	return ExitOK
}

// sessionManifestOf 从运行结果提取产品合同 manifest；与 run 命令语义一致。
func sessionManifestOf(result app.RunResult) *contract.RunManifest {
	if result.Result == nil {
		return nil
	}
	return &result.Result.Manifest
}

// sessionListContinuable 列出当前工作区可续链的唯一链尾，供 continue 无参调用。
func (command Command) sessionListContinuable(ctx context.Context, workspace, stateDir string, configuration config.Config, jsonOutput bool) int {
	tree, err := command.Service.ListSessionTree(ctx, workspace, stateDir, configuration)
	if err != nil {
		fmt.Fprintln(command.Stderr, "session continue failed:", err)
		return exitFor(err)
	}
	type tailItem struct {
		RunID     domain.RunID    `json:"run_id"`
		SessionID string          `json:"session_id"`
		Task      string          `json:"task"`
		State     domain.RunState `json:"state"`
		Forked    bool            `json:"forked"`
	}
	tails := make([]tailItem, 0)
	childrenOf := make(map[domain.RunID]int)
	for _, node := range tree.Nodes {
		if node.PrevRunID != "" {
			childrenOf[node.PrevRunID]++
		}
	}
	for _, root := range tree.Roots {
		// 沿唯一子链走到链尾；分叉节点本身不可自动续链。visited 防止损坏
		// 日志成环时死循环，环上节点按不可续链处理。
		current := root
		visited := map[domain.RunID]bool{}
		for childrenOf[current] == 1 {
			if visited[current] {
				current = ""
				break
			}
			visited[current] = true
			node := tree.Nodes[current]
			if len(node.Children) != 1 {
				break
			}
			current = node.Children[0]
		}
		if current == "" {
			continue
		}
		if childrenOf[current] != 0 || visited[current] {
			continue
		}
		node := tree.Nodes[current]
		if !(node.State == domain.RunCompleted || node.State == domain.RunFailed || node.State == domain.RunCancelled) {
			continue
		}
		tails = append(tails, tailItem{RunID: node.ID, SessionID: node.SessionID, Task: node.Task, State: node.State, Forked: node.ForkSource != nil})
	}
	for _, tail := range tails {
		if jsonOutput {
			if writeJSONChecked(command.Stdout, tail) != nil {
				return ExitFailed
			}
			continue
		}
		fmt.Fprintf(command.Stdout, "%s · 会话 %s · %s · %s\n", tail.RunID, tail.SessionID, tail.State, firstLineClip(tail.Task, 48))
	}
	if len(tails) == 0 && !jsonOutput {
		fmt.Fprintln(command.Stdout, "暂无可续链的会话链尾")
	}
	return ExitOK
}

// firstLineClip 截取首行并限长，用于人类视图的任务摘要。
func firstLineClip(text string, limit int) string {
	line, _, _ := strings.Cut(text, "\n")
	line = strings.TrimSpace(line)
	runes := []rune(line)
	if len(runes) <= limit {
		return line
	}
	return string(runes[:limit]) + "…"
}
