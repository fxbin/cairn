// Package app 的父子委派 Journal 发射在已有父运行内追加已校验事实。
// 调用方必须持有该 Journal 的串行写入权；序号冲突失败关闭，不自动重试。
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

// ErrSubagentAppend 表示委派事件无法安全追加（父运行缺失、序号冲突或载荷非法）。
var ErrSubagentAppend = errors.New("subagent journal append failed")

// RecordSubagentDelegated 向已有父运行追加委派事实，拒绝重复子身份与父身份不一致。
func RecordSubagentDelegated(ctx context.Context, journal ports.Journal, parentRunID domain.RunID, workspaceHash string, options DelegateOptions, occurredAt time.Time) (domain.Event, error) {
	if options.ParentRunID == "" {
		options.ParentRunID = parentRunID
	}
	draft, err := DelegatedEventDraft(parentRunID, workspaceHash, options, occurredAt)
	if err != nil {
		return domain.Event{}, fmt.Errorf("%w: %w", ErrSubagentAppend, err)
	}
	projection, events, err := subagentParent(ctx, journal, parentRunID, workspaceHash)
	if err != nil {
		return domain.Event{}, err
	}
	if existing, exists := projection.Children[string(options.ChildRunID)]; exists {
		// 幂等：相同子身份已委派时返回既有事实，不重复追加。
		if existing.State == "delegated" || existing.State == "completed" {
			for index := len(events) - 1; index >= 0; index-- {
				if events[index].Type != domain.EventSubagentDelegated {
					continue
				}
				var payload DelegatedPayload
				if err := json.Unmarshal(events[index].Payload, &payload); err != nil {
					continue
				}
				if payload.ChildRunID == string(options.ChildRunID) {
					return events[index], nil
				}
			}
		}
		return domain.Event{}, fmt.Errorf("%w: child is already delegated", ErrSubagentAppend)
	}
	return appendSubagentEvent(ctx, journal, projection, draft)
}

// RecordSubagentCompleted 只收束已委派的子身份；相同完成事实幂等返回，不覆写历史结果。
func RecordSubagentCompleted(ctx context.Context, journal ports.Journal, parentRunID domain.RunID, workspaceHash string, childRunID domain.RunID, outcome string, usage json.RawMessage, occurredAt time.Time) (domain.Event, error) {
	draft, err := CompletedEventDraft(parentRunID, workspaceHash, childRunID, outcome, usage, occurredAt)
	if err != nil {
		return domain.Event{}, fmt.Errorf("%w: %w", ErrSubagentAppend, err)
	}
	projection, events, err := subagentParent(ctx, journal, parentRunID, workspaceHash)
	if err != nil {
		return domain.Event{}, err
	}
	child, exists := projection.Children[string(childRunID)]
	if !exists {
		return domain.Event{}, fmt.Errorf("%w: child was not delegated", ErrSubagentAppend)
	}
	if child.State == "completed" {
		if child.Outcome != outcome {
			return domain.Event{}, fmt.Errorf("%w: child completion conflicts with history", ErrSubagentAppend)
		}
		for index := len(events) - 1; index >= 0; index-- {
			if events[index].Type != domain.EventSubagentCompleted {
				continue
			}
			var payload CompletedPayload
			if err := json.Unmarshal(events[index].Payload, &payload); err != nil {
				return domain.Event{}, fmt.Errorf("%w: invalid completed payload", ErrSubagentAppend)
			}
			if payload.ChildRunID == string(childRunID) {
				return events[index], nil
			}
		}
		return domain.Event{}, fmt.Errorf("%w: child completion is missing", ErrSubagentAppend)
	}
	return appendSubagentEvent(ctx, journal, projection, draft)
}

// subagentParent 回放已有父运行并绑定工作区；缺失或损坏历史不得创建残缺事件链。
func subagentParent(ctx context.Context, journal ports.Journal, runID domain.RunID, workspaceHash string) (domain.RunProjection, []domain.Event, error) {
	if journal == nil {
		return domain.RunProjection{}, nil, fmt.Errorf("%w: journal is required", ErrSubagentAppend)
	}
	events, err := journal.Load(ctx, runID)
	if err != nil {
		return domain.RunProjection{}, nil, fmt.Errorf("%w: load parent: %w", ErrSubagentAppend, err)
	}
	if len(events) == 0 {
		return domain.RunProjection{}, nil, fmt.Errorf("%w: parent run is missing", ErrSubagentAppend)
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return domain.RunProjection{}, nil, fmt.Errorf("%w: replay parent: %w", ErrSubagentAppend, err)
	}
	if projection.ID != runID || projection.WorkspaceHash != workspaceHash {
		return domain.RunProjection{}, nil, fmt.Errorf("%w: parent identity does not match", ErrSubagentAppend)
	}
	return projection, events, nil
}

// appendSubagentEvent 以当前末序号作为 CAS 期望值，并在写入前验证完整投影迁移。
func appendSubagentEvent(ctx context.Context, journal ports.Journal, projection domain.RunProjection, draft domain.EventDraft) (domain.Event, error) {
	if err := projection.ValidateDraft(draft); err != nil {
		return domain.Event{}, fmt.Errorf("%w: validate event: %w", ErrSubagentAppend, err)
	}
	event, err := journal.Append(ctx, projection.LastSeq, draft)
	if err != nil {
		return domain.Event{}, fmt.Errorf("%w: %w", ErrSubagentAppend, err)
	}
	return event, nil
}

// validateSubagentRunParent 在创建子运行和调用模型前核对已有委派，避免执行后才发现归属或预算不符。
func validateSubagentRunParent(ctx context.Context, journal ports.Journal, workspaceHash string, runID domain.RunID, options RunOptions) error {
	if options.ParentRunID == "" {
		return nil
	}
	if options.ParentRunID == runID {
		return fmt.Errorf("%w: child must differ from parent", ErrSubagentAppend)
	}
	projection, events, err := subagentParent(ctx, journal, options.ParentRunID, workspaceHash)
	if err != nil {
		return err
	}
	child, exists := projection.Children[string(runID)]
	if !exists || child.State != "delegated" {
		return fmt.Errorf("%w: child must have an unfinished delegation", ErrSubagentAppend)
	}
	for index := len(events) - 1; index >= 0; index-- {
		if events[index].Type != domain.EventSubagentDelegated {
			continue
		}
		var delegated DelegatedPayload
		if err := json.Unmarshal(events[index].Payload, &delegated); err != nil {
			return fmt.Errorf("%w: invalid delegated payload", ErrSubagentAppend)
		}
		if delegated.ChildRunID != string(runID) {
			continue
		}
		if delegated.Budget.MaxTurns < 0 || delegated.Budget.MaxTokens < 0 {
			return fmt.Errorf("%w: historical delegated budget must not be negative", ErrSubagentAppend)
		}
		if delegated.TaskDigest != TaskDigest(options.Task) {
			return fmt.Errorf("%w: delegated task does not match", ErrSubagentAppend)
		}
		if delegated.Budget.MaxTurns > 0 && options.Config.Budget.MaxTurns > delegated.Budget.MaxTurns {
			return fmt.Errorf("%w: child turn budget exceeds delegation", ErrSubagentAppend)
		}
		if maximum := delegated.Budget.MaxTokens; maximum > 0 {
			input, output := int64(options.Config.Budget.MaxInputTokens), int64(options.Config.Budget.MaxOutputTokens)
			if input > maximum || output > maximum-input {
				return fmt.Errorf("%w: child token budget exceeds delegation", ErrSubagentAppend)
			}
		}
		// 父预算扣减：子预算不得超出委派额度，且与父配置余量交叉校验。
		parentRemaining := NewBudgetRemaining(options.Config.Budget)
		childNeed := BudgetRemaining{
			MaxTurns:        delegated.Budget.MaxTurns,
			MaxInputTokens:  int64(options.Config.Budget.MaxInputTokens),
			MaxOutputTokens: int64(options.Config.Budget.MaxOutputTokens),
		}
		if _, err := parentRemaining.Deduct(childNeed); err != nil {
			return fmt.Errorf("%w: %w", ErrSubagentAppend, err)
		}
		return nil
	}
	return fmt.Errorf("%w: delegation fact is missing", ErrSubagentAppend)
}
