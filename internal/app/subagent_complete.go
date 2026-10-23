// Package app 的子运行终态回写只依据已持久化的子运行状态追加父运行事实。
// 审批、对账暂停和创建失败都不是完成；回写失败必须由调用方显式上报。
// Author: fxbin
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// subagentCompletionTimeout 限制终态落盘后的父运行通知，独立于已取消的模型调用上下文。
const subagentCompletionTimeout = 5 * time.Second

// SubagentUsage 是回写父运行的可选用量摘要。
type SubagentUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// NotifySubagentCompleted 校验子运行的终态与父身份后回写；非终态不发射完成事实。
func NotifySubagentCompleted(ctx context.Context, journal ports.Journal, workspaceHash string, options RunOptions, childRunID domain.RunID, outcome string, usage *SubagentUsage, occurredAt time.Time) error {
	if options.ParentRunID == "" {
		return nil
	}
	if journal == nil {
		return fmt.Errorf("%w: journal is required", ErrSubagentAppend)
	}
	events, err := journal.Load(ctx, childRunID)
	if err != nil {
		return fmt.Errorf("%w: load child: %w", ErrSubagentAppend, err)
	}
	if len(events) == 0 {
		return fmt.Errorf("%w: child run is missing", ErrSubagentAppend)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return fmt.Errorf("%w: replay child: %w", ErrSubagentAppend, err)
	}
	if projection.ParentRunID != options.ParentRunID || projection.WorkspaceHash != workspaceHash {
		return fmt.Errorf("%w: child identity does not match", ErrSubagentAppend)
	}
	if !projection.Terminal() {
		return nil
	}
	if outcome != subagentRunOutcome(projection.State) {
		return fmt.Errorf("%w: child outcome does not match Journal", ErrSubagentAppend)
	}
	var raw json.RawMessage
	if usage != nil {
		if usage.PromptTokens < 0 || usage.CompletionTokens < 0 {
			return fmt.Errorf("%w: usage must not be negative", ErrSubagentAppend)
		}
		raw, err = json.Marshal(usage)
		if err != nil {
			return fmt.Errorf("%w: encode usage: %w", ErrSubagentAppend, err)
		}
	}
	_, err = RecordSubagentCompleted(ctx, journal, options.ParentRunID, workspaceHash, childRunID, outcome, raw, occurredAt)
	return err
}

// subagentRunOutcome 把已确认的 Journal 终态映射为结果短语；非终态不生成结果。
func subagentRunOutcome(state domain.RunState) string {
	switch state {
	case domain.RunCompleted:
		return "ok"
	case domain.RunFailed:
		return "failed"
	case domain.RunCancelled:
		return "cancelled"
	default:
		return ""
	}
}

// MapRunOutcome 仅映射已确认终态之后的调用错误；不得用它判断运行是否已经结束。
func MapRunOutcome(err error, _ string) string {
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	if err != nil {
		return "failed"
	}
	return "ok"
}
