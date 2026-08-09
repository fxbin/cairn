// Package agent 包含串行、单写入者的 Agent 执行循环。
package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fxbin/cairn/internal/artifact"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/policy"
	"github.com/fxbin/cairn/internal/ports"
	"github.com/fxbin/cairn/internal/tools"
)

// ErrTurnBudgetExceeded 表示运行耗尽了允许的模型轮次。
var ErrTurnBudgetExceeded = errors.New("turn budget exhausted")

// BudgetExceededError 标识导致运行停止的持久化预算维度；为兼容调用方，
// 它会解包为 ErrTurnBudgetExceeded。
type BudgetExceededError struct{ Dimension string }

// Error 返回耗尽预算维度的稳定错误文本。
func (err *BudgetExceededError) Error() string {
	return fmt.Sprintf("budget exhausted: %s", err.Dimension)
}

// Unwrap 使预算错误可被 errors.Is 识别为轮次预算耗尽。
func (err *BudgetExceededError) Unwrap() error { return ErrTurnBudgetExceeded }

// WaitingApprovalError 表示运行在尚未启动副作用前等待人工决定。
type WaitingApprovalError struct {
	RunID       domain.RunID
	OperationID domain.OperationID
	Fingerprint string
	Summary     string
}

// Error 返回等待审批操作及其绑定指纹。
func (err *WaitingApprovalError) Error() string {
	return fmt.Sprintf("approval required for %s (%s)", err.OperationID, err.Fingerprint)
}

// PolicyDeniedError 表示策略明确拒绝了模型提议的操作。
type PolicyDeniedError struct{ Summary string }

// Error 返回被拒绝操作的策略摘要。
func (err *PolicyDeniedError) Error() string { return "policy denied: " + err.Summary }

// VerificationFailureError 是固定验证命令失败的持久化反馈；它不是终态错误，
// 模型仍可在剩余轮次中修复工作区。
type VerificationFailureError struct {
	CommandID string
	Feedback  string
}

// Error 返回可反馈给模型的验证失败信息。
func (err *VerificationFailureError) Error() string { return err.Feedback }

// EventSink 接收 Loop 已成功追加到日志的领域事件。
type EventSink func(domain.Event)

// Loop 协调模型、策略、工具、事件日志、恢复和固定验证流程。
type Loop struct {
	Journal                ports.Journal
	Provider               ports.Provider
	Tools                  *tools.Registry
	Gate                   *policy.Gate
	Model                  string
	ConfigFingerprint      string
	MaxTurns               int
	MaxToolCalls           int
	MaxInputTokens         int
	MaxOutputTokens        int
	MaxCostUSD             float64
	InputTokenUSD          float64
	OutputTokenUSD         float64
	ContextAutoCompact     bool
	ContextMaxPromptTokens int
	ContextMaxSummaryBytes int
	Verification           []VerificationCommand
	// VerificationWorkspace 与工具工作区刻意不同：普通 workspace.ignore
	// 不能掩盖 verifier 产生的副作用。
	VerificationWorkspace tools.Workspace
	Artifacts             *artifact.Store
	OnEvent               EventSink
	SystemPrompt          string
}

// VerificationCommand 是 Loop 在完成前按配置执行的冻结命令。
type VerificationCommand struct {
	ID        string
	Arguments json.RawMessage
}

// verificationPlan 是持久化为制品并绑定审批的验证计划快照。
type verificationPlan struct {
	Version             string                `json:"version"`
	VerificationID      string                `json:"verification_id"`
	ConfigFingerprint   string                `json:"config_fingerprint"`
	LastMutationSeq     uint64                `json:"last_mutation_seq"`
	CommandsFingerprint string                `json:"commands_fingerprint"`
	Commands            []VerificationCommand `json:"commands"`
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

// modelResponsePayload 是写入 model_response_finished 事件的响应内容。
type modelResponsePayload struct {
	Text      string           `json:"text"`
	ToolCalls []ports.ToolCall `json:"tool_calls,omitempty"`
	Usage     *ports.Usage     `json:"usage,omitempty"`
}

// modelRequestPayload 是写入模型请求事件的可审计提供方信息。
type modelRequestPayload struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// toolCallPayload 是写入工具提议事件的模型调用事实。
type toolCallPayload struct {
	CallID    string          `json:"call_id"`
	ToolName  string          `json:"tool_name"`
	Arguments json.RawMessage `json:"arguments"`
}

// operationPayload 是工具操作开始或终态事件的可回放证据字段。
type operationPayload struct {
	ToolName        string        `json:"tool_name"`
	SideEffectClass string        `json:"side_effect_class,omitempty"`
	MutationBarrier bool          `json:"mutation_barrier,omitempty"`
	PlannedPath     string        `json:"planned_path,omitempty"`
	PlannedBefore   string        `json:"planned_before,omitempty"`
	PlannedAfter    string        `json:"planned_after,omitempty"`
	Result          any           `json:"result,omitempty"`
	ResultRef       *artifact.Ref `json:"result_ref,omitempty"`
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
		loop.MaxTurns = 16
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
	for turn := 0; turn < loop.MaxTurns; turn++ {
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
			_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(struct {
				Error string `json:"error"`
			}{Error: err.Error()})})
			_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})})
			_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
				Error string `json:"error"`
			}{Error: err.Error()})})
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
				var verificationFailure *VerificationFailureError
				if errors.As(err, &verificationFailure) {
					messages = append(messages, ports.Message{Role: "user", Content: verificationFailure.Feedback})
					if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
						return Result{}, err
					}
					messages, err = loop.compactContextIfNeeded(ctx, &projection, input, messages)
					if err != nil {
						return Result{}, err
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
			messages = append(messages, ports.Message{Role: "tool", ToolCallID: call.ID, Content: string(encoded)})
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
			return Result{}, err
		}
		messages, err = loop.compactContextIfNeeded(ctx, &projection, input, messages)
		if err != nil {
			return Result{}, err
		}
	}
	if ctx.Err() != nil {
		return loop.persistCancellation(ctx, &projection, input, nil)
	}
	return loop.persistBudgetExceeded(ctx, &projection, input, "turns")
}

// persistCancellation 关闭待审批状态和当前轮次，并把取消原因持久化为终态事件。
func (loop *Loop) persistCancellation(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID *domain.TurnID) (Result, error) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return loop.persistBudgetExceeded(ctx, projection, input, "wall_time")
	}
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if projection.State == domain.RunCancelling {
		return Result{RunID: input.RunID}, context.Canceled
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCancelRequested, TurnID: turnID, Payload: mustJSON(struct {
		Cause string `json:"cause"`
	}{Cause: "user_interrupt"})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if err := loop.cancelPendingApproval(commitContext, projection, input, "user_interrupt"); err != nil {
		return Result{RunID: input.RunID}, err
	}
	// A pending approval is the highest-priority recovery overlay. The model's
	// candidate response may already be durable, but it must not start a second
	// verification plan or another tool path before that exact operation is
	// resolved.
	if projection.CurrentTurn != nil && projection.PendingApproval == nil {
		currentTurn := *projection.CurrentTurn
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &currentTurn, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "cancelled"})}); err != nil {
			return Result{RunID: input.RunID}, err
		}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCancelled, Payload: mustJSON(struct {
		Cause string `json:"cause"`
	}{Cause: "user_interrupt"})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	return Result{RunID: input.RunID}, context.Canceled
}

// persistBudgetExceeded 将触发的预算维度及失败状态追加到事件日志。
func (loop *Loop) persistBudgetExceeded(ctx context.Context, projection *domain.RunProjection, input RunInput, dimension string) (Result, error) {
	commitContext, cancel := loop.commitContext(ctx)
	defer cancel()
	if projection.State == domain.RunCancelling {
		return Result{RunID: input.RunID}, &BudgetExceededError{Dimension: dimension}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCancelRequested, Payload: mustJSON(struct {
		Cause            string `json:"cause"`
		ExpectedTerminal string `json:"expected_terminal"`
		Dimension        string `json:"budget_dimension"`
	}{Cause: "budget_exhausted", ExpectedTerminal: "failed", Dimension: dimension})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if err := loop.cancelPendingApproval(commitContext, projection, input, "budget_exhausted"); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if projection.CurrentTurn != nil {
		turnID := *projection.CurrentTurn
		if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})}); err != nil {
			return Result{RunID: input.RunID}, err
		}
	}
	if err := loop.append(commitContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
		Error     string `json:"error"`
		Dimension string `json:"budget_dimension"`
	}{Error: (&BudgetExceededError{Dimension: dimension}).Error(), Dimension: dimension})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	return Result{RunID: input.RunID}, &BudgetExceededError{Dimension: dimension}
}

// commitContext 派生保留调用方值但不受运行取消影响的短暂持久化上下文。
func (loop *Loop) commitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

// appendResume records an invocation only at a recovery boundary where there
// is no unresolved overlay. In particular, an invalid approval must not leave
// a misleading run_resumed event behind.
// appendResume 在恢复前记录调用标识、来源序号和 verifier 工作区观察值。
func (loop *Loop) appendResume(ctx context.Context, projection *domain.RunProjection, input *RunInput) error {
	if input.ResumeInvocation == "" {
		return nil
	}
	if input.ResumeObservation == "" || input.ResumeFromSeq == 0 {
		return errors.New("resume observation metadata is incomplete")
	}
	if projection.PendingApproval != nil || projection.PendingOperation != nil {
		return errors.New("run_resumed cannot be recorded with an unresolved overlay")
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunResumed, Payload: mustJSON(struct {
		Invocation           string `json:"invocation"`
		FromSeq              uint64 `json:"from_seq"`
		WorkspaceObservation string `json:"workspace_observation"`
	}{Invocation: input.ResumeInvocation, FromSeq: input.ResumeFromSeq, WorkspaceObservation: input.ResumeObservation})}); err != nil {
		return err
	}
	input.ResumeInvocation = ""
	return nil
}

// cancelPendingApproval 在终止前关闭审批覆盖层；它明确由系统发起，不会消费
// 人工授权或启动操作，但会留下可回放的审计原因。
func (loop *Loop) cancelPendingApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, cause string) error {
	if projection.PendingApproval == nil {
		return nil
	}
	if projection.CurrentTurn == nil {
		return errors.New("pending approval has no open turn")
	}
	pending := projection.PendingApproval
	turnID := *projection.CurrentTurn
	return loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
		StopCause   string `json:"stop_cause"`
	}{Fingerprint: pending.Fingerprint, Actor: "system", Reason: "approval closed while the run was stopping", StopCause: cause})})
}

// Approval 是 cairn resume 提供的显式、一次性审批决定。
type Approval struct {
	Fingerprint string
	Approve     bool
	Reason      string
}

// Resume 继续在模型工具启动前暂停的运行；它绝不为其他指纹复用审批，也不重启已开始操作。
func (loop *Loop) Resume(ctx context.Context, input RunInput, approval Approval) (Result, error) {
	if loop.Journal == nil || loop.Provider == nil || loop.Tools == nil || loop.Gate == nil {
		return Result{}, errors.New("agent loop dependencies and policy gate are required")
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
		return Result{}, errors.New("run is not resumable")
	}
	if projection.PendingOperation != nil && (projection.PendingOperation.SideEffectClass == "workspace" || projection.PendingOperation.SideEffectClass == "process") {
		return Result{}, errors.New("run has a dangling side-effect operation that requires reconciliation")
	}
	if projection.PendingOperation != nil {
		if projection.CurrentTurn == nil {
			return Result{}, errors.New("dangling read operation has no open turn")
		}
		pending := projection.PendingOperation
		call, found := proposedToolCall(events, pending.ID)
		if !found {
			return Result{}, errors.New("dangling read operation has no replayable tool proposal")
		}
		turnID := *projection.CurrentTurn
		result := tools.Failure("interrupted_read", "read-only operation was interrupted before its result was persisted")
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.ID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &pending.ID, Payload: mustJSON(result)}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
			return Result{}, err
		}
		if err := loop.appendResume(ctx, &projection, &input); err != nil {
			return Result{}, err
		}
		input.Task = projection.Task
		return loop.Run(ctx, input)
	}
	if projection.PendingApproval == nil {
		if err := loop.appendResume(ctx, &projection, &input); err != nil {
			return Result{}, err
		}
	}
	// A persisted approval is the current authoritative continuation point. A
	// completed model response can coexist with a verification approval, so it
	// must not be replayed until that approval has been resolved.
	if projection.CurrentTurn != nil && projection.PendingApproval == nil {
		turnID := *projection.CurrentTurn
		if response, found := completedModelResponseForTurn(events, turnID); found {
			input.Task = projection.Task
			if len(response.ToolCalls) == 0 {
				return loop.resumeFinalModelResponse(ctx, &projection, input, turnID, response, events)
			}
			if !hasAnyToolProposal(events, response.ToolCalls) {
				for _, call := range response.ToolCalls {
					if _, err := loop.executeTool(ctx, &projection, input, turnID, call); err != nil {
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
						var denied *PolicyDeniedError
						if errors.As(err, &denied) {
							return loop.failCurrentTurn(ctx, &projection, input, turnID, denied)
						}
						return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
					}
				}
				if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
					return Result{}, err
				}
				return loop.Run(ctx, input)
			}
		}
	}
	if projection.PendingApproval == nil {
		attemptID, turnID, incomplete := incompleteModelAttempt(events)
		if !incomplete {
			if projection.CurrentTurn == nil {
				input.Task = projection.Task
				return loop.Run(ctx, input)
			}
			return Result{}, errors.New("open turn has no resumable model attempt or approval")
		}
		if projection.CurrentTurn == nil || *projection.CurrentTurn != turnID {
			return Result{}, errors.New("interrupted model attempt does not match the open turn")
		}
		input.Task = projection.Task
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventModelRequestFailed, TurnID: &turnID, AttemptID: &attemptID, Payload: mustJSON(struct {
			Error     string `json:"error"`
			Retryable bool   `json:"retryable"`
		}{Error: "interrupted", Retryable: true})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
			return Result{}, err
		}
		return loop.Run(ctx, input)
	}
	if projection.CurrentTurn == nil {
		return Result{}, errors.New("run is not waiting for an approval")
	}
	input.Task = projection.Task
	pending := projection.PendingApproval
	if approval.Fingerprint == "" || approval.Fingerprint != pending.Fingerprint {
		return Result{}, errors.New("approval fingerprint does not match the pending operation")
	}
	call, modelProposal := proposedToolCall(events, pending.OperationID)
	verification, verificationProposal := proposedVerificationCommand(events, pending.OperationID)
	if !modelProposal && !verificationProposal {
		return Result{}, errors.New("pending approval has no replayable tool proposal")
	}
	if verificationProposal {
		call = ports.ToolCall{ID: string(pending.OperationID), Name: "exec_command", Arguments: verification.Arguments}
	}
	turnID := *projection.CurrentTurn
	if !approval.Approve {
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
			Fingerprint string `json:"fingerprint"`
			Reason      string `json:"reason"`
		}{Fingerprint: pending.Fingerprint, Reason: approval.Reason})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: tools.Failure("approval_denied", approval.Reason)})}); err != nil {
			return Result{}, err
		}
		if verificationProposal {
			if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: false})}); err != nil {
				return Result{}, err
			}
		}
		_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})})
		_ = loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
			Error string `json:"error"`
		}{Error: "approval denied"})})
		return Result{RunID: input.RunID}, &PolicyDeniedError{Summary: approval.Reason}
	}
	decision, err := loop.resumeDecision(projection, call, verification, verificationProposal)
	if err != nil {
		return Result{}, err
	}
	if decision.Effect != policy.Ask || decision.Fingerprint != pending.Fingerprint {
		return loop.expireApproval(ctx, &projection, input, turnID, pending, call, verificationProposal, decision)
	}
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		var budget *BudgetExceededError
		if errors.As(err, &budget) {
			return loop.persistBudgetExceeded(ctx, &projection, input, budget.Dimension)
		}
		return Result{}, err
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalGranted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
	}{Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	if err := loop.appendResume(ctx, &projection, &input); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
	}{Effect: "allow", Rule: "approved_once", Fingerprint: pending.Fingerprint})}); err != nil {
		return Result{}, err
	}
	if verificationProposal {
		if verification.CommandIndex < 0 || verification.CommandIndex >= len(loop.Verification) {
			return Result{}, errors.New("verification command index does not match frozen configuration")
		}
		if err := loop.executeVerificationCommand(ctx, &projection, input, turnID, pending.OperationID, loop.Verification[verification.CommandIndex]); err != nil {
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			var verificationFailure *VerificationFailureError
			if errors.As(err, &verificationFailure) {
				if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
					return Result{}, err
				}
				return loop.Run(ctx, input)
			}
			return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
		}
		if err := loop.runVerificationCommands(ctx, &projection, input, turnID, verification.VerificationID, verification.PlanRef, verification.CommandIndex+1); err != nil {
			var waiting *WaitingApprovalError
			if errors.As(err, &waiting) {
				return Result{RunID: input.RunID}, waiting
			}
			if ctx.Err() != nil {
				return loop.persistCancellation(ctx, &projection, input, &turnID)
			}
			var verificationFailure *VerificationFailureError
			if errors.As(err, &verificationFailure) {
				if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
					return Result{}, err
				}
				return loop.Run(ctx, input)
			}
			return loop.failCurrentTurn(ctx, &projection, input, turnID, err)
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: projection.LastAssistantMessage})}); err != nil {
			return Result{}, err
		}
		return Result{RunID: input.RunID, Response: projection.LastAssistantMessage}, nil
	}
	sideEffect := loop.Tools.SideEffect(call.Name)
	startedPayload := operationPayload{ToolName: call.Name, SideEffectClass: string(sideEffect), MutationBarrier: sideEffect == tools.SideEffectProcess}
	if call.Name == "apply_patch" {
		plan, err := loop.Gate.Patch.Plan(call.Arguments)
		if err != nil {
			return Result{}, err
		}
		startedPayload.PlannedPath, startedPayload.PlannedBefore, startedPayload.PlannedAfter = plan.Path, plan.BeforeHash, plan.AfterHash
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(startedPayload)}); err != nil {
		return Result{}, err
	}
	result := loop.Tools.Execute(ctx, call.Name, call.Arguments)
	result = redactToolResult(result)
	eventType := domain.EventOperationSucceeded
	cancelled := result.Code == "cancelled" && ctx.Err() != nil
	if cancelled {
		eventType = domain.EventOperationCancelled
	} else if !result.OK {
		eventType = domain.EventOperationFailed
	}
	resultContext := ctx
	var cancel context.CancelFunc
	if cancelled {
		resultContext, cancel = loop.commitContext(ctx)
		defer cancel()
	}
	payload, err := loop.resultPayload(call.Name, result)
	if err != nil {
		return Result{}, err
	}
	if err := loop.append(resultContext, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(payload)}); err != nil {
		return Result{}, err
	}
	if cancelled {
		return loop.persistCancellation(ctx, &projection, input, &turnID)
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(result)}); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, &projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
		return Result{}, err
	}
	// A resumed proposal has now been consumed. Calling Run starts the next
	// model turn; the prior tool result is already durable even if that request
	// later fails.
	return loop.Run(ctx, input)
}

// resumeFinalModelResponse 在恢复时重新处理已完成模型响应的最终回复或工具调用。
func (loop *Loop) resumeFinalModelResponse(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, response modelResponsePayload, events []domain.Event) (Result, error) {
	if !hasAssistantMessage(events, turnID) {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventAssistantMessageAppended, TurnID: &turnID, Payload: mustJSON(domain.AssistantMessagePayload{Text: response.Text})}); err != nil {
			return Result{}, err
		}
	}
	if err := loop.runVerification(ctx, projection, input, turnID); err != nil {
		if ctx.Err() != nil {
			return loop.persistCancellation(ctx, projection, input, &turnID)
		}
		var waiting *WaitingApprovalError
		if errors.As(err, &waiting) {
			return Result{RunID: input.RunID}, waiting
		}
		return loop.failCurrentTurn(ctx, projection, input, turnID, err)
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "final"})}); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunCompleted, Payload: mustJSON(domain.RunCompletedPayload{FinalResponse: response.Text})}); err != nil {
		return Result{}, err
	}
	return Result{RunID: input.RunID, Response: response.Text}, nil
}

// completedModelResponseForTurn 查找指定轮次已持久化的完整模型响应。
func completedModelResponseForTurn(events []domain.Event, turnID domain.TurnID) (modelResponsePayload, bool) {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		if event.Type != domain.EventModelResponseFinished || event.TurnID == nil || *event.TurnID != turnID {
			continue
		}
		var payload modelResponsePayload
		return payload, json.Unmarshal(event.Payload, &payload) == nil
	}
	return modelResponsePayload{}, false
}

// hasAnyToolProposal 判断模型响应中的任一工具调用是否已经被提议并持久化。
func hasAnyToolProposal(events []domain.Event, calls []ports.ToolCall) bool {
	callIDs := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		callIDs[call.ID] = struct{}{}
	}
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed {
			continue
		}
		var payload toolCallPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			if _, found := callIDs[payload.CallID]; found {
				return true
			}
		}
	}
	return false
}

// hasAssistantMessage 判断指定轮次是否已写入用户可见的助手消息。
func hasAssistantMessage(events []domain.Event, turnID domain.TurnID) bool {
	for _, event := range events {
		if event.Type == domain.EventAssistantMessageAppended && event.TurnID != nil && *event.TurnID == turnID {
			return true
		}
	}
	return false
}

// resumeDecision 为已提议操作重建策略判定，并保留验证计划的独立审批边界。
func (loop *Loop) resumeDecision(projection domain.RunProjection, call ports.ToolCall, verification verificationProposalPayload, verificationProposal bool) (policy.Decision, error) {
	if verificationProposal {
		planSHA := ""
		if verification.PlanRef != nil {
			planSHA = verification.PlanRef.SHA256
		}
		observation, err := loop.verificationObservation()
		if err != nil {
			return policy.Decision{}, fmt.Errorf("observe workspace for verification approval: %w", err)
		}
		decision, err := loop.Gate.DecideVerificationWithObservation(verification.Arguments, planSHA, verification.CommandIndex, loop.ConfigFingerprint, observation)
		if err != nil {
			return policy.Decision{}, err
		}
		return freshApprovalDecision(projection, decision), nil
	}
	decision, err := loop.Gate.Decide(call.Name, call.Arguments)
	if err != nil {
		return policy.Decision{}, err
	}
	return freshApprovalDecision(projection, decision), nil
}

// expireApproval records that a one-shot approval no longer describes the
// current workspace, then lets the model make a new decision from durable
// tool/verification feedback. The old command is never started.
// expireApproval 将过期或不匹配的审批转换为可审计拒绝，而不执行关联操作。
func (loop *Loop) expireApproval(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, pending *domain.PendingApproval, call ports.ToolCall, verificationProposal bool, _ policy.Decision) (Result, error) {
	reason := "approval expired because the command, policy, or workspace observation changed; re-decision required"
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalDenied, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Reason      string `json:"reason"`
	}{Fingerprint: pending.Fingerprint, Reason: reason})}); err != nil {
		return Result{}, err
	}
	result := tools.Failure("approval_stale", reason)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
		return Result{}, err
	}
	if verificationProposal {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: false})}); err != nil {
			return Result{}, err
		}
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFeedbackAppended, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(struct {
			Text string `json:"text"`
		}{Text: "Verification approval expired after workspace drift. Re-inspect the workspace and decide whether to repair or rerun verification."})}); err != nil {
			return Result{}, err
		}
	} else if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &pending.OperationID, Payload: mustJSON(result)}); err != nil {
		return Result{}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "continue"})}); err != nil {
		return Result{}, err
	}
	if err := loop.appendResume(ctx, projection, &input); err != nil {
		return Result{}, err
	}
	return loop.Run(ctx, input)
}

// failCurrentTurn 持久化当前轮次失败和运行失败，返回原始失败原因。
func (loop *Loop) failCurrentTurn(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, cause error) (Result, error) {
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventTurnClosed, TurnID: &turnID, Payload: mustJSON(domain.TurnClosedPayload{Outcome: "failed"})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventRunFailed, Payload: mustJSON(struct {
		Error string `json:"error"`
	}{Error: cause.Error()})}); err != nil {
		return Result{RunID: input.RunID}, err
	}
	return Result{RunID: input.RunID}, cause
}

// incompleteModelAttempt 查找请求已开始但未持久化终态的模型尝试。
func incompleteModelAttempt(events []domain.Event) (domain.AttemptID, domain.TurnID, bool) {
	pending := make(map[domain.AttemptID]domain.TurnID)
	for _, event := range events {
		switch event.Type {
		case domain.EventModelRequestStarted:
			if event.AttemptID != nil && event.TurnID != nil {
				pending[*event.AttemptID] = *event.TurnID
			}
		case domain.EventModelResponseFinished, domain.EventModelRequestFailed:
			if event.AttemptID != nil {
				delete(pending, *event.AttemptID)
			}
		}
	}
	var latest domain.AttemptID
	var turn domain.TurnID
	for attempt, turnID := range pending {
		if string(attempt) > string(latest) {
			latest, turn = attempt, turnID
		}
	}
	return latest, turn, latest != ""
}

// proposedToolCall 从事件历史恢复某操作对应的工具调用。
func proposedToolCall(events []domain.Event, operationID domain.OperationID) (ports.ToolCall, bool) {
	for _, event := range events {
		if event.Type != domain.EventToolCallProposed || event.OperationID == nil || *event.OperationID != operationID {
			continue
		}
		var payload toolCallPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return ports.ToolCall{}, false
		}
		return ports.ToolCall{ID: payload.CallID, Name: payload.ToolName, Arguments: payload.Arguments}, payload.CallID != "" && payload.ToolName != "" && json.Valid(payload.Arguments)
	}
	return ports.ToolCall{}, false
}

// proposedVerificationCommand 从事件历史恢复某操作对应的验证命令提议。
func proposedVerificationCommand(events []domain.Event, operationID domain.OperationID) (verificationProposalPayload, bool) {
	for _, event := range events {
		if event.Type != domain.EventVerificationCommandProposed || event.OperationID == nil || *event.OperationID != operationID {
			continue
		}
		var payload verificationProposalPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return verificationProposalPayload{}, false
		}
		return payload, payload.VerificationID != "" && payload.CommandIndex >= 0 && json.Valid(payload.Arguments)
	}
	return verificationProposalPayload{}, false
}

// messagesFromEvents 将已持久化事实重建为下一次模型请求所需的对话上下文。
func (loop *Loop) messagesFromEvents(events []domain.Event, fallbackTask string) []ports.Message {
	task := fallbackTask
	checkpoint, throughSeq := latestContextCheckpoint(events)
	proposals := make(map[domain.OperationID]ports.ToolCall)
	completedCalls := make(map[string]bool)
	for _, event := range events {
		if event.Type != domain.EventRunCreated {
			continue
		}
		var payload domain.RunCreatedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Task != "" {
			task = payload.Task
		}
	}
	for _, event := range events {
		if event.Seq <= throughSeq {
			continue
		}
		switch event.Type {
		case domain.EventToolCallProposed:
			if event.OperationID == nil {
				continue
			}
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" {
				proposals[*event.OperationID] = ports.ToolCall{ID: payload.CallID, Name: payload.ToolName, Arguments: payload.Arguments}
			}
		case domain.EventToolResultAppended:
			if event.OperationID == nil {
				continue
			}
			if call, exists := proposals[*event.OperationID]; exists {
				completedCalls[call.ID] = true
			}
		}
	}
	messages := loop.contextMessages(task, checkpoint)
	for _, event := range events {
		if event.Seq <= throughSeq {
			continue
		}
		switch event.Type {
		case domain.EventModelResponseFinished:
			var payload modelResponsePayload
			if json.Unmarshal(event.Payload, &payload) != nil || len(payload.ToolCalls) == 0 {
				continue
			}
			complete := true
			for _, call := range payload.ToolCalls {
				if !completedCalls[call.ID] {
					complete = false
					break
				}
			}
			if complete {
				messages = append(messages, ports.Message{Role: "assistant", Content: payload.Text, ToolCalls: payload.ToolCalls})
			}
		case domain.EventToolCallProposed:
			if event.OperationID == nil {
				continue
			}
			var payload toolCallPayload
			if json.Unmarshal(event.Payload, &payload) == nil && payload.CallID != "" {
				proposals[*event.OperationID] = ports.ToolCall{ID: payload.CallID, Name: payload.ToolName, Arguments: payload.Arguments}
			}
		case domain.EventToolResultAppended:
			if event.OperationID == nil {
				continue
			}
			if call, exists := proposals[*event.OperationID]; exists {
				messages = append(messages, ports.Message{Role: "tool", ToolCallID: call.ID, Content: string(event.Payload)})
			}
		case domain.EventVerificationFeedbackAppended:
			var payload struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && payload.Text != "" {
				messages = append(messages, ports.Message{Role: "user", Content: payload.Text})
			}
		}
	}
	return messages
}

// latestContextCheckpoint 返回最新有效检查点及其所替代历史的最后事件序号。
func latestContextCheckpoint(events []domain.Event) (string, uint64) {
	var summary string
	var throughSeq uint64
	for _, event := range events {
		if event.Type != domain.EventContextCompacted {
			continue
		}
		var payload domain.ContextCompactedPayload
		if json.Unmarshal(event.Payload, &payload) == nil && payload.Summary != "" && payload.ThroughSeq < event.Seq {
			summary = payload.Summary
			throughSeq = payload.ThroughSeq
		}
	}
	return summary, throughSeq
}

// runVerification 创建并执行冻结验证计划，任何失败都会反馈给后续模型轮次。
func (loop *Loop) runVerification(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID) error {
	if projection.LastMutationSeq != 0 && len(loop.Verification) == 0 {
		return errors.New("workspace changed but no verification commands are configured")
	}
	verificationID := domain.NewID("verify")
	plan, planRef, err := loop.createVerificationPlan(verificationID, projection.LastMutationSeq)
	if err != nil {
		return err
	}
	observation, err := loop.verificationObservation()
	if err != nil {
		return fmt.Errorf("observe workspace before verification plan: %w", err)
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationStarted, TurnID: &turnID, Payload: mustJSON(struct {
		VerificationID       string        `json:"verification_id"`
		MutationSeq          uint64        `json:"last_mutation_seq"`
		PlanRef              *artifact.Ref `json:"plan_ref,omitempty"`
		CommandsFingerprint  string        `json:"commands_fingerprint"`
		WorkspaceObservation string        `json:"workspace_observation"`
	}{VerificationID: verificationID, MutationSeq: projection.LastMutationSeq, PlanRef: planRef, CommandsFingerprint: plan.CommandsFingerprint, WorkspaceObservation: observation})}); err != nil {
		return err
	}
	if projection.LastMutationSeq == 0 {
		return loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: true})})
	}
	return loop.runVerificationCommands(ctx, projection, input, turnID, verificationID, planRef, 0)
}

// createVerificationPlan 冻结验证命令及配置指纹，并将计划作为脱敏制品保存。
func (loop *Loop) createVerificationPlan(verificationID string, lastMutationSeq uint64) (verificationPlan, *artifact.Ref, error) {
	commands, err := json.Marshal(loop.Verification)
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("encode verification commands: %w", err)
	}
	digest := sha256.Sum256(commands)
	plan := verificationPlan{
		Version:             "v1",
		VerificationID:      verificationID,
		ConfigFingerprint:   loop.ConfigFingerprint,
		LastMutationSeq:     lastMutationSeq,
		CommandsFingerprint: "sha256:" + hex.EncodeToString(digest[:]),
		Commands:            append([]VerificationCommand(nil), loop.Verification...),
	}
	if loop.Artifacts == nil {
		return plan, nil, nil
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("encode verification plan: %w", err)
	}
	ref, err := loop.Artifacts.Put(encoded, "application/json")
	if err != nil {
		return verificationPlan{}, nil, fmt.Errorf("store verification plan: %w", err)
	}
	return plan, &ref, nil
}

// runVerificationCommands 从指定索引顺序执行冻结计划中的验证命令。
func (loop *Loop) runVerificationCommands(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, verificationID string, planRef *artifact.Ref, startIndex int) error {
	for index := startIndex; index < len(loop.Verification); index++ {
		if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
			return err
		}
		command := loop.Verification[index]
		operationID := domain.NewOperationID()
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationCommandProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(verificationProposalPayload{VerificationID: verificationID, CommandIndex: index, PlanRef: planRef, Arguments: command.Arguments})}); err != nil {
			return err
		}
		planSHA := ""
		if planRef != nil {
			planSHA = planRef.SHA256
		}
		observation, err := loop.verificationObservation()
		if err != nil {
			return fmt.Errorf("observe workspace for verification approval: %w", err)
		}
		decision, err := loop.Gate.DecideVerificationWithObservation(command.Arguments, planSHA, index, loop.ConfigFingerprint, observation)
		if err != nil {
			return err
		}
		decision = freshApprovalDecision(*projection, decision)
		if err := loop.append(ctx, projection, policyDecisionDraft(input, turnID, operationID, decision)); err != nil {
			return err
		}
		if decision.Effect == policy.Ask {
			if err := loop.append(ctx, projection, approvalRequestedDraft(input, turnID, operationID, decision)); err != nil {
				return err
			}
			return &WaitingApprovalError{RunID: input.RunID, OperationID: operationID, Fingerprint: decision.Fingerprint, Summary: decision.Summary}
		}
		if decision.Effect == policy.Deny {
			return &PolicyDeniedError{Summary: decision.Summary}
		}
		if err := loop.executeVerificationCommand(ctx, projection, input, turnID, operationID, command); err != nil {
			return err
		}
	}
	return loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: true})})
}

// verificationProposalPayload 是验证命令提议事件的可回放载荷。
type verificationProposalPayload struct {
	VerificationID string          `json:"verification_id"`
	CommandIndex   int             `json:"command_index"`
	PlanRef        *artifact.Ref   `json:"plan_ref,omitempty"`
	Arguments      json.RawMessage `json:"arguments"`
}

// policyDecisionDraft 将策略决定编码为可追加的领域事件草稿。
func policyDecisionDraft(input RunInput, turnID domain.TurnID, operationID domain.OperationID, decision policy.Decision) domain.EventDraft {
	return domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
		Summary     string `json:"summary"`
	}{Effect: string(decision.Effect), Rule: decision.RuleID, Fingerprint: decision.Fingerprint, Summary: decision.Summary})}
}

// approvalRequestedDraft 将待审批操作编码为可追加的领域事件草稿。
func approvalRequestedDraft(input RunInput, turnID domain.TurnID, operationID domain.OperationID, decision policy.Decision) domain.EventDraft {
	return domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalRequested, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Fingerprint string `json:"fingerprint"`
		Summary     string `json:"summary"`
		Rule        string `json:"rule"`
	}{Fingerprint: decision.Fingerprint, Summary: decision.Summary, Rule: decision.RuleID})}
}

// executeVerificationCommand 在策略授权后执行单条冻结验证命令并记录证据。
func (loop *Loop) executeVerificationCommand(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, operationID domain.OperationID, command VerificationCommand) error {
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		return err
	}
	if loop.Gate == nil {
		return errors.New("policy gate is required for verification")
	}
	beforeObservation, err := loop.verificationObservation()
	if err != nil {
		return fmt.Errorf("observe workspace before verification: %w", err)
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: "exec_command", SideEffectClass: string(tools.SideEffectProcess), MutationBarrier: false})}); err != nil {
		return err
	}
	result := redactToolResult(loop.Tools.Execute(ctx, "exec_command", command.Arguments))
	eventType := domain.EventOperationSucceeded
	cancelled := result.Code == "cancelled" && ctx.Err() != nil
	if cancelled {
		eventType = domain.EventOperationCancelled
	} else if !result.OK {
		eventType = domain.EventOperationFailed
	}
	resultContext := ctx
	var cancel context.CancelFunc
	if cancelled {
		resultContext, cancel = loop.commitContext(ctx)
		defer cancel()
	}
	if result.OK {
		afterObservation, observationErr := loop.verificationObservation()
		if observationErr != nil {
			return fmt.Errorf("observe workspace after verification: %w", observationErr)
		}
		if afterObservation != beforeObservation {
			result = tools.Failure("verification_mutated_workspace", "verification command modified the tool-visible workspace; repair or rerun verification before completion")
			eventType = domain.EventOperationFailed
		}
	}
	payload, err := loop.resultPayload("exec_command", result)
	if err != nil {
		return err
	}
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(payload)}); err != nil {
		return err
	}
	if cancelled {
		return ctx.Err()
	}
	if result.OK {
		return nil
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFinished, TurnID: &turnID, Payload: mustJSON(domain.VerificationFinishedPayload{OK: false})}); err != nil {
		return err
	}
	feedback := fmt.Sprintf("Verification command %s failed (%s). Inspect the result, repair the workspace, and run the configured verification again. Evidence: %s", command.ID, result.Code, result.Preview)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventVerificationFeedbackAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Text string `json:"text"`
	}{Text: feedback})}); err != nil {
		return err
	}
	return &VerificationFailureError{CommandID: command.ID, Feedback: feedback}
}

// verificationObservation 返回 verifier 视图的稳定摘要，用于绑定命令审批。
func (loop *Loop) verificationObservation() (string, error) {
	if loop.VerificationWorkspace.Root != "" {
		return loop.VerificationWorkspace.Observation()
	}
	if loop.Gate == nil {
		return "", errors.New("policy gate is required for workspace observation")
	}
	return loop.Gate.Patch.Workspace.Observation()
}

// executeTool 在策略、审批和日志授权后执行一次模型提议的固定工具调用。
func (loop *Loop) executeTool(ctx context.Context, projection *domain.RunProjection, input RunInput, turnID domain.TurnID, call ports.ToolCall) (tools.Result, error) {
	if call.ID == "" || call.Name == "" || !json.Valid(call.Arguments) {
		return tools.Result{}, errors.New("model returned invalid tool call")
	}
	if containsSecretJSON(call.Arguments) {
		return tools.Result{}, errors.New("model tool arguments contain a secret-like value")
	}
	if err := loop.ensureToolBudget(ctx, input.RunID); err != nil {
		return tools.Result{}, err
	}
	operationID := domain.NewOperationID()
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolCallProposed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(toolCallPayload{CallID: call.ID, ToolName: call.Name, Arguments: call.Arguments})}); err != nil {
		return tools.Result{}, err
	}
	if loop.Gate == nil {
		return tools.Result{}, errors.New("policy gate is required")
	}
	decision, err := loop.Gate.Decide(call.Name, call.Arguments)
	if err != nil {
		return tools.Result{}, err
	}
	decision = freshApprovalDecision(*projection, decision)
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventPolicyDecided, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
		Effect      string `json:"effect"`
		Rule        string `json:"rule"`
		Fingerprint string `json:"fingerprint"`
		Summary     string `json:"summary"`
	}{Effect: string(decision.Effect), Rule: decision.RuleID, Fingerprint: decision.Fingerprint, Summary: decision.Summary})}); err != nil {
		return tools.Result{}, err
	}
	if decision.Effect == policy.Ask {
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventApprovalRequested, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(struct {
			Fingerprint string `json:"fingerprint"`
			Summary     string `json:"summary"`
			Rule        string `json:"rule"`
		}{Fingerprint: decision.Fingerprint, Summary: decision.Summary, Rule: decision.RuleID})}); err != nil {
			return tools.Result{}, err
		}
		return tools.Result{}, &WaitingApprovalError{RunID: input.RunID, OperationID: operationID, Fingerprint: decision.Fingerprint, Summary: decision.Summary}
	}
	if decision.Effect == policy.Deny {
		result := tools.Failure("policy_denied", decision.Summary)
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationFailed, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(operationPayload{ToolName: call.Name, Result: result})}); err != nil {
			return tools.Result{}, err
		}
		// 策略拒绝不会执行副作用，但模型仍应得到可审计的失败结果，以便在剩余轮次中
		// 修正参数或改用允许的方案；人工审批拒绝与验证计划拒绝仍保持终态语义。
		if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(result)}); err != nil {
			return tools.Result{}, err
		}
		return result, nil
	}
	sideEffect := loop.Tools.SideEffect(call.Name)
	startedPayload := operationPayload{ToolName: call.Name, SideEffectClass: string(sideEffect), MutationBarrier: sideEffect == tools.SideEffectProcess}
	if call.Name == "apply_patch" {
		plan, err := loop.Gate.Patch.Plan(call.Arguments)
		if err != nil {
			return tools.Result{}, err
		}
		startedPayload.PlannedPath, startedPayload.PlannedBefore, startedPayload.PlannedAfter = plan.Path, plan.BeforeHash, plan.AfterHash
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventOperationStarted, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(startedPayload)}); err != nil {
		return tools.Result{}, err
	}
	result := loop.Tools.Execute(ctx, call.Name, call.Arguments)
	result = redactToolResult(result)
	eventType := domain.EventOperationSucceeded
	cancelled := result.Code == "cancelled" && ctx.Err() != nil
	if cancelled {
		eventType = domain.EventOperationCancelled
	} else if !result.OK {
		eventType = domain.EventOperationFailed
	}
	resultContext := ctx
	var cancel context.CancelFunc
	if cancelled {
		resultContext, cancel = loop.commitContext(ctx)
		defer cancel()
	}
	payload, err := loop.resultPayload(call.Name, result)
	if err != nil {
		return tools.Result{}, err
	}
	if err := loop.append(resultContext, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: eventType, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(payload)}); err != nil {
		return tools.Result{}, err
	}
	if cancelled {
		return result, ctx.Err()
	}
	if err := loop.append(ctx, projection, domain.EventDraft{RunID: input.RunID, WorkspaceHash: input.WorkspaceHash, Type: domain.EventToolResultAppended, TurnID: &turnID, OperationID: &operationID, Payload: mustJSON(result)}); err != nil {
		return tools.Result{}, err
	}
	return result, nil
}

// collectedResponse 汇集一次流式模型响应的文本、工具调用与 token 用量。
type collectedResponse struct {
	Text      string
	ToolCalls []ports.ToolCall
	Usage     *ports.Usage
}

// budgetUsage 保存从持久化响应中累计的输入与输出 token。
type budgetUsage struct {
	InputTokens  int
	OutputTokens int
}

// add 将单次模型 token 用量累加到运行总量。
func (usage *budgetUsage) add(value *ports.Usage) {
	if value == nil {
		return
	}
	usage.InputTokens += value.PromptTokens
	usage.OutputTokens += value.CompletionTokens
}

// budgetUsageFromEvents 从已持久化模型响应事件重建累计 token 消耗。
func budgetUsageFromEvents(events []domain.Event) budgetUsage {
	var usage budgetUsage
	for _, event := range events {
		if event.Type != domain.EventModelResponseFinished {
			continue
		}
		var payload modelResponsePayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			usage.add(payload.Usage)
		}
	}
	return usage
}

// responseBudgetExceeded 检查 token 和显式定价成本是否超过配置上限。
func (loop *Loop) responseBudgetExceeded(usage budgetUsage) *BudgetExceededError {
	if loop.MaxInputTokens > 0 && usage.InputTokens > loop.MaxInputTokens {
		return &BudgetExceededError{Dimension: "input_tokens"}
	}
	if loop.MaxOutputTokens > 0 && usage.OutputTokens > loop.MaxOutputTokens {
		return &BudgetExceededError{Dimension: "output_tokens"}
	}
	if loop.MaxCostUSD > 0 && usage.cost(loop.InputTokenUSD, loop.OutputTokenUSD) > loop.MaxCostUSD {
		return &BudgetExceededError{Dimension: "cost_usd"}
	}
	return nil
}

// cost 按显式输入、输出单价计算累计 token 的美元成本。
func (usage budgetUsage) cost(inputRate, outputRate float64) float64 {
	return float64(usage.InputTokens)*inputRate + float64(usage.OutputTokens)*outputRate
}

// ensureToolBudget 统计已启动操作，并在达到上限时拒绝新工具执行。
func (loop *Loop) ensureToolBudget(ctx context.Context, runID domain.RunID) error {
	if loop.MaxToolCalls <= 0 {
		return nil
	}
	events, err := loop.Journal.Load(ctx, runID)
	if err != nil {
		return err
	}
	started := 0
	for _, event := range events {
		if event.Type == domain.EventOperationStarted {
			started++
		}
	}
	if started >= loop.MaxToolCalls {
		return &BudgetExceededError{Dimension: "tool_calls"}
	}
	return nil
}

// callModel 调用 Provider 并严格收集一个以 Done 结束的有效流式响应。
func (loop *Loop) callModel(ctx context.Context, messages []ports.Message) (collectedResponse, error) {
	var response collectedResponse
	done := false
	err := loop.Provider.Stream(ctx, ports.ModelRequest{Model: loop.Model, Messages: messages, Tools: loop.Tools.Specs()}, func(event ports.ModelEvent) error {
		if done {
			return errors.New("provider emitted event after done")
		}
		switch event.Kind {
		case ports.TextDelta:
			response.Text += event.Text
		case ports.ToolCallEvent:
			if event.ToolCall == nil || event.ToolCall.ID == "" || event.ToolCall.Name == "" || !json.Valid(event.ToolCall.Arguments) {
				return errors.New("provider emitted invalid tool call")
			}
			response.ToolCalls = append(response.ToolCalls, *event.ToolCall)
		case ports.UsageEvent:
			if event.Usage == nil || response.Usage != nil {
				return errors.New("provider emitted invalid usage event")
			}
			usage := *event.Usage
			response.Usage = &usage
		case ports.Done:
			done = true
		default:
			return fmt.Errorf("unknown provider event %q", event.Kind)
		}
		return nil
	})
	if err != nil {
		return collectedResponse{}, err
	}
	if !done {
		return collectedResponse{}, errors.New("provider returned without done")
	}
	return response, nil
}

// append 以当前投影序号追加事件、应用投影并向外部 sink 广播。
func (loop *Loop) append(ctx context.Context, projection *domain.RunProjection, draft domain.EventDraft) error {
	event, err := loop.Journal.Append(ctx, projection.LastSeq, draft)
	if err != nil {
		return fmt.Errorf("append %s after sequence %d: %w", draft.Type, projection.LastSeq, err)
	}
	if err := projection.Apply(event); err != nil {
		return err
	}
	if loop.OnEvent != nil {
		loop.OnEvent(event)
	}
	return nil
}

// prompt 返回显式系统提示词，或提供安全工具使用的默认提示词。
func (loop *Loop) prompt() string {
	if strings.TrimSpace(loop.SystemPrompt) != "" {
		return loop.SystemPrompt
	}
	return "You are Cairn, a careful coding agent. Use the supplied tools for repository facts. Return a concise final answer when the task is answered."
}

// mustJSON 将事件载荷序列化并脱敏；仅编程错误会触发 panic。
func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Errorf("encode event payload: %w", err))
	}
	return append(json.RawMessage(nil), artifact.RedactJSON(encoded)...)
}

// containsSecretJSON 通过比较脱敏前后 JSON 判断载荷是否含有敏感内容。
func containsSecretJSON(raw json.RawMessage) bool {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return false
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return false
	}
	return !bytes.Equal(canonical, artifact.RedactJSON(canonical))
}

// freshApprovalDecision 为曾被不确定副作用对账过的同一动作强制重新人工审批。
func freshApprovalDecision(projection domain.RunProjection, decision policy.Decision) policy.Decision {
	if decision.Effect == policy.Allow && projection.RequiresFreshApproval(decision.Fingerprint) {
		decision.Effect = policy.Ask
		decision.RuleID = "reconciled_action_requires_approval"
		decision.Summary = "the same action was previously reconciled after an uncertain side effect; explicit approval is required"
	}
	return decision
}

// resultPayload 将工具结果包装为事件载荷，并在可用时持久化脱敏制品引用。
func (loop *Loop) resultPayload(toolName string, result tools.Result) (operationPayload, error) {
	payload := operationPayload{ToolName: toolName, Result: result}
	if loop.Artifacts == nil {
		return payload, nil
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return operationPayload{}, fmt.Errorf("encode %s result artifact: %w", toolName, err)
	}
	ref, err := loop.Artifacts.Put(encoded, "application/json")
	if err != nil {
		return operationPayload{}, fmt.Errorf("store %s result artifact: %w", toolName, err)
	}
	payload.ResultRef = &ref
	return payload, nil
}

// redactToolResult 清除工具结果预览和 JSON 数据中可能泄露的凭据。
func redactToolResult(result tools.Result) tools.Result {
	result.Preview = string(artifact.Redact([]byte(result.Preview)))
	if len(result.Data) != 0 {
		result.Data = append(json.RawMessage(nil), artifact.RedactJSON(result.Data)...)
	}
	return result
}
