// slash_trust.go 实现 TUI 命令 /trust：管理工作区信任标记与显式拒绝标记。
// Author: fxbin
package tui

import (
	"fmt"

	"github.com/fxbin/cairn/internal/tools"
)

// appendTrust 展示或切换工作区信任状态。
func (console *RunConsole) appendTrust(argument string) {
	root := console.input.Workspace
	switch argument {
	case "":
		trusted := tools.IsTrusted(root)
		untrusted := tools.IsExplicitlyUntrusted(root)
		status := "默认信任（无显式标记）"
		if trusted {
			status = "已信任（.cairn-trusted）"
		}
		if untrusted {
			status = "显式不信任（.cairn-untrusted）"
		}
		console.appendLine(itemStyle.Render("工作区信任状态"))
		console.appendLine(dimStyle.Render(fmt.Sprintf("  %s", status)))
		console.appendLine(dimStyle.Render("  /trust trust|untrust|reset 切换状态"))
	case "trust":
		if err := tools.TrustWorkspace(root); err != nil {
			console.appendLine(warnStyle.Render("标记失败: " + err.Error()))
			return
		}
		console.appendLine(okStyle.Render("已标记为信任（.cairn-trusted）"))
	case "untrust":
		if err := tools.UntrustWorkspace(root); err != nil {
			console.appendLine(warnStyle.Render("操作失败: " + err.Error()))
			return
		}
		if err := tools.WriteUntrustedMarker(root); err != nil {
			console.appendLine(warnStyle.Render("标记失败: " + err.Error()))
			return
		}
		console.appendLine(okStyle.Render("已标记为不信任（.cairn-untrusted），写操作将被拒绝"))
	case "reset":
		_ = tools.UntrustWorkspace(root)
		_ = tools.RemoveUntrustedMarker(root)
		console.appendLine(okStyle.Render("已恢复默认信任状态"))
	default:
		console.appendLine(warnStyle.Render("用法: /trust [trust|untrust|reset]"))
	}
}
