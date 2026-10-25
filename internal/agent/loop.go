// Package agent 包含串行、单写入者的 Agent 执行循环。
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/fxbin/cairn/internal/hooks"
	"strings"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// Loop 协调模型、策略、工具、事件日志、恢复和固定验证流程。
type Loop struct {
	mcpModelDeclarations map[string]string
	projectInstructions  tools.ProjectInstructions
	Journal              ports.Journal
	Provider             ports.Provider
	Tools                *tools.Registry
	Gate                 *policy.Gate
	Model                string
	// ProviderKind 是历史 ProviderKind 词表值，写入 model_request_started；
	// 由配置 effective protocol 经闭合映射派生，不再硬编码。
	ProviderKind      string
	ConfigFingerprint string
	MaxTurns          int
	// Guard 阈值：零值回落默认；显式配置经 fingerprintView 进入配置指纹。
	GuardConsecutiveFailures       int
	GuardPeriodicWindow            int
	GuardNoProgressLimit           int
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
	// DecisionCheckpointEnabled 打开软决策检查点：无补丁提议且已有读取证据
	// 时插入一次说明性反馈（可答证据不足），绝不强制写入。
	DecisionCheckpointEnabled  bool
	DecisionCheckpointMinTurns int
	VerificationAutoAfterPatch bool
	Verification               []VerificationCommand
	// VerificationWorkspace 与工具工作区刻意不同：普通 workspace.ignore
	// 不能掩盖 verifier 产生的副作用。
	VerificationWorkspace tools.Workspace
	Artifacts             *artifact.Store
	OnEvent               EventSink
	SystemPrompt          string
	// Hooks 是可选工作区钩子；pre_tool 由 Policy 收紧，post_tool 记录动作。
	Hooks []hooks.Rule
	// OnHookActions 接收生命周期钩子动作（不含 pre_tool 效果）；宿主负责
	// 按 queue_followup 合同入队。nil 时动作仅丢弃，不产生副作用。
	OnHookActions func(event hooks.Event, actions []hooks.Action)
	// Delegator 执行 delegate_task 委派：以父运行身份受控启动子 run 并同步
	// 等待终态或审批暂停。nil 表示当前运行不接受委派（子运行不嵌套），调用
	// 失败关闭；回调期间应用层会向本 run Journal 追加 delegated/completed
	// 事实，Loop 在回调返回后先再同步投影再继续追加。
	Delegator DelegatorFunc
	// 会话前情（ADR-0018）：调用方显式携带时注入消息首位；前情卡制品引用
	// 同时进入创建事件以支持 resume 回放。
	PriorContext    string
	PriorContextRef *artifact.Ref
	// startedOperations 与 startedOperationsKnown 增量维护已启动操作计数，
	// 供工具调用预算检查复用，避免每次工具调用都全量重载事件日志。
	startedOperations      int
	startedOperationsKnown bool
	// steeringInbox 是安全点 steering 的线程安全投递口；Loop 在工具批次结束
	// 后、下一次模型请求前排空并持久化消费。TUI/回调只投递请求，不直接写
	// Journal，保持单写入者边界。
	steeringInbox    chan SteeringRequest
	steeringConsumed map[string]struct{}
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

// SteeringRequest 是安全点 steering 的一次补充当前任务请求；InputID 用于
// 幂等，正文由调用方写入受限脱敏制品后传引用。
type SteeringRequest struct {
	InputID    string
	Content    string
	ContentRef *artifact.Ref
}

// SubmitSteering 向运行中的 Loop 投递补充当前任务请求；非阻塞，重复 InputID
// 由消费端幂等处理。run 未在运行或收件箱已关闭时返回错误。
func (loop *Loop) SubmitSteering(request SteeringRequest) error {
	if loop.steeringInbox == nil {
		return errors.New("steering inbox is not active")
	}
	select {
	case loop.steeringInbox <- request:
		return nil
	default:
		return errors.New("steering inbox is full")
	}
}

// SteeringBridge 是跨 goroutine 投递安全点 steering 的稳定句柄；Service.Run
// 在装配 Loop 后接管它，调用方在运行期间用 Submit 投递补充指导。
type SteeringBridge struct {
	inbox chan SteeringRequest
}

// NewSteeringBridge 创建带有限缓冲的 steering 桥。
func NewSteeringBridge() *SteeringBridge {
	return &SteeringBridge{inbox: make(chan SteeringRequest, 64)}
}

// Submit 投递一次补充当前任务请求；InputID 必填，正文由消费端脱敏落制品。
func (bridge *SteeringBridge) Submit(inputID, content string) error {
	if bridge == nil || bridge.inbox == nil {
		return errors.New("steering bridge is not active")
	}
	if inputID == "" {
		return errors.New("steering input id is required")
	}
	select {
	case bridge.inbox <- SteeringRequest{InputID: inputID, Content: content}:
		return nil
	default:
		return errors.New("steering inbox is full")
	}
}

// adopt 把桥接收件箱接入 Loop；Run 结束后由 defer 关闭。
func (loop *Loop) adopt(bridge *SteeringBridge) {
	if bridge == nil {
		return
	}
	loop.steeringInbox = bridge.inbox
}

// AdoptSteering 把外部 steering 桥接入 Loop，供运行期间投递补充指导。
func (loop *Loop) AdoptSteering(bridge *SteeringBridge) {
	loop.adopt(bridge)
}

// drainSteering 在安全点排空收件箱；返回待消费请求。
func (loop *Loop) drainSteering() []SteeringRequest {
	if loop.steeringInbox == nil {
		return nil
	}
	var pending []SteeringRequest
	for {
		select {
		case request := <-loop.steeringInbox:
			pending = append(pending, request)
		default:
			return pending
		}
	}
}

// Run 从当前事件投影继续执行模型轮次、工具与验证，直至终态或可恢复暂停。
// 返回错误前统一兜底持久化取消，保证中断信号落在事件追加间隙时也能落盘。
func (loop *Loop) Run(ctx context.Context, input RunInput) (Result, error) {
	if loop.steeringInbox == nil {
		loop.steeringInbox = make(chan SteeringRequest, 64)
	}
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
	// 观察点：用户任务进入运行；仅收集动作。
	loop.dispatchLifecycleHooks(hooks.EventUserPromptSubmit, "")
	events, err := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
	if err != nil {
		return Result{}, err
	}
	projection, err := domain.Replay(events)
	if err != nil {
		return Result{}, err
	}
	if err := loop.restoreMCPResults(events); err != nil {
		return Result{}, err
	}
	if err := loop.restoreSkillResults(events); err != nil {
		return Result{}, err
	}
	if err := loop.restoreProjectInstructions(events); err != nil {
		return Result{}, err
	}
	if ctx.Err() != nil {
		return loop.persistCancellation(ctx, &projection, input, projection.CurrentTurn)
	}
	if projection.State != domain.RunRunning {
		return Result{}, fmt.Errorf("run %s is not runnable from state %s", input.RunID, projection.State)
	}
	if result, stopped, budgetErr := loop.stopForPersistedResponseBudget(ctx, &projection, input, events); stopped {
		return result, budgetErr
	}
	messages, err := loop.messagesFromEvents(events, input.Task)
	if err != nil {
		return Result{}, err
	}
	loop.steeringConsumed = steeringConsumedFromEvents(events)
	loop.requeueUnconsumedSteering(events)
	messages, err = loop.compactContextIfNeeded(ctx, &projection, input, messages)
	if err != nil {
		return Result{}, err
	}
	usage := budgetUsageFromEvents(events)
	malformedToolArgumentFailures := malformedToolArgumentsFailureCount(events)
	usedTurns := openedTurnCount(events)
	// 恢复可能停在已关闭轮次和反馈写入之间；安全边界补齐检查点，不能多等一次模型请求。
	if usedTurns < loop.MaxTurns {
		messages, err = loop.maybeDecisionCheckpoint(ctx, &projection, input, messages)
		if err != nil {
			return Result{RunID: input.RunID}, err
		}
	}
	for turn := usedTurns; turn < loop.MaxTurns; turn++ {
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, &projection, input, nil)
		}
		// 安全点：上一模型响应及其工具批次已结束，下一次模型请求之前。
		// 排空并持久化 steering 消费，把补充指导注入消息。
		messages, err = loop.consumeSteeringAtSafetyPoint(ctx, &projection, input, messages)
		if err != nil {
			return Result{RunID: input.RunID}, err
		}
		turnID := domain.NewTurnID()
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnOpened, TurnID: &turnID, Payload: mustJSON(struct {
			Turn int `json:"turn"`
		}{Turn: turn + 1})}); err != nil {
			return Result{}, err
		}
		loop.dispatchLifecycleHooks(hooks.EventTurnStart, "")
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, &projection, input, &turnID)
		}
		attemptID := domain.NewAttemptID()
		providerKind := loop.ProviderKind
		if providerKind == "" {
			providerKind = "openai_compatible"
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestStarted, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelRequestPayload{Provider: providerKind, Model: loop.Model})}); err != nil {
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
		// 连续性块（thinking 签名等）写入受保护制品，事件只带引用；正文不进普通事件。
		var protocolBlocksRef *artifact.Ref
		if len(response.ProtocolBlocks) > 0 && loop.Artifacts != nil {
			encoded, err := json.Marshal(response.ProtocolBlocks)
			if err != nil {
				return Result{}, fmt.Errorf("encode protocol blocks: %w", err)
			}
			ref, err := loop.Artifacts.Put(encoded, "application/json")
			if err != nil {
				return Result{}, fmt.Errorf("store protocol blocks: %w", err)
			}
			protocolBlocksRef = &ref
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelResponseFinished, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(modelResponsePayload{Text: response.Text, ToolCalls: response.ToolCalls, Usage: response.Usage, UsageEstimated: usageEstimated, StopReason: response.StopReason, ProtocolBlocksRef: protocolBlocksRef})}); err != nil {
			return Result{}, err
		}
		usage.add(response.Usage)
		if exceeded := loop.responseBudgetExceeded(usage); exceeded != nil {
			return loop.persistBudgetExceeded(ctx, &projection, input, exceeded.Dimension)
		}
		messages = append(messages, ports.Message{Role: "assistant", Content: response.Text, ToolCalls: response.ToolCalls, ProtocolBlocks: response.ProtocolBlocks})
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
		outcomes, batchErr := loop.executeToolCalls(ctx, &projection, input, turnID, response.ToolCalls)
		if batchErr != nil && (len(outcomes) == 0 || outcomes[len(outcomes)-1].err == nil) {
			// 错误位于非末位（其后调用未执行）时由本分支统一分类；分类集与
			// 单调用分支保持一致：落盘失败与对账暂停不得被误终态化或降级。
			if special := classifyBatchSpecialError(batchErr); special != nil {
				return Result{RunID: input.RunID}, special
			}
			var waiting *ports.WaitingApprovalError
			if errors.As(batchErr, &waiting) {
				return Result{RunID: input.RunID}, waiting
			}
			var budget *ports.BudgetExceededError
			if errors.As(batchErr, &budget) {
				return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
			}
			var guard *ports.LoopGuardExceededError
			if errors.As(batchErr, &guard) {
				return Result{RunID: input.RunID}, loop.persistLoopGuardExceeded(ctx, &projection, input, guard.Reason)
			}
			return loop.failCurrentTurn(ctx, &projection, input, turnID, batchErr)
		}
		for index, call := range response.ToolCalls {
			if index < len(outcomes) && outcomes[index].err != nil {
				err := outcomes[index].err
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
			var toolResult tools.Result
			if index < len(outcomes) {
				toolResult = outcomes[index].result
			}
			encoded, err := json.Marshal(loop.Tools.ModelResult(call.Name, toolResult))
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

// consumeSteeringAtSafetyPoint 在安全点排空 steering 收件箱：逐条持久化
// steering_input_submitted（幂等）与 steering_input_consumed，并把补充指导
// 注入下一次模型请求。重复 InputID 不重复注入。
func (loop *Loop) consumeSteeringAtSafetyPoint(ctx context.Context, projection *domain.RunProjection, input RunInput, messages []ports.Message) ([]ports.Message, error) {
	pending := loop.drainSteering()
	if len(pending) == 0 {
		return messages, nil
	}
	consumed := steeringConsumedIDs(loop.steeringConsumed)
	for _, request := range pending {
		if request.InputID == "" {
			continue
		}
		if _, already := consumed[request.InputID]; already {
			continue
		}
		contentRef := request.ContentRef
		content := request.Content
		if contentRef == nil && content != "" && loop.Artifacts != nil {
			ref, err := loop.Artifacts.Put([]byte(content), "text/plain")
			if err != nil {
				return messages, fmt.Errorf("store steering content: %w", err)
			}
			contentRef = &ref
			if restored, err := loop.Artifacts.Open(ref); err == nil {
				content = string(restored)
			}
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventSteeringInputSubmitted, Payload: mustJSON(steeringInputPayload{
			InputID: request.InputID, ContentRef: contentRef, Kind: "steer",
		})}); err != nil {
			return messages, err
		}
		outcome := "applied"
		if strings.TrimSpace(content) == "" {
			outcome = "rejected"
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventSteeringInputConsumed, Payload: mustJSON(steeringConsumedPayload{
			InputID: request.InputID, ContentRef: contentRef, Outcome: outcome,
		})}); err != nil {
			return messages, err
		}
		if outcome == "applied" {
			messages = append(messages, ports.Message{Role: "user", Content: "[用户补充当前任务指导]\n" + content})
		}
		if loop.steeringConsumed == nil {
			loop.steeringConsumed = map[string]struct{}{}
		}
		loop.steeringConsumed[request.InputID] = struct{}{}
	}
	return messages, nil
}

// steeringInputPayload 是 steering_input_submitted 的业务字段。
type steeringInputPayload struct {
	InputID    string        `json:"input_id"`
	ContentRef *artifact.Ref `json:"content_ref,omitempty"`
	Kind       string        `json:"kind"`
}

// steeringConsumedPayload 是 steering_input_consumed 的业务字段。
type steeringConsumedPayload struct {
	InputID    string        `json:"input_id"`
	ContentRef *artifact.Ref `json:"content_ref,omitempty"`
	Outcome    string        `json:"outcome"`
}

// steeringConsumedIDs 返回已消费 InputID 集合视图；仅读用途，空源返回空映射。
func steeringConsumedIDs(source map[string]struct{}) map[string]struct{} {
	if source == nil {
		return map[string]struct{}{}
	}
	return source
}

// steeringConsumedFromEvents 从事件流重建已消费 InputID，冷恢复避免重复注入。
func steeringConsumedFromEvents(events []domain.Event) map[string]struct{} {
	consumed := make(map[string]struct{})
	for _, event := range events {
		if event.Type != domain.EventSteeringInputConsumed {
			continue
		}
		var payload struct {
			InputID string `json:"input_id"`
		}
		if json.Unmarshal(event.Payload, &payload) == nil && payload.InputID != "" {
			consumed[payload.InputID] = struct{}{}
		}
	}
	return consumed
}

// requeueUnconsumedSteering 把已提交但未消费的 steering 输入重新投递回收件箱，
// 冷恢复后仍能在安全点生效；正文从制品解引用，损坏则跳过并留待对账。
func (loop *Loop) requeueUnconsumedSteering(events []domain.Event) {
	if loop.steeringInbox == nil {
		loop.steeringInbox = make(chan SteeringRequest, 64)
	}
	for _, event := range events {
		if event.Type != domain.EventSteeringInputSubmitted {
			continue
		}
		var payload struct {
			InputID    string          `json:"input_id"`
			ContentRef json.RawMessage `json:"content_ref"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.InputID == "" {
			continue
		}
		if _, consumed := loop.steeringConsumed[payload.InputID]; consumed {
			continue
		}
		content := ""
		if len(payload.ContentRef) > 0 && loop.Artifacts != nil {
			var ref artifact.Ref
			if json.Unmarshal(payload.ContentRef, &ref) == nil {
				if restored, err := loop.Artifacts.Open(ref); err == nil {
					content = string(restored)
				}
			}
		}
		select {
		case loop.steeringInbox <- SteeringRequest{InputID: payload.InputID, Content: content}:
		default:
		}
	}
}

// closeTurnAndCompact 写入继续轮次的关闭事件，并在需要时生成上下文检查点。
func (loop *Loop) closeTurnAndCompact(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, messages []ports.Message) ([]ports.Message, error) {
	if err := loop.closeContinuationTurn(ctx, projection, input, turnID); err != nil {
		return nil, err
	}
	if len(loop.Tools.ActiveSkills()) != 0 {
		events, loadErr := loop.Journal.Load(context.WithoutCancel(ctx), input.RunID)
		if loadErr != nil {
			return nil, loadErr
		}
		rebuilt, rebuildErr := loop.rebuildMessagesFromEvents(events, input.Task, true)
		if rebuildErr != nil {
			return nil, rebuildErr
		}
		messages = rebuilt
	}
	messages, err := loop.compactContextIfNeeded(ctx, projection, input, messages)
	if err != nil {
		return nil, err
	}
	return loop.maybeDecisionCheckpoint(ctx, projection, input, messages)
}
