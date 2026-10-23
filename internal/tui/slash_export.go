// slash_export.go 实现 TUI 命令 /export：把当前会话已展示事件摘要导出为
// 工作区内 Markdown 文件。只读内存 transcript，不触碰 Journal，不扩大权限。
// Author: fxbin
package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// defaultExportDir 是导出文件所在目录名（工作区下）。
const defaultExportDir = ".cairn-exports"

// exportTranscript 将 transcript 写入工作区导出文件，返回展示行。
func (console *RunConsole) exportTranscript(argument string) {
	root := strings.TrimSpace(console.input.Workspace)
	if root == "" {
		if cwd, err := os.Getwd(); err == nil {
			root = cwd
		}
	}
	if root == "" {
		console.appendLine(warnStyle.Render("无法解析工作区，/export 未执行"))
		return
	}
	name := strings.TrimSpace(argument)
	if name == "" {
		name = "transcript-" + time.Now().Format("20060102-150405") + ".md"
	}
	if !strings.HasSuffix(name, ".md") {
		name += ".md"
	}
	if strings.Contains(name, "..") || strings.ContainsAny(name, `/\`) {
		console.appendLine(warnStyle.Render("文件名不能包含路径分隔符或 .."))
		return
	}
	dir := filepath.Join(root, defaultExportDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		console.appendLine(warnStyle.Render("创建导出目录失败: " + err.Error()))
		return
	}
	target := filepath.Join(dir, name)
	var b strings.Builder
	b.WriteString("# Cairn 会话导出\n\n")
	b.WriteString(fmt.Sprintf("- 导出时间：%s\n", time.Now().Format(time.RFC3339)))
	b.WriteString(fmt.Sprintf("- 工作区：%s\n", root))
	b.WriteString(fmt.Sprintf("- 事件条数：%d\n\n", len(console.transcript)))
	for _, event := range console.transcript {
		line := summarizeEvent(event)
		if line == "" {
			continue
		}
		b.WriteString(fmt.Sprintf("- `%s` %s\n", event.Type, strings.TrimSpace(stripANSIForExport(line))))
	}
	if err := os.WriteFile(target, []byte(b.String()), 0o644); err != nil {
		console.appendLine(warnStyle.Render("写入导出文件失败: " + err.Error()))
		return
	}
	console.appendLine(okStyle.Render("已导出到 " + target))
}

// stripANSI 移除样式控制序列，避免导出文本携带终端颜色码。
func stripANSIForExport(s string) string {
	var b strings.Builder
	inEsc := false
	for _, r := range s {
		switch {
		case inEsc:
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
				inEsc = false
			}
		case r == 0x1b:
			inEsc = true
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
