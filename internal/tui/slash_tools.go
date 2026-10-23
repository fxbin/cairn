// slash_tools.go 实现 TUI 命令 /tools：只读展示内置工具面与副作用分类。
// Author: fxbin
package tui

import "fmt"

// builtinToolInfo 是内置工具的展示摘要。
type builtinToolInfo struct {
	Name        string
	Effect      string
	Description string
}

// builtinTools 是 Cairn 内置工具面（与 presetTools 一致）。
var builtinTools = []builtinToolInfo{
	{"list_files", "none", "列举工作区文件"},
	{"read_file", "none", "读取工作区文件"},
	{"search_text", "none", "工作区文本检索"},
	{"apply_patch", "workspace", "确定性单文件补丁"},
	{"exec_command", "process", "argv-only 命令执行"},
	{"send_message", "none", "父子 run 跟进消息"},
	{"web_fetch", "process", "受限网页抓取"},
	{"web_search", "process", "受限网页搜索"},
	{"exec_session", "process", "受控 tmux 会话（默认禁用）"},
}

// appendTools 列出内置工具与副作用分类。
func (console *RunConsole) appendTools() {
	console.appendLine(itemStyle.Render(fmt.Sprintf("内置工具 %d 个", len(builtinTools))))
	for _, tool := range builtinTools {
		console.appendLine(dimStyle.Render(fmt.Sprintf("  %s [%s] — %s", tool.Name, tool.Effect, tool.Description)))
	}
	console.appendLine(dimStyle.Render("  副作用 none=只读 · workspace=写工作区 · process=进程/网络"))
	console.appendLine(dimStyle.Render("  MCP 工具经 mcp_discover / tool_search 按需加载"))
}
