// Package app 的父子委派事件草稿：把已校验载荷组装为可追加的 Journal 事件
// 草稿；不在此层做 I/O，调用方持有 Loop/Journal 写入权。
// Author: fxbin
package app

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/fxbin/cairn/internal/domain"
)

// DelegatedEventDraft 为父运行组装 subagent_delegated 事件草稿。
// 子 run 必须已分配标识；载荷只含任务摘要与预算，不含任务全文。
func DelegatedEventDraft(parentRunID domain.RunID, workspaceHash string, options DelegateOptions, occurredAt time.Time) (domain.EventDraft, error) {
	if parentRunID == "" {
		return domain.EventDraft{}, fmt.Errorf("%w: parent run id is required", ErrInvalidDelegation)
	}
	if options.ParentRunID != parentRunID {
		return domain.EventDraft{}, fmt.Errorf("%w: parent run identity does not match", ErrInvalidDelegation)
	}
	payload, err := EncodeDelegated(options)
	if err != nil {
		return domain.EventDraft{}, err
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return domain.EventDraft{
		RunID:         parentRunID,
		WorkspaceHash: workspaceHash,
		OccurredAt:    occurredAt,
		Type:          domain.EventSubagentDelegated,
		Payload:       payload,
	}, nil
}

// CompletedEventDraft 为父运行组装 subagent_completed 事件草稿。
// outcome 与 child_run_id 必填；usage 可省略。
func CompletedEventDraft(parentRunID domain.RunID, workspaceHash string, childRunID domain.RunID, outcome string, usage json.RawMessage, occurredAt time.Time) (domain.EventDraft, error) {
	if parentRunID == "" {
		return domain.EventDraft{}, fmt.Errorf("%w: parent run id is required", ErrInvalidDelegation)
	}
	if childRunID == parentRunID {
		return domain.EventDraft{}, fmt.Errorf("%w: child must differ from parent", ErrInvalidDelegation)
	}
	payload, err := EncodeCompleted(childRunID, outcome, usage)
	if err != nil {
		return domain.EventDraft{}, err
	}
	if occurredAt.IsZero() {
		occurredAt = time.Now().UTC()
	}
	return domain.EventDraft{
		RunID:         parentRunID,
		WorkspaceHash: workspaceHash,
		OccurredAt:    occurredAt,
		Type:          domain.EventSubagentCompleted,
		Payload:       payload,
	}, nil
}
