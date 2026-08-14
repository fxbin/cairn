// Package agent 包含串行、单写入者的 Agent 执行循环。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// Loop 协调模型、策略、工具、事件日志、恢复和固定验证流程。
type Loop struct {
	Journal                    ports.Journal
	Provider                   ports.Provider
	Tools                      *tools.Registry
	Gate                       *policy.Gate
	Model                      string
	ConfigFingerprint          string
	MaxTurns                   int
	MaxToolCalls               int
	MaxInputTokens             int
	MaxOutputTokens            int
	MaxCostUSD                 float64
	InputTokenUSD              float64
	OutputTokenUSD             float64
	ContextAutoCompact         bool
	ContextMaxPromptTokens     int
	ContextMaxSummaryBytes     int
	VerificationAutoAfterPatch bool
	Verification               []VerificationCommand
	// VerificationWorkspace 与工具工作区刻意不同：普通 workspace.ignore
	// 不能掩盖 verifier 产生的副作用。
	VerificationWorkspace tools.Workspace
	Artifacts             *artifact.Store
	OnEvent               EventSink
	SystemPrompt          string
	// startedOperations 与 startedOperationsKnown 增量维护已启动操作计数，
	// 供工具调用预算检查复用，避免每次工具调用都全量重载事件日志。
	startedOperations      int
	startedOperationsKnown bool
}

// RunInput 是启动或恢复 Loop 所需的运行标识、任务与恢复事实。
type RunInput struct {
	RunID             domain.RunID
	Task              string
	WorkspaceHash     string
	ResumeInvocation  string
	ResumeFromSeq     uint64
	ResumeObservation string
}

// Result 返回 Loop 完成或暂停后的运行标识与最终回复。
type Result struct {
	RunID    domain.RunID
	Response string
}

// Run 从当前事件投影继续执行模型轮次、工具与验证，直至终态或可恢复暂停。
func (loop *Loop) Run(ctx context.Context, input RunInput) (Result, error) {
	if loop.Journal == nil || loop.Provider == nil || loop.Tools == nil || loop.Gate == nil {
		return Result{}, errors.New("agent loop dependencies and policy gate are required")
	}
	if input.RunID == "" || input.Task == "" || input.WorkspaceHash == "" {
		return Result{}, errors.New("run ID, task, and workspace hash are required")
	}
	if loop.MaxTurns <= 0 {
		loop.MaxTurns = fallbackMaxTurns
	}
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return loop.persistCancellation(ctx, &projection, input, projection.CurrentTurn)
	}
	if projection.State != domain.RunRunning {
		return Result{}, fmt.Errorf("run %s is not runnable from state %s", input.RunID, projection.State)
	}
	messages := loop.messagesFromEvents(events, input.Task)
	messages, err = loop.compactContextIfNeeded(ctx, &projection, input, messages)
	if err != nil {
		return Result{}, err
	}
	usage := budgetUsageFromEvents(events)
	malformedToolArgumentFailures := malformedToolArgumentsFailureCount(events)
	usedTurns := openedTurnCount(events)
	for turn := usedTurns; turn < loop.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, &projection, input, nil)
		}
		turnID := domain.NewTurnID()
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID, Payload: mustJSON(struct {
			Turn int `json:"turn"`
		}{Turn: turn + 1})}); err != nil {
			return Result{}, err
		}
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, &projection, input, &turnID)
		}
		attemptID := domain.NewAttemptID()
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelRequestPayload{Provider: "openai_compatible", Model: loop.Model})}); err != nil {
			return Result{}, err
		}
		response, err := loop.callModel(ctx, messages)
		if err != nil {
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			if errors.Is(err, ports.ErrMalformedToolCallArguments) {
				if appendErr := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(struct {
					Error      string `json:"error"`
					ErrorClass string `json:"error_class"`
					Retryable  bool   `json:"retryable"`
				}{Error: ports.ErrMalformedToolCallArguments.Error(), ErrorClass: malformedToolArgumentsErrorClass, Retryable: malformedToolArgumentFailures == 0})}); appendErr != nil {
					return Result{}, appendErr
				}
				if malformedToolArgumentFailures == 0 {
					malformedToolArgumentFailures++
					messages = append(messages, ports.Message{Role: "user", Content: malformedToolArgumentsFeedback})
					messages, err = loop.closeTurnAndCompact(ctx, &projection, input, turnID, messages)
					if err != nil {
						return Result{}, err
					}
					continue
				}
				if appendErr := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})}); appendErr != nil {
					return Result{}, appendErr
				}
				if appendErr := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
					Error      string `json:"error"`
					ErrorClass string `json:"error_class"`
				}{Error: ports.ErrMalformedToolCallArguments.Error(), ErrorClass: malformedToolArgumentsErrorClass})}); appendErr != nil {
					return Result{}, appendErr
				}
				return Result{}, fmt.Errorf("model request: %w", ports.ErrMalformedToolCallArguments)
			}
			_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelRequestFailedPayload{Error: err.Error()})})
			_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})})
			_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(modelRequestFailedPayload{Error: err.Error()})})
			return Result{}, fmt.Errorf("model request: %w", err)
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelResponseFinished, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelResponsePayload{Text: response.Text, ToolCalls: response.ToolCalls, Usage: response.Usage})}); err != nil {
			return Result{}, err
		}
		usage.add(response.Usage)
		if exceeded := loop.responseBudgetExceeded(usage); exceeded != nil {
			return loop.persistBudgetExceeded(ctx, &projection, input, exceeded.Dimension)
		}
		messages = append(messages, ports.Message{Role: "assistant", Content: response.Text, ToolCalls: response.ToolCalls})
		if len(response.ToolCalls) == 0 {
			if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventAssistantMessageAppended, TurnID: &turnID, Payload: mustJSON(domain.AssistantMessagePayload{Text: response.Text})}); err != nil {
				return Result{}, err
			}
			if err := loop.runVerification(ctx, &projection, input, turnID); err != nil {
				if ctx.Err() != nil {
					return loop.persistCancellation(ctx, &projection, input, &turnID)
				}
				var waiting *WaitingApprovalError
				if errors.As(err, &waiting) {
					return Result{RunID: input.RunID}, waiting
				}
				var budget *BudgetExceededError
				if errors.As(err, &budget) {
					return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
				}
				var guard *LoopGuardExceededError
				if errors.As(err, &guard) {
					return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
				}
				var verificationFailure *VerificationFailureError
				if errors.As(err, &verificationFailure) {
					messages = append(messages, ports.Message{Role: "user", Content: verificationFailure.Feedback})
					messages, err = loop.closeTurnAndCompact(ctx, &projection, input, turnID, messages)
					if err != nil {
						return Result{RunID: input.RunID}, err
					}
					continue
				}
				_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})})
				_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
					Error string `json:"error"`
				}{Error: err.Error()})})
				return Result{RunID: input.RunID}, err
			}
			if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
				return Result{}, err
			}
			if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: response.Text})}); err != nil {
				return Result{}, err
			}
			return Result{RunID: input.RunID, Response: response.Text}, nil
		}
		patched := false
		allToolCallsSucceeded := true
		for _, call := range response.ToolCalls {
			toolResult, err := loop.executeTool(ctx, &projection, input, turnID, call)
			if err != nil {
				if ctx.Err() != nil {
					return loop.persistCancellation(ctx, &projection, input, &turnID)
				}
				var budget *BudgetExceededError
				if errors.As(err, &budget) {
					return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
				}
				var guard *LoopGuardExceededError
				if errors.As(err, &guard) {
					return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
				}
				var waiting *WaitingApprovalError
				if errors.As(err, &waiting) {
					return Result{RunID: input.RunID}, waiting
				}
				var denied *PolicyDeniedError
				if errors.As(err, &denied) {
					_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})})
					_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
						Error string `json:"error"`
					}{Error: denied.Error()})})
				}
				if !errors.As(err, &denied) {
					return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
				}
				return Result{RunID: input.RunID}, err
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			encoded, err := json.Marshal(toolResult)
			if err != nil {
				return Result{}, err
			}
			patched = patched || call.Name == "apply_patch" && toolResult.OK
			allToolCallsSucceeded = allToolCallsSucceeded && toolResult.OK
			messages = append(messages, ports.Message{Role: "tool", ToolCallID: call.ID, Content: string(encoded)})
		}
		if loop.VerificationAutoAfterPatch && patched && allToolCallsSucceeded {
			result, feedback, err := loop.finishAutomaticVerification(ctx, &projection, input, turnID)
			if err != nil || feedback == "" {
				return result, err
			}
			messages = append(messages, ports.Message{Role: "user", Content: feedback})
			messages, err = loop.closeTurnAndCompact(ctx, &projection, input, turnID, messages)
			if err != nil {
				return Result{}, err
			}
			continue
		}
		messages, err = loop.closeTurnAndCompact(ctx, &projection, input, turnID, messages)
		if err != nil {
			return Result{RunID: input.RunID}, err
		}
	}
	if ctx.Err() != nil {
		return loop.persistCancellation(ctx, &projection, input, nil)
	}
	return loop.persistBudgetExceeded(ctx, &projection, input, "turns")
}

// closeTurnAndCompact 写入继续轮次的关闭事件，并在需要时生成上下文检查点。
func (loop *Loop) closeTurnAndCompact(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, messages []ports.Message) ([]ports.Message, error) {
	if err := loop.closeContinuationTurn(ctx, projection, input, turnID); err != nil {
		return nil, err
	}
	return loop.compactContextIfNeeded(ctx, projection, input, messages)
}
