// Package app 的父子运行委派：在同一 Journal 内创建带 parent_run_id 的子 run
// 并发射 subagent_delegated/completed 事实；不引入 Team/daemon/RPC。
// Author: fxbin
package app

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
)

// SubagentBudget 上限由父运行裁剪；超限失败关闭，不静默扩大。
type SubagentBudget struct {
	MaxTurns  int   `json:"max_turns,omitempty"`
	MaxTokens int64 `json:"max_tokens,omitempty"`
}

// DelegateOptions 描述一次子任务委派：任务摘要与预算上限写入 Journal。
type DelegateOptions struct {
	ParentRunID domain.RunID
	ChildRunID  domain.RunID
	Task        string
	Budget      SubagentBudget
}

// ErrInvalidDelegation 表示委派参数不满足合同（空标识、自派发或摘要缺失）。
var ErrInvalidDelegation = errors.New("invalid subagent delegation")

// TaskDigest 计算任务摘要，Journal 只保留摘要不复制任务全文。
func TaskDigest(task string) string {
	sum := sha256.Sum256([]byte(task))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidateDelegate 校验委派参数：父子标识非空且不相同，任务摘要非空。
func ValidateDelegate(options DelegateOptions) error {
	if options.ParentRunID == "" || options.ChildRunID == "" {
		return fmt.Errorf("%w: parent and child run ids are required", ErrInvalidDelegation)
	}
	if options.ParentRunID == options.ChildRunID {
		return fmt.Errorf("%w: child must differ from parent", ErrInvalidDelegation)
	}
	if options.Budget.MaxTurns < 0 || options.Budget.MaxTokens < 0 {
		return fmt.Errorf("%w: budget must not be negative", ErrInvalidDelegation)
	}
	if strings.TrimSpace(options.Task) == "" {
		return fmt.Errorf("%w: task summary is required", ErrInvalidDelegation)
	}
	return nil
}

// DelegatedPayload 是 subagent_delegated 的可回放载荷。
type DelegatedPayload struct {
	ChildRunID string         `json:"child_run_id"`
	TaskDigest string         `json:"task_digest"`
	Budget     SubagentBudget `json:"budget,omitempty"`
}

// CompletedPayload 是 subagent_completed 的可回放载荷。
type CompletedPayload struct {
	ChildRunID string          `json:"child_run_id"`
	Outcome    string          `json:"outcome"`
	Usage      json.RawMessage `json:"usage,omitempty"`
}

// EncodeDelegated 序列化委派载荷；失败时返回错误由调用方拒绝追加。
func EncodeDelegated(options DelegateOptions) (json.RawMessage, error) {
	if err := ValidateDelegate(options); err != nil {
		return nil, err
	}
	payload, err := json.Marshal(DelegatedPayload{
		ChildRunID: string(options.ChildRunID),
		TaskDigest: TaskDigest(options.Task),
		Budget:     options.Budget,
	})
	if err != nil {
		return nil, fmt.Errorf("encode subagent_delegated: %w", err)
	}
	raw, err := artifact.EncodeRedactedJSON(json.RawMessage(payload))
	if err != nil {
		return nil, err
	}
	return raw, nil
}

// EncodeCompleted 序列化完成载荷；outcome 必填且 child_run_id 与委派一致。
func EncodeCompleted(childRunID domain.RunID, outcome string, usage json.RawMessage) (json.RawMessage, error) {
	if childRunID == "" || strings.TrimSpace(outcome) == "" {
		return nil, fmt.Errorf("%w: child_run_id and outcome are required", ErrInvalidDelegation)
	}
	payload, err := json.Marshal(CompletedPayload{
		ChildRunID: string(childRunID),
		Outcome:    outcome,
		Usage:      usage,
	})
	if err != nil {
		return nil, fmt.Errorf("encode subagent_completed: %w", err)
	}
	raw, err := artifact.EncodeRedactedJSON(json.RawMessage(payload))
	if err != nil {
		return nil, err
	}
	return raw, nil
}
