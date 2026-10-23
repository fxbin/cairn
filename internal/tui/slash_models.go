// slash_models.go 实现 TUI 命令 /models：基于 SearchModels 的模型目录
// 只读检索；不触网络、不改配置。
// Author: fxbin
package tui

import (
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/tools"
)

// appendModels 按关键字检索并展示模型条目。
func (console *RunConsole) appendModels(argument string) {
	query := strings.TrimSpace(argument)
	entries := console.modelEntries()
	matched := tools.SearchModels(query, entries)
	if len(matched) == 0 {
		console.appendLine(dimStyle.Render("无匹配模型（/models [关键字] 可过滤）"))
		return
	}
	console.appendLine(itemStyle.Render(fmt.Sprintf("模型目录 %d 条（关键字 %q）:", len(matched), query)))
	for _, entry := range matched {
		console.appendLine(dimStyle.Render(fmt.Sprintf("  %s · %s · %s", entry.Provider, entry.Name, entry.ID)))
	}
	console.appendLine(dimStyle.Render("  切换请用 /model"))
}

// modelEntries 从当前配置与解析项组装可检索目录。
func (console RunConsole) modelEntries() []tools.ModelSearchEntry {
	entries := []tools.ModelSearchEntry{{
		ID:       console.input.Config.Provider.Model,
		Name:     console.input.Config.Provider.Model,
		Provider: "active",
	}}
	for _, item := range console.input.Config.Resolution {
		if item.Field == "provider.model" && item.Value != "" {
			entries = append(entries, tools.ModelSearchEntry{
				ID:       item.Value,
				Name:     item.Value,
				Provider: "resolved",
			})
		}
	}
	return entries
}
