// slash_config.go 实现 TUI 命令 /config：只读展示当前生效配置摘要
// （模型、协议、预算、上下文、只读模式）；不写配置文件。
// Author: fxbin
package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

// appendConfig 展示当前生效配置摘要。
func (console *RunConsole) appendConfig() {
	cfg := console.input.Config
	console.appendLine(itemStyle.Render("当前配置（只读）"))
	console.appendLine(dimStyle.Render(fmt.Sprintf("  model: %s · base_url: %s",
		cfg.Provider.Model, cfg.Provider.BaseURL)))
	console.appendLine(dimStyle.Render(fmt.Sprintf("  budget: max_turns=%d · max_input=%d · max_output=%d",
		cfg.Budget.MaxTurns, cfg.Budget.MaxInputTokens, cfg.Budget.MaxOutputTokens)))
	console.appendLine(dimStyle.Render(fmt.Sprintf("  context: auto_compact=%v · max_prompt_tokens=%d · max_summary_bytes=%d",
		cfg.Context.AutoCompact, cfg.Context.MaxPromptTokens, cfg.Context.MaxSummaryBytes)))
	console.appendLine(dimStyle.Render(fmt.Sprintf("  policy: write=%s · exec=%s",
		cfg.Policy.Write, cfg.Policy.Exec)))
	if console.input.Config.Provider.Capability != nil {
		cap := console.input.Config.Provider.Capability
		console.appendLine(dimStyle.Render(fmt.Sprintf("  capability: tool_calls=%v · streaming_usage=%v",
			cap.ToolCalls, cap.StreamingUsage)))
	}
	console.appendLine(dimStyle.Render("  修改请用 /model 或 cairn provider configure"))
}

// appendBatchPlan 展示最近一次工具批次的并行计划诊断。
func (console *RunConsole) appendBatchPlan() {
	if console.lastBatchPlan == nil || len(console.lastBatchPlan.Entries) == 0 {
		console.appendLine(dimStyle.Render("批次计划：暂无（最近一次模型响应未产生工具调用）"))
		return
	}
	plan := *console.lastBatchPlan
	console.appendLine(itemStyle.Render(fmt.Sprintf("批次计划：%d 组 · %d 调用 · 可并行=%v", plan.Groups, len(plan.Entries), plan.Parallelizable)))
	for _, entry := range plan.Entries {
		marker := "串行"
		if entry.Parallel {
			marker = "并行"
		}
		console.appendLine(dimStyle.Render(fmt.Sprintf("  [%s] g%d %s (%s)", marker, entry.Group, entry.ToolName, entry.CallID)))
	}
	console.appendLine(dimStyle.Render("  执行仍按 Journal 单写入者顺序，本行仅为诊断"))
}

// configSettableFields 是允许 /config set 修改的白名单字段。
var configSettableFields = map[string]bool{
	"budget.max_turns":          true,
	"budget.max_input_tokens":   true,
	"budget.max_output_tokens":  true,
	"context.auto_compact":      true,
	"context.max_prompt_tokens": true,
	"context.max_summary_bytes": true,
}

// appendConfigSet 按白名单修改配置字段；不信任工作区拒绝写。
func (console *RunConsole) appendConfigSet(argument string) {
	if err := tools.GuardTrustedWrite(console.input.Workspace, "config write"); err != nil {
		console.appendLine(warnStyle.Render(err.Error()))
		return
	}
	parts := strings.Fields(argument)
	if len(parts) != 2 || !configSettableFields[parts[0]] {
		console.appendLine(warnStyle.Render("用法: /config set <字段> <值>"))
		console.appendLine(dimStyle.Render("白名单: budget.max_turns · budget.max_input_tokens · budget.max_output_tokens · context.auto_compact · context.max_prompt_tokens · context.max_summary_bytes"))
		return
	}
	field, value := parts[0], parts[1]
	switch field {
	case "budget.max_turns":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			console.input.Config.Budget.MaxTurns = n
		} else {
			console.appendLine(warnStyle.Render("值必须为正整数"))
			return
		}
	case "budget.max_input_tokens":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			console.input.Config.Budget.MaxInputTokens = n
		} else {
			console.appendLine(warnStyle.Render("值必须为正整数"))
			return
		}
	case "budget.max_output_tokens":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			console.input.Config.Budget.MaxOutputTokens = n
		} else {
			console.appendLine(warnStyle.Render("值必须为正整数"))
			return
		}
	case "context.auto_compact":
		console.input.Config.Context.AutoCompact = value == "true" || value == "1"
	case "context.max_prompt_tokens":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			console.input.Config.Context.MaxPromptTokens = n
		} else {
			console.appendLine(warnStyle.Render("值必须为正整数"))
			return
		}
	case "context.max_summary_bytes":
		if n, err := strconv.Atoi(value); err == nil && n > 0 {
			console.input.Config.Context.MaxSummaryBytes = n
		} else {
			console.appendLine(warnStyle.Render("值必须为正整数"))
			return
		}
	}
	console.appendLine(okStyle.Render(fmt.Sprintf("已设置 %s = %s（热内存，下次运行生效）", field, value)))
}
