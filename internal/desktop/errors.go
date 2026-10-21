package desktop

import (
	"context"
	"errors"
	"os"

	"github.com/fxbin/cairn/internal/app"
	"github.com/fxbin/cairn/internal/ports"
)

// 本文件把内部错误映射为前端稳定错误码，确保审批唤醒与失败关闭语义可被 UI
// 准确区分；文本不得包含密钥或原始模型输出。

// 错误码常量供前端分支使用，保持稳定。
const (
	CodeInvalidRequest      = "invalid_request"
	CodeWaitingApproval     = "waiting_approval"
	CodeWaitingReconcile    = "waiting_reconcile"
	CodeConfigDrift         = "config_drift"
	CodeStateCorrupt        = "state_corrupt"
	CodeSessionContinuation = "session_continuation"
	CodeActiveRun           = "active_run"
	CodePolicyDenied        = "policy_denied"
	CodeCancelled           = "cancelled"
	CodeNotFound            = "not_found"
	CodeUnavailable         = "unavailable"
	CodeProvider            = "provider"
	CodeBudget              = "budget"
	CodeInternal            = "internal"
)

// MapError 将任意错误转换为可序列化 ErrorBody；未知错误收敛为 internal。
func MapError(err error) ErrorBody {
	if err == nil {
		return ErrorBody{}
	}
	var waiting *ports.WaitingApprovalError
	if errors.As(err, &waiting) {
		return ErrorBody{
			Code:        CodeWaitingApproval,
			Message:     "操作等待人工审批",
			RunID:       string(waiting.RunID),
			OperationID: string(waiting.OperationID),
			Fingerprint: waiting.Fingerprint,
			Summary:     waiting.Summary,
		}
	}
	var reconcile *ports.WaitingReconcileError
	if errors.As(err, &reconcile) {
		return ErrorBody{
			Code:        CodeWaitingReconcile,
			Message:     "存在悬而未决副作用，需要人工对账",
			RunID:       string(reconcile.RunID),
			OperationID: string(reconcile.OperationID),
			Summary:     reconcile.Reason,
		}
	}
	var denied *ports.PolicyDeniedError
	if errors.As(err, &denied) {
		return ErrorBody{Code: CodePolicyDenied, Message: "策略拒绝了该操作"}
	}
	var budget *ports.BudgetExceededError
	if errors.As(err, &budget) {
		return ErrorBody{Code: CodeBudget, Message: "预算已耗尽: " + budget.Dimension}
	}
	switch {
	case errors.Is(err, app.ErrWaitingReconcile):
		return ErrorBody{Code: CodeWaitingReconcile, Message: "存在悬而未决副作用，需要人工对账"}
	case errors.Is(err, app.ErrConfigDrift):
		return ErrorBody{Code: CodeConfigDrift, Message: "配置指纹与运行创建时不一致，已失败关闭"}
	case errors.Is(err, app.ErrStateCorrupt):
		return ErrorBody{Code: CodeStateCorrupt, Message: "运行状态损坏，已失败关闭"}
	case errors.Is(err, app.ErrSessionContinuation):
		return ErrorBody{Code: CodeSessionContinuation, Message: "所选历史运行不满足安全续链条件"}
	case errors.Is(err, app.ErrActiveRun):
		return ErrorBody{Code: CodeActiveRun, Message: "该工作区已有进行中的运行"}
	case errors.Is(err, context.Canceled):
		return ErrorBody{Code: CodeCancelled, Message: "运行已取消"}
	case errors.Is(err, os.ErrNotExist):
		return ErrorBody{Code: CodeNotFound, Message: "未找到请求的资源"}
	}
	return ErrorBody{Code: CodeInternal, Message: "内部错误，请通过诊断面板查看详细信息"}
}

// AsErrorBody 便于在返回 (T, error) 的绑定方法中统一处理。
func AsErrorBody(err error) (ErrorBody, bool) {
	if err == nil {
		return ErrorBody{}, false
	}
	return MapError(err), true
}
