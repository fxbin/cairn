// loop_checkpoint.go 实现软决策检查点：在仍无补丁提议且已有读取证据时，
// 向模型插入一次说明性反馈（事件可回放），要求交代路径/假设/缺口/下一步。
// 它绝不强制写入，也绝不携带私有 verifier 或参考解。
//
// Author: fxbin
package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// decisionCheckpointPrefix 标记检查点反馈，便于回放去重与审计检索。
const decisionCheckpointPrefix = "[决策检查点]"

// decisionCheckpointSummary 是压缩时优先保留的固定提醒；历史反馈不授权写入或确认完成。
const decisionCheckpointSummary = "[决策检查点] 已触发收敛提醒，不授权写入，也不确认完成。保持原始任务和权限范围；只读任务继续只读。已有证据足够时采取任务要求的最小可验证动作，否则只定向补齐具体证据缺口。"

// decisionCheckpointSummaryMaxBytes 限制固定反馈的摘要占用，避免挤占全部事实预算。
const decisionCheckpointSummaryMaxBytes = 480

// decisionCheckpointCompactionNote 在反馈已存在时保留固定范围约束，不复制用户或模型的任意说明。
func decisionCheckpointCompactionNote(messages []ports.Message, maximumBytes int) string {
	for _, message := range messages {
		if message.Role != "user" {
			continue
		}
		if !strings.HasPrefix(message.Content, decisionCheckpointPrefix) && !(strings.HasPrefix(message.Content, contextCheckpointPrefix) && strings.Contains(message.Content, "\n"+decisionCheckpointPrefix)) {
			continue
		}
		note := clipText(decisionCheckpointSummary, min(maximumBytes-len("\n"), decisionCheckpointSummaryMaxBytes))
		if note == "" {
			return ""
		}
		return note + "\n"
	}
	return ""
}

// decisionCheckpointPrompt 是插入模型上下文的反馈正文。措辞刻意开放：
// 允许回答“证据不足”，不要求也不暗示必须立刻补丁。
const decisionCheckpointPrompt = `[决策检查点] 尚未见 apply_patch 提议。请保持原始任务范围，仅基于已读事实收敛下一步（可答“证据不足”）：
1) 已确认的代码路径与定位；
2) 若任务需要修改，最小可验证的修改假设；若原任务只读，继续保持只读；
3) 真正仍缺的证据，不要仅为罗列所有相关文件继续泛读；
4) 下一步具体动作及工具、目标路径或读取范围。
已有事实足够时，选择与原任务一致的局部动作；证据不足时只定向读取该缺口。
这不是任务完成确认，不得仅因收到检查点而宣布完成或写入。不要猜测未读内容，也不要重构原文空白后提交未经确认的补丁。`

// maybeDecisionCheckpoint 在满足条件时插入一次软决策检查点。条件：
// 已启用、关闭边界无 pending、仍无 apply_patch 提议、已有成功读取回执、轮次达到
// 阈值且仍有预算、尚未插入过。日志读取失败时跳过；反馈持久化失败返回错误，
// 已关闭的事实留待恢复。成功时事件与模型消息使用同一反馈正文。
func (loop *Loop) maybeDecisionCheckpoint(ctx context.Context, projection *domain.RunProjection, input RunInput, messages []ports.Message) ([]ports.Message, error) {
	if !loop.DecisionCheckpointEnabled || projection.State != domain.RunRunning || projection.CurrentTurn != nil || projection.PendingApproval != nil || projection.PendingOperation != nil {
		return messages, nil
	}
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return messages, nil
	}
	if decisionCheckpointAlreadyFired(events) {
		return messages, nil
	}
	if hasApplyPatchProposal(events) {
		return messages, nil
	}
	if !hasReadFileEvidence(events) {
		return messages, nil
	}
	minTurns := loop.DecisionCheckpointMinTurns
	if minTurns <= 0 {
		minTurns = 4
	}
	turns := openedTurnCount(events)
	if turns < minTurns || loop.MaxTurns > 0 && turns >= loop.MaxTurns {
		return messages, nil
	}
	turnID := domain.NewTurnID()
	if err := loop.append(ctx, projection, domain.EventDraft{
		RunID:         input.RunID,
		WorkspaceHash: input.WorkspaceHash,
		Type:          domain.EventVerificationFeedbackAppended,
		TurnID:        &turnID,
		Payload: mustJSON(struct {
			Text string `json:"text"`
		}{Text: decisionCheckpointPrompt}),
	}); err != nil {
		return messages, fmt.Errorf("persist decision checkpoint: %w", err)
	}
	return append(messages, ports.Message{Role: "user", Content: decisionCheckpointPrompt}), nil
}

// decisionCheckpointAlreadyFired 判断本 run 是否已插入过检查点。
func decisionCheckpointAlreadyFired(events []domain.Event) bool {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventVerificationFeedbackAppended {
			continue
		}
		var payload struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && strings.HasPrefix(payload.Text, decisionCheckpointPrefix) {
			return true
		}
	}
	return false
}

// hasApplyPatchProposal 判断历史中是否已出现 apply_patch 工具提议。
func hasApplyPatchProposal(events []domain.Event) bool {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload struct {
			ToolName string `json:"tool_name"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ToolName == "apply_patch" {
			return true
		}
	}
	return false
}

// hasReadFileEvidence 只接受已成功完成且追加了成功回执的 read_file，拒绝将提议、拒绝或未完成操作当作已读事实。
func hasReadFileEvidence(events []domain.Event) bool {
	reads := make(map[domain.OperationID]bool)
	for _, event := range events {
		if event.OperationID == nil {
			continue
		}
		switch event.Type {
		case domain.EventToolCallProposed:
			var payload struct {
				ToolName string `json:"tool_name"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.ToolName == "read_file" {
				reads[*event.OperationID] = false
			}
		case domain.EventOperationSucceeded:
			if _, exists := reads[*event.OperationID]; exists {
				reads[*event.OperationID] = true
			}
		case domain.EventToolResultAppended:
			var payload struct {
				OK bool `json:"ok"`
			}
			if reads[*event.OperationID] && json.Unmarshal(event.Payload, &payload) == nil && payload.OK {
				return true
			}
		}
	}
	return false
}
