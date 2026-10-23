// slash_more.go 实现 TUI 通用命令 /plan /review /diff /approvals /mention。
// 均为只读投影：不写 Journal、不改工作区、不授予执行权限。
// Author: fxbin
package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/fxbin/cairn/internal/domain"
)

// appendPlan 投影当前任务与会话链摘要，帮助用户确认“接下来做什么”。
func (console *RunConsole) appendPlan() {
	console.appendLine(itemStyle.Render("当前计划（只读）"))
	task := console.lastTask
	if task == "" {
		task = strings.TrimSpace(console.taskField.Value())
	}
	if task == "" {
		task = "（未设置）"
	}
	console.appendLine(dimStyle.Render("  任务: " + firstLine(task, 120)))
	if console.sessionID != "" {
		console.appendLine(dimStyle.Render(fmt.Sprintf("  会话: %s · 历史任务 %d · 续链 %s", console.sessionID, len(console.sessionRuns), shortRun(console.sessionPrev))))
	} else {
		console.appendLine(dimStyle.Render("  会话: 未开链（下一任务将新建）"))
	}
	if len(console.sessionRuns) > 0 {
		console.appendLine(dimStyle.Render("  链尾:"))
		start := 0
		if len(console.sessionRuns) > 3 {
			start = len(console.sessionRuns) - 3
		}
		for _, entry := range console.sessionRuns[start:] {
			console.appendLine(dimStyle.Render(fmt.Sprintf("    · %s · %s · %s", shortRun(entry.id), entry.verdict, firstLine(entry.summary, 40))))
		}
	}
	console.appendLine(dimStyle.Render("  下一步: 直接输入任务描述；运行中用 /steer 补充；用 /queue 预排下一任务"))
}

// appendReview 投影会话链各任务结论，便于回看“做了什么、结果如何”。
func (console *RunConsole) appendReview() {
	console.appendLine(itemStyle.Render("执行回顾（只读）"))
	if len(console.sessionRuns) == 0 {
		console.appendLine(dimStyle.Render("  当前会话尚无已完成任务"))
		return
	}
	for index, entry := range console.sessionRuns {
		console.appendLine(dimStyle.Render(fmt.Sprintf("  %d. %s · %s · %s", index+1, shortRun(entry.id), entry.verdict, firstLine(entry.summary, 80))))
	}
	console.appendLine(dimStyle.Render(fmt.Sprintf("  合计 %d 个任务 · 本任务 in %d / out %d tok", len(console.sessionRuns), console.promptTokens, console.outputTokens)))
}

// gitStatusTimeout 限制只读 Git 查询的总等待时间，避免阻塞界面退出。
const gitStatusTimeout = 5 * time.Second

// gitStatusOutputLimit 限制状态查询内存；显示条数限制不能替代读取体积限制。
const gitStatusOutputLimit = 256 * 1024

// gitStatusDisplayLimit 是单次查询展示的路径条数上限。
const gitStatusDisplayLimit = 20

// gitStatusMsg 把异步查询结果绑定到最新请求，旧结果不能覆盖后发查询。
type gitStatusMsg struct {
	request uint64
	lines   []string
	err     error
}

// gitStatusOutput 在读取时限制 Git 输出体积，不在完整分配之后才截断。
type gitStatusOutput struct{ buffer bytes.Buffer }

// Write 仅接受上限内的输出；超限立即停止管道复制并由命令时限兜底回收。
func (output *gitStatusOutput) Write(data []byte) (int, error) {
	remaining := gitStatusOutputLimit - output.buffer.Len()
	if len(data) > remaining {
		_, _ = output.buffer.Write(data[:remaining])
		return remaining, io.ErrShortBuffer
	}
	return output.buffer.Write(data)
}

// diffCmd 在后台执行无可选索引写入、无 fsmonitor 回调的有界状态查询。
func (console *RunConsole) diffCmd() tea.Cmd {
	console.diffRequest++
	request, root, parent := console.diffRequest, workspaceRootOrDefault(console.input.Workspace), console.rootCtx
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(parent, gitStatusTimeout)
		defer cancel()
		if _, err := os.Stat(root); err != nil {
			return gitStatusMsg{request: request, err: errors.New("无法解析工作区目录")}
		}
		cmd := exec.CommandContext(ctx, "git", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-C", root, "status", "--porcelain=v1", "-z", "--ignore-submodules=all")
		cmd.WaitDelay = time.Second
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "GIT_") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
		output := new(gitStatusOutput)
		cmd.Stdout = output
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			switch {
			case ctx.Err() != nil:
				return gitStatusMsg{request: request, err: errors.New("Git 查询已取消或超时")}
			case errors.Is(err, io.ErrShortBuffer):
				return gitStatusMsg{request: request, err: errors.New("Git 状态输出超出上限，未展示不完整结果")}
			default:
				return gitStatusMsg{request: request, err: errors.New("git status 不可用（非 Git 仓库或查询失败）")}
			}
		}
		lines, err := gitStatusLines(output.buffer.String())
		return gitStatusMsg{request: request, lines: lines, err: err}
	}
}

// gitStatusLines 按 NUL 记录解析状态与重命名，文件名经转义后才进入终端。
func gitStatusLines(raw string) ([]string, error) {
	if raw == "" {
		return []string{"工作区干净（无未提交变更）"}, nil
	}
	records := strings.Split(raw, "\x00")
	if records[len(records)-1] != "" {
		return nil, errors.New("Git 状态记录不完整")
	}
	records = records[:len(records)-1]
	var shown []string
	total := 0
	for index := 0; index < len(records); index++ {
		record := records[index]
		if len(record) < 4 || record[2] != ' ' {
			return nil, errors.New("Git 状态记录无效")
		}
		line := record[:2] + " " + strconv.Quote(record[3:])
		if strings.ContainsAny(record[:2], "RC") {
			index++
			if index >= len(records) {
				return nil, errors.New("Git 重命名记录不完整")
			}
			line += " ← " + strconv.Quote(records[index])
		}
		total++
		if len(shown) < gitStatusDisplayLimit {
			shown = append(shown, "  "+line)
		}
	}
	lines := []string{fmt.Sprintf("工作区变更 %d 项（只读 Git 状态）:", total)}
	lines = append(lines, shown...)
	if total > len(shown) {
		lines = append(lines, fmt.Sprintf("  … 另有 %d 项", total-len(shown)))
	}
	return append(lines, "  完整 diff 请用 git diff 或 GUI 检查器"), nil
}

// appendApprovals 投影当前是否处于等待审批，并提示查看方式。
func (console *RunConsole) appendApprovals() {
	if console.waiting == nil {
		console.appendLine(dimStyle.Render("当前无待审批操作"))
		return
	}
	console.appendLine(warnStyle.Render("存在待审批操作"))
	if len(console.approvalDetail) > 0 {
		for _, line := range console.approvalDetail {
			console.appendLine(line)
		}
	}
	console.appendLine(dimStyle.Render("  运行中的审批弹层可直接允许/拒绝；v 查看差异"))
}

// appendMention 展示 @ 引用语法说明，帮助用户引用文件进任务。
func (console *RunConsole) appendMention() {
	console.appendLine(itemStyle.Render("@ 引用（mention）"))
	console.appendLine(dimStyle.Render("  在任务描述中写 @path/to/file 可引用工作区文件"))
	console.appendLine(dimStyle.Render("  多个引用用空格分隔；路径相对工作区根"))
	console.appendLine(dimStyle.Render("  引用内容进入模型上下文前会按敏感与大小边界处理"))
	console.appendLine(dimStyle.Render("  示例: 修复 @internal/tui/run.go 中的命令分发"))
}

// firstLine 截取首行并限长。
func firstLine(text string, limit int) string {
	line := strings.TrimSpace(strings.SplitN(text, "\n", 2)[0])
	runes := []rune(line)
	if len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return line
}

// shortRun 缩短 run 标识便于展示。
func shortRun(id domain.RunID) string {
	if id == "" {
		return "-"
	}
	runes := []rune(string(id))
	if len(runes) <= 12 {
		return string(id)
	}
	return string(runes[:12]) + "…"
}
