// theme.go 集中界面的语义化文本样式与主题切换（T3.3）：包级样式变量是全
// 部渲染点的单一取用口，主题表只改变这些变量的取值，切换在界面事件循环
// 内完成（单 goroutine，无并发写）。会话内生效、不落盘。
//
// Author: fxbin
package tui

import (
	"github.com/charmbracelet/lipgloss"
)

// themeName 是一个内置主题的标识与展示名。
type themeName struct {
	id    string
	label string
}

// builtinThemes 是内置主题清单（切换顺序即清单顺序）：default 采用与 Cairn
// Studio 一致的 Paper Stone / Basalt Night 自适应色板；classic 保留基础 ANSI
// 调色板；mono 去色保留排版强调，适配无色终端与抓屏管道；light 针对浅色终端提升对比度。
var builtinThemes = []themeName{
	{id: "default", label: "暖石默认"},
	{id: "classic", label: "经典终端"},
	{id: "mono", label: "无色"},
	{id: "light", label: "浅色终端"},
}

// activeThemeIndex 是当前主题在清单中的序号；由 /theme 在事件循环内推进。
var activeThemeIndex int

// applyTheme 按序号应用主题：重赋全部语义样式变量。序号越界失败关闭为
// 默认主题。
func applyTheme(index int) {
	if index < 0 || index >= len(builtinThemes) {
		index = 0
	}
	activeThemeIndex = index
	switch builtinThemes[index].id {
	case "mono":
		titleStyle = lipgloss.NewStyle().Bold(true)
		dimStyle = lipgloss.NewStyle().Faint(true)
		okStyle = lipgloss.NewStyle().Bold(true)
		warnStyle = lipgloss.NewStyle().Bold(true)
		errStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
		itemStyle = lipgloss.NewStyle()
		itemSelectedStyle = lipgloss.NewStyle().Bold(true).Reverse(true)
		badgeStyle = lipgloss.NewStyle().Bold(true)
	case "light":
		titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#9A4420"))
		dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#6B655C"))
		okStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#2D603D"))
		warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#8A5A00"))
		errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#A61B1B"))
		itemStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#1F1D1A"))
		itemSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7E3314"))
		badgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5B554C"))
	case "classic":
		titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
		dimStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
		okStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
		warnStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
		errStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
		itemStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("7"))
		itemSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
		badgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	default:
		titleStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#B55C35", Dark: "#D47952"})
		dimStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#787268", Dark: "#968F84"})
		okStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#3F6F4E", Dark: "#6BA37D"})
		warnStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#9A6700", Dark: "#D49B38"})
		errStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#B42318", Dark: "#E05A4E"})
		itemStyle = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "#1F1D1A", Dark: "#EAE5DC"})
		itemSelectedStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#B55C35", Dark: "#F0A07C"})
		badgeStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.AdaptiveColor{Light: "#5B554C", Dark: "#C9C2B6"})
	}
}

// cycleTheme 推进到下一个内置主题并返回切换提示。
func cycleTheme() string {
	next := (activeThemeIndex + 1) % len(builtinThemes)
	applyTheme(next)
	return "主题已切换: " + builtinThemes[next].label
}

// currentThemeLabel 返回当前主题展示名。
func currentThemeLabel() string {
	return builtinThemes[activeThemeIndex].label
}
