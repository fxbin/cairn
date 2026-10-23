// slash_search.go 实现 TUI 命令 /search：在当前 transcript 与会话链中
// 检索关键字；只读投影，不触盘、不改状态。
// Author: fxbin
package tui

import (
	"fmt"
	"strings"
)

// appendSearch 在 transcript/会话链中检索关键字。
func (console *RunConsole) appendSearch(argument string) {
	keyword := strings.TrimSpace(argument)
	if keyword == "" {
		console.appendLine(warnStyle.Render("用法: /search <关键字>（检索 transcript 与会话链）"))
		return
	}
	lower := strings.ToLower(keyword)
	hits := 0
	for _, event := range console.transcript {
		if !strings.Contains(strings.ToLower(string(event.Type)), lower) &&
			!strings.Contains(strings.ToLower(string(event.Payload)), lower) {
			continue
		}
		hits++
		if hits > 20 {
			break
		}
		line := summarizeEvent(event)
		if line == "" {
			line = string(event.Type)
		}
		console.appendLine(dimStyle.Render(fmt.Sprintf("  #%d %s — %s", event.Seq, event.Type, stripANSIForExport(line))))
	}
	for _, entry := range console.sessionRuns {
		if strings.Contains(strings.ToLower(entry.summary), lower) || strings.Contains(strings.ToLower(entry.verdict), lower) {
			hits++
			console.appendLine(dimStyle.Render(fmt.Sprintf("  [链] %s · %s · %s", shortRun(entry.id), entry.verdict, firstLine(entry.summary, 60))))
		}
	}
	if hits == 0 {
		console.appendLine(dimStyle.Render("无匹配（transcript 仅含最近事件）"))
		return
	}
	console.appendLine(dimStyle.Render(fmt.Sprintf("共 %d 处匹配（最多展示 20 条事件）", hits)))
}
