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
	Journal                        ports.Journal
	Provider                       ports.Provider
	Tools                          *tools.Registry
	Gate                           *policy.Gate
	Model                          string
	ConfigFingerprint              string
	MaxTurns                       int
	MaxToolCalls                   int
	MaxInputTokens                 int
	MaxOutputTokens                int
	MaxCostUSD                     float64
	InputTokenUSD                  float64
	OutputTokenUSD                 float64
	RequireProviderUsage           bool
	AllowedProviderStopReasons     []string
	ContextAutoCompact             bool
	ContextMaxPromptTokens         int
	ContextMaxSummaryBytes         int
	ContextInventoryBudgetPercent  int
	ContextThresholdReservePercent int
	VerificationAutoAfterPatch     bool
	Verification                   []VerificationCommand
	// VerificationWorkspace 与工具工作区刻意不同：普通 workspace.ignore
	// 不能掩盖 verifier 产生的副作用。
	VerificationWorkspace tools.Workspace
	Artifacts             *artifact.Store
	OnEvent               EventSink
	SystemPrompt          string
	// 会话前情（ADR-0018）：调用方显式携带时注入消息首位；前情卡制品引用
	// 同时进入创建事件以支持 resume 回放。
	PriorContext    string
	PriorContextRef *artifact.Ref
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
	// 会话链关系（ADR-0018）：调用方显式携带时写入创建事件并作为消息首位
	// 注入前情；空值时运行行为与无会话场景完全一致。
	SessionID       string
	PrevRunID       domain.RunID
	PriorContext    string
	PriorContextRef *artifact.Ref
}

// Result 返回 Loop 完成或暂停后的运行标识与最终回复。
type Result struct {
	RunID    domain.RunID
	Response string
}

// Run 从当前事件投影继续执行模型轮次、工具与验证，直至终态或可恢复暂停。
// 返回错误前统一兜底持久化取消，保证中断信号落在事件追加间隙时也能落盘。
func (loop *Loop) Run(ctx context.Context, input RunInput) (Result, error) {
	result, err := loop.run(ctx, input)
	return result, loop.persistCancellationIfInterrupted(ctx, input, err)
}

// persistCancellationIfInterrupted 在错误为用户取消且 run 仍处 running 时补写
// 持久化取消序列；已处 cancelling 的断点由 Resume 按既有请求确定性收束。
func (loop *Loop) persistCancellationIfInterrupted(ctx context.Context, input RunInput, err error) error {
	if !errors.Is(err, context.Canceled) || ctx.Err() == nil {
		return err
	}
	events, loadErr := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if loadErr != nil {
		return errors.Join(err, fmt.Errorf("load run while persisting cancellation: %w", loadErr))
	}
	projection, replayErr := domain.Replay(events)
	if replayErr != nil {
		return errors.Join(err, fmt.Errorf("replay run while persisting cancellation: %w", replayErr))
	}
	events, projection, repairErr := loop.repairRecoverableOperationGaps(ctx, input, events, projection)
	if repairErr != nil {
		return errors.Join(err, repairErr)
	}
	verificationRepaired, repairErr := loop.finishVerificationForCancellation(ctx, &projection, input, events)
	if repairErr != nil {
		return errors.Join(err, repairErr)
	}
	if verificationRepaired {
		events, loadErr = loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if loadErr != nil {
			return errors.Join(err, fmt.Errorf("reload run after cancelling unstarted verification: %w", loadErr))
		}
		projection, replayErr = domain.Replay(events)
		if replayErr != nil {
			return errors.Join(err, fmt.Errorf("replay run after cancelling unstarted verification: %w", replayErr))
		}
	}
	if hasRecoverableTerminalGap(events) {
		return errors.Join(err, errors.New("run cancellation still has a recoverable terminal gap"))
	}
	if projection.State != domain.RunRunning {
		return err
	}
	if _, cancelErr := loop.persistCancellation(context.WithoutCancel(ctx), &projection, input, projection.CurrentTurn); cancelErr != nil {
		if cancelErr == context.Canceled {
			return err
		}
		return errors.Join(err, cancelErr)
	}
	return context.Canceled
}

// run 执行模型轮次、工具与验证的主循环，直至终态或可恢复暂停。
func (loop *Loop) run(ctx context.Context, input RunInput) (Result, error) {
	// 会话前情装配（ADR-0018）：输入显式携带优先，供消息组装与创建事件
	// 一致引用；resume 场景在 messagesFromEvents 中按事件回放补齐。
	loop.PriorContext = input.PriorContext
	loop.PriorContextRef = input.PriorContextRef
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
	messages, err := loop.messagesFromEvents(events, input.Task)
	if err != nil {
		return Result{}, err
	}
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
			if errors.Is(err, errSensitiveToolArguments) {
				failure := modelRequestFailedPayload{Error: errSensitiveToolArguments.Error(), ErrorClass: "sensitive_tool_arguments"}
				cause := fmt.Errorf("model request: %w", errSensitiveToolArguments)
				if appendErr := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(failure)}); appendErr != nil {
					return Result{RunID: input.RunID}, errors.Join(cause, appendErr)
				}
				return loop.persistRunFailure(ctx, &projection, input, &turnID, runFailurePayload{Error: failure.Error, ErrorClass: failure.ErrorClass}, cause)
			}
			if errors.Is(err, ports.ErrMalformedToolCallArguments) {
				cause := fmt.Errorf("model request: %w", ports.ErrMalformedToolCallArguments)
				if appendErr := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(struct {
					Error      string `json:"error"`
					ErrorClass string `json:"error_class"`
					Retryable  bool   `json:"retryable"`
				}{Error: ports.ErrMalformedToolCallArguments.Error(), ErrorClass: malformedToolArgumentsErrorClass, Retryable: malformedToolArgumentFailures == 0})}); appendErr != nil {
					return Result{RunID: input.RunID}, errors.Join(cause, appendErr)
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
				return loop.persistRunFailure(ctx, &projection, input, &turnID, runFailurePayload{Error: ports.ErrMalformedToolCallArguments.Error(), ErrorClass: malformedToolArgumentsErrorClass}, cause)
			}
			// 提供方失败按分类分叉：瞬态失败只关闭当前轮次、保持 run 非终态，
			// 已完成的对话与工具上下文留给 resume 续跑；永久失败才终态化 run。
			var classified ports.ClassifiedError
			errorClass, retryable := "", false
			if errors.As(err, &classified) {
				errorClass, retryable = classified.ErrorClass(), classified.IsRetryable()
			}
			failure := modelRequestFailedPayload{Error: err.Error(), ErrorClass: errorClass, Retryable: retryable}
			cause := fmt.Errorf("model request: %w", err)
			if appendErr := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(failure)}); appendErr != nil {
				return Result{RunID: input.RunID}, errors.Join(cause, appendErr)
			}
			if retryable {
				if appendErr := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})}); appendErr != nil {
					return Result{RunID: input.RunID}, errors.Join(cause, appendErr)
				}
				return Result{RunID: input.RunID}, cause
			}
			return loop.persistRunFailure(ctx, &projection, input, &turnID, runFailurePayload{Error: failure.Error, ErrorClass: failure.ErrorClass}, cause)
		}
		// 提供方未回报 usage 时以字节估算兜底并显式标记：token 与成本上限
		// 在不合规端点上仍近似生效，且估算随事件持久化、resume 重建预算时
		// 可复算，不与提供方回报值混淆。
		usageEstimated := false
		if response.Usage == nil {
			usageEstimated = true
			response.Usage = &ports.Usage{PromptTokens: loop.estimatePromptTokens(messages), CompletionTokens: estimateResponseTokens(response)}
			response.Usage.TotalTokens = response.Usage.PromptTokens + response.Usage.CompletionTokens
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelResponseFinished, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelResponsePayload{Text: response.Text, ToolCalls: response.ToolCalls, Usage: response.Usage, UsageEstimated: usageEstimated, StopReason: response.StopReason})}); err != nil {
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
				var persistence *verificationPersistenceError
				if errors.As(err, &persistence) {
					return Result{RunID: input.RunID}, err
				}
				if errors.Is(err, ports.ErrWaitingReconcile) {
					return Result{RunID: input.RunID}, err
				}
				if ctx.Err() != nil {
					return loop.persistCancellation(ctx, &projection, input, &turnID)
				}
				var waiting *ports.WaitingApprovalError
				if errors.As(err, &waiting) {
					return Result{RunID: input.RunID}, waiting
				}
				var budget *ports.BudgetExceededError
				if errors.As(err, &budget) {
					return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
				}
				var guard *ports.LoopGuardExceededError
				if errors.As(err, &guard) {
					return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
				}
				var verificationFailure *ports.VerificationFailureError
				if errors.As(err, &verificationFailure) {
					messages = append(messages, ports.Message{Role: "user", Content: verificationFailure.Feedback})
					messages, err = loop.closeTurnAndCompact(ctx, &projection, input, turnID, messages)
					if err != nil {
						return Result{RunID: input.RunID}, err
					}
					continue
				}
				return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
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
				var persistence *toolResultPersistenceError
				if errors.As(err, &persistence) {
					return Result{RunID: input.RunID}, err
				}
				if errors.Is(err, ports.ErrWaitingReconcile) {
					return Result{RunID: input.RunID}, err
				}
				if ctx.Err() != nil {
					return loop.persistCancellation(ctx, &projection, input, &turnID)
				}
				var budget *ports.BudgetExceededError
				if errors.As(err, &budget) {
					return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
				}
				var guard *ports.LoopGuardExceededError
				if errors.As(err, &guard) {
					return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
				}
				var waiting *ports.WaitingApprovalError
				if errors.As(err, &waiting) {
					return Result{RunID: input.RunID}, waiting
				}
				return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
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
