package contract

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/fxbin/cairn/internal/agent"
	"github.com/fxbin/cairn/internal/domain"
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
)

// ClassifyError 把运行返回的错误映射为统一失败分类；它与事件侧
// ClassifyTerminal 共用同一词表，是失败分类的唯一 owner。
func ClassifyError(err error) string {
	var waiting *agent.WaitingApprovalError
	var denied *agent.PolicyDeniedError
	var exhausted *agent.BudgetExceededError
	var guarded *agent.LoopGuardExceededError
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
