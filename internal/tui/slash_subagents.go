// slash_subagents.go 实现 TUI 命令 /subagents：投影当前会话的父子运行
// 状态、消息与预算摘要；只读展示，不启动子代理。
// Author: fxbin
package tui

import (
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
)

// appendSubagents 展示父子运行关系与进度。
func (console *RunConsole) appendSubagents() {
	if len(console.subagentViews) == 0 {
		console.appendLine(dimStyle.Render("暂无子运行（父任务可通过委派产生子代理）"))
		return
	}
	console.appendLine(itemStyle.Render(fmt.Sprintf("子运行 %d 个", len(console.subagentViews))))
	for _, view := range console.subagentViews {
		console.appendLine(dimStyle.Render(fmt.Sprintf("  %s · %s · %s", shortRun(domain.RunID(view.ChildRunID)), view.State, firstLine(view.Task, 40))))
		if view.Outcome != "" {
			console.appendLine(dimStyle.Render(fmt.Sprintf("    outcome=%s · 工具 %d · in %d tok", view.Outcome, view.ToolCalls, view.Tokens)))
		}
		if view.PendingApproval {
			console.appendLine(warnStyle.Render("    ⏸ 待审批"))
		}
	}
	// 预算汇总：各子运行已用与委派上限。
	totalTokens := int64(0)
	totalTools := 0
	for _, view := range console.subagentViews {
		totalTokens += view.Tokens
		totalTools += view.ToolCalls
	}
	console.appendLine(itemStyle.Render(fmt.Sprintf("预算汇总：子运行 %d · 工具 %d · token %d", len(console.subagentViews), totalTools, totalTokens)))
	if budget := console.input.Config.Budget; budget.MaxTurns > 0 || budget.MaxInputTokens > 0 {
		console.appendLine(dimStyle.Render(fmt.Sprintf("  父预算：max_turns=%d · max_input=%d · max_output=%d",
			budget.MaxTurns, budget.MaxInputTokens, budget.MaxOutputTokens)))
	}
	console.appendLine(dimStyle.Render("  详细计划用 /cost · /status"))
}

// appendSubagentDetail 展示单个子运行详情。
func (console *RunConsole) appendSubagentDetail(argument string) {
	id := strings.TrimSpace(argument)
	if id == "" {
		console.appendLine(warnStyle.Render("用法: /subagents <child-run-id>"))
		return
	}
	for _, view := range console.subagentViews {
		if !strings.HasPrefix(string(view.ChildRunID), id) && string(view.ChildRunID) != id {
			continue
		}
		console.appendLine(itemStyle.Render("子运行详情"))
		console.appendLine(dimStyle.Render(fmt.Sprintf("  id=%s parent=%s", view.ChildRunID, view.ParentRunID)))
		console.appendLine(dimStyle.Render(fmt.Sprintf("  state=%s outcome=%s", view.State, view.Outcome)))
		console.appendLine(dimStyle.Render(fmt.Sprintf("  task=%s", view.Task)))
		console.appendLine(dimStyle.Render(fmt.Sprintf("  tools=%d tokens=%d approval=%v", view.ToolCalls, view.Tokens, view.PendingApproval)))
		return
	}
	console.appendLine(warnStyle.Render("未找到子运行 " + id))
}
