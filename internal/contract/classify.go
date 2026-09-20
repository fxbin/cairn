package contract

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/domain"
	"github.com/fxbin/cairn/internal/ports"
)

// 失败分类是普通 Run 与 benchmark 共用的统一词表：错误分类优先使用
// 领域错误类型，终态分类优先使用事件 payload 中的持久化事实。
const (
	// FailureApproval 表示运行停在等待人工审批的可恢复暂停。
	FailureApproval = "approval"
	// FailurePolicy 表示策略门拒绝了提议的副作用。
	FailurePolicy = "policy"
	// FailureCancelled 表示运行被用户或系统取消。
	FailureCancelled = "cancelled"
	// FailureTimeout 表示运行超过墙钟上限。
	FailureTimeout = "timeout"
	// FailureAgent 兜底表示未被细分类别的执行异常。
	FailureAgent = "agent"
	// FailureVerification 表示 benchmark 判定运行未以可信验证状态完成。
	FailureVerification = "verification"
	// FailureBudgetTurns 表示模型轮次预算耗尽。
	FailureBudgetTurns = "budget_turns"
	// FailureBudgetToolCalls 表示工具调用预算耗尽。
	FailureBudgetToolCalls = "budget_tool_calls"
	// FailureBudgetInputTokens 表示输入 token 预算耗尽。
	FailureBudgetInputTokens = "budget_input_tokens"
	// FailureBudgetOutputTokens 表示输出 token 预算耗尽。
	FailureBudgetOutputTokens = "budget_output_tokens"
	// FailureBudgetWallTime 表示墙钟预算耗尽。
	FailureBudgetWallTime = "budget_wall_time"
	// FailureBudgetCostUSD 表示成本预算耗尽。
	FailureBudgetCostUSD = "budget_cost_usd"
	// FailureLoopGuard 表示运行期熔断导致终态失败。
	FailureLoopGuard = "loop_guard"
	// FailureLoopGuardRepeatedOperationFailure 表示重复操作失败熔断。
	FailureLoopGuardRepeatedOperationFailure = "loop_guard_repeated_operation_failure"
	// FailureLoopGuardPeriodicTurnCycle 表示轮次周期熔断。
	FailureLoopGuardPeriodicTurnCycle = "loop_guard_periodic_turn_cycle"
	// FailureLoopGuardNoProgress 表示没有工作区或验证器进展的熔断。
	FailureLoopGuardNoProgress = "loop_guard_no_workspace_or_verifier_progress"
	// FailureMalformedToolArguments 表示模型返回的工具参数不符合合同。
	FailureMalformedToolArguments = "malformed_tool_arguments"
	// FailureSensitiveToolArguments 表示模型尝试将敏感参数交给工具。
	FailureSensitiveToolArguments = "sensitive_tool_arguments"
	// FailureRateLimited 表示 Provider 明确返回可分类的限流错误。
	FailureRateLimited = "rate_limited"
	// FailurePaymentRequired 表示 Provider 因余额、计费或付款要求拒绝请求。
	FailurePaymentRequired = "payment_required"
	// FailureProviderCapabilityMismatch 表示 Provider 违背冻结能力合同。
	FailureProviderCapabilityMismatch = "provider_capability_mismatch"
)

var knownTerminalFailureClasses = map[string]struct{}{
	FailureApproval:                          {},
	FailurePolicy:                            {},
	FailureCancelled:                         {},
	FailureTimeout:                           {},
	FailureAgent:                             {},
	FailureVerification:                      {},
	FailureBudgetTurns:                       {},
	FailureBudgetToolCalls:                   {},
	FailureBudgetInputTokens:                 {},
	FailureBudgetOutputTokens:                {},
	FailureBudgetWallTime:                    {},
	FailureBudgetCostUSD:                     {},
	FailureLoopGuard:                         {},
	FailureLoopGuardRepeatedOperationFailure: {},
	FailureLoopGuardPeriodicTurnCycle:        {},
	FailureLoopGuardNoProgress:               {},
	FailureMalformedToolArguments:            {},
	FailureSensitiveToolArguments:            {},
	FailureRateLimited:                       {},
	FailurePaymentRequired:                   {},
	FailureProviderCapabilityMismatch:        {},
}

// budgetFailureClasses 是 Agent 自身预算终止的闭合子集：这类终态携带完整的
// 事件证据与 token 用量，属于评测的有效失败样本而非提供方故障。
var budgetFailureClasses = map[string]struct{}{
	FailureBudgetTurns:        {},
	FailureBudgetToolCalls:    {},
	FailureBudgetInputTokens:  {},
	FailureBudgetOutputTokens: {},
	FailureBudgetWallTime:     {},
	FailureBudgetCostUSD:      {},
}

// IsBudgetFailure 判断终态分类是否属于 Agent 自身预算终止：评测编排据此把
// 预算耗尽的 Run 按有效失败样本计入成功率与成本，而不是当作提供方故障停摆。
func IsBudgetFailure(class string) bool {
	_, budget := budgetFailureClasses[class]
	return budget
}

// IsKnownTerminalFailureClass 判断值是否属于当前稳定的终态失败分类词表。该词表是
// M2-A 精确查询可使用的受控子集；未知 Event payload 分类不会被放宽成查询 token。
func IsKnownTerminalFailureClass(value string) bool {
	_, exists := knownTerminalFailureClasses[value]
	return exists
}

// ClassifyError 把运行返回的错误映射为统一失败分类；它与事件侧
// ClassifyTerminal 共用同一词表，是失败分类的唯一 owner。
func ClassifyError(err error) string {
	var waiting *agent.WaitingApprovalError
	var denied *agent.PolicyDeniedError
	var exhausted *agent.BudgetExceededError
	var guarded *agent.LoopGuardExceededError
	var providerFailure ports.ClassifiedError
	switch {
	case errors.As(err, &waiting):
		return FailureApproval
	case errors.As(err, &denied):
		return FailurePolicy
	case errors.As(err, &exhausted):
		return "budget_" + exhausted.Dimension
	case errors.As(err, &guarded):
		return "loop_guard_" + guarded.Reason
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTimeout
	case errors.Is(err, context.Canceled):
		return FailureCancelled
	case errors.As(err, &providerFailure):
		if class := providerFailure.ErrorClass(); IsKnownTerminalFailureClass(class) {
			return class
		}
		return FailureAgent
	default:
		return FailureAgent
	}
}

// ClassifyTerminal 从终态事件推导统一失败分类：run_failed 优先读取
// error_class，其次组合 budget_dimension，run_cancelled 归入 cancelled。
// 非失败终态或无非终态事件返回空字符串。
func ClassifyTerminal(events []domain.Event) string {
	for index := len(events) - 1; index >= 0; index-- {
		event := events[index]
		switch event.Type {
		case domain.EventRunFailed:
			var payload struct {
				ErrorClass      string `json:"error_class"`
				BudgetDimension string `json:"budget_dimension"`
			}
			if json.Unmarshal(event.Payload, &payload) != nil {
				return FailureAgent
			}
			if payload.ErrorClass != "" {
				return payload.ErrorClass
			}
			if payload.BudgetDimension != "" {
				return "budget_" + payload.BudgetDimension
			}
			return FailureAgent
		case domain.EventRunCancelled:
			return FailureCancelled
		case domain.EventRunCompleted:
			return ""
		}
	}
	return ""
}
