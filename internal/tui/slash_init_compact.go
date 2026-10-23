// slash_init_compact.go 实现 TUI 通用命令 /init 与 /compact：
// /init 在工作区生成或更新 AGENTS.md 骨架；/compact 展示上下文用量与最近压缩检查点。
// 两者只做本地文件写入或只读投影，不触碰 Journal、不授予执行权限。
//
// Author: fxbin
package tui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fxbin/cairn/internal/domain"
)

// defaultAgentsSkeleton 是 /init 写入的最小项目规约骨架，调用方可在其上补充。
const defaultAgentsSkeleton = `# 项目规约

## 开始工作
- 使用中文沟通与提交说明。
- 先检查 git status 与相关测试，再动手修改。

## 边界
- 不提交密钥与用户隐私数据。
- 未经明确授权不 push、不改写历史。

## 导航
- 在此补充构建、测试命令与目录职责。
`

// initProjectInstructions 在工作区根写入 AGENTS.md 骨架。
// 已存在时需 --force 覆盖；路径必须落在工作区内，失败关闭。
func (console *RunConsole) initProjectInstructions(argument string) (bool, tea.Cmd) {
	root := strings.TrimSpace(console.input.Workspace)
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if root == "" {
		console.appendLine(warnStyle.Render("无法解析工作区目录，/init 未执行"))
		return true, nil
	}
	target := filepath.Join(root, "AGENTS.md")
	force := strings.TrimSpace(argument) == "--force"
	if _, err := os.Lstat(target); err == nil && !force {
		console.appendLine(warnStyle.Render("AGENTS.md 已存在；如需覆盖请使用 /init --force"))
		return true, nil
	}
	if err := os.WriteFile(target, []byte(defaultAgentsSkeleton), 0o644); err != nil {
		console.appendLine(warnStyle.Render("写入 AGENTS.md 失败: " + err.Error()))
		return true, nil
	}
	console.appendLine(okStyle.Render("已写入 " + target + "（AGENTS.md 骨架，可继续编辑）"))
	return true, nil
}

// appendCompactStatus 投影当前上下文用量与最近一次压缩检查点摘要。
// 数据只来自 RunConsole 已累计统计与已持久化事件，不触发真实压缩。
func (console *RunConsole) appendCompactStatus() {
	console.appendLine(itemStyle.Render(fmt.Sprintf(
		"上下文用量：本任务 in %d / out %d tok · 会话累计 in %d / out %d tok",
		console.promptTokens, console.outputTokens,
		console.sessionPromptTokens, console.sessionOutputTokens,
	)))
	if budget := console.maxTurnsBudget(); budget > 0 {
		console.appendLine(dimStyle.Render(fmt.Sprintf("轮次预算 %d · 工具调用 %d · 轮次 %d", budget, console.toolCalls, console.turn)))
	} else {
		console.appendLine(dimStyle.Render(fmt.Sprintf("工具调用 %d · 轮次 %d", console.toolCalls, console.turn)))
	}
	var latest *domain.Event
	for i := len(console.transcript) - 1; i >= 0; i-- {
		if console.transcript[i].Type == domain.EventContextCompacted {
			latest = &console.transcript[i]
			break
		}
	}
	if latest == nil {
		console.appendLine(dimStyle.Render("压缩检查点：本次会话尚无（达到阈值时自动压缩）"))
		return
	}
	var payload struct {
		ThroughSeq            uint64 `json:"through_seq"`
		EstimatedTokensBefore int    `json:"estimated_units_before"`
		EstimatedTokensAfter  int    `json:"estimated_units_after"`
		Strategy              string `json:"strategy"`
		Trigger               string `json:"trigger"`
	}
	if err := json.Unmarshal(latest.Payload, &payload); err != nil {
		console.appendLine(dimStyle.Render("压缩检查点：已存在但载荷不可解析"))
		return
	}
	trigger := payload.Trigger
	if trigger == "" {
		trigger = "unknown"
	}
	console.appendLine(dimStyle.Render(fmt.Sprintf(
		"压缩检查点：through_seq=%d · 估算 %d → %d · strategy=%s · trigger=%s",
		payload.ThroughSeq, payload.EstimatedTokensBefore, payload.EstimatedTokensAfter, payload.Strategy, trigger,
	)))
}

// initProjectInstructionsLines 兼容命令层调用：委托 initProjectInstructions。
func (console *RunConsole) initProjectInstructionsLines(argument string) {
	console.initProjectInstructions(argument)
}
