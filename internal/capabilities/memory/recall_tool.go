// [INPUT]: 依赖实例召回回调与固定参数词表。
// [OUTPUT]: 提供MemoryRecall工具规格、校验和只读执行。
// [POS]: 工具只返回证据账本，工作区由应用绑定。
// [PROTOCOL]: Update this header on change, then check AGENTS.md

// Package memory 承载受控记忆能力。
// Author: fxbin
package memory

import (
	"context"
	"fmt"
	"time"

	"encoding/json"
	"github.com/fxbin/cairn/internal/contract"
	"github.com/fxbin/cairn/internal/ports"
)

// MemoryRecallRequest 是模型可见的召回参数；不含工作区、正文或自由文本。
type MemoryRecallRequest struct {
	TaskPromptDigest    string     `json:"task_prompt_digest,omitempty"`
	Outcomes            []string   `json:"outcomes,omitempty"`
	FailureClasses      []string   `json:"failure_classes,omitempty"`
	TrustedVerification string     `json:"trusted_verification,omitempty"`
	CreatedFrom         *time.Time `json:"created_from,omitempty"`
	CreatedUntil        *time.Time `json:"created_until,omitempty"`
	Limit               int        `json:"limit,omitempty"`
	Precision           string     `json:"precision,omitempty"`
}

// MemoryRecall 只读召回工具。
type MemoryRecall struct {
	Explain MemoryExplainFunc
}

// Spec 返回模型可见工具声明。
func (MemoryRecall) Spec() ports.ToolSpec {
	return ports.ToolSpec{
		Name:        "memory_recall",
		Description: "Read-only recall of structured run_outcome memory for the current workspace. Closed filters only: outcomes, failure_classes, trusted_verification, UTC time window, limit, precision. No free text, no keywords, no write/revoke/promote. Server forces the workspace scope.",
		Parameters: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{
			"task_prompt_digest":{"type":"string"},
			"outcomes":{"type":"array","items":{"type":"string","enum":["completed","failed","cancelled"]}},
			"failure_classes":{"type":"array","items":{"type":"string"}},
			"trusted_verification":{"type":"string","enum":["any","required","not_required"]},
			"created_from":{"type":"string"},
			"created_until":{"type":"string"},
			"limit":{"type":"integer","minimum":1,"maximum":50},
			"precision":{"type":"string","enum":["compact","trace"]}
		}}`),
	}
}

// SideEffect 声明为无副作用，可进只读并行组。
func (MemoryRecall) SideEffect() contract.SideEffectClass { return contract.SideEffectNone }

// Execute 执行只读召回；未装配回调时失败关闭。
func (tool MemoryRecall) Execute(ctx context.Context, raw json.RawMessage) contract.ToolResult {
	explain := tool.Explain
	if explain == nil {
		return contract.ToolFailure("memory_recall_unavailable", "memory recall is not configured for this runtime")
	}
	var request MemoryRecallRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return contract.ToolFailure("invalid_arguments", "memory_recall arguments are not valid JSON")
	}
	if err := validateMemoryRecallRequest(request); err != nil {
		return contract.ToolFailure("invalid_arguments", err.Error())
	}
	data, err := explain(ctx, request)
	if err != nil {
		return contract.ToolFailure("memory_recall_failed", err.Error())
	}
	return contract.ToolSuccess(data, "memory evidence ledger", int64(len(data)), "", false)
}

// validateMemoryRecallRequest 校验封闭字段取值；未知或越界失败关闭。
func validateMemoryRecallRequest(request MemoryRecallRequest) error {
	switch request.TrustedVerification {
	case "", "any", "required", "not_required":
	default:
		return fmt.Errorf("unsupported trusted_verification %q", request.TrustedVerification)
	}
	switch request.Precision {
	case "", "compact", "trace":
	default:
		return fmt.Errorf("unsupported precision %q", request.Precision)
	}
	for _, outcome := range request.Outcomes {
		switch outcome {
		case "completed", "failed", "cancelled":
		default:
			return fmt.Errorf("unsupported outcome %q", outcome)
		}
	}
	if request.Limit < 0 || request.Limit > 50 {
		return fmt.Errorf("limit out of range")
	}
	return nil
}
