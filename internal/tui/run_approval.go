// 审批详情与 Diff 预览。
// Author: fxbin
package tui

import (
	"encoding/json"
	"errors"
	"os"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

func (console RunConsole) loadApprovalDetail(waiting *ports.WaitingApprovalError) []string {
	workspace := console.input.Workspace
	if workspace == "" {
		if current, err := os.Getwd(); err == nil {
			workspace = current
		}
	}
	inspected, err := console.input.Service.Inspect(console.rootCtx, workspace, console.input.StateDir, console.input.Config, waiting.RunID, true)
	if err != nil {
		return []string{dimStyle.Render("（参数预览不可用: " + err.Error() + "）")}
	}
	return approvalProposalPreview(inspected.Events)
}

// approvalProposalPreview 从事件流尾部提取最近一次工具提议的参数预览行；
// 无提议事件时返回空切片。
func approvalProposalPreview(events []domain.Event) []string {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload struct {
			ToolName  string          `json:"tool_name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if !eventPayload(event, &payload) {
			return nil
		}
		return proposalLines(payload.ToolName, payload.Arguments)
	}
	return nil
}

// openApprovalDiff 在审批态打开待审批补丁的差异视图；差异不可用时就地提示
// 并留在审批态，不阻塞审批决定。
func (console RunConsole) openApprovalDiff() (tea.Model, tea.Cmd) {
	lines, err := console.patchApprovalDiff()
	if err != nil {
		console.appendLine(warnStyle.Render("差异不可用: " + err.Error()))
		return console, nil
	}
	console.diffLines = lines
	console.diffCursor = 0
	console.diffReturn = consoleApproval
	console.mode = consoleDiff
	return console, nil
}

// patchApprovalDiff 现算待审批补丁的前后全文：审批发生在快照生成之前，磁盘
// 现内容即 before，由 ApplyPatch.Plan 以零写入方式给出确定性改写计划。
func (console RunConsole) patchApprovalDiff() ([]string, error) {
	if console.waiting == nil {
		return nil, errors.New("当前无待审批操作")
	}
	inspected, err := console.input.Service.Inspect(console.rootCtx, workspaceRootOrDefault(console.input.Workspace), console.input.StateDir, console.input.Config, console.waiting.RunID, true)
	if err != nil {
		return nil, err
	}
	toolName, arguments, found := proposalArgumentsForOperation(inspected.Events, console.waiting.OperationID)
	if !found {
		return nil, errors.New("未找到待审批操作的提议参数")
	}
	if toolName != "apply_patch" {
		return nil, errors.New("该审批不是补丁写入，无差异可看")
	}
	workspace, err := console.previewWorkspace()
	if err != nil {
		return nil, err
	}
	plan, err := tools.ApplyPatch{Workspace: workspace}.Plan(arguments)
	if err != nil {
		return nil, err
	}
	return renderPlannedPatchDiff(plan, arguments), nil
}

// previewWorkspace 构造补丁差异预览用的工作区视图：与 Boot 装配同一 ignore
// 与读取上限配置来源，保证 Plan 的边界判定与策略侧一致。
func (console RunConsole) previewWorkspace() (tools.Workspace, error) {
	configuration := console.input.Config
	return tools.NewWorkspace(workspaceRootOrDefault(console.input.Workspace), configuration.Workspace.Ignore, configuration.Workspace.MaxReadBytes, configuration.Workspace.MaxSearchFiles, configuration.Workspace.MaxSearchBytes)
}

// proposalArgumentsForOperation 从事件流提取指定操作的工具提议身份与参数。
func proposalArgumentsForOperation(events []domain.Event, operationID domain.OperationID) (string, json.RawMessage, bool) {
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed || event.OperationID == nil || *event.OperationID != operationID {
			continue
		}
		var payload struct {
			ToolName  string          `json:"tool_name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil {
			return "", nil, false
		}
		return payload.ToolName, payload.Arguments, payload.ToolName != "" && len(payload.Arguments) != 0
	}
	return "", nil, false
}

// openTranscriptDiff 在事件查看器中打开光标所在补丁提议的差异视图。
func (console RunConsole) openTranscriptDiff() (tea.Model, tea.Cmd) {
	console.diffLines = console.transcriptPatchDiff()
	console.diffCursor = 0
	console.diffReturn = consoleTranscript
	console.mode = consoleDiff
	return console, nil
}

// transcriptPatchDiff 渲染光标所在补丁提议的差异：before 优先取同一操作
// operation_started 的 planned_before_ref 制品（只读解引用并校验哈希），缺省
// 时按参数如实降级——create 仍可完整渲染，update 只显示无定位替换块。
func (console RunConsole) transcriptPatchDiff() []string {
	if len(console.transcript) == 0 || console.transcriptCursor >= len(console.transcript) {
		return []string{warnStyle.Render("（无事件可看）")}
	}
	event := console.transcript[console.transcriptCursor]
	if event.Type != domain.EventToolCallProposed {
		return []string{warnStyle.Render("（光标行不是工具提议；v 仅对补丁提议有效）")}
	}
	toolName, arguments, found := "", json.RawMessage(nil), false
	if event.OperationID != nil {
		toolName, arguments, found = proposalArgumentsForOperation(console.transcript, *event.OperationID)
	}
	if !found {
		return []string{warnStyle.Render("（提议参数不可解码，无法生成差异）")}
	}
	if toolName != "apply_patch" {
		return []string{warnStyle.Render("（该提议是 " + toolName + "，无补丁差异）")}
	}
	before, beforeKnown := console.beforeSnapshotFor(event)
	lines := renderPatchDiff(arguments, before, beforeKnown)
	// 未生效补丁的差异是提议参数的改写推演而非既成事实，头部如实标注终态。
	if outcome, terminal := operationOutcomeFor(console.transcript, event); terminal && outcome != domain.EventOperationSucceeded && outcome != domain.EventOperationReconciled {
		note := "（该补丁未生效（" + string(outcome) + "），以下为提议参数的改写推演）"
		if len(lines) > 0 {
			lines = append([]string{lines[0], warnStyle.Render(note)}, lines[1:]...)
		} else {
			lines = []string{warnStyle.Render(note)}
		}
	}
	return lines
}

// operationOutcomeFor 在事件查看器内查找同一操作的终态事件；未到终态（等待
// 审批、悬挂）返回 terminal=false。
func operationOutcomeFor(events []domain.Event, proposal domain.Event) (domain.EventType, bool) {
	if proposal.OperationID == nil {
		return "", false
	}
	for _, event := range events {
		if event.OperationID == nil || *event.OperationID != *proposal.OperationID {
			continue
		}
		switch event.Type {
		case domain.EventOperationSucceeded, domain.EventOperationFailed, domain.EventOperationCancelled, domain.EventOperationReconciled:
			return event.Type, true
		}
	}
	return "", false
}

// beforeSnapshotFor 在事件查看器内查找同一操作 operation_started 的 before
// 快照制品并只读解引用；无制品（旧运行或脱敏跳过）或读取失败均如实返回不
// 可用。
func (console RunConsole) beforeSnapshotFor(proposal domain.Event) ([]byte, bool) {
	if proposal.OperationID == nil {
		return nil, false
	}
	for _, event := range console.transcript {
		if event.Type != domain.EventOperationStarted || event.OperationID == nil || *event.OperationID != *proposal.OperationID {
			continue
		}
		var payload struct {
			PlannedBeforeRef *artifact.Ref `json:"planned_before_ref"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.PlannedBeforeRef == nil {
			return nil, false
		}
		content, err := console.input.Service.ReadRunArtifact(workspaceRootOrDefault(console.input.Workspace), console.input.StateDir, console.input.Config, *payload.PlannedBeforeRef)
		if err != nil {
			return nil, false
		}
		return content, true
	}
	return nil, false
}

// proposalLines 按工具类型把提议参数投影为预览行；内容预览截断到固定行数
// 与字符宽度，完整参数以事件日志与制品为准。
