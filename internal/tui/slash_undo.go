// slash_undo.go 实现 TUI 命令 /undo：撤销当前草稿输入或最近一次排队项；
// 不回退事件日志、不改工作区文件。
// Author: fxbin
package tui

import "strings"

// appendUndo 撤销最近草稿/排队输入。
func (console *RunConsole) appendUndo(argument string) {
	target := strings.TrimSpace(argument)
	switch target {
	case "", "draft":
		previous := console.taskField.Value()
		if strings.TrimSpace(previous) == "" {
			console.appendLine(dimStyle.Render("无草稿可撤销（/undo queue 撤销最近排队项）"))
			return
		}
		console.taskField.SetValue("")
		console.appendLine(okStyle.Render("已清空当前草稿（事件日志与工作区未变）"))
	case "queue":
		if len(console.queue) == 0 {
			console.appendLine(dimStyle.Render("队列为空"))
			return
		}
		last := console.queue[len(console.queue)-1]
		console.queue = console.queue[:len(console.queue)-1]
		console.taskField.SetValue(last)
		console.appendLine(okStyle.Render("已撤销最近排队项并回填草稿（事件日志与工作区未变）"))
	default:
		console.appendLine(warnStyle.Render("用法: /undo [draft|queue]"))
	}
}
