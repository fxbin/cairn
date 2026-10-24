// 向导中继与事件落盘摘要。
// Author: fxbin
package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/domain"
)

func (console RunConsole) relayWizardKey(message tea.KeyMsg) (tea.Model, tea.Cmd) {
	return console.relayWizardMsg(message)
}

// relayWizardMsg 把任意消息（按键、spinner 节拍、输入框闪烁等）转发给嵌
// 入向导并桥接其命令；consoleEventMsg 属于主界面运行流不进入向导。
func (console RunConsole) relayWizardMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	if console.wizard == nil {
		console.mode = consoleInput
		return console, nil
	}
	model, cmd := console.wizard.Update(msg)
	wizard, ok := model.(ConfigWizard)
	if !ok {
		console.mode = consoleInput
		return console, nil
	}
	console.wizard = &wizard
	if cmd == nil {
		return console, nil
	}
	return console, relayWizardCmd(cmd, console.wizardGeneration)
}

// relayWizardCmd 递归包装嵌入向导命令及 Batch 子命令，使所有异步结果携带
// 创建它们的向导实例世代；nil 结果保持 nil，由 Bubble Tea 忽略。
func relayWizardCmd(cmd tea.Cmd, generation uint64) tea.Cmd {
	return func() tea.Msg {
		message := cmd()
		if message == nil {
			return nil
		}
		if batch, ok := message.(tea.BatchMsg); ok {
			wrapped := make(tea.BatchMsg, 0, len(batch))
			for _, child := range batch {
				wrapped = append(wrapped, relayWizardCmd(child, generation))
			}
			return wrapped
		}
		return wizardRelayMsg{generation: generation, message: message}
	}
}

// exitWizard 退出配置向导回到输入态：提交成功时经调用方回调热重载配置
// 使后续任务立即生效，重载失败如实提示（新配置对下次进程仍生效）；取消
// 或失败时不改变现有配置。
func (console *RunConsole) exitWizard() {
	console.mode = consoleInput
	submitted := console.wizard != nil && console.wizard.outcome != nil
	console.wizard = nil
	console.lines = nil
	if submitted && console.input.ReloadConfig != nil {
		if reloaded, err := console.input.ReloadConfig(); err == nil {
			console.input.Config = reloaded
			console.appendLine(okStyle.Render("配置已更新，后续任务即用新模型"))
			return
		} else {
			console.appendLine(warnStyle.Render("配置已写入但当前界面重载失败: " + err.Error()))
			return
		}
	}
	if submitted {
		console.appendLine(dimStyle.Render("配置已写入，重启或下次运行生效"))
		return
	}
	console.appendLine(dimStyle.Render("已退出配置向导（配置未修改）"))
}

// applyEvent 把已持久化事件追加为摘要行并累计运行统计。
func (console *RunConsole) applyEvent(event domain.Event) {
	console.transcript = append(console.transcript, event)
	if len(console.transcript) > transcriptEventLimit {
		console.transcript = console.transcript[len(console.transcript)-transcriptEventLimit:]
	}
	console.updateSteeringReceipt(event)
	switch event.Type {
	case domain.EventTurnOpened:
		console.turn++
		console.turnToolCalls = 0
		console.turnExecutedTools = 0
		console.turnPromptTokens = 0
		console.turnOutputTokens = 0
		console.turnOpenedAt = event.OccurredAt
	case domain.EventToolCallProposed:
		console.toolCalls++
		console.turnToolCalls++
	case domain.EventOperationStarted:
		console.turnExecutedTools++
	case domain.EventModelResponseFinished:
		var payload struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if eventPayload(event, &payload) {
			console.promptTokens += int64(payload.Usage.PromptTokens)
			console.outputTokens += int64(payload.Usage.CompletionTokens)
			console.turnPromptTokens += int64(payload.Usage.PromptTokens)
			console.turnOutputTokens += int64(payload.Usage.CompletionTokens)
		}
	case domain.EventSubagentDelegated, domain.EventSubagentCompleted:
		console.populateSubagentView(event)
	case domain.EventTurnClosed:
		console.appendLine(console.turnSummaryLine(event))
	case domain.EventOperationSucceeded:
		if console.rewinding != nil {
			var payload struct {
				ToolName string `json:"tool_name"`
			}
			if eventPayload(event, &payload) && payload.ToolName == "apply_patch" {
				console.rewinding.applied++
			}
		}
	}
	if line := summarizeEvent(event); line != "" {
		console.appendLine(line)
	}
}

// appendLine 追加展示行并维持滚动缓冲上限。
func (console *RunConsole) appendLine(line string) {
	console.lines = append(console.lines, line)
	if len(console.lines) > maxConsoleLines {
		console.lines = console.lines[len(console.lines)-maxConsoleLines:]
	}
}

// loadApprovalDetail 在审批暂停时从事件日志提取最近一次工具提议的参数预
// 览：apply_patch 展示写入模式、目标路径与内容/替换摘要，exec 展示 argv，
// 其余工具截断参数原文。预览是已持久化事实的只读投影，不可用时如实标注
// 而不阻塞审批。
