// slash_usage.go 实现 TUI 命令 /usage：展示用量告警并给出可一键应用的
// 修复建议；/usage fix 应用安全修复（不改 Policy/网络工具）。
// Author: fxbin
package tui

import (
	"fmt"

	"github.com/fxbin/cairn/internal/agent"
)

// UsageWarning 是一条用量告警及其修复动作。
type UsageWarning struct {
	Message string
	FixKey  string
	FixDesc string
}

// usageWarnings 根据当前状态生成告警与修复建议。
func (console RunConsole) usageWarnings() []UsageWarning {
	var warnings []UsageWarning
	limit := console.input.Config.Context.MaxPromptTokens
	if limit > 0 {
		percent := int(console.promptTokens) * 100 / limit
		if percent >= 70 {
			warnings = append(warnings, UsageWarning{
				Message: fmt.Sprintf("上下文已用 %d%%", percent),
				FixKey:  "auto_compact",
				FixDesc: "开启自动压缩 context.auto_compact=true",
			})
		}
	}
	if !console.input.Config.Context.AutoCompact {
		warnings = append(warnings, UsageWarning{
			Message: "自动压缩未开启，长对话可能触发预算上限",
			FixKey:  "auto_compact",
			FixDesc: "开启自动压缩 context.auto_compact=true",
		})
	}
	if console.input.Config.Budget.MaxTurns > 0 && console.turn >= console.input.Config.Budget.MaxTurns {
		warnings = append(warnings, UsageWarning{
			Message: fmt.Sprintf("轮次已达上限 %d", console.input.Config.Budget.MaxTurns),
			FixKey:  "raise_turns",
			FixDesc: "提高 budget.max_turns（/config set budget.max_turns <N>）",
		})
	}
	return warnings
}

// appendUsage 展示用量告警与修复建议。
func (console *RunConsole) appendUsage() {
	warnings := console.usageWarnings()
	if len(warnings) == 0 {
		console.appendLine(okStyle.Render("用量正常，无告警"))
		console.appendLine(dimStyle.Render(fmt.Sprintf("  token in %d / out %d · 轮次 %d", console.promptTokens, console.outputTokens, console.turn)))
		return
	}
	console.appendLine(itemStyle.Render(fmt.Sprintf("用量告警 %d 条", len(warnings))))
	for index, warning := range warnings {
		console.appendLine(warnStyle.Render(fmt.Sprintf("  %d. %s", index+1, warning.Message)))
		console.appendLine(dimStyle.Render(fmt.Sprintf("     修复[%s]: %s", warning.FixKey, warning.FixDesc)))
	}
	console.appendLine(dimStyle.Render("  输入 /usage fix 自动应用安全修复"))
}

// applyUsageFix 应用安全修复（仅 auto_compact 等无风险项）。
func (console *RunConsole) applyUsageFix() {
	if !console.input.Config.Context.AutoCompact {
		console.input.Config.Context.AutoCompact = true
		console.appendLine(okStyle.Render("已开启 auto_compact（热内存）"))
	}
	console.appendLine(dimStyle.Render("已应用全部安全修复"))
}

// appendUsagePhases 展示活跃/等待用户阶段时长。
func (console *RunConsole) appendUsagePhases() {
	if console.phaseTracker == nil {
		console.appendLine(dimStyle.Render("阶段遥测：未启用"))
		return
	}
	totals := console.phaseTracker.TotalByPhase()
	active := totals[agent.UIPhaseActive]
	prompt := totals[agent.UIPhasePrompt]
	console.appendLine(itemStyle.Render("阶段时长"))
	console.appendLine(dimStyle.Render(fmt.Sprintf("  活跃 %s · 等待用户 %s", active, prompt)))
}
