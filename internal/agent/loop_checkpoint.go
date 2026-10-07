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
// 已启用、本轮关闭后仍无 apply_patch 提议、已有读_file 证据、已完成轮次达到
// 阈值、且本 run 尚未插入过。成功时持久化 verification_feedback 事件并把
// 同一文本追加进消息；失败关闭时原样返回消息，不中断运行。
func (loop *Loop) maybeDecisionCheckpoint(ctx context.Context, projection *domain.RunProjection, input RunInput, messages []ports.Message) ([]ports.Message, error) {
	if !loop.DecisionCheckpointEnabled {
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
	if openedTurnCount(events) < minTurns {
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

// hasReadFileEvidence 判断历史中是否已有 read_file 提议（模型已请求读取源码）。
func hasReadFileEvidence(events []domain.Event) bool {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload struct {
			ToolName string `json:"tool_name"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.ToolName == "read_file" {
			return true
		}
	}
	return false
}
