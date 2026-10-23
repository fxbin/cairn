// terminal_capability.go 定义终端能力覆盖：允许显式声明超链接/真彩/图像
// 支持，覆盖自动探测结果；供 exec_session 与 TUI 渲染选择。
// Author: fxbin
package tools

// TerminalCapabilities 是终端能力覆盖声明。
type TerminalCapabilities struct {
	// Hyperlinks 表示 OSC8 超链接支持。
	Hyperlinks bool `json:"hyperlinks"`
	// TrueColor 表示 24-bit 真彩支持。
	TrueColor bool `json:"truecolor"`
	// Images 表示内联图像支持。
	Images bool `json:"images"`
	// Overridden 为 true 时忽略自动探测。
	Overridden bool `json:"overridden"`
}

// DefaultTerminalCapabilities 返回保守默认（全部关闭，自动探测）。
func DefaultTerminalCapabilities() TerminalCapabilities {
	return TerminalCapabilities{}
}

// OverrideTerminalCapabilities 应用显式覆盖。
func OverrideTerminalCapabilities(base TerminalCapabilities, hyperlinks, truecolor, images bool) TerminalCapabilities {
	return TerminalCapabilities{
		Hyperlinks: hyperlinks,
		TrueColor:  truecolor,
		Images:     images,
		Overridden: true,
	}
}

// DetectTerminalCapabilities 从环境变量推断能力（TERM/COLORTERM）。
func DetectTerminalCapabilities(term, colorTerm string) TerminalCapabilities {
	capabilities := TerminalCapabilities{}
	if term == "xterm-256color" || term == "screen-256color" || term == "tmux-256color" {
		capabilities.TrueColor = colorTerm == "truecolor" || colorTerm == "24bit"
		capabilities.Hyperlinks = true
	}
	if term == "xterm-kitty" || term == "wezterm" {
		capabilities.Images = true
		capabilities.Hyperlinks = true
		capabilities.TrueColor = true
	}
	return capabilities
}

// Effective 返回最终生效能力：覆盖优先于探测。
func (capabilities TerminalCapabilities) Effective(detected TerminalCapabilities) TerminalCapabilities {
	if capabilities.Overridden {
		return capabilities
	}
	return detected
}
