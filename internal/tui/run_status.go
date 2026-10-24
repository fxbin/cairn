// 状态栏、告警与摘要行。
// Author: fxbin
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/config"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/tools"
)

func (console RunConsole) referenceWorkspace() tools.Workspace {
	workspace, err := workspaceForRoot(workspaceRootOrDefault(console.input.Workspace))
	if err != nil {
		return tools.Workspace{}
	}
	return workspace
}

// nextSetupInput 构造向导输入：写入目标跟随当前实际生效的配置位置（用
// 户级存在即用户级，否则工作区），避免改配置改到未生效的那份上。
func (console RunConsole) nextSetupInput() app.SetupInput {
	workspace := console.input.Workspace
	if workspace == "" {
		if current, err := os.Getwd(); err == nil {
			workspace = current
		}
	}
	input := app.SetupInput{
		Workspace: workspace, CatalogURL: "", CatalogCacheDir: "",
		Admission: console.input.Config.Provider.Admission,
	}
	if userPath, err := config.UserConfigPath(); err == nil {
		if resolved := config.ResolveDefaultPath(workspace); resolved == userPath {
			input.ScopeUserConfig = true
		}
	}
	return input
}

// editorClosedMsg 承载外部编辑器收束后的输入内容与执行错误。
type editorClosedMsg struct {
	content string
	err     error
}

// openEditorCmd 把当前输入送外部编辑器（VISUAL 优先于 EDITOR）：临时文件
// 写入、挂起界面执行编辑、恢复后读回内容并清理临时文件。未配置编辑器时
// 就地提示。
func (console RunConsole) openEditorCmd() tea.Cmd {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		console.appendLine(warnStyle.Render("未配置 VISUAL/EDITOR 环境变量，无法打开外部编辑器"))
		return nil
	}
	temporary, err := os.CreateTemp("", "cairn-task-*.md")
	if err != nil {
		console.appendLine(warnStyle.Render("创建编辑临时文件失败: " + err.Error()))
		return nil
	}
	path := temporary.Name()
	if _, err := temporary.WriteString(console.taskField.Value()); err != nil {
		temporary.Close()
		os.Remove(path)
		console.appendLine(warnStyle.Render("写入编辑临时文件失败: " + err.Error()))
		return nil
	}
	temporary.Close()
	return tea.ExecProcess(exec.Command(editor, path), func(err error) tea.Msg {
		content, readErr := os.ReadFile(path)
		os.Remove(path)
		if readErr != nil {
			return editorClosedMsg{err: readErr}
		}
		return editorClosedMsg{content: string(content), err: err}
	})
}

// firstLineClip 取文本首行并按字符截断，作为链节点的任务摘要。
func firstLineClip(text string, limit int) string {
	first := strings.SplitN(strings.TrimSpace(text), "\n", 2)[0]
	runes := []rune(first)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return first
}

// statusWorkspace 返回展示用工作区标识；空工作区显示当前目录语义。
func (console RunConsole) statusWorkspace() string {
	if console.input.Workspace == "" {
		return "当前目录"
	}
	return console.input.Workspace
}

// statusLine 生成运行中的统计行：轮次、工具调用、累计 token 与上下文水位。
func (console RunConsole) statusLine() string {
	turns := fmt.Sprintf("轮次 %d", console.turn)
	if budget := console.maxTurnsBudget(); budget > 0 {
		turns = fmt.Sprintf("轮次 %d/%d", console.turn, budget)
	}
	hint := "Esc 取消 · 输入即排队 · Ctrl+O 事件"
	line := fmt.Sprintf("%s · 工具调用 %d · 累计 in %d / out %d tok", turns, console.toolCalls, console.promptTokens, console.outputTokens)
	if watermark := console.contextWatermark(); watermark != "" {
		line += " · " + watermark
	}
	return dimStyle.Render(line + " · " + hint)
}

// appendContextAlerts 在事件流中插入上下文告警行。
func (console *RunConsole) appendContextAlerts() {
	for _, alert := range console.contextAlerts() {
		console.appendLine(alert)
	}
}

// contextAlerts 返回当前上下文用量触发的告警行（无则空）。
func (console RunConsole) contextAlerts() []string {
	limit := console.input.Config.Context.MaxPromptTokens
	if limit <= 0 {
		return nil
	}
	used := int(console.promptTokens)
	percent := used * 100 / limit
	switch {
	case percent >= 90:
		return []string{warnStyle.Render(fmt.Sprintf("⚠ 上下文已用 %d%%，即将触发自动压缩或预算上限", percent))}
	case percent >= 70:
		return []string{warnStyle.Render(fmt.Sprintf("! 上下文已用 %d%%，建议 /compact 查看检查点", percent))}
	}
	return nil
}

// contextWatermark 按 max_prompt_tokens 计算上下文用量百分比水位。
func (console RunConsole) contextWatermark() string {
	limit := console.input.Config.Context.MaxPromptTokens
	if limit <= 0 {
		return ""
	}
	used := int(console.promptTokens)
	percent := used * 100 / limit
	marker := ""
	switch {
	case percent >= 90:
		marker = "‼"
	case percent >= 70:
		marker = "!"
	}
	return fmt.Sprintf("上下文 %d%% %s", percent, marker)
}

// turnSummaryLine 生成回合收束执行摘要：结果、工具次数、token 与墙钟耗时。
// 耗时取本回合 turn_opened→turn_closed 的事件 OccurredAt 差值，缺失时为 0，
// 不推断未持久化的时间。
func (console RunConsole) turnSummaryLine(event domain.Event) string {
	var payload struct {
		Outcome string `json:"outcome"`
	}
	eventPayload(event, &payload)
	outcome := payload.Outcome
	if outcome == "" {
		outcome = "unknown"
	}
	duration := time.Duration(0)
	if !console.turnOpenedAt.IsZero() && !event.OccurredAt.IsZero() && !event.OccurredAt.Before(console.turnOpenedAt) {
		duration = event.OccurredAt.Sub(console.turnOpenedAt)
	}
	return dimStyle.Render(fmt.Sprintf("▸ 第 %d 轮收束(%s) · 工具 %d（提议）/ 执行 %d · in %d / out %d tok · %s",
		console.turn, outcome, console.turnToolCalls, console.turnExecutedTools, console.turnPromptTokens, console.turnOutputTokens,
		formatDuration(duration)))
}

// formatDuration 把回合耗时格式化为秒级可读文本，避免引入完整时钟依赖。
func formatDuration(duration time.Duration) string {
	milliseconds := duration.Milliseconds()
	if milliseconds < 1000 {
		return fmt.Sprintf("%dms", milliseconds)
	}
	return fmt.Sprintf("%.1fs", float64(milliseconds)/1000)
}

// finalResponsePreview 截取最终回复的首段作为终态预览，完整回复以运行结
// 果与事件日志为准。
func finalResponsePreview(response string) string {
	firstLine := strings.SplitN(strings.TrimSpace(response), "\n", 2)[0]
	runes := []rune(firstLine)
	const previewLimit = 120
	if len(runes) > previewLimit {
		firstLine = string(runes[:previewLimit]) + "…"
	}
	return "回复: " + firstLine
}

// RunConsoleLoop 以终端模式运行单任务主界面并返回进程级结论；非 TTY 环境
// 由调用方先行拦截。
