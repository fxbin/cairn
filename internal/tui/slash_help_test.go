// slash_help_test 锁定 TUI 只读命令面的三件事：/help 的命令清单与登记表不漂移、
// 候选提示按前缀过滤、/subagents 的只读投影如实展示子运行与预算汇总。
//
// Author: fxbin
package tui

import (
	"strings"
	"testing"

	"github.com/fxbin/cairn/internal/config"
)

// TestHelpListsEveryRegisteredCommand 锁定 /help 列出全部已注册命令。
//
// 命令登记在 consoleCommands 一张表里，候选提示、Tab 补全与 /help 共用；
// 若 /help 退回手写副本，新增命令就会在帮助里看不到，这条用例即变红。
func TestHelpListsEveryRegisteredCommand(t *testing.T) {
	console := RunConsole{}
	handled, _ := console.runCommand("/help")
	if !handled {
		t.Fatal("/help 必须被分发器接受")
	}
	text := strings.Join(console.lines, "\n")
	for _, command := range consoleCommands {
		if !strings.Contains(text, command.name) {
			t.Fatalf("/help 缺少已注册命令 %s", command.name)
		}
	}
}

// TestHelpFitsScrollbackBuffer 锁定 /help 的整屏内容放得下滚动缓冲。
//
// 控制台只保留最近 maxConsoleLines 行，超出的部分会把帮助开头滚出屏幕，
// 用户看到的是从中间开始的帮助。这条用例守住「帮助必须一屏读完」。
func TestHelpFitsScrollbackBuffer(t *testing.T) {
	console := RunConsole{}
	console.runCommand("/help")
	if len(console.lines) > maxConsoleLines {
		t.Fatalf("/help 输出 %d 行，超过滚动缓冲 %d 行，开头会被滚出屏幕", len(console.lines), maxConsoleLines)
	}
}

// TestCommandSuggestionsCarryDescriptions 锁定命令说明在候选提示里给出：
// /help 只列命令名以保证一屏放得下，说明由候选提示承载。
func TestCommandSuggestionsCarryDescriptions(t *testing.T) {
	suggestions := strings.Join(commandSuggestions(""), "\n")
	for _, command := range consoleCommands {
		if !strings.Contains(suggestions, command.name) {
			t.Fatalf("候选提示缺少命令 %s", command.name)
		}
		if !strings.Contains(suggestions, command.description) {
			t.Fatalf("候选提示缺少命令 %s 的说明 %q", command.name, command.description)
		}
	}
}

// TestCommandSuggestionsFilterByPrefix 锁定候选提示按前缀过滤：空前缀给全量，
// 命中前缀给子集，无命中给空。
func TestCommandSuggestionsFilterByPrefix(t *testing.T) {
	if got := len(commandSuggestions("")); got != len(consoleCommands) {
		t.Fatalf("空前缀应给出全部 %d 条命令，得到 %d 条", len(consoleCommands), got)
	}
	suggestions := commandSuggestions("/sub")
	if len(suggestions) != 1 {
		t.Fatalf("/sub 应只命中 /subagents，得到 %d 条: %v", len(suggestions), suggestions)
	}
	if !strings.Contains(suggestions[0], "/subagents") {
		t.Fatalf("候选提示缺少 /subagents: %q", suggestions[0])
	}
	if got := commandSuggestions("/不存在的命令"); len(got) != 0 {
		t.Fatalf("无匹配前缀应返回空，得到 %v", got)
	}
}

// TestSubagentsEmptyState 锁定没有子运行时如实说明，不显示为零。
func TestSubagentsEmptyState(t *testing.T) {
	console := RunConsole{}
	console.appendSubagents()
	text := strings.Join(console.lines, "\n")
	if !strings.Contains(text, "暂无子运行") {
		t.Fatalf("空态应说明暂无子运行: %q", text)
	}
	if strings.Contains(text, "子运行 0 个") {
		t.Fatalf("空态不应显示子运行计数: %q", text)
	}
}

// TestSubagentsShowsProgressAndBudget 锁定子运行进度与预算汇总如实展示。
func TestSubagentsShowsProgressAndBudget(t *testing.T) {
	console := RunConsole{
		input: RunConsoleInput{Config: config.Config{Budget: config.BudgetConfig{
			MaxTurns:        40,
			MaxInputTokens:  700000,
			MaxOutputTokens: 60000,
		}}},
		subagentViews: []SubagentView{
			{
				ChildRunID:      "run-child-1",
				ParentRunID:     "run-parent",
				Task:            "拆分实现",
				State:           "running",
				ToolCalls:       3,
				Tokens:          1200,
				PendingApproval: true,
			},
			{
				ChildRunID: "run-child-2",
				State:      "completed",
				Outcome:    "ok",
				ToolCalls:  2,
				Tokens:     800,
			},
		},
	}
	console.appendSubagents()
	text := strings.Join(console.lines, "\n")
	for _, required := range []string{
		"子运行 2 个",
		"run-child-1",
		"待审批",
		"预算汇总：子运行 2 · 工具 5 · token 2000",
		"max_turns=40",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("子运行展示缺少 %q: %q", required, text)
		}
	}
}

// TestSubagentDetailByID 锁定按 id 查子运行详情，并区分「未找到」。
func TestSubagentDetailByID(t *testing.T) {
	console := RunConsole{subagentViews: []SubagentView{{
		ChildRunID:  "run-child-1",
		ParentRunID: "run-parent",
		Task:        "拆分实现",
		State:       "completed",
		Outcome:     "ok",
		ToolCalls:   3,
		Tokens:      1200,
	}}}
	console.appendSubagentDetail("run-child-1")
	text := strings.Join(console.lines, "\n")
	for _, required := range []string{"子运行详情", "parent=run-parent", "outcome=ok", "tokens=1200"} {
		if !strings.Contains(text, required) {
			t.Fatalf("子运行详情缺少 %q: %q", required, text)
		}
	}

	console.lines = nil
	console.appendSubagentDetail("run-不存在")
	if !strings.Contains(strings.Join(console.lines, "\n"), "未找到子运行") {
		t.Fatalf("未知 id 应提示未找到: %v", console.lines)
	}

	console.lines = nil
	console.appendSubagentDetail("  ")
	if !strings.Contains(strings.Join(console.lines, "\n"), "用法") {
		t.Fatalf("空 id 应给出用法提示: %v", console.lines)
	}
}
