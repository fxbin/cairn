// 事件摘要、状态文案与 View 渲染。
// Author: fxbin
package tui

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/domain"
)

func proposalLines(toolName string, arguments json.RawMessage) []string {
	const previewLines = 8
	const previewWidth = 72
	switch toolName {
	case "apply_patch":
		var patch struct {
			Path         string `json:"path"`
			Mode         string `json:"mode"`
			ExpectedSHA  string `json:"expected_sha256"`
			Content      string `json:"content"`
			Replacements []struct {
				Old string `json:"old"`
				New string `json:"new"`
			} `json:"replacements"`
		}
		if !jsonPiece(arguments, &patch) {
			return []string{dimStyle.Render("（apply_patch 参数不可解码）")}
		}
		lines := []string{itemStyle.Render(fmt.Sprintf("  apply_patch %s · %s · 基线 %s", patch.Mode, patch.Path, shortDigest(patch.ExpectedSHA)))}
		if patch.Content != "" {
			lines = append(lines, dimStyle.Render("  将写入内容预览:"))
			lines = append(lines, previewText(patch.Content, previewLines, previewWidth)...)
		}
		for _, replacement := range patch.Replacements {
			lines = append(lines, dimStyle.Render(fmt.Sprintf("  替换: %s → %s", clip(replacement.Old, previewWidth), clip(replacement.New, previewWidth))))
		}
		return lines
	case "exec_command":
		var command struct {
			Argv []string `json:"argv"`
		}
		if !jsonPiece(arguments, &command) {
			return []string{dimStyle.Render("（exec_command 参数不可解码）")}
		}
		return []string{itemStyle.Render("  exec_command argv: " + strings.Join(command.Argv, " "))}
	default:
		if len(arguments) == 0 {
			return nil
		}
		return []string{itemStyle.Render("  " + clip(string(arguments), previewWidth))}
	}
}

// jsonPiece 解码提议参数片段；空参数返回 false。
func jsonPiece(raw json.RawMessage, target any) bool {
	if len(raw) == 0 {
		return false
	}
	return json.Unmarshal(raw, target) == nil
}

// previewText 把多行文本投影为带缩进的预览行并截断行数。
func previewText(text string, maxLines, width int) []string {
	rows := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(rows) > maxLines {
		rows = append(rows[:maxLines], fmt.Sprintf("  …（共 %d 行）", len(strings.Split(text, "\n"))))
	}
	lines := make([]string, 0, len(rows))
	for _, row := range rows {
		lines = append(lines, dimStyle.Render("  | "+clip(row, width)))
	}
	return lines
}

// clip 截断单行文本到字符宽度。
func clip(text string, width int) string {
	runes := []rune(text)
	if len(runes) <= width {
		return text
	}
	return string(runes[:width]) + "…"
}

// shortDigest 缩短十六进制摘要便于展示。
func shortDigest(digest string) string {
	const digestPrefix = 12
	runes := []rune(digest)
	if len(runes) <= digestPrefix {
		return digest
	}
	return string(runes[:digestPrefix]) + "…"
}

// verdictText 返回不带样式的终态结论短语，供链记录使用。
func (console RunConsole) verdictText() string {
	if console.runErr != nil {
		return "失败"
	}
	if console.result == nil || console.result.Result == nil {
		return "结束（投影缺失）"
	}
	return contract.RunPhaseLabel(console.result.Result.Outcome)
}

// verdictLine 生成终态判定行：展示统一阶段词、完成证据可信等级，并在失败
// 时补充错误分类。阶段词与可信等级都取自 contract 词表，TUI 不自造第二套
// 说法。
func (console RunConsole) verdictLine() string {
	if console.runErr != nil {
		return errStyle.Render("● 运行失败: " + console.runErr.Error())
	}
	if console.result == nil || console.result.Result == nil {
		return errStyle.Render("● 运行结束（结果投影缺失）")
	}
	summary := console.result.Result
	phase := contract.RunPhaseLabel(summary.Outcome)
	trust := contract.CompletionTrustLabel(contract.CompletionTrustOf(summary.Evidence))
	verdict := okStyle.Render(fmt.Sprintf("● %s（outcome=%s，完成证据=%s", phase, summary.Outcome, trust))
	if summary.FailureClass != "" {
		verdict = errStyle.Render(fmt.Sprintf("● %s（outcome=%s，failure=%s，完成证据=%s", phase, summary.Outcome, summary.FailureClass, trust))
	}
	return verdict + "）"
}

// eventPayload 把事件 payload 解码到目标结构；解码失败返回 false，由调用
// 方按信息缺失处理而不是终止。
func eventPayload(event domain.Event, target any) bool {
	if len(event.Payload) == 0 {
		return false
	}
	return json.Unmarshal(event.Payload, target) == nil
}

// summarizeEvent 把已持久化事件投影为一行展示摘要；返回空串表示该事件静
// 默（仍计入事实流但不占屏幕）。摘要只读事件字段，不做推断。
func summarizeEvent(event domain.Event) string {
	switch event.Type {
	case domain.EventRunStarted:
		return dimStyle.Render("▶ 运行开始")
	case domain.EventTurnOpened:
		var payload struct {
			TurnIndex int `json:"turn_index"`
		}
		eventPayload(event, &payload)
		return titleStyle.Render(fmt.Sprintf("── 第 %d 轮 ──", payload.TurnIndex))
	case domain.EventToolCallProposed:
		var payload struct {
			ToolName string `json:"tool_name"`
		}
		eventPayload(event, &payload)
		return itemStyle.Render("· 工具: " + payload.ToolName)
	case domain.EventOperationSucceeded:
		var payload struct {
			DurationMs int64 `json:"duration_ms"`
		}
		eventPayload(event, &payload)
		return dimStyle.Render(fmt.Sprintf("  ✓ 完成（%dms）", payload.DurationMs))
	case domain.EventOperationFailed:
		var payload struct {
			ErrorClass string `json:"error_class"`
		}
		eventPayload(event, &payload)
		return errStyle.Render("  ✗ 工具失败: " + payload.ErrorClass)
	case domain.EventModelResponseFinished:
		var payload struct {
			StopReason string `json:"stop_reason"`
			Usage      struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		eventPayload(event, &payload)
		return dimStyle.Render(fmt.Sprintf("· 模型回复（%s · in %d / out %d tok）", payload.StopReason, payload.Usage.PromptTokens, payload.Usage.CompletionTokens))
	case domain.EventModelRequestFailed:
		var payload struct {
			ErrorClass string `json:"error_class"`
		}
		eventPayload(event, &payload)
		return errStyle.Render("⚠ 模型请求失败: " + payload.ErrorClass)
	case domain.EventApprovalRequested:
		var payload struct {
			Summary string `json:"summary"`
		}
		eventPayload(event, &payload)
		return warnStyle.Render("⏸ 审批请求: " + payload.Summary)
	case domain.EventVerificationStarted:
		return dimStyle.Render("· 验证运行…")
	case domain.EventVerificationFinished:
		var payload struct {
			OK bool `json:"ok"`
		}
		eventPayload(event, &payload)
		if payload.OK {
			return okStyle.Render("✓ 验证通过")
		}
		return errStyle.Render("✗ 验证未通过")
	case domain.EventContextCompacted:
		var payload struct {
			UnitsBefore int `json:"estimated_units_before"`
			UnitsAfter  int `json:"estimated_units_after"`
		}
		eventPayload(event, &payload)
		return dimStyle.Render(fmt.Sprintf("· 上下文压缩 %d → %d", payload.UnitsBefore, payload.UnitsAfter))
	case domain.EventRunCancelled:
		return warnStyle.Render("● 已取消")
	case domain.EventRunFailed:
		var payload struct {
			Reason string `json:"reason"`
		}
		eventPayload(event, &payload)
		return errStyle.Render("● 运行失败: " + payload.Reason)
	case domain.EventRunCompleted:
		return okStyle.Render("● 运行完成")
	default:
		return ""
	}
}

// renderTranscriptLine 把单条事件投影为查看器行：序号、类型与轮次归属，
// 只呈现结构事实不含 payload 全文。
func renderTranscriptLine(event domain.Event) string {
	return fmt.Sprintf("  %4d  %-28s turn=%d", event.Seq, string(event.Type), event.TurnID)
}

// maxTurnsBudget 从 Resolved Execution Plan 读取轮次预算；缺失或不可解析
// 返回 0 表示不展示上限。
func (console RunConsole) maxTurnsBudget() int {
	for _, entry := range console.input.Config.Resolution {
		if entry.Field == "budget.max_turns" {
			value, err := strconv.Atoi(entry.Value)
			if err == nil && value > 0 {
				return value
			}
			return 0
		}
	}
	return 0
}

// View 渲染主界面：输入态聚焦任务框，运行态滚动事件摘要与统计行，审批态
// 展示决定入口，终态展示判定与后续动作。
func (console RunConsole) View() string {
	var sections []string
	sections = append(sections, titleStyle.Render("Cairn")+dimStyle.Render("  单任务运行"))
	sections = append(sections, strings.Join(console.lines, "\n"))
	switch console.mode {
	case consoleInput:
		if len(console.lines) == 0 {
			sections = append(sections, console.welcomeBlock())
		}
		if current := strings.TrimSpace(console.taskField.Value()); strings.HasPrefix(current, "/") && !strings.HasPrefix(current, "/skill:") {
			if suggestions := commandSuggestions(current); len(suggestions) > 0 {
				sections = append(sections, "", strings.Join(suggestions, "\n"))
			} else {
				sections = append(sections, "", warnStyle.Render("没有匹配的命令（/help 查看全部）"))
			}
		}
		sections = append(sections, "", console.taskField.View(), dimStyle.Render("Enter 执行/提交 · Tab 补全命令 · Ctrl+J 换行 · Ctrl+O 事件 · Ctrl+C 退出"))
	case consoleRunning:
		if console.queuing {
			sections = append(sections, "", console.queueField.View(), dimStyle.Render("/steer <文本> 补充当前任务 · 其余 Enter 入队下一任务 · Ctrl+J 换行"))
		}
		if len(console.steeringStatus) > 0 {
			parts := make([]string, 0, len(console.steeringStatus))
			for _, receipt := range console.steeringStatus {
				parts = append(parts, receipt.id+"="+receipt.state)
			}
			sections = append(sections, dimStyle.Render("补充输入: "+strings.Join(parts, " · ")))
		}
		sections = append(sections, "", console.statusLine())
	case consoleApproval:
		sections = append(sections, strings.Join(console.approvalDetail, "\n"))
		sections = append(sections, "", warnStyle.Render("Enter 批准 / Esc 拒绝 / v 差异"))
	case consoleModelWizard:
		if console.wizard != nil {
			sections = append(sections, console.wizard.View())
		}
	case consoleTranscript:
		sections = []string{titleStyle.Render("事件查看器") + dimStyle.Render(fmt.Sprintf("  %d 条已持久化事件 · ↑↓ 滚动 / v 补丁差异 / Esc 返回", len(console.transcript)))}
		end := console.transcriptCursor + transcriptWindowHeight
		if end > len(console.transcript) {
			end = len(console.transcript)
		}
		for index := console.transcriptCursor; index < end; index++ {
			sections = append(sections, renderTranscriptLine(console.transcript[index]))
		}
	case consoleDiff:
		sections = []string{titleStyle.Render("补丁差异") + dimStyle.Render(fmt.Sprintf("  %d 行 · ↑↓ 滚动 / Esc 返回", len(console.diffLines)))}
		end := console.diffCursor + diffWindowHeight
		if end > len(console.diffLines) {
			end = len(console.diffLines)
		}
		for index := console.diffCursor; index < end; index++ {
			sections = append(sections, "  "+console.diffLines[index])
		}
	case consoleDone:
		sections = append(sections, "")
		if console.runErr == nil && console.result != nil && console.result.Result != nil {
			if response := strings.TrimSpace(console.result.Response); response != "" {
				sections = append(sections, itemStyle.Render(finalResponsePreview(response)))
			}
		}
		queueHint := "Enter 新任务 / Ctrl+C 退出"
		if len(console.queue) > 0 {
			queueHint = fmt.Sprintf("Enter 预填排队任务（%d 条）· Tab 丢弃队首 / Ctrl+C 退出", len(console.queue))
		}
		sections = append(sections, dimStyle.Render(queueHint))
	}
	return lipgloss.JoinVertical(lipgloss.Left, sections...) + "\n"
}

// welcomeBlock 生成空态启动信息区：模型与端点、工作区、轮次预算与审计
// 提示——全部来自已加载配置的只读投影，让用户打开即知"我在用什么、在哪
// 干活、从哪查证"。
func (console RunConsole) welcomeBlock() string {
	turns := "未配置"
	if budget := console.maxTurnsBudget(); budget > 0 {
		turns = strconv.Itoa(budget)
	}
	model := console.input.Config.Provider.Model
	if model == "" {
		model = "未配置模型"
	}
	lines := []string{
		itemStyle.Render(fmt.Sprintf("模型 %s · %s", model, hostOf(console.input.Config.Provider.BaseURL))),
		dimStyle.Render(fmt.Sprintf("工作区 %s · 轮次预算 %s", console.statusWorkspace(), turns)),
		dimStyle.Render("运行全程事件可审计（cairn inspect <run-id> 查证）· 密钥仅环境与本机凭据文件"),
		"",
		dimStyle.Render("输入任务开始 · /help 查看命令"),
		"",
	}
	return strings.Join(lines, "\n")
}

// hostOf 从端点地址提取主机名用于展示；解析失败返回原串截断。
func hostOf(baseURL string) string {
	if parsed, err := url.Parse(baseURL); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	if len(baseURL) > 40 {
		return baseURL[:40] + "…"
	}
	return baseURL
}

// consoleCommands 是主界面的命令清单：候选提示、Tab 补全与分发校验共用
// 本表，新增命令必须在此登记。
var consoleCommands = []struct {
	name        string
	description string
}{
	{"/help", "命令与按键清单"},
	{"/model", "模型配置向导（提交后热生效）"},
	{"/new", "新任务清屏（会话累计保留）"},
	{"/status", "模型、端点与预算"},
	{"/sessions", "当前会话链与工作区历史"},
	{"/tree", "会话树（含跨会话分支）"},
	{"/history", "从历史会话唯一链尾继续"},
	{"/fork", "从终态 run 创建新会话分支"},
	{"/steer", "运行中补充当前任务（安全点生效）"},
	{"/queue", "持久后续队列 add/confirm/cancel/list"},
	{"/rewind", "回退会话对话链"},
	{"/rewind-files", "回退工作区补丁（撤销所选任务起）"},
	{"/cost", "本任务与会话累计 token"},
	{"/theme", "切换主题"},
	{"/editor", "外部编辑器编辑当前输入"},
	{"/init", "生成或更新工作区 AGENTS.md 骨架"},
	{"/compact", "查看上下文用量与压缩检查点"},
	{"/clear", "清屏（保留会话链与统计）"},
	{"/context", "展示上下文预算与压缩策略"},
	{"/doctor", "只读环境与配置诊断"},
	{"/export", "导出会话事件摘要为 Markdown"},
	{"/plan", "查看当前任务与会话链计划"},
	{"/review", "回顾本会话任务结论"},
	{"/diff", "列出工作区未提交变更"},
	{"/approvals", "查看待审批操作"},
	{"/mention", "@ 引用文件语法说明"},
	{"/config", "查看当前生效配置"},
	{"/models", "检索模型目录"},
	{"/tools", "查看内置工具面"},
	{"/undo", "撤销草稿或最近排队项"},
	{"/search", "检索 transcript 与会话链"},
	{"/subagents", "查看子运行进度"},
	{"/usage", "用量告警与一键修复"},
	{"/trust", "管理信任标记"},
	{"/quit", "退出"},
}

// commandSuggestions 按输入前缀匹配命令清单，返回展示行；空前缀返回全部。
func commandSuggestions(text string) []string {
	var lines []string
	for _, command := range consoleCommands {
		if text == "" || strings.HasPrefix(command.name, text) {
			lines = append(lines, itemStyle.Render(command.name)+dimStyle.Render("  "+command.description))
		}
	}
	return lines
}

// helpCommandLines 按命令清单生成 /help 的展示行，每行四条命令名。
//
// 名称取自 consoleCommands 这张表：/help 不再维护第二份手写副本，新增命令
// 只登记一处，候选提示、Tab 补全与帮助不会漂移。命令说明在输入时的候选
// 提示里逐条给出，这里只列命令名——控制台的滚动缓冲只有 maxConsoleLines
// 行，命令清单必须连同提示行一起放得下，否则帮助的开头会被滚出屏幕。
func helpCommandLines() []string {
	const perLine = 4
	lines := make([]string, 0, len(consoleCommands)/perLine+1)
	for start := 0; start < len(consoleCommands); start += perLine {
		end := start + perLine
		if end > len(consoleCommands) {
			end = len(consoleCommands)
		}
		names := make([]string, 0, end-start)
		for _, command := range consoleCommands[start:end] {
			names = append(names, command.name)
		}
		lines = append(lines, strings.Join(names, "  "))
	}
	return lines
}

// completeCommand 返回前缀匹配的补全结果：唯一匹配补全到完整命令，多
// 匹配补全到公共前缀（比输入长时），零匹配返回空串——与 readline 的
// Tab 行为一致。
func completeCommand(text string) string {
	if text == "" {
		return ""
	}
	common := ""
	count := 0
	for _, command := range consoleCommands {
		if !strings.HasPrefix(command.name, text) {
			continue
		}
		count++
		if count == 1 {
			common = command.name
		} else {
			common = commonPrefix(common, command.name)
		}
	}
	if count == 0 || len(common) <= len(text) {
		return ""
	}
	return common
}

// commonPrefix 返回两串的最长公共前缀。
func commonPrefix(left, right string) string {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for index := 0; index < limit; index++ {
		if left[index] != right[index] {
			return left[:index]
		}
	}
	return left[:limit]
}

// queueCommand 处理 /queue 子命令：list 只读投影，add 入队，confirm 确认启动
// 新 run，cancel 取消待消费输入。所有写操作经 App 层 Journal 命名空间。
