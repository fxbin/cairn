// slash_context_doctor.go 实现 TUI 通用命令 /clear /context /doctor。
// /clear 仅清屏保留会话链与统计；/context 投影上下文预算配置；/doctor
// 调用 app.Doctor 只读诊断并展示非敏感摘要。三者不写 Journal、不授予权限。
// Author: fxbin
package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

// appendContextBudget 投影上下文预算与压缩策略（只读配置，不触发压缩）。
func (console *RunConsole) appendContextBudget() {
	cfg := console.input.Config
	console.appendLine(itemStyle.Render("上下文预算"))
	console.appendLine(dimStyle.Render(fmt.Sprintf("  auto_compact=%v · max_prompt_tokens=%d · max_summary_bytes=%d",
		cfg.Context.AutoCompact, cfg.Context.MaxPromptTokens, cfg.Context.MaxSummaryBytes)))
	console.appendLine(dimStyle.Render(fmt.Sprintf("  inventory_budget_percent=%d · threshold_reserve_percent=%d",
		cfg.Context.InventoryBudgetPercent, cfg.Context.ThresholdReservePercent)))
	console.appendLine(dimStyle.Render("  策略 deterministic_journal_checkpoint_v4（压缩只写检查点，历史事件不删）"))
	if budget := console.maxTurnsBudget(); budget > 0 {
		console.appendLine(dimStyle.Render(fmt.Sprintf("  轮次预算 %d · 当前轮次 %d · 估算 in %d tok", budget, console.turn, console.promptTokens)))
	}
}

// doctorDoneMsg 承载只读诊断摘要行。
type doctorDoneMsg struct {
	lines []string
}

// doctorCmd 异步执行工作区只读诊断；失败也只回写错误摘要。
func (console *RunConsole) doctorCmd() tea.Cmd {
	input := *console
	return func() tea.Msg {
		root := input.input.Workspace
		if strings.TrimSpace(root) == "" {
			return doctorDoneMsg{lines: []string{"无法解析工作区目录"}}
		}
		report, err := input.input.Service.Doctor(input.rootCtx, root, input.input.StateDir, input.input.Config)
		if err != nil {
			return doctorDoneMsg{lines: []string{"诊断失败: " + err.Error()}}
		}
		return doctorDoneMsg{lines: doctorSummaryLines(report)}
	}
}

// doctorSummaryLines 把 DoctorReport 压成少量非敏感展示行。
// appendSafetyEvents 投影最近安全事件摘要（blocked/prompted/hooked/sandboxed）。
func (console *RunConsole) appendSafetyEvents() {
	summary := console.safetySummary()
	if summary == "" {
		console.appendLine(dimStyle.Render("安全事件：暂无拦截记录"))
		return
	}
	console.appendLine(itemStyle.Render("安全事件（最近）"))
	console.appendLine(dimStyle.Render("  " + summary))
}

// safetySummary 从 transcript 中统计工具失败/审批次数，作为安全事件代理。
func (console RunConsole) safetySummary() string {
	blocked, prompted := 0, 0
	for _, event := range console.transcript {
		switch event.Type {
		case domain.EventOperationFailed:
			blocked++
		case domain.EventApprovalRequested:
			prompted++
		}
	}
	if blocked == 0 && prompted == 0 {
		return ""
	}
	return fmt.Sprintf("failed=%d · approval_req=%d（详细以 policy.SafetyLog 为准）", blocked, prompted)
}

// appendSecurityPolicy 投影危险命令与信任状态。
// appendHeartbeat 投影 Provider 心跳与重试状态。
func (console *RunConsole) appendHeartbeat() {
	console.appendLine(itemStyle.Render("Provider 心跳"))
	console.appendLine(dimStyle.Render("  探测: 按需触发（web_fetch/web_search 可用时显示延迟）"))
	console.appendLine(dimStyle.Render("  重试: 指数退避 + 抖动（max 3 次 · base 100ms · max 5s）"))
	console.appendLine(dimStyle.Render("  幂等: 仅只读/幂等工具自动重试"))
}

func (console *RunConsole) appendSecurityPolicy() {
	root := console.input.Workspace
	console.appendLine(itemStyle.Render("安全策略"))
	console.appendLine(dimStyle.Render("  危险命令: 任何模式下拒绝（rm -rf / · mkfs · dd · shutdown · fork 炸弹）"))
	if root == "" {
		console.appendLine(dimStyle.Render("  工作区信任: 未知（未绑定工作区）"))
		return
	}
	if tools.IsExplicitlyUntrusted(root) {
		console.appendLine(warnStyle.Render("  工作区信任: 显式拒绝（.cairn-untrusted 存在）"))
	} else if tools.IsTrusted(root) {
		console.appendLine(dimStyle.Render("  工作区信任: 显式信任（.cairn-trusted）"))
	} else {
		console.appendLine(dimStyle.Render("  工作区信任: 默认信任（无显式标记）"))
	}
}

func doctorSummaryLines(report app.DoctorReport) []string {
	ready := "否"
	if report.Ready {
		ready = "是"
	}
	lines := []string{
		itemStyle.Render("环境诊断（只读）"),
		dimStyle.Render(fmt.Sprintf("  就绪=%s · 配置有效=%v · preset=%s · profile=%s", ready, report.ConfigValid, report.Preset, report.Profile)),
		dimStyle.Render(fmt.Sprintf("  策略 write=%s exec=%s · 工具 %d 个 · 验证命令 %d",
			report.Policy.Write, report.Policy.Exec, len(report.Tools), report.VerificationCount)),
		dimStyle.Render(fmt.Sprintf("  Journal ok=%v(%s) · 制品 ok=%v · 记忆 ok=%v(%s)",
			report.JournalOK, report.JournalState, report.ArtifactsOK, report.MemoryOK, report.MemoryState)),
		dimStyle.Render(fmt.Sprintf("  提供方 profile=%s · admission=%v", report.Profile, report.Admission)),
	}
	if report.BuildError != "" {
		lines = append(lines, warnStyle.Render("  构建身份异常: "+report.BuildError))
	}
	return lines
}
